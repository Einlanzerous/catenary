package client

// The instrument's own tests. Compare is what the kill test's "zero lost, zero
// duplicated, zero phantom" rests on, so each count is watched going non-zero
// here on a planted journal — and cmd/catenary's kill test then watches each
// one go non-zero against a deliberately broken client over a real server.

import (
	"strings"
	"testing"

	"github.com/magos/catenary/internal/wire"
)

func entry(id, conv string, seq, logSeq int64) LogEntry {
	return LogEntry{ID: wire.Uuid(id), ConversationID: wire.Uuid(conv), Seq: seq, LogSeq: logSeq}
}

func held(e LogEntry) wire.Message {
	return wire.Message{ID: e.ID, ConversationID: e.ConversationID, Seq: e.Seq, LogSeq: e.LogSeq}
}

func journalOf(entries ...LogEntry) Snapshot {
	var s Snapshot
	for _, e := range entries {
		s.Messages = append(s.Messages, held(e))
		s.Counted = append(s.Counted, e.ID)
	}
	return s
}

func TestCompareIsCleanWhenTheJournalIsTheLog(t *testing.T) {
	log := []LogEntry{entry("a", "A", 1, 1), entry("b", "B", 1, 4), entry("c", "A", 2, 9)}
	if r := Compare(log, journalOf(log...)); !r.Clean() {
		t.Fatalf("identical journal and log: %s", r)
	}
}

func TestCompareCountsEachFault(t *testing.T) {
	a, b, c := entry("a", "A", 1, 1), entry("b", "A", 2, 2), entry("c", "A", 3, 3)
	log := []LogEntry{a, b, c}

	t.Run("lost", func(t *testing.T) {
		r := Compare(log, journalOf(a, c))
		if len(r.Lost) != 1 || r.Lost[0] != "b" || r.Clean() {
			t.Errorf("lost = %v, want [b]: %s", r.Lost, r)
		}
	})
	t.Run("phantom", func(t *testing.T) {
		r := Compare(log, journalOf(a, b, c, entry("ghost", "A", 9, 99)))
		if len(r.Phantom) != 1 || r.Phantom[0] != "ghost" || r.Clean() {
			t.Errorf("phantom = %v, want [ghost]: %s", r.Phantom, r)
		}
	})
	t.Run("duplicated", func(t *testing.T) {
		s := journalOf(a, b, c)
		s.Counted = append(s.Counted, "b")
		r := Compare(log, s)
		if len(r.Duplicated) != 1 || r.Duplicated[0] != "b" || r.Clean() {
			t.Errorf("duplicated = %v, want [b]: %s", r.Duplicated, r)
		}
	})
	t.Run("mismatched", func(t *testing.T) {
		wrong := b
		wrong.LogSeq = 7
		r := Compare(log, journalOf(a, wrong, c))
		if len(r.Mismatched) != 1 || r.Mismatched[0] != "b" || r.Clean() {
			t.Errorf("mismatched = %v, want [b]: %s", r.Mismatched, r)
		}
	})
	t.Run("a seq held twice", func(t *testing.T) {
		// What a store kept across a truncated-and-regrown log looks like:
		// the old message and the new one at the same place in the thread.
		r := Compare(log, journalOf(a, b, c, entry("old-b", "A", 2, 5)))
		if len(r.SeqConflicts) != 1 || !strings.Contains(r.SeqConflicts[0], "seq 2") || r.Clean() {
			t.Errorf("seq conflicts = %v: %s", r.SeqConflicts, r)
		}
		if len(r.OutOfOrder) != 1 {
			t.Errorf("out of order = %v, want A: the thread at seq 2 reads log_seq 2 then 5 then seq 3 at 3", r.OutOfOrder)
		}
	})
}

func TestSameViewNamesEveryDifference(t *testing.T) {
	x := Snapshot{
		Cursor: 9, HasCursor: true,
		Messages:      []wire.Message{held(entry("a", "A", 1, 1))},
		Conversations: []wire.Conversation{{ID: "A", Name: "one"}},
		Users:         []wire.User{{ID: "u", Name: "Ada"}},
	}
	if d := SameView(x, x); len(d) != 0 {
		t.Fatalf("a snapshot differs from itself: %v", d)
	}
	y := x
	y.Cursor = 8
	y.Conversations = []wire.Conversation{{ID: "A", Name: "renamed"}}
	y.Users = nil
	d := SameView(x, y)
	if len(d) != 3 {
		t.Fatalf("differences = %v, want the cursor, the conversation and the user", d)
	}
}
