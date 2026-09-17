package store

// CANT-29 — reuse detection's oracle.
//
// THE TWO HALVES ARE EQUALLY LOAD-BEARING, and a suite testing only one would
// be worse than none. A detector that never fires hands over the archive; a
// detector that fires on ordinary traffic invalidates a real person's family
// every time their phone wakes up, and gets turned off. So the tests below come
// in pairs: what MUST invalidate, and what MUST NOT.
//
// The false-positive side has a live example already in the repository —
// CANT-97's TestTwoSimultaneousPresentationsYieldExactlyOnePairAndOneRefusal is
// eight goroutines presenting one token, which is exactly the shape a naive
// detector reads as eight replays.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// backdateRotation moves a rotation into the past by rewriting the SUCCESSOR's
// issued_at, which is what dates a rotation — there is deliberately no
// `replaced_at` column. This is how a test reaches "presented long after it was
// spent" without sleeping through the grace window.
//
// make_interval RATHER THAN A DURATION STRING. Go renders 70s as "1m10s", and
// `m` is ambiguous in a Postgres interval literal; seconds as a number is not.
func backdateRotation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, device uuid.UUID, age time.Duration) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
		UPDATE refresh_tokens SET issued_at = now() - make_interval(secs => $2)
		 WHERE device_id = $1 AND id <> family_id`, device, age.Seconds())
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() == 0 {
		t.Fatal("precondition: no successor row to backdate — nothing has rotated")
	}
}

// familyState is what the account looks like after the fact.
type familyState struct {
	refreshRows, refreshRevoked, accessRows, accessRevoked int
	deviceRevoked                                          bool
}

func readFamilyState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, device uuid.UUID) familyState {
	t.Helper()
	var st familyState
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM refresh_tokens WHERE device_id = $1),
		       (SELECT count(*) FROM refresh_tokens WHERE device_id = $1 AND revoked_at IS NOT NULL),
		       (SELECT count(*) FROM access_tokens  WHERE device_id = $1),
		       (SELECT count(*) FROM access_tokens  WHERE device_id = $1 AND revoked_at IS NOT NULL),
		       (SELECT revoked_at IS NOT NULL FROM devices WHERE id = $1)`, device).
		Scan(&st.refreshRows, &st.refreshRevoked, &st.accessRows, &st.accessRevoked, &st.deviceRevoked); err != nil {
		t.Fatal(err)
	}
	return st
}

// revocationsDuring returns every revocation the given act published.
//
// A MARKER, NOT A SLEEP, on both ends. The listener is proved subscribed before
// the act with awaitSubscribed — Postgres queues nothing for a listener that was
// not there, so acting first would test nothing — and a trailing sentinel is
// published afterwards and read for. The listener delivers in order on one
// goroutine, so everything ahead of that sentinel is what the act published, and
// "nothing was published" is a real assertion rather than a timeout.
func revocationsDuring(ctx context.Context, t *testing.T, pool *pgxpool.Pool, during func()) []RevocationPayload {
	t.Helper()
	got := make(chan RevocationPayload, 16)
	l := &Listener[RevocationPayload]{
		DSN: testDSN(t), Channel: RevocationChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p RevocationPayload) { got <- p },
		OnGap:    func(context.Context) {},
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = l.Run(runCtx) }()

	sentinel, isSentinel := revocationSentinel()
	awaitSubscribed(ctx, t, pool, RevocationChannel, got, sentinel, isSentinel)

	during()

	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, RevocationChannel, sentinel); err != nil {
		t.Fatalf("trailing sentinel: %v", err)
	}
	var out []RevocationPayload
	for {
		select {
		case p := <-got:
			if isSentinel(p) {
				return out
			}
			out = append(out, p)
		case <-time.After(15 * time.Second):
			t.Fatal("the trailing sentinel never arrived")
		}
	}
}

// ---------------------------------------------------------------------------
// It fires

