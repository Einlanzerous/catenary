package store

// CANT-14 — the two things about SendMessage's inputs that no other test in
// this package reaches: a missing idempotency key, and a message with no text.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// A send with no idempotency key is REFUSED, loudly.
//
// The key is required rather than optional because an optional one is a field
// a caller forgets, and forgetting would quietly opt that send out of
// deduplication — no error, no signal, and a bot that retries on a timer
// double-posts. Every sending surface supplies one: CANT-75's REST send takes
// it in the body and its Done-when requires that a sender with no device row
// is deduplicated exactly like one with.
func TestASendWithNoIdempotencyKeyIsRefused(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool)
	u := mkUser(ctx, t, pool, "forgetful")
	conv := mkGroup(ctx, t, pool, "room")

	_, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, Text: ptr("no key")})
	if err == nil {
		t.Fatal("a send with no client_id was accepted; it would never be deduplicated and nothing would say so")
	}
	if !errors.Is(err, ErrNoClientID) {
		t.Errorf("refused with %v, want ErrNoClientID so a caller can tell this from a database failure", err)
	}

	// And it is refused BEFORE anything is drawn or written.
	var msgs, lastSeq, counter int64
	pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&msgs)
	pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, conv).Scan(&lastSeq)
	pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&counter)
	if msgs != 0 || lastSeq != 0 || counter != 0 {
		t.Errorf("a refused send left state behind: %d messages, last_seq %d, log_counter %d", msgs, lastSeq, counter)
	}
}

// The ordinals the caller is handed are the ones the transaction drew — the
// caller acks with these, so they cannot be approximations.
func TestSendReturnsTheOrdinalsItDrew(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")

	sent, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: uuid.New(), Text: ptr("x")})
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

	sent, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: uuid.New()})
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
