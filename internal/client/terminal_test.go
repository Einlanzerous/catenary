package client

// CANT-123 against a stub that ends sessions exactly as scripted. The closes
// the REAL server can be made to produce — 4001, each bare 1008, the two
// refusals, the hello timeout's 4002 — and the HTTP route a revoked device
// takes are cmd/catenary/terminal_test.go. What is only here is what the real
// server cannot be asked for on demand: error{internal}, an unlisted close
// code, a proxy's 401, a rotation racing a refusal.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

func TestTheCloseTableIsTheRecords(t *testing.T) {
	sessionErr := func(code wire.ErrorCode, retryable bool) *wire.ServerError {
		return &wire.ServerError{Code: code, Message: "x", Retryable: retryable}
	}
	for _, tc := range []struct {
		name      string
		status    websocket.StatusCode
		preceding *wire.ServerError
		want      closeVerdict
	}{
		{"4001", 4001, nil, stopProtocolFailure},
		{"4001, whatever preceded it", 4001, sessionErr(wire.ErrorCodeInternal, true), stopProtocolFailure},
		{"1008 bare", 1008, nil, stopProtocolFailure},
		{"1008 after error{unauthorized}", 1008, sessionErr(wire.ErrorCodeUnauthorized, false), stopProtocolFailure},
		{"1008 after error{wire_version_unsupported}", 1008, sessionErr(wire.ErrorCodeWireVersionUnsupported, false), stopProtocolFailure},
		{"1008 after error{internal}, retryable", 1008, sessionErr(wire.ErrorCodeInternal, true), reconnectAtMaximum},
		{"1008 after error{internal}, NOT retryable — the flag is not consulted", 1008, sessionErr(wire.ErrorCodeInternal, false), reconnectAtMaximum},
		{"1008 after error{rate_limited}", 1008, sessionErr(wire.ErrorCodeRateLimited, true), reconnect},
		{"1008 after a code a later server adds", 1008, sessionErr("quota_exceeded", false), reconnect},
		{"1001 drain", 1001, nil, reconnect},
		{"1012 head unreadable", 1012, nil, reconnect},
		{"4000 heartbeat timeout", 4000, nil, reconnect},
		{"4002 hello timeout", 4002, nil, reconnect},
		{"abnormal closure", -1, nil, reconnect},
		{"1000, unlisted", 1000, nil, reconnect},
		{"1009, unlisted", 1009, nil, reconnect},
		{"1011, unlisted", 1011, nil, reconnect},
		{"a private code a later server adds", 4999, nil, reconnect},
	} {
		if got, _ := classifyClose(tc.status, tc.preceding); got != tc.want {
			t.Errorf("%s: verdict %d, want %d", tc.name, got, tc.want)
		}
	}
}

// --- a stub that ends sessions as scripted -------------------------------------

// ending is one session's script: frames written after `ready`, then a close.
type ending struct {
	frames []any
	status websocket.StatusCode
}

type closer struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	script  []ending // one per session, in order; the last repeats
	dialsAt []time.Time
}

func newCloser(t *testing.T, script ...ending) *closer {
	t.Helper()
	cl := &closer{t: t, script: script}
	cl.srv = httptest.NewServer(http.HandlerFunc(cl.serve))
	t.Cleanup(cl.srv.Close)
	return cl
}

func (cl *closer) dials() []time.Time {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return append([]time.Time(nil), cl.dialsAt...)
}

func (cl *closer) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/sync" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"log_seq":0,"messages":[],"conversations":[],"users":[],"has_more":false,"server_time":"2026-01-01T00:00:00.000Z"}`))
		return
	}
	cl.mu.Lock()
	n := len(cl.dialsAt)
	cl.dialsAt = append(cl.dialsAt, time.Now())
	end := cl.script[min(n, len(cl.script)-1)]
	cl.mu.Unlock()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := context.Background()
	if _, _, err := conn.Read(ctx); err != nil {
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, readyFrame(cl.t)); err != nil {
		return
	}
	for _, f := range end.frames {
		b, err := json.Marshal(f)
		if err != nil {
			cl.t.Errorf("marshal a scripted frame: %v", err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			return
		}
	}
	if end.status == 0 {
		_, _, _ = conn.Read(ctx) // hold the session open
		return
	}
	_ = conn.Close(end.status, "scripted")
}

