package main

// CANT-134 criterion 4 — deactivation severs every live socket, through the
// REAL router setup() builds, the real store on Postgres, the real revocation
// listener on its own connection, and real sockets.
//
// THIS IS revocation_test.go's CLAIM FOR THE OTHER SUBJECT, and it is not the
// same claim. That file proves a DEVICE revocation crosses Postgres and ends
// one socket; the hub's user branch has been built and tested since CANT-30 but
// had no publisher at all until CANT-134, so nothing anywhere had ever run the
// account path end to end. Under CANT-28 ruling 2 a session is authorized once
// at accept and outlives its access token, so `store.DeactivateUser` reaches a
// live socket only through this chain — and every link in it is somewhere it
// could silently not be.
//
// THE OTHER THREE DOORS ARE HERE TOO, over HTTP, because "deprovisioned" has to
// mean all four at once: a third device cannot refresh, a previously issued
// invitation cannot enroll, and an access token cannot authenticate.

import (
	"errors"
	"net/http"
	"testing"

	"github.com/magos/catenary/internal/hub"
	"github.com/magos/catenary/internal/store"
)

func TestDeactivatingAPersonSeversEveryDeviceAndShutsEveryDoor(t *testing.T) {
	r := newRig(t)
	runRevocations(r)

	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada Lovelace")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")

	// TWO of Ada's devices on live sockets, so "every device, at once" is a
	// claim this test can make rather than one it demonstrates for a sample of
	// one. A THIRD device, enrolled and not attached, is what the refusal at
	// /refresh is about — a phone that was offline during the offboard and comes
	// back an hour later. And a socket on a DIFFERENT account, because an
	// account-wide revocation is exactly the shape that could take the estate
	// down with it.
	phoneCred := r.enroll(ada, "Pixel 8 Pro")
	laptopCred := r.enroll(ada, "Framework 13")
	offlineCred := r.enroll(ada, "the tablet in a drawer")
	phone := openAt(t, r.ctx, r.base, phoneCred, ada)
	laptop := openAt(t, r.ctx, r.base, laptopCred, ada)
	theoCred := r.enroll(theo, "Theo's phone")
	theoSession := openAt(t, r.ctx, r.base, theoCred, theo)

	// An invitation issued and never redeemed — the mailbox link an offboard
	// has to withdraw.
	pending, err := r.st.IssueEnrollmentToken(r.ctx, ada)
	if err != nil {
		t.Fatal(err)
	}

	// Everything is live before the offboard. Without this the test could pass
	// against a build where the sockets were never open.
	phone.pingPong()
	laptop.pingPong()
	theoSession.pingPong()
	if code, body := do(t, r.d.router, refreshRequest(t, string(offlineCred.RefreshToken))); code != http.StatusOK {
		t.Fatalf("precondition: the offline device should be able to refresh: %d %s", code, body)
	}

	out, err := r.st.DeactivateUser(r.ctx, ada)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if !out.Changed() || out.Revoked.Devices != 3 {
		t.Fatalf("the offboard reported %+v, want three devices revoked", out)
	}

	// BOTH SOCKETS END, with the terminal code and not a transient one. A client
	// that reconnects on 4001 is the failure CANT-31 names; a client that is
	// never told at all is the failure this ticket closes.
	for _, tc := range []struct {
		name string
		s    *session
	}{{"the phone", phone}, {"the laptop", laptop}} {
		if code := tc.s.expectClose(); code != hub.StatusRevoked {
			t.Errorf("%s closed with %d, want %d (StatusRevoked). A deactivation that does not reach the "+
				"socket leaves a deprovisioned person receiving every message in every room they were "+
				"in until the connection happens to drop", tc.name, code, hub.StatusRevoked)
		}
	}

	// Somebody else's session is untouched, which is what makes the account
	// subject a scalpel rather than a restart.
	theoSession.pingPong()

	// THE THIRD DOOR: the device that was offline. Its refresh token was
	// revoked by the sweep, and users.deactivated_at closes the route besides.
	if code, body := do(t, r.d.router, refreshRequest(t, string(offlineCred.RefreshToken))); code != http.StatusUnauthorized {
		t.Errorf("POST /refresh for a deprovisioned person = %d, want 401: %s", code, body)
	}

	// THE FOURTH: the invitation in their mailbox.
	if code, body := do(t, r.d.router, enrollRequest(t, pending.Plaintext, "a device that must not exist")); code != http.StatusUnauthorized {
		t.Errorf("POST /enroll with an invitation issued before the offboard = %d, want 401: %s", code, body)
	}

	// And the credential seam itself, which is what every other route is built
	// on.
	if _, err := r.st.Authenticate(r.ctx, string(phoneCred.AccessToken)); !errors.Is(err, store.ErrUnauthorized) {
		t.Errorf("Authenticate still accepts a deprovisioned person's access token: %v", err)
	}
}

// The gap, for the account subject. Postgres queues nothing for a disconnected
// listener and a revocation has no cursor to resync from, so an offboard raised
// while this instance's listener was down is gone with no record of it — and
// the sessions it named are still being served. The re-check on reconnect is
// the only thing that closes it, and it has to reach a DEACTIVATED account and
// not only a revoked device.
func TestAnOffboardMissedWhileTheListenerWasDownIsCaughtOnReconnect(t *testing.T) {
	r := newRig(t)

	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada Lovelace")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	phone := openAt(t, r.ctx, r.base, r.enroll(ada, "Pixel 8 Pro"), ada)
	theoSession := openAt(t, r.ctx, r.base, r.enroll(theo, "Theo's phone"), theo)
	phone.pingPong()
	theoSession.pingPong()

	// No revocation listener is running at all — runRevocations is deliberately
	// not called — so the notification is delivered to nobody, exactly as an
	// instance whose listener was reconnecting would see it.
	if _, err := r.st.DeactivateUser(r.ctx, ada); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	r.d.hub.OnRevocationGap(r.ctx)

	if code := phone.expectClose(); code != hub.StatusRevoked {
		t.Errorf("an offboard raised while the listener was down left the socket open: close = %d, "+
			"want %d. store.DeadDevices is what the re-check asks, and the offboard's devices sweep is "+
			"what makes it answer", code, hub.StatusRevoked)
	}
	theoSession.pingPong()
}
