package main

// CANT-26's third `Done when`: the web client's existing assertions hold
// against REAL DATA rather than against `web/src/mock/fixtures.ts`.
//
// This builds the canvas's own shape in Postgres — a run of five above the
// divider, three of them from other people — serves it through the real
// serveSync, and writes the response out. `verify.sh` then hands that file to
// `web/readstate.ts`, which decodes it with the generated TypeScript decoder
// and runs the client's OWN newCount and unreadCount over it.
//
// Nothing here is normalised or hand-shaped: the ids are whatever the store
// assigned and the timestamps are whatever Postgres wrote, which is the point.
// A capture that had been tidied first would be a fixture again.
//
// It also happens to be the test over serveSync that CANT-20's review noted was
// missing — the one line that copies read_seq off the page now has a caller.
//
// RUN THE SUITE WITH `-p 1` WHEN A DSN IS SET, which is what verify.sh does.
// There is one test database, and this package and internal/store both begin by
// rolling every migration back and reapplying. Go runs different packages'
// tests in parallel by default, so without the flag the two reset the schema
// out from under each other and fail on whichever table vanished first — a
// failure that says nothing about the code.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/store"
)

// captureEnv names where verify.sh wants the served page written. Unset means
// the test still runs and still asserts; it just does not leave a file behind.
const captureEnv = "CATENARY_SYNC_CAPTURE"

func TestTheServedPageCarriesTheCanvasNumbers(t *testing.T) {
	dsn := os.Getenv("CATENARY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATENARY_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := store.MigrateDown(ctx, pool, 0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.New(pool, store.DefaultLimits(), slog.New(slog.DiscardHandler))

	// Seven members, because the canvas's header reads `7 MEMBERS · TLS` and
	// its read receipt reads `READ 5/7`. Theo is the reader.
	theo := mkUser(ctx, t, pool, "theo", "Theo")
	nadia := mkUser(ctx, t, pool, "nadia", "Nadia Ruiz")
	others := []uuid.UUID{theo, nadia}
	for _, h := range []string{"ilse", "marcus", "priya", "sam", "wei"} {
		others = append(others, mkUser(ctx, t, pool, h, h))
	}
	conv := mkGroup(ctx, t, pool, "Kitchen Table", others...)

	// One message below the divider, then the mark, then the run of five.
	send(ctx, t, st, conv, nadia, "read this one already")
	if _, err := st.MarkRead(ctx, conv, theo, 1); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	send(ctx, t, st, conv, nadia, "new one")
	send(ctx, t, st, conv, theo, "mine, so it is not unread")
	send(ctx, t, st, conv, nadia, "new two")
	send(ctx, t, st, conv, theo, "also mine")
	last := send(ctx, t, st, conv, nadia, "new three")

	// Five of the other six members have read the last message, so the canvas's
	// READ 5/7 is a real count over real receipts rather than a chosen number.
	for _, u := range others[2:] {
		if _, err := st.MarkRead(ctx, conv, u, last.Seq); err != nil {
			t.Fatalf("mark read for a member: %v", err)
		}
	}
	// Theo does NOT read it — his mark stays at 1, which is what leaves the
	// three unread the canvas draws as "3 NEW". Five other members reading it
	// is what makes READ 5/7 true at the same time; the two facts are about
	// different people and the page has to carry both.

	// serveSync is the function under test, not store.Sync — this is the whole
	// composition-root path, read_seq copy included.
	page, err := serveSync(ctx, st, theo, 0, 0)
	if err != nil {
		t.Fatalf("serveSync: %v", err)
	}

	// The server's half of the client's rule, asserted here so a failure says
	// which side is wrong before the TypeScript check even runs.
	var conversation = page.Conversations[0]
	if conversation.FirstUnreadSeq == nil {
		t.Fatal("first_unread_seq is absent; theo has three unread messages")
	}
	var unread int
	for _, m := range page.Messages {
		if int64(m.Seq) >= int64(*conversation.FirstUnreadSeq) && string(m.AuthorID) != theo.String() {
			unread++
		}
	}
	if unread != 3 {
		t.Errorf("the client's unread rule over this page counts %d, want the canvas's 3", unread)
	}
	if conversation.MemberCount != 7 {
		t.Errorf("member_count = %d, want 7", conversation.MemberCount)
	}
	// Found-or-fail rather than a loop that can `continue` past every message
	// and report success having compared nothing.
	var found bool
	for _, m := range page.Messages {
		if string(m.ID) != last.ID.String() {
			continue
		}
		found = true
		// Six: the five members who marked read, plus Nadia who wrote it. The
		// numerator counts the same population member_count does, so READ 6/7
		// here means one member — Theo — has not seen it, which is exactly the
		// three-unread state the rest of this test asserts.
		if m.ReadBy == nil || *m.ReadBy != 6 {
			t.Errorf("read_by on the last message = %v, want 6 — READ 6/7", m.ReadBy)
		}
	}
	if !found {
		t.Errorf("the last message (%s) is not on the page; nothing above was compared", last.ID)
	}

	raw, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := os.Getenv(captureEnv)
	if out == "" {
		out = filepath.Join(t.TempDir(), "served-sync.json")
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	t.Logf("served page written to %s (%d bytes)", out, len(raw))
}

func mkUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, handle, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, handle, display_name) VALUES ($1, $2, $3)`, id, handle, name); err != nil {
		t.Fatalf("mkUser(%s): %v", handle, err)
	}
	return id
}

func mkGroup(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', $2)`, id, name); err != nil {
		t.Fatalf("mkGroup: %v", err)
	}
	for _, u := range members {
		if _, err := pool.Exec(ctx,
			`INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`,
			id, u); err != nil {
			t.Fatalf("mkMember: %v", err)
		}
	}
	return id
}

func send(ctx context.Context, t *testing.T, st *store.Store, conv, author uuid.UUID, text string) store.Sent {
	t.Helper()
	s, err := st.SendMessage(ctx, store.NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: &text,
	})
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return s
}
