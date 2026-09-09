package store

// CANT-89 — the change marker, and the ONE place it is drawn.
//
// `/sync` serves a conversation or a user whose marker is above the caller's
// cursor, which is the half of SyncResponse.conversations' and .users' promise
// that loadConversations and loadUsers used to concede they did not keep.
//
// THE DRAW IS NOT IN THE SAME STATEMENT AS THE MUTATION, AND THAT IS
// DELIBERATE. The obvious shape is a data-modifying CTE:
//
//	WITH d AS (UPDATE log_counter … RETURNING value)
//	UPDATE conversations SET name = $2, metadata_log_seq = d.value FROM d …
//
// Postgres documents that sub-statements in WITH "are executed concurrently
// with each other and with the main query" and that "the order in which the
// specified updates actually happen is unpredictable". So a CTE cannot promise
// the conversation row is locked BEFORE the counter row — and messages.go's
// lock order says the counter is taken LAST for every writer. A send holding
// conversations(A) and waiting on log_counter, against a rename holding
// log_counter and waiting on conversations(A), is exactly the cycle that rule
// exists to prevent. It would also not have covered half the trigger list: a
// membership change writes conversation_members AND conversations, so "one
// statement" was never on offer for it.
//
// So each bump is: LOCK the target row, DRAW, WRITE — three statements, one
// transaction, counter last. What matters is not the statement count but that
// no drawn value ever crosses a commit boundary, which is Invariant 1 for this
// column: a marker drawn in one transaction and written in another is a
// bigserial by a different route, and the message it loses is a rename that
// commits after a page has already advanced past its number.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// drawMetadataLogSeq takes the next value from the deployment-wide counter, on
// the CALLER'S transaction.
//
// Private, and the only statement in the service module that draws for a
// marker — metadata_guard_test.go fails the build on a second one. It is not
// exported because a caller outside this file could hold the value across a
// commit, which is the one thing this design forbids; every exported path goes
// through a bump function below, which draws and writes together.
//
// The counter row lock is held from here to commit, which is what makes every
// value at or below a `SELECT value FROM log_counter` already committed. That
// property is what lets `/sync` treat the marker exactly as it treats log_seq.
func drawMetadataLogSeq(ctx context.Context, tx pgx.Tx) (int64, error) {
	var v int64
	if err := tx.QueryRow(ctx,
		`UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: draw metadata_log_seq: %w", err)
	}
	return v, nil
}

// bumpConversationMetadata marks a conversation's shared metadata as changed:
// name, kind, membership, retention.
//
// LOCKS FIRST. The caller's own UPDATE would take the same row lock, but taking
// it here rather than trusting the caller is what makes the order a property of
// this function instead of a convention four call sites have to remember — and
// a lock already held by this transaction costs nothing to take again.
//
// It has no production caller yet, because no rename, promotion or membership
// change exists (CANT-75 writes the first). That is the point of the guard
// test: the day one is written, it fails the build until it comes through here.
func bumpConversationMetadata(ctx context.Context, tx pgx.Tx, conv uuid.UUID) (int64, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id = $1 FOR UPDATE`, conv).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, fmt.Errorf("store: bump conversation metadata: %w", ErrNotAMember)
	}
	if err != nil {
		return 0, fmt.Errorf("store: bump conversation metadata: lock: %w", err)
	}
	v, err := drawMetadataLogSeq(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE conversations SET metadata_log_seq = $2 WHERE id = $1`, conv, v); err != nil {
		return 0, fmt.Errorf("store: bump conversation metadata: %w", err)
	}
	return v, nil
}

// bumpMemberMetadata marks ONE member's per-member state as changed:
// first_unread_seq and muted.
//
// Per member rather than per conversation, and that is ruling 1's whole
// argument. A receipt changes what ONE person's devices should see; a
// conversation-level marker would wake all seven for it.
func bumpMemberMetadata(ctx context.Context, tx pgx.Tx, conv, user uuid.UUID) (int64, error) {
	var got uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT user_id FROM conversation_members
		  WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, conv, user).Scan(&got)
	if err == pgx.ErrNoRows {
		return 0, fmt.Errorf("store: bump member metadata: %w", ErrNotAMember)
	}
	if err != nil {
		return 0, fmt.Errorf("store: bump member metadata: lock: %w", err)
	}
	v, err := drawMetadataLogSeq(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE conversation_members SET metadata_log_seq = $3
		  WHERE conversation_id = $1 AND user_id = $2`, conv, user, v); err != nil {
		return 0, fmt.Errorf("store: bump member metadata: %w", err)
	}
	return v, nil
}

// bumpUserMetadata marks a display name as changed.
//
// Reaches only viewers who share a conversation with this user — ruling 3, and
// the guard lives in loadUsers' query rather than here, because it is a fact
// about the READER and this function does not have one.
func bumpUserMetadata(ctx context.Context, tx pgx.Tx, user uuid.UUID) (int64, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, user).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, fmt.Errorf("store: bump user metadata: no such user %s", user)
	}
	if err != nil {
		return 0, fmt.Errorf("store: bump user metadata: lock: %w", err)
	}
	v, err := drawMetadataLogSeq(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET metadata_log_seq = $2 WHERE id = $1`, user, v); err != nil {
		return 0, fmt.Errorf("store: bump user metadata: %w", err)
	}
	return v, nil
}
