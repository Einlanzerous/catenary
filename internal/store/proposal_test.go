package store

// CANT-125 — the proposed successor, and what a collision on it means.
//
// The proposal changes ONE thing: whose string the successor's hash is of.
// Everything reuse detection guards has to be exactly as it was, so most of
// what is here is the old tests' assertions made again with a proposal in the
// request — and then the three branches of routeCollision, each with the
// account's state read back from the database rather than inferred from the
// error.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// proposal is a well-formed Token that is not any minted one.
func proposal(t *testing.T) string {
	t.Helper()
	p, _, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func nothingRevoked(ctx context.Context, t *testing.T, st familyState, what string) {
	t.Helper()
	if st.refreshRevoked != 0 || st.accessRevoked != 0 || st.deviceRevoked {
		t.Errorf("%s: something was revoked: %+v", what, st)
	}
}

// The server issues what the client proposed — and stores only its hash, so
// the proof that it did is that the proposal then WORKS as a refresh token.
func TestAProposedSuccessorIsTheTokenTheServerIssues(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	p := proposal(t)
	got, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p)
	if err != nil {
		t.Fatalf("rotate with a proposal: %v", err)
	}
	if got.Refresh.Plaintext != p {
		t.Fatal("the returned refresh token is not the proposal; the response is authoritative, and it must SAY so by returning it")
	}
	if got.Access.Plaintext == "" || got.DeviceID != e.DeviceID {
		t.Errorf("the rotation is otherwise malformed: %+v", got.DeviceID)
	}

	// ONE LOST RESPONSE IS SURVIVABLE: a client that never saw `got` still
	// knows p, presents it as its newest token, and is rotated normally.
	if _, err := st.RotateRefreshProposing(ctx, p, proposal(t)); err != nil {
		t.Fatalf("the proposal did not work as the device's refresh token: %v", err)
	}
	nothingRevoked(ctx, t, readFamilyState(ctx, t, pool, e.DeviceID), "two honest rotations")

	var plaintextStored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, []byte(p)).Scan(&plaintextStored); err != nil {
		t.Fatal(err)
	}
	if plaintextStored != 0 {
		t.Error("the proposal's PLAINTEXT is in token_hash; only its hash may be stored")
	}
}

// BRANCH 2 — THE IN-FLIGHT RETRY. (R, P) committed and the response was lost;
// the client sends (R, P) again. The rotation it is asking for has happened.
// 503: never a 500, and never a 401, which would now be a logout.
func TestRetryingTheSamePairInsideTheWindowIsARetryNotARefusal(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")
	p := proposal(t)
	if _, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p); err != nil {
		t.Fatal(err)
	}

	_, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p)
	if !errors.Is(err, ErrRefreshRetryPresentProposal) {
		t.Fatalf("the in-flight retry = %v, want ErrRefreshRetryPresentProposal — the client must be told to stop presenting R", err)
	}
	// STILL THAT ANSWER A SECOND TIME, and nothing worse: inside the window a
	// repeat is tolerated. What it must never be is an instruction.
	if _, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p); !errors.Is(err, ErrRefreshRetryPresentProposal) {
		t.Errorf("the same retry again = %v", err)
	}
	after := readFamilyState(ctx, t, pool, e.DeviceID)
	nothingRevoked(ctx, t, after, "the in-flight retry")
	if after.refreshRows != 2 {
		t.Errorf("%d refresh rows, want the original and its one successor — the retry must not have forked the family", after.refreshRows)
	}
	// And the client's way out works: it presents P.
	if _, err := st.RotateRefreshProposing(ctx, p, proposal(t)); err != nil {
		t.Errorf("presenting the proposal after the retry: %v", err)
	}
}

