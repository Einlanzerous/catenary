package main

// CANT-121, against the real router and a real database: a client killed after
// a rotation restarts presenting the rotated pair, not the spent one.
//
// internal/client's own tests pin this against a stub that records what it was
// shown. This one makes the server the judge. The enrollment-time access token
// is expired in Postgres, so whichever pair the restarted client presents is
// accepted or refused by the same Authenticate every real request goes through
// — and the restart is handed the journal and nothing else, because Config no
// longer has a field it could be handed a credential through.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// refreshOverHTTP is one real POST /refresh. The reference client does not
// refresh by itself until CANT-124; this is the rotation it will perform,
// done by hand so the durable half can be proved before the half that calls it.
func (k *killRig) refreshOverHTTP(refreshToken string) wire.RefreshResponse {
	k.t.Helper()
	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		k.t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(k.ctx, http.MethodPost, k.base()+"/refresh", bytes.NewReader(body))
	if err != nil {
		k.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatalf("POST /refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		k.t.Fatalf("POST /refresh = %d", resp.StatusCode)
	}
	var out wire.RefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		k.t.Fatalf("decode the refresh response: %v", err)
	}
	return out
}

func rotatedFrom(t *testing.T, held client.Credential, r wire.RefreshResponse) client.Credential {
	t.Helper()
	next, err := client.CredentialFromEnroll(wire.EnrollResponse{
		DeviceID:    held.DeviceID,
		AccessToken: r.AccessToken, AccessExpiresAt: r.AccessExpiresAt,
		RefreshToken: r.RefreshToken, RefreshExpiresAt: r.RefreshExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestAKilledClientRestartsWithTheRotatedPair(t *testing.T) {
	for _, tc := range []struct {
		name string
		// persist is whether the rotation reaches the journal before the
		// kill. false is the NEGATIVE CONTROL: it is the old shape, where
		// the pair a restart presents is the one it was first given.
		persist   bool
		wantReady bool
	}{
		{"the rotation was persisted: the restart gets in", true, true},
		{"negative control — it was not: the restart presents the spent pair and is refused", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKillRig(t, nil)
			cs := k.cast()
			dev := k.enroll(cs.theo, "theo's phone")

			journal := client.NewJournal()
			first := k.client(dev, journal, client.Faults{})
			awaitClient(t, first, "the first ready and catch-up", func() bool {
				s := first.Status()
				return s.Readys >= 1 && s.CaughtUp
			})
			enrolled, _ := journal.Credential()

			// Rotate for real, then expire every access token the device
			// held BEFORE the rotation. Rotation alone leaves the old access
			// token valid until its own expiry, which would let a restart
			// presenting the spent pair in for fifteen minutes and hide
			// exactly the bug this test is about.
			rotated := rotatedFrom(t, enrolled, k.refreshOverHTTP(enrolled.RefreshToken))
			tag, err := k.pool.Exec(k.ctx,
				`UPDATE access_tokens SET expires_at = now() - interval '1 minute'
				  WHERE device_id = $1 AND issued_at < (SELECT max(issued_at) FROM access_tokens WHERE device_id = $1)`,
				string(dev.DeviceID))
			if err != nil {
				t.Fatal(err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("expired %d access tokens, want exactly the enrollment-time one", tag.RowsAffected())
			}
			if tc.persist {
				if err := journal.Rotate(rotated); err != nil {
					t.Fatalf("persist the rotation: %v", err)
				}
			}

			// THE SOCKET IS AUTHORIZED ONCE, AT ACCEPT (CANT-28 ruling 2): the
			// expiry above does not end the live session. That is not this
			// ticket's claim, but a session that dropped here would make the
			// restart below prove less than it appears to.
			if s := first.Status(); !s.Ready {
				t.Fatalf("expiring the access token ended the live session: %+v", s)
			}

			first.Kill()
			second := k.client(dev, first.Journal(), client.Faults{}) // `dev` is ignored: the journal is enrolled
			dialsBefore := second.Status().Dials

			ctx, cancel := context.WithTimeout(k.ctx, 10*time.Second)
			defer cancel()
			if !tc.wantReady {
				ctx, cancel = context.WithTimeout(k.ctx, 2*time.Second)
				defer cancel()
			}
			err = second.Await(ctx, func() bool { s := second.Status(); return s.Ready && s.CaughtUp })
			s := second.Status()
			switch {
			case tc.wantReady && err != nil:
				t.Fatalf("the restarted client never got back in with the rotated pair: %v; status %+v", err, s)
			case !tc.wantReady && err == nil:
				t.Fatal("a restart presenting the expired enrollment-time token reached ready — the control proves nothing")
			case !tc.wantReady && (s.DialErrors == 0 || s.Dials == dialsBefore):
				t.Errorf("the refused restart recorded no dial error: %+v", s)
			}

			got, _ := second.Journal().Credential()
			want := enrolled
			if tc.persist {
				want = rotated
			}
			if got != want {
				t.Errorf("the restarted client holds %+v, want %+v", redact(got), redact(want))
			}
		})
	}
}

// redact keeps a failure message from printing a token a real server minted.
func redact(c client.Credential) client.Credential {
	c.AccessToken, c.RefreshToken = "…"+tail(c.AccessToken), "…"+tail(c.RefreshToken)
	return c
}

func tail(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
