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
		_, err := bumpConversationMetadata(ctx, tx, conv)
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
	done := make(chan struct{})

	// The client, advancing its cursor the whole time the renames run.
	go func() {
		cursor := base.HighWater
		for {
			select {
			case <-done:
				return
			default:
			}
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
		}
	}()

	var wg sync.WaitGroup
	for _, conv := range convs {
		wg.Add(1)
		go func(conv uuid.UUID) {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := bumpConversationMetadata(ctx, tx, conv); err != nil {
				return
			}
			_ = tx.Commit(ctx)
		}(conv)
	}
	wg.Wait()
	close(done)

	// One final page from wherever the reader got to. Whatever the race did,
	// every rename must be reachable by a client that is still asking.
	final, err := st.Sync(ctx, viewer, base.HighWater, 100)
	if err != nil {
		t.Fatalf("final sync: %v", err)
	}
	mu.Lock()
	for _, c := range final.Conversations {
		seen[c.ID] = true
	}
	mu.Unlock()

	var missing int
	for _, conv := range convs {
		if !seen[conv] {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d of %d renames never reached the client", missing, n)
	}
}