// BRANCH 3 — THE SECURITY-CRITICAL ONE. A thief who copied the pair (R, P)
// and replays it after the window gets exactly what a thief replaying a spent
// token without a proposal gets today: a 401, and the whole family invalidated.
// The proposal must buy a replay nothing.
func TestReplayingACopiedPairOutsideTheWindowStillInvalidatesTheFamily(t *testing.T) {
	for _, tc := range []struct {
		name string
		// replayWith is what the thief proposes: the copied P, or a P of their own.
		replayWith func(copied string, t *testing.T) string
	}{
		{"the copied pair, proposal and all", func(copied string, _ *testing.T) string { return copied }},
		{"the copied token with a proposal of the thief's own", func(_ string, t *testing.T) string { return proposal(t) }},
		{"the copied token with no proposal — today's replay, unchanged", func(string, *testing.T) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool := freshDB(t)
			st := New(pool, DefaultLimits(), discardLogger())
			e := enrolled(ctx, t, st, pool, "ada")
			p := proposal(t)
			if _, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p); err != nil {
				t.Fatal(err)
			}
			backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)
			nothingRevoked(ctx, t, readFamilyState(ctx, t, pool, e.DeviceID), "precondition")

			_, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, tc.replayWith(p, t))
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("the replay = %v, want ErrUnauthorized", err)
			}
			after := readFamilyState(ctx, t, pool, e.DeviceID)
			if !after.deviceRevoked || after.refreshRevoked != after.refreshRows || after.accessRevoked != after.accessRows {
				t.Errorf("the replay went undetected — the family is not invalidated: %+v", after)
			}
			// The legitimate holder of P is locked out too, which is what
			// invalidation means and what forces the re-authentication.
			if _, err := st.RotateRefreshProposing(ctx, p, proposal(t)); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("the successor still rotates after the family was invalidated: %v", err)
			}
		})
	}
}

// BRANCH 1 — A COLLISION WHILE THE PRESENTED TOKEN IS STILL LIVE. Nothing has
// rotated it, so this is a bad proposal and not a retry of anything. 503; the
// client mints a fresh proposal and succeeds; nothing is revoked, and nothing
// reaches reuse detection.
func TestACollidingProposalOnALiveTokenIsARetryAndTheTokenSurvives(t *testing.T) {
	ctx, pool := freshDB(t)
	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)
	e := enrolled(ctx, t, st, pool, "ada")
	other := enrolled(ctx, t, st, pool, "theo")

	for name, colliding := range map[string]string{
		"another device's live refresh token": other.Refresh.Plaintext,
		"the presented token itself":          e.Refresh.Plaintext,
	} {
		_, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, colliding)
		if !errors.Is(err, ErrRefreshRetryFreshProposal) {
			t.Fatalf("%s as the proposal = %v, want ErrRefreshRetryFreshProposal — the token is still good", name, err)
		}
	}
	for who, dev := range map[string]Enrollment{"the proposer": e, "the bystander": other} {
		st := readFamilyState(ctx, t, pool, dev.DeviceID)
		nothingRevoked(ctx, t, st, who)
		if st.refreshRows != 1 {
			t.Errorf("%s holds %d refresh rows, want 1 — a refused proposal committed something", who, st.refreshRows)
		}
	}
	if logged := buf.String(); containsAny(logged, "replay suspected", "family invalidated") {
		t.Errorf("a bad proposal reached reuse detection:\n%s", logged)
	}

	// THE TOKEN IS STILL GOOD: a fresh proposal succeeds.
	if _, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, proposal(t)); err != nil {
		t.Fatalf("a fresh proposal after the collision: %v", err)
	}
	// And the bystander's token was not consumed by being named.
	if _, err := st.RotateRefresh(ctx, other.Refresh.Plaintext); err != nil {
		t.Errorf("the bystander's token no longer rotates: %v", err)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// THE TWO NEGATIVE CONTROLS (plan criterion 26), each watched failing in the
// direction its branch exists to prevent.
func TestWithoutItsBranchesACollisionFailsTheWayEachBranchPrevents(t *testing.T) {
	t.Run("live-token branch removed: a healthy device's bad proposal is answered 401", func(t *testing.T) {
		ctx, pool := freshDB(t)
		st := New(pool, DefaultLimits(), discardLogger())
		st.collisionFault = faultCollisionNoLiveBranch
		e := enrolled(ctx, t, st, pool, "ada")
		other := enrolled(ctx, t, st, pool, "theo")

		_, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, other.Refresh.Plaintext)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("with branch 1 removed the collision = %v; the control expects the 401 that branch exists to prevent", err)
		}
		// WHAT THE CONTROL ACTUALLY SHOWS, recorded because the plan predicted
		// more: the device is NOT revoked — refusalOutcome finds R unspent and
		// invalidates nothing. The damage is the 401 itself, which a client
		// has treated as terminal since CANT-123: a working device, logged out.
		nothingRevoked(ctx, t, readFamilyState(ctx, t, pool, e.DeviceID), "the misrouted bad proposal")
	})

	t.Run("collision unrouted: the copied-pair replay is a 500 and goes undetected", func(t *testing.T) {
		ctx, pool := freshDB(t)
		st := New(pool, DefaultLimits(), discardLogger())
		st.collisionFault = faultCollisionUnrouted
		e := enrolled(ctx, t, st, pool, "ada")
		p := proposal(t)
		if _, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p); err != nil {
			t.Fatal(err)
		}
		backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)

		_, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p)
		if err == nil || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrRefreshRetry) {
			t.Fatalf("unrouted, the replay = %v; the control expects the raw unique violation", err)
		}
		after := readFamilyState(ctx, t, pool, e.DeviceID)
		if after.deviceRevoked || after.refreshRevoked != 0 {
			t.Fatalf("the control proves nothing: the family was invalidated anyway: %+v", after)
		}
		// The thief's replay cost them nothing and told the account nothing.
	})
}

