package store

// CANT-26 — what a receipt is allowed to claim, and the one derivation the
// badge and the "N NEW" divider both come from.

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

// A RECEIPT IS A HIGH-WATER MARK. The wire says a lower mark than one already
// held "is simply discarded", and this is that sentence as a test: the mark
// does not move, and Advanced says so, which is what keeps CANT-21's fanout
// from waking every device in a room to announce nothing.
func TestAReceiptOnlyEverMovesForward(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	conv := mkGroup(ctx, t, pool, "room", alice, bob)
	for i := 0; i < 5; i++ {
		send(ctx, t, st, conv, bob, "hello")
	}

	r, err := st.MarkRead(ctx, conv, alice, 3)
	if err != nil {
		t.Fatalf("mark read 3: %v", err)
	}
	if r.UpToSeq != 3 || !r.Advanced {
		t.Fatalf("first receipt = %+v, want up_to_seq 3 and advanced", r)
	}

	// Out of order, or a duplicate: discarded, and reported as discarded.
	r, err = st.MarkRead(ctx, conv, alice, 1)
	if err != nil {
		t.Fatalf("mark read 1: %v", err)
	}
	if r.UpToSeq != 3 {
		t.Errorf("up_to_seq = %d after a lower claim, want the mark held at 3", r.UpToSeq)
	}
	if r.Advanced {
		t.Error("advanced = true for a mark that did not move")
	}

	// THE ECHO IS THE MARK, NOT THE CLAIM. Broadcasting the request back would
	// tell every other member this reader had un-read two messages.
	if r.UpToSeq == 1 {
		t.Error("the receipt echoed the claim rather than the mark")
	}
}

// A RECEIPT CANNOT CLAIM A MESSAGE THAT DOES NOT EXIST, and this is the case
// with teeth. up_to_seq is client-supplied. Unclamped, one claim of 2^53-1
// marks every message that ever arrives in this room as read, forever — and the
// member cannot undo it, because the rule above discards every lower mark. That
// is an unread badge that never comes back, from one buggy client.
func TestAReceiptIsClampedToMessagesThatExist(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	conv := mkGroup(ctx, t, pool, "room", alice, bob)
	send(ctx, t, st, conv, bob, "one")
	send(ctx, t, st, conv, bob, "two")

	// Seq's own maximum, 2^53-1, which is the largest a VALID frame can carry
	// — the schema caps it there so a seq stays an exact integer in a
	// JavaScript double. So this is not a malformed request, just a wrong one,
	// and the refusal in the test below does not cover it.
	const seqMax = 9007199254740991
	r, err := st.MarkRead(ctx, conv, alice, seqMax)
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if r.UpToSeq != 2 {
		t.Errorf("up_to_seq = %d, want 2 — the head is what exists", r.UpToSeq)
	}

	// The half that matters: what arrives next is still unread.
	send(ctx, t, st, conv, bob, "three")
	seq, err := st.FirstUnreadSeq(ctx, conv, alice)
	if err != nil {
		t.Fatalf("first unread: %v", err)
	}
	if seq == nil || *seq != 3 {
		t.Errorf("first_unread_seq = %v, want 3 — an over-claim must not swallow the future", seq)
	}
}

// A non-member gets `not_a_member` rather than a silent no-op, and the code is
// the table's decision rather than this file's.
func TestOnlyAMemberCanMarkRead(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	stranger := mkUser(ctx, t, pool, "stranger")
	conv := mkGroup(ctx, t, pool, "room", alice, bob)
	send(ctx, t, st, conv, bob, "members only")

	_, err := st.MarkRead(ctx, conv, stranger, 1)
	assertCode(t, err, wire.ErrorCodeNotAMember, ErrNotAMember)

	// A conversation that does not exist answers the same way, so a caller
	// cannot use a receipt to learn whether a room they cannot see is there.
	_, err = st.MarkRead(ctx, uuid.New(), alice, 1)
	assertCode(t, err, wire.ErrorCodeNotAMember, ErrNotAMember)
}

