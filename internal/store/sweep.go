package store

// CANT-118 — the sweep for spent access_tokens and refresh_tokens rows, so the
// two credential tables CANT-97's rotation writes into stop growing without
// bound (~96 rows/device/day, and nothing deleted either one before this
// ticket). See SpentCredentialRetention in tokens.go for the window and the
// exposure it trades against that growth.
//
// ROW BY ROW, OLDEST issued_at FIRST, AND ONLY ROWS THAT ARE ALREADY DEAD.
// "Dead" is expired or revoked — never merely superseded, which is a fact
// about `replaced_by` alone and can be true years before a row's own window
// passes. That distinction is the whole reason this is a retention window
// rather than a delete-on-rotate: a freshly superseded refresh token is
// exactly the row CANT-29's reuse detection reads.
//
// OLDEST-FIRST IS WHAT KEEPS refresh_tokens.replaced_by FROM EVER DANGLING,
// AND THE ARGUMENT NEEDS BOTH ITS HALVES STATED. A predecessor's row was
// written by an earlier, already-committed transaction — issued_at defaults
// to the inserting transaction's own now() — so it is always at least as OLD
// as the successor its replaced_by names. Age alone is not eligibility,
// though: eligibility is age AND deadness, and the second half rests on a
// premise this file does not itself enforce — every refresh row takes
// expires_at = issued_at + RefreshTokenLifetime from ONE constant
// (insertRefreshProposed, refresh.go), which makes expiry monotone in
// issued_at, and family invalidation revokes WHERE family_id = $1
// (refresh.go), which can never leave a live ancestor pointing at a revoked
// descendant. Together those two make a predecessor at least as eligible as
// whatever it points to, whenever that row is eligible at all.
//
// THAT UNIFORM-LIFETIME PREMISE IS LOAD-BEARING, AND A LATER CHANGE CAN BREAK
// IT: shorten RefreshTokenLifetime and a predecessor issued under the old,
// longer lifetime can outlive a successor issued under the new, shorter one.
// This ticket's own tests all run under a single lifetime, by construction,
// so none of them would catch that — whoever changes RefreshTokenLifetime has
// to re-check this argument rather than trust it.
//
// THAT LAST CLAIM IS PROVEN AGAINST REAL POSTGRES IN sweep_test.go RATHER THAN
// ASSUMED. A single DELETE naming BOTH ends of a dead chain link at once does
// NOT trip the ON DELETE RESTRICT on replaced_by, because Postgres checks a
// NOT DEFERRABLE foreign key once the whole triggering statement's own row
// changes have already happened — by which point neither row is there to be a
// violation. What DOES trip it, and is watched failing in the same file, is
// deleting the referenced end alone while its predecessor survives — which is
// the shape "a sweep that would break a chain" takes, and which this
// predicate cannot produce: nothing here ever deletes a successor without its
// predecessor being at least as eligible in the same or an earlier pass.
//
// NOT CANT-67. That ticket's message-retention sweep does not exist yet, and
// nothing here touches messages, enrollment_tokens or devices.

import (
	"context"
	"fmt"
	"time"
)

// sweepBatchSize bounds one DELETE within a single sweep pass, so removing a
// large backlog never holds a lock over either credential table for longer
// than a moment — the same concern rotateBudget answers for a single
// rotation, applied to a statement that could otherwise touch every row a
// service has ever issued.
const sweepBatchSize = 1000

// SweepInterval is how often the credential sweep loop runs.
//
// A CONSTANT, on SpentCredentialRetention's own terms: this is a posture, not
// a dial an operator should be able to turn without re-reading the argument
// for it. An hour keeps a row from lingering long past its window, and is
// infrequent enough that a pass is background noise on a deployment this
// size.
const SweepInterval = time.Hour

