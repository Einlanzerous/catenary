package store

// CANT-134 criterion 5 — whatever reverses an offboard revokes for itself.
//
// THIS FILE REPLACES CANT-130's TestEnsurePersonOnADeactivatedPersonRefusesAnd-
// ChangesNothing, which pinned the placeholder ruling 5 always meant to
// supersede: ErrPersonDeactivated, and nothing changed. The behaviour that test
// asserted is now the behaviour that would be a bug.
//
// THE CENTRAL CASE IS THE RACE RedeemEnrollment ACCEPTS (tokens.go): a device
// row that commits AFTER an offboard's own sweep has run, so the sweep cannot
// see it. While the account is disabled that device has no access, which is why
// the race is accepted. Clear deactivated_at over it and it is live again —
// which is why the reversal sweeps for itself rather than trusting the offboard
// to have. strayDevice below drives exactly that interleaving, deterministically.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// strayDevice drives the redeem-during-offboard interleaving and returns the
// device it leaves behind, together with the offboard that could not see it.
//
// HOW THE INTERLEAVING IS FORCED, AND WHY IT IS DETERMINISTIC RATHER THAN
// TIMED. Store.offboardPause is called by revokeCredentialsTx AFTER its devices
// write and BEFORE its enrollment supersede (see that field's own comment), and
// this fixture redeems from inside it. At that moment:
//
//   - the offboard has set deactivated_at but has NOT COMMITTED, so the
//     redeem's own snapshot reads it as NULL and RedeemEnrollment proceeds —
//     the unlocked read its doc comment describes;
//   - the offboard's devices sweep has ALREADY RUN, so the row this insert
//     creates is not in it and is not revoked;
//   - nothing blocks: the redeem locks its enrollment row first (the offboard
//     has not reached that table yet), then takes only KEY SHARE on users(id),
//     which does not conflict with the offboard's FOR NO KEY UPDATE.
//
// The redeem runs on the offboard's own goroutine and still commits in a
// SEPARATE TRANSACTION, because it checks out its own pool connection — which
// is the property that matters here, and doing it inline means there is no
// synchronization of the test's own to get wrong.
func strayDevice(ctx context.Context, t *testing.T, st *Store, f offboarded) Enrollment {
	t.Helper()

	var stray Enrollment
	var strayErr error
	st.offboardPause = func() {
		stray, strayErr = st.RedeemEnrollment(ctx, f.Pending.Plaintext, "enrolled mid-offboard")
	}
	out, err := st.DeactivateUser(ctx, f.UserID)
	st.offboardPause = nil
	if err != nil {
		t.Fatalf("the offboard failed: %v", err)
	}
	if strayErr != nil {
		t.Fatalf("the redeem racing the offboard was refused (%v) — it must SUCCEED, or this fixture "+
			"is not driving the race tokens.go documents", strayErr)
	}
	if !out.Deactivated {
		t.Fatal("the offboard did not deactivate the account")
	}

	// The state the race leaves, asserted rather than assumed: a live device on
	// a deactivated account — and no access, which is why R6 accepts it.
	var revoked *time.Time
	mustScan(t, st.pool.QueryRow(ctx, `SELECT revoked_at FROM devices WHERE id = $1`, stray.DeviceID), &revoked)
	if revoked != nil {
		t.Fatal("the stray device was revoked by the offboard, so the reversal has nothing to resurrect " +
			"and the control below would prove nothing")
	}
	if _, err := st.Authenticate(ctx, stray.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("the stray device has access while the account is deactivated: %v", err)
	}
	return stray
}

// ---------------------------------------------------------------------------
// The reversal

