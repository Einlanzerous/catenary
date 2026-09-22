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
// LOCK ORDER, and it is this file's rule to keep. THREE row locks that this
// file takes ON PURPOSE, all held until commit, in this order:
//
//	conversations  →  messages (the reply_to source, IN THIS CONVERSATION)  →  log_counter
//
// CANT-14 established the outer two. CANT-83 added `messages` in the middle —
// though it was NOT new, only implicit and too late. The FK check on the
// insert at position 11 takes KEY SHARE on the reply_to source row, which put
// `messages` after `log_counter` in the real order while the comment claimed
// two. Position 8b takes that same lock explicitly, above the counter draw, so
// a source deleted mid-send cannot make the insert raise 23503 on a send the
// author cannot fix. An implicit lock is still a lock, and one an ordering
// rule does not name is one nobody can reason about.
//
// "IN THIS CONVERSATION" IS LOAD-BEARING and it is why replyToResolveQuery is
// scoped. This list names TABLES, and a table order cannot describe a hazard
// that is per ROW. An unscoped resolve let a send in conversation A hold
// `conversations(A)` and then take `messages(X ∈ B)` — for a ref it was about
// to discard — so a sweep holding `conversations(B)` and `messages(X)` while
// it moved on to A closed a cycle in which BOTH parties had obeyed
// "conversations then messages". Scoped, the only row this transaction can
// lock is one whose conversation it already holds, which is what turns the
// arrow above into a real order instead of a table list. It is also what makes
// the paragraph above TRUE: with the scope, the row locked at 8b is exactly
// the row position 11's FK would have locked, and no other.
//
// `conversation_members` IS NOT TAKEN HERE, and that is a decision rather than
// an omission. An earlier revision of this file advanced the author's own
// read_seq between the two draws, which locked their member row and made this
// a four-lock transaction. CANT-83 removed it: first_unread_seq is now derived
// with an author filter (see 0005_read_seq_derivation), so the send has no
// reason to touch a member row at all. That deletes a lock, a lock-ordering
// obligation stated outward to two other tickets, and the deadlock class that
// came with them — out of the one transaction in this project that cannot
// afford any of the three.
//
// AND TWO MORE THAT THE INSERT TAKES WITHOUT ASKING, named here because the
// paragraph above is otherwise a false absolute — and because that paragraph's
// own argument is what convicts it. `messages` has four foreign keys, and an
// insert takes KEY SHARE on every row it references, held to commit.
// `conversations` is already held more strongly from position 8 and the
// reply_to source from 8b, but `users(author_id)` and — when the sender has one
// — `devices(sender_device_id)` are locked for the FIRST time at position 11,
// after the log_counter draw.
//
// No path in this service takes a conflicting lock on either, and CANT-91 is
// the ticket that had to keep it that way rather than inherit it. The argument
// is the same one in both places: `users.deactivated_at` and
// `devices.revoked_at` are non-key updates, so they take FOR NO KEY UPDATE and
// do not conflict with KEY SHARE; the only DELETEs against those tables are the
// RESTRICT assertions in schema_test.go; and two sends never contend, because
// KEY SHARE is shared.
//
// `internal/store/metadata.go` locks `users` and `conversation_members` FOR NO
// KEY UPDATE FOR THIS REASON AND NOT BY HABIT. It has to take those rows before
// it draws — the counter is last there too — so a FOR UPDATE would hold a user
// row while waiting on the counter, against a send holding the counter and
// waiting for KEY SHARE on the same user at position 11. That is a cycle no
// ordering can remove, because the counter is genuinely before `users` for a
// bump and after it for a send. The weaker lock is what makes the two paths
// compose, and anything that later locks a user or member row owes the same
// argument.
//
// It is written down anyway for the two tickets most likely to want a key-level
// lock there: CANT-33 offboards accounts, and CANT-63 touches this file. A
// reader is entitled to treat this list as exhaustive, so it has to be.
//
// CANT-130's EnsurePerson (internal/store/persons.go) IS THE FIRST OF THOSE
// TO LAND, and it takes exactly this lock for exactly this reason: finding an
// existing person by email and reading `deactivated_at` to decide whether to
// re-invite them or refuse must not straddle a concurrent deactivation
// landing in between. FOR NO KEY UPDATE, never FOR UPDATE, on metadata.go's
// own argument — it does not conflict with the KEY SHARE a send takes on
// `users(author_id)` at position 11, so the two compose.
//
// IT DRAWS NO ORDINAL ON ITS CREATE PATH OR ITS PLAIN RE-INVITE, AND IT DOES ON
// ITS REVERSAL — the clause CANT-137 had to split, because the sentence here
// used to say "never" of the whole function and that is now true of only two of
// its three branches. `ensurePersonOnce` IS the reversal (it calls
// reactivateTx), so when the locked lookup finds a deactivated person it takes
// the CANT-134 paragraph's order below in full — the person's rooms first, from
// an unlocked peek above the lookup, and `log_counter` last, one statement
// before its Commit. An ordinary invite or re-invite of an active person locks
// `users` and `enrollment_tokens` and nothing else, and never enters the
// deployment-wide serialised section at all: the draw is gated on
// `deactivated_at` having actually been cleared, so every Purser bundle re-run
// stays out of it.
//
// Said here rather than left to the paragraph below because this list is read as
// exhaustive, and a reader looking up where EnsurePerson may take a lock must
// not find a claim that is true of two branches out of three.
//
// SetEmail (same file) TAKES THE SAME LOCK ON THE SAME ARGUMENT, added in
// review (#79): reading `email IS NOT NULL` and writing it on a later
// statement, with nothing held between them, let two racing `set-email`
// calls on one handle both read "no email yet" and the second silently
// overwrite the first's — the refusal `ErrPersonAlreadyHasEmail` exists
// precisely to prevent never firing. FOR NO KEY UPDATE on the lookup closes
// it the same way: the second caller blocks on the first's commit and reads
// the row as it now is.
//
// CANT-134's OFFBOARD AND ITS REVERSAL (internal/store/offboard.go) ARE THE
// TICKET THIS PARAGRAPH WAS WRITTEN FOR, and since CANT-137 BOTH DIRECTIONS
// take:
//
//	conversations (every room of the person, ascending id)  →
//	conversation_members  →  users  →  refresh_tokens  →  access_tokens  →
//	devices  →  enrollment_tokens  →  log_counter, LAST
//
// FOR NO KEY UPDATE on the user row, never FOR UPDATE, because setting or
// clearing `deactivated_at` is a NON-KEY update: it composes with the KEY
// SHARE a send takes on `users(author_id)` at position 11, so an offboard can
// neither block a send nor cycle against one, exactly as metadata.go's own
// argument requires of anything that locks a user row.
//
// THIS PARAGRAPH USED TO SAY "NEITHER DRAWS `log_counter`", AND CANT-137
// CORRECTED IT. Both directions now draw, exactly once, last. `member_count` and
// `read_by` count ACTIVE members (CANT-135 ruling 1), so setting or clearing that
// one column changes what /sync serves for every room the person is in — and
// without a marker the change reached nobody until something else happened to
// touch the room. So both directions enter the deployment-wide serialised
// section, briefly, after every row lock above is already held. Three reasons
// that cannot cycle, and the first is why `conversations` moved to the front:
//
//   - AGAINST A SEND. A send takes `conversations(X)` at position 8 and draws at
//     10. An offboard takes every room of the person before the counter and draws
//     last, so both writers agree that conversations comes before the counter,
//     and the two serialise on a conversation row or on the counter rather than
//     waiting on each other in opposite directions. The FOR NO KEY UPDATE on
//     `users` and `devices` still passes through the KEY SHARE position 11 takes,
//     so nothing there changed.
//   - AGAINST A METADATA BUMP. Identical table order, ascending id within each
//     table, counter last. That is also why the offboard cannot leave its
//     conversation locks to the bump at the bottom of its own transaction: the
//     bump runs after `users`, so those would be the wrong locks at the wrong
//     time, and holding `users` while reaching for `conversations` is the cycle
//     metadata.go's header describes from the other side.
//   - AGAINST invalidateFamily AND EnsurePerson'S CREATE PATH. Neither touches
//     `conversations`, `conversation_members` or the counter at all, so the two
//     new positions are invisible to both and the credential-table argument below
//     is unchanged.
//
// THE THREE CREDENTIAL TABLES ARE TAKEN IN invalidateFamily'S ORDER (refresh.go),
// and that is a hard requirement rather than a convention: a replay-triggered
// invalidation writes refresh_tokens → access_tokens → devices over rows that
// are a SUBSET of an offboard's, so the reverse order over the same rows is a
// cycle, and Postgres resolves it by aborting one — a 500 on either the
// offboard or the invalidation, both of which are the answer to somebody's
// credential being in doubt. `enrollment_tokens` comes last BEFORE THE COUNTER
// and cannot cycle
// against RedeemEnrollment, which takes that row FIRST and afterwards takes
// only KEY SHARE on `users(id)` (compatible) and a brand-new `devices` row
// (unlockable by anyone else): a redeem therefore never waits on an offboard,
// and a cycle needs both directions. A redeem takes neither `conversations` nor
// `log_counter`, so CANT-137's two new positions leave that argument alone.
//
// The devices write is also the thing that makes the OTHER mechanisms agree,
// as invalidateFamily's own comment says of its copy: Authenticate refuses a
// revoked device, DeadDevices names it, and CANT-30's gap re-check reaches the
// same verdict as the notification the offboard publishes.
//
// POSITION 12 TAKES NO LOCK WHEN IT RUNS, AND ITS COMMIT TAKES ONE THIS LIST
// WOULD OTHERWISE MISS. pg_notify appends to a backend-local pending list;
// nothing is locked until CommitTransaction reaches PreCommit_Notify, which
// takes an AccessExclusiveLock on "database 0" (async.c: "Serialize writers by
// acquiring a special lock that we hold till after commit") so queue entries
// land in commit order. That lock is INSTANCE-WIDE, not per database: every
// notifying committer on the shared Postgres serializes through it, other
// services' databases included, and RevokeDevice in tokens.go is the other
// transaction in this service that takes it. It cannot join a cycle: it is
// acquired inside commit, after every row lock above, and no transaction
// waits on a row lock after acquiring it, so it is last in every notifier's
// order by construction. What it costs is a second serial point nested inside
// the log_counter one — the deployment-wide section is draw, insert, notify,
// then a commit that queues under an instance-wide lock — and it is paid
// whether or not anyone is listening, because PreCommit_Notify returns early
// only when nothing is pending. Negligible at this scale, and named because an
// unnamed lock is one nobody can reason about.
//
// POSITION 11b TAKES ONE MORE KEY SHARE, AND NOTHING CAN CONTEND FOR IT. The
// attachment insert's foreign key locks the `messages` row it references —
// the row position 11 inserted one statement earlier, which no other
// transaction can see until this one commits. Named because this list is read
// as exhaustive, not because it can join a cycle.
//
// POSITION 7 TAKES WHATEVER THE RESOLVER TAKES, AND IT TAKES IT FIRST.
// RefuseUploads locks nothing and queries nothing. CANT-48's resolver has to
// lock and consume upload rows in this transaction (UploadResolver's contract),
// and at position 7 those locks come BEFORE `conversations`. So, stated outward:
//
//   - A RESOLVER LOCKS ONLY UPLOAD-SIDE ROWS — never `conversations`, `messages`
//     or `log_counter`. CANT-48 owns that half.
//   - NO PATH MAY LOCK `conversations` OR `messages` AND THEN AN UPLOAD-SIDE ROW.
//     CANT-67 owns that half: if its sweep ever reclaims upload rows while
//     holding a conversation floor, it does so in a separate transaction or
//     takes the upload rows first.
//
// Above the draw rather than beside 8b, and the difference is the reason. 8b
// moved below the draw because its lock is on `messages`, which CANT-67 takes
// after `conversations` — there was a direction to agree with. No path takes
// upload rows in any order yet. Below the draw, the resolver would run while
// this send holds the conversation row, and every send in the conversation
// would queue behind whatever lookup CANT-48 writes; above it, a slow resolver
// costs one pool connection.
//
// Taking log_counter LAST keeps the deployment-wide serialised section down to
// draw, insert, attachments, notify, commit rather than the whole transaction. The larger reason for
// fixing any order at all is that two writers taking the same locks in
// opposite orders DEADLOCK, and Postgres resolves a deadlock by aborting
// somebody's send.
//
// STATED OUTWARD, because two other tickets take some of these locks and only
// this file states the order:
//
//   - CANT-67's sweep advances a floor on `conversations` and then deletes
//     from `messages`. That is the same direction as this file, which is the
//     whole reason 8b sits below the conversation draw rather than at
//     position 6: taking `messages` first would invert against that sweep and
//     turn a rare permanent failure into a routine deadlock.
//   - CANT-63 draws log_counter when an edit bumps updated_log_seq, and takes
//     these in this order too.
//
//   - CANT-26's receipt write takes `conversation_members` and, since CANT-89,
//     `log_counter` after it — the per-member marker that carries
//     first_unread_seq and muted to that member's other devices.
//
// THAT LAST ONE CHANGED THE PICTURE AND THE SENTENCE HERE HAD TO CHANGE WITH
// IT. This used to say the send path and the receipt path "cannot order against
// each other at all", which was true while nothing in the receipt path drew
// from the counter. It no longer is: the two now share `log_counter`. What
// replaces it is a stronger claim than a coincidence — the send path never
// LOCKS `conversation_members` (it reads it, at the membership EXISTS below;
// "touches" would be too loose, and this file qualifies that distinction
// elsewhere), so the counter is the ONLY lock the two share, and both take it
// LAST. No cycle exists in either direction, and none can
// appear while the counter stays at the bottom of every writer's order, which
// is what this list is for.
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

