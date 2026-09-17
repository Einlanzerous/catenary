package main

// CANT-30's `Done when`, end to end: the REAL router setup() builds, the real
// store on Postgres, the real revocation listener on its own connection, and
// real sockets.
//
// THE CLAIM IS THE ONE THE PERSON CLICKING THE BUTTON BELIEVES THEY MADE.
// internal/hub proves the severance logic against an in-memory peer; what that
// cannot prove is that a revocation written by the store actually crosses
// Postgres, reaches another component's listener, and ends a socket that is
// held open on the other side of a real WebSocket. Under CANT-28 ruling 2 a
// session is authorized once at accept and outlives its access token, so
// nothing about `store.RevokeDevice` reaches a live socket by itself — this
// whole path is what makes "immediately rather than at its next reconnect"
// true, and every link in it is somewhere it could silently not be.
//
// ITS OWN LISTENER, STARTED HERE. newRig runs the MESSAGE listener only, and
// its waitForListen counts LISTEN backends without distinguishing channels —
// killrig_test.go builds a rig running exactly one, so raising that count
// globally would hang it. This file waits for its own channel by name instead,
// which is both narrower and the thing this test actually depends on.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/hub"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// runRevocations starts the rig's revocation listener and waits until it is
// registered, on the same terms rig.runListener uses for the message one.
func runRevocations(r *rig) {
	r.t.Helper()
	if r.d.revocations == nil {
		r.t.Fatal("setup() built no revocation listener — CANT-30's channel is unconsumed, " +
			"and a revoked device's socket would live until it happened to drop")
	}
	lctx, stop := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.d.revocations.Run(lctx)
	}()
	r.t.Cleanup(func() { stop(); <-done })

	// BY CHANNEL NAME, not by a count. A revocation committed before this
	// listener registers is gone — Postgres queues nothing for a listener that
	// is not there — so a wait that matched the message listener's backend
	// would let this test race exactly the hole the feature exists to close.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := r.pool.QueryRow(r.ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event = 'ClientRead'
			   AND query ILIKE '%' || $1 || '%'
			   AND backend_start >= $2`, store.RevocationChannel, r.since).Scan(&n); err != nil {
			r.t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the %s listener did not register within the deadline", store.RevocationChannel)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// deviceID parses the id the enrollment response carried. wire.Uuid is a
// string type the generated decoder has already validated against the schema's
// pattern, so a failure here is a generator bug rather than input.
func deviceID(t *testing.T, id wire.Uuid) uuid.UUID {
	t.Helper()
	parsed, err := uuid.Parse(string(id))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// The whole of the `Done when`, over real sockets: the revoked device's socket
// ends at once and with the terminal code, and nothing else is disturbed.
func TestARevokedDevicesSocketIsSeveredImmediatelyAndTheOtherDevicesAreNot(t *testing.T) {
	r := newRig(t)
	runRevocations(r)

	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada Lovelace")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")

	// Two devices for one person, so "the other devices are untouched" is a
	// claim this test can actually make — and a third socket on a different
	// account, so it also covers "somebody else's".
	phoneCred := r.enroll(ada, "Pixel 8 Pro")
	laptopCred := r.enroll(ada, "Framework 13")
	phone := openAt(t, r.ctx, r.base, phoneCred, ada)
	laptop := openAt(t, r.ctx, r.base, laptopCred, ada)
	theoCred := r.enroll(theo, "Theo's phone")
	theoSession := openAt(t, r.ctx, r.base, theoCred, theo)

	// Everything is live before the revocation. Without this the test could
	// pass against a build where the sockets were never open.
	phone.pingPong()
	laptop.pingPong()
	theoSession.pingPong()

	revoked, err := r.st.RevokeDevice(r.ctx, deviceID(t, phoneCred.DeviceID))
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !revoked {
		t.Fatal("RevokeDevice reported nothing to do on a live device")
	}

	// The socket ends, with the terminal code and not a transient one. A client
	// that reconnects on 4001 is the failure CANT-31 names.
	if code := phone.expectClose(); code != hub.StatusRevoked {
		t.Errorf("the revoked device's socket closed with %d, want %d (StatusRevoked). "+
			"A revocation that does not reach the socket leaves the phone receiving every "+
			"message in every room until it happens to drop", code, hub.StatusRevoked)
	}

	// And the person's other device, and a different person, carry on. These
	// are round trips through each socket's own read goroutine, so a severance
	// that had reached them would fail here rather than go unnoticed.
	laptop.pingPong()
	theoSession.pingPong()
}

// The gap: a revocation raised while this instance's listener was down is gone
// with no record of it, and there is no cursor to replay. The re-check on
// reconnect is the only thing that closes it.
func TestARevocationMissedWhileTheListenerWasDownIsCaughtOnReconnect(t *testing.T) {
	r := newRig(t)

	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada Lovelace")
	phoneCred := r.enroll(ada, "Pixel 8 Pro")
	laptopCred := r.enroll(ada, "Framework 13")
	phone := openAt(t, r.ctx, r.base, phoneCred, ada)
	laptop := openAt(t, r.ctx, r.base, laptopCred, ada)
	phone.pingPong()
	laptop.pingPong()

	// The revocation happens with NO revocation listener running at all, which
	// is exactly what an instance whose listener was reconnecting would see:
	// the notification is raised and delivered to nobody.
	if _, err := r.st.RevokeDevice(r.ctx, deviceID(t, phoneCred.DeviceID)); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The listener now starts. Its OnGap fires on the first subscription only
	// if it has connected before, so the re-check is driven directly here —
	// the store-backed half of it is what this test is about, and notify.go's
	// own tests own the reconnect-raises-a-gap half.
	r.d.hub.OnRevocationGap(r.ctx)

	if code := phone.expectClose(); code != hub.StatusRevoked {
		t.Errorf("a revocation raised while the listener was down left the socket open: "+
			"close = %d, want %d. Postgres queues nothing for a disconnected listener and a "+
			"revocation has no cursor, so the re-check is the only thing that would ever "+
			"notice", code, hub.StatusRevoked)
	}
	laptop.pingPong()
}

// The listener is wired into the composition root rather than only being
// constructible: a deps with no revocation listener consumes nothing.
func TestSetupWiresTheRevocationListener(t *testing.T) {
	_, _, _, d := processFixture(t, slog.New(slog.DiscardHandler))
	if d.revocations == nil {
		t.Fatal("setup() built no revocation listener")
	}
	if d.revocations.Channel != store.RevocationChannel {
		t.Errorf("the revocation listener is on %q, want %q",
			d.revocations.Channel, store.RevocationChannel)
	}
	if d.listener.Channel == d.revocations.Channel {
		t.Error("both listeners are on one channel. CANT-28 ruling 7 chose a second channel " +
			"precisely because a revocation and a message decode DIFFERENTLY on a parse " +
			"failure — a misrouted payload fails silently rather than erroring")
	}
	// A revocation carries no close code of its own on the wire, so the only
	// thing that tells a client why it was severed is the status. Pinned here
	// so a change to it is deliberate.
	if hub.StatusRevoked != websocket.StatusCode(4001) {
		t.Errorf("StatusRevoked = %d, want 4001 — CANT-35's table names it", int(hub.StatusRevoked))
	}
}
