package store

// CANT-97 — rotation's oracle.
//
// TWO OF THESE ARE CANT-28'S CRITERIA, CARRIED HERE VERBATIM rather than
// restated: the exchange's concurrency and a deactivated account's third door.
// That ticket's own tests say so at the top of tokens_test.go, and they are
// here because the code they test is here.
//
// Every refusal test below is arranged so that ONLY the condition under test
// can be doing the work — the token is live and unrotated, the device is
// un-revoked, nothing has expired. Without that a passing test proves only that
// something said no, which is the failure mode a four-door credential check is
// most prone to.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// enrolled is one device with its first pair, which is where every test here
// starts. Rotation has nothing to exchange until enrollment has happened.
func enrolled(ctx context.Context, t *testing.T, st *Store, pool *pgxpool.Pool, handle string) Enrollment {
	t.Helper()
	user := mkUser(ctx, t, pool, handle)
	issued, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	e, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel 8 Pro")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return e
}

// assertRefreshLive fails unless every reason to refuse EXCEPT the one under
// test is absent. The sibling of tokens_test.go's assertLive, for the token
// this ticket's doors are about.
func assertRefreshLive(ctx context.Context, t *testing.T, pool *pgxpool.Pool, device uuid.UUID) {
	t.Helper()
	var replacedBy *uuid.UUID
	var revoked *time.Time
	var expires time.Time
	if err := pool.QueryRow(ctx, `
		SELECT replaced_by, revoked_at, expires_at
		  FROM refresh_tokens WHERE device_id = $1 AND replaced_by IS NULL`, device).
		Scan(&replacedBy, &revoked, &expires); err != nil {
		t.Fatalf("precondition: no live refresh token for the device: %v", err)
	}
	switch {
	case replacedBy != nil:
		t.Fatal("precondition: the refresh token has already been rotated, so this test proves nothing")
	case revoked != nil:
		t.Fatal("precondition: the refresh token is revoked, so this test proves nothing")
	case !expires.After(ServerTime()):
		t.Fatal("precondition: the refresh token has expired, so this test proves nothing")
	}
}

// ---------------------------------------------------------------------------
// The exchange itself

func TestRotationReturnsANewPairAndMarksThePresentedTokenReplaced(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	got, err := st.RotateRefresh(ctx, e.Refresh.Plaintext)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if got.UserID != e.UserID || got.DeviceID != e.DeviceID {
		t.Errorf("rotated to user %s device %s, want %s / %s",
			got.UserID, got.DeviceID, e.UserID, e.DeviceID)
	}
	// BOTH are new. A response that returned the presented refresh token
	// unchanged would be a non-rotating credential wearing a rotating one's
	// name, which is the shape CANT-28 exists to rule out.
	if got.Refresh.Plaintext == e.Refresh.Plaintext {
		t.Error("the exchange returned the SAME refresh token; this endpoint rotates, " +
			"and a client presenting the original twice must be refused on the second call")
	}
	if got.Access.Plaintext == e.Access.Plaintext {
		t.Error("the exchange returned the same access token")
	}
	if got.Access.Plaintext == got.Refresh.Plaintext {
		t.Error("the access and refresh tokens are the same string")
	}
	if !got.Access.ExpiresAt.After(ServerTime()) || !got.Refresh.ExpiresAt.After(got.Access.ExpiresAt) {
		t.Errorf("expiries are wrong: access %v, refresh %v", got.Access.ExpiresAt, got.Refresh.ExpiresAt)
	}

	// The predecessor names its successor, and the successor carries the
	// family. That pair of facts is what CANT-29's detection reasons over.
	var replacedBy *uuid.UUID
	var oldFamily uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT replaced_by, family_id FROM refresh_tokens
		 WHERE device_id = $1 AND id = family_id`, e.DeviceID).Scan(&replacedBy, &oldFamily); err != nil {
		t.Fatal(err)
	}
	if replacedBy == nil {
		t.Fatal("the presented token has no replaced_by — the rotation did not mark it, so a " +
			"replay is indistinguishable from a first use and CANT-29 has nothing to detect")
	}
	var newFamily uuid.UUID
	var newReplacedBy *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT family_id, replaced_by FROM refresh_tokens WHERE id = $1`, *replacedBy).
		Scan(&newFamily, &newReplacedBy); err != nil {
		t.Fatalf("the successor row named by replaced_by does not exist: %v", err)
	}
	if newFamily != oldFamily {
		t.Errorf("the successor's family_id is %s, want the predecessor's %s — invalidation is "+
			"one predicate over one column, and a family that does not carry forward breaks it",
			newFamily, oldFamily)
	}
	if newReplacedBy != nil {
		t.Error("the successor is already marked replaced")
	}
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM refresh_tokens WHERE device_id = $1`, e.DeviceID); n != 2 {
		t.Errorf("%d refresh rows after one rotation, want exactly 2", n)
	}
}

func TestASecondPresentationOfARotatedTokenIsRefused(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a rotated token was exchanged a second time: err = %v. Single use is the whole "+
			"of this credential's security story", err)
	}
	// And the refused attempt left nothing behind. The successor row is
	// inserted before the conditional update, so a refusal that did not roll
	// back would leave a second live head of one family.
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM refresh_tokens WHERE device_id = $1`, e.DeviceID); n != 2 {
		t.Errorf("%d refresh rows after a refused replay, want 2 — the rollback is supposed to "+
			"take the successor row with it", n)
	}
}

