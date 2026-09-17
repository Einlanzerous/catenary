package hub

// CANT-30 — severance, against the in-memory peer.
//
// The claim this file makes is the one in the ticket's `Done when`: a revoked
// device's live socket ends "immediately rather than at its next reconnect",
// and "the other devices are untouched". Both halves need more than one
// session attached to be worth anything — a test with a single peer cannot
// tell "severed the right one" from "severed everything".
//
// THE INDEX IS THE PRECISE ORACLE, NOT THE CLOSE. Attached() is decremented
// under the same lock that selects the doomed sessions, so it is exact the
// moment OnRevocation returns; the Close itself runs in a goroutine per
// session, as the drain and the head-unreadable sever already do, so the
// STATUS is waited for. Asserting only on the close would make "severed
// nothing" and "severed but slow" the same result.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
)

// waitClosed waits for a peer to be closed with a particular status.
func waitClosed(t *testing.T, p *peer, want websocket.StatusCode) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range p.closedWith() {
			if s == want {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("peer %s was not closed with %d; closes = %v", p.user, want, p.closedWith())
}

// stillOpen fails if a peer was closed at all. The window matches peer.nothing's
// — long enough that a sever racing this check would have landed, since the
// close is enqueued on a goroutine before OnRevocation returns.
func stillOpen(t *testing.T, p *peer) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if c := p.closedWith(); len(c) != 0 {
		t.Fatalf("peer %s was closed with %v; it was not the subject of the revocation", p.user, c)
	}
	if n := p.closedNow(); n != 0 {
		t.Fatalf("peer %s was dropped %d times without a handshake", p.user, n)
	}
}

func deviceOf(p *peer) *uuid.UUID {
	d := p.DeviceID()
	return &d
}

func TestARevokedDevicesSessionIsSeveredAndTheOtherDevicesAreUntouched(t *testing.T) {
	f := newFixture(t)
	phone := f.attach(f.ada)
	laptop := f.attach(f.ada)
	theo := f.attach(f.theo)
	if n := f.hub.Attached(); n != 3 {
		t.Fatalf("precondition: %d sessions attached, want 3", n)
	}

	f.hub.OnRevocation(context.Background(), store.RevocationPayload{DeviceID: deviceOf(phone)})

	// Exact, and synchronous: exactly one session left the index.
	if n := f.hub.Attached(); n != 2 {
		t.Errorf("%d sessions attached after revoking one device, want 2 — a revocation that "+
			"severed more than its subject has logged out devices nobody revoked", n)
	}
	waitClosed(t, phone, StatusRevoked)
	// The person's OTHER device, and a different person entirely, are untouched.
	// This is the half of the `Done when` a single-peer test cannot make.
	stillOpen(t, laptop)
	stillOpen(t, theo)
}

func TestADeactivatedUsersSessionsAreAllSevered(t *testing.T) {
	f := newFixture(t)
	phone := f.attach(f.ada)
	laptop := f.attach(f.ada)
	theo := f.attach(f.theo)

	ada := f.ada
	f.hub.OnRevocation(context.Background(), store.RevocationPayload{UserID: &ada})

	if n := f.hub.Attached(); n != 1 {
		t.Errorf("%d sessions attached after deactivating an account, want 1", n)
	}
	waitClosed(t, phone, StatusRevoked)
	waitClosed(t, laptop, StatusRevoked)
	stillOpen(t, theo)
}

// The payload carries a device OR a user, so neither is a publisher this hub
// can act on — and it must say so rather than return quietly.
func TestARevocationWithNoSubjectIsIgnoredAndSaidSo(t *testing.T) {
	f := newFixture(t)
	ada := f.attach(f.ada)

	f.hub.OnRevocation(context.Background(), store.RevocationPayload{})

	if n := f.hub.Attached(); n != 1 {
		t.Errorf("%d sessions attached, want 1 — a subjectless revocation severed something", n)
	}
	stillOpen(t, ada)
	var warned bool
	for _, rec := range f.log.atLeast(0) {
		if rec.Message == "revocation with no subject" {
			warned = true
		}
	}
	if !warned {
		t.Error("a revocation with neither subject set was dropped silently; a publisher writing " +
			"the wrong shape would never be found")
	}
}

// --- the gap ---------------------------------------------------------------

// A revocation has no cursor, so a missed notification is closed by asking the
// database about the sessions this instance actually holds.
func TestARevocationGapSeversOnlyTheSessionsTheDatabaseSaysAreDead(t *testing.T) {
	f := newFixture(t)
	phone := f.attach(f.ada)
	laptop := f.attach(f.ada)
	theo := f.attach(f.theo)

	var asked []uuid.UUID
	dead := phone.DeviceID()
	// Set AFTER the attaches, so the attach re-check each of them ran answered
	// "nothing is dead" and only the gap's own call reaches this.
	f.st.deadDevices = func(_ context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
		asked = append([]uuid.UUID(nil), ids...)
		return []uuid.UUID{dead}, nil
	}

	f.hub.OnRevocationGap(context.Background())

	if n := f.hub.Attached(); n != 2 {
		t.Errorf("%d sessions attached after the gap re-check, want 2", n)
	}
	waitClosed(t, phone, StatusRevoked)
	stillOpen(t, laptop)
	stillOpen(t, theo)

	// Every attached device was offered for checking. One that was not would be
	// a session this instance keeps serving with no second chance to notice.
	if len(asked) != 3 {
		t.Errorf("the re-check was asked about %d devices, want all 3 attached: %v", len(asked), asked)
	}
}

