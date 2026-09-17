package store

// CANT-97 — the rotating exchange, and the four doors it closes in one write.
//
// CANT-28 ruling 5 put this here rather than in that ticket so rotation gets
// the same line-by-line read as the reuse detection built over it. The columns
// were landed there and written by nothing; this is what writes them.
//
// WHAT IS NOT HERE, AND IS NOT FORGOTTEN. A replay — a second presentation of a
// token that already has `replaced_by` — is REFUSED here and does not yet
// invalidate its family. That is CANT-29's, by its own `Done when`: "a replayed
// refresh invalidates the family, not the request". The seam it needs is the
// zero-row branch of the conditional update below, which is exactly the state
// its detection reasons over.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Rotated is one exchange's result: the next pair, and who it belongs to.
//
// Both tokens are new. A response that returned the presented refresh token
// unchanged would be a non-rotating credential wearing a rotating one's name,
// which is the shape CANT-28 exists to rule out — and the wire schema says so
// in RefreshResponse's own description, so the two cannot drift.
type Rotated struct {
	UserID   uuid.UUID
	DeviceID uuid.UUID
	Access   IssuedToken
	Refresh  IssuedToken
}

// RotateRefresh exchanges a refresh token for the next pair.
//
// ONE WRITE DECIDES EVERYTHING, AND THE READ ABOVE IT DECIDES NOTHING. The
// SELECT exists only to learn what the INSERT needs — the device to hang the
// successor on, and the family to carry forward — and it deliberately does NOT
// check whether the token has been rotated, revoked, or expired. Every one of
// those is a predicate on the conditional UPDATE, and that update is the sole
// authority.
//
// THAT IS A CORRECTION, AND IT IS WORTH THE PARAGRAPH. The first version of
// this function checked all four doors in the read and then repeated them in
// the write. It was measurably worse than it looks. The concurrency test that
// is this ticket's central criterion PASSED against a build with the
// single-use predicate deleted from the update — because the read refused
// every racer first, so the eight goroutines serialised at the SELECT and the
// test was exercising a sequential replay seven times rather than the race it
// names. A criterion that a broken implementation satisfies is not a
// criterion. With the doors in one place, a replay and a lost race are the
// same zero-row result from the same statement, so the test cannot pass
// without the predicate whether the racers overlap or not.
//
// FOUR DOORS, ALL IN THAT ONE WRITE. The token must be live, unrotated and
// unexpired; its DEVICE must not be revoked; its ACCOUNT must not be
// deactivated. The last two are joins rather than separate queries, so the
// refusal is atomic with the rotation:
//
//   - users.deactivated_at is R6's third door. Purser's offboard is
//     disable-then-revoke, and the whole argument for that ordering rests on
//     one sentence — "a disabled account cannot refresh a token or enroll a
//     device". CANT-28 made the enroll half true; this is the refresh half.
//     Without it the offboard fails open in exactly the way the rejected
//     ordering was rejected for.
//   - devices.revoked_at is the device's, found on CANT-28's own review. A
//     write reasoning over the token row alone would let a revoked device keep
//     rotating its family, because RevokeDevice does not touch that device's
//     refresh_tokens. No ACCESS comes of it — Authenticate's join refuses the
//     minted token — but it leaves a live rotating chain in precisely the
//     state CANT-29's detection reasons over, which is the wrong thing to hand
//     that ticket.
//
// NO EXPLICIT `FOR UPDATE`, AND NO LOCK ANY OTHER PATH CONTENDS FOR. Under
// READ COMMITTED the losing UPDATE blocks on the winner's row lock,
// re-evaluates its WHERE against the committed version and matches nothing.
// That is the whole of the atomicity, and it is what makes two in-flight
// requests from a waking phone produce one new pair and one refusal rather
// than a forked family with `replaced_by` written twice. `log_counter` is
// never drawn, so this never enters the deployment-wide serialised section.
func (s *Store) RotateRefresh(ctx context.Context, presented string) (Rotated, error) {
	if presented == "" {
		// DEBUG, matching Authenticate: this is somebody presenting nothing,
		// which the request log already records as a 401. At WARN it would be a
		// second line per anonymous probe on a route with no limiter.
		s.logger.DebugContext(ctx, "refresh refused", "reason", "no credential presented")
		return Rotated{}, ErrUnauthorized
	}

	// Hashed once. The lookup and the comparison must be the same bytes, and
	// computing it twice is how they would stop being.
	presentedHash := HashToken(presented)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Rotated{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		tokenID    uuid.UUID
		familyID   uuid.UUID
		deviceID   uuid.UUID
		userID     uuid.UUID
		storedHash []byte
	)
	// WHAT THE INSERT NEEDS, AND NOTHING THAT DECIDES ANYTHING. A reader
	// checking this function for correctness should be able to assume every
	// column below is stale by the time it is used, because the update
	// re-checks the ones that matter.
	err = tx.QueryRow(ctx, `
		SELECT r.id, r.family_id, r.device_id, d.user_id, r.token_hash
		  FROM refresh_tokens r
		  JOIN devices d ON d.id = r.device_id
		 WHERE r.token_hash = $1`,
		presentedHash).
		Scan(&tokenID, &familyID, &deviceID, &userID, &storedHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Unknown by definition, so there is nothing to name but the fact. The
		// credential itself is never logged.
		s.logger.WarnContext(ctx, "refresh refused", "reason", "unknown credential")
		return Rotated{}, ErrUnauthorized
	case err != nil:
		return Rotated{}, fmt.Errorf("store: rotate refresh: %w", err)
	}

	// The lookup was an equality match on an indexed column, so this can only
	// fail if the index and the row disagree. Checked anyway, in constant time,
	// so the comparison exists where a reader looks for it.
	if !tokensEqual(storedHash, presentedHash) {
		s.logger.WarnContext(ctx, "refresh refused", "reason", "hash mismatch", "token_id", tokenID)
		return Rotated{}, ErrUnauthorized
	}

	now := ServerTime()

	// The successor, carrying the family forward. Inserted BEFORE the
	// conditional update so that a refusal discards it on the rollback — a
	// successor row whose predecessor was never marked is a second live head of
	// one family, which is the fork this ordering exists to prevent.
	newID := uuid.New()
	refresh, err := insertRefresh(ctx, tx, newID, deviceID, familyID, now)
	if err != nil {
		return Rotated{}, err
	}

	// THE AUTHORITY. Single-use, unexpired, un-revoked, live device, live
	// account — all five in one statement, so the outcome cannot depend on
	// anything read earlier having still been true.
	var rotated uuid.UUID
	err = tx.QueryRow(ctx, `
		UPDATE refresh_tokens r
		   SET replaced_by = $2
		  FROM devices d
		  JOIN users u ON u.id = d.user_id
		 WHERE r.id = $1
		   AND r.replaced_by IS NULL
		   AND r.revoked_at IS NULL
		   AND r.expires_at > $3
		   AND d.id = r.device_id
		   AND d.revoked_at IS NULL
		   AND u.deactivated_at IS NULL
		 RETURNING r.id`,
		tokenID, newID, now).Scan(&rotated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// One refusal to the caller, and the rollback takes the successor with
		// it. WHICH door closed is read back afterwards and written to the log
		// only — CANT-28 declines a rate limiter on the credential routes and
		// names visibility as what stands in for it, which is worth nothing if
		// every refusal logs the same sentence.
		s.logRotationRefusal(ctx, tx, tokenID, familyID, deviceID, userID)
		return Rotated{}, ErrUnauthorized
	case err != nil:
		return Rotated{}, fmt.Errorf("store: rotate refresh: mark replaced: %w", err)
	}

	access, err := issueAccess(ctx, tx, userID, &deviceID, now)
	if err != nil {
		return Rotated{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Rotated{}, fmt.Errorf("store: commit: %w", err)
	}
	s.logger.InfoContext(ctx, "refresh rotated",
		"token_id", tokenID, "replaced_by", newID, "family_id", familyID,
		"device_id", deviceID, "user_id", userID)
	return Rotated{UserID: userID, DeviceID: deviceID, Access: access, Refresh: refresh}, nil
}

// logRotationRefusal names the door that closed, AFTER the write has already
// refused.
//
// A DIAGNOSTIC AND NOT A DECISION, which is the whole reason it runs second.
// Deciding here would put the doors in two places and let them disagree; by
// reading only once the update has declined, this cannot change an outcome
// however wrong it is. It runs inside the same transaction, before the
// rollback, so it sees the same snapshot the update just refused against.
//
// `lost the rotation race` is the DEFAULT rather than a case, and that is
// deliberate: if every visible door is open, the row changed under this
// transaction between the update and this read, which is a waking phone's
// second in-flight request and the one cause with nothing in the row to name
// it by.
func (s *Store) logRotationRefusal(ctx context.Context, tx pgx.Tx, tokenID, familyID, deviceID, userID uuid.UUID) {
	var (
		replacedBy    *uuid.UUID
		tokenRevoked  *time.Time
		expiresAt     time.Time
		deviceRevoked *time.Time
		deactivated   *time.Time
	)
	if err := tx.QueryRow(ctx, `
		SELECT r.replaced_by, r.revoked_at, r.expires_at, d.revoked_at, u.deactivated_at
		  FROM refresh_tokens r
		  JOIN devices d ON d.id = r.device_id
		  JOIN users u ON u.id = d.user_id
		 WHERE r.id = $1`, tokenID).
		Scan(&replacedBy, &tokenRevoked, &expiresAt, &deviceRevoked, &deactivated); err != nil {
		// The refusal still stands; only its explanation is missing. Logged as
		// such rather than swallowed, because a refusal nobody can account for
		// is the one worth finding in a log.
		s.logger.WarnContext(ctx, "refresh refused",
			"reason", "cause could not be read", "token_id", tokenID, "error", err)
		return
	}

	reason := "lost the rotation race"
	switch {
	case replacedBy != nil:
		// THE REPLAY, AND THE ONE CANT-29 IS WAITING FOR. The family is named
		// because that ticket's invalidation is one predicate over this column.
		reason = "token already rotated"
	case tokenRevoked != nil:
		reason = "token revoked"
	case !expiresAt.After(ServerTime()):
		reason = "token expired"
	case deviceRevoked != nil:
		reason = "device revoked"
	case deactivated != nil:
		reason = "account deactivated"
	}
	s.logger.WarnContext(ctx, "refresh refused", "reason", reason,
		"token_id", tokenID, "family_id", familyID, "device_id", deviceID, "user_id", userID)
}

// insertRefresh writes one refresh token into a family.
//
// THE ID IS THE CALLER'S RATHER THAN THIS FUNCTION'S, and that is what lets the
// two callers share one INSERT. Enrollment needs it so the first token can be
// its own family; rotation needs it so the predecessor's `replaced_by` can name
// the row this writes. A helper that minted its own id would have to return it
// anyway, and the version of this that duplicated the INSERT instead is how the
// two paths would come to disagree about a column.
func insertRefresh(ctx context.Context, tx pgx.Tx, id, deviceID, family uuid.UUID, now time.Time) (IssuedToken, error) {
	plaintext, hash, err := MintToken()
	if err != nil {
		return IssuedToken{}, err
	}
	expires := now.Add(RefreshTokenLifetime)
	if _, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (id, device_id, token_hash, family_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, id, deviceID, hash, family, expires); err != nil {
		return IssuedToken{}, fmt.Errorf("store: issue refresh token: %w", err)
	}
	return IssuedToken{Plaintext: plaintext, ExpiresAt: expires}, nil
}
