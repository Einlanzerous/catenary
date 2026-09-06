package store

// CANT-83 — the half of criterion 10 that needs a real database.
//
// The criterion asks for a REAL drop rather than a synthetic SQLSTATE, and this
// is why: a terminated backend does not surface as class 08 and does not
// surface as a bare connection error. It surfaces as a PgError with SQLSTATE
// 57P01 at FATAL severity, and the first version of isTransient classified that
// permanent — which would have parked a perfectly good message at `failed`
// while the operator was doing nothing worse than restarting Postgres.
//
// Kept in stage 1 rather than deferred to stage 3, because a classification bug
// that only a database can see is a classification bug that comes back.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestTerminatedBackendIsClassifiedTransient(t *testing.T) {
	dsn := os.Getenv("CATENARY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATENARY_TEST_DATABASE_URL unset — needs a real Postgres 16")
	}
	ctx := context.Background()

	victim, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect victim: %v", err)
	}
	defer func() { _ = victim.Close(context.Background()) }()

	killer, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect killer: %v", err)
	}
	defer func() { _ = killer.Close(context.Background()) }()

	tx, err := victim.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read backend pid: %v", err)
	}
	if _, err := killer.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate backend %d: %v", pid, err)
	}
	// The termination is asynchronous; the victim learns about it on its next
	// exchange with the server.
	time.Sleep(150 * time.Millisecond)

	// The NEXT STATEMENT is the case that was wrong. It comes back as a
	// PgError, so isTransient's early return decides it, and 57P01 is in
	// neither class 08 nor the two 40-codes.
	_, stmtErr := tx.Exec(ctx, `SELECT 1`)
	if stmtErr == nil {
		t.Fatal("the statement after a terminate succeeded; the backend was not killed")
	}
	var pgErr *pgconn.PgError
	if !errors.As(stmtErr, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError after a terminate, got %T: %v", stmtErr, stmtErr)
	}
	// Pinned, because the widening above is built on these two values and a
	// future pgx or Postgres changing them should say so here rather than by
	// silently reverting the classification.
	if pgErr.Code != "57P01" {
		t.Errorf("SQLSTATE = %q, want 57P01 (admin_shutdown)", pgErr.Code)
	}
	if sev := firstNonEmpty(pgErr.SeverityUnlocalized, pgErr.Severity); sev != "FATAL" {
		t.Errorf("severity = %q, want FATAL", sev)
	}
	if got := SendErrorFor(stmtErr); !got.Retryable {
		t.Errorf("a terminated backend was classified permanent (%v) — criterion 10's own test would fail", got)
	}

	// And the COMMIT that follows, which takes a different path entirely: no
	// PgError at all, pgconn's connLockError, caught by SafeToRetry.
	commitErr := tx.Commit(ctx)
	if commitErr == nil {
		t.Fatal("commit succeeded on a terminated backend")
	}
	if errors.As(commitErr, &pgErr) {
		t.Errorf("expected no PgError on the commit, got SQLSTATE %q", pgErr.Code)
	}
	if got := SendErrorFor(commitErr); !got.Retryable {
		t.Errorf("the commit after a terminate was classified permanent (%v)", got)
	}
}
