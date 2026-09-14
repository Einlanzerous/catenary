package client

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/magos/catenary/internal/wire"
)

// LogEntry is one committed message as the server's own record has it: a row
// of `messages` in a conversation the viewer is a member of. The caller reads
// these from the store; this package does not reach for a database.
type LogEntry struct {
	ID             wire.Uuid
	ConversationID wire.Uuid
	Seq            int64
	LogSeq         int64
}

// Report is R1's verify step: the client's journal against the server's log.
type Report struct {
	ServerMessages int
	ClientMessages int
	Counted        int

	// Lost: committed, visible to the viewer, not held.
	Lost []wire.Uuid
	// Phantom: held, and not in the server's log for this viewer.
	Phantom []wire.Uuid
	// Duplicated: counted more than once since the last wipe.
	Duplicated []wire.Uuid
	// Mismatched: held under a different conversation, seq or log_seq than
	// the server committed it with.
	Mismatched []wire.Uuid
	// SeqConflicts: two different ids held at one (conversation, seq). Seq is
	// dense and unique per conversation, so this is a thread that cannot be
	// rendered without lying.
	SeqConflicts []string
	// OutOfOrder: conversations whose held thread, read in seq order, does not
	// ascend in log_seq — the two ordinals disagreeing about the order.
	OutOfOrder []wire.Uuid
}

// Clean is zero lost, zero duplicated, zero phantom, nothing mismatched, and
// every thread in order.
func (r Report) Clean() bool {
	return len(r.Lost)+len(r.Phantom)+len(r.Duplicated)+len(r.Mismatched)+
		len(r.SeqConflicts)+len(r.OutOfOrder) == 0
}

func (r Report) String() string {
	return fmt.Sprintf("server %d · client %d · counted %d · LOST %d · DUPLICATED %d · PHANTOM %d · mismatched %d · seq conflicts %d · out of order %d",
		r.ServerMessages, r.ClientMessages, r.Counted, len(r.Lost), len(r.Duplicated), len(r.Phantom),
		len(r.Mismatched), len(r.SeqConflicts), len(r.OutOfOrder))
}

// Compare checks a journal against the server's log for the same viewer.
func Compare(server []LogEntry, s Snapshot) Report {
	r := Report{ServerMessages: len(server), ClientMessages: len(s.Messages), Counted: len(s.Counted)}

	committed := make(map[wire.Uuid]LogEntry, len(server))
	for _, e := range server {
		committed[e.ID] = e
	}
	// A literal, not make(…, n): wireview's guard counts make over a Message
	// map as construction (CANT-84), and this builds none.
	held := map[wire.Uuid]wire.Message{}
	for _, m := range s.Messages {
		held[m.ID] = m
	}

	for _, e := range server {
		if _, ok := held[e.ID]; !ok {
			r.Lost = append(r.Lost, e.ID)
		}
	}
	for _, m := range s.Messages {
		e, ok := committed[m.ID]
		switch {
		case !ok:
			r.Phantom = append(r.Phantom, m.ID)
		case m.ConversationID != e.ConversationID || m.Seq != e.Seq || m.LogSeq != e.LogSeq:
			r.Mismatched = append(r.Mismatched, m.ID)
		}
	}

	counts := map[wire.Uuid]int{}
	for _, id := range s.Counted {
		if counts[id]++; counts[id] == 2 {
			r.Duplicated = append(r.Duplicated, id)
		}
	}

	type slot struct {
		conv wire.Uuid
		seq  int64
	}
	at := map[slot]wire.Uuid{}
	threads := map[wire.Uuid][]wire.Message{}
	for _, m := range s.Messages {
		k := slot{m.ConversationID, m.Seq}
		if other, ok := at[k]; ok && other != m.ID {
			r.SeqConflicts = append(r.SeqConflicts, fmt.Sprintf("%s seq %d held as %s and %s", m.ConversationID, m.Seq, other, m.ID))
		} else {
			at[k] = m.ID
		}
		threads[m.ConversationID] = append(threads[m.ConversationID], m)
	}
	for conv, th := range threads {
		sort.Slice(th, func(a, b int) bool {
			if th[a].Seq != th[b].Seq {
				return th[a].Seq < th[b].Seq
			}
			return th[a].LogSeq < th[b].LogSeq
		})
		for i := 1; i < len(th); i++ {
			if th[i].LogSeq <= th[i-1].LogSeq {
				r.OutOfOrder = append(r.OutOfOrder, conv)
				break
			}
		}
	}

	for _, ids := range [][]wire.Uuid{r.Lost, r.Phantom, r.Duplicated, r.Mismatched, r.OutOfOrder} {
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	}
	sort.Strings(r.SeqConflicts)
	return r
}

// SameView lists every difference between two snapshots' stores — messages,
// conversations and users compared record by record, and the cursor. Empty
// means identical. The counted log and the wipe count are history, not store,
// and are not compared.
func SameView(a, b Snapshot) []string {
	var diffs []string
	if a.Cursor != b.Cursor || a.HasCursor != b.HasCursor {
		diffs = append(diffs, fmt.Sprintf("cursor %d (set %v) vs %d (set %v)", a.Cursor, a.HasCursor, b.Cursor, b.HasCursor))
	}
	diffs = append(diffs, diffByID("message", a.Messages, b.Messages, func(m wire.Message) wire.Uuid { return m.ID })...)
	diffs = append(diffs, diffByID("conversation", a.Conversations, b.Conversations, func(c wire.Conversation) wire.Uuid { return c.ID })...)
	diffs = append(diffs, diffByID("user", a.Users, b.Users, func(u wire.User) wire.Uuid { return u.ID })...)
	return diffs
}

func diffByID[T any](kind string, a, b []T, id func(T) wire.Uuid) []string {
	am, bm := map[wire.Uuid]T{}, map[wire.Uuid]T{}
	for _, v := range a {
		am[id(v)] = v
	}
	for _, v := range b {
		bm[id(v)] = v
	}
	var diffs []string
	for k, av := range am {
		bv, ok := bm[k]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s %s only in the first", kind, k))
		case !reflect.DeepEqual(av, bv):
			diffs = append(diffs, fmt.Sprintf("%s %s differs: %+v vs %+v", kind, k, av, bv))
		}
	}
	for k := range bm {
		if _, ok := am[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s %s only in the second", kind, k))
		}
	}
	sort.Strings(diffs)
	return diffs
}