// `up_to_seq` below 1 is refused rather than absorbed, and it is refused
// WITHOUT a wire code. The enum has no member for "your input is malformed",
// `internal` would be a lie about whose fault it is, and syncHandler already
// answers that shape with a plain 400. Seq's minimum is 1 on the wire, so a
// well-formed frame never gets here.
func TestAMalformedMarkIsRefusedAndIsNotAWireCode(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	conv := mkGroup(ctx, t, pool, "room", alice, bob)

	for _, seq := range []int64{0, -1} {
		_, err := st.MarkRead(ctx, conv, alice, seq)
		if !errors.Is(err, ErrSeqOutOfRange) {
			t.Errorf("MarkRead(%d) = %v, want ErrSeqOutOfRange", seq, err)
		}
		var se *SendError
		if errors.As(err, &se) {
			t.Errorf("MarkRead(%d) carried wire code %q; malformed input has none", seq, se.Code)
		}
	}
}

// read_by counts everyone EXCEPT the author, which is what makes `READ 5/7`
// mean five other people. The author's own receipt is the trap: bob reads his
// own message back and it must still be READ 1, not READ 2.
func TestReadByCountsEveryMemberButTheAuthor(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	carol := mkUser(ctx, t, pool, "carol")
	conv := mkGroup(ctx, t, pool, "room", alice, bob, carol)
	m := send(ctx, t, st, conv, bob, "who has seen this")

	readBy := func() int64 {
		t.Helper()
		page, err := st.Sync(ctx, bob, 0, 100)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		return page.ReadBy[m.ID]
	}

	if got := readBy(); got != 0 {
		t.Errorf("read_by = %d before anyone read it, want 0", got)
	}
	if _, err := st.MarkRead(ctx, conv, bob, m.Seq); err != nil {
		t.Fatalf("bob reads his own: %v", err)
	}
	if got := readBy(); got != 0 {
		t.Errorf("read_by = %d after only the AUTHOR read it, want 0", got)
	}
	if _, err := st.MarkRead(ctx, conv, alice, m.Seq); err != nil {
		t.Fatalf("alice reads: %v", err)
	}
	if got := readBy(); got != 1 {
		t.Errorf("read_by = %d after one other member read it, want 1", got)
	}
	if _, err := st.MarkRead(ctx, conv, carol, m.Seq); err != nil {
		t.Fatalf("carol reads: %v", err)
	}
	if got := readBy(); got != 2 {
		t.Errorf("read_by = %d after both others read it, want 2", got)
	}
}

// ONE DERIVATION, NOT TWO THAT AGREE TODAY. The store method and the sync
// page's inline column are the same const; this is the test that would catch
// one of them being edited on its own.
//
// It runs over the shape the canvas draws — a run of five with three from other
// people — because that is the shape the web client's own assertions are
// written against.
func TestBothPathsToFirstUnreadSeqAgree(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	conv := mkGroup(ctx, t, pool, "room", alice, bob)

	send(ctx, t, st, conv, bob, "seen")
	if _, err := st.MarkRead(ctx, conv, alice, 1); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	// Five above the mark, three of them from bob and two of them alice's own.
	send(ctx, t, st, conv, bob, "new one")
	send(ctx, t, st, conv, alice, "mine")
	send(ctx, t, st, conv, bob, "new two")
	send(ctx, t, st, conv, alice, "also mine")
	send(ctx, t, st, conv, bob, "new three")

	direct, err := st.FirstUnreadSeq(ctx, conv, alice)
	if err != nil {
		t.Fatalf("first unread: %v", err)
	}
	page, err := st.Sync(ctx, alice, 0, 100)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	inPage := page.Conversations[0].FirstUnreadSeq

	if direct == nil || inPage == nil {
		t.Fatalf("first_unread_seq direct = %v, in page = %v; both want 2", direct, inPage)
	}
	if *direct != *inPage {
		t.Errorf("the two paths disagree: direct %d, page %d", *direct, *inPage)
	}
	if *direct != 2 {
		t.Errorf("first_unread_seq = %d, want 2 — the first message above the mark alice did not write", *direct)
	}

	// And alice's own messages are not unread, which is the half read_seq + 1
	// got right and the author filter must not lose. Three of the five above
	// the mark are bob's; that is the "3 NEW" the canvas draws.
	var unread int
	for _, m := range page.Messages {
		if m.Seq >= *direct && m.AuthorID != alice {
			unread++
		}
	}
	if unread != 3 {
		t.Errorf("the client's rule over this page counts %d unread, want 3", unread)
	}
}
