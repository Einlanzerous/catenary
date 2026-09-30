package client

// CANT-170: CANT-35 ruling 3 → B in the Go reference, under a fake clock. The
// same claims web/src/transport/test/backoff.test.ts makes of the TypeScript
// transport (CANT-35 criterion 9): a 10-minute outage in which every dial fails
// stays within 156 dials at the minimum draw, and so does a path that answers
// `ready` and drops at once. Each rule has a named negative control, a Faults
// switch that restores the client as it was before the ruling, and each control
// must FAIL the check it stands against — a bound nobody has seen broken is a
// claim about the instrument, not the client.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// draws is a Config.Jitter whose every byte is the same: 0 is the minimum draw
// (the worst case for dial counts), 0xff the maximum.
type draws byte

func (b draws) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

var (
	minimumDraw = draws(0x00)
	maximumDraw = draws(0xff)
)

const (
	tenMinutes = 10 * time.Minute
	// outageBound is CANT-35 criterion 9's bound, as the TypeScript asserts it.
	outageBound = 156
)

// dialPlan is what the server does with the n-th upgrade (from 0).
type dialPlan int

const (
	failDial      dialPlan = iota // 503 on the upgrade: a dial that never opens
	readyThenDrop                 // `ready`, then the socket is gone at once
	readyAndHold                  // `ready`, then open until the test releases it
)

// backoffRig is a client whose clock only moves when Run waits between dials,
// and by exactly what it waits: the dial itself costs no simulated time.
type backoffRig struct {
	t      *testing.T
	srv    *httptest.Server
	c      *Client
	clk    *fakeClock
	start  time.Time
	plan   func(n int) dialPlan
	held   chan chan struct{}
	quit   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	dials int
	waits []time.Duration
}

// newBackoffRig runs a client until limit of simulated time has passed or it
// has made stopAt dials, whichever is first. A control can then fail by a wide
// margin without making thousands of real sockets.
func newBackoffRig(t *testing.T, plan func(n int) dialPlan, faults Faults, jitter draws, limit time.Duration, stopAt int) *backoffRig {
	t.Helper()
	r := &backoffRig{t: t, clk: newClock(time.Now()), plan: plan, held: make(chan chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
	r.start = r.clk.now()
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	t.Cleanup(func() { close(r.quit) }) // before the server's Close: cleanups run last-in first
	c, err := New(Config{BaseURL: r.srv.URL, Journal: enrolledJournal(t, "token"), Jitter: jitter, Faults: faults})
	if err != nil {
		t.Fatal(err)
	}
	c.now = r.clk.now
	c.sleep = func(ctx context.Context, d time.Duration) bool {
		r.mu.Lock()
		r.waits = append(r.waits, d)
		dials := r.dials
		r.mu.Unlock()
		r.clk.add(d)
		if r.clk.now().Sub(r.start) > limit || dials >= stopAt {
			r.cancel()
			return false
		}
		return ctx.Err() == nil
	}
	r.c = c
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { defer close(r.done); _ = c.Run(ctx) }()
	t.Cleanup(func() { c.Kill(); <-r.done })
	return r
}

func (r *backoffRig) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/sync" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"log_seq":0,"messages":[],"conversations":[],"users":[],"has_more":false,"server_time":"2026-01-01T00:00:00.000Z"}`))
		return
	}
	r.mu.Lock()
	n := r.dials
	r.dials++
	r.mu.Unlock()
	plan := r.plan(n)
	if plan == failDial {
		http.Error(w, "the network is down", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := req.Context()
	if _, _, err := conn.Read(ctx); err != nil {
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, readyFrame(r.t)); err != nil { // heartbeat_interval_sec 30
		return
	}
	if plan == readyAndHold {
		release := make(chan struct{})
		select {
		case r.held <- release:
		case <-r.quit:
			return
		}
		select {
		case <-release:
		case <-r.quit:
		}
	}
}

// wait blocks until Run has returned: the limit, or the dial cap, was reached.
func (r *backoffRig) wait() {
	r.t.Helper()
	select {
	case <-r.done:
	case <-time.After(60 * time.Second):
		r.t.Fatalf("the rig did not finish; %d dials, %s simulated", r.dialCount(), r.clk.now().Sub(r.start))
	}
}

func (r *backoffRig) dialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dials
}

func (r *backoffRig) waitsSoFar() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.waits)
}

func ms(v ...int) []time.Duration {
	out := make([]time.Duration, len(v))
	for i, x := range v {
		out[i] = time.Duration(x) * time.Millisecond
	}
	return out
}

// --- the checks, each a claim a negative control must break -------------------

// outageAt is a 10-minute outage from the floor, every dial failing (or, with
// plan, answering however plan says), at one fixed draw. It returns the dials
// made — the first included — and the waits between them.
func outageAt(t *testing.T, plan func(int) dialPlan, faults Faults, jitter draws) (int, []time.Duration) {
	t.Helper()
	r := newBackoffRig(t, plan, faults, jitter, tenMinutes, 4*outageBound)
	r.wait()
	return r.dialCount(), r.waitsSoFar()
}

func always(p dialPlan) func(int) dialPlan { return func(int) dialPlan { return p } }

