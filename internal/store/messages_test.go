package store

// CANT-14 — the two things about SendMessage's inputs that no other test in
// this package reaches: a missing idempotency key, and a message with no text.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
	// The "and nothing was drawn" half of this test used to live here, as three
	// counters read back and compared against 0. CANT-79 measured it and it did
	// not discriminate: the deferred tx.Rollback un-draws both counters, so all
	// three read 0 whether the guard fires before Begin or halfway through the
	// transaction. Moving the guard to after both draws left it green.
	//
	// An assertion whose pass condition is the zero value cannot tell "clean"
	// from "never ran". The claim it was making is real and worth keeping, so
	// it moved to the test below, which can fail.
}

// The guard fires BEFORE the database is touched at all.
//
// This is the half of the test above that CANT-79 replaced. It is the same
// claim — 5d87a10's "refuses uuid.Nil with ErrNoClientID, before anything is
// drawn or written" — asserted in a way that can go red: the guard is the first
// statement of SendMessage, ahead of attemptSend and therefore ahead of Begin,
// so over a CLOSED pool it must still return ErrNoClientID rather than a
// connection error. Move the guard after either draw and Begin fails first,
// which is a different error and a failing test.
//
// It needs no database, so unlike everything else in this file it runs even
// when CATENARY_TEST_DATABASE_URL is unset — the guard is checked in every
// environment rather than only where Postgres happens to be up.
func TestTheMissingKeyGuardFiresBeforeTheDatabaseIsTouched(t *testing.T) {
	ctx := context.Background()

	// pgxpool is lazy: New parses the DSN and dials nothing. Closing it makes
	// any Begin fail immediately and locally, which is the point — this pool
	// never has to reach a server that is not there.
	pool, err := pgxpool.New(ctx, "postgres://nobody@127.0.0.1:1/unreachable")
	if err != nil {
		t.Fatalf("constructing the pool should not need a server: %v", err)
	}
	pool.Close()

	_, err = New(pool).SendMessage(ctx, NewMessage{
		ConversationID: uuid.New(),
		AuthorID:       uuid.New(),
		Text:           ptr("no key"),
	})
	if !errors.Is(err, ErrNoClientID) {
		t.Fatalf("over a closed pool a keyless send failed with %v, want ErrNoClientID — "+
			"the guard reached the database, so it is no longer ahead of the draws", err)
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
	mustScan(t, pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, conv), &lastSeq)
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &counter)
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
