package store

// CANT-89 — the marker as /sync actually serves it, against a real Postgres.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// inTx runs one bump the way a mutation would: on a transaction it commits.
func inTx(ctx context.Context, t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx) error) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func convIDs(page SyncPage) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, c := range page.Conversations {
		out[c.ID] = true
	}
	return out
}

// Criterion 0. The case the whole ticket is named for: a conversation whose
// metadata changed, on a page carrying no message of its own — and the
// membership guard, which is the half a bare marker clause would lose.
func TestAChangedConversationIsOnThePageWithNoMessageOfItsOwn(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	viewer := mkUser(ctx, t, pool, "viewer")
	other := mkUser(ctx, t, pool, "other")
	mine := mkGroup(ctx, t, pool, "Kitchen Table", viewer, other)
	theirs := mkGroup(ctx, t, pool, "Not Mine", other)

	base, err := st.Sync(ctx, viewer, 0, 50)
	if err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	// A rename in each. The viewer is a member of one of them.
	//
	// ONE bump naming both rows, not two bumps. Two would hold log_counter
	// while asking for the second conversation's row lock, which is the cycle
	// against SendMessage that metadata.go's header describes — and this test
	// is where that caller was first written.
	inTx(ctx, t, pool, func(tx pgx.Tx) error {
		_, err := newMetadataBump().conversation(mine).conversation(theirs).apply(ctx, tx)
		return err
	})

	page, err := st.Sync(ctx, viewer, base.HighWater, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(page.Messages) != 0 {
		t.Fatalf("page carries %d messages; the point is that it carries none", len(page.Messages))
	}
	got := convIDs(page)
	if !got[mine] {
		t.Error("the renamed conversation the viewer is in is absent — this is the ticket")
	}
	if got[theirs] {
		t.Error("a conversation the viewer is NOT a member of was served; the marker clause dropped the membership join")
	}
}

// Criterion 1, ruling 3. Both halves, because the second is the one a bare
// `metadata_log_seq > $after` would get wrong.
func TestARenamedUserReachesOnlyAViewerWhoSharesARoom(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	viewer := mkUser(ctx, t, pool, "viewer")
	roommate := mkUser(ctx, t, pool, "roommate")
	stranger := mkUser(ctx, t, pool, "stranger")
	mkGroup(ctx, t, pool, "Shared", viewer, roommate)
	mkGroup(ctx, t, pool, "Elsewhere", stranger)

	base, err := st.Sync(ctx, viewer, 0, 50)
	if err != nil {
		t.Fatalf("baseline sync: %v", err)
	}

	inTx(ctx, t, pool, func(tx pgx.Tx) error {
		_, err := newMetadataBump().user(roommate).user(stranger).apply(ctx, tx)
		return err
	})

	page, err := st.Sync(ctx, viewer, base.HighWater, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	seen := map[uuid.UUID]bool{}
	for _, u := range page.Users {
		seen[u.ID] = true
	}
	if !seen[roommate] {
		t.Error("a renamed user the viewer shares a room with is absent; the rename lands nowhere")
	}
	if seen[stranger] {
		t.Error("a renamed user the viewer shares NO room with was served — ruling 3 says the reachability guard stays on")
	}
}

// Criterion 2. The bound is the cursor the client is about to be handed, not
// the head Sync read at the top.
func TestTheMarkerClauseIsBoundedByThePagesHighWater(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	viewer := mkUser(ctx, t, pool, "viewer")
	other := mkUser(ctx, t, pool, "other")
	talkative := mkGroup(ctx, t, pool, "Talkative", viewer, other)
	quiet := mkGroup(ctx, t, pool, "Quiet", viewer, other)

	for i := 0; i < 5; i++ {
		send(ctx, t, st, talkative, other, "chatter")
	}
	// The rename happens AFTER those messages, so its marker is above the
	// high-water mark of any page that truncates inside them.
	inTx(ctx, t, pool, func(tx pgx.Tx) error {
		_, err := newMetadataBump().conversation(quiet).apply(ctx, tx)
		return err
	})

	page, err := st.Sync(ctx, viewer, 0, 2)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !page.HasMore {
		t.Fatal("page did not truncate; the test cannot say anything")
	}
	if convIDs(page)[quiet] {
		t.Error("a conversation whose marker is above this page's high water was served; " +
			"the clause is bounded by head rather than by the cursor the client gets back")
	}

	// And it arrives once the cursor reaches it.
	for {
		page, err = st.Sync(ctx, viewer, page.HighWater, 2)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if convIDs(page)[quiet] {
			return
		}
		if !page.HasMore {
			t.Fatal("the renamed conversation never arrived, on any page")
		}
	}
}

// Criterion 3, both halves. The second is what justifies the guard test: a row
// left at the default is invisible to every cursor there is.
func TestAnEmptyConversationIsDiscoverableOnlyIfItsCreationDrew(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	viewer := mkUser(ctx, t, pool, "viewer")
	// mkGroup inserts directly, so this row carries the DEFAULT 0 — exactly
	// what a creation path that forgot to draw would leave behind.
	conv := mkGroup(ctx, t, pool, "Silent", viewer)

	page, err := st.Sync(ctx, viewer, 0, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if convIDs(page)[conv] {
		t.Fatal("a conversation at metadata_log_seq 0 was served on a bootstrap; " +
			"then DEFAULT 0 would be safe and the guard test would have nothing to protect")
	}

	inTx(ctx, t, pool, func(tx pgx.Tx) error {
		_, err := newMetadataBump().conversation(conv).apply(ctx, tx)
		return err
	})

	page, err = st.Sync(ctx, viewer, 0, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !convIDs(page)[conv] {
		t.Error("an empty conversation whose creation drew is still undiscoverable on a bootstrap")
	}
}

// Criterion 4. A draw and a read differ in exactly one case, and it is the case
// that matters: the client that is already caught up.
func TestTheBackfillDrawsRatherThanReadingTheCounter(t *testing.T) {
	ctx, pool := freshDB(t)

	// Roll 0006 back, so rows exist as they would have before the migration.
	if err := MigrateDown(ctx, pool, 1); err != nil {
		t.Fatalf("roll back 0006: %v", err)
	}
	viewer := mkUser(ctx, t, pool, "viewer")
	conv := mkGroup(ctx, t, pool, "Existing", viewer)

	// A fully caught-up client's cursor IS the counter's committed value:
	// Sync sets HighWater = head when has_more is false.
	headAtMigration := head(ctx, t, pool)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("re-apply 0006: %v", err)
	}

	var marker int64
	mustScan(t, pool.QueryRow(ctx,
		`SELECT metadata_log_seq FROM conversations WHERE id = $1`, conv), &marker)
	if marker <= headAtMigration {
		t.Fatalf("backfilled marker is %d and head at migration was %d; a caught-up client sends "+
			"after=%d and never sees this row again — which is the DEFAULT 0 hole one value later",
			marker, headAtMigration, headAtMigration)
	}

	// The assertion that matters, end to end: that client's next page carries it.
	st := New(pool, DefaultLimits(), discardLogger())
	page, err := st.Sync(ctx, viewer, headAtMigration, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !convIDs(page)[conv] {
		t.Error("a client whose cursor equals head-at-migration did not receive its conversation")
	}
}

// Ruling 1 option 0. The ticket's first bullet, as a test: read on one device,
// and the conversation comes back on the same member's next page from another.
func TestAReceiptBringsTheConversationBackForThatMembersOtherDevices(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	viewer := mkUser(ctx, t, pool, "viewer")
	other := mkUser(ctx, t, pool, "other")
	conv := mkGroup(ctx, t, pool, "Kitchen Table", viewer, other)

	last := send(ctx, t, st, conv, other, "something to read")

	// The laptop catches up to here and then goes quiet.
	base, err := st.Sync(ctx, viewer, 0, 50)
	if err != nil {
		t.Fatalf("baseline sync: %v", err)
	}
	if base.Conversations[0].FirstUnreadSeq == nil {
		t.Fatal("nothing unread at the baseline; the test cannot say anything")
	}

	// The phone reads the thread.
	if _, err := st.MarkRead(ctx, conv, viewer, last.Seq); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	page, err := st.Sync(ctx, viewer, base.HighWater, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !convIDs(page)[conv] {
		t.Fatal("reading on another device did not bring the conversation back; the badge stays lit")
	}
	if page.Conversations[0].FirstUnreadSeq != nil {
		t.Errorf("first_unread_seq is %d on the refreshed row, want absent",
			*page.Conversations[0].FirstUnreadSeq)
	}

	// A receipt that does not advance must not draw: no-op receipts would enter
	// the deployment-wide serialised section to record that nothing happened.
	before := head(ctx, t, pool)
	if _, err := st.MarkRead(ctx, conv, viewer, last.Seq); err != nil {
		t.Fatalf("duplicate mark read: %v", err)
	}
	after := head(ctx, t, pool)
	if after != before {
		t.Errorf("a duplicate receipt drew from the counter (%d → %d); Advanced is supposed to gate it",
			before, after)
	}
}
