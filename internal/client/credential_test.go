package client

// CANT-121: the credential is durable state, not configuration.
//
// A refresh token is single-use, so the pair a device holds changes over its
// life, and a restart has to come back with the pair it last persisted rather
// than the one it was first given. These tests pin the Journal's half of that
// — what is kept, what refuses, what a wipe and a Kill do to it — against a
// stub server that records exactly what each session presented. The same
// property against the real router and a real database is
// cmd/catenary/credential_test.go.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

// enrolledJournal is a fresh journal holding a credential for a new device,
// which is what every Client in this package's tests starts from.
func enrolledJournal(t *testing.T, accessToken string) *Journal {
	t.Helper()
	j := NewJournal()
	if err := j.Enroll(Credential{
		DeviceID: wire.Uuid(uuid.NewString()), AccessToken: accessToken, RefreshToken: "refresh-of-" + accessToken,
	}); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestNewRefusesAJournalNobodyEnrolled(t *testing.T) {
	for name, j := range map[string]*Journal{"no journal": nil, "an empty journal": NewJournal()} {
		if _, err := New(Config{BaseURL: "http://example.invalid", Journal: j}); !errors.Is(err, ErrNoCredential) {
			t.Errorf("New over %s = %v, want ErrNoCredential", name, err)
		}
	}
}

// The refusal is the guard. A restart is built by a caller that still holds
// the enrollment response, and the easy mistake is to seed the journal with it
// again — over a pair that is by then a rotation ahead.
func TestEnrollNeverOverwritesAHeldCredential(t *testing.T) {
	j := enrolledJournal(t, "enrolled")
	first, _ := j.Credential()
	rotated := first
	rotated.AccessToken, rotated.RefreshToken = "rotated", "refresh-of-rotated"
	if err := j.Rotate(rotated); err != nil {
		t.Fatal(err)
	}

	if err := j.Enroll(first); !errors.Is(err, ErrCredentialHeld) {
		t.Fatalf("Enroll over a held credential = %v, want ErrCredentialHeld", err)
	}
	if got, _ := j.Credential(); got != rotated {
		t.Errorf("the refused Enroll still changed the credential: %+v, want the rotated pair", got)
	}
}

func TestRotateKeepsTheDeviceAndNeedsACredential(t *testing.T) {
	if err := NewJournal().Rotate(Credential{DeviceID: "d", AccessToken: "a"}); !errors.Is(err, ErrNoCredential) {
		t.Errorf("Rotate over an empty journal = %v, want ErrNoCredential", err)
	}

	j := enrolledJournal(t, "enrolled")
	held, _ := j.Credential()
	other := held
	other.DeviceID, other.AccessToken = wire.Uuid(uuid.NewString()), "someone-else"
	if err := j.Rotate(other); !errors.Is(err, ErrCredentialDevice) {
		t.Errorf("Rotate naming another device = %v, want ErrCredentialDevice", err)
	}
	if got, _ := j.Credential(); got != held {
		t.Errorf("a refused Rotate changed the credential: %+v", got)
	}
}

// Obligation 4's wipe discards the message store a stale cursor invalidated.
// The credential is not part of that, and a client that lost it on a wipe
// would turn a server rollback into a re-enrollment for every device.
func TestAWipeKeepsTheCredential(t *testing.T) {
	j := enrolledJournal(t, "enrolled")
	want, _ := j.Credential()

	j.mu.Lock()
	j.cursor, j.hasCursor = 41, true
	j.resetLocked()
	j.wipes++
	j.mu.Unlock()

	got, ok := j.Credential()
	if !ok || got != want {
		t.Errorf("after a wipe the journal holds %+v (held=%v), want the credential untouched", got, ok)
	}
	if snap := j.Snapshot(); snap.HasCursor {
		t.Error("the wipe did not clear the cursor — this test wiped nothing")
	}
}

// Kill is `kill -9`, and nothing a killed client had in flight reaches the
// Journal. A refresh response landing after the kill is no exception: the
// process that would have persisted it is dead.
func TestAKilledClientCannotRotate(t *testing.T) {
	j := enrolledJournal(t, "enrolled")
	held, _ := j.Credential()
	c, err := New(Config{BaseURL: "http://example.invalid", Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	next := held
	next.AccessToken = "rotated"

	c.Kill()
	if err := c.rotate(next); !errors.Is(err, ErrKilled) {
		t.Fatalf("rotate after Kill = %v, want ErrKilled", err)
	}
	if got, _ := j.Credential(); got != held {
		t.Errorf("a killed client's rotation reached the journal: %+v", got)
	}

	// And the live path works, so the refusal above is the guard and not a
	// rotate that never succeeds.
	live, err := New(Config{BaseURL: "http://example.invalid", Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	if err := live.rotate(next); err != nil {
		t.Fatalf("rotate on a live client: %v", err)
	}
	if got, _ := j.Credential(); got != next {
		t.Errorf("a live client's rotation did not reach the journal: %+v", got)
	}
}

// presented is what one stub-server session saw the client offer.
type presented struct {
	mu       sync.Mutex
	upgrades []string // the catenary.token.<token> subprotocol, token only
	bearers  []string // /sync's Authorization, token only
	devices  []wire.Uuid
}

func (p *presented) snapshot() (upgrades, bearers []string, devices []wire.Uuid) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.upgrades...), append([]string(nil), p.bearers...), append([]wire.Uuid(nil), p.devices...)
}

// recordingServer accepts any token and records every one it is shown, on
// both surfaces, with the hello's device id.
func recordingServer(t *testing.T) (*httptest.Server, *presented) {
	t.Helper()
	p := &presented{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync":
			p.mu.Lock()
			p.bearers = append(p.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			p.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"log_seq":0,"messages":[],"conversations":[],"users":[],"has_more":false,"server_time":"2026-01-01T00:00:00.000Z"}`))
		case "/ws":
			for _, sp := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
				if tok, ok := strings.CutPrefix(strings.TrimSpace(sp), tokenSubprotocolPrefix); ok {
					p.mu.Lock()
					p.upgrades = append(p.upgrades, tok)
					p.mu.Unlock()
				}
			}
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if f, err := wire.DecodeClientFrame(data); err == nil {
				if hello, ok := f.(wire.ClientHello); ok {
					p.mu.Lock()
					p.devices = append(p.devices, hello.DeviceID)
					p.mu.Unlock()
				}
			}
			if err := conn.Write(context.Background(), websocket.MessageText, readyFrame(t)); err != nil {
				return
			}
			_, _, _ = conn.Read(context.Background()) // held until the client goes
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, p
}

func runUntilCaughtUp(t *testing.T, c *Client) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(context.Background()) }()
	stop = func() { c.Kill(); <-done }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Await(ctx, func() bool { s := c.Status(); return s.Ready && s.CaughtUp }); err != nil {
		stop()
		t.Fatalf("the client never reached ready and caught up: %v; status %+v", err, c.Status())
	}
	return stop
}

// THE TICKET'S CLAIM, against a server that records what it was shown: a
// client killed after a rotation restarts presenting the rotated pair on the
// upgrade AND on /sync, under the same device id, and the spent token is never
// seen again.
func TestARestartPresentsTheRotatedPairNotTheSpentOne(t *testing.T) {
	srv, seen := recordingServer(t)
	cfg := Config{BaseURL: srv.URL, BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond}

	j := enrolledJournal(t, "enrolled-access")
	enrolled, _ := j.Credential()
	cfg.Journal = j
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop := runUntilCaughtUp(t, first)

	rotated := enrolled
	rotated.AccessToken, rotated.RefreshToken = "rotated-access", "rotated-refresh"
	if err := first.rotate(rotated); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	stop()
	upBefore, bearBefore, _ := seen.snapshot()

	// The restart is handed the journal and nothing else. There is no field
	// through which it could be handed the enrollment-time pair.
	cfg.Journal = first.Journal()
	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runUntilCaughtUp(t, second)()

	up, bear, devices := seen.snapshot()
	up, bear = up[len(upBefore):], bear[len(bearBefore):]
	if len(up) == 0 || len(bear) == 0 {
		t.Fatalf("the restarted client reached neither surface: upgrades %v, bearers %v", up, bear)
	}
	for _, tok := range append(up, bear...) {
		if tok != "rotated-access" {
			t.Errorf("the restarted client presented %q; every request after the restart must carry the rotated token", tok)
		}
	}
	for _, d := range devices {
		if d != enrolled.DeviceID {
			t.Errorf("a hello named device %s, want %s on both sides of the rotation", d, enrolled.DeviceID)
		}
	}
	if got, _ := second.Journal().Credential(); got.RefreshToken != "rotated-refresh" {
		t.Errorf("the restarted client holds refresh token %q, want the rotated one", got.RefreshToken)
	}
}

// The credential is re-read at every use, not copied onto the Client at New.
// A rotation written by another context sharing the store — record §2's two
// tabs, or an app and its push worker — is what this client's next dial
// carries, without a restart.
func TestARunningClientPicksUpARotationOnItsNextDial(t *testing.T) {
	srv, seen := recordingServer(t)
	j := enrolledJournal(t, "enrolled-access")
	c, err := New(Config{BaseURL: srv.URL, Journal: j, BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer runUntilCaughtUp(t, c)()

	next, _ := j.Credential()
	next.AccessToken = "rotated-elsewhere"
	if err := j.Rotate(next); err != nil { // the Journal's own door: another context
		t.Fatal(err)
	}
	dials := c.Status().Dials
	c.Sever()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Await(ctx, func() bool { s := c.Status(); return s.Dials > dials && s.Ready }); err != nil {
		t.Fatalf("the client never redialed after Sever: %v", err)
	}
	up, _, _ := seen.snapshot()
	if last := up[len(up)-1]; last != "rotated-elsewhere" {
		t.Errorf("the redial presented %q, want the token another context rotated in", last)
	}
}
