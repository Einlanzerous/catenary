package store

// CANT-89 criterion 5 — the property the marker has to have, and the only test
// here that can falsify it.
//
// The claim: no marker that commits after a page was served can be at or below
// that page's high-water mark. It is Invariant 1 for this column, and it holds
// for the same reason log_seq's does — the draw takes the log_counter row lock
// inside the MUTATION'S OWN transaction and holds it to commit, so every value
// at or below a `SELECT value FROM log_counter` belongs to a finished
// transaction.
//
// Break that and you get a bigserial by another route. The failure is silent
// and permanent, and it is not the marker that is lost — it is the rename: the
// client's cursor moves past a number whose row had not been written yet, and
// no page after that can ever carry it, because `metadata_log_seq > $after` is
// false forever.
//
// TWO ARMS, and Arm 2 is the proof rather than the net. A guard that has only
// run against a correct implementation is a guard nobody has seen work, which
// is why logorder_test.go carries a scripted negative and why this does too.

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// badRename is the shape the real helper exists to prevent: the draw is its own
// transaction and commits, and the row is written afterwards. Every statement
// is individually reasonable and the pair is a bigserial.
func badRename(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var v int64
	mustScan(t, pool.QueryRow(ctx,
		`UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value`), &v)
	return v
}

func writeMarker(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conv uuid.UUID, v int64) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE conversations SET metadata_log_seq = $2 WHERE id = $1`, conv, v); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

// Arm 1 — the forced schedule, and it is the proof.
//
// Draw, then serve a page, then write. Against the bad shape the rename is
// gone from every page that follows; against the real helper the schedule
// cannot be reached at all, because the draw is not visible until the write
// commits with it.
func TestAMarkerDrawnOutsideItsTransactionIsLost(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	viewer := mkUser(ctx, t, pool, "viewer")
	conv := mkGroup(ctx, t, pool, "Kitchen Table", viewer)

	// The bad shape, step 1: the number is committed and the row is not.
	v := badRename(ctx, t, pool)

	// The client syncs in the gap and its cursor moves past that number.
	page, err := st.Sync(ctx, viewer, 0, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	cursor := page.HighWater
	if cursor < v {
		t.Fatalf("cursor %d is below the drawn marker %d; the schedule did not reach the state under test", cursor, v)
	}

	// The bad shape, step 2 — and it is already too late.
	writeMarker(ctx, t, pool, conv, v)

	after, err := st.Sync(ctx, viewer, cursor, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if convIDs(after)[conv] {
		t.Fatal("the rename arrived, so this schedule does not demonstrate the hazard and " +
			"Arm 2 below is proving nothing")
	}

	// THE SAME SCHEDULE AGAINST THE REAL HELPER. The draw is inside the
	// mutation's transaction, so there is no gap for the page to fall into and
	// the rename is on the very next page.
	inTx(ctx, t, pool, func(tx pgx.Tx) error {
		_, err := newMetadataBump().conversation(conv).apply(ctx, tx)
		return err
	})
	fixed, err := st.Sync(ctx, viewer, cursor, 50)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !convIDs(fixed)[conv] {
		t.Error("a rename through the helper did not reach a client whose cursor was already past " +
			"a number drawn before it — the property this file exists for")
	}
}

// Arm 2 — the randomized race, and it is the net rather than the proof.
//
// Renames in distinct conversations, concurrent with a client advancing its
// cursor. Nothing but log_counter orders the renames against each other, which
// is the same reason logorder_test.go races DISTINCT conversations: a shared
// row lock upstream would serialise them before the counter was consulted and
// the test would pass against anything.
//
// THE ASSERTION IS ON WHAT THE READER SAW, AND ONLY THAT. An earlier version
// unioned in a final catch-all `Sync` from the baseline cursor before counting,
// which made the arm unfalsifiable: every marker is above that baseline and a
// page with nothing to truncate on returns HighWater = head, so the catch-all
// alone satisfied the assertion and nothing the reader observed was
// load-bearing. Swapping the real bump for this file's own badRename/writeMarker
// pair still reported nothing missing. The claim this arm is written to make is
// "a client that keeps syncing never misses a rename", so the client's own
// observations have to be the whole of the evidence.
func TestEveryRenameReachesAClientThatKeepsSyncing(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	viewer := mkUser(ctx, t, pool, "viewer")
	const n = 12
	convs := make([]uuid.UUID, n)
	for i := range convs {
		convs[i] = mkGroup(ctx, t, pool, "room", viewer)
	}

	base, err := st.Sync(ctx, viewer, 0, 100)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}

	seen := map[uuid.UUID]bool{}
	var mu sync.Mutex
	renamesDone := make(chan struct{})
	readerDone := make(chan struct{})

	// The client, advancing its cursor monotonically the whole time. Once the
	// renames are finished it keeps asking until the cursor has settled — a
	// rename can commit as a page closes, and a client that stopped there
	// would be reporting its own timing rather than the server's property.
	go func() {
		defer close(readerDone)
		cursor := base.HighWater
		settled := 0
		for {
			page, err := st.Sync(ctx, viewer, cursor, 100)
			if err != nil {
				return
			}
			mu.Lock()
			for _, c := range page.Conversations {
				seen[c.ID] = true
			}
			mu.Unlock()
			cursor = page.HighWater

			select {
			case <-renamesDone:
				if settled++; settled >= 3 {
					return
				}
			default:
				settled = 0
			}
		}
	}()

	var wg sync.WaitGroup
	for _, conv := range convs {
		wg.Add(1)
		go func(conv uuid.UUID) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := newMetadataBump().conversation(conv).apply(ctx, tx); err != nil {
				t.Errorf("bump: %v", err)
				return
			}
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("commit: %v", err)
			}
		}(conv)
	}
	wg.Wait()
	close(renamesDone)
	<-readerDone

	mu.Lock()
	defer mu.Unlock()
	var missing int
	for _, conv := range convs {
		if !seen[conv] {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d of %d renames never reached a client that kept syncing throughout", missing, n)
	}
}

// One atomic change draws ONE marker, and every row it touched carries it.
//
// This is the observable consequence of locking every target before drawing.
// The shape it replaces drew per row, which meant a membership change — two
// rows, one event — recorded itself as two events, and, worse, held the counter
// while reaching for the second row's lock.
func TestOneChangeDrawsOneMarkerForEveryRowItTouches(t *testing.T) {
	ctx, pool := freshDB(t)

	viewer := mkUser(ctx, t, pool, "viewer")
	joiner := mkUser(ctx, t, pool, "joiner")
	a := mkGroup(ctx, t, pool, "A", viewer, joiner)
	b := mkGroup(ctx, t, pool, "B", viewer)

	before := head(ctx, t, pool)
	var drawn int64
	inTx(ctx, t, pool, func(tx pgx.Tx) error {
		var err error
		drawn, err = newMetadataBump().
			conversation(a).conversation(b).
			member(a, joiner).
			user(joiner).
			apply(ctx, tx)
		return err
	})

	if got := head(ctx, t, pool); got != before+1 {
		t.Errorf("counter moved %d → %d for one change touching four rows; want a single draw",
			before, got)
	}

	for _, q := range []struct {
		what string
		sql  string
		args []any
	}{
		{"conversations A", `SELECT metadata_log_seq FROM conversations WHERE id = $1`, []any{a}},
		{"conversations B", `SELECT metadata_log_seq FROM conversations WHERE id = $1`, []any{b}},
		{"the member row", `SELECT metadata_log_seq FROM conversation_members
			WHERE conversation_id = $1 AND user_id = $2`, []any{a, joiner}},
		{"the user row", `SELECT metadata_log_seq FROM users WHERE id = $1`, []any{joiner}},
	} {
		var got int64
		mustScan(t, pool.QueryRow(ctx, q.sql, q.args...), &got)
		if got != drawn {
			t.Errorf("%s carries marker %d, want the change's own %d", q.what, got, drawn)
		}
	}
}

// A multi-row bump racing the send path must not deadlock.
//
// The shape this replaces did: after its first row a per-row helper held
// log_counter and then asked for the second row's lock, while SendMessage holds
// `conversations(X)` at position 8 and draws the counter at position 10. Holder
// of the counter wanting a conversation, against holder of that conversation
// wanting the counter, is a cycle, and Postgres resolves it by aborting
// somebody's send — which is what messages.go's lock-order note exists to
// prevent and what it now describes accurately.
//
// A deadlock surfaces as 40P01 rather than as a wrong answer, so "no error over
// a run of races" is a real assertion rather than a hopeful one.
func TestAMultiRowBumpDoesNotDeadlockAgainstTheSendPath(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	author := mkUser(ctx, t, pool, "author")
	first := mkGroup(ctx, t, pool, "First", author)
	second := mkGroup(ctx, t, pool, "Second", author)

	const rounds = 24
	errs := make(chan error, rounds*2)
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		// The bump names both conversations, so it wants two row locks and the
		// counter — the case a per-row helper got wrong.
		go func() {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := newMetadataBump().conversation(first).conversation(second).apply(ctx, tx); err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
			}
		}()
		// The send takes conversations(second) and then draws.
		go func() {
			defer wg.Done()
			if _, err := st.SendMessage(ctx, NewMessage{
				ClientID: uuid.New(), ConversationID: second, AuthorID: author, Text: ptr("racing"),
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a bump racing the send path failed — a deadlock here is 40P01: %v", err)
	}
}

// A rename of a user racing that user's own sends must not deadlock, and this
// is the cycle NO ordering could have removed.
//
// SendMessage never locks a user row until position 11, when the insert takes
// KEY SHARE via `messages.author_id → users(id)` — AFTER the counter draw at
// position 10. A bump has to take its rows BEFORE its draw. So the counter is
// genuinely before `users` for one writer and after it for the other, and the
// only thing that makes them compose is the strength of the lock: FOR NO KEY
// UPDATE passes straight through KEY SHARE, while FOR UPDATE does not.
//
// TestAMultiRowBumpDoesNotDeadlockAgainstTheSendPath does not cover this — its
// bump names conversations only, and `conversations` IS ordered consistently
// with the send path. Against FOR UPDATE on the users row this fails with
// 40P01.
func TestAUserBumpDoesNotDeadlockAgainstThatUsersSends(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "Kitchen Table", author)

	const rounds = 24
	errs := make(chan error, rounds*2)
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := newMetadataBump().user(author).apply(ctx, tx); err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := st.SendMessage(ctx, NewMessage{
				ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("racing"),
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a user bump racing that user's sends failed — a deadlock here is 40P01: %v", err)
	}
}
