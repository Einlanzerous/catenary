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
// LOCK ORDER, and it is this file's rule to keep. FOUR row locks, all held
// until commit, taken in this order and no other:
//
//	conversations  →  messages (the reply_to source)  →  conversation_members  →  log_counter
//
// CANT-14 established the outer two. CANT-83 added the other two.
// `conversation_members` is new: an author's own send advances their read_seq
// (position 9), which locks their member row between the two draws.
//
// `messages` is NOT new — it was always taken, implicitly and too late. The
// FK check on the insert at position 11 takes KEY SHARE on the reply_to source
// row, which put `messages` after `log_counter` in the real order while the
// comment claimed three locks. What CANT-83 changed is WHEN: position 8b takes
// that same lock explicitly, above the counter draw, so a source deleted
// mid-send cannot make the insert raise 23503 on a send the author cannot fix.
// An implicit lock is still a lock, and one an ordering rule does not name is
// one nobody can reason about.
//
// Taking log_counter LAST keeps the deployment-wide serialised section down to
// draw-insert-commit rather than the whole transaction. The larger reason for
// fixing any order at all is that two writers taking the same locks in
// opposite orders DEADLOCK, and Postgres resolves a deadlock by aborting
// somebody's send.
//
// STATED OUTWARD, because three other tickets take some of these locks and
// only this file states the order:
//
//   - Never take the conversation row AFTER a member row. CANT-26's receipt
//     write takes the member row alone, which is safe as a single lock — but
//     if it ever also touches the conversation, the conversation goes first.
//   - CANT-67's sweep advances a floor on `conversations` and then deletes
//     from `messages`. That is the same direction as this file, which is the
//     whole reason 8b sits below the conversation draw rather than at
//     position 6: taking `messages` first would invert against that sweep and
//     turn a rare permanent failure into a routine deadlock.
//   - CANT-63 draws log_counter when an edit bumps updated_log_seq, and takes
//     these in this order too.
//
// The rule lives here rather than on those three tickets because this is the
// file all of them have to edit, and a rule stated where the work happens is a
// rule that cannot be missed.

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

// replyToResolveQuery is position 8b's read, named so the test that proves FOR
// KEY SHARE actually blocks a concurrent delete runs THIS query rather than a
// copy of it. Drop the clause here and that test goes red, which is the point:
// the whole fix is the clause.
const replyToResolveQuery = `SELECT conversation_id FROM messages WHERE id = $1 FOR KEY SHARE`

// NewMessage is one send, as the caller describes it. Everything the SERVER
// owns — both ordinals, `at`, the row id — is absent by construction: a
// caller cannot supply them, so a caller cannot get them wrong.
type NewMessage struct {
	ConversationID uuid.UUID
	AuthorID       uuid.UUID

	// ClientID is the idempotency key, scoped (author_id, client_id) to match
	// ClientSend.client_id's normative "the server deduplicates on (account,
	// client_id)". REQUIRED, and SendMessage refuses the zero value.
	//
	// Every sending surface supplies one, bots included: CANT-75's REST send
	// takes it in the body and its Done-when requires that "a sender with no
	// device row is deduplicated exactly like one with". A cron-driven bot
	// needs it MORE than a phone does — it retries mechanically, on a timer,
	// possibly from two replicas.
	//
	// It is required here rather than optional because an optional key is one
	// a caller forgets, and forgetting would silently opt that send out of
	// deduplication with no error and no signal. The column stays nullable
	// (CANT-13's call) so a genuinely server-originated path could one day
	// write NULL; nothing does today, and this primitive is not the thing
	// that should be able to.
	ClientID uuid.UUID

	// SenderDeviceID is nil for a bot, which has no device.
	SenderDeviceID *uuid.UUID

	// ReplyTo is part of THIS row rather than a follow-up UPDATE. A second
	// write would bump no ordinal and would not be covered by the idempotency
	// key, so a replay would re-run it.
	ReplyTo *uuid.UUID

	// Text is nil for a message carrying only attachments, which is why the
	// column is nullable.
	//
	// NEITHER Text NOR Attachments is required. A send with both absent is
	// STORED rather than refused: `ClientSend` makes both optional, so it is a
	// legal frame, and no ErrorCode describes it. Refusing would mean either
	// widening a closed enum — a wire change under CANT-74's unresolved
	// compatibility policy — or reporting a client bug as `internal`, which is
	// a lie about whose fault it is. It costs one seq and renders as an empty
	// bubble, and it counts toward everyone's unread exactly as a message
	// containing a single space would. The composer is where an empty send
	// should be prevented.
	Text *string

	// Attachments as the SENDER describes them. CANT-83 only counts them, to
	// enforce CATENARY_MAX_ATTACHMENTS; CANT-85 resolves the upload ids and
	// writes the rows inside this transaction.
	Attachments []NewAttachment
}

