package client

// The hello is the first frame on every socket, whatever the caller is doing.
// The server closes a session whose first known frame is anything else with
// 1008 (internal/api's awaitHello), so a Send that raced a reconnect would
// sever a fresh session — and CANT-27's harness sends on a loop while sessions
// drop and redial. PR #51's review found the window: the socket was published
// to Send before the hello was written.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

// readyFrame is a minimal, valid `ready` frame for a fake server to send.
func readyFrame(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(wire.ServerReady{
		SessionID: wire.Uuid(uuid.NewString()), ServerTime: "2026-01-01T00:00:00.000Z",
		HeartbeatIntervalSec: 30, MissedPongLimit: 2, LogSeq: 0, Resumed: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNoFrameCanPrecedeTheHello(t *testing.T) {
	accepted := make(chan struct{})
	first := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.Error(w, "no /sync here", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		close(accepted)
		_, data, err := conn.Read(context.Background())
		if err != nil {
			first <- "read error: " + err.Error()
			return
		}
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &probe)
		first <- probe.Type
		_, _, _ = conn.Read(context.Background()) // hold the socket until the client goes
	}))
	t.Cleanup(srv.Close)

	j := NewJournal()
	c, err := New(Config{BaseURL: srv.URL, AccessToken: "token", DeviceID: wire.Uuid(uuid.NewString()), Journal: j})
	if err != nil {
		t.Fatal(err)
	}

	// HOLD THE JOURNAL: the session dials, then cannot read the cursor its
	// hello carries. It is parked exactly between the upgrade and the hello.
	j.mu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			j.mu.Unlock()
		}
	}
	t.Cleanup(unlock)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(context.Background())
	}()
	t.Cleanup(func() {
		unlock()
		c.Kill()
		<-done
	})

	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the client never upgraded")
	}

	// Every Send in this window must find no socket. One that finds it writes
	// a `send` frame before the hello.
	send := wire.ClientSend{ClientID: wire.Uuid(uuid.NewString()), ConversationID: wire.Uuid(uuid.NewString())}
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := c.Send(ctx, send)
		cancel()
		if !errors.Is(err, ErrNotConnected) {
			t.Fatalf("Send between the upgrade and the hello = %v, want ErrNotConnected", err)
		}
	}

	unlock()
	select {
	case got := <-first:
		if got != "hello" {
			t.Errorf("the server's first frame is %q, want hello", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no frame reached the server")
	}
}

// CANT-27's reconnect storm needs to drop every client's socket at once and
// watch it come back on its own — a capability Kill lacks (Kill also stops
// Run for good) and Close lacks (a graceful handshake is not what a storm
// looks like). Sever is the addition.
func TestSeverEndsTheSessionWithoutStoppingRun(t *testing.T) {
	var upgrades atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(context.Background()); err != nil { // the hello
			return
		}
		upgrades.Add(1)
		if err := conn.Write(context.Background(), websocket.MessageText, readyFrame(t)); err != nil {
			return
		}
		_, _, _ = conn.Read(context.Background()) // held until Sever or the test ends
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{
		BaseURL: srv.URL, AccessToken: "token", DeviceID: wire.Uuid(uuid.NewString()),
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(context.Background()) }()
	t.Cleanup(func() { c.Kill(); <-done })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Await(ctx, func() bool { return c.Status().Ready }); err != nil {
		t.Fatalf("waiting for the first ready: %v", err)
	}

	// A Sever with nothing open is a documented no-op; proved here rather
	// than assumed, since a panic on a nil conn would fail every other test
	// that never calls it.
	idle, err := New(Config{BaseURL: srv.URL, AccessToken: "unused", DeviceID: wire.Uuid(uuid.NewString())})
	if err != nil {
		t.Fatal(err)
	}
	idle.Sever()

	c.Sever()

	if err := c.Await(ctx, func() bool { return upgrades.Load() >= 2 && c.Status().Ready }); err != nil {
		t.Fatalf("waiting for the reconnect: %v; status %+v", err, c.Status())
	}
	select {
	case <-done:
		t.Fatal("Run returned after Sever; Sever must not stop Run the way Kill does")
	default:
	}
	if s := c.Status(); s.Dials < 2 || s.Readys < 2 {
		t.Errorf("dials %d, readys %d — want at least two of each after one Sever", s.Dials, s.Readys)
	}
}

// The close code a session ended with is CANT-27's "close statuses seen":
// evidence that the client is tolerating the drain (1001), heartbeat-timeout
// (4000) and head-unreadable (1012) codes CANT-35's table names, and the
// abnormal case (-1) a `kill -9` or a network cut produces, rather than
// treating any of them as a protocol error.
func TestCloseStatusesRecordsTheCodeAndAbnormalClosures(t *testing.T) {
	var sessions atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
		if err != nil {
			return
		}
		if _, _, err := conn.Read(context.Background()); err != nil {
			conn.CloseNow()
			return
		}
		if err := conn.Write(context.Background(), websocket.MessageText, readyFrame(t)); err != nil {
			conn.CloseNow()
			return
		}
		if sessions.Add(1) == 1 {
			_ = conn.Close(websocket.StatusGoingAway, "draining") // a real close frame: 1001
		} else {
			_ = conn.CloseNow() // no close frame: abnormal, -1
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{
		BaseURL: srv.URL, AccessToken: "token", DeviceID: wire.Uuid(uuid.NewString()),
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(context.Background()) }()
	t.Cleanup(func() { c.Kill(); <-done })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Await(ctx, func() bool {
		s := c.Status()
		return s.CloseStatuses[int(websocket.StatusGoingAway)] >= 1 && s.CloseStatuses[-1] >= 1
	}); err != nil {
		t.Fatalf("waiting for both close codes: %v; status %+v", err, c.Status())
	}
}

// A pure dial failure never held a session to end, and Stats.DialErrors
// already counts it — CANT-27's review found it ALSO landing in
// CloseStatuses' -1 bucket, double-counted under two names.
func TestDialFailuresDoNotReachCloseStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no /ws here", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{
		BaseURL: srv.URL, AccessToken: "token", DeviceID: wire.Uuid(uuid.NewString()),
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(context.Background()) }()
	t.Cleanup(func() { c.Kill(); <-done })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Await(ctx, func() bool { return c.Status().DialErrors >= 3 }); err != nil {
		t.Fatalf("waiting for dial errors: %v; status %+v", err, c.Status())
	}
	if s := c.Status(); len(s.CloseStatuses) != 0 {
		t.Errorf("CloseStatuses = %v after %d dial errors, want empty — a dial failure never opened a session to end", s.CloseStatuses, s.DialErrors)
	}
}

