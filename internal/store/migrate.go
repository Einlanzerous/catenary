package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/migrations"
)

// migration is a single parsed migration, both directions.
type migration struct {
	version string // numeric prefix, e.g. "0001"
	name    string // base name, e.g. "0001_identity"
	up      string
	down    string // required; an up with no down is refused at load
}

// Migrate applies every pending up migration in ascending version order.
//
// Each runs in its own transaction alongside its schema_migrations row, so the
// two can never disagree. Idempotent: already-applied versions are skipped, so
// calling it on every boot is the intended use and not merely tolerated. A
// migrator that is idempotent only by convention fails on the first restart
// loop, which is the first time anybody finds out.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if err := ensureTable(ctx, pool); err != nil {
		return err
	}
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}
	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		if err := applyOne(ctx, pool, m.up, m.version, true); err != nil {
			return fmt.Errorf("store: apply %s up: %w", m.name, err)
		}
	}
	return nil
}

// MigrateDown rolls back the n most recently applied migrations, newest first.
// n <= 0 rolls back everything.
//
// The mechanical reversibility is real and CANT-13 proves it with an
// up-down-up round trip. What stops being true the moment there is a message
// worth keeping is that RUNNING it is survivable: 0003 down drops the log.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool, n int) error {
	if err := ensureTable(ctx, pool); err != nil {
		return err
	}
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].version > migs[j].version })

	done := 0
	for _, m := range migs {
		if n > 0 && done >= n {
			break
		}
		if !applied[m.version] {
			continue
		}
		if err := applyOne(ctx, pool, m.down, m.version, false); err != nil {
			return fmt.Errorf("store: apply %s down: %w", m.name, err)
		}
		done++
	}
	return nil
}

// AppliedVersions reports which migration versions the database has recorded,
// in ascending order. Exported for `catenary migrate status`.
func AppliedVersions(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	if err := ensureTable(ctx, pool); err != nil {
		return nil, err
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(applied))
	for v := range applied {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

func ensureTable(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("store: ensure schema_migrations: %w", err)
	}
	return nil
}

// migrateLockKey is an arbitrary constant, shared by every process running this
// migrator against one database. Any value works; it only has to be the same.
const migrateLockKey int64 = 0x43414e54 // "CANT"

// applyOne runs one migration body and records or removes its version in a
// single transaction, so a failed rollback leaves the version recorded as
// still applied rather than leaving the two halves disagreeing.
//
// It takes a transaction-scoped advisory lock first. Without one, two processes
// starting together against a fresh database both read an empty
// schema_migrations and both run 0001; the loser's CREATE TABLE raises 42P07,
// Migrate returns it and runServe exits 1. Single-instance today, so it would
// recover on the restart — but `catenary migrate up` typed by hand while the
// container is booting is the same race, and it is the moment somebody is
// already worried about the database. The lock is released at commit or
// rollback either way.
func applyOne(ctx context.Context, pool *pgxpool.Pool, body, version string, up bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey); err != nil {
		return fmt.Errorf("store: take migration lock: %w", err)
	}

	// Re-read under the lock: the process that waited here may be about to
	// re-run a migration the winner has already applied and committed.
	var already bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&already); err != nil {
		return err
	}
	if already == up {
		return nil // already applied, or already rolled back
	}

	if _, err := tx.Exec(ctx, body); err != nil {
		return err
	}
	if up {
		_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// loadMigrations reads every embedded pair and fails loudly on a missing down
// file. An up migration with no down is one nobody can reverse, and finding
// that out during an incident is the wrong time.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations dir: %w", err)
	}

	var migs []migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		base := strings.TrimSuffix(name, ".up.sql")
		version, _, ok := strings.Cut(base, "_")
		if !ok || version == "" {
			return nil, fmt.Errorf("store: migration %q lacks a version prefix", name)
		}
		up, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %w", name, err)
		}
		down, err := fs.ReadFile(migrations.FS, base+".down.sql")
		if err != nil {
			return nil, fmt.Errorf("store: %s has no .down.sql: %w", base, err)
		}
		migs = append(migs, migration{
			version: version, name: base,
			up: string(up), down: string(down),
		})
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	return migs, nil
}