// THE IMPORTANT NEGATIVE: a failed re-check must not tell healthy clients to
// stop reconnecting.
func TestARevocationGapThatCannotBeReadSeversForReconnectAndNotAsRevoked(t *testing.T) {
	f := newFixture(t)
	ada := f.attach(f.ada)
	theo := f.attach(f.theo)

	f.st.deadDevices = func(context.Context, []uuid.UUID) ([]uuid.UUID, error) {
		return nil, errors.New("the database is not answering")
	}

	f.hub.OnRevocationGap(context.Background())

	if n := f.hub.Attached(); n != 0 {
		t.Errorf("%d sessions still attached after an unreadable re-check, want 0 — this instance "+
			"cannot say which of them is revoked, and the door re-authenticates on reconnect", n)
	}
	for _, p := range []*peer{ada, theo} {
		waitClosed(t, p, websocket.StatusServiceRestart)
		for _, s := range p.closedWith() {
			if s == StatusRevoked {
				t.Errorf("a session was closed with StatusRevoked after a FAILED re-check. "+
					"Nothing here knows it was revoked, and %d is terminal by CANT-35's table — "+
					"this would tell every healthy client on the instance never to come back",
					int(StatusRevoked))
			}
		}
	}
	var loud bool
	for _, rec := range f.log.atLeast(0) {
		if rec.Message == "revoked-device re-check failed after a gap; sessions severed for reconnect" {
			loud = true
		}
	}
	if !loud {
		t.Error("the failed re-check was not logged at all")
	}
}

// An instance holding nothing asks nothing: the query is per-instance and the
// round trip is pointless when there are no sessions to check.
func TestARevocationGapWithNoSessionsAsksTheDatabaseNothing(t *testing.T) {
	f := newFixture(t)
	f.hub.OnRevocationGap(context.Background())
	if n := f.st.deadChecks.Load(); n != 0 {
		t.Errorf("the re-check ran %d times with no sessions attached, want 0", n)
	}
}

// --- the window between the door and the index -------------------------------
//
// Found by the reviewer on PR #63. OnRevocation is edge-triggered and can only
// close what is in the index when the notification arrives; the door holds a
// socket for up to the hello timeout before attaching it. A revocation landing
// in that window severs nothing, is not queued, and never comes again.

func TestASessionWhoseCredentialDiedInTheDoorIsSeveredAtAttach(t *testing.T) {
	f := newFixture(t)
	// Everything asked about is dead. The peer's device id is minted inside
	// attach, so the fake answers the QUESTION rather than a particular id.
	f.st.deadDevices = func(_ context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
		return ids, nil
	}

	p := f.attach(f.ada)

	if n := f.hub.Attached(); n != 0 {
		t.Errorf("%d sessions attached, want 0. A device revoked between the door and the "+
			"index streams for the life of the socket — which is the exact sentence "+
			"OnRevocation's own comment says this feature exists to prevent", n)
	}
	waitClosed(t, p, StatusRevoked)
	if n := f.st.deadChecks.Load(); n != 1 {
		t.Errorf("the attach re-check ran %d times, want exactly 1 per attach", n)
	}
}

func TestALiveDeviceAttachesNormallyAndIsCheckedExactlyOnce(t *testing.T) {
	f := newFixture(t)
	p := f.attach(f.ada)

	if n := f.hub.Attached(); n != 1 {
		t.Fatalf("%d sessions attached, want 1", n)
	}
	stillOpen(t, p)
	// One indexed read per socket open, and no more: this is on the path of
	// every connection.
	if n := f.st.deadChecks.Load(); n != 1 {
		t.Errorf("the attach re-check ran %d times for one attach, want exactly 1", n)
	}
}

// THE ASYMMETRY WITH THE GAP, AND IT IS DELIBERATE. A failed re-check HERE
// allows the session; a failed re-check at a gap severs everything. The door
// authenticated this device seconds ago, so the exposure is one hello timeout,
// while refusing on a failed read would turn a momentary database blip into
// "nobody can open a socket at all".
func TestAFailedAttachReCheckAllowsTheSessionAndSaysSo(t *testing.T) {
	f := newFixture(t)
	f.st.deadDevices = func(context.Context, []uuid.UUID) ([]uuid.UUID, error) {
		return nil, errors.New("the database is not answering")
	}

	p := f.attach(f.ada)

	if n := f.hub.Attached(); n != 1 {
		t.Errorf("%d sessions attached, want 1 — a database blip must not stop every socket "+
			"in the estate from opening", n)
	}
	stillOpen(t, p)
	var warned bool
	for _, rec := range f.log.atLeast(0) {
		if rec.Message == "attach liveness re-check failed; session allowed" {
			warned = true
		}
	}
	if !warned {
		t.Error("a failed attach re-check was silent; a persistent failure would have to be " +
			"inferred rather than read")
	}
}

// A payload carrying BOTH subjects names each session once. CANT-33's
// deprovision is the natural producer — revoking a person's device and
// deactivating their account is one action.
func TestAPayloadCarryingBothSubjectsSeversEachSessionOnce(t *testing.T) {
	f := newFixture(t)
	p := f.attach(f.ada)
	ada := f.ada
	device := p.DeviceID()

	f.hub.OnRevocation(context.Background(), store.RevocationPayload{UserID: &ada, DeviceID: &device})

	if n := f.hub.Attached(); n != 0 {
		t.Errorf("%d sessions attached, want 0", n)
	}
	waitClosed(t, p, StatusRevoked)
	if n := len(p.closedWith()); n != 1 {
		t.Errorf("the session was closed %d times, want 1. removeLocked is idempotent so the "+
			"COUNT stays right either way — it is sessions_severed, the telemetry this feature "+
			"is read through, that a double match quietly inflates", n)
	}
}