// checkOutageBound: every dial fails for ten minutes, and at the minimum draw
// that is at most 156 dials — and at least 150, so the bound is not vacuous. The
// ramp is 250, 500, 1000, 2000, 4000, then the 5 s ceiling, each drawn at 0.8×.
func checkOutageBound(t *testing.T, faults Faults) error {
	dials, waits := outageAt(t, always(failDial), faults, minimumDraw)
	t.Logf("every dial failing, minimum draw: %d dials in ten minutes", dials)
	if dials > outageBound || dials < 150 {
		return fmt.Errorf("%d dials in a ten-minute outage, want 150–%d", dials, outageBound)
	}
	if want := ms(200, 400, 800, 1600, 3200, 4000, 4000); !slices.Equal(waits[:len(want)], want) {
		return fmt.Errorf("waits %v at the minimum draw, want %v", waits[:len(want)], want)
	}
	return nil
}

// checkReadyThenDrop: a path that answers `ready` and drops at once readied for
// no time at all, so no session resets the ramp and it climbs exactly as a dead
// network's does.
func checkReadyThenDrop(t *testing.T, faults Faults) error {
	dials, _ := outageAt(t, always(readyThenDrop), faults, minimumDraw)
	t.Logf("ready then dropped, minimum draw: %d dials in ten minutes", dials)
	if dials > outageBound {
		return fmt.Errorf("%d dials in ten minutes of ready-then-drop (reset-on-any-ready makes ~3000, and the rig stops at %d), want at most %d", dials, 4*outageBound, outageBound)
	}
	return nil
}

// checkStability: five failed dials grow the ramp to 4 s; a session ready for
// one millisecond less than its 30 s interval leaves it climbing (5 s next); one
// ready for the whole interval resets it to the floor. At the maximum draw, so
// every wait is its nominal.
func checkStability(t *testing.T, faults Faults) error {
	interval := 30 * time.Second
	plan := func(n int) dialPlan {
		if n < 5 {
			return failDial
		}
		return readyAndHold
	}
	r := newBackoffRig(t, plan, faults, maximumDraw, time.Hour, 7)
	for _, held := range []time.Duration{interval - time.Millisecond, interval} {
		select {
		case release := <-r.held:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := r.c.Await(ctx, func() bool { return r.c.Status().Ready })
			cancel()
			if err != nil {
				return fmt.Errorf("the held session never readied: %w", err)
			}
			r.clk.add(held)
			close(release)
		case <-time.After(30 * time.Second):
			return errors.New("the held session never arrived")
		}
	}
	var got []time.Duration
	if err := awaitErr(func() bool { got = r.waitsSoFar(); return len(got) >= 7 }); err != nil {
		return fmt.Errorf("only %d waits: %v", len(got), got)
	}
	if want := ms(250, 500, 1000, 2000, 4000, 5000, 250); !slices.Equal(got[:7], want) {
		return fmt.Errorf("waits %v, want %v: short session → the ramp carries on, stable session → the floor", got[:7], want)
	}
	return nil
}

// checkMaximumDraw: at the maximum draw every wait is exactly its nominal, the
// ceiling included, and the outage stays inside the bound too.
func checkMaximumDraw(t *testing.T, faults Faults) error {
	dials, waits := outageAt(t, always(failDial), faults, maximumDraw)
	t.Logf("every dial failing, maximum draw: %d dials in ten minutes", dials)
	if dials > outageBound {
		return fmt.Errorf("%d dials, want at most %d", dials, outageBound)
	}
	if want := ms(250, 500, 1000, 2000, 4000, 5000, 5000); !slices.Equal(waits[:len(want)], want) {
		return fmt.Errorf("waits %v at the maximum draw, want %v", waits[:len(want)], want)
	}
	if m := slices.Max(waits); m > defaultBackoffMax {
		return fmt.Errorf("a wait of %s is above the %s ceiling", m, defaultBackoffMax)
	}
	return nil
}

func awaitErr(pred func() bool) error {
	deadline := time.Now().Add(30 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			return errors.New("timed out")
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

func TestBackoffATenMinuteOutageStaysWithinTheBound(t *testing.T) {
	if err := checkOutageBound(t, Faults{}); err != nil {
		t.Error(err)
	}
	if err := checkMaximumDraw(t, Faults{}); err != nil {
		t.Error(err)
	}
}

func TestBackoffReadyThenDropStaysWithinTheBound(t *testing.T) {
	if err := checkReadyThenDrop(t, Faults{}); err != nil {
		t.Error(err)
	}
}

func TestBackoffOnlyAStableSessionResetsTheRamp(t *testing.T) {
	if err := checkStability(t, Faults{}); err != nil {
		t.Error(err)
	}
}

// Each negative control restores one pre-ruling behaviour and must fail the
// check it names: the check can see the rule it claims to pin.
func TestBackoffNegativeControls(t *testing.T) {
	for _, nc := range []struct {
		name   string
		faults Faults
		check  func(*testing.T, Faults) error
	}{
		{"Faults{ResetOnAnyReady} breaks the ready-then-drop bound", Faults{ResetOnAnyReady: true}, checkReadyThenDrop},
		{"Faults{ResetOnAnyReady} resets after a short session", Faults{ResetOnAnyReady: true}, checkStability},
		{"Faults{NoJitter} waits the nominal at the minimum draw", Faults{NoJitter: true}, checkOutageBound},
	} {
		t.Run(nc.name, func(t *testing.T) {
			err := nc.check(t, nc.faults)
			if err == nil {
				t.Fatal("the control passes: the check does not pin this rule")
			}
			t.Logf("fails, as it must: %v", err)
		})
	}
}