// replyToResolveQuery is position 8b's read, named so the tests that prove the
// locking behaviour run THIS query rather than a copy of it.
//
// SCOPED TO THE CONVERSATION, and that is not a filter — it is what decides
// which row gets locked. `FOR KEY SHARE` locks only rows the query RETURNS, so
// with `WHERE id = $1` alone a send in conversation A would take, and hold to
// commit, a lock on a message in conversation B that it is about to discard.
// That is a lock nothing else in this transaction has any business holding, and
// it breaks the ordering rule below in a way the rule cannot express: two sends
// obeying "conversations then messages" can still deadlock with a sweep when
// the two locks are in DIFFERENT conversations. Scoped, the only row ever
// locked is the one position 11's foreign key would have locked anyway, which
// is what makes the claim in the ordering note true rather than nearly true.
const replyToResolveQuery = `SELECT conversation_id FROM messages
                              WHERE id = $1 AND conversation_id = $2 FOR KEY SHARE`

// replyToClassifyQuery runs ONLY when the scoped read above returns nothing,
// and only to decide which of the two drop reasons to log. It takes no lock, by
// design: this send will not store the value, so it has no business holding the
// row, and a diagnostic is not worth an ordering obligation.
const replyToClassifyQuery = `SELECT conversation_id FROM messages WHERE id = $1`

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

	// Attachments as the SENDER describes them, in the order they are to be
	// shown. Counted at position 2 against CATENARY_MAX_ATTACHMENTS, resolved
	// through the store's UploadResolver at position 7, and written at 11b in
	// this transaction with `position` set to this slice's index (CANT-85).
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
	switch {
	case isUniqueViolation(err, dedupConstraint):
		// Somebody else committed this key while we were drawing. Our ordinals
		// went back with the rollback; re-read theirs.
		return s.sentByKey(ctx, m.AuthorID, m.ClientID)

	case errors.Is(err, ErrUploadNotFound):
		// THE RACE LOSER UNDER A CONSUMING RESOLVER (CANT-85). Two sends under
		// one key — a client re-sending after a timeout while the first attempt
		// is still in flight — both pass position 4, because neither row exists
		// yet. The loser's resolver blocks on the winner's upload-row lock, sees
		// the handle consumed once the winner commits, and says not-found.
		// Refused as-is, that is upload_not_found, NOT RETRYABLE, for a message
		// every other member can see: the outbox marking failed something the
		// server kept.
		//
		// So before a not-found leaves the store, the key is read again. The
		// attempt's deferred rollback has already released the resolver's locks,
		// and the winner, if there is one, has committed — that is what made the
		// handle read as consumed. This is sound only because the resolver
		// contract requires locking what is consumed; see UploadResolver.
		//
		// It runs on every not-found, the kind mismatch included. A row found
		// there is the original, which position 4 would have returned anyway had
		// the winner committed a moment sooner.
		original, rerr := s.sentByKey(ctx, m.AuthorID, m.ClientID)
		switch {
		case rerr == nil:
			return original, nil
		case errors.Is(rerr, ErrNotFound):
			// The handle really is gone. The refusal stands.
			return Sent{}, err
		default:
			// The store could not confirm the refusal. Reporting the re-read's
			// own failure — transient, as a rule — is truer than a permanent
			// upload_not_found, and the key makes the client's retry free.
			return Sent{}, rerr
		}
	}
	return Sent{}, err
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

	// 7 — resolve the uploads this send names (CANT-85). Skipped entirely for a
	// send with no attachments; see resolveUploads.
	//
	// BELOW 4, so a replay never resolves: once CANT-48 consumes uploads, the
	// replay of an acked attachment send would otherwise be refused for handles
	// its own first attempt used up. BELOW 5, so a non-member hears not_a_member
	// before anything is said about what they sent.
	//
	// ABOVE 8, and that is the lock-order note's to explain: whatever the
	// resolver locks is taken before `conversations`, so a slow resolver costs
	// one pool connection rather than every send in this conversation queueing
	// behind it on the conversation row. The race window that leaves is closed
	// in send, not by moving this.
	atts, err := s.resolveUploads(ctx, tx, m)
	if err != nil {
		return Sent{}, err
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
	// Read inside the transaction AND under a key-share lock, so a source this
	// send will actually store cannot be deleted between here and the insert.
	// Without the lock there is a third outcome the snapshot argument does not
	// cover: visible here, gone at position 11, and the FK raises 23503 — which
	// is not a transient class, so the send fails permanently over a reply_to
	// the sender cannot fix.
	//
	// One query in the happy path. The second runs only on a drop, which is
	// rare and already writing a log line.
	replyTo := m.ReplyTo
	if replyTo != nil {
		var sourceConv uuid.UUID
		err := tx.QueryRow(ctx, replyToResolveQuery, *replyTo, m.ConversationID).Scan(&sourceConv)
		switch {
		case err == nil:
			// Resolved, in this conversation, and now held.
		case !errors.Is(err, pgx.ErrNoRows):
			return Sent{}, fmt.Errorf("store: resolve reply_to: %w", err)
		default:
			// Dropped. LOGGED at info with both ids, because this is the one
			// case where the served Message differs from what the sender's
			// client optimistically rendered — someone will eventually ask why
			// the reply arrow vanished, and the answer should be findable.
			//
			// The unscoped read here is what keeps the two reasons apart. It
			// is worth one extra round trip on a path that is already
			// exceptional, and it takes no lock.
			var elsewhere uuid.UUID
			switch classifyErr := tx.QueryRow(ctx, replyToClassifyQuery, *replyTo).Scan(&elsewhere); {
			case classifyErr == nil:
				s.logger.InfoContext(ctx, "reply_to dropped: the source is in another conversation",
					"conversation_id", m.ConversationID, "author_id", m.AuthorID,
					"reply_to_message_id", *replyTo, "source_conversation_id", elsewhere)
			case errors.Is(classifyErr, pgx.ErrNoRows):
				s.logger.InfoContext(ctx, "reply_to dropped: the source does not resolve",
					"conversation_id", m.ConversationID, "author_id", m.AuthorID,
					"reply_to_message_id", *replyTo)
			default:
				return Sent{}, fmt.Errorf("store: classify a dropped reply_to: %w", classifyErr)
			}
			replyTo = nil
		}
	}

	// 9 — DELIBERATELY EMPTY. The author's own read_seq used to be advanced
	// here, and CANT-83 removed it.
	//
	// 0002_conversations justified the write as making first_unread_seq
	// arithmetic — read_seq + 1 — "rather than a scan that has to skip your own
	// messages". It is not a scan: UNIQUE (conversation_id, seq) already indexes
	// it, so CANT-26 derives it as the first seq above read_seq this viewer did
	// not write, which stops at the first row it finds. 0005_read_seq_derivation
	// carries the correction into the column comment.
	//
	// The arithmetic version was WRONG in a case that is not exotic. An author
	// with five unread messages who replies without opening the thread drew a
	// new seq, and the floor swallowed the five below it: the badge and the "N
	// NEW" divider vanished on every one of their devices, permanently, for
	// messages they had never seen. A single scalar read_seq cannot say "1-5
	// unread, 6 is mine" — the author filter can, and it costs one indexed
	// lookup per conversation on a rail nobody has built yet.
	//
	// GIVEN UP WITH IT, knowingly: the row count of that UPDATE was an exact
	// membership re-check under the member row lock, immediately before commit,
	// and position 5's EXISTS takes no lock. A membership revoked between the
	// two now lets one more message through from someone who was a member when
	// asked. That is the trade — the alternative is a FOR SHARE at position 5,
	// which takes the member row BEFORE `conversations` and inverts the order
	// against CANT-67's sweep. A benign extra message beats a deadlock class.

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

	// 11b — the attachment rows, in ONE statement, on tx (CANT-85). After the
	// message because the foreign key needs the row; before the notify because
	// the notify is the last statement before Commit.
	//
	// A MESSAGE AND ITS ATTACHMENTS COMMIT TOGETHER OR NOT AT ALL. A committed
	// message whose rows are missing is visible on /sync with a hole in it, and
	// the client has no way to ask again. A failure here — a CHECK, the FK, a
	// dropped connection — returns through the same deferred rollback as every
	// failure below position 10, which takes the message and un-draws both
	// ordinals with it.
	if err := insertAttachments(ctx, tx, out.ID, atts); err != nil {
		return Sent{}, err
	}

	// 12 — notify, INSIDE the transaction, on the same connection, LAST
	// before commit. CANT-18 ruling 2 (not CANT-14's ruling 2 above, which
	// is the idempotency order): Postgres delivers a notification at commit,
	// so raised here it fires exactly when the message becomes visible and
	// never when it does not. Raised anywhere else it is a second write that
	// can succeed without the message, or the reverse — and only the FIRST
	// of those is something a test can see (the rollback test went red on a
	// pool.Exec here). A message without its notify — the after-commit
	// placement — has no observer, which is why this line is read rather
	// than only tested.
	//
	// The payload is CANT-21's, built from the values THIS transaction wrote
	// and never re-read: (conversation_id, seq) is UNIQUE on messages, so a
	// listening instance fetches exactly the row that committed. Encode
	// enforces the 8,000-byte cap, which is a send-path property here rather
	// than a fanout one — pg_notify raises on the connection that called it,
	// so an over-cap payload would fail the send, not drop a notification.
	// Loud rather than silent on both errors, on the same reasoning as
	// RevokeDevice in tokens.go, which is this block's twin and stays
	// byte-identical in shape.
	//
	// CANT-85 landed the attachments insert ABOVE this, at 11b. The notify is
	// the last statement before Commit, and it stays last.
	payload, err := NotifyPayload{ConversationID: m.ConversationID, Seq: out.Seq}.Encode()
	if err != nil {
		return Sent{}, fmt.Errorf("store: notify: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, payload); err != nil {
		return Sent{}, fmt.Errorf("store: notify: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Sent{}, fmt.Errorf("store: commit: %w", err)
	}
	return out, nil
}

// sentByKey re-reads the winner of a race under one key. Two races reach it:
// the dedup constraint at position 11, and — since CANT-85 — a consumed upload
// at position 7, where the loser's resolver sees the winner's handles already
// used. Either way the attempt has rolled back, so this reads on the pool.
func (s *Store) sentByKey(ctx context.Context, authorID, clientID uuid.UUID) (Sent, error) {
	var out Sent
	err := s.pool.QueryRow(ctx,
		`SELECT id, conversation_id, seq, log_seq, at FROM messages WHERE author_id = $1 AND client_id = $2`,
		authorID, clientID).Scan(&out.ID, &out.ConversationID, &out.Seq, &out.LogSeq, &out.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sent{}, ErrNotFound
	}
	if err != nil {
		return Sent{}, fmt.Errorf("store: re-read by idempotency key: %w", err)
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