// The whole `Done when` in one test: the family is invalidated, the legitimate
// device is forced to re-authenticate rather than silently continuing, and the
// event is recorded.
func TestAReplayedRefreshInvalidatesTheFamilyAndForcesReAuthentication(t *testing.T) {
	ctx, pool := freshDB(t)
	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)
	e := enrolled(ctx, t, st, pool, "ada")

	// The device rotates normally. This is the pair a thief would steal.
	next, err := st.RotateRefresh(ctx, e.Refresh.Plaintext)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// Long enough ago that this cannot be an echo of that rotation.
	backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)

	// Precondition: nothing is revoked, so only the replay can do the work.
	if before := readFamilyState(ctx, t, pool, e.DeviceID); before.refreshRevoked != 0 ||
		before.accessRevoked != 0 || before.deviceRevoked {
		t.Fatalf("precondition: something was already revoked: %+v", before)
	}
	if _, err := st.Authenticate(ctx, next.Access.Plaintext); err != nil {
		t.Fatalf("precondition: the rotated access token should work: %v", err)
	}
	mark := len(logLines(t, buf))

	// THE REPLAY.
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a replayed token was exchanged: err = %v", err)
	}

	after := readFamilyState(ctx, t, pool, e.DeviceID)
	if after.refreshRevoked != after.refreshRows {
		t.Errorf("%d of %d refresh rows revoked, want all — refusing the replay and leaving the "+
			"chain live is what leaves an attacker holding a valid token",
			after.refreshRevoked, after.refreshRows)
	}
	if after.accessRevoked != after.accessRows {
		t.Errorf("%d of %d access tokens revoked, want all. A family-only invalidation lets the "+
			"holder carry on for the rest of the access TTL, which is the \"silently continuing\" "+
			"the Done when rules out", after.accessRevoked, after.accessRows)
	}
	if !after.deviceRevoked {
		t.Error("devices.revoked_at is not set, so Authenticate, DeadDevices and CANT-30's gap " +
			"re-check all disagree with the revocation that was just published")
	}

	// Forced to re-authenticate: what worked a moment ago does not.
	if _, err := st.Authenticate(ctx, next.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the rotated access token still authenticates after invalidation: err = %v", err)
	}
	// And the chain cannot be continued from its live head either.
	if _, err := st.RotateRefresh(ctx, next.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the family's live head still rotates after invalidation: err = %v", err)
	}

	// The event, recorded — with the ids, and never the credential.
	var recorded map[string]any
	for _, l := range logLines(t, buf)[mark:] {
		if l["msg"] == "refresh token replayed; family invalidated" {
			recorded = l
		}
	}
	if recorded == nil {
		t.Fatal("the invalidation was not recorded; an incident nobody can find in a log is one " +
			"nobody will act on")
	}
	if recorded["level"] != "ERROR" {
		t.Errorf("recorded at %v, want ERROR — every other refusal here is somebody's credential "+
			"being ordinary, and this is the only line that says a token was copied", recorded["level"])
	}
	for _, want := range []string{"family_id", "device_id", "user_id"} {
		if recorded[want] == nil {
			t.Errorf("the recorded event does not name %s", want)
		}
	}
	for _, l := range logLines(t, buf) {
		for k, v := range l {
			if s, ok := v.(string); ok {
				for _, secret := range []string{e.Refresh.Plaintext, next.Refresh.Plaintext, next.Access.Plaintext} {
					if strings.Contains(s, secret) {
						t.Errorf("a credential reached the log in field %q", k)
					}
				}
			}
		}
	}
}

// Severance rides on CANT-30's channel, so a replay ends the live socket too.
func TestAnInvalidatedFamilyPublishesTheRevocationThatSeversTheSocket(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)

	got := revocationsDuring(ctx, t, pool, func() {
		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("replay: %v", err)
		}
	})
	if len(got) != 1 {
		t.Fatalf("%d revocations published, want exactly 1 — without one the replayed device keeps "+
			"its live socket until it happens to drop, which is CANT-30's whole argument", len(got))
	}
	if got[0].DeviceID == nil || *got[0].DeviceID != e.DeviceID {
		t.Errorf("the revocation names %v, want device %s", got[0].DeviceID, e.DeviceID)
	}
}

