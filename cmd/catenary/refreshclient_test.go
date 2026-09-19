package main

// CANT-124 against the real router and a real database: the reference client
// refreshes its own pair — before it needs to, and when it is refused — and
// does it once per credential however many clients hold it.
//
// internal/client's own tests pin the rules against a stub that mints pairs.
// Here the server is the judge: an access token is expired in Postgres, and
// what gets back in is whatever the real Authenticate and the real
// RotateRefresh say gets back in.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// refreshingClient is runClient with Config.Refresh on and, optionally, a
// device clock that is not the real one. start=false builds without running.
func (k *killRig) refreshingClient(dev wire.EnrollResponse, j *client.Journal, refresh bool, now func() time.Time, start bool) *client.Client {
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
		BaseURL: k.base(), Journal: j, ClientInfo: "cant-124-test",
		Refresh: refresh, Now: now,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 250 * time.Millisecond,
	})
	if err != nil {
		k.t.Fatal(err)
	}
	if !start {
		return c
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(k.ctx)
	}()
	k.t.Cleanup(func() {
		c.Kill()
		<-done
	})
	return c
}

// expireAccess makes every access token the device holds already expired, as
// the database sees it.
func (k *killRig) expireAccess(device wire.Uuid) {
	k.t.Helper()
	tag, err := k.pool.Exec(k.ctx,
		`UPDATE access_tokens SET expires_at = now() - interval '1 minute' WHERE device_id = $1 AND expires_at > now()`, string(device))
	if err != nil {
		k.t.Fatal(err)
	}
	if tag.RowsAffected() == 0 {
		k.t.Fatal("no live access token to expire — the test expired nothing")
	}
}

// deviceHealth is what reuse detection would have changed: how many refresh
// tokens the device's family holds, how many of them are revoked, and whether
// the device itself is.
func (k *killRig) deviceHealth(device wire.Uuid) (tokens, revokedTokens int, deviceRevoked bool) {
	k.t.Helper()
	if err := k.pool.QueryRow(k.ctx,
		`SELECT count(*), count(*) FILTER (WHERE revoked_at IS NOT NULL) FROM refresh_tokens WHERE device_id = $1`,
		string(device)).Scan(&tokens, &revokedTokens); err != nil {
		k.t.Fatal(err)
	}
	if err := k.pool.QueryRow(k.ctx,
		`SELECT revoked_at IS NOT NULL FROM devices WHERE id = $1`, string(device)).Scan(&deviceRevoked); err != nil {
		k.t.Fatal(err)
	}
	return tokens, revokedTokens, deviceRevoked
}

// THE `Done when`'S FIRST CLAUSE (plan criterion 16). The socket was
// authorized once at accept and outlives its access token (CANT-28 ruling 2);
// REST was not. With the token expired in the database a triggered catch-up
// completes, the client never redials, and a message sent afterwards still
// arrives live on the socket it had all along.
func TestRESTRefreshesWithoutDisturbingALiveSocket(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh bool
	}{
		{"refresh on: the catch-up completes and the socket never notices", true},
		{"negative control — refresh off: the catch-up spins on 401", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKillRig(t, nil)
			cs := k.cast()
			l := &ledger{}
			theoDev, adaDev := k.enroll(cs.theo, "theo's phone"), k.enroll(cs.ada, "ada's laptop")
			theo := k.refreshingClient(theoDev, client.NewJournal(), tc.refresh, nil, true)
			ada := k.client(adaDev, nil, client.Faults{})
			for who, c := range map[string]*client.Client{"theo": theo, "ada": ada} {
				awaitClient(t, c, who+"'s first ready and catch-up", func() bool { s := c.Status(); return s.Readys >= 1 && s.CaughtUp })
			}

			k.expireAccess(theoDev.DeviceID)
			before := theo.Status()
			theo.CatchUp()

			ctx, cancel := context.WithTimeout(k.ctx, 3*time.Second)
			defer cancel()
			err := theo.Await(ctx, func() bool { s := theo.Status(); return s.Pages > before.Pages && s.CaughtUp })
			after := theo.Status()

			if !tc.refresh {
				if err == nil {
					t.Fatal("with refresh off the catch-up completed on an expired token — the expiry expired nothing")
				}
				if after.SyncErrors == before.SyncErrors {
					t.Errorf("the refused catch-up recorded no sync error: %+v", after.Stats)
				}
				return
			}
			if err != nil {
				t.Fatalf("the catch-up never completed: %v; stats %+v", err, after.Stats)
			}
			if after.Refreshes != 1 || after.SyncErrors != before.SyncErrors {
				t.Errorf("refreshes %d, new sync errors %d; want one refresh and the 401 never surfaced", after.Refreshes, after.SyncErrors-before.SyncErrors)
			}
			if after.Dials != before.Dials || after.Readys != before.Readys || !after.Ready {
				t.Errorf("the socket was disturbed: dials %d→%d, readys %d→%d, ready=%v", before.Dials, after.Dials, before.Readys, after.Readys, after.Ready)
			}

			// And it still delivers, live, on that same socket.
			ack := l.socket(t, ada, "ada", cs.a, "after theo's refresh")
			awaitClient(t, theo, "ada's message, live", func() bool { return theo.Holds(ack.MessageID) })
			final := theo.Status()
			if final.LiveFrames <= after.LiveFrames {
				t.Errorf("the message reached theo but not as a live frame: %d→%d", after.LiveFrames, final.LiveFrames)
			}
			if final.Dials != before.Dials {
				t.Errorf("theo redialed (%d→%d); the message did not arrive on the original socket", before.Dials, final.Dials)
			}
			if tokens, revoked, dead := k.deviceHealth(theoDev.DeviceID); tokens != 2 || revoked != 0 || dead {
				t.Errorf("the device holds %d refresh tokens, %d revoked, device revoked=%v; want 2, 0, false", tokens, revoked, dead)
			}
		})
	}
}

