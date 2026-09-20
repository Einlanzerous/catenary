package store

// CANT-118 — the sweep's oracle.
//
// THE SAME PAIRED SHAPE reuse_test.go and proposal_test.go USE: what must be
// removed, and what must survive — plus, since this ticket's whole risk is a
// deletion that could go wrong rather than one that could fail to happen, the
// two RESTRICT tests at the bottom that prove the ordering argument in
// sweep.go against real Postgres instead of assuming it.
//
// ROWS ARE BUILT DIRECTLY, NOT THROUGH RotateRefresh/backdateRotation, where a
// test needs to control issued_at and expires_at independently — a real
// rotation always sets expires_at = issued_at + a fixed lifetime, and several
// of the cases below (a token revoked before its own natural expiry; a token
// old enough to sweep but not yet expired) do not arise that way on purpose.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mkAccessTokenRow inserts one access_tokens row with the three columns the
// sweep's predicate reads set directly. device is nil for a bot's token
// (NULL device_id), which 0007's CHECK ties to a nil expiresAt.
func mkAccessTokenRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	user uuid.UUID, device *uuid.UUID, issuedAt time.Time, expiresAt *time.Time, revoked bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, hash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	var revokedAt *time.Time
	if revoked {
		r := time.Now()
		revokedAt = &r
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO access_tokens (id, token_hash, user_id, device_id, issued_at, expires_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, hash, user, device, issuedAt, expiresAt, revokedAt); err != nil {
		t.Fatalf("mkAccessTokenRow: %v", err)
	}
	return id
}

// bulkOldDeadAccessTokens inserts n access_tokens rows in one statement, all
// issued age ago and already expired — for the batch-bound test, where
// inserting thousands of rows one at a time would make the test itself the
// slow part.
func bulkOldDeadAccessTokens(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user, device uuid.UUID, age time.Duration, n int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO access_tokens (id, token_hash, user_id, device_id, issued_at, expires_at)
		SELECT gen_random_uuid(), decode(md5(random()::text || g::text), 'hex'), $1, $2,
		       now() - ($3 * interval '1 second'), now() - interval '1 hour'
		  FROM generate_series(1, $4) AS g`,
		user, device, age.Seconds(), n); err != nil {
		t.Fatalf("bulkOldDeadAccessTokens: %v", err)
	}
}

// mkRefreshTokenRow inserts one refresh_tokens row with issued_at, expires_at
// and revoked_at set directly, and no replaced_by — linkReplacement adds that
// once both ends of a link exist.
func mkRefreshTokenRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	device, family uuid.UUID, issuedAt, expiresAt time.Time, revoked bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, hash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	var revokedAt *time.Time
	if revoked {
		r := time.Now()
		revokedAt = &r
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO refresh_tokens (id, device_id, token_hash, family_id, issued_at, expires_at, revoked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, device, hash, family, issuedAt, expiresAt, revokedAt); err != nil {
		t.Fatalf("mkRefreshTokenRow: %v", err)
	}
	return id
}

func linkReplacement(ctx context.Context, t *testing.T, pool *pgxpool.Pool, predecessor, successor uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE refresh_tokens SET replaced_by = $2 WHERE id = $1`, predecessor, successor); err != nil {
		t.Fatalf("linkReplacement: %v", err)
	}
}

func accessTokenExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_tokens WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func refreshTokenExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM refresh_tokens WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// danglingReplacedByCount is the query the ticket asks the family test to
// assert with: every row whose replaced_by names something that no longer
// exists. It should always be zero — that is what the FK is for — but the
// point of this ticket is that a sweep bug here would corrupt rather than
// fail loudly, so it is checked rather than trusted.
func danglingReplacedByCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return countRows(ctx, t, pool, `
		SELECT count(*) FROM refresh_tokens r
		 WHERE r.replaced_by IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM refresh_tokens s WHERE s.id = r.replaced_by)`)
}