func TestEnsurePersonOnADeactivatedPersonReactivatesThemAfterRevokingEverything(t *testing.T) {
	ctx, pool := freshDB(t)
	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)
	f := newOffboarded(ctx, t, st)

	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	mark := len(logLines(t, buf))

	back, err := st.EnsurePerson(ctx, f.Email, "Ada Lovelace")
	if err != nil {
		t.Fatalf("ensure on a deactivated person: %v", err)
	}
	if back.Outcome != PersonReactivated {
		t.Errorf("outcome = %q, want %q — Purser's Provision stays idempotent by reactivating, and the "+
			"operator who caused it learns which of the two happened from this value alone",
			back.Outcome, PersonReactivated)
	}
	if back.Account.UserID != f.UserID {
		t.Error("the reactivation produced a different account")
	}
	if back.Account.Status != PersonStatusActive {
		t.Errorf("status = %q, want active", back.Account.Status)
	}

	var deactivated *time.Time
	mustScan(t, pool.QueryRow(ctx, `SELECT deactivated_at FROM users WHERE id = $1`, f.UserID), &deactivated)
	if deactivated != nil {
		t.Error("deactivated_at is still set after a reactivation")
	}

	// NOTHING FROM BEFORE SURVIVES. The person enrolls again from nothing.
	state := readAccountState(ctx, t, pool, f.UserID)
	if state.devicesRevoked != state.devices {
		t.Errorf("%d of %d devices live after a reactivation, want 0 — clearing the column over a live "+
			"device hands it back everything it had", state.devices-state.devicesRevoked, state.devices)
	}
	for _, tc := range []struct{ name, token string }{
		{"the phone's access token", f.Phone.Access.Plaintext},
		{"the laptop's access token", f.Laptop.Access.Plaintext},
	} {
		if _, err := st.Authenticate(ctx, tc.token); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s works again after a reactivation: %v", tc.name, err)
		}
	}
	if _, err := st.RotateRefresh(ctx, f.Phone.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the phone's refresh token rotates again after a reactivation: %v", err)
	}
	if _, err := st.RedeemEnrollment(ctx, f.Pending.Plaintext, "an old invitation"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("an invitation from before the offboard redeems again: %v", err)
	}

	// ONLY A NEW ENROLLMENT GETS IN.
	if _, err := st.RedeemEnrollment(ctx, back.Token.Plaintext, "a new phone"); err != nil {
		t.Errorf("the reactivation's own token does not redeem: %v", err)
	}

	// THE WARN LINE. Any later invite naming Catenary un-offboards, because
	// Purser skips a service only while its account row is active — so the one
	// place an operator can see that their bundle re-run reversed somebody's
	// offboard is this line.
	var recorded map[string]any
	for _, l := range logLines(t, buf)[mark:] {
		if l["msg"] == "person reactivated; an offboard was reversed" {
			recorded = l
		}
	}
	if recorded == nil {
		t.Fatal("the reversal was not logged; an offboard silently undone is the failure ruling 5's " +
			"own consequence paragraph is about")
	}
	if recorded["level"] != "WARN" {
		t.Errorf("logged at %v, want WARN — every other outcome of EnsurePerson is ordinary "+
			"provisioning, and this one undid something somebody did on purpose", recorded["level"])
	}
	if recorded["user_id"] == nil || recorded["devices_revoked"] == nil {
		t.Errorf("the line does not carry the ids and counts a reader needs: %v", recorded)
	}
	for _, l := range logLines(t, buf) {
		for k, v := range l {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if strings.Contains(strings.ToLower(s), f.Email) {
				t.Errorf("the reactivation log carries the email address in field %q", k)
			}
			for _, secret := range []string{back.Token.Plaintext, f.Pending.Plaintext, f.Phone.Access.Plaintext} {
				if strings.Contains(s, secret) {
					t.Errorf("a credential reached the log in field %q", k)
				}
			}
		}
	}
}

func TestAReactivationPublishesTheRevocationForWhatItSwept(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	stray := strayDevice(ctx, t, st, f)

	got := revocationsDuring(ctx, t, pool, func() {
		if _, err := st.EnsurePerson(ctx, f.Email, "Ada"); err != nil {
			t.Errorf("reactivate: %v", err)
		}
	})
	if len(got) != 1 {
		t.Fatalf("%d revocations published by a reactivation that revoked a live device, want 1 — that "+
			"device's socket is attached to some instance right now, and the notification is the only "+
			"thing that severs it", len(got))
	}
	if got[0].UserID == nil || *got[0].UserID != f.UserID {
		t.Errorf("the revocation names %v, want user %s", got[0].UserID, f.UserID)
	}
	_ = stray
}

// A reactivation of an account with nothing live left to revoke publishes
// nothing, on the same terms as a converged offboard.
func TestAReactivationWithNothingLiveToRevokePublishesNothing(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("offboard: %v", err)
	}

	var back EnsuredPerson
	got := revocationsDuring(ctx, t, pool, func() {
		var err error
		if back, err = st.EnsurePerson(ctx, f.Email, "Ada"); err != nil {
			t.Errorf("reactivate: %v", err)
		}
	})
	if back.Outcome != PersonReactivated {
		t.Fatalf("outcome = %q, want reactivated", back.Outcome)
	}
	if len(got) != 0 {
		t.Errorf("%d revocations published while reactivating a fully converged account, want 0 — "+
			"there is no session left to sever, and a notification for one is the re-notify R6 forbids",
			len(got))
	}
}