// sweepAccessTokensOnce removes up to sweepBatchSize eligible rows from
// access_tokens: issued before SpentCredentialRetention ago, and already
// dead.
//
// A BOT'S NON-EXPIRING TOKEN CANNOT SATISFY "DEAD" BY EXPIRY, HOWEVER OLD IT
// IS. 0007 ties a NULL expires_at to a NULL device_id, and IssueBotToken
// (bots.go) is the only path that produces one; the predicate below reads
// that NULL as "cannot be expired" rather than as a bound that vacuously
// passes, so a live long-lived bot credential is never eligible no matter how
// old issued_at gets — only a REVOKED bot token is, and only once it is old
// enough too. See TestALiveBotTokenSurvivesTheSweepHoweverOldButARevokedOneDoesNot.
func (s *Store) sweepAccessTokensOnce(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM access_tokens
		 WHERE id IN (
			SELECT id FROM access_tokens
			 WHERE issued_at < now() - ($1 * interval '1 second')
			   AND (revoked_at IS NOT NULL OR (expires_at IS NOT NULL AND expires_at <= now()))
			 ORDER BY issued_at, id
			 LIMIT $2
		 )`, SpentCredentialRetention.Seconds(), sweepBatchSize)
	if err != nil {
		return 0, fmt.Errorf("store: sweep access tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

// sweepRefreshTokensOnce is sweepAccessTokensOnce's sibling for
// refresh_tokens, where expires_at is never NULL — 0007 declares it NOT NULL
// for every row, including a live family's head — so "dead" needs no NULL
// guard the way access_tokens' does.
func (s *Store) sweepRefreshTokensOnce(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM refresh_tokens
		 WHERE id IN (
			SELECT id FROM refresh_tokens
			 WHERE issued_at < now() - ($1 * interval '1 second')
			   AND (revoked_at IS NOT NULL OR expires_at <= now())
			 ORDER BY issued_at, id
			 LIMIT $2
		 )`, SpentCredentialRetention.Seconds(), sweepBatchSize)
	if err != nil {
		return 0, fmt.Errorf("store: sweep refresh tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SweepCredentials removes every access_tokens and refresh_tokens row whose
// SpentCredentialRetention window has passed, one sweepBatchSize DELETE at a
// time per table, looping until each table is drained or ctx ends. It
// returns how many rows it removed from each, for RunCredentialSweep's one
// log line per pass.
//
// A RETURN OF EXACTLY sweepBatchSize DOES NOT MEAN "DONE"; ANYTHING LESS
// DOES. That is the loop condition below: a full batch says there may be
// more, so it asks again, and a partial or empty one says this table's
// currently-eligible rows are gone.
func (s *Store) SweepCredentials(ctx context.Context) (accessDeleted, refreshDeleted int64, err error) {
	for {
		if e := ctx.Err(); e != nil {
			return accessDeleted, refreshDeleted, e
		}
		n, e := s.sweepAccessTokensOnce(ctx)
		if e != nil {
			return accessDeleted, refreshDeleted, e
		}
		accessDeleted += n
		if n < sweepBatchSize {
			break
		}
	}
	for {
		if e := ctx.Err(); e != nil {
			return accessDeleted, refreshDeleted, e
		}
		n, e := s.sweepRefreshTokensOnce(ctx)
		if e != nil {
			return accessDeleted, refreshDeleted, e
		}
		refreshDeleted += n
		if n < sweepBatchSize {
			break
		}
	}
	return accessDeleted, refreshDeleted, nil
}

// RunCredentialSweep runs SweepCredentials every SweepInterval until ctx is
// done. cmd/catenary's serve starts this the same way it starts the two
// notify Listener loops in internal/store/notify.go, and stops it the same
// way — by cancelling ctx.
//
// WRITTEN SO CANT-67 CAN ADOPT IT RATHER THAN DUPLICATE IT. That ticket's
// message-retention sweep does not exist yet; when it does, this loop's shape
// — tick, sweep, log only if something moved — is what it should share
// rather than a second ticker running the same shape of work against a
// different pair of tables. Nothing here reaches beyond access_tokens and
// refresh_tokens, and CANT-67 itself is not built by this ticket.
//
// SILENT ON A PASS THAT REMOVED NOTHING. On a lightly loaded deployment that
// is the overwhelming majority of passes, and a line for each one, forever,
// is exactly the log noise that trains a reader to stop reading this channel.
func (s *Store) RunCredentialSweep(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		access, refresh, err := s.SweepCredentials(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Stopped mid-pass by the same cancellation that is about to
				// end this loop on the next iteration; not a failure worth a
				// line of its own.
				return
			}
			s.logger.ErrorContext(ctx, "credential sweep failed", "error", err)
			continue
		}
		if access != 0 || refresh != 0 {
			s.logger.InfoContext(ctx, "credential sweep",
				"access_tokens_removed", access, "refresh_tokens_removed", refresh)
		}
	}
}