// backdateRootIssuance moves a family's ROOT token — the one row whose id
// equals its own family_id — issued_at and expires_at back by the same
// amount, preserving the real 60-day gap RedeemEnrollment set between them.
// That is what actually happens to a real spent token given enough elapsed
// time, rather than a synthetic revocation: shifting both by more than
// SpentCredentialRetention lands expires_at in the past too, which is what
// makes the row eligible for the sweep.
func backdateRootIssuance(ctx context.Context, t *testing.T, pool *pgxpool.Pool, device uuid.UUID, age time.Duration) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
		UPDATE refresh_tokens
		   SET issued_at  = issued_at  - make_interval(secs => $2),
		       expires_at = expires_at - make_interval(secs => $2)
		 WHERE device_id = $1 AND id = family_id`, device, age.Seconds())
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatal("precondition: no root row to backdate")
	}
}

// ---------------------------------------------------------------------------
// What the sweep removes, and what it spares

func TestSweepRemovesOldDeadAccessTokensButSparesYoungAndLiveOnes(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")
	device := mkDevice(ctx, t, pool, user, "Pixel")

	past := SpentCredentialRetention + 24*time.Hour
	recent := time.Hour

	oldExpired := mkAccessTokenRow(ctx, t, pool, user, &device,
		time.Now().Add(-past), ptr(time.Now().Add(-time.Hour)), false)
	oldRevokedNotYetExpired := mkAccessTokenRow(ctx, t, pool, user, &device,
		time.Now().Add(-past), ptr(time.Now().Add(time.Hour)), true)
	youngExpired := mkAccessTokenRow(ctx, t, pool, user, &device,
		time.Now().Add(-recent), ptr(time.Now().Add(-time.Minute)), false)
	live := mkAccessTokenRow(ctx, t, pool, user, &device,
		time.Now(), ptr(time.Now().Add(AccessTokenLifetime)), false)

	access, refresh, err := st.SweepCredentials(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if access != 2 || refresh != 0 {
		t.Fatalf("swept %d access, %d refresh rows; want 2 and 0", access, refresh)
	}
	for id, want := range map[uuid.UUID]bool{
		oldExpired:              false,
		oldRevokedNotYetExpired: false,
		youngExpired:            true,
		live:                    true,
	} {
		if got := accessTokenExists(ctx, t, pool, id); got != want {
			t.Errorf("access token %s exists = %v, want %v", id, got, want)
		}
	}
}

func TestALiveBotTokenSurvivesTheSweepHoweverOldButARevokedOneDoesNot(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot := mkUser(ctx, t, pool, "argosy")
	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'bot' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}

	// Ten years: "however old" is not a figure of speech. A NULL expires_at
	// can never satisfy "expired", so age alone must never be enough.
	ancient := 10 * 365 * 24 * time.Hour
	live := mkAccessTokenRow(ctx, t, pool, bot, nil, time.Now().Add(-ancient), nil, false)
	revoked := mkAccessTokenRow(ctx, t, pool, bot, nil, time.Now().Add(-ancient), nil, true)

	access, refresh, err := st.SweepCredentials(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if access != 1 || refresh != 0 {
		t.Fatalf("swept %d access, %d refresh rows; want 1 and 0", access, refresh)
	}
	if !accessTokenExists(ctx, t, pool, live) {
		t.Error("a live bot token was swept; a non-expiring credential must never be removed while " +
			"it is still live, however old")
	}
	if accessTokenExists(ctx, t, pool, revoked) {
		t.Error("a revoked bot token, old enough to be past the window, survived the sweep")
	}
}

func TestSweepRemovesOldDeadRefreshTokensButSparesYoungAndLiveOnes(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")
	device := mkDevice(ctx, t, pool, user, "Pixel")
	family := uuid.New()

	past := SpentCredentialRetention + 24*time.Hour
	recent := time.Hour

	oldExpired := mkRefreshTokenRow(ctx, t, pool, device, family,
		time.Now().Add(-past), time.Now().Add(-time.Hour), false)
	oldRevokedNotYetExpired := mkRefreshTokenRow(ctx, t, pool, device, family,
		time.Now().Add(-past), time.Now().Add(time.Hour), true)
	youngExpired := mkRefreshTokenRow(ctx, t, pool, device, family,
		time.Now().Add(-recent), time.Now().Add(-time.Minute), false)
	liveHead := mkRefreshTokenRow(ctx, t, pool, device, family,
		time.Now(), time.Now().Add(RefreshTokenLifetime), false)

	access, refresh, err := st.SweepCredentials(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if access != 0 || refresh != 2 {
		t.Fatalf("swept %d access, %d refresh rows; want 0 and 2", access, refresh)
	}
	for id, want := range map[uuid.UUID]bool{
		oldExpired:              false,
		oldRevokedNotYetExpired: false,
		youngExpired:            true,
		liveHead:                true,
	} {
		if got := refreshTokenExists(ctx, t, pool, id); got != want {
			t.Errorf("refresh token %s exists = %v, want %v", id, got, want)
		}
	}
}

// A family with a live head and hundreds of old spent rows loses the old
// rows, the chain that remains is intact, and no replaced_by dangles.
func TestSweepingAFamilyWithHundredsOfOldSpentRowsLeavesTheLiveChainIntact(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	var head, family uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id, family_id FROM refresh_tokens WHERE device_id = $1 AND id = family_id`,
		e.DeviceID).Scan(&head, &family); err != nil {
		t.Fatal(err)
	}

	const ancestors = 300
	past := SpentCredentialRetention + 24*time.Hour
	ids := make([]uuid.UUID, ancestors)
	for i := range ancestors {
		// Strictly increasing issued_at with i: ids[0] is the oldest, ids[len-1]
		// the youngest ancestor — still well past the window — so linking
		// ids[i] -> ids[i+1] -> ... -> head always points from older to newer,
		// which is the ordering RotateRefresh itself produces.
		age := past + time.Duration(ancestors-i)*time.Minute
		ids[i] = mkRefreshTokenRow(ctx, t, pool, e.DeviceID, family,
			time.Now().Add(-age), time.Now().Add(-age).Add(RefreshTokenLifetime), false)
	}
	for i := range ancestors - 1 {
		linkReplacement(ctx, t, pool, ids[i], ids[i+1])
	}
	linkReplacement(ctx, t, pool, ids[ancestors-1], head)

	access, refresh, err := st.SweepCredentials(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if access != 0 || refresh != ancestors {
		t.Fatalf("swept %d access, %d refresh rows; want 0 and %d", access, refresh, ancestors)
	}

	if n := countRows(ctx, t, pool, `SELECT count(*) FROM refresh_tokens WHERE device_id = $1`, e.DeviceID); n != 1 {
		t.Errorf("%d refresh rows survive, want 1 — only the live head", n)
	}
	if !refreshTokenExists(ctx, t, pool, head) {
		t.Fatal("the live head itself was swept")
	}
	var headReplacedBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT replaced_by FROM refresh_tokens WHERE id = $1`, head).Scan(&headReplacedBy); err != nil {
		t.Fatal(err)
	}
	if headReplacedBy != nil {
		t.Error("the live head now has a replaced_by; it should still be the unrotated family root")
	}
	if n := danglingReplacedByCount(ctx, t, pool); n != 0 {
		t.Errorf("%d rows reference a replaced_by that no longer exists", n)
	}
}

// ---------------------------------------------------------------------------
// CANT-29's reuse detection, and CANT-125's routing, against a swept database

// The three claims the ticket's `Done when` makes about detection surviving a
// sweep, together because they share one family and one sweep call: (a) a
// spent token still inside its own retention window, presented outside the
// grace window, still invalidates the family; (b) the live head still
// rotates; (c) a token whose window HAS passed and was actually swept is
// refused 401 and invalidates nothing — the stated exposure, proven.
func TestReuseDetectionAgainstASweptDatabase(t *testing.T) {
	t.Run("a spent token still inside its window invalidates the family after an intervening sweep", func(t *testing.T) {
		ctx, pool := freshDB(t)
		st := New(pool, DefaultLimits(), discardLogger())
		e := enrolled(ctx, t, st, pool, "ada")

		next, err := st.RotateRefresh(ctx, e.Refresh.Plaintext)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		// Outside the ten-second grace window; nowhere near the 67-day
		// retention window — the root token this test replays must still be
		// there, and the sweep below must agree that it should be.
		backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)

		if _, _, err := st.SweepCredentials(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}

		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("replay after an intervening sweep = %v, want ErrUnauthorized", err)
		}
		after := readFamilyState(ctx, t, pool, e.DeviceID)
		if !after.deviceRevoked || after.refreshRevoked != after.refreshRows || after.accessRevoked != after.accessRows {
			t.Errorf("a sweep between the rotation and the replay defeated detection: %+v", after)
		}
		if _, err := st.Authenticate(ctx, next.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("the rotated access token still authenticates after invalidation: %v", err)
		}
	})

	t.Run("the live head still rotates after an intervening sweep that had nothing to do", func(t *testing.T) {
		ctx, pool := freshDB(t)
		st := New(pool, DefaultLimits(), discardLogger())
		e := enrolled(ctx, t, st, pool, "ada")
		first, err := st.RotateRefresh(ctx, e.Refresh.Plaintext)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		if _, _, err := st.SweepCredentials(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if _, err := st.RotateRefresh(ctx, first.Refresh.Plaintext); err != nil {
			t.Errorf("the live head did not rotate after a sweep: %v", err)
		}
	})

	t.Run("a swept token is refused 401 and invalidates nothing — the stated exposure", func(t *testing.T) {
		ctx, pool := freshDB(t)
		logger, buf := captureLogger()
		st := New(pool, DefaultLimits(), logger)
		e := enrolled(ctx, t, st, pool, "ada")

		var rootID uuid.UUID
		if err := pool.QueryRow(ctx, `
			SELECT id FROM refresh_tokens WHERE device_id = $1 AND id = family_id`,
			e.DeviceID).Scan(&rootID); err != nil {
			t.Fatal(err)
		}

		next, err := st.RotateRefresh(ctx, e.Refresh.Plaintext)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		// Past BOTH windows: the root's own issued_at/expires_at are shifted
		// back far enough that its window has passed, so the sweep below
		// actually removes it rather than merely being old enough to look
		// like a replay.
		backdateRootIssuance(ctx, t, pool, e.DeviceID, SpentCredentialRetention+time.Hour)

		_, refreshDeleted, err := st.SweepCredentials(ctx)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if refreshDeleted == 0 || refreshTokenExists(ctx, t, pool, rootID) {
			t.Fatal("precondition: the sweep did not remove the presented token's row; this test proves nothing")
		}
		mark := len(logLines(t, buf))

		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("presenting a swept token = %v, want ErrUnauthorized", err)
		}

		// THE EXPOSURE, PROVEN: nothing was invalidated. The family the swept
		// token belonged to is still live.
		after := readFamilyState(ctx, t, pool, e.DeviceID)
		if after.deviceRevoked || after.refreshRevoked != 0 || after.accessRevoked != 0 {
			t.Errorf("a swept, unrecognisable token still invalidated something: %+v", after)
		}
		if _, err := st.Authenticate(ctx, next.Access.Plaintext); err != nil {
			t.Errorf("the legitimate device's rotated access token stopped working: %v", err)
		}
		if _, err := st.RotateRefresh(ctx, next.Refresh.Plaintext); err != nil {
			t.Errorf("the family's live head no longer rotates: %v", err)
		}
		for _, l := range logLines(t, buf)[mark:] {
			if l["msg"] == "refresh token replayed; family invalidated" {
				t.Error("a swept token's presentation was reported as a detected replay; " +
					"it must read as an ordinary unknown credential, which is exactly the exposure")
			}
		}
	})
}

// CANT-125's in-flight-retry branch (routeCollision branch 2) still answers
// present_proposal after a sweep that ran in between and found nothing of
// this family old enough to touch.
func TestInFlightRetryBranchStillAnswersPresentProposalAfterASweep(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")
	p := proposal(t)
	if _, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p); err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.SweepCredentials(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	_, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p)
	if !errors.Is(err, ErrRefreshRetryPresentProposal) {
		t.Fatalf("the in-flight retry after a sweep = %v, want ErrRefreshRetryPresentProposal", err)
	}
	nothingRevoked(ctx, t, readFamilyState(ctx, t, pool, e.DeviceID), "the in-flight retry after a sweep")
}

// ---------------------------------------------------------------------------
// The batch bound, and the loop

func TestSweepDeletesInBoundedBatches(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")
	device := mkDevice(ctx, t, pool, user, "Pixel")

	past := SpentCredentialRetention + 24*time.Hour
	const total = sweepBatchSize*2 + 137
	bulkOldDeadAccessTokens(ctx, t, pool, user, device, past, total)

	// One low-level call is bounded, which is what makes the loop necessary.
	first, err := st.sweepAccessTokensOnce(ctx)
	if err != nil {
		t.Fatalf("sweepAccessTokensOnce: %v", err)
	}
	if first != sweepBatchSize {
		t.Fatalf("one batch removed %d rows, want exactly %d", first, sweepBatchSize)
	}

	// SweepCredentials drains the rest, in further batches of its own.
	access, refresh, err := st.SweepCredentials(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if refresh != 0 {
		t.Errorf("swept %d refresh rows, want 0", refresh)
	}
	if access != int64(total-sweepBatchSize) {
		t.Errorf("the loop removed %d more rows, want %d", access, total-sweepBatchSize)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM access_tokens WHERE user_id = $1`, user); n != 0 {
		t.Errorf("%d access token rows survive, want 0 — every eligible row across every batch", n)
	}
}

