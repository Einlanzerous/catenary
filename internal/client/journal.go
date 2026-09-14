package client

import (
	"sort"
	"sync"

	"github.com/magos/catenary/internal/wire"
)

// Journal is the client's DURABLE state: the messages, conversations and users
// it holds, the cursor, and the evidence log of which messages it counted.
//
// It outlives a Client. A kill abandons the Client and keeps the Journal, and a
// restart is a new Client over the same Journal — which is what a process that
// died and came back finds on disk. That is the whole of what "durable" means
// in-process, and it is enough for the property under test: nothing a killed
// client did after its death reaches the Journal (Client.Kill takes this lock
// to die), and everything it did before is here.
//
// OBLIGATION 1 — PERSIST BEFORE RENDER — HOLDS BY CONSTRUCTION. A page's
// messages, conversations, users and cursor are written under one lock, and
// nothing is observable (Status, Holds, Snapshot, the Await wake-up) until the
// lock is released. There is no window in which a message is shown or counted
// and the cursor that covers it is not written. On real storage that is a
// transaction or an fsync, and proving it there is CANT-35's and CANT-42's.
type Journal struct {
	mu sync.Mutex

	cursor    int64
	hasCursor bool

	messages      map[wire.Uuid]wire.Message
	conversations map[wire.Uuid]wire.Conversation
	users         map[wire.Uuid]wire.User

	// counted is R1's journal: every message id in the order the client first
	// counted it. A message counted twice is a duplicate delivery the client
	// failed to recognise, and Compare reports it. Cleared by a wipe, because
	// a wipe discards the store the counts were counts of.
	counted []wire.Uuid

	// wipes is how many discard-and-bootstraps this journal has been through.
	// It doubles as the epoch a page request is issued in: a page requested
	// before a wipe is dropped rather than applied over it.
	wipes int
}

// NewJournal returns an empty journal: no cursor, nothing held.
func NewJournal() *Journal {
	j := &Journal{}
	j.resetLocked()
	return j
}

// resetLocked is obligation 4's wipe: messages, conversations, users and the
// cursor, all of them. The caller holds mu and bumps wipes.
func (j *Journal) resetLocked() {
	j.cursor, j.hasCursor = 0, false
	j.messages = map[wire.Uuid]wire.Message{}
	j.conversations = map[wire.Uuid]wire.Conversation{}
	j.users = map[wire.Uuid]wire.User{}
	j.counted = nil
}

// Snapshot is a consistent copy of a Journal.
type Snapshot struct {
	Cursor    int64
	HasCursor bool
	Wipes     int

	// Messages sorted by conversation, then seq — the thread as it renders.
	Messages      []wire.Message
	Conversations []wire.Conversation // by id
	Users         []wire.User         // by id

	// Counted is every message id in the order it was first counted since
	// the last wipe. A correct client counts each id exactly once.
	Counted []wire.Uuid
}

// Snapshot copies the journal under its lock.
func (j *Journal) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := Snapshot{
		Cursor: j.cursor, HasCursor: j.hasCursor, Wipes: j.wipes,
		Counted: append([]wire.Uuid(nil), j.counted...),
	}
	for _, m := range j.messages {
		s.Messages = append(s.Messages, m)
	}
	sort.Slice(s.Messages, func(a, b int) bool {
		if s.Messages[a].ConversationID != s.Messages[b].ConversationID {
			return s.Messages[a].ConversationID < s.Messages[b].ConversationID
		}
		if s.Messages[a].Seq != s.Messages[b].Seq {
			return s.Messages[a].Seq < s.Messages[b].Seq
		}
		return s.Messages[a].ID < s.Messages[b].ID
	})
	for _, c := range j.conversations {
		s.Conversations = append(s.Conversations, c)
	}
	sort.Slice(s.Conversations, func(a, b int) bool { return s.Conversations[a].ID < s.Conversations[b].ID })
	for _, u := range j.users {
		s.Users = append(s.Users, u)
	}
	sort.Slice(s.Users, func(a, b int) bool { return s.Users[a].ID < s.Users[b].ID })
	return s
}
