package main

// Shared fixtures for restoreprobe's own tests — restoreprobe_test.go (behind
// the restoreprobesmoke build tag) and restoreprobe_cli_test.go (in the
// ordinary go test ./... sweep). NO BUILD TAG HERE: a tagged file's own
// helpers are invisible to a build that omits the tag, so anything both
// files need lives in a file neither omission excludes.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mkGroupForProbe creates one group conversation with members already
// joined — the direct-insert shape provisionUsers already uses for soak's
// own shared room, restated here because that function's own room creation
// is private to one call site and takes no member list.
func mkGroupForProbe(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', $2)`, id, name); err != nil {
		t.Fatalf("mkGroupForProbe: %v", err)
	}
	for _, u := range members {
		if _, err := pool.Exec(ctx,
			`INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`, id, u); err != nil {
			t.Fatalf("mkGroupForProbe: add member: %v", err)
		}
	}
	return id
}

// truncateLogForTest mirrors cmd/catenary/restore_test.go's truncateLog
// exactly: server/ cannot import cmd/catenary, a main package, so the same
// four statements — every message above keep gone, last_seq and read_seq
// pulled back inside what remains, the counter at keep — are restated here
// rather than reached into a main package this module cannot see.
func truncateLogForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, keep int64) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`DELETE FROM messages WHERE log_seq > $1`, []any{keep}},
		{`UPDATE conversations c
		     SET last_seq = COALESCE((SELECT max(m.seq) FROM messages m WHERE m.conversation_id = c.id), 0)`, nil},
		{`UPDATE conversation_members cm
		     SET read_seq = LEAST(cm.read_seq, c.last_seq)
		    FROM conversations c
		   WHERE c.id = cm.conversation_id`, nil},
		{`UPDATE log_counter SET value = $1 WHERE id = 1`, []any{keep}},
	} {
		if _, err := tx.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatalf("truncate: %v\n%s", err, s.q)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