func TestCredentialSweepLoopStopsWhenItsContextIsCancelled(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		st.RunCredentialSweep(loopCtx)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCredentialSweep did not return once its context was cancelled")
	}
}

// ---------------------------------------------------------------------------
// The RESTRICT proof (plan decision 4): what a single DELETE naming both ends
// of a dead chain link does, and what deleting only the referenced end alone
// does — proven against real Postgres, not assumed.

func TestSweepingBothEndsOfADeadChainLinkInOneBatchSucceeds(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")
	device := mkDevice(ctx, t, pool, user, "Pixel")
	family := uuid.New()

	past := SpentCredentialRetention + 24*time.Hour
	predecessor := mkRefreshTokenRow(ctx, t, pool, device, family,
		time.Now().Add(-past-time.Minute), time.Now().Add(-past-time.Minute).Add(RefreshTokenLifetime), false)
	successor := mkRefreshTokenRow(ctx, t, pool, device, family,
		time.Now().Add(-past), time.Now().Add(-past).Add(RefreshTokenLifetime), false)
	linkReplacement(ctx, t, pool, predecessor, successor)

	// BOTH rows are dead and old enough, so one sweep pass names both ends of
	// this link in the same DELETE. This is the case sweep.go's own comment
	// argues works: a NOT DEFERRABLE foreign key is checked once the whole
	// statement's own row changes have already happened, so by the time the
	// RESTRICT trigger runs, neither row is there to be a violation.
	n, err := st.sweepRefreshTokensOnce(ctx)
	if err != nil {
		t.Fatalf("sweeping a dead chain link in one batch: %v", err)
	}
	if n != 2 {
		t.Fatalf("removed %d rows, want 2", n)
	}
	if refreshTokenExists(ctx, t, pool, predecessor) || refreshTokenExists(ctx, t, pool, successor) {
		t.Error("one end of the link survived the batch that named both")
	}
}

