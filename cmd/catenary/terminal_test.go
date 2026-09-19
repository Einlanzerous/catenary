package main

// CANT-123 against the real router and a real database: every close the real
// door can be made to produce, and the HTTP route a revoked device takes, end
// the way CANT-31's record says. internal/client's own tests script the closes
// the server cannot be asked for on demand; here the server picks the code and
// the client has to agree with it — which is the divergence this guards.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// terminalClient builds (and does not run) a client over j with faults.
func (k *killRig) terminalClient(dev wire.EnrollResponse, j *client.Journal, refresh bool, faults client.Faults) *client.Client {
	k.t.Helper()
	if _, held := j.Credential(); !held {
		cred, err := client.CredentialFromEnroll(dev)
		if err != nil {
			k.t.Fatal(err)
		}
		if err := j.Enroll(cred); err != nil {
			k.t.Fatal(err)
		}
	}
	c, err := client.New(client.Config{
		BaseURL: k.base(), Journal: j, ClientInfo: "cant-123-test", Refresh: refresh, Faults: faults,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 250 * time.Millisecond,
	})
	if err != nil {
		k.t.Fatal(err)
	}
	return c
}

// run starts c and returns the channel Run's error arrives on. The client is
// killed at the end of the test if it is still going.
func (k *killRig) run(c *client.Client) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Run(k.ctx) }()
	k.t.Cleanup(func() { c.Kill() })
	return done
}

func awaitTerminal(t *testing.T, done <-chan error, bound time.Duration) client.Terminal {
	t.Helper()
	select {
	case err := <-done:
		var te *client.TerminalError
		if !errors.As(err, &te) {
			t.Fatalf("Run returned %v, want a TerminalError", err)
		}
		return te.Terminal
	case <-time.After(bound):
		t.Fatal("the client never stopped")
		return client.Terminal{}
	}
}

func rawFrame(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// EVERY PERMANENT CLOSE THE REAL DOOR PRODUCES STOPS THE CLIENT — and with the
// terminal branch faulted out, the same misbehaving client dials for ever,
// which is the failure this replaces.
func TestTheRealDoorsPermanentClosesStopTheClient(t *testing.T) {
	k := newKillRig(t, nil)
	cs := k.cast()
	other := k.enroll(cs.ada, "ada's laptop")

	type tcase struct {
		name       string
		faults     func(dev wire.EnrollResponse) client.Faults
		wantReason string
	}
	hello := func(dev wire.Uuid, version int64) map[string]any {
		return map[string]any{"type": "hello", "wire_version": version, "device_id": dev}
	}
	cases := []tcase{
		{"a malformed frame — bare 1008",
			func(wire.EnrollResponse) client.Faults { return client.Faults{AfterReady: []byte(`{"type":"send",`)} }, "bare"},
		{"a second hello — bare 1008",
			func(d wire.EnrollResponse) client.Faults {
				return client.Faults{AfterReady: rawFrame(t, hello(d.DeviceID, int64(wire.WireVersion)))}
			}, "bare"},
		{"a first frame that is not a hello — bare 1008",
			func(wire.EnrollResponse) client.Faults {
				return client.Faults{HelloInstead: rawFrame(t, map[string]any{"type": "ping", "id": "too-early"})}
			}, "bare"},
		{"a hello naming another device — 1008 after error{unauthorized}",
			func(wire.EnrollResponse) client.Faults {
				return client.Faults{HelloInstead: rawFrame(t, hello(other.DeviceID, int64(wire.WireVersion)))}
			}, "error{unauthorized}"},
		{"a wire version the server does not speak — 1008 after error{wire_version_unsupported}",
			func(d wire.EnrollResponse) client.Faults {
				return client.Faults{HelloInstead: rawFrame(t, hello(d.DeviceID, int64(wire.WireVersion)+1))}
			}, "error{wire_version_unsupported}"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := k.enroll(cs.theo, "theo's phone "+uuid.NewString()[:8])
			j := client.NewJournal()
			c := k.terminalClient(dev, j, false, tc.faults(dev))
			term := awaitTerminal(t, k.run(c), 10*time.Second)
			if term.Kind != client.TerminalProtocol || !strings.Contains(term.Reason, tc.wantReason) {
				t.Errorf("terminal %+v, want a protocol terminal whose reason names %q", term, tc.wantReason)
			}
			s := c.Status()
			if s.Dials != 1 || s.CloseStatuses[1008] != 1 {
				t.Errorf("dials %d, close statuses %v; want one dial ended by one 1008", s.Dials, s.CloseStatuses)
			}
			if s.Terminal != term {
				t.Errorf("Status names %+v, Run returned %+v", s.Terminal, term)
			}
			// TERMINAL DELETES NOTHING, so a false one is recovered by a
			// relaunch — and A PROTOCOL TERMINAL ENDS ON RELAUNCH: a new
			// client over the same journal, without the bug, gets in.
			if _, held := j.Credential(); !held {
				t.Fatal("going terminal deleted the stored credential")
			}
			relaunched := k.terminalClient(dev, j, false, client.Faults{})
			k.run(relaunched)
			awaitClient(t, relaunched, "the relaunch gets in", func() bool { s := relaunched.Status(); return s.Ready && s.CaughtUp })

			// NEGATIVE CONTROL, once — on the malformed frame, as the
			// criterion asks: terminal branch removed, the client redials.
			if i == 0 {
				looping := k.terminalClient(k.enroll(cs.theo, "theo's tablet"), client.NewJournal(), false,
					client.Faults{AfterReady: []byte(`{"type":"send",`), NeverTerminal: true})
				k.run(looping)
				awaitClient(t, looping, "the control dials again and again", func() bool { return looping.Status().Dials >= 3 })
				if got := looping.Status().CloseStatuses[1008]; got < 2 {
					t.Errorf("the control saw %d bare 1008s; it should have walked into the same close each time", got)
				}
			}
		})
	}
}

