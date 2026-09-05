package store

// CANT-14 — both ordinals, assigned inside the insert's own transaction.
//
// This is the single easiest place in this project to lose data silently, and
// the shape of this file is the defence. There is no function here that DRAWS
// an ordinal without also writing the row: the draw is reachable only through
// SendMessage, so Invariant 1 is enforced by the operation's shape rather than
// by a comment asking callers to be careful. (sentByKey returns ordinals but
// draws none — it re-reads a row that is already committed.)
//
// A `bigserial` would be simpler and wrong. A sequence hands out its number
// outside the transaction, so two inserters can commit out of order: a client
// that has seen log_seq 100 never asks for 99 again, and the message holding
// 99 is gone from every sync that follows. It stays invisible until it is
// somebody's message that never arrived, and no single-threaded test catches
// it. internal/store/logorder_test.go is the test that does.
//
// LOCK ORDER, and it is this file's rule to keep: conversations.last_seq
// FIRST, log_counter LAST. Both are row locks held until commit. Taking
// log_counter last keeps the deployment-wide serialised section down to
// draw-insert-commit rather than the whole transaction — and, the larger
// reason, two writers taking the two locks in opposite orders DEADLOCK, and
// Postgres resolves a deadlock by aborting somebody's send. CANT-63 will draw
// this counter when an edit bumps updated_log_seq; it must take the two in
// this order. (CANT-67's sweep does not draw it at all — it advances a floor
// on the conversation.)

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// dedupConstraint is the name Postgres generates for `UNIQUE (author_id,
// client_id)` in migrations/0003_messages.up.sql. Named here so the coupling
// is visible: TestDedupConstraintIsOnAuthorAndClientID pins the constraint's
// existence and columns, and this pins its name.
const dedupConstraint = "messages_author_id_client_id_key"

// NewMessage is one send, as the caller describes it. Everything the SERVER
// owns — both ordinals, `at`, the row id — is absent by construction: a
// caller cannot supply them, so a caller cannot get them wrong.
type NewMessage struct {
	ConversationID uuid.UUID
	AuthorID       uuid.UUID

	// ClientID is the idempotency key, scoped (author_id, client_id) to match
	// ClientSend.client_id's normative "the server deduplicates on (account,
	// client_id)".
	//
	// Nil is legal and means "no deduplication", which is correct rather than
	// sloppy: not every row arrives over a socket. A bot posting through
	// CANT-75's REST send and anything the server originates have no
	// client_id, and Postgres treating those NULLs as distinct is right —
	// they were never deduplicated in the first place. A nil key skips the
	// check below, because a check against NULL could never match anyway.
	ClientID *uuid.UUID

	// SenderDeviceID is nil for a bot, which has no device.
	SenderDeviceID *uuid.UUID

	// ReplyTo is part of THIS row rather than a follow-up UPDATE. A second
	// write would bump no ordinal and would not be covered by the idempotency
	// key, so a replay would re-run it.
	ReplyTo *uuid.UUID

	// Text is nil for a message carrying only attachments, which is why the
	// column is nullable. Attachments themselves are CANT-18's.
	Text *string
}

// Sent is the outcome of a send: the row's identity, both ordinals, the
// server-assigned timestamp, and whether this call created the row or found a
// replay of one that already existed.
type Sent struct {
	ID     uuid.UUID
	Seq    int64
	LogSeq int64
	At     time.Time

	// Duplicate reports a replay. It is NOT an error: a replay is a
	// successful send that happens to have happened already, and the caller
	// wants the original's ordinals so it can ack with them. Modelling it as
	// an error would push every caller into inspecting error strings.
	Duplicate bool
}