// EIGHT SIMULTANEOUS PRESENTATIONS OF ONE (R, P). One rotates; the other seven
// are told to retry. None is refused and none is a 500, and the family has one
// successor — the INSERT's unique index serialises them before any reaches the
// door.
func TestSimultaneousPresentationsOfOnePairYieldOneRotationAndRetries(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")
	p := proposal(t)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, p)
		}()
	}
	close(start)
	wg.Wait()

	var rotated, retried int
	for i, err := range errs {
		switch {
		case err == nil:
			rotated++
		case errors.Is(err, ErrRefreshRetryPresentProposal):
			retried++ // each loser is told the rotation happened, and to present P
		default:
			t.Errorf("racer %d = %v; every loser must be a retry, never a refusal and never a failure", i, err)
		}
	}
	if rotated != 1 || retried != n-1 {
		t.Errorf("%d rotated and %d retried, want 1 and %d", rotated, retried, n-1)
	}
	after := readFamilyState(ctx, t, pool, e.DeviceID)
	nothingRevoked(ctx, t, after, "the race")
	if after.refreshRows != 2 {
		t.Errorf("%d refresh rows, want 2 — the family forked", after.refreshRows)
	}
}

// THE TRANSACTION IS BOUNDED WELL BELOW THE WINDOW (plan criterion 27). A
// rotation that cannot finish does not commit, so it cannot stamp a successor
// with a transaction-start `now()` older than the window its own retry will be
// judged by.
func TestARotationThatCannotFinishInItsBudgetCommitsNothing(t *testing.T) {
	if rotateBudget*2 > ReuseGraceWindow {
		t.Fatalf("rotateBudget %s is not well below ReuseGraceWindow %s", rotateBudget, ReuseGraceWindow)
	}
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	// Another transaction holds R's row, so the rotation's UPDATE blocks.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM refresh_tokens WHERE device_id = $1 FOR UPDATE`, e.DeviceID); err != nil {
		t.Fatal(err)
	}

	began := time.Now()
	_, err = st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, proposal(t))
	took := time.Since(began)
	if err == nil || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrRefreshRetry) {
		t.Fatalf("a rotation blocked past its budget = %v, want a plain failure", err)
	}
	if took < rotateBudget || took > rotateBudget+2*time.Second {
		t.Errorf("it gave up after %s, want about the %s budget", took, rotateBudget)
	}
	_ = blocker.Rollback(ctx)

	after := readFamilyState(ctx, t, pool, e.DeviceID)
	if after.refreshRows != 1 {
		t.Errorf("%d refresh rows after a rotation that gave up, want 1 — it committed a successor", after.refreshRows)
	}
	// The token is still good: the client's retry succeeds.
	if _, err := st.RotateRefreshProposing(ctx, e.Refresh.Plaintext, proposal(t)); err != nil {
		t.Errorf("the retry after the abandoned rotation: %v", err)
	}
}

// routeCollision is reached BY CONSTRAINT NAME, so that a 23505 on the primary
// key is never mistaken for a colliding proposal. 0007 declares the constraint
// inline and Postgres names it; this pins the name the code matches on, so a
// migration that renames it fails here and not as a 500 in production.
func TestTheCollisionIsRecognisedByTheConstraintPostgresActuallyNamed(t *testing.T) {
	ctx, pool := freshDB(t)
	var name string
	if err := pool.QueryRow(ctx, `
		SELECT c.conname
		  FROM pg_constraint c
		  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		 WHERE c.conrelid = 'refresh_tokens'::regclass AND c.contype = 'u' AND a.attname = 'token_hash'`).Scan(&name); err != nil {
		t.Fatalf("refresh_tokens has no UNIQUE constraint on token_hash: %v", err)
	}
	if name != refreshTokenHashConstraint {
		t.Errorf("the constraint is %q; isTokenHashCollision matches %q and would route nothing", name, refreshTokenHashConstraint)
	}
}