// THE RACE tokens.go ACCEPTS, DRIVEN, AND THEN REVERSED.
func TestTheReversalRevokesTheDeviceThatEnrolledDuringTheOffboard(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	stray := strayDevice(ctx, t, st, f)

	back, err := st.EnsurePerson(ctx, f.Email, "Ada")
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if back.Outcome != PersonReactivated {
		t.Fatalf("outcome = %q, want reactivated", back.Outcome)
	}

	var revoked *time.Time
	mustScan(t, pool.QueryRow(ctx, `SELECT revoked_at FROM devices WHERE id = $1`, stray.DeviceID), &revoked)
	if revoked == nil {
		t.Error("the stray device is still live after the reactivation")
	}
	if _, err := st.Authenticate(ctx, stray.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the stray device's ACCESS TOKEN works after the reactivation: %v.\n"+
			"This is the device RedeemEnrollment's accepted race leaves behind: the offboard's sweep "+
			"could not see it, and clearing deactivated_at is what would hand it back everything it "+
			"holds.", err)
	}
	if _, err := st.RotateRefresh(ctx, stray.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the stray device's REFRESH TOKEN rotates after the reactivation: %v — which buys a "+
			"fresh access token every fifteen minutes, indefinitely", err)
	}
	// And the socket-severance side of it: the device row is what the hub's gap
	// re-check reads, so a live row would mean a live session survives a
	// listener restart too.
	dead, err := st.DeadDevices(ctx, []uuid.UUID{stray.DeviceID})
	if err != nil {
		t.Fatalf("dead devices: %v", err)
	}
	if len(dead) != 1 {
		t.Errorf("DeadDevices does not name the stray device, so CANT-30's gap re-check would leave its " +
			"session attached")
	}
}

// Criterion 5's negative control: rev 1 of the plan, which relied on the
// offboard having revoked everything. For exactly this device it had not.
func TestWithoutItsSweepTheReversalResurrectsTheStrayDevice(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	stray := strayDevice(ctx, t, st, f)

	st.personGuardFault = personFaultReactivationSkipsSweep
	if _, err := st.EnsurePerson(ctx, f.Email, "Ada"); err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	var revoked *time.Time
	mustScan(t, pool.QueryRow(ctx, `SELECT revoked_at FROM devices WHERE id = $1`, stray.DeviceID), &revoked)
	if revoked != nil {
		t.Fatal("with the reversal's sweep removed the stray device should have survived; this control " +
			"proves nothing")
	}
	if _, err := st.Authenticate(ctx, stray.Access.Plaintext); err != nil {
		t.Fatalf("with the reversal's sweep removed the stray device should AUTHENTICATE again — the "+
			"resurrection this control exists to demonstrate: %v", err)
	}
	// The phone and laptop the offboard did revoke stay revoked either way,
	// which is why "the offboard already revoked everything" reads as true
	// until you look for this one device.
	if _, err := st.Authenticate(ctx, f.Phone.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the phone came back too, which would be a different and larger bug: %v", err)
	}
}

// ONE TRANSACTION, FROM THE OTHER SIDE: the clear cannot commit without the
// sweep, so a failure between them rolls back both.
func TestAFaultBetweenTheReversalsSweepAndItsClearLeavesTheOffboardStanding(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	stray := strayDevice(ctx, t, st, f)

	before := tablesSnapshot(ctx, t, pool)
	st.personGuardFault = personFaultReactivationFailsAfterSweep

	got := revocationsDuring(ctx, t, pool, func() {
		if _, err := st.EnsurePerson(ctx, f.Email, "Ada"); err == nil {
			t.Error("the injected fault did not fail the reactivation")
		}
	})

	if len(got) != 0 {
		t.Errorf("%d revocations published by a rolled-back reactivation, want 0", len(got))
	}
	if after := tablesSnapshot(ctx, t, pool); after != before {
		t.Error("a failed reactivation changed the database.\n" +
			"The sweep, the publish, the clear and the fresh invitation are one transaction, so a " +
			"failure anywhere in them leaves the offboard exactly as it was — rather than an account " +
			"whose devices were revoked but which is still deactivated, or the reverse.")
	}
	var deactivated *time.Time
	mustScan(t, pool.QueryRow(ctx, `SELECT deactivated_at FROM users WHERE id = $1`, f.UserID), &deactivated)
	if deactivated == nil {
		t.Error("the account was reactivated by a call that failed")
	}
	// And the stray device is untouched, which is what makes this the
	// all-or-nothing claim rather than just "the column did not move".
	var revoked *time.Time
	mustScan(t, pool.QueryRow(ctx, `SELECT revoked_at FROM devices WHERE id = $1`, stray.DeviceID), &revoked)
	if revoked != nil {
		t.Error("the sweep's revocation of the stray device survived a rolled-back reactivation")
	}
}