func errorFrame(code wire.ErrorCode, retryable bool, clientID *wire.Uuid) map[string]any {
	f := map[string]any{"type": "error", "code": code, "message": "scripted", "retryable": retryable}
	if clientID != nil {
		f["client_id"] = *clientID
	}
	return f
}

// runToEnd runs a client until Run returns or the bound passes, and hands
// back Run's error (nil if it was still running and had to be killed).
func runToEnd(t *testing.T, c *Client, bound time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(bound):
		c.Kill()
		<-done
		return nil
	}
}

func terminalOf(err error) Terminal {
	var te *TerminalError
	if errors.As(err, &te) {
		return te.Terminal
	}
	return Terminal{}
}

// EVERY ROW OF RECORD §4, end to end: what the client does after a session
// ends that way. "Stops" is Run returning a TerminalError having dialed once;
// "reconnects" is a second dial.
func TestWhatTheClientDoesAfterEachClose(t *testing.T) {
	someSend := wire.Uuid(uuid.NewString())
	for _, tc := range []struct {
		name      string
		end       ending
		faults    Faults
		wantStops bool
	}{
		{"4001 stops", ending{status: 4001}, Faults{}, true},
		{"a bare 1008 stops", ending{status: 1008}, Faults{}, true},
		{"1008 after error{unauthorized} stops", ending{[]any{errorFrame(wire.ErrorCodeUnauthorized, false, nil)}, 1008}, Faults{}, true},
		{"1008 after error{wire_version_unsupported} stops", ending{[]any{errorFrame(wire.ErrorCodeWireVersionUnsupported, false, nil)}, 1008}, Faults{}, true},
		{"1008 after error{internal} reconnects", ending{[]any{errorFrame(wire.ErrorCodeInternal, false, nil)}, 1008}, Faults{}, false},
		{"1008 after error{rate_limited} reconnects", ending{[]any{errorFrame(wire.ErrorCodeRateLimited, true, nil)}, 1008}, Faults{}, false},

		// PRECEDED BY means the last frame, carrying no client_id.
		{"an earlier error naming a SEND does not make a bare 1008 transient",
			ending{[]any{errorFrame(wire.ErrorCodeRateLimited, true, &someSend)}, 1008}, Faults{}, true},
		{"a session error followed by ANOTHER frame no longer precedes the close",
			ending{[]any{errorFrame(wire.ErrorCodeInternal, true, nil), map[string]any{"type": "pong", "id": "late"}}, 1008}, Faults{}, true},

		{"1001 reconnects", ending{status: 1001}, Faults{}, false},
		{"1012 reconnects", ending{status: 1012}, Faults{}, false},
		{"4000 reconnects", ending{status: 4000}, Faults{}, false},
		{"4002 reconnects", ending{status: 4002}, Faults{}, false},
		{"1000, which nobody listed, reconnects", ending{status: 1000}, Faults{}, false},
		{"1009, which nobody listed, reconnects", ending{status: 1009}, Faults{}, false},
		{"1011, which nobody listed, reconnects", ending{status: 1011}, Faults{}, false},
		{"a private code a later server adds reconnects", ending{status: 4999}, Faults{}, false},

		// THE NEGATIVE CONTROLS, one in each direction.
		{"control — terminal branch removed: 4001 reconnects", ending{status: 4001}, Faults{NeverTerminal: true}, false},
		{"control — terminal branch removed: a bare 1008 reconnects", ending{status: 1008}, Faults{NeverTerminal: true}, false},
		{"control — terminal made unconditional: a transient 1001 stops", ending{status: 1001}, Faults{AlwaysTerminal: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := newCloser(t, tc.end, ending{}) // the second session, if any, is held open
			c, err := New(fastBackoff(Config{BaseURL: cl.srv.URL, Journal: enrolledJournal(t, "token"), Faults: tc.faults}))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.Run(context.Background()) }()
			defer func() { c.Kill() }()

			if tc.wantStops {
				select {
				case err := <-done:
					term := terminalOf(err)
					if term.Kind != TerminalProtocol {
						t.Fatalf("Run returned %v, want a protocol terminal", err)
					}
					if got := c.Status().Terminal; got != term || got.Reason == "" {
						t.Errorf("Status names %+v, Run returned %+v; want the same, with a reason", got, term)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("the client never stopped; dials %d", len(cl.dials()))
				}
				time.Sleep(100 * time.Millisecond) // many backoffs' worth
				if n := len(cl.dials()); n != 1 {
					t.Errorf("a stopped client dialed %d times, want exactly 1", n)
				}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.Await(ctx, func() bool { s := c.Status(); return s.Dials >= 2 && s.Ready }); err != nil {
				t.Fatalf("the client never came back: %v; status %+v", err, c.Status())
			}
			if got := c.Status().Terminal; got.Kind != NotTerminal {
				t.Errorf("a client that reconnected reports terminal %+v", got)
			}
		})
	}
}

// error{internal} RECONNECTS AT MAXIMUM BACKOFF, whatever `retryable` says —
// even straight after a session that reached `ready`, which would otherwise
// reset the backoff to its minimum.
func TestInternalReconnectsAtTheMaximumBackoff(t *testing.T) {
	const bmin, bmax = 5 * time.Millisecond, 600 * time.Millisecond
	for _, tc := range []struct {
		name string
		end  ending
		slow bool
	}{
		{"error{internal, retryable:true}", ending{[]any{errorFrame(wire.ErrorCodeInternal, true, nil)}, 1008}, true},
		{"control — error{rate_limited}: the ordinary minimum", ending{[]any{errorFrame(wire.ErrorCodeRateLimited, true, nil)}, 1008}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := newCloser(t, tc.end, ending{})
			c, err := New(Config{BaseURL: cl.srv.URL, Journal: enrolledJournal(t, "token"), BackoffMin: bmin, BackoffMax: bmax})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); _ = c.Run(context.Background()) }()
			defer func() { c.Kill(); <-done }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.Await(ctx, func() bool { return c.Status().Dials >= 2 && c.Status().Ready }); err != nil {
				t.Fatal(err)
			}
			d := cl.dials()
			gap := d[1].Sub(d[0])
			if tc.slow && gap < bmax {
				t.Errorf("redialed after %s, want at least the maximum backoff %s", gap, bmax)
			}
			if !tc.slow && gap >= bmax {
				t.Errorf("redialed after %s; an ordinary close should come back near the minimum, not the maximum", gap)
			}
		})
	}
}

