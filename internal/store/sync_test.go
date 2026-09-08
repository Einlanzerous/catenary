package store

// CANT-20 — paging, has_more at the boundary, the normative ordering, and the
// cursor rule that is easy to get subtly wrong.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func send(ctx context.Context, t *testing.T, st *Store, conv, author uuid.UUID, text string) Sent {
	t.Helper()
	s, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: &text,
	})
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return s
}

func head(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var v int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &v)
	return v
}

// The ordering is NORMATIVE, not incidental: a client applies the page as a
// stream and may stop anywhere without leaving a hole behind its cursor, which
// is only true if the order is the log's.
func TestSyncReturnsMessagesAscendingByLogSeq(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	for i := 0; i < 5; i++ {
		send(ctx, t, st, conv, author, "m")
	}

	page, err := st.Sync(ctx, author, 0, 100)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(page.Messages) != 5 {
		t.Fatalf("got %d messages, want 5", len(page.Messages))
	}
	for i := 1; i < len(page.Messages); i++ {
		if page.Messages[i].LogSeq <= page.Messages[i-1].LogSeq {
			t.Errorf("log_seq not ascending at %d: %d after %d",
				i, page.Messages[i].LogSeq, page.Messages[i-1].LogSeq)
		}
	}
}

// has_more is honest AT THE BOUNDARY, which is the case a limit+1 read exists
// to observe rather than guess: a page holding exactly `limit` messages with
// nothing behind it must report false.
func TestHasMoreIsHonestAtTheBoundary(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	for i := 0; i < 3; i++ {
		send(ctx, t, st, conv, author, "m")
	}

	for _, tc := range []struct {
		name     string
		limit    int
		wantN    int
		wantMore bool
	}{
		{"a page short of the limit", 10, 3, false},
		{"a page EXACTLY at the limit with nothing behind it", 3, 3, false},
		{"a page at the limit with one behind it", 2, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := st.Sync(ctx, author, 0, tc.limit)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if len(page.Messages) != tc.wantN {
				t.Errorf("got %d messages, want %d", len(page.Messages), tc.wantN)
			}
			if page.HasMore != tc.wantMore {
				t.Errorf("has_more = %t, want %t", page.HasMore, tc.wantMore)
			}
		})
	}
}

// THE CURSOR RULE, and the half that is easy to get wrong.
//
// The wire says the cursor is "always the caller's new high-water mark — not the
// server's head", AND that "deriving this client-side by maxing over messages is
// wrong the moment a page contains no messages the client can see". Both hold at
// once, and they pull opposite ways: a full page stops at its last message, a
// caught-up page goes to the head it was read against — including past rows this
// reader will never see.
//
// Without the second half a reader in a quiet conversation re-scans every other
// conversation's traffic on every single call, forever.
func TestTheCursorAdvancesPastMessagesTheReaderCannotSee(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	mine := mkUser(ctx, t, pool, "mine")
	stranger := mkUser(ctx, t, pool, "stranger")
	myConv := mkGroup(ctx, t, pool, "mine", mine)
	theirConv := mkGroup(ctx, t, pool, "theirs", stranger)

	send(ctx, t, st, myConv, mine, "visible")
	for i := 0; i < 4; i++ {
		send(ctx, t, st, theirConv, stranger, "not for me")
	}

	page, err := st.Sync(ctx, mine, 0, 100)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("got %d messages, want the 1 in my conversation", len(page.Messages))
	}
	if page.HasMore {
		t.Error("has_more is true with nothing visible behind the page")
	}
	if h := head(ctx, t, pool); page.HighWater != h {
		t.Errorf("cursor = %d, want the head %d — a caught-up reader must advance past "+
			"the four messages they cannot see, or they re-scan them forever",
			page.HighWater, h)
	}

	// And the next call returns nothing rather than the same page again.
	next, err := st.Sync(ctx, mine, page.HighWater, 100)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(next.Messages) != 0 {
		t.Errorf("the cursor did not advance: %d messages came back again", len(next.Messages))
	}
}

// A full page stops at its last message, because nothing beyond it was scanned.
func TestAFullPageStopsAtItsLastMessage(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	for i := 0; i < 5; i++ {
		send(ctx, t, st, conv, author, "m")
	}

	page, err := st.Sync(ctx, author, 0, 2)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !page.HasMore {
		t.Fatal("has_more is false on a page with three messages behind it")
	}
	if want := page.Messages[len(page.Messages)-1].LogSeq; page.HighWater != want {
		t.Errorf("cursor = %d, want %d — the last message actually returned. Claiming the "+
			"head here would skip everything between it and the page", page.HighWater, want)
	}
}

// A reader sees only conversations they are in.
func TestSyncReturnsOnlyWhatTheReaderIsAMemberOf(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	mine := mkUser(ctx, t, pool, "mine")
	stranger := mkUser(ctx, t, pool, "stranger")
	theirConv := mkGroup(ctx, t, pool, "theirs", stranger)
	send(ctx, t, st, theirConv, stranger, "private")

	page, err := st.Sync(ctx, mine, 0, 100)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(page.Messages) != 0 {
		t.Errorf("a non-member received %d messages", len(page.Messages))
	}
	if len(page.Conversations) != 0 {
		t.Errorf("a non-member received %d conversations", len(page.Conversations))
	}
}

// The fan-out the wire requires: every user referenced by the page, and the
// conversation metadata a client needs to render one it has never seen.
func TestSyncCarriesTheConversationsAndUsersThePageReferences(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	conv := mkGroup(ctx, t, pool, "room", alice, bob)
	send(ctx, t, st, conv, bob, "hello")

	page, err := st.Sync(ctx, alice, 0, 100)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(page.Conversations) != 1 {
		t.Fatalf("got %d conversations, want 1", len(page.Conversations))
	}
	c := page.Conversations[0]
	if c.MemberCount != 2 {
		t.Errorf("member_count = %d, want 2", c.MemberCount)
	}
	if c.LastSeq != 1 {
		t.Errorf("head_seq source = %d, want 1", c.LastSeq)
	}
	// Alice has read nothing and bob wrote seq 1, so her first unread is 1.
	if c.FirstUnreadSeq == nil || *c.FirstUnreadSeq != 1 {
		t.Errorf("first_unread_seq = %v, want 1", c.FirstUnreadSeq)
	}
	// Both members, because a client needs a name for every id it will render.
	if len(page.Users) != 2 {
		t.Errorf("got %d users, want both members", len(page.Users))
	}
}

// Your own message is not your own unread — 0005's author filter, reaching the
// wire through the same query.
func TestFirstUnreadSeqSkipsTheReadersOwnMessages(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	conv := mkGroup(ctx, t, pool, "room", alice, bob)
	send(ctx, t, st, conv, alice, "mine")
	send(ctx, t, st, conv, alice, "also mine")

	page, err := st.Sync(ctx, alice, 0, 100)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := page.Conversations[0].FirstUnreadSeq; got != nil {
		t.Errorf("first_unread_seq = %d for a reader whose only unread messages are their "+
			"own; Invariant 3 says you cannot have an unread message you sent", *got)
	}
}