// Criterion — rotation is atomic under concurrency.
//
// THE CASE IS A PHONE WAKING UP: two in-flight requests both find a stale
// access token and both present the same refresh token within milliseconds.
// Sequentially, `replaced_by IS NOT NULL` refuses the second on any
// implementation; the interesting case is two that read the row before either
// has written it. A read-then-write forks the family into two live chains with
// replaced_by written twice — and then either a genuine replay is never
// detected, or a legitimate double-refresh invalidates a real person's family
// and forces a re-authentication that looks exactly like the incident it is
// meant to report.
func TestTwoSimultaneousPresentationsYieldExactlyOnePairAndOneRefusal(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	const racers = 8
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	results := make([]error, racers)
	pairs := make([]Rotated, racers)
	for i := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			pairs[i], results[i] = st.RotateRefresh(ctx, e.Refresh.Plaintext)
		}()
	}
	start.Done()
	done.Wait()

	var winners, refusals int
	for i, err := range results {
		switch {
		case err == nil:
			winners++
			if pairs[i].Refresh.Plaintext == "" || pairs[i].Access.Plaintext == "" {
				t.Error("a winning rotation returned an empty credential")
			}
		case errors.Is(err, ErrUnauthorized):
			refusals++
		default:
			t.Errorf("racer %d failed with an unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Errorf("%d of %d concurrent presentations succeeded, want exactly 1 — two live chains "+
			"from one token is a forked family, and CANT-29's detection cannot tell that from "+
			"a theft afterwards", winners, racers)
	}
	if refusals != racers-1 {
		t.Errorf("%d refusals, want %d", refusals, racers-1)
	}

	// The database agrees: one successor, one marked predecessor.
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM refresh_tokens WHERE device_id = $1`, e.DeviceID); n != 2 {
		t.Errorf("%d refresh rows after the race, want exactly 2", n)
	}
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM refresh_tokens WHERE device_id = $1 AND replaced_by IS NOT NULL`,
		e.DeviceID); n != 1 {
		t.Errorf("%d rows carry replaced_by after the race, want exactly 1 — more than one means "+
			"the write read the row before deciding, which is the fork this ticket exists to "+
			"rule out", n)
	}
}

// ---------------------------------------------------------------------------
// The four doors