// A second replay against an already-invalidated family writes and publishes
// nothing, on R6's idempotence terms.
func TestInvalidatingAnAlreadyInvalidatedFamilyIsANoOp(t *testing.T) {
	ctx, pool := freshDB(t)
	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)
	e := enrolled(ctx, t, st, pool, "ada")
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("first replay: %v", err)
	}
	first := readFamilyState(ctx, t, pool, e.DeviceID)
	mark := len(logLines(t, buf))

	got := revocationsDuring(ctx, t, pool, func() {
		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("second replay: %v", err)
		}
	})
	// THE GUARDS MAKE THE WRITES IDEMPOTENT AND SAY NOTHING ABOUT THE NOTIFY,
	// which is the bug this test found: publishing again would sever a device
	// that was severed the first time, and R6 asks for the opposite.
	if len(got) != 0 {
		t.Errorf("%d revocations published on a second replay, want 0 — a retry finding its work "+
			"already done must not re-notify", len(got))
	}
	if after := readFamilyState(ctx, t, pool, e.DeviceID); after != first {
		t.Errorf("the second replay changed state: %+v -> %+v", first, after)
	}

	// Idempotent is not the same as silent. Somebody is presenting a stolen
	// token repeatedly, and that is worth a line — just not a second incident.
	var sawRepeat, sawSecondIncident bool
	for _, l := range logLines(t, buf)[mark:] {
		switch l["msg"] {
		case "refresh replay against an already-invalidated family":
			sawRepeat = true
		case "refresh token replayed; family invalidated":
			sawSecondIncident = true
		}
	}
	if !sawRepeat {
		t.Error("a repeat replay against an already-invalidated family was logged as nothing at all")
	}
	if sawSecondIncident {
		t.Error("a repeat replay was reported as a fresh invalidation; nothing was invalidated")
	}
}

// ---------------------------------------------------------------------------
// It does NOT fire

// THE FALSE POSITIVE THAT MATTERS MOST. Eight goroutines present one token —
// CANT-97's concurrency case, and the shape of a phone waking with two queued
// requests. Exactly one wins; the seven refusals are echoes, not replays, and
// the family must survive untouched.
func TestALostRotationRaceDoesNotInvalidateTheFamily(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	const racers = 8
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	results := make([]error, racers)
	for i := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, results[i] = st.RotateRefresh(ctx, e.Refresh.Plaintext)
		}()
	}
	start.Done()
	done.Wait()

	var winners int
	for _, err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d winners, want 1 — this test's precondition is CANT-97's own criterion", winners)
	}

	after := readFamilyState(ctx, t, pool, e.DeviceID)
	if after.refreshRevoked != 0 || after.accessRevoked != 0 || after.deviceRevoked {
		t.Errorf("a lost race invalidated the family: %+v.\n"+
			"This is a working phone with two in-flight refreshes. Invalidating here logs a real "+
			"person out and reports an incident that did not happen — the failure this ticket's "+
			"own description names, and the reason the grace window exists", after)
	}
	// The device's own credential still works, which is what makes the seven
	// refusals harmless rather than merely unlogged.
	if _, err := st.Authenticate(ctx, e.Access.Plaintext); err != nil {
		t.Errorf("the device's access token stopped working after a lost race: %v", err)
	}
}

// The same presentation, sequentially, inside the window: refused, and not an
// incident. This is the retry-after-a-lost-response case.
func TestASpentTokenPresentedInsideTheGraceWindowIsRefusedButNotAnIncident(t *testing.T) {
	ctx, pool := freshDB(t)
	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)
	e := enrolled(ctx, t, st, pool, "ada")

	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	mark := len(logLines(t, buf))

	// No backdating: the rotation just happened, so this is inside the window.
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("the spent token was exchanged: err = %v", err)
	}

	if after := readFamilyState(ctx, t, pool, e.DeviceID); after.refreshRevoked != 0 ||
		after.accessRevoked != 0 || after.deviceRevoked {
		t.Errorf("a presentation inside the grace window invalidated the family: %+v", after)
	}

	var sawIncident, sawRefusal bool
	for _, l := range logLines(t, buf)[mark:] {
		switch l["msg"] {
		case "refresh token replayed; family invalidated":
			sawIncident = true
		case "refresh refused":
			sawRefusal = true
		}
	}
	if sawIncident {
		t.Error("an incident was reported for a presentation inside the grace window")
	}
	if !sawRefusal {
		t.Error("the refusal was not logged at all; visibility is what stands in for the rate " +
			"limiter this route deliberately does not have")
	}
}

