package store

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"sync"
	"testing"
)

// Two processes starting together against a fresh database both read an empty
// schema_migrations. Without the advisory lock the loser re-runs 0001 and its
// CREATE TABLE raises 42P07, which surfaces as a boot failure.
func TestConcurrentMigrateIsSafe(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := MigrateDown(ctx, pool, 0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// A COLD start, which is the scenario this test is named for. MigrateDown
	// leaves schema_migrations behind — no .down.sql drops it — so without this
	// every migrator finds the table present, CREATE TABLE IF NOT EXISTS is a
	// no-op, and the race in ensureTable is never reached. The first version of
	// this test proved the lock in applyOne and nothing else.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS schema_migrations`); err != nil {
		t.Fatalf("drop bookkeeping table: %v", err)
	}

	const n = 6
	pools := make([]*pgxpool.Pool, n)
	for i := range n {
		p, err := Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		defer p.Close()
		pools[i] = p
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = Migrate(ctx, pools[i])
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("migrator %d failed under concurrency: %v", i, err)
		}
	}
	if n := publicTableCount(ctx, t, pool); n != 7 {
		t.Errorf("%d tables after %d concurrent migrators, want 7", n, 6)
	}
	// Derived rather than a literal: this assertion is about DUPLICATES, not
	// about how many migrations exist, and a literal makes every future
	// migration look like a concurrency regression.
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	applied, _ := AppliedVersions(ctx, pool)
	if len(applied) != len(migs) {
		t.Errorf("applied = %v, want %d with no duplicates", applied, len(migs))
	}
}