// SendMessage writes one message, drawing both ordinals inside the same
// transaction as the insert.
//
// Ruling 2, in order:
//
//  1. Check idempotency BEFORE either ordinal is drawn. The tempting idiom —
//     INSERT ... ON CONFLICT DO NOTHING, then SELECT — commits either way,
//     including the counter bumps, so a replay draws both ordinals, inserts
//     nothing, and leaves a permanent hole in that conversation's seq. The
//     wire tells every client a seq gap means a message it is missing.
//  2. Draw the conversation's ordinal first and the global counter last.
//  3. On the residual race — two concurrent sends under one key both pass the
//     check — catch the unique violation, roll back, and re-select by key.
//     The rollback UN-DRAWS both ordinals, because they came from rows rather
//     than from a sequence. That is the difference between a row counter and
//     a bigserial at the exact moment it matters, and it is why the race
//     leaves no hole in either ordinal.
func (s *Store) SendMessage(ctx context.Context, m NewMessage) (Sent, error) {
	sent, err := s.attemptSend(ctx, m)
	if err == nil {
		return sent, nil
	}
	if !isUniqueViolation(err, dedupConstraint) {
		return Sent{}, err
	}
	if m.ClientID == nil {
		// Unreachable by construction: without a key there is no dedup
		// constraint to violate. Returned rather than ignored, because
		// reaching it would mean the constraint means something else now.
		return Sent{}, fmt.Errorf("store: dedup conflict on a send with no client_id: %w", err)
	}
	// Somebody else committed this key while we were drawing. Our ordinals
	// went back with the rollback; re-read theirs.
	return s.sentByKey(ctx, m.AuthorID, *m.ClientID)
}

func (s *Store) attemptSend(ctx context.Context, m NewMessage) (Sent, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Sent{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1 — check, before anything is drawn.
	if m.ClientID != nil {
		var existing Sent
		err := tx.QueryRow(ctx,
			`SELECT id, seq, log_seq, at FROM messages WHERE author_id = $1 AND client_id = $2`,
			m.AuthorID, *m.ClientID).Scan(&existing.ID, &existing.Seq, &existing.LogSeq, &existing.At)
		switch {
		case err == nil:
			existing.Duplicate = true
			return existing, nil // rolled back by the defer: nothing was drawn
		case !errors.Is(err, pgx.ErrNoRows):
			return Sent{}, fmt.Errorf("store: check idempotency key: %w", err)
		}
	}

	// 2 — draw, conversation first. See the lock-order note at the top.
	var out Sent
	if err := tx.QueryRow(ctx,
		`UPDATE conversations SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq`,
		m.ConversationID).Scan(&out.Seq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Sent{}, fmt.Errorf("store: conversation %s: %w", m.ConversationID, ErrNotFound)
		}
		return Sent{}, fmt.Errorf("store: draw seq: %w", err)
	}

	// 3 — and the global counter LAST, so the deployment-wide serialised
	// section is draw-insert-commit rather than the whole transaction.
	if err := tx.QueryRow(ctx,
		`UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value`).Scan(&out.LogSeq); err != nil {
		return Sent{}, fmt.Errorf("store: draw log_seq: %w", err)
	}

	// 4 — insert. `at` is the schema's job, not the caller's, and
	// updated_log_seq starts equal to log_seq; both come back rather than
	// being assumed.
	out.ID = uuid.New()
	if err := tx.QueryRow(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, seq, log_seq, updated_log_seq,
		                      text, client_id, sender_device_id, reply_to)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8, $9)
		RETURNING at`,
		out.ID, m.ConversationID, m.AuthorID, out.Seq, out.LogSeq,
		m.Text, m.ClientID, m.SenderDeviceID, m.ReplyTo).Scan(&out.At); err != nil {
		// Wrapped, so the 23505 the caller retries on is still reachable
		// through errors.As.
		return Sent{}, fmt.Errorf("store: insert message: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Sent{}, fmt.Errorf("store: commit: %w", err)
	}
	return out, nil
}

// sentByKey re-reads the winner of a dedup race.
func (s *Store) sentByKey(ctx context.Context, authorID, clientID uuid.UUID) (Sent, error) {
	var out Sent
	err := s.pool.QueryRow(ctx,
		`SELECT id, seq, log_seq, at FROM messages WHERE author_id = $1 AND client_id = $2`,
		authorID, clientID).Scan(&out.ID, &out.Seq, &out.LogSeq, &out.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sent{}, ErrNotFound
	}
	if err != nil {
		return Sent{}, fmt.Errorf("store: re-read after dedup conflict: %w", err)
	}
	out.Duplicate = true
	return out, nil
}

// isUniqueViolation reports whether err is a 23505 against constraint. An
// empty constraint matches any unique violation.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(constraint == "" || pgErr.ConstraintName == constraint)
}