// NewAttachment mirrors the wire's OutboundAttachment, which carries a kind and
// an upload handle and nothing else: dimensions, duration, peaks, EXIF
// stripping and the blurhash are all computed server-side on ingest, so the
// client cannot report them and cannot get them wrong.
type NewAttachment struct {
	Kind     string
	UploadID uuid.UUID
}

// Sent is the outcome of a send: the row's identity, both ordinals, the
// server-assigned timestamp, and whether this call created the row or found a
// replay of one that already existed.
type Sent struct {
	ID uuid.UUID

	// ConversationID is the conversation the row IS IN, which is not always
	// the one the caller asked about.
	//
	// Deduplication is scoped (author_id, client_id) — per the wire's
	// normative "the server deduplicates on (account, client_id)" — and NOT
	// per conversation. So an author who reuses one key across two
	// conversations gets the FIRST row back, with the first conversation's
	// dense seq. The idempotency check has to sit above the membership and
	// existence checks (criterion 4: a replay returns the original even when
	// the sender has since been removed), so reordering is not available and
	// is not wanted.
	//
	// That leaves one rule, and it is a rule for the transports rather than
	// for this file: BUILD THE ACK FROM THIS STRUCT, NEVER FROM THE REQUEST.
	// Acking the request's conversation with this row's seq tells a client
	// that a seq exists in a thread it does not, and a dense seq the client
	// cannot see is a message it will believe it is missing forever. There is
	// no security question here — dedup is per author, so an author only ever
	// learns about their own message — which is why the store returns the
	// truth rather than refusing.
	ConversationID uuid.UUID

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
	sent, err := s.send(ctx, m)
	if err == nil {
		return sent, nil
	}

	// EVERY refusal logs exactly once, and it does so here rather than at each
	// return, so that is a property of the shape rather than a rule six call
	// sites have to remember. The level and the code-derived fields are the
	// table's decision (senderror.go); the ids are this file's.
	//
	// The BODY is not among them, at any level. The size is reportable and the
	// text is not — a size refusal that copied the message into a log would
	// give it a different retention story than the message itself, which is
	// what D1's honesty about what the server can see costs if nobody keeps it.
	se := sendErrorFor(err)
	s.logger.Log(ctx, se.Level(), "send refused", append([]any{
		"conversation_id", m.ConversationID,
		"author_id", m.AuthorID,
		"client_id", m.ClientID,
	}, se.LogAttrs()...)...)
	return Sent{}, se
}

// send is SendMessage without the logging, so the refusal path has exactly one
// exit and the ordering below reads as the plan's twelve positions.
func (s *Store) send(ctx context.Context, m NewMessage) (Sent, error) {
	// 1 — the key, before the pool is touched. Loud rather than silent: a
	// forgotten key is a send that would quietly never be deduplicated.
	if m.ClientID == uuid.Nil {
		return Sent{}, fmt.Errorf("store: send without an idempotency key: %w", ErrNoClientID)
	}

	// 2 — the bounds, also before the pool is touched. Pure arithmetic over
	// the frame, so an oversized send costs no transaction and no round trip.
	if err := s.checkBounds(m); err != nil {
		return Sent{}, err
	}

	sent, err := s.attemptSend(ctx, m)
	if err == nil {
		return sent, nil
	}
	if !isUniqueViolation(err, dedupConstraint) {
		return Sent{}, err
	}
	// Somebody else committed this key while we were drawing. Our ordinals
	// went back with the rollback; re-read theirs.
	return s.sentByKey(ctx, m.AuthorID, m.ClientID)
}

// checkBounds is position 2: everything refusable without a database.
//
// The BYTE length of text, not its rune count — the column and the wire both
// count bytes, and a rune bound would refuse a different set of messages than
// the database would accept.
//
// The size is in the error; the body never is, at any level. D1 declines
// end-to-end encryption and names its mitigation as honesty about what the
// server can see, and that honesty is worth less if a size refusal copies the
// message into a log with a different retention story than the message itself.
func (s *Store) checkBounds(m NewMessage) error {
	if m.Text != nil && len(*m.Text) > s.limits.MaxMessageBytes {
		return fmt.Errorf("store: message body is %d bytes, limit %d: %w",
			len(*m.Text), s.limits.MaxMessageBytes, ErrMessageTooLarge)
	}
	if len(m.Attachments) > s.limits.MaxAttachments {
		return fmt.Errorf("store: send carries %d attachments, limit %d: %w",
			len(m.Attachments), s.limits.MaxAttachments, ErrTooManyAttachments)
	}
	return nil
}

