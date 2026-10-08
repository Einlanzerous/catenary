package store

// CANT-266 — migration 0011 adds conversations.create_key and the partial
// unique index over it, applies to a database that already holds
// conversations, and reverses cleanly.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigration0011AppliesToADatabaseHoldingConversations(t *testing.T) {
	ctx, pool := freshDB(t)
	if err := MigrateDown(ctx, pool, migrationsAfter(t, "0010")); err != nil {
		t.Fatalf("roll back to 0010: %v", err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', 'legacy')`, id); err != nil {
		t.Fatalf("arrange (pre-0011 insert): %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("0011 did not apply to a database holding a conversation: %v", err)
	}
	var key *string
	if err := pool.QueryRow(ctx, `SELECT create_key FROM conversations WHERE id = $1`, id).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != nil {
		t.Errorf("a pre-existing conversation was given create_key %q by the migration", *key)
	}
}

func TestConversationCreateKeyIsUniqueOnlyWhenSet(t *testing.T) {
	ctx, pool := freshDB(t)
	insert := func(key any) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO conversations (id, kind, name, create_key) VALUES ($1, 'group', 'g', $2)`, uuid.New(), key)
		return err
	}
	// Any number of keyless rows: the index is partial.
	for i := 0; i < 3; i++ {
		if err := insert(nil); err != nil {
			t.Fatalf("keyless insert %d: %v", i, err)
		}
	}
	creator, request := uuid.NewString(), uuid.NewString()
	key := creator + "|" + request
	if err := insert(key); err != nil {
		t.Fatalf("first keyed insert: %v", err)
	}
	err := insert(key)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "conversations_create_key_idx" {
		t.Fatalf("duplicate create_key = %v, want a 23505 on conversations_create_key_idx", err)
	}
	// The same request id from a different creator is a different key.
	if err := insert(uuid.NewString() + "|" + request); err != nil {
		t.Fatalf("same request id, different creator: %v", err)
	}
}

func TestMigration0011DownRemovesTheColumnAndIndex(t *testing.T) {
	ctx, pool := freshDB(t)
	if err := MigrateDown(ctx, pool, migrationsAfter(t, "0010")); err != nil {
		t.Fatalf("down: %v", err)
	}
	var col, idx bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'conversations' AND column_name = 'create_key'),
		       to_regclass('conversations_create_key_idx') IS NOT NULL`).Scan(&col, &idx); err != nil {
		t.Fatal(err)
	}
	if col || idx {
		t.Errorf("after 0011 down: create_key column present = %v, index present = %v, want neither", col, idx)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := publicTableCount(ctx, t, pool); n != plannedTableCount {
		t.Errorf("%d tables after down-then-up, want %d — 0011 adds no table", n, plannedTableCount)
	}
}