// --- record §5: terminal, by HTTP ----------------------------------------------

// RECORD §5's /refresh ROWS. Catenary's own 401 is terminal only when the
// stored credential is still the one presented; a 401 from a hop in front is
// not Catenary's; and a pair another context rotated mid-flight is not gone.
func TestWhenARefusedRefreshIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		// answer writes /refresh's response; j is the journal, so a case can
		// be "another context rotated it while this request was in flight".
		answer       func(w http.ResponseWriter, j *Journal)
		faults       Faults
		wantTerminal TerminalKind
		wantErr      bool
	}{
		{"Catenary's own 401, the stored pair still the one presented: TERMINAL",
			func(w http.ResponseWriter, _ *Journal) { unauthorized(w) }, Faults{}, TerminalCredential, true},
		{"a proxy's 401 — not Catenary's: not terminal",
			func(w http.ResponseWriter, _ *Journal) {
				http.Error(w, "cloudflare access: session expired", http.StatusUnauthorized)
			}, Faults{}, NotTerminal, true},
		{"Catenary's 401, but another context rotated the pair in flight: not terminal, and not an error",
			func(w http.ResponseWriter, j *Journal) {
				held, _ := j.Credential()
				held.AccessToken, held.RefreshToken = padToken("access-elsewhere"), padToken("refresh-elsewhere")
				_ = j.Rotate(held)
				unauthorized(w)
			}, Faults{}, NotTerminal, false},
		{"a 503: an unknown outcome, not terminal",
			func(w http.ResponseWriter, _ *Journal) { http.Error(w, "busy", http.StatusServiceUnavailable) }, Faults{}, NotTerminal, true},
		{"control — terminal branch removed: Catenary's own 401 is just an error",
			func(w http.ResponseWriter, _ *Journal) { unauthorized(w) }, Faults{NeverTerminal: true}, NotTerminal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := NewJournal()
			if err := j.Enroll(Credential{
				DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken("access-0"), RefreshToken: padToken("refresh-0"),
				AccessExpiresAt: time.Now().Add(10 * time.Second), // due
			}); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/refresh" {
					tc.answer(w, j)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			before, _ := j.Credential()

			c, err := New(Config{BaseURL: srv.URL, Journal: j, Refresh: true, Faults: tc.faults})
			if err != nil {
				t.Fatal(err)
			}
			err = c.RefreshIfDue(context.Background())
			if (err != nil) != tc.wantErr {
				t.Errorf("RefreshIfDue = %v, wantErr %v", err, tc.wantErr)
			}
			if got := c.Status().Terminal.Kind; got != tc.wantTerminal {
				t.Errorf("terminal = %s, want %s", got, tc.wantTerminal)
			}
			if tc.wantTerminal != NotTerminal {
				if terminalOf(err).Kind != tc.wantTerminal {
					t.Errorf("the error returned is %v, want a TerminalError naming %s", err, tc.wantTerminal)
				}
				// TERMINAL NEVER DELETES THE STORED CREDENTIAL.
				if after, ok := j.Credential(); !ok || after != before {
					t.Errorf("going terminal changed the stored credential: held=%v %+v", ok, after)
				}
				// And a terminal client does not run.
				if err := runToEnd(t, c, 2*time.Second); terminalOf(err).Kind != tc.wantTerminal {
					t.Errorf("Run on a terminal client returned %v, want its TerminalError", err)
				}
			}
		})
	}
}