func (s *Store) attemptSend(ctx context.Context, m NewMessage) (Sent, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Sent{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 4 — check, before anything is drawn.
	var existing Sent
	err = tx.QueryRow(ctx,
		`SELECT id, conversation_id, seq, log_seq, at FROM messages WHERE author_id = $1 AND client_id = $2`,
		m.AuthorID, m.ClientID).Scan(&existing.ID, &existing.ConversationID, &existing.Seq, &existing.LogSeq, &existing.At)
	switch {
	case err == nil:
		existing.Duplicate = true
		return existing, nil // rolled back by the defer: nothing was drawn
	case !errors.Is(err, pgx.ErrNoRows):
		return Sent{}, fmt.Errorf("store: check idempotency key: %w", err)
	}

	// 5 — membership and existence, in ONE query and as two EXISTS rather than
	// a join. A join cannot distinguish "no such conversation" from "you are
	// not in it", and the wire has a separate code for each.
	//
	// This LEAKS EXISTENCE, deliberately: a non-member learns that an id
	// resolves. Collapsing the two would tell a member of a deleted
	// conversation the wrong thing, and CANT-18's Done-when requires
	// not_a_member for a non-member send. Among a small trusted group with
	// authenticated senders that is the right trade; senderror.go carries the
	// reasoning next to the codes.
	var convExists, isMember bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM conversations WHERE id = $1),
		       EXISTS (SELECT 1 FROM conversation_members
		                WHERE conversation_id = $1 AND user_id = $2)`,
		m.ConversationID, m.AuthorID).Scan(&convExists, &isMember); err != nil {
		return Sent{}, fmt.Errorf("store: resolve conversation and membership: %w", err)
	}
	if !convExists {
		return Sent{}, fmt.Errorf("store: conversation %s: %w", m.ConversationID, ErrConversationNotFound)
	}
	if !isMember {
		return Sent{}, fmt.Errorf("store: %s in conversation %s: %w",
			m.AuthorID, m.ConversationID, ErrNotAMember)
	}

	// 8 — draw, conversation first. See the lock-order note at the top.
	var out Sent
	if err := tx.QueryRow(ctx,
		`UPDATE conversations SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq`,
		m.ConversationID).Scan(&out.Seq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unreachable by the ordinary miss, which position 5 already
			// refused. What is left is the narrow race where the conversation
			// is deleted between that read and this write — still
			// conversation_not_found, and still the sender's answer.
			return Sent{}, fmt.Errorf("store: conversation %s: %w", m.ConversationID, ErrConversationNotFound)
		}
		return Sent{}, fmt.Errorf("store: draw seq: %w", err)
	}

	// 8b — reply_to. The source must exist AND be in this conversation.
	//
	// MOVED HERE FROM POSITION 6, below the conversation lock, and the plan is
	// superseded on that point rather than quietly bent. At position 6 this
	// read took no lock, so a source deleted between the read and the insert
	// made position 11's FK check raise 23503 — class 23, not transient — and
	// the send failed PERMANENTLY over a reply_to the sender cannot fix, which
	// is the outcome Ruling 4 exists to prevent arriving at the insert instead
	// of at the resolve.
	//
	// FOR KEY SHARE closes it, and the order is why it has to be here. The
	// lock is not new: position 11's FK check already takes KEY SHARE on this
	// exact row, just too late to help. What moves is WHEN — from after the
	// log_counter draw to before it — and the constraint is CANT-67, whose
	// sweep goes `conversations` then `messages`. Taking it at position 6,
	// above the conversation lock, would have inverted against that sweep and
	// traded a rare permanent failure for a routine deadlock. Here, both take
	// `conversations` first and then `messages`, which is the same direction.
	//
	// RULING 4: a source that does not resolve stores NULL and the send
	// SUCCEEDS. The FK only requires reply_to to be *a* message, and the wire
	// builds ReplyRef live and server-side with a one-line preview of the
	// source — so a reply_to pointing into another conversation would render
	// that conversation's text to every member of this one. Nothing renders
	// previews yet, which is exactly why it is cheap to close now.
	//
	// NULL rather than a refusal, because the schema and the wire both already
	// model "the source is gone, so there is no ref": messages.reply_to is ON
	// DELETE SET NULL for CANT-67's sweep, and Message.reply_to is optional.
	// Refusing would invent a new failure for a case the system already has an
	// answer to, and would fail a send over a field the sender cannot fix.
	//
	// Read inside the transaction AND under a key-share lock, so the source
	// cannot be deleted between this read and the insert. Without the lock
	// there is a third outcome the snapshot argument does not cover: visible
	// here, gone at position 11.
	replyTo := m.ReplyTo
	if replyTo != nil {
		var sourceConv uuid.UUID
		err := tx.QueryRow(ctx, replyToResolveQuery, *replyTo).Scan(&sourceConv)
		switch {
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return Sent{}, fmt.Errorf("store: resolve reply_to: %w", err)

		// LOGGED, at info and with both ids, because this is the one case
		// where the served Message differs from what the sender's client
		// optimistically rendered — someone will eventually ask why the reply
		// arrow vanished, and the answer should be findable.
		case errors.Is(err, pgx.ErrNoRows):
			s.logger.InfoContext(ctx, "reply_to dropped: the source does not resolve",
				"conversation_id", m.ConversationID, "author_id", m.AuthorID,
				"reply_to_message_id", *replyTo)
			replyTo = nil
		case sourceConv != m.ConversationID:
			s.logger.InfoContext(ctx, "reply_to dropped: the source is in another conversation",
				"conversation_id", m.ConversationID, "author_id", m.AuthorID,
				"reply_to_message_id", *replyTo, "source_conversation_id", sourceConv)
			replyTo = nil
		}
	}

	// 9 — the author's own read_seq, in THIS transaction.
	//
	// You have read what you just sent. 0002_conversations states this and
	// rests CANT-26's first_unread_seq on it: with the author's own send
	// advancing read_seq, first_unread_seq is arithmetic (read_seq + 1, absent
	// when read_seq = last_seq) rather than a scan that has to skip your own
	// messages. Nothing wrote it until now.
	//
	// GREATEST, never a bare assignment — and NOT for the reason it is tempting
	// to give. The obvious story is "a receipt for a later seq can commit
	// between the draw and this statement", and that race cannot happen:
	// position 8 holds the conversation row exclusively until commit, so for
	// this whole window out.Seq is the highest seq that can exist here, and a
	// receipt above it would have to name a message that does not exist.
	//
	// The real reason is that read_seq is not this file's column. CANT-26 owns
	// the receipt write, nothing clamps a receipt to last_seq, and a member
	// row is reachable from outside this transaction entirely. A floor is
	// correct under every arrival order including ones this file cannot see;
	// an assignment is correct only under the ones it can. The cost of
	// GREATEST is nothing, and the cost of being wrong is resurrecting
	// messages a member has already read.
	//
	// Here rather than in CANT-26 because it is a write into this transaction,
	// and CANT-26 is an evidence ticket. It takes the member row lock, which
	// is why the lock-order note at the top now names three locks and states
	// the rule outward for the tickets that take that row without sending.
	//
	// A REPLAY never reaches this line: position 4 returns before any draw, so
	// re-sending an acked message cannot advance a read_seq that has since
	// moved on.
	tag, err := tx.Exec(ctx,
		`UPDATE conversation_members SET read_seq = GREATEST(read_seq, $3)
		  WHERE conversation_id = $1 AND user_id = $2`,
		m.ConversationID, m.AuthorID, out.Seq)
	if err != nil {
		return Sent{}, fmt.Errorf("store: advance the author's read_seq: %w", err)
	}
	// AND THE ROW COUNT IS THE ANSWER, so it is not thrown away. Position 5 is
	// an unlocked read; this statement takes the member row lock. A membership
	// revoked in between leaves this matching zero rows, and without the check
	// the send would commit anyway — a message authored by a non-member, which
	// is the state criterion 1 exists to refuse. It is the same answer position
	// 5 gave, taken again at the last moment it can still be wrong, and it
	// costs one integer comparison.
	//
	// Untested deliberately: the window is between two statements of one
	// transaction and cannot be driven from a test without a scheduler. The
	// assertion is insurance rather than a claim, and it cannot misfire — a
	// member who exists always matches one row.
	if tag.RowsAffected() == 0 {
		return Sent{}, fmt.Errorf("store: %s left conversation %s mid-send: %w",
			m.AuthorID, m.ConversationID, ErrNotAMember)
	}

	// 10 — and the global counter LAST, so the deployment-wide serialised
	// section is draw-insert-commit rather than the whole transaction.
	if err := tx.QueryRow(ctx,
		`UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value`).Scan(&out.LogSeq); err != nil {
		return Sent{}, fmt.Errorf("store: draw log_seq: %w", err)
	}

	// 11 — insert. `at` is the schema's job, not the caller's, and
	// updated_log_seq starts equal to log_seq; both come back rather than
	// being assumed.
	out.ID = uuid.New()
	out.ConversationID = m.ConversationID
	if err := tx.QueryRow(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, seq, log_seq, updated_log_seq,
		                      text, client_id, sender_device_id, reply_to)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8, $9)
		RETURNING at`,
		out.ID, m.ConversationID, m.AuthorID, out.Seq, out.LogSeq,
		m.Text, m.ClientID, m.SenderDeviceID, replyTo).Scan(&out.At); err != nil {
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
		`SELECT id, conversation_id, seq, log_seq, at FROM messages WHERE author_id = $1 AND client_id = $2`,
		authorID, clientID).Scan(&out.ID, &out.ConversationID, &out.Seq, &out.LogSeq, &out.At)
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