// The window is a boundary, so it is tested as one.
func TestTheGraceWindowBoundaryDecidesEchoFromReplay(t *testing.T) {
	for _, tc := range []struct {
		name       string
		age        time.Duration
		invalidate bool
	}{
		{"just inside", ReuseGraceWindow - 2*time.Second, false},
		{"just outside", ReuseGraceWindow + 2*time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool := freshDB(t)
			st := New(pool, DefaultLimits(), discardLogger())
			e := enrolled(ctx, t, st, pool, "ada")
			if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
				t.Fatalf("rotate: %v", err)
			}
			backdateRotation(ctx, t, pool, e.DeviceID, tc.age)

			if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("presentation: %v", err)
			}
			if got := readFamilyState(ctx, t, pool, e.DeviceID).deviceRevoked; got != tc.invalidate {
				t.Errorf("at age %v the family was invalidated = %v, want %v (window is %v)",
					tc.age, got, tc.invalidate, ReuseGraceWindow)
			}
		})
	}
}

// The other doors are refusals, not incidents: administering an account must
// never look like a theft.
func TestTheOtherRefusalsNeverInvalidateAFamily(t *testing.T) {
	ctx, pool := freshDB(t)
	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)

	t.Run("an expired refresh token", func(t *testing.T) {
		e := enrolled(ctx, t, st, pool, "expired")
		if _, err := pool.Exec(ctx,
			`UPDATE refresh_tokens SET expires_at = now() - interval '1 second' WHERE device_id = $1`,
			e.DeviceID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("expired: %v", err)
		}
		if after := readFamilyState(ctx, t, pool, e.DeviceID); after.deviceRevoked {
			t.Error("an expired token invalidated the family; a device falling out of use is not a theft")
		}
	})

	t.Run("a deactivated account", func(t *testing.T) {
		e := enrolled(ctx, t, st, pool, "gone")
		if _, err := pool.Exec(ctx,
			`UPDATE users SET deactivated_at = now() WHERE id = $1`, e.UserID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("deactivated: %v", err)
		}
		if after := readFamilyState(ctx, t, pool, e.DeviceID); after.deviceRevoked {
			t.Error("a deactivated account invalidated the family; an offboard is not a theft")
		}
	})

	for _, l := range logLines(t, buf) {
		if l["msg"] == "refresh token replayed; family invalidated" {
			t.Error("an ordinary refusal reported a replay incident")
		}
	}
}

// THE INVALIDATION MUST OUTLIVE THE REQUEST THAT TRIGGERED IT.
//
// The context reaching refusalOutcome is the caller's — router.go passes
// r.Context() — and Go cancels it the moment the client's connection closes.
// The refusal is already decided by then, so cancellation cannot change what
// the caller is told; what it could do is kill the invalidation, letting
// somebody present a stolen token, hang up, and leave the family live at will —
// confirming the token is spent without ever tripping the detector.
//
// CALLED DIRECTLY RATHER THAN THROUGH RotateRefresh, deliberately. A context
// cancelled before that call fails at pool.Begin long before any detection
// happens, so there would be nothing to observe; the detachment belongs to the
// function that owns it, and this hands it the dead context on purpose.
func TestAnInvalidationSurvivesACancelledCallerContext(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)

	var tokenID, familyID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT id, family_id FROM refresh_tokens
		 WHERE device_id = $1 AND id = family_id`, e.DeviceID).Scan(&tokenID, &familyID); err != nil {
		t.Fatal(err)
	}

	dead, cancel := context.WithCancel(ctx)
	cancel()
	st.refusalOutcome(dead, tokenID, familyID, e.DeviceID, e.UserID)

	after := readFamilyState(ctx, t, pool, e.DeviceID)
	if !after.deviceRevoked || after.refreshRevoked != after.refreshRows {
		t.Errorf("the family survived a replay whose caller had hung up: %+v.\n"+
			"The refusal is already decided by that point, so nothing about the caller's "+
			"lifetime should reach this — and if it does, a prober can confirm a stolen token "+
			"is spent and keep the family alive, repeatably", after)
	}
}

// A refused replay must leave no successor behind. The rotation transaction is
// rolled back BEFORE invalidation precisely so the family being revoked cannot
// simultaneously acquire a brand-new live token.
func TestAReplayLeavesNoNewTokenInTheFamilyItInvalidates(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replay: %v", err)
	}

	if after := readFamilyState(ctx, t, pool, e.DeviceID); after.refreshRows != 2 {
		t.Errorf("%d refresh rows after a replay, want 2 — the replay's own successor insert must "+
			"roll back, or the invalidation revokes a family that just gained a live token",
			after.refreshRows)
	}
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM refresh_tokens WHERE device_id = $1 AND revoked_at IS NULL`,
		e.DeviceID); n != 0 {
		t.Errorf("%d unrevoked refresh rows survive the invalidation, want 0", n)
	}
}
