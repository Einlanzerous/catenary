package store

// CANT-254 / CANT-256 — migration 0012 widens conversations.kind to admit
// 'self', adds the per-person partial unique index over direct_key, and its
// down migration deletes the contents of every self conversation and nothing
// else. The down is the part that destroys authored messages, so the tests
// that matter most here are the ones that count what survives it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// freshDBAt0011 is a migrated database rolled back to the schema before 0012.
func freshDBAt0011(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, pool := freshDB(t)
	if err := MigrateDown(ctx, pool, migrationsAfter(t, "0011")); err != nil {
		t.Fatalf("roll back to 0011: %v", err)
	}
	return ctx, pool
}

func insertTestUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, handle string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, handle, display_name) VALUES ($1, $2, $2)`, id, handle); err != nil {
		t.Fatalf("insert user %s: %v", handle, err)
	}
	return id
}

// insertTestMessage writes a message row directly; n gives it distinct ordinals.
func insertTestMessage(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conv, author uuid.UUID, seq, n int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO messages (id, conversation_id, author_id, seq, log_seq, updated_log_seq, text)
		 VALUES ($1, $2, $3, $4, $5, $5, 'a note')`, id, conv, author, seq, n); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return id
}

func rowCounts(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"conversations", "conversation_members", "messages", "attachments"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

func TestMigration0012AcceptsSelfAndRefusesASecondSelfForOnePerson(t *testing.T) {
	ctx, pool := freshDBAt0011(t)

	// Before: 'self' is not a kind.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name, direct_key) VALUES ($1, 'self', 'Notes', $2)`,
		uuid.New(), uuid.NewString()); err == nil {
		t.Fatal("kind = 'self' was accepted before 0012")
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("0012 did not apply: %v", err)
	}

	owner, other := uuid.NewString(), uuid.NewString()
	insert := func(key string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO conversations (id, kind, name, direct_key) VALUES ($1, 'self', 'Notes', $2)`, uuid.New(), key)
		return err
	}
	if err := insert(owner); err != nil {
		t.Fatalf("first self conversation: %v", err)
	}
	var pgErr *pgconn.PgError
	err := insert(owner)
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "conversations_self_key_idx" {
		t.Fatalf("a second self for one person = %v, want a 23505 on conversations_self_key_idx", err)
	}
	if err := insert(other); err != nil {
		t.Fatalf("a self for a different person: %v", err)
	}

	// The index is partial: a direct whose key happens to equal a self's does not
	// collide with it, and an unknown kind is still refused.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, direct_key) VALUES ($1, 'direct', $2)`, uuid.New(), owner); err != nil {
		t.Fatalf("a direct with a self's key: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name) VALUES ($1, 'channel', 'x')`, uuid.New()); err == nil {
		t.Fatal("kind = 'channel' was accepted")
	}
}

func TestMigration0012DownDeletesOnlyTheSelfConversationsContents(t *testing.T) {
	ctx, pool := freshDB(t) // fully migrated, 0012 included

	alice := insertTestUser(ctx, t, pool, "alice-0012")
	bob := insertTestUser(ctx, t, pool, "bob-0012")

	mkConv := func(kind, name, key string, members ...uuid.UUID) uuid.UUID {
		id := uuid.New()
		var nm, k any
		if name != "" {
			nm = name
		}
		if key != "" {
			k = key
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO conversations (id, kind, name, direct_key) VALUES ($1, $2, $3, $4)`, id, kind, nm, k); err != nil {
			t.Fatalf("insert %s conversation: %v", kind, err)
		}
		for _, m := range members {
			if _, err := pool.Exec(ctx,
				`INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`, id, m); err != nil {
				t.Fatalf("insert member: %v", err)
			}
		}
		return id
	}
	selfA := mkConv("self", "Notes", alice.String(), alice)
	selfB := mkConv("self", "Notes", bob.String(), bob)
	direct := mkConv("direct", "", alice.String()+"|"+bob.String(), alice, bob)
	group := mkConv("group", "Kitchen Table", "", alice, bob)

	var n int64
	next := func() int64 { n++; return n }
	voice := insertTestMessage(ctx, t, pool, selfA, alice, 1, next())
	insertTestMessage(ctx, t, pool, selfA, alice, 2, next())
	insertTestMessage(ctx, t, pool, selfB, bob, 1, next())
	keepD := insertTestMessage(ctx, t, pool, direct, alice, 1, next())
	keepG := insertTestMessage(ctx, t, pool, group, bob, 1, next())
	// A reply inside a self conversation pointing at its own message is fine; a
	// direct message's attachment must survive.
	if _, err := pool.Exec(ctx,
		`INSERT INTO attachments (id, message_id, kind, storage_key, position, duration_ms, peaks, transcript_state)
		 VALUES ($1, $2, 'voice', 'k/self', 0, 4000, '{1,2,3}', 'pending'), ($3, $4, 'voice', 'k/direct', 0, 4000, '{1,2,3}', 'pending')`,
		uuid.New(), voice, uuid.New(), keepD); err != nil {
		t.Fatalf("insert attachments: %v", err)
	}

	before := rowCounts(ctx, t, pool)

	if err := MigrateDown(ctx, pool, 1); err != nil {
		t.Fatalf("0012 down: %v", err)
	}

	after := rowCounts(ctx, t, pool)
	want := map[string]int{
		"conversations":        before["conversations"] - 2, // selfA, selfB
		"conversation_members": before["conversation_members"] - 2,
		"messages":             before["messages"] - 3,
		"attachments":          before["attachments"] - 1, // the self voice note, by cascade
	}
	for tbl, w := range want {
		if after[tbl] != w {
			t.Errorf("%s: %d rows after down, want %d (before %d)", tbl, after[tbl], w, before[tbl])
		}
	}

	// Nothing of a self conversation is left, anywhere.
	for _, q := range []string{
		`SELECT count(*) FROM conversations WHERE id IN ($1, $2)`,
	} {
		var c int
		if err := pool.QueryRow(ctx, q, selfA, selfB).Scan(&c); err != nil || c != 0 {
			t.Errorf("self conversations survived the down: %d, %v", c, err)
		}
	}
	// Every direct and group conversation, its messages and its attachment are intact.
	for _, id := range []uuid.UUID{direct, group} {
		var c int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE id = $1`, id).Scan(&c); err != nil || c != 1 {
			t.Errorf("conversation %s: %d, %v", id, c, err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_members WHERE conversation_id = $1`, id).Scan(&c); err != nil || c != 2 {
			t.Errorf("members of %s: %d, %v", id, c, err)
		}
	}
	for _, id := range []uuid.UUID{keepD, keepG} {
		var c int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE id = $1`, id).Scan(&c); err != nil || c != 1 {
			t.Errorf("message %s: %d, %v", id, c, err)
		}
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attachments WHERE message_id = $1`, keepD).Scan(&kept); err != nil || kept != 1 {
		t.Errorf("the direct message's attachment: %d, %v", kept, err)
	}

	// Both old constraints are back: no self kind, and no self index.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name, direct_key) VALUES ($1, 'self', 'Notes', 'k')`, uuid.New()); err == nil {
		t.Error("kind = 'self' accepted after the down")
	}
	var idx int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'conversations_self_key_idx'`).Scan(&idx); err != nil || idx != 0 {
		t.Errorf("conversations_self_key_idx after the down: %d, %v", idx, err)
	}
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = 'conversations'::regclass AND conname = 'conversations_kind_check'`).Scan(&def); err != nil {
		t.Fatalf("read the restored CHECK: %v", err)
	}
	if strings.Contains(def, "self") || !strings.Contains(def, "direct") || !strings.Contains(def, "group") {
		t.Errorf("restored kind CHECK = %s, want direct and group only", def)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("up again after the down: %v", err)
	}
}

func TestMigration0012DownLogsTheCountItDeletes(t *testing.T) {
	ctx, pool := freshDB(t)
	alice := insertTestUser(ctx, t, pool, "alice-0012-log")
	conv := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name, direct_key) VALUES ($1, 'self', 'Notes', $2)`, conv, alice.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`, conv, alice); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		insertTestMessage(ctx, t, pool, conv, alice, i, 900+i)
	}

	// A pool whose connections collect server notices: the down raises the count
	// as a NOTICE, and the runner's output is wherever the operator reads those.
	cfg := pool.Config().Copy()
	var notices []string
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Message) }
	logged, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logged.Close)

	if err := MigrateDown(ctx, logged, 1); err != nil {
		t.Fatalf("0012 down: %v", err)
	}
	joined := strings.Join(notices, "\n")
	if !strings.Contains(joined, "deleting 3 message(s)") {
		t.Errorf("the down did not report the 3 messages it deleted; notices: %q", joined)
	}
}
