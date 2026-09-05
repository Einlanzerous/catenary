package store

// CANT-14 — the two nullable inputs, which no other test in this package
// reaches. Every call site elsewhere passes a client_id and a text, so without
// these the nil branches of SendMessage are unexecuted and the claim that a
// server-originated row inserts at all is reasoning rather than evidence.

import (
	"testing"

	"github.com/google/uuid"
)

// A nil ClientID means NO DEDUPLICATION, and that is correct rather than
// sloppy: not every row arrives over a socket. A bot on CANT-75's REST send
// and anything the server originates have no key. Postgres treats those NULLs
// as distinct in the unique constraint, so two of them are two messages —
// which is right, because they were never deduplicated in the first place.
func TestSendsWithNoIdempotencyKeyAreNeverDeduplicated(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool)
	u := mkUser(ctx, t, pool, "server")
	conv := mkGroup(ctx, t, pool, "room")

	first, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, Text: ptr("one")})
	if err != nil {
		t.Fatalf("a send with no client_id failed outright: %v", err)
	}
	second, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, Text: ptr("two")})
	if err != nil {
		t.Fatalf("a second send with no client_id failed: %v", err)
	}

	if first.Duplicate || second.Duplicate {
		t.Errorf("a keyless send reported itself a duplicate: %+v, %+v", first, second)
	}
	if first.ID == second.ID {
		t.Fatal("two keyless sends collapsed into one message; NULLs are being treated as equal")
	}
	if second.Seq <= first.Seq || second.LogSeq <= first.LogSeq {
		t.Errorf("ordinals did not advance across two keyless sends: %+v then %+v", first, second)
	}

	var nulls int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE author_id = $1 AND client_id IS NULL`, u).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 2 {
		t.Errorf("%d rows stored a NULL client_id, want 2", nulls)
	}
	assertDense(t, seqs(ctx, t, pool, conv))
}

// A keyless send still takes both ordinals from the same transaction — the nil
// key skips the idempotency check, not the draw.
func TestAKeylessSendStillDrawsBothOrdinalsInOneTransaction(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool)
	u := mkUser(ctx, t, pool, "server")
	conv := mkGroup(ctx, t, pool, "room")

	sent, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, Text: ptr("x")})
	if err != nil {
		t.Fatal(err)
	}

	var lastSeq, counter int64
	pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, conv).Scan(&lastSeq)
	pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&counter)
	if lastSeq != sent.Seq || counter != sent.LogSeq {
		t.Errorf("returned (seq %d, log_seq %d) but the counters read (%d, %d)",
			sent.Seq, sent.LogSeq, lastSeq, counter)
	}
	if sent.At.IsZero() {
		t.Error("at was not returned; it is server-assigned and the caller needs it to ack")
	}
}

// Text is nullable because a message may carry only attachments — that is what
// the column's nullability is FOR, and CANT-18 is the ticket that sends one.
func TestAMessageMayCarryNoText(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")

	sent, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: ptr(uuid.New())})
	if err != nil {
		t.Fatalf("a message with no text was rejected: %v", err)
	}

	var text *string
	if err := pool.QueryRow(ctx, `SELECT text FROM messages WHERE id = $1`, sent.ID).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != nil {
		t.Errorf("text stored as %q, want NULL — an empty string and no text are different things", *text)
	}
}