// A TERMINAL RAISED BY THE PROACTIVE REFRESH STOPS BEFORE THE DIAL. The pair
// is due, Catenary refuses its refresh token, and the client is terminal
// before it has touched the network for a socket — so it counts no dial, and
// no dial error for a dial that would only have failed on a cancelled context.
func TestAProactiveTerminalCountsNoDial(t *testing.T) {
	var sawSocket, sawSync bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		switch r.URL.Path {
		case "/ws":
			sawSocket = true
		case "/sync":
			sawSync = true
		}
		mu.Unlock()
		unauthorized(w)
	}))
	t.Cleanup(srv.Close)
	j := NewJournal()
	if err := j.Enroll(Credential{
		DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken("access-0"), RefreshToken: padToken("refresh-0"),
		AccessExpiresAt: time.Now().Add(10 * time.Second), // due
	}); err != nil {
		t.Fatal(err)
	}
	c, err := New(fastBackoff(Config{BaseURL: srv.URL, Journal: j, Refresh: true}))
	if err != nil {
		t.Fatal(err)
	}
	term := terminalOf(runToEnd(t, c, 5*time.Second))
	if term.Kind != TerminalCredential {
		t.Fatalf("terminal %+v, want a credential terminal from the proactive refresh", term)
	}
	if s := c.Status(); s.Dials != 0 || s.DialErrors != 0 || s.Terminal != term {
		t.Errorf("dials %d, dial errors %d, status terminal %+v; want 0, 0 and the terminal Run returned", s.Dials, s.DialErrors, s.Terminal)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawSocket || sawSync {
		t.Errorf("a client terminal before its first dial still reached the server: ws=%v sync=%v", sawSocket, sawSync)
	}
}