// THE HELLO TIMEOUT IS TRANSIENT, AND THE TWO SIDES AGREE ON ITS NUMBER. A
// first frame of an unknown type is ignored by the door, which goes on waiting
// for a hello and closes 4002 at its deadline (CANT-122). Before that ticket
// this was a bare 1008, and this client would have stopped on it. It waits out
// the door's real ten-second deadline, once, because the number is the contract
// between a server constant and a client table and nothing else checks both.
func TestTheHelloTimeoutReconnects(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the door's real hello deadline")
	}
	k := newKillRig(t, nil)
	cs := k.cast()
	c := k.terminalClient(k.enroll(cs.theo, "theo's phone"), client.NewJournal(), false,
		client.Faults{HelloInstead: []byte(`{"type":"a-frame-from-the-future"}`)})
	done := k.run(c)

	ctx, cancel := context.WithTimeout(k.ctx, 25*time.Second)
	defer cancel()
	if err := c.Await(ctx, func() bool { return c.Status().Dials >= 2 }); err != nil {
		select {
		case rerr := <-done:
			t.Fatalf("the client stopped on the hello timeout: %v; close statuses %v", rerr, c.Status().CloseStatuses)
		default:
			t.Fatalf("the client never redialed: %v; status %+v", err, c.Status())
		}
	}
	s := c.Status()
	if s.CloseStatuses[4002] == 0 {
		t.Errorf("close statuses %v; want the hello timeout's 4002 — a 1008 here means the server and the record disagree", s.CloseStatuses)
	}
	if s.Terminal.Kind != client.NotTerminal {
		t.Errorf("a transient close left the client terminal: %+v", s.Terminal)
	}
}

// 4001: REVOKED WHILE CONNECTED. The hub severs the live session (CANT-30) and
// the client stops instead of dialing a door that will never let it in.
func TestARevokedLiveSessionStopsOn4001(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults client.Faults
		stops  bool
	}{
		{"it stops", client.Faults{}, true},
		{"negative control — terminal branch removed: it dials for ever", client.Faults{NeverTerminal: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKillRig(t, nil)
			// The kill rig runs the message listener only; a live session is
			// severed by the OTHER one, on CANT-30's revocation channel.
			runRevocations(&rig{t: t, ctx: k.ctx, pool: k.pool, st: k.st, d: k.proc.d, since: dbNow(k.ctx, t, k.pool)})
			cs := k.cast()
			dev := k.enroll(cs.theo, "theo's phone")
			c := k.terminalClient(dev, client.NewJournal(), false, tc.faults)
			done := k.run(c)
			awaitClient(t, c, "ready", func() bool { s := c.Status(); return s.Ready && s.CaughtUp })

			if revoked, err := k.st.RevokeDevice(k.ctx, uuid.MustParse(string(dev.DeviceID))); err != nil || !revoked {
				t.Fatalf("revoke: %v, %v", revoked, err)
			}
			if !tc.stops {
				awaitClient(t, c, "the control redials a door that refuses it", func() bool { s := c.Status(); return s.Dials >= 3 && s.DialErrors >= 2 })
				return
			}
			term := awaitTerminal(t, done, 10*time.Second)
			if term.Kind != client.TerminalProtocol || !strings.Contains(term.Reason, "4001") {
				t.Errorf("terminal %+v, want a protocol terminal naming 4001", term)
			}
			if s := c.Status(); s.Dials != 1 || s.CloseStatuses[4001] != 1 {
				t.Errorf("dials %d, close statuses %v; want one dial ended by one 4001", s.Dials, s.CloseStatuses)
			}
		})
	}
}