// R6's third door, and the sentence the offboard ordering rests on: "a disabled
// account cannot refresh a token or enroll a device". CANT-28 made the enroll
// half true; this is the refresh half.
func TestADeactivatedUserCannotExchangeARefreshToken(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "grace")

	if _, err := pool.Exec(ctx,
		`UPDATE users SET deactivated_at = now() WHERE id = $1`, e.UserID); err != nil {
		t.Fatal(err)
	}
	// Everything else is deliberately still in order, so only
	// users.deactivated_at can be doing the work.
	assertRefreshLive(ctx, t, pool, e.DeviceID)

	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a deactivated account exchanged a refresh token: err = %v.\n"+
			"R6 chose disable-then-revoke on exactly this sentence, so without it the offboard "+
			"fails open in the way the rejected ordering was rejected for", err)
	}
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM refresh_tokens WHERE device_id = $1`, e.DeviceID); n != 1 {
		t.Errorf("%d refresh rows after a refused exchange, want 1", n)
	}
}

// The device's door, found on CANT-28's review. RevokeDevice writes
// devices.revoked_at and does not touch that device's refresh_tokens, so a
// write reasoning over the token row alone would let a revoked device keep
// rotating its family forever.
func TestARevokedDeviceCannotExchangeARefreshToken(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	revoked, err := st.RevokeDevice(ctx, e.DeviceID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !revoked {
		t.Fatal("RevokeDevice reported nothing to do on a live device")
	}
	// The token itself is untouched by revocation — which is the point.
	assertRefreshLive(ctx, t, pool, e.DeviceID)

	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a revoked device rotated its family: err = %v.\n"+
			"No access comes of it, because Authenticate's join refuses the minted token — but it "+
			"leaves a live rotating chain in exactly the state CANT-29's detection reasons over, "+
			"which is the wrong thing to hand that ticket", err)
	}
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM refresh_tokens WHERE device_id = $1`, e.DeviceID); n != 1 {
		t.Errorf("%d refresh rows after a revoked device's attempt, want 1", n)
	}
}

func TestAnExpiredRefreshTokenIsRefused(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	if _, err := pool.Exec(ctx,
		`UPDATE refresh_tokens SET expires_at = now() - interval '1 second' WHERE device_id = $1`,
		e.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("an expired refresh token was exchanged: err = %v. Sixty days is what makes a "+
			"device forgotten in a drawer fall out of the account, and a column nothing checks "+
			"reads as protection it does not provide", err)
	}
}

func TestAnUnknownRefreshTokenIsRefused(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	unknown, _, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotateRefresh(ctx, unknown); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a well-formed token that was never issued was exchanged: err = %v", err)
	}
	if _, err := st.RotateRefresh(ctx, ""); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("an empty credential was exchanged: err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// The point of the whole exchange

// A DEVICE THAT HAS REFRESHED AUTHENTICATES PAST THE ACCESS TTL. This is the
// `Done when` clause that says the feature works rather than that its edges
// are guarded: fifteen minutes is short by design, and without a working
// exchange every client is logged out four times an hour.
func TestADeviceThatHasRefreshedAuthenticatesPastTheAccessTTL(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	// The original access token ages out, exactly as it would on a phone that
	// slept for an hour.
	if _, err := pool.Exec(ctx,
		`UPDATE access_tokens SET expires_at = now() - interval '1 second' WHERE device_id = $1`,
		e.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Authenticate(ctx, e.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("precondition: the original access token should have expired")
	}

	got, err := st.RotateRefresh(ctx, e.Refresh.Plaintext)
	if err != nil {
		t.Fatalf("rotate after the access token expired: %v. The refresh token is what a client "+
			"has left at this point, and if it cannot be exchanged the device is logged out", err)
	}
	caller, err := st.Authenticate(ctx, got.Access.Plaintext)
	if err != nil {
		t.Fatalf("the access token from a rotation was refused: %v", err)
	}
	if caller.UserID != e.UserID || caller.DeviceID != e.DeviceID {
		t.Errorf("the rotated token resolves to user %s device %s, want %s / %s",
			caller.UserID, caller.DeviceID, e.UserID, e.DeviceID)
	}
	// And the new refresh token works for the NEXT exchange, so this is a chain
	// rather than one extra life.
	if _, err := st.RotateRefresh(ctx, got.Refresh.Plaintext); err != nil {
		t.Errorf("the second rotation failed: %v", err)
	}
}
