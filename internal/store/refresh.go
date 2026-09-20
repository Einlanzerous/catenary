package store

// CANT-97 — the rotating exchange, and the four doors it closes in one write.
//
// CANT-28 ruling 5 put this here rather than in that ticket so rotation gets
// the same line-by-line read as the reuse detection built over it. The columns
// were landed there and written by nothing; this is what writes them.
//
// CANT-29 IS ALSO HERE NOW: a replayed refresh invalidates its whole family
// rather than just the request. refusalOutcome is where a refusal is turned
// into a meaning, and invalidateFamily is what a replay costs.
//
// THE ZERO-ROW RESULT IS A SUPERSET OF REPLAY, which is the difficulty the
// whole of CANT-29 is about. A presentation whose conditional update returns
// zero rows against a row that already carries `replaced_by` is a replay — and
// so is a benign double-refresh from a waking phone, and so is a client
// retrying after a response was lost in flight. Nothing in the row separates
// them, so the distinguisher is TIME: see ReuseGraceWindow, and
// docs/decisions/cant-29-reuse-detection.md for the decision and its exposure.
//
// An earlier version of this comment called that branch "exactly the state its
// detection reasons over", which invited exactly the mistake CANT-97's
// criterion 1 warns about — invalidating a real person's family on a legitimate
// double-refresh, and reporting it as the incident it is meant to detect.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ReuseGraceWindow is how long after a rotation a presentation of the spent
// token is read as an echo rather than as a replay.
//
// A CONSTANT RATHER THAN CONFIG, on exactly the terms CANT-28's three lifetimes
// get: this is a security posture a human picked, and an environment variable
// here would be a way to widen the window without anybody re-reading the
// argument for it.
//
// IT IS SIZED FOR CONCURRENT IN-FLIGHT REQUESTS AND FOR NOTHING LONGER. A phone
// waking up dispatches two refreshes within milliseconds of each other; the
// loser's presentation arrives just after the winner commits, the client
// already holds the winner's pair, and nothing is wrong. That is the false
// positive this window exists for, and CANT-97's own concurrency test is
// exactly its shape. It is NOT sized for a network retry minutes later: a
// working device's next refresh is a quarter of an hour away, so a theft's
// victim presenting their stale token falls outside this by three orders of
// magnitude.
//
// THE EXPOSURE IT BUYS, STATED RATHER THAN LEFT TO BE FOUND. An attacker who
// replays a stolen token within ten seconds of the victim's own rotation
// escapes family invalidation. They still get a 401 and the spent token still
// buys them nothing — what they escape is the DETECTION. That is the trade and
// it is the right way round: the alternative invalidates a real person's family
// every time their phone wakes up, and a detector that fires on ordinary
// traffic is one that gets turned off.
// THE AGE IT IS COMPARED AGAINST IS COMPUTED BY POSTGRES, NOT BY THIS PROCESS,
// and that is a correction rather than a detail (found in review on #64).
// `issued_at` is stamped by the database — insertRefresh does not write that
// column, so it comes from 0007's `DEFAULT now()` — while ServerTime() is
// time.Now() in the application. Subtracting one from the other made this
// ticket's entire discrimination a comparison across two hosts' clocks against
// a ten-second threshold: a database more than ten seconds ahead makes every
// age negative, every presentation an echo, and the detector silently off,
// with every test still green because CI shares one clock between the runner
// and its Postgres service. refusalOutcome therefore asks the database for the
// age directly.
//
// `now()` IS TRANSACTION-START TIME on both sides, so a slow winner spends part
// of the loser's budget. At milliseconds against ten seconds that is noise, and
// it is named here so it is not rediscovered as a bug.
const ReuseGraceWindow = 10 * time.Second

// refusalOutcomeBudget bounds the work a refusal triggers once it has been
// detached from the caller's context. Five seconds, matching the only other
// WithoutCancel in this package (notify.go's connection close).
const refusalOutcomeBudget = 5 * time.Second

// rotateBudget bounds the rotation transaction, and IT IS DELIBERATELY WELL
// BELOW ReuseGraceWindow (CANT-125). Three seconds against ten.
//
// The successor's `issued_at` is `now()`, and in Postgres that is the
// transaction's START. A winner that took twelve seconds to commit would stamp
// its successor twelve seconds in the past, and the client's own legitimate
// retry — arriving the instant that commit became visible — would already read
// as older than the window: a replay, and a revoked device, produced by nothing
// but a slow database. Bounding the transaction bounds that skew. A rotation
// that cannot finish inside the budget does not commit at all, and the client
// retries a token that is still good.
const rotateBudget = 3 * time.Second