// ---------------------------------------------------------------------------
// Criterion 6, for the reversal — it cannot reach a bot either.

func TestReactivationCannotReachABot(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	botToken, err := st.IssueBotToken(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	// A bot cannot hold an email (0010's CHECK), so the only way to aim
	// EnsurePerson at one at all is to remove the CHECK first — and then
	// deactivate it by hand, since DeactivateUser will not.
	dropBotHasNoEmailCheck(ctx, t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE users SET email = 'argosy@example.com', deactivated_at = now() WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}

	ep, err := st.EnsurePerson(ctx, "argosy@example.com", "Someone")
	if err == nil && (ep.Account.UserID == bot || ep.Outcome == PersonReactivated) {
		t.Fatalf("EnsurePerson reactivated a bot: %+v", ep)
	}
	var deactivated *time.Time
	mustScan(t, pool.QueryRow(ctx, `SELECT deactivated_at FROM users WHERE id = $1`, bot), &deactivated)
	if deactivated == nil {
		t.Error("the bot was re-enabled")
	}
	if _, err := st.Authenticate(ctx, botToken.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Error("the bot's token works again, so something cleared its deactivated_at")
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM enrollment_tokens WHERE user_id = $1`, bot); n != 0 {
		t.Errorf("the bot has %d enrollment tokens, want 0", n)
	}
}

// Criterion 6's negative control for the kind filter, from the REVERSAL's call
// site. persons_test.go already watches it failing for the existing-person
// branch; this is the branch CANT-134 added, and it is the more expensive one
// to get wrong — it CLEARS deactivated_at rather than merely reporting an
// outcome.
func TestWithoutTheKindFilterEnsurePersonWouldReactivateADeactivatedBot(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	botToken, err := st.IssueBotToken(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	dropBotHasNoEmailCheck(ctx, t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE users SET email = 'argosy@example.com', deactivated_at = now() WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}

	st.personGuardFault = personFaultLookupIgnoresKind
	ep, err := st.EnsurePerson(ctx, "argosy@example.com", "Someone")
	if err != nil {
		t.Fatalf("with the filter removed the bot should have been reactivated: %v", err)
	}
	if ep.Account.UserID != bot || ep.Outcome != PersonReactivated {
		t.Fatalf("got %+v, want the bot reactivated — the defect this fault exists to demonstrate", ep)
	}
	var deactivated *time.Time
	mustScan(t, pool.QueryRow(ctx, `SELECT deactivated_at FROM users WHERE id = $1`, bot), &deactivated)
	if deactivated != nil {
		t.Fatal("the bot was not actually re-enabled, so this control shows less than it claims")
	}
	// The bot's own token is revoked by the reversal's sweep and then the
	// account is re-enabled: a service account whose credential Purser just
	// destroyed, which is the shape of the damage.
	if _, err := st.Authenticate(ctx, botToken.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the bot's token survived a sweep that ran over its account: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 7's third clause — a reactivation finds the rooms as they were.

func TestAReactivationFindsTheRoomsAsTheyWere(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", f.UserID, theo)

	sent := send(ctx, t, st, conv, f.UserID, "before the offboard")
	send(ctx, t, st, conv, theo, "and one from theo")
	if _, err := st.MarkRead(ctx, conv, theo, sent.Seq); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	before := membershipSnapshot(ctx, t, pool)
	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("offboard: %v", err)
	}
	if mid := membershipSnapshot(ctx, t, pool); mid != before {
		t.Fatal("the offboard already changed membership; criterion 7's other tests cover that")
	}
	if _, err := st.EnsurePerson(ctx, f.Email, "Ada"); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if after := membershipSnapshot(ctx, t, pool); after != before {
		t.Error("a reactivation changed membership, receipts or messages.\n" +
			"Ruling 7's own wording is that the person is frozen in place and a reactivation finds their " +
			"rooms as they left them — which is only true if neither direction writes that table.")
	}

	// And the rooms are readable again from the reactivated account's own
	// point of view, which is what "finds them" means.
	page, err := st.Sync(ctx, f.UserID, 0, 50)
	if err != nil {
		t.Fatalf("sync as the reactivated person: %v", err)
	}
	if len(page.Conversations) != 1 || page.Conversations[0].ID != conv {
		t.Errorf("the reactivated person syncs %d conversations, want the one they were in", len(page.Conversations))
	}
	if len(page.Messages) != 2 {
		t.Errorf("the reactivated person syncs %d messages, want the 2 that were there", len(page.Messages))
	}
}