// CANT-109 — Config.ExtraHeaders reaches both surfaces. A small front proxy
// stands in for Cloudflare Access in front of the deployed router
// (construct-server/config/traefik/dynamic/routers.yml, the catenary
// router's AUD): it 403s any request — /sync or the /ws upgrade alike —
// missing either CF-Access-Client-Id or CF-Access-Client-Secret, or carrying
// the wrong value, and only then forwards to a real backend behind it. The
// backend is reached, and the client reaches ready+caught-up, only when
// ExtraHeaders carries both correctly — proving the headers land on the HTTP
// request AND the upgrade request, not just one of the two.
func TestExtraHeadersReachHTTPAndTheUpgrade(t *testing.T) {
	const wantID, wantSecret = "test-access-client-id", "test-access-client-secret"

	var syncHits, wsHits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync":
			syncHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"log_seq":0,"messages":[],"conversations":[],"users":[],"has_more":false,"server_time":"2026-01-01T00:00:00.000Z"}`))
		case "/ws":
			wsHits.Add(1)
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			if _, _, err := conn.Read(context.Background()); err != nil { // the hello
				return
			}
			if err := conn.Write(context.Background(), websocket.MessageText, readyFrame(t)); err != nil {
				return
			}
			_, _, _ = conn.Read(context.Background()) // held until the test ends
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	// httputil.ReverseProxy proxies a Connection: Upgrade request (the
	// WebSocket handshake) transparently, exactly as Traefik does — so a
	// gate placed in front of it is a faithful stand-in for Access sitting
	// in front of the real router, on both surfaces at once.
	rp := httputil.NewSingleHostReverseProxy(backendURL)

	var refusals atomic.Int64
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Access-Client-Id") != wantID || r.Header.Get("CF-Access-Client-Secret") != wantSecret {
			refusals.Add(1)
			http.Error(w, "cloudflare access: service token required", http.StatusForbidden)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)

	correct := http.Header{}
	correct.Set("CF-Access-Client-Id", wantID)
	correct.Set("CF-Access-Client-Secret", wantSecret)

	for _, tc := range []struct {
		name      string
		headers   http.Header
		wantReady bool
	}{
		{"both headers correct", correct, true},
		{"no headers at all", nil, false},
		{"secret missing", http.Header{"CF-Access-Client-Id": {wantID}}, false},
		{"id missing", http.Header{"CF-Access-Client-Secret": {wantSecret}}, false},
		{"wrong secret", http.Header{"CF-Access-Client-Id": {wantID}, "CF-Access-Client-Secret": {"not-the-secret"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Config{
				BaseURL: gate.URL, AccessToken: "token", DeviceID: wire.Uuid(uuid.NewString()),
				ExtraHeaders: tc.headers, BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); _ = c.Run(context.Background()) }()
			t.Cleanup(func() { c.Kill(); <-done })

			bound := 300 * time.Millisecond
			if tc.wantReady {
				bound = 10 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), bound)
			defer cancel()
			pred := func() bool { return c.Status().Ready }
			if tc.wantReady {
				pred = func() bool { s := c.Status(); return s.Ready && s.CaughtUp }
			}
			err = c.Await(ctx, pred)
			switch {
			case tc.wantReady && err != nil:
				t.Fatalf("a client WITH the correct Access headers never reached ready+caught-up: %v; status %+v", err, c.Status())
			case !tc.wantReady && err == nil:
				t.Fatalf("a client with %s reached ready — the gate did not refuse it", tc.name)
			case !tc.wantReady:
				if s := c.Status(); s.DialErrors == 0 {
					t.Errorf("a client with %s never recorded a dial error — want the gate's 403 to fail the upgrade", tc.name)
				}
			}
		})
	}

	if wsHits.Load() == 0 || syncHits.Load() == 0 {
		t.Fatalf("the real backend behind the gate was never reached — this test proves nothing: ws=%d sync=%d", wsHits.Load(), syncHits.Load())
	}
	if refusals.Load() == 0 {
		t.Errorf("the gate never recorded a refusal — the negative cases above prove nothing without it")
	}
}
