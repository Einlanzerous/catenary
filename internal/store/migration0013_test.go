package store

// CANT-309 — migration 0013 creates uploads: applies, rolls back, and holds the
// two constraints the resolver and the account lifecycle lean on.

import (
	"testing"

	"github.com/google/uuid"
)

func TestMigration0013CreatesUploadsAndDownDropsIt(t *testing.T) {
	ctx, pool := freshDB(t)

	exists := func() bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'uploads'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !exists() {
		t.Fatal("0013 did not create uploads")
	}
	// 0014 and 0015 sit above 0013 and are rolled back first.
	if err := MigrateDown(ctx, pool, migrationsAfter(t, "0012")); err != nil {
		t.Fatalf("0013 down: %v", err)
	}
	if exists() {
		t.Fatal("0013 down left uploads behind")
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("0013 up again: %v", err)
	}
	if !exists() {
		t.Fatal("0013 did not re-apply")
	}
}

func TestUploadsConstraints(t *testing.T) {
	ctx, pool := freshDB(t)
	user := insertTestUser(ctx, t, pool, "uploader-0013")

	good := func() error {
		_, err := pool.Exec(ctx,
			`INSERT INTO uploads (id, uploader, kind, storage_key, bytes) VALUES ($1, $2, 'voice', 'k', 1)`,
			uuid.New(), user)
		return err
	}
	if err := good(); err != nil {
		t.Fatalf("a plain voice upload row: %v", err)
	}

	cases := map[string]string{
		"unknown kind":       `INSERT INTO uploads (id, uploader, kind, storage_key, bytes) VALUES (gen_random_uuid(), $1, 'video', 'k', 1)`,
		"negative bytes":     `INSERT INTO uploads (id, uploader, kind, storage_key, bytes) VALUES (gen_random_uuid(), $1, 'voice', 'k', -1)`,
		"zero width":         `INSERT INTO uploads (id, uploader, kind, storage_key, bytes, width) VALUES (gen_random_uuid(), $1, 'image', 'k', 1, 0)`,
		"peak over 100":      `INSERT INTO uploads (id, uploader, kind, storage_key, bytes, peaks) VALUES (gen_random_uuid(), $1, 'voice', 'k', 1, '{101}')`,
		"more than 512 bars": `INSERT INTO uploads (id, uploader, kind, storage_key, bytes, peaks) VALUES (gen_random_uuid(), $1, 'voice', 'k', 1, array_fill(1::smallint, ARRAY[513]))`,
	}
	for name, stmt := range cases {
		if _, err := pool.Exec(ctx, stmt, user); err == nil {
			t.Errorf("%s: the CHECK accepted it", name)
		}
	}

	// uploader is ON DELETE RESTRICT: an account with uploads cannot be deleted
	// out from under the handles that name it.
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err == nil {
		t.Error("deleting a user with uploads succeeded; want ON DELETE RESTRICT")
	}
}