// REACTIVE, IN ONE ROUND TRIP (plan criterion 19). The device's clock has been
// stepped back an hour, so the proactive check is confidently wrong and a
// fresh start presents a dead pair. Catenary's 401 on the /sync beside the
// upgrade is answered by exactly one refresh and one retry, and the next dial
// gets in with the rotated pair.
func TestADefeatedProactiveCheckIsRecoveredByTheReactiveOne(t *testing.T) {
	k := newKillRig(t, nil)
	cs := k.cast()
	dev := k.enroll(cs.theo, "theo's phone")
	k.expireAccess(dev.DeviceID)

	stepped := func() time.Time { return time.Now().Add(-time.Hour) }
	theo := k.refreshingClient(dev, client.NewJournal(), true, stepped, true)
	awaitClient(t, theo, "ready and caught up, from a dead pair", func() bool { s := theo.Status(); return s.Ready && s.CaughtUp })

	s := theo.Status()
	if s.Refreshes != 1 || s.RefreshErrors != 0 {
		t.Errorf("refreshes %d, refresh errors %d; want exactly one rotation", s.Refreshes, s.RefreshErrors)
	}
	if s.SyncErrors != 0 {
		t.Errorf("%d sync errors; the 401 should have been recovered inside the fetch that met it", s.SyncErrors)
	}
	if s.DialErrors == 0 {
		t.Error("no dial error was recorded — the dead pair was never presented, so the proactive check was not defeated")
	}
	if tokens, revoked, dead := k.deviceHealth(dev.DeviceID); tokens != 2 || revoked != 0 || dead {
		t.Errorf("the device holds %d refresh tokens, %d revoked, device revoked=%v; want 2, 0, false", tokens, revoked, dead)
	}
	// The rotation re-learned the offset from the real server's Date.
	if cr, _ := theo.Journal().Credential(); cr.ClockOffset < 59*time.Minute || cr.ClockOffset > 61*time.Minute {
		t.Errorf("the rotated pair's clock offset is %s, want about +1h", cr.ClockOffset)
	}
}

// SINGLE-FLIGHT PER CREDENTIAL, NOT PER PROCESS (plan criterion 20). Six
// independent clients share one stored credential and all find it due at once
// — their clocks read fourteen minutes into a fifteen-minute token. The real
// server sees exactly one rotation, and no family is revoked.
func TestClientsSharingACredentialProduceOneRotation(t *testing.T) {
	const n = 6
	k := newKillRig(t, nil)
	cs := k.cast()
	dev := k.enroll(cs.theo, "theo's phone")

	late := func() time.Time { return time.Now().Add(14 * time.Minute) }
	j := client.NewJournal()
	clients := make([]*client.Client, n)
	for i := range clients {
		clients[i] = k.refreshingClient(dev, j, true, late, false)
	}
	enrolled, _ := j.Credential()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, c := range clients {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = c.RefreshIfDue(k.ctx) }()
	}
	wg.Wait()

	var refreshed, skipped int
	for i, c := range clients {
		if errs[i] != nil {
			t.Errorf("client %d: %v", i, errs[i])
		}
		s := c.Status()
		refreshed, skipped = refreshed+s.Refreshes, skipped+s.RefreshesSkipped
	}
	if refreshed != 1 || skipped != n-1 {
		t.Errorf("%d refreshed and %d skipped, want 1 and %d", refreshed, skipped, n-1)
	}
	if tokens, revoked, dead := k.deviceHealth(dev.DeviceID); tokens != 2 || revoked != 0 || dead {
		t.Errorf("the device holds %d refresh tokens, %d revoked, device revoked=%v; want exactly one successor and nothing revoked", tokens, revoked, dead)
	}
	rotated, _ := j.Credential()
	if rotated.RefreshToken == enrolled.RefreshToken || rotated.AccessToken == enrolled.AccessToken {
		t.Error("the shared journal still holds the enrollment-time pair")
	}

	// The rotated pair works, for a client that was not the one that fetched
	// it: persist-before-use means it is simply what the journal now holds.
	fresh := k.refreshingClient(dev, j, true, late, true)
	awaitClient(t, fresh, "a seventh client gets in on the shared rotation", func() bool { s := fresh.Status(); return s.Ready && s.CaughtUp })
	if s := fresh.Status(); s.Refreshes != 0 {
		t.Errorf("the seventh client refreshed %d times; the offset learned by the rotation should have told it the pair is fresh", s.Refreshes)
	}
}
