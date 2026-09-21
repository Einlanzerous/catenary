package store

// CANT-130 criterion 8 — migration 0010 applies to a database that already
// holds the people soakrig created, carries exactly one CHECK, and nothing
// generated ever learns about the column it adds.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// freshDBAt0009 gives the test a database migrated up to and including 0009
// — the schema every person soakrig ever created was inserted against —
// rolled back one step from freshDB's full set, so a test can insert exactly
// what pre-0010 code inserts and then apply 0010 against it.
func freshDBAt0009(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, pool := freshDB(t)
	if err := MigrateDown(ctx, pool, 1); err != nil {
		t.Fatalf("roll back to 0009: %v", err)
	}
	return ctx, pool
}

func TestMigration0010AppliesToADatabaseHoldingEmaillessPersons(t *testing.T) {
	ctx, pool := freshDBAt0009(t)

	// Exactly soakrig's own insert (server/cmd/soakrig/provision.go's
	// insertUser), against the pre-0010 schema.
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, handle, display_name) VALUES ($1, $2, $3)`, id, "soak-legacy", "soak-legacy"); err != nil {
		t.Fatalf("arrange (pre-0010 insert): %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("0010 did not apply to a database holding an email-less person: %v", err)
	}

	var email *string
	if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, id).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != nil {
		t.Error("a pre-existing row was given a non-NULL email by the migration")
	}
}

func TestSoakrigsExactInsertStillSucceedsAfter0010(t *testing.T) {
	ctx, pool := freshDB(t) // fully migrated, including 0010

	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, handle, display_name) VALUES ($1, $2, $3)`, id, "soak-1", "soak-1"); err != nil {
		t.Fatalf("soakrig's own insert no longer works after 0010: %v", err)
	}
}

func TestABotCannotHoldAnEmail(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}

	_, err = pool.Exec(ctx, `UPDATE users SET email = 'argosy@example.com' WHERE id = $1`, bot)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("giving a bot an email = %v, want a 23514 CHECK violation", err)
	}
	if pgErr.ConstraintName != usersBotHasNoEmailConstraint {
		t.Errorf("violated constraint %q, want %q", pgErr.ConstraintName, usersBotHasNoEmailConstraint)
	}
}

func TestEmailIsUniqueCaseInsensitively(t *testing.T) {
	ctx, pool := freshDB(t)
	mkUser(ctx, t, pool, "alice")
	if _, err := pool.Exec(ctx, `UPDATE users SET email = 'alice@example.com' WHERE handle = 'alice'`); err != nil {
		t.Fatal(err)
	}
	mkUser(ctx, t, pool, "alice2")

	_, err := pool.Exec(ctx, `UPDATE users SET email = 'ALICE@EXAMPLE.COM' WHERE handle = 'alice2'`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("a case-different duplicate email = %v, want a 23505 unique violation", err)
	}
	if pgErr.ConstraintName != usersEmailLowerIndexConstraint {
		t.Errorf("violated constraint %q, want %q", pgErr.ConstraintName, usersEmailLowerIndexConstraint)
	}
}

func TestMigration0010DownRemovesTheColumnIndexAndCheck(t *testing.T) {
	ctx, pool := freshDB(t)

	if err := MigrateDown(ctx, pool, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email')`).
		Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("users.email survived 0010's down migration")
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := publicTableCount(ctx, t, pool); n != plannedTableCount {
		t.Errorf("%d tables after down-then-up, want %d — 0010 adds no table, only alters users", n, plannedTableCount)
	}
}

// No wire type — Go, TypeScript or Dart — ever gains an email field for a
// user. The wire schema itself is not this row's to edit; this is the guard
// against a hand edit, or a schema change nobody meant, slipping one in.
func TestNoGeneratedWirePackageMentionsEmail(t *testing.T) {
	root := moduleRoot(t)
	for _, rel := range []string{
		filepath.Join("internal", "wire", "generated.go"),
		filepath.Join("web", "src", "wire", "generated.ts"),
		filepath.Join("dart", "lib", "src", "generated.dart"),
	} {
		path := filepath.Join(root, rel)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.Contains(strings.ToLower(string(src)), "email") {
			t.Errorf("%s mentions email — no wire type carries one (CLAUDE.md invariant 3, D1's honesty argument)", rel)
		}
	}
}
