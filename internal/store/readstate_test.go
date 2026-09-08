package store

// CANT-26 — what a receipt is allowed to claim, and the one derivation the
// badge and the "N NEW" divider both come from.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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

// read_by COUNTS THE SAME POPULATION member_count DOES, or the fraction the
// room renders can never reach n/n.
//
// The first version of this excluded the author from the numerator while
// Conversation.member_count counted everybody. In a three-member room where all
// three had read a message the label said READ 2/3 — the author told one member
// had not seen it when everybody had, permanently, because 3/3 was unreachable.
// The canvas's reference data draws READ 7/7 and READ 9/9, which that query
// could not produce.
//
// The author counts from the moment the message exists and without a receipt,
// because sending does not advance read_seq — that was built and removed in
// CANT-83 for swallowing the sender's own unread backlog (0005). So `1` here
// before anybody has read anything is the correct floor, not an off-by-one.
func TestReadByCountsEveryMemberIncludingTheAuthor(t *testing.T) {
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
	members := func() int64 {
		t.Helper()
		page, err := st.Sync(ctx, bob, 0, 100)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		return page.Conversations[0].MemberCount
	}

	if got := readBy(); got != 1 {
		t.Errorf("read_by = %d before anyone else read it, want 1 — the author", got)
	}
	// The author sending a receipt for their own message changes nothing: they
	// were already counted, and double-counting them would put the numerator
	// above the denominator.
	if _, err := st.MarkRead(ctx, conv, bob, m.Seq); err != nil {
		t.Fatalf("bob reads his own: %v", err)
	}
	if got := readBy(); got != 1 {
		t.Errorf("read_by = %d after the author's own receipt, want 1", got)
	}
	if _, err := st.MarkRead(ctx, conv, alice, m.Seq); err != nil {
		t.Fatalf("alice reads: %v", err)
	}
	if got := readBy(); got != 2 {
		t.Errorf("read_by = %d after one other member read it, want 2", got)
	}
	if _, err := st.MarkRead(ctx, conv, carol, m.Seq); err != nil {
		t.Fatalf("carol reads: %v", err)
	}

	// The claim that matters: n/n is reachable.
	if got, want := readBy(), members(); got != want {
		t.Errorf("read_by = %d of member_count %d after EVERY member read it — "+
			"the fraction can never close", got, want)
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

// `Advanced` MEANS "THIS CALL MOVED IT", and two of one member's devices are
// what makes that hard.
//
// The single-statement version read the held mark from the statement's own
// snapshot while the SET re-read the row after the lock was granted. So with
// two receipts in flight for the same member, the loser saw the OLD mark as
// "before" and the winner's value as "after", and reported Advanced for a move
// it had not made — which is exactly the redundant broadcast the field exists
// to suppress. Reading the mark under the same lock the write takes is what
// makes the question answerable.
//
// Staged rather than raced: a transaction holds the row, MarkRead blocks on it,
// the holder moves the mark past what MarkRead is claiming and commits. Under
// the old shape this reported Advanced: true.
func TestAdvancedIsFalseWhenAnotherDeviceGotThereFirst(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	alice := mkUser(ctx, t, pool, "alice")
	bob := mkUser(ctx, t, pool, "bob")
	conv := mkGroup(ctx, t, pool, "room", alice, bob)
	for i := 0; i < 6; i++ {
		send(ctx, t, st, conv, bob, "backlog")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		SELECT read_seq FROM conversation_members
		 WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, conv, alice); err != nil {
		t.Fatalf("hold the row: %v", err)
	}

	type result struct {
		r   ReadReceipt
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := st.MarkRead(ctx, conv, alice, 3)
		done <- result{r, err}
	}()
	waitForLockWaiter(ctx, t, pool)

	// The other device gets there first, with a higher mark.
	if _, err := tx.Exec(ctx, `
		UPDATE conversation_members SET read_seq = 5
		 WHERE conversation_id = $1 AND user_id = $2`, conv, alice); err != nil {
		t.Fatalf("the other device's write: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("mark read: %v", got.err)
		}
		if got.r.UpToSeq != 5 {
			t.Errorf("up_to_seq = %d, want 5 — the mark, not the claim", got.r.UpToSeq)
		}
		if got.r.Advanced {
			t.Error("advanced = true for a mark another device had already moved past; " +
				"this is the redundant broadcast the field exists to suppress")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("MarkRead never returned; it is still waiting on a lock nobody holds")
	}
}

// waitForLockWaiter blocks until some session is waiting on a lock, so the
// staging above does not depend on a sleep being long enough.
func waitForLockWaiter(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		mustScan(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`), &n)
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session ever blocked on the row lock")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