// REVOKED WHILE DISCONNECTED — THE COMMON CASE (plan criterion 14). The device
// never sees a close code. Its upgrade is refused, the /sync beside it is
// refused, the reactive refresh that answers that is refused by Catenary
// itself with the stored pair still the one presented — and the client stops.
//
// Then the exits (criterion 9): it is RE-CHECKED ONCE ON RELAUNCH, by simply
// running, and stops again having asked exactly once more; nothing was deleted
// (criterion 13); and only a re-enrollment ends it.
func TestADeviceRevokedWhileOfflineStopsOnTheHTTPRoute(t *testing.T) {
	k := newKillRig(t, nil)
	cs := k.cast()
	dev := k.enroll(cs.theo, "theo's phone")
	j := client.NewJournal()

	// A first life, so the journal has a store worth not deleting.
	l := &ledger{}
	ada := k.client(k.enroll(cs.ada, "ada's laptop"), nil, client.Faults{})
	first := k.terminalClient(dev, j, true, client.Faults{})
	firstDone := k.run(first)
	awaitClient(t, ada, "ada ready", func() bool { s := ada.Status(); return s.Ready && s.CaughtUp })
	awaitClient(t, first, "theo ready", func() bool { s := first.Status(); return s.Ready && s.CaughtUp })
	ack := l.socket(t, ada, "ada", cs.a, "before the revocation")
	awaitClient(t, first, "theo holds it", func() bool { return first.Holds(ack.MessageID) })
	first.Kill()
	<-firstDone
	held, _ := j.Credential()
	before := j.Snapshot()

	if revoked, err := k.st.RevokeDevice(k.ctx, uuid.MustParse(string(dev.DeviceID))); err != nil || !revoked {
		t.Fatalf("revoke: %v, %v", revoked, err)
	}

	for life := 1; life <= 2; life++ {
		c := k.terminalClient(dev, j, true, client.Faults{})
		term := awaitTerminal(t, k.run(c), 10*time.Second)
		if term.Kind != client.TerminalCredential {
			t.Fatalf("life %d: terminal %+v, want a credential terminal", life, term)
		}
		s := c.Status()
		if len(s.CloseStatuses) != 0 {
			t.Errorf("life %d: close statuses %v; this route must not depend on ever seeing a close code", life, s.CloseStatuses)
		}
		if s.RefreshErrors != 1 || s.Refreshes != 0 {
			t.Errorf("life %d: %d refused refreshes, %d rotations; a relaunch re-checks exactly ONCE", life, s.RefreshErrors, s.Refreshes)
		}
		// NEITHER ENDS ON A TIMER OR A NETWORK CHANGE: Run has returned, and
		// many backoffs later the client has not dialed again.
		dials := s.Dials
		c.CatchUp()
		c.Sever()
		time.Sleep(600 * time.Millisecond)
		if got := c.Status().Dials; got != dials {
			t.Errorf("life %d: a terminal client dialed again (%d→%d)", life, dials, got)
		}
	}

	// NOTHING WAS DELETED.
	if now, ok := j.Credential(); !ok || now != held {
		t.Error("going terminal changed or deleted the stored credential")
	}
	after := j.Snapshot()
	if len(after.Messages) != len(before.Messages) || len(after.Messages) == 0 || after.Cursor != before.Cursor {
		t.Errorf("going terminal changed the local store: %d→%d messages, cursor %d→%d",
			len(before.Messages), len(after.Messages), before.Cursor, after.Cursor)
	}

	// ONLY RE-ENROLLMENT ENDS IT: a new device for the same person, enrolled
	// into the same journal, keeps the store and gets in.
	fresh, err := client.CredentialFromEnroll(k.enroll(cs.theo, "theo's phone, re-enrolled"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Reenroll(fresh); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	back := k.terminalClient(dev, j, true, client.Faults{})
	k.run(back)
	awaitClient(t, back, "the re-enrolled device gets in", func() bool { s := back.Status(); return s.Ready && s.CaughtUp })
	if !back.Holds(ack.MessageID) {
		t.Error("re-enrollment lost the local store")
	}
}