// ErrRefreshRetry is a rotation that was neither performed nor refused: the
// client should send the request again. The handler answers it 503.
//
// IT IS NEVER A 401, AND THAT IS ITS WHOLE PURPOSE. Since CANT-123 a client
// treats Catenary's own 401 on /refresh as terminal — the credential is gone,
// stop, re-enroll — so answering 401 to a client whose token is perfectly good,
// or whose rotation has in fact just succeeded, would log a person out of a
// working device. See routeCollision for the two cases that produce it.
var ErrRefreshRetry = errors.New("store: refresh not performed; retry")

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
	return s.RotateRefreshProposing(ctx, presented, "")
}

// RotateRefreshProposing is RotateRefresh with the client's PROPOSED successor
// (CANT-31 ruling 1, CANT-125). An empty proposal is the original behaviour,
// byte for byte: the server mints the successor.
//
// WHAT THE PROPOSAL CHANGES, AND THE ONLY THING. The successor row's
// token_hash is hash(proposed) instead of the hash of a token minted here, and
// the plaintext handed back is the proposal. Every door is unchanged and is
// still the one conditional UPDATE below. A client that proposed its successor
// knows it even when the response is lost, which is what this buys.
//
// WHAT IT ADDS IS ONE NEW WAY TO FAIL, BEFORE THE DOORS. token_hash is UNIQUE,
// so a proposal whose hash is already stored makes the successor's INSERT raise
// 23505 — and the INSERT runs before the UPDATE that would have refused a spent
// token. Without routing, that is a 500 for the client retrying (R, P) after a
// lost response, and worse, it is a 500 for a thief replaying a copied (R, P):
// the replay would never reach reuse detection. routeCollision decides what the
// collision meant.
//
// THE PROPOSAL'S ENCODING IS THE CALLER'S TO HAVE CHECKED. The handler decodes
// it with the generated decoder, which holds it to the Token pattern; this
// function hashes what it is given.
func (s *Store) RotateRefreshProposing(ctx context.Context, presented, proposed string) (Rotated, error) {
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

	// A PROPOSAL EQUAL TO THE PRESENTED TOKEN asks for a credential rotated
	// into itself. It would collide with R's own row and route as a bad
	// proposal anyway; refused here so the reason is legible, and as a retry
	// rather than a 401 for the reason ErrRefreshRetry gives.
	if proposed != "" && proposed == presented {
		s.logger.WarnContext(ctx, "refresh not performed", "reason", "the proposed successor is the presented token")
		return Rotated{}, ErrRefreshRetry
	}

	// BOUNDED — see rotateBudget. The caller's context still cancels it sooner.
	// txCtx governs the transaction ONLY: the refusal and collision paths below
	// run on the caller's ctx, because they detach from it themselves and must
	// not inherit a deadline that may already have passed.
	txCtx, cancelTx := context.WithTimeout(ctx, rotateBudget)
	defer cancelTx()
	callerCtx := ctx
	ctx = txCtx

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
	refresh, err := insertRefreshProposed(ctx, tx, newID, deviceID, familyID, now, proposed)
	if proposed != "" && isTokenHashCollision(err) {
		// ROLLED BACK FIRST, for the reason the refusal below gives: what
		// follows reasons about COMMITTED state, and may revoke a family this
		// transaction must not then commit a token into. The failed INSERT has
		// aborted the transaction anyway; this makes the order explicit.
		_ = tx.Rollback(ctx)
		if s.collisionFault == faultCollisionUnrouted {
			return Rotated{}, err // the 23505 as it was before routing existed: a 500
		}
		return Rotated{}, s.routeCollision(callerCtx, tokenID, familyID, deviceID, userID, HashToken(proposed))
	}
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
		// ROLLED BACK EXPLICITLY, AND BEFORE ANYTHING ELSE. The ordering is
		// load-bearing rather than tidy: this transaction is holding the
		// uncommitted successor row insertRefresh wrote above, and CANT-29's
		// invalidation below revokes a whole family. Doing both in this
		// transaction and committing would mint a brand-new live token INTO the
		// family it had just revoked — the one outcome an invalidation must not
		// produce. The deferred Rollback would fire eventually, but eventually
		// is after the work below, and that work has to reason about committed
		// state rather than about this transaction's own insert.
		_ = tx.Rollback(ctx)
		s.refusalOutcome(callerCtx, tokenID, familyID, deviceID, userID)
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

// routeCollision decides what it MEANT that a proposed successor's hash was
// already stored (CANT-125). It runs after the rotation transaction has rolled
// back, on the pool, so it reads committed state — the same terms
// refusalOutcome runs on, and for the same reasons.
//
// THREE BRANCHES, AND THE ORDER IS THE SECURITY ARGUMENT. It re-reads R, the
// PRESENTED token, and asks how R relates to C, the row that already holds
// hash(P):
//
//  1. R IS STILL LIVE — `replaced_by IS NULL`. Nothing has rotated R, so this
//     is not a retry of anything: the client proposed a string that some other
//     token already hashes to. A reused proposal, or a badly seeded generator.
//     R is a healthy token and the client keeps it. ErrRefreshRetry → 503; the
//     client mints a FRESH proposal and succeeds. THIS MUST NEVER REACH
//     refusalOutcome — not because refusalOutcome would revoke anything (R is
//     unspent, so it would not), but because the caller would be told 401, and
//     a 401 here is now a logout.
//
//  2. R WAS ROTATED INTO C, MOMENTS AGO — `replaced_by` IS C and C was issued
//     inside ReuseGraceWindow. This is the in-flight retry: the same client
//     sent (R, P), the rotation committed, the response was lost or is still
//     on its way, and it sent (R, P) again. The rotation it is asking for HAS
//     HAPPENED and P is its new token. ErrRefreshRetry → 503, and the client
//     presents P. Never a 500, and never a 401.
//
//  3. ANYTHING ELSE FALLS THROUGH TO refusalOutcome, UNCHANGED. R rotated into
//     C outside the window is a copied (R, P) pair being replayed — and it
//     invalidates the family exactly as a spent token presented without a
//     proposal does today. R rotated into some OTHER row, R revoked, anything
//     unexpected: all of it is a presentation of a token that is not live, and
//     the existing detector is the authority on what that means. The proposal
//     buys a thief nothing.
//
// THE AGE IS SUBTRACTED IN SQL, on the database's clock, as refusalOutcome's
// is; and the window is the SAME constant, so there is one line between an
// echo and a replay and not two.
func (s *Store) routeCollision(ctx context.Context, tokenID, familyID, deviceID, userID uuid.UUID, proposedHash []byte) error {
	// Detached and bounded, exactly as refusalOutcome is and for its reason: a
	// caller who hangs up must not be able to skip the detector.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refusalOutcomeBudget)
	defer cancel()

	var (
		replacedBy  *uuid.UUID
		collidingID *uuid.UUID
		ageSec      *float64
	)
	err := s.pool.QueryRow(rctx, `
		SELECT r.replaced_by, c.id,
		       EXTRACT(EPOCH FROM (now() - c.issued_at))::float8
		  FROM refresh_tokens r
		  LEFT JOIN refresh_tokens c ON c.token_hash = $2
		 WHERE r.id = $1`, tokenID, proposedHash).Scan(&replacedBy, &collidingID, &ageSec)
	if err != nil {
		// UNREADABLE MEANS THE DETECTOR DECIDES. The safe reading of "a
		// collision I cannot explain" is not "tell the client to retry".
		s.logger.WarnContext(rctx, "refresh collision could not be read; deferring to reuse detection",
			"token_id", tokenID, "error", err)
		s.refusalOutcome(ctx, tokenID, familyID, deviceID, userID)
		return ErrUnauthorized
	}

	switch {
	case replacedBy == nil && s.collisionFault != faultCollisionNoLiveBranch:
		s.logger.WarnContext(rctx, "refresh not performed",
			"reason", "the proposed successor collides with a stored token; the presented token is unspent",
			"token_id", tokenID, "family_id", familyID, "device_id", deviceID, "user_id", userID)
		return ErrRefreshRetry

	case replacedBy != nil && collidingID != nil && *replacedBy == *collidingID && ageSec != nil &&
		time.Duration(*ageSec*float64(time.Second)) < ReuseGraceWindow:
		s.logger.InfoContext(rctx, "refresh not performed",
			"reason", "in-flight retry: this token was rotated into the proposed successor moments ago",
			"token_id", tokenID, "replaced_by", *replacedBy, "family_id", familyID,
			"device_id", deviceID, "user_id", userID,
			"rotated_age_ms", time.Duration(*ageSec*float64(time.Second)).Milliseconds(),
			"grace_window_ms", ReuseGraceWindow.Milliseconds())
		return ErrRefreshRetry
	}

	s.refusalOutcome(ctx, tokenID, familyID, deviceID, userID)
	return ErrUnauthorized
}

// refusalOutcome decides what a refused rotation MEANS, and it is the whole of
// CANT-29.
//
// IT RUNS AFTER THE ROTATION TRANSACTION HAS ROLLED BACK, ON THE POOL. Both
// halves matter. The rollback has discarded the successor row, so nothing here
// can commit a credential into a family it is about to revoke. And a fresh read
// sees COMMITTED state, which is the only state worth reasoning about: the
// winner of a race has necessarily committed already, because the conditional
// update declined only once that commit landed.
//
// IT IS STILL NOT A SECOND DOOR. The rotation was refused by one statement and
// stays refused; nothing below can turn a refusal into a success. What it adds
// is what the refusal MEANT, which is a different question with a different
// answer for the caller (always the same 401) and for the account (sometimes an
// invalidated family).
//
// THE DISTINGUISHER IS TIME, BECAUSE THE ROW HAS NOTHING ELSE. `replaced_by`
// being set means the token was spent — by a replay, by the loser of a race, or
// by a client retrying after a lost response — and the row cannot separate
// them. What separates them is WHEN: a race is milliseconds, a working device's
// next refresh is fifteen minutes. So the successor's `issued_at` dates the
// rotation and ReuseGraceWindow draws the line. See that constant for the
// exposure this buys and why it is the right way round.
//
// CHAIN DEPTH WAS CONSIDERED AND IS WRONG HERE, recorded so it is not
// rediscovered as an improvement. "Invalidate only if the successor has itself
// been rotated" needs no constant and misses the primary threat: an attacker
// who uses a stolen token FIRST leaves the successor unrotated, so the victim's
// later presentation reads as benign and the attacker keeps the family.
func (s *Store) refusalOutcome(ctx context.Context, tokenID, familyID, deviceID, userID uuid.UUID) {
	// DETACHED FROM THE CALLER'S CONTEXT, AND BOUNDED. Found in review on #64.
	// The context that arrives here is the REQUEST's — router.go passes
	// r.Context() — and Go cancels that the moment the client's connection
	// closes. The refusal has already been decided by the time this runs, so
	// cancellation can no longer affect what the caller is told; what it CAN do
	// is kill the invalidation. A prober could then present a stolen token,
	// hang up, and leave the family live at will — learning the token is spent
	// without ever tripping the detector, which is precisely the state
	// invalidateFamily's comment says must not happen. The same path would fire
	// benignly every time a mobile client timed out on a late retry.
	//
	// WithoutCancel with a timeout over it, which is the shape notify.go
	// already uses for the same reason: the caller's lifetime is the wrong one
	// for work the caller does not own.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refusalOutcomeBudget)
	defer cancel()

	var (
		replacedBy    *uuid.UUID
		tokenRevoked  *time.Time
		expiresAt     time.Time
		deviceRevoked *time.Time
		deactivated   *time.Time
		rotatedAgeSec *float64
	)
	// THE SUCCESSOR'S issued_at IS THE MOMENT THIS TOKEN WAS ROTATED, and there
	// is no `replaced_at` column because there does not need to be. The join is
	// safe by construction: `refresh_tokens.replaced_by` is ON DELETE RESTRICT,
	// so a chain cannot be broken in the middle and the row a replaced token
	// names always exists — schema_test.go calls that constraint "precisely the
	// evidence CANT-29's reuse detection reads".
	//
	// THE AGE IS SUBTRACTED IN SQL, so both sides of it come from the database's
	// clock. Returning the bare timestamp for Go to subtract against its own
	// clock is what made this a cross-host comparison; see ReuseGraceWindow.
	if err := s.pool.QueryRow(ctx, `
		SELECT r.replaced_by, r.revoked_at, r.expires_at,
		       d.revoked_at, u.deactivated_at,
		       EXTRACT(EPOCH FROM (now() - succ.issued_at))::float8
		  FROM refresh_tokens r
		  JOIN devices d ON d.id = r.device_id
		  JOIN users u ON u.id = d.user_id
		  LEFT JOIN refresh_tokens succ ON succ.id = r.replaced_by
		 WHERE r.id = $1`, tokenID).
		Scan(&replacedBy, &tokenRevoked, &expiresAt, &deviceRevoked,
			&deactivated, &rotatedAgeSec); err != nil {
		// The refusal still stands; only its explanation is missing. Logged as
		// such rather than swallowed, because a refusal nobody can account for
		// is the one worth finding in a log.
		s.logger.WarnContext(ctx, "refresh refused",
			"reason", "cause could not be read", "token_id", tokenID, "error", err)
		return
	}

	if replacedBy != nil {
		s.spentTokenOutcome(ctx, tokenID, familyID, deviceID, userID, rotatedAgeSec)
		return
	}

	// EVERY OTHER DOOR IS A REFUSAL AND NOT AN INCIDENT. A revoked, expired or
	// deactivated credential is somebody's account being administered or a
	// device falling out of use; none of them is evidence that a token was
	// copied, and none of them invalidates anything.
	//
	// THE DEFAULT IS EFFECTIVELY UNREACHABLE and is labelled honestly rather
	// than removed: reaching it needs the row visible with every door open,
	// which cannot follow this update declining. If it ever appears in a log,
	// something changed the row in a way nothing in this schema does.
	reason := "refused, and the row names no reason"
	switch {
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

// spentTokenOutcome answers the one question this ticket exists for: was that a
// replay, or an echo of a rotation that already worked?
func (s *Store) spentTokenOutcome(ctx context.Context, tokenID, familyID, deviceID, userID uuid.UUID, rotatedAgeSec *float64) {
	// UNDATEABLE MEANS TREAT IT AS A REPLAY, and the bias is deliberate. The
	// RESTRICT on replaced_by makes this unreachable, so arriving here means the
	// schema's own guarantee has failed — and the safe reading of "a spent token
	// was presented and I cannot tell you when it was spent" is the incident,
	// not the echo. Logged as its own reason so it is never mistaken for an
	// ordinary detection.
	if rotatedAgeSec == nil {
		s.logger.ErrorContext(ctx, "refresh replay suspected; the rotation could not be dated",
			"token_id", tokenID, "family_id", familyID, "device_id", deviceID, "user_id", userID)
		s.invalidateFamily(ctx, familyID, deviceID, userID, "rotation could not be dated")
		return
	}

	if age := time.Duration(*rotatedAgeSec * float64(time.Second)); age < ReuseGraceWindow {
		// THE ECHO. The rotation this token lost to happened moments ago, so the
		// client that is holding the winner's pair and the client that sent this
		// are overwhelmingly the same client. Refused — single use still holds,
		// and the caller gets the same 401 as everything else — but nothing is
		// invalidated and no incident is reported.
		s.logger.WarnContext(ctx, "refresh refused", "reason", "token already rotated, within the reuse grace window",
			"token_id", tokenID, "family_id", familyID, "device_id", deviceID, "user_id", userID,
			"rotated_age_ms", age.Milliseconds(), "grace_window_ms", ReuseGraceWindow.Milliseconds())
		return
	}

	s.invalidateFamily(ctx, familyID, deviceID, userID, "a spent refresh token was presented after the grace window")
}

// invalidateFamily revokes a whole refresh chain, everything that chain
// currently buys, and the device behind it — in one transaction, publishing the
// revocation that severs the live socket.
//
// ALL FOUR WRITES OR NONE, AND THE NOTIFY INSIDE THEM. This is CANT-18's ruling
// 2 applied a third time, on RevokeDevice's own argument: Postgres delivers a
// NOTIFY at commit, so a notification cannot exist without its cause or the
// reverse. A partially-applied invalidation is the worst possible state — a
// family revoked while the device keeps its access token, or a device revoked
// while nobody is told to sever its socket.
//
// WHY THE ACCESS TOKENS AND THE DEVICE, AND NOT JUST THE FAMILY. The `Done
// when` says the legitimate device is "forced to re-authenticate rather than
// silently continuing", and a family-only revocation does not deliver that: the
// device's current access token keeps working for the rest of its fifteen
// minutes, and under CANT-28 ruling 2 its live socket outlives the token
// entirely. Revoking the access tokens closes the request path; setting
// devices.revoked_at is what makes every other mechanism agree — Authenticate
// refuses it, DeadDevices names it, and CANT-30's gap re-check reaches the same
// verdict as the notification does.
//
// A FAMILY IS ONE DEVICE'S, which is what makes the device write correct rather
// than collateral: a family begins at one enrollment and every rotation carries
// the same device_id forward, so "this family" and "this device's credentials"
// are the same set.
//
// THE DEVICE WRITE IS ALSO NOT REDUNDANT, AND MUST NOT BE REMOVED AS SUCH. The
// family UPDATE takes its snapshot at statement start; if it blocks on a row
// lock held by a concurrent legitimate rotation, Postgres re-checks only the
// locked row on unblock rather than re-scanning, so a successor that
// transaction committed in the meantime is invisible to it and survives
// unrevoked. Nothing exploitable follows — `devices.revoked_at` is set in this
// same transaction, and RotateRefresh's `d.revoked_at IS NULL` door refuses
// that survivor — but "the whole family" is guaranteed by the device write
// rather than by the family predicate alone, and the tests' "no unrevoked rows"
// property holds in the sequential case.
//
// IDEMPOTENT ON R6'S TERMS, AND THE GUARDS ALONE DO NOT BUY IT. The `IS NULL`
// predicates make the three WRITES idempotent; they say nothing about the
// NOTIFY, and a second replay against an already-invalidated family would
// otherwise publish a revocation for a device that was severed the first time.
// So the device write doubles as the gate, exactly as RevokeDevice's does: it
// RETURNS the row it changed, and no transition means no publish.
//
// THE LOG KEYS ON SOMETHING WIDER THAN THE NOTIFY, deliberately. A device can
// already be revoked — by an earlier replay, or by an administrator revoking a
// lost phone — while its refresh family is still live, and revoking that family
// IS an invalidation worth recording even though its sockets were severed long
// ago. So the incident line fires when anything changed, and a replay that
// changed nothing still gets a line of its own rather than silence: somebody
// presenting a stolen token repeatedly is worth seeing.
//
// THE ERROR IS LOGGED AND NOT RETURNED. Its caller is a refusal path that has
// already decided the answer is 401, and there is no outcome it could change.
// What must not happen is silence: an invalidation that failed leaves an
// attacker holding a live family, which is the exact condition this ticket
// exists to end.
func (s *Store) invalidateFamily(ctx context.Context, familyID, deviceID, userID uuid.UUID, why string) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "family invalidation failed", "stage", "begin",
			"family_id", familyID, "device_id", deviceID, "user_id", userID, "error", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	fam, err := tx.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		 WHERE family_id = $1 AND revoked_at IS NULL`, familyID)
	if err != nil {
		s.logger.ErrorContext(ctx, "family invalidation failed", "stage", "refresh_tokens",
			"family_id", familyID, "device_id", deviceID, "user_id", userID, "error", err)
		return
	}
	acc, err := tx.Exec(ctx, `
		UPDATE access_tokens SET revoked_at = now()
		 WHERE device_id = $1 AND revoked_at IS NULL`, deviceID)
	if err != nil {
		s.logger.ErrorContext(ctx, "family invalidation failed", "stage", "access_tokens",
			"family_id", familyID, "device_id", deviceID, "user_id", userID, "error", err)
		return
	}
	// THE DEVICE WRITE IS ALSO THE IDEMPOTENCE GATE — see the doc comment. It
	// returns the row it changed so that "already revoked" is a fact this
	// function holds rather than one it assumes from the other two counts.
	var severed bool
	var changedDevice uuid.UUID
	err = tx.QueryRow(ctx, `
		UPDATE devices SET revoked_at = now()
		 WHERE id = $1 AND revoked_at IS NULL
		 RETURNING id`, deviceID).Scan(&changedDevice)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Already revoked, so CANT-30 severed whatever sockets it had the first
		// time and there is nobody left to tell.
		severed = false
	case err != nil:
		s.logger.ErrorContext(ctx, "family invalidation failed", "stage", "devices",
			"family_id", familyID, "device_id", deviceID, "user_id", userID, "error", err)
		return
	default:
		severed = true
	}

	if severed {
		payload, err := RevocationPayload{DeviceID: &deviceID}.Encode()
		if err != nil {
			s.logger.ErrorContext(ctx, "family invalidation failed", "stage", "payload",
				"family_id", familyID, "device_id", deviceID, "user_id", userID, "error", err)
			return
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, RevocationChannel, payload); err != nil {
			s.logger.ErrorContext(ctx, "family invalidation failed", "stage", "notify",
				"family_id", familyID, "device_id", deviceID, "user_id", userID, "error", err)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		s.logger.ErrorContext(ctx, "family invalidation failed", "stage", "commit",
			"family_id", familyID, "device_id", deviceID, "user_id", userID, "error", err)
		return
	}

	if fam.RowsAffected() == 0 && acc.RowsAffected() == 0 && !severed {
		// NOTHING LEFT TO INVALIDATE, AND NOT SILENT. A repeat replay against a
		// family that is already gone is not a fresh incident — nothing changed
		// and nothing was severed — but it is somebody presenting a stolen
		// token again, which is worth a line.
		s.logger.WarnContext(ctx, "refresh replay against an already-invalidated family",
			"why", why, "family_id", familyID, "device_id", deviceID, "user_id", userID)
		return
	}

	// THE EVENT, RECORDED. At ERROR rather than WARN, and deliberately: every
	// other refusal in this file is somebody's credential being ordinary, and
	// this one is the only line in the service that says a token was copied.
	// Ids and counts, never the token — the same rule every credential log here
	// follows.
	s.logger.ErrorContext(ctx, "refresh token replayed; family invalidated",
		"why", why, "family_id", familyID, "device_id", deviceID, "user_id", userID,
		"refresh_tokens_revoked", fam.RowsAffected(), "access_tokens_revoked", acc.RowsAffected(),
		"socket_severed", severed, "grace_window_ms", ReuseGraceWindow.Milliseconds())
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
	return insertRefreshProposed(ctx, tx, id, deviceID, family, now, "")
}

// insertRefreshProposed is insertRefresh storing the hash of a CLIENT-PROPOSED
// token instead of one minted here; an empty proposal mints. One INSERT for
// both, for the reason insertRefresh gives for sharing one between its callers.
// The error is wrapped, so isTokenHashCollision can still find the PgError.
func insertRefreshProposed(ctx context.Context, tx pgx.Tx, id, deviceID, family uuid.UUID, now time.Time, proposed string) (IssuedToken, error) {
	plaintext, hash := proposed, HashToken(proposed)
	if proposed == "" {
		var err error
		if plaintext, hash, err = MintToken(); err != nil {
			return IssuedToken{}, err
		}
	}
	expires := now.Add(RefreshTokenLifetime)
	if _, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (id, device_id, token_hash, family_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, id, deviceID, hash, family, expires); err != nil {
		return IssuedToken{}, fmt.Errorf("store: issue refresh token: %w", err)
	}
	return IssuedToken{Plaintext: plaintext, ExpiresAt: expires}, nil
}

// collisionFault is a deliberately broken routeCollision, for tests that have
// to watch the right behaviour's absence fail (plan criterion 26).
type collisionFault int

const (
	faultCollisionNone collisionFault = iota
	// faultCollisionNoLiveBranch removes branch 1: a collision while the
	// presented token is still live falls through to the refusal path.
	faultCollisionNoLiveBranch
	// faultCollisionUnrouted removes routing altogether: the unique violation
	// is returned as the error it is, which the handler answers 500.
	faultCollisionUnrouted
)

// isTokenHashCollision reports whether err is refresh_tokens' UNIQUE on
// token_hash refusing an insert. BY CONSTRAINT NAME as well as by SQLSTATE: a
// 23505 on the primary key would be a uuid collision, which is not a proposal
// colliding and must not be routed as one.
func isTokenHashCollision(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == refreshTokenHashConstraint
}

// refreshTokenHashConstraint is the name Postgres gave 0007's inline
// `token_hash BYTEA NOT NULL UNIQUE`. proposal_test.go pins it against
// pg_constraint.
const refreshTokenHashConstraint = "refresh_tokens_token_hash_key"
