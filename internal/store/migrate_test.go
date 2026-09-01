package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDSN returns the test database DSN or skips. CI and verify.sh inject it;
// there is no fallback baked into the tests, because a test that silently
// points at a developer's own database is a test that eventually drops it.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CATENARY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATENARY_TEST_DATABASE_URL not set; skipping database test")
	}
	return dsn
}

// freshDB gives the test an empty database with every migration applied.
func freshDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := MigrateDown(ctx, pool, 0); err != nil {
		t.Fatalf("reset (down): %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, pool
}

func publicTableCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`).Scan(&n)
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	return n
}

func TestLoadMigrationsHasBothDirections(t *testing.T) {
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) != 3 {
		t.Fatalf("loaded %d migrations, want 3", len(migs))
	}
	for _, m := range migs {
		if m.up == "" {
			t.Errorf("%s: empty up migration", m.name)
		}
		// An up with no down is a migration nobody can reverse. loadMigrations
		// already refuses one; assert it so a future loosening breaks loudly.
		if m.down == "" {
			t.Errorf("%s: empty down migration", m.name)
		}
	}
	if migs[0].version != "0001" || migs[2].version != "0003" {
		t.Errorf("migrations are not in ascending version order: %v", migs)
	}
}

// Criterion 13, first half. Boot applies migrations on every start, so a
// migrator that is idempotent only by convention fails on the first restart
// loop — which is the first time anybody finds out.
func TestMigrateIsIdempotent(t *testing.T) {
	ctx, pool := freshDB(t)

	before := publicTableCount(ctx, t, pool)
	for i := range 3 {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("migrate run %d: %v", i+2, err)
		}
	}
	if after := publicTableCount(ctx, t, pool); after != before {
		t.Errorf("table count moved across repeat migrations: %d -> %d", before, after)
	}

	applied, err := AppliedVersions(ctx, pool)
	if err != nil {
		t.Fatalf("applied: %v", err)
	}
	if len(applied) != 3 {
		t.Errorf("applied versions = %v, want 3 rows and no duplicates", applied)
	}
}

// Criterion 13, second half. Rev 2 of the plan asserted reversibility and no
// criterion touched it; an untested .down.sql is worse than an absent one,
// because it invites someone to run it.
func TestMigrateUpDownUpRoundTrip(t *testing.T) {
	ctx, pool := freshDB(t)

	if n := publicTableCount(ctx, t, pool); n != 7 {
		t.Fatalf("after up: %d tables, want 7 (6 + log_counter)", n)
	}
	if err := MigrateDown(ctx, pool, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := publicTableCount(ctx, t, pool); n != 0 {
		t.Fatalf("after down: %d tables, want 0", n)
	}
	if v, err := AppliedVersions(ctx, pool); err != nil || len(v) != 0 {
		t.Fatalf("after down: applied = %v, err = %v; want empty", v, err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := publicTableCount(ctx, t, pool); n != 7 {
		t.Fatalf("after up again: %d tables, want 7", n)
	}

	// The counter is seeded by the migration, not by the first insert. A
	// re-applied 0003 that forgot the seed would leave nothing to draw from.
	var id, value int64
	if err := pool.QueryRow(ctx, `SELECT id, value FROM log_counter`).Scan(&id, &value); err != nil {
		t.Fatalf("log_counter after round trip: %v", err)
	}
	if id != 1 || value != 0 {
		t.Errorf("log_counter = (%d, %d), want (1, 0)", id, value)
	}
}

// Rolling back one at a time, newest first.
func TestMigrateDownIsIncremental(t *testing.T) {
	ctx, pool := freshDB(t)

	if err := MigrateDown(ctx, pool, 1); err != nil {
		t.Fatalf("down 1: %v", err)
	}
	applied, _ := AppliedVersions(ctx, pool)
	if len(applied) != 2 || applied[len(applied)-1] != "0002" {
		t.Errorf("after down 1: applied = %v, want [0001 0002]", applied)
	}
	// 0003's tables are gone; 0002's remain.
	for table, want := range map[string]bool{"messages": false, "attachments": false, "log_counter": false, "conversations": true, "users": true} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("to_regclass(%s): %v", table, err)
		}
		if exists != want {
			t.Errorf("after down 1: %s exists = %v, want %v", table, exists, want)
		}
	}
}