// The negative control: the one shape "a sweep that would break a chain" can
// take, and Postgres's own backstop against it. This is not SweepCredentials'
// own query — oldest-first ordering never produces this shape, which is the
// whole argument sweep.go makes — it is the schema-level guarantee that
// argument relies on, exercised directly so it is proven rather than assumed.
func TestDeletingOnlyAReferencedSuccessorWhileItsPredecessorSurvivesFailsLoudly(t *testing.T) {
	ctx, pool := freshDB(t)
	user := mkUser(ctx, t, pool, "ada")
	device := mkDevice(ctx, t, pool, user, "Pixel")
	family := uuid.New()

	past := SpentCredentialRetention + 24*time.Hour
	predecessor := mkRefreshTokenRow(ctx, t, pool, device, family,
		time.Now().Add(-past-time.Minute), time.Now().Add(-past-time.Minute).Add(RefreshTokenLifetime), false)
	successor := mkRefreshTokenRow(ctx, t, pool, device, family,
		time.Now().Add(-past), time.Now().Add(-past).Add(RefreshTokenLifetime), false)
	linkReplacement(ctx, t, pool, predecessor, successor)

	_, err := pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE id = $1`, successor)
	if err == nil {
		t.Fatal("deleting the referenced end alone succeeded; a sweep bug of this shape would " +
			"silently break the chain instead of failing loudly")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("delete failed with %v, want a foreign key violation (23503)", err)
	}

	// FAILING LOUDLY, NOT CORRUPTING: both rows are exactly as they were.
	if !refreshTokenExists(ctx, t, pool, predecessor) || !refreshTokenExists(ctx, t, pool, successor) {
		t.Error("the failed delete still removed something")
	}
}
