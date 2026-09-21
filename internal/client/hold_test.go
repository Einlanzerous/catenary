package client

// CANT-127: what bounds the chain, measured.
//
// The claim these tests exist to make is a NUMBER: eight hours of a dead
// network cost one link rather than ~5,700, and a day of black-holed /refresh
// costs about a hundred rather than ~17,000. So the rigs below simulate hours
// rather than label them — the transport advances the device's wall clock by
// Run's own dial ceiling on every dial, and the dial count is asserted — and
// every suppressor has a control that is WATCHED FAILING with it faulted off
// (Faults.Unbounded).
//
// HOW THE CLOCK AND THE REAL PACING COEXIST. Run paces itself with time.After,
// which no test can fake, and only wallNow reads Config.Now. So the drives set
// the real backoff to microseconds and let the fake clock carry the hours: 8 h
// is 8h/5s = 5,760 dials, and the dial count is asserted as that quotient rather
// than as a literal.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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

// simPerDial is what one dial costs in SIMULATED time: Run's dial backoff
// ceiling, which is the rate a client redials at once its backoff has grown, and
// the rate the ticket's ~720-links-an-hour measurement is taken at.
const simPerDial = defaultBackoffMax

// --- the instruments ---------------------------------------------------------

// fakeClock is the device's wall clock under the test's control. Config.Now
// reads it and nothing else does, so Run's own pacing stays real time.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newClock(at time.Time) *fakeClock { return &fakeClock{at: at.Round(0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at.Round(0)
	c.mu.Unlock()
}

// countingRand is a device whose generator is DETERMINISTIC and never repeats:
// the k-th proposal of one run is the k-th proposal of the next. Criterion 7's
// oracle needs exactly that — it compares two runs request for request, and a
// CSPRNG would make every byte differ for reasons having nothing to do with the
// suppressors. Each client gets its own, seeded apart, so two clients over one
// credential never mint the same successor.
type countingRand struct {
	mu sync.Mutex
	n  uint64
}

func (r *countingRand) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	for i := range p {
		p[i] = 0
	}
	for i := 0; i < 8 && i < len(p); i++ {
		p[len(p)-1-i] = byte(r.n >> (8 * i))
	}
	return len(p), nil
}

// logLines collects the client's own account of itself at Info and above, so a
// test can COUNT lines rather than eyeball them: criterion 11(b) is an assertion
// about how many, over 5,760 dials.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Enabled(context.Context, slog.Level) bool { return true }

func (l *logLines) Handle(_ context.Context, r slog.Record) error {
	line := r.Level.String() + " " + r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	return nil
}

func (l *logLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logLines) WithGroup(string) slog.Handler      { return l }
func (l *logLines) logger() *slog.Logger               { return slog.New(l) }

func (l *logLines) matching(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return out
}

// --- the estate --------------------------------------------------------------

// estate is chain_test.go's family — the refresh family, with the replay and
// revocation bookkeeping every test here checks — plus the two surfaces a Run
// needs: /sync and the upgrade. Both refuse any access token but the one the
// family last minted, which is what an expired pair looks like from the server's
// side.
type estate struct {
	t   *testing.T
	f   *family
	srv *httptest.Server

	mu sync.Mutex
	// first is the enrollment access token while it is still good, and "" for a
	// device that has been away long enough for it to have expired.
	first string
	// answer, when set, is what /sync replies instead of an honest page: a hop's
	// 401, a captive portal's 200, a 502 (criterion 4).
	answer func(w http.ResponseWriter)
	syncs  int
	// frames is raw bytes a test writes on the open socket, decodable or not.
	frames chan []byte
}

func newEstate(t *testing.T, firstAccess string, script ...string) *estate {
	t.Helper()
	e := &estate{t: t, f: newFamily(t, firstRefresh, script...), first: firstAccess, frames: make(chan []byte, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/refresh", e.f.serve)
	mux.HandleFunc("/sync", e.sync)
	mux.HandleFunc("/ws", e.ws)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// liveAccess is the access token this estate accepts: the one the family's last
// rotation minted, or the enrollment token while there has been none.
func (e *estate) liveAccess() string {
	e.f.mu.Lock()
	n := e.f.minted
	e.f.mu.Unlock()
	if n == 0 {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.first
	}
	return padToken("access-" + string(rune('0'+n)))
}

func (e *estate) sync(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.syncs++
	answer := e.answer
	e.mu.Unlock()
	if answer != nil {
		answer(w)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if live := e.liveAccess(); live == "" || tok != live {
		unauthorized(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"log_seq":0,"messages":[],"conversations":[],"users":[],"has_more":false,"server_time":"2026-01-01T00:00:00.000Z"}`))
}

func (e *estate) syncCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.syncs
}

func (e *estate) ws(w http.ResponseWriter, r *http.Request) {
	var tok string
	for _, sp := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sp), tokenSubprotocolPrefix); ok {
			tok = v
		}
	}
	if live := e.liveAccess(); live == "" || tok != live {
		unauthorized(w)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := context.Background()
	if _, _, err := conn.Read(ctx); err != nil { // the hello
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, readyFrame(e.t)); err != nil {
		return
	}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case raw := <-e.frames:
			if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
				return
			}
		case <-gone:
			return
		}
	}
}

// journal is a device enrolled with the family's first refresh token and an
// access token already inside the refresh floor, so every dial finds the pair
// due and the suppressors are the only thing that can stop one.
func (e *estate) journal(t *testing.T, now time.Time) *Journal {
	t.Helper()
	j := NewJournal()
	if err := j.Enroll(Credential{
		DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken("access-0"), AccessExpiresAt: now.Add(10 * time.Second),
		RefreshToken: firstRefresh, RefreshExpiresAt: now.Add(60 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return j
}

// --- the network -------------------------------------------------------------

// drive is the transport every rig here runs on. It does three things: it fails
// the surfaces an outage takes down, it advances the fake clock by the dial
// ceiling on EVERY dial, and it records every /refresh the client SENT —
// arrived or not, which is what criterion 7's oracle compares, since a request
// into a dead network never reaches the family at all.
type drive struct {
	clk   *fakeClock
	start time.Time
	// span is how long the outage lasts in simulated time, and heal says whether
	// it ever ends.
	span time.Duration
	heal bool
	dead map[string]bool
	base http.RoundTripper
	j    *Journal

	mu            sync.Mutex
	dials         int
	sent          []presentation
	returned      bool
	chainAtReturn int
	sentAtReturn  int
	dialsAtReturn int
}

func newDrive(clk *fakeClock, j *Journal, span time.Duration, heal bool, dead ...string) *drive {
	d := &drive{clk: clk, start: clk.now(), span: span, heal: heal, dead: map[string]bool{}, base: http.DefaultTransport, j: j}
	for _, p := range dead {
		d.dead[p] = true
	}
	return d
}

func (d *drive) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/refresh" {
		d.record(r)
	}
	// DECIDED BEFORE THE ADVANCE, so a dial's fate belongs to the moment it was
	// made: span/simPerDial dials fail and the next one is let through.
	down := d.down()
	if r.URL.Path == "/ws" {
		d.mu.Lock()
		d.dials++
		d.mu.Unlock()
		d.clk.add(simPerDial)
	}
	if down && d.dead[r.URL.Path] {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("the network is down")}
	}
	return d.base.RoundTrip(r)
}

func (d *drive) down() bool {
	if d.clk.now().Sub(d.start) < d.span {
		return true
	}
	if !d.heal {
		return true
	}
	d.mu.Lock()
	if !d.returned {
		// WHAT THE OUTAGE COST, read at the moment it ends and before recovery
		// can change any of it. The dial count is read here rather than from
		// Stats.DialErrors because the dials the RECOVERY spends — the upgrade is
		// refused until the walk has rotated the pair — belong to the walk and
		// not to the outage.
		d.returned, d.sentAtReturn, d.dialsAtReturn = true, len(d.sent), d.dials
		if d.j != nil {
			d.chainAtReturn = len(d.j.Chain())
		}
	}
	d.mu.Unlock()
	return false
}

// record reads the /refresh body with the generated decoder and puts it back, so
// the request is untouched and the tape is what the wire carried.
func (d *drive) record(r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	var req wire.RefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	p := presentation{token: string(req.RefreshToken)}
	if req.ProposedRefreshToken != nil {
		p.proposal = string(*req.ProposedRefreshToken)
	}
	d.mu.Lock()
	d.sent = append(d.sent, p)
	d.mu.Unlock()
}

func (d *drive) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

func (d *drive) tape() []presentation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]presentation(nil), d.sent...)
}

func (d *drive) cost() (chain, sent, dials int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.chainAtReturn, d.sentAtReturn, d.dialsAtReturn
}

// driveBackoff is the rigs' REAL pacing, and it is microseconds rather than
// fastBackoff's milliseconds for one reason: 5,760 dials at a 20 ms ceiling is
// two minutes of wall clock for eight simulated hours, and the catch-up
// goroutine shares this pair — a nanosecond there is a busy loop against the
// stub. The SIMULATED pacing is the fake clock, which the transport advances by
// the real 5 s dial ceiling on every dial, so the dial count and the hours still
// agree.
func driveBackoff(cfg Config) Config {
	cfg.BackoffMin, cfg.BackoffMax = 50*time.Microsecond, 50*time.Microsecond
	return cfg
}

// --- the rig -----------------------------------------------------------------

type rigOpts struct {
	// span and heal describe the outage: how long it lasts in simulated time,
	// and whether it ends. A zero span is a network that was never down.
	span time.Duration
	heal bool
	// dead names the surfaces the outage takes down, by path.
	dead []string
	// accessLive keeps the enrollment access token good on the server's side; the
	// default is a device that has been away long enough for it to have expired.
	accessLive bool
	script     []string
	faults     Faults
	rand       io.Reader
}

type rig struct {
	e   *estate
	clk *fakeClock
	net *drive
	log *logLines
	j   *Journal
	c   *Client
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	clk := newClock(time.Now())
	first := ""
	if o.accessLive {
		first = padToken("access-0")
	}
	e := newEstate(t, first, o.script...)
	j := e.journal(t, clk.now())
	r := &rig{e: e, clk: clk, net: newDrive(clk, j, o.span, o.heal, o.dead...), log: &logLines{}, j: j}
	r.c = r.client(t, o.faults, o.rand)
	return r
}

// client is another context over the same credential and the same network: a
// second tab, or an app beside its push worker.
func (r *rig) client(t *testing.T, faults Faults, rnd io.Reader) *Client {
	t.Helper()
	c, err := New(driveBackoff(Config{
		BaseURL: r.e.srv.URL, Journal: r.j, Refresh: true, Now: r.clk.now, Logger: r.log.logger(),
		HTTPClient: &http.Client{Transport: r.net}, Rand: rnd, Faults: faults,
		// CANT-129'S WAIT SLEEPS THROUGH THIS SEAM, and these rigs have to charge
		// it simPerDial rather than the duration it asked for. A refused client
		// makes no dials, and the dial is the only thing that advances the fake
		// clock here — so without a seam the black-holed-/refresh rig freezes at
		// the first hold. And the wait polls at c.backoffMax, which driveBackoff
		// compresses to microseconds so that simulated hours cost seconds:
		// charging `d` would advance the clock by 50 µs a poll and a
		// fifteen-minute hold would never end (measured: 185,616 polls, 9.3
		// simulated seconds, in 180 s of real time). simPerDial is the same
		// accounting the transport uses for a dial — one poll of the wait stands
		// in for one dial not made, and costs what that dial would have cost.
		Pause: func(ctx context.Context, _ time.Duration) bool {
			time.Sleep(50 * time.Microsecond)
			r.clk.add(simPerDial)
			return ctx.Err() == nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (r *rig) run() (stop func()) {
	done := make(chan struct{})
	go func() { defer close(done); _ = r.c.Run(context.Background()) }()
	return sync.OnceFunc(func() { r.c.Kill(); <-done })
}

// awaitDials waits for the drive to have made n dials, which is how a test waits
// for simulated hours to pass.
func (r *rig) awaitDials(t *testing.T, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := r.c.Await(ctx, func() bool { return r.net.dialCount() >= n }); err != nil {
		t.Fatalf("only %d of %d dials in the time allowed: %v; status %+v", r.net.dialCount(), n, err, r.c.Status())
	}
}

// awaitFor polls, for the two states no client change notifies: a frame the
// decoder refuses is counted and skipped without one, and a gate that opens is
// not a change to anything Await watches — which is criterion 9(d) working.
func awaitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// buildLink is an EXPLICIT refresh into a black-holed /refresh: one link, one
// stamp, and the wall-clock time the stamp holds. RefreshIfDue is never held
// (criterion 5), so this is also the only way a test can put a client in the
// unsettled state the suppressors are about.
func (r *rig) buildLink(t *testing.T) time.Time {
	t.Helper()
	at := r.clk.now()
	links := len(r.j.Chain())
	if err := r.c.RefreshIfDue(context.Background()); err == nil {
		t.Fatal("the black-holed /refresh reported success")
	}
	if got := len(r.j.Chain()); got != links+1 {
		t.Fatalf("the chain is %d links, want %d", got, links+1)
	}
	sent, ok := r.j.LastSent()
	if !ok || !sent.Equal(at) {
		t.Fatalf("last_sent_at is %v (present=%v), want the wall clock at the write, %v", sent, ok, at)
	}
	return at
}

// assertNoReplays is criterion 7's floor, and no test here is exempt from it:
// whatever a suppressor does or does not do, a spent token is never presented
// and the family is never revoked.
func assertNoReplays(t *testing.T, f *family) {
	t.Helper()
	seen, _, replays, revoked := f.record()
	if replays != 0 || revoked {
		t.Errorf("a spent token was presented %d times and the family is revoked=%v; presentations %v", replays, revoked, seen)
	}
}

func count401s(statuses []int) int {
	var n int
	for _, s := range statuses {
		if s == http.StatusUnauthorized {
			n++
		}
	}
	return n
}

// subsequence reports whether want's elements appear in got, in order and
// without anything between them being needed — got with some elements REMOVED
// and none added or reordered.
func subsequence(sub, all []presentation) bool {
	i := 0
	for _, p := range all {
		if i < len(sub) && sub[i] == p {
			i++
		}
	}
	return i == len(sub)
}

// --- criterion 1 · the ticket's own test -------------------------------------

// EIGHT SIMULATED HOURS OF A DEAD NETWORK COST ONE LINK (criterion 1(a)), and
// the walk on return is one `unknown credential` and then the live token.
//
// Also criterion 11(b): across 5,760 dials the client says O(1) things about
// refreshing — one line for the one attempt that was actually made and failed,
// and one each for the gate closing and opening. Not one per held attempt.
func TestEightHoursOfADeadNetworkCostsOneLink(t *testing.T) {
	const span = 8 * time.Hour
	wantDials := int(span / simPerDial) // 5,760
	r := newRig(t, rigOpts{span: span, heal: true, dead: []string{"/ws", "/sync", "/refresh"}})
	stop := r.run()
	defer stop()

	// The network returns when the fake clock passes the span. The /sync beside
	// the next dial is refused by Catenary, which opens the gate, and the
	// reactive half walks to the live token.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := r.c.Await(ctx, func() bool { s := r.c.Status(); return s.Refreshes == 1 && s.Ready }); err != nil {
		t.Fatalf("the client never rotated and reconnected: %v; status %+v", err, r.c.Status())
	}
	stop()

	s := r.c.Status()
	chain, sent, dials := r.net.cost()
	if dials != wantDials {
		t.Errorf("the outage cost %d dials, want %d — the simulated hours and the dial count must agree", dials, wantDials)
	}
	if s.DialErrors < wantDials {
		t.Errorf("%d failed dials in all, want at least the outage's %d", s.DialErrors, wantDials)
	}
	if chain > 1 {
		t.Errorf("the persisted chain at the moment the network returned was %d links, want at most 1", chain)
	}
	if sent != 1 {
		t.Errorf("%d /refresh requests went out over %s of a dead network, want the 1 that made the link", sent, span)
	}
	seen, statuses, _, _ := r.e.f.record()
	if got := count401s(statuses); got > 1 {
		t.Errorf("the server refused %d tokens as unknown before the live one answered, want at most 1; presentations %v", got, seen)
	}
	assertNoReplays(t, r.e.f)
	cr, _ := r.j.Credential()
	if cr.RefreshToken == firstRefresh || cr.RefreshToken != r.e.f.live() {
		t.Errorf("the client holds %q, the server's live token is %q", cr.RefreshToken, r.e.f.live())
	}
	if got := len(r.j.Chain()); got != 0 {
		t.Errorf("the chain is %d links after the answer, want empty", got)
	}
	if s.RefreshesHeldUnreachable < wantDials-2 {
		t.Errorf("%d attempts were held by the gate over %d dials; the suppressor is not what stopped them", s.RefreshesHeldUnreachable, wantDials)
	}
	if s.RefreshErrors != 1 || s.RefreshesHeldBackoff != 0 {
		t.Errorf("refresh errors %d and backoff holds %d, want the one real failure and nothing counted twice", s.RefreshErrors, s.RefreshesHeldBackoff)
	}

	// CRITERION 11(b), by count. The one `proactive refresh failed` line is the
	// attempt that WAS made, at the start of the outage, and failed; the 5,759
	// held ones say nothing at all.
	about := r.log.matching("refresh")
	if len(about) > 4 {
		t.Errorf("%d log lines about refreshing over %d dials, want O(1):\n%s", len(about), wantDials, strings.Join(about, "\n"))
	}
	if failed := r.log.matching("proactive refresh failed"); len(failed) != 1 {
		t.Errorf("%d `proactive refresh failed` lines, want the single real failure: %v", len(failed), failed)
	}
	// ONE LINE PER TRANSITION, and there are two of them here: the gate closed
	// when the network died and opened when Catenary answered. Bounded rather
	// than counted exactly, because Run's loop and the recovery's own walk can
	// each observe the stamp moving; criterion 12 pins the one-per-transition
	// rule where it is deterministic.
	closed, opened := r.log.matching("refresh gate closed"), r.log.matching("refresh gate open")
	if len(closed) < 1 || len(opened) < 1 || len(closed)+len(opened) > 3 {
		t.Errorf("the gate logged %d closes and %d opens over %d dials, want one each and O(1) in any case", len(closed), len(opened), wantDials)
	}
}

// --- criterion 2 · the control, watched failing ------------------------------

// THE SAME EIGHT HOURS WITH THE SUPPRESSORS FAULTED OFF mint a link per dial. It
// is what every client did before this ticket, and criterion 1 cannot pass
// against a build that simply never mints links.
func TestWithoutTheSuppressorsEveryDialMintsALink(t *testing.T) {
	const span = 8 * time.Hour
	wantDials := int(span / simPerDial)
	r := newRig(t, rigOpts{span: span, dead: []string{"/ws", "/sync", "/refresh"}, faults: Faults{Unbounded: true}})
	stop := r.run()
	defer stop()
	r.awaitDials(t, wantDials)
	chain := len(r.j.Chain())
	stop()

	if chain <= 5000 {
		t.Errorf("the control's chain is %d links after %d dials, want the same order as the dials", chain, wantDials)
	}
	if chain < wantDials-2 {
		t.Errorf("the control's chain is %d links after %d dials, want one per dial", chain, wantDials)
	}
	assertNoReplays(t, r.e.f)
}

// --- criterion 7 · nothing is added or reordered -----------------------------

// THE SUPPRESSED CLIENT SENDS A SUBSET OF WHAT THE UNSUPPRESSED ONE SENT, in the
// same order. Both runs are the same script over the same deterministic
// generator, so the tapes are comparable request for request: what the
// suppressors do is REMOVE requests, and they never add one, reorder one, or
// change which token is presented with which proposal.
func TestTheSuppressorsOnlyRemoveRequests(t *testing.T) {
	const span = time.Hour
	wantDials := int(span / simPerDial) // 720
	tape := func(faults Faults) []presentation {
		r := newRig(t, rigOpts{span: span, dead: []string{"/ws", "/sync", "/refresh"}, faults: faults, rand: &countingRand{}})
		stop := r.run()
		defer stop()
		r.awaitDials(t, wantDials)
		stop()
		assertNoReplays(t, r.e.f)
		return r.net.tape()
	}
	held, unbounded := tape(Faults{}), tape(Faults{Unbounded: true})

	if len(held) >= len(unbounded) {
		t.Fatalf("the suppressed client sent %d requests and the unsuppressed one %d; the comparison proves nothing unless fewer went out", len(held), len(unbounded))
	}
	if !subsequence(held, unbounded) {
		t.Errorf("the suppressed tape is not the unsuppressed one with requests removed:\nheld      %v\nunbounded %v", held, unbounded[:min(len(unbounded), 8)])
	}
	for i, p := range held {
		if p.proposal == "" || p.token == "" {
			t.Errorf("request %d carried %+v; every /refresh presents a token with a proposal", i, p)
		}
	}
}

// --- criterion 3 · the case the gate cannot cover ----------------------------

// /sync ANSWERS AND /refresh IS BLACK-HOLED FOR 24 SIMULATED HOURS. The gate is
// open on every cycle — Catenary is answering — so the backoff is the only thing
// bounding the growth, and it holds the chain to about a hundred links instead
// of the ~17,000 the dial rate would give. Then /refresh heals and the walk is
// NOT capped: one attempt reaches the live token.
func TestABlackHoledRefreshIsBoundedByTheBackoff(t *testing.T) {
	const span = 24 * time.Hour
	// 8 links to climb the curve to the cap, one per cap after that, and one for
	// the attempt that straddles the moment it heals.
	bound := 8 + int(span/refreshBackoffCap) + 1
	// /refresh IS BLACK-HOLED AND THE UPGRADE CANNOT BE ESTABLISHED, which is the
	// shape of the case: Catenary answers /sync — with its own 401, since the
	// access token is long expired — so the gate is open on every cycle and the
	// backoff is alone.
	r := newRig(t, rigOpts{span: span, heal: true, dead: []string{"/refresh", "/ws"}})
	stop := r.run()
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err := r.c.Await(ctx, func() bool { s := r.c.Status(); return s.Refreshes == 1 && s.Ready }); err != nil {
		t.Fatalf("the client never rotated and reconnected: %v; status %+v", err, r.c.Status())
	}
	stop()

	chain, sent, _ := r.net.cost()
	if chain > bound {
		t.Errorf("the chain was %d links after %s of black-holed /refresh, want at most %d", chain, span, bound)
	}
	if chain < 8 {
		t.Errorf("the chain was only %d links; the backoff cannot have been what bounded it", chain)
	}
	if sent != chain {
		t.Errorf("%d /refresh requests went out for %d links; an attempt that settles nothing is exactly one link", sent, chain)
	}
	s := r.c.Status()
	if s.RefreshWalkBacks != chain {
		t.Errorf("%d walk-backs for a chain of %d; the walk on return is one step per link, in ONE attempt", s.RefreshWalkBacks, chain)
	}
	// THE BACKOFF IS WHAT BOUNDED IT, AND SINCE CANT-129 IT IS WAITED OUT RATHER
	// THAN ATTEMPTED INTO. Before that ticket every dial's proactive refresh met
	// the delay and was counted as held, so RefreshesHeldBackoff was the evidence
	// here. Now Catenary's own 401 on /sync marks the access token refused, Run
	// stops dialing, and the one attempt per hold goes out THROUGH the probe at
	// the moment the delay has elapsed — so nothing is left to be held by it, and
	// that counter is structurally zero in this arrangement. What must not happen
	// is NEITHER: a build where the delay neither holds an attempt nor stands one
	// down has lost the suppressor this test is about. The bound itself is
	// unchanged and is asserted above, on the chain.
	if s.RefreshesHeldBackoff+s.SyncsWithheld == 0 {
		t.Error("the delay neither held an attempt nor stood a request down; the gate cannot have been open")
	}
	assertNoReplays(t, r.e.f)
	if cr, _ := r.j.Credential(); cr.RefreshToken != r.e.f.live() {
		t.Errorf("the client holds %q, the server's live token is %q", cr.RefreshToken, r.e.f.live())
	}
}

// THE CURVE ITSELF, as record §3 states it: 5 s doubling to a 15-minute cap,
// counted from the send. Asserted as arithmetic rather than as a table of
// literals, and past the cap it is the cap however long the chain gets.
func TestTheDelayDoublesToTheCap(t *testing.T) {
	if got := refreshDelay(0); got != 0 {
		t.Errorf("a settled credential waits %s, want nothing", got)
	}
	want := refreshBackoffBase
	for links := 1; links <= 8; links++ {
		if got := refreshDelay(links); got != want {
			t.Errorf("%d links wait %s, want %s", links, got, want)
		}
		want = min(want*2, refreshBackoffCap)
	}
	for _, links := range []int{9, 64, 1_000, 1_000_000} {
		if got := refreshDelay(links); got != refreshBackoffCap {
			t.Errorf("%d links wait %s, want the cap %s", links, got, refreshBackoffCap)
		}
	}
	// The eight links the plan's number comes from: about 21 minutes in total.
	var total time.Duration
	for links := 1; links <= 8; links++ {
		total += refreshDelay(links)
	}
	if total < 20*time.Minute || total > 22*time.Minute {
		t.Errorf("the first eight links take %s, want about 21 minutes", total)
	}
}

// --- criterion 4 · only Catenary opens the gate ------------------------------

// ONLY CATENARY OPENS THE GATE, and only an answer LATER than the last send.
// Everything else on the way — a hop's 401, a captive portal's 200, a 502, a
// network error — leaves it closed, and so does an answer that arrives at or
// before the stamp.
func TestOnlyCatenaryOpensTheGate(t *testing.T) {
	catenary401 := func(w http.ResponseWriter) { unauthorized(w) }
	proxy401 := func(w http.ResponseWriter) { http.Error(w, "access: session expired", http.StatusUnauthorized) }
	portal200 := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>sign in to this network</html>"))
	}
	bad502 := func(w http.ResponseWriter) { http.Error(w, "bad gateway", http.StatusBadGateway) }

	for _, tc := range []struct {
		name string
		// answer is what /sync replies; nil is an honest page. dead fails the
		// request in the transport instead.
		answer   func(http.ResponseWriter)
		dead     bool
		wantOpen bool
	}{
		{name: "a /sync 200 the generated decoder accepts", wantOpen: true},
		{name: "Catenary's own 401 on /sync", answer: catenary401, wantOpen: true},
		{name: "a 401 from a hop in front", answer: proxy401},
		{name: "a 200 that does not decode as a SyncResponse", answer: portal200},
		{name: "a 502 from a hop in front", answer: bad502},
		{name: "a network error", dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dead := []string{"/refresh"}
			if tc.dead {
				dead = append(dead, "/sync")
			}
			r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: dead})
			r.buildLink(t)

			// PAST THE BACKOFF, so the gate is the only thing left to decide
			// anything — and past the SEND, so the answer below is strictly later
			// than the stamp and the tie is somebody else's test.
			r.clk.add(time.Minute)
			r.e.mu.Lock()
			r.e.answer = tc.answer
			r.e.mu.Unlock()

			// The answer under test. A Catenary 401 makes fetch refresh in the
			// same pass, which is the reactive half passing the gate by
			// construction; every other answer leaves fetch's 401 branch alone.
			before := len(r.net.tape())
			_, _ = r.c.fetch(context.Background(), 0)
			hold := r.c.refreshHold()
			_ = r.c.refreshDueWhenAllowed(context.Background())
			requests := len(r.net.tape()) - before

			switch {
			case tc.wantOpen && requests != 1:
				t.Errorf("the gate should have opened, and %d automatic /refresh requests went out; want exactly 1 (hold %v)", requests, hold)
			case !tc.wantOpen && requests != 0:
				t.Errorf("the gate should have stayed closed, and %d automatic /refresh requests went out (hold %v)", requests, hold)
			case !tc.wantOpen && hold != RefreshHeldUnreachable:
				t.Errorf("the hold is %v, want %v", hold, RefreshHeldUnreachable)
			}
			assertNoReplays(t, r.e.f)
		})
	}
}

// LATER THAN IS STRICT, AND EQUAL IS CLOSED. An answer that reached this context
// BEFORE its last send is no evidence the network still works, and one that
// arrived at the same instant is not either — the safe direction is one held
// attempt, never an extra one.
func TestAnAnswerNoLaterThanTheSendLeavesTheGateClosed(t *testing.T) {
	t.Run("the answer arrived before the send", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
		if _, err := r.c.fetch(context.Background(), 0); err != nil { // a page, first
			t.Fatal(err)
		}
		r.clk.add(10 * time.Second)
		r.buildLink(t) // and the send after it
		r.clk.add(time.Minute)

		if got := r.c.refreshHold(); got != RefreshHeldUnreachable {
			t.Errorf("hold %v, want the gate closed: the page arrived before the send", got)
		}
		before := len(r.net.tape())
		if err := r.c.refreshDueWhenAllowed(context.Background()); !errors.Is(err, errRefreshHeld) {
			t.Errorf("the automatic refresh = %v, want errRefreshHeld", err)
		}
		if got := len(r.net.tape()) - before; got != 0 {
			t.Errorf("%d /refresh requests went out", got)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("the answer and the send are the same instant", func(t *testing.T) {
		// Catenary's 401 on /sync over an EMPTY chain: the reactive refresh goes
		// out in the same pass and on the same clock reading, so the link it
		// leaves is stamped at exactly the time of the answer that allowed it.
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, dead: []string{"/refresh"}})
		at := r.clk.now()
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("fetch = %v, want Catenary's 401", err)
		}
		sent, ok := r.j.LastSent()
		if !ok || !sent.Equal(at) {
			t.Fatalf("the reactive attempt left stamp %v (present=%v), want %v", sent, ok, at)
		}
		if got := r.c.refreshHold(); got != RefreshHeldUnreachable {
			t.Errorf("hold %v with the answer and the send at the same instant, want the gate closed", got)
		}
		before := len(r.net.tape())
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the second fetch = %v, want Catenary's 401", err)
		}
		if got := len(r.net.tape()) - before; got != 0 {
			t.Errorf("%d /refresh requests went out on a tie", got)
		}
		// One minute later the same answer is strictly later, and it opens.
		r.clk.add(time.Minute)
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the third fetch = %v, want Catenary's 401", err)
		}
		if got := len(r.net.tape()) - before; got != 1 {
			t.Errorf("%d /refresh requests once the answer was later than the send, want 1", got)
		}
		assertNoReplays(t, r.e.f)
	})
}

// A SERVER FRAME OPENS IT TOO — on a socket that was already up as much as on a
// new one — and one the generated decoder REFUSES does not. Nothing a hop in
// front can write decodes as a frame.
func TestAServerFrameOpensTheGateAndNothingElseOnTheSocketDoes(t *testing.T) {
	r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
	stop := r.run()
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.c.Await(ctx, func() bool { s := r.c.Status(); return s.Ready && s.CaughtUp }); err != nil {
		t.Fatalf("the client never got a socket up: %v; status %+v", err, r.c.Status())
	}
	// THE NEW SOCKET'S OWN `ready` IS A FRAME, so the gate is open before there
	// is a chain at all. Build the link now, which moves the stamp past it.
	r.clk.add(time.Second)
	sent := r.buildLink(t)
	r.clk.add(time.Minute) // past the backoff; only the gate is left
	if got := r.c.refreshHold(); got != RefreshHeldUnreachable {
		t.Fatalf("with the last send after the last frame the hold is %v, want %v", got, RefreshHeldUnreachable)
	}

	// A frame the decoder REFUSES is not an answer.
	before := len(r.net.tape())
	r.e.frames <- []byte(`{"type":"ready","session_id":"not-a-uuid"}`)
	awaitFor(t, "the undecodable frame to arrive", func() bool { return r.c.Status().Undecodable > 0 })
	if got := r.c.refreshHold(); got != RefreshHeldUnreachable {
		t.Errorf("a frame the generated decoder refuses opened the gate: hold %v", got)
	}

	// One it ACCEPTS is, on the socket that was already up.
	r.e.frames <- []byte(`{"type":"ping","id":"hb-from-the-server"}`)
	awaitFor(t, "the accepted frame to open the gate", func() bool { return r.c.refreshHold() == RefreshNotHeld })
	// CRITERION 9(d): OPENING IT SENDS NOTHING. The socket is still up, so
	// nothing asks for a refresh, and none goes out.
	time.Sleep(20 * time.Millisecond)
	if got := len(r.net.tape()) - before; got != 0 {
		t.Errorf("%d /refresh requests went out on the gate opening; opening it is not a trigger", got)
	}
	if !sent.Before(r.clk.now()) {
		t.Fatal("the test never advanced its clock past the send; the comparison was a tie")
	}
	assertNoReplays(t, r.e.f)
}

// --- criterion 5 · the explicit entry point stays unconditional --------------

// AN EXPLICIT REFRESH IS NEVER HELD, which is what RefreshIfDue's doc comment
// says and what the three walk tests rely on — they call it back to back over a
// non-empty chain, which is only possible because the suppressors sit at Run's
// and fetch's call sites and not here.
func TestAnExplicitRefreshIsNeverHeld(t *testing.T) {
	r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, dead: []string{"/refresh"}})
	r.buildLink(t)
	// The gate is closed and the backoff has not elapsed: both suppressors say no.
	if got := r.c.refreshHold(); got != RefreshHeldUnreachable {
		t.Fatalf("hold %v, want the gate closed", got)
	}
	if err := r.c.refreshDueWhenAllowed(context.Background()); !errors.Is(err, errRefreshHeld) {
		t.Fatalf("the automatic call = %v, want errRefreshHeld", err)
	}

	before := len(r.net.tape())
	for range 3 {
		if err := r.c.RefreshIfDue(context.Background()); err == nil || errors.Is(err, errRefreshHeld) {
			t.Fatalf("an explicit RefreshIfDue = %v, want the plain failure of an attempt that was actually made", err)
		}
	}
	if got := len(r.net.tape()) - before; got != 3 {
		t.Errorf("three explicit refreshes sent %d requests, want 3 — each costs a link", got)
	}
	if got := len(r.j.Chain()); got != 4 {
		t.Errorf("the chain is %d links after one held attempt and three explicit ones, want 4", got)
	}
	assertNoReplays(t, r.e.f)
}

// --- criterion 6 · the reactive half still recovers a lost response ----------

// A ROTATION THAT COMMITTED AND WHOSE RESPONSE WAS LOST, recovered through fetch
// with no further input: the first /sync after the network returns is refused by
// Catenary, the reactive refresh walks to the live token, and the retried /sync
// succeeds with the rotated pair.
func TestTheReactiveHalfStillRecoversALostResponse(t *testing.T) {
	r := newRig(t, rigOpts{span: 0, script: []string{loseResponse}})
	// The rotation commits; the response never arrives.
	if err := r.c.RefreshIfDue(context.Background()); err == nil {
		t.Fatal("the lost response was not noticed")
	}
	if got := len(r.j.Chain()); got != 1 {
		t.Fatalf("the chain is %d links, want the one the lost rotation left", got)
	}
	r.clk.add(time.Minute) // the network was away; the backoff has long elapsed

	page, err := r.c.fetch(context.Background(), 0)
	if err != nil {
		t.Fatalf("fetch after the lost response: %v", err)
	}
	if page.ServerTime == "" {
		t.Errorf("the retried /sync returned %+v, want a decoded page", page)
	}
	s := r.c.Status()
	if s.Refreshes != 1 || s.RefreshWalkBacks != 0 || s.Terminal.Kind != NotTerminal {
		t.Errorf("refreshes %d, walk-backs %d, terminal %v; want one refresh, no walk-back — the lost rotation's proposal IS the live token", s.Refreshes, s.RefreshWalkBacks, s.Terminal.Kind)
	}
	if got := len(r.j.Chain()); got != 0 {
		t.Errorf("the chain is %d links after the answer, want empty", got)
	}
	if cr, _ := r.j.Credential(); cr.RefreshToken != r.e.f.live() {
		t.Errorf("the client holds %q, the server's live token is %q", cr.RefreshToken, r.e.f.live())
	}
	assertNoReplays(t, r.e.f)
}

// --- criterion 8 · the stamp is the send, and it survives a kill -------------

func TestTheStampIsTheSendAndSurvivesAKill(t *testing.T) {
	t.Run("the journal holds the link AND its stamp when the request arrives", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 0})
		var (
			mu        sync.Mutex
			atArrival []ChainLink
			stamp     time.Time
			present   bool
		)
		r.e.f.onRefresh = func(presentation) {
			mu.Lock()
			atArrival = r.j.Chain()
			stamp, present = r.j.LastSent()
			mu.Unlock()
		}
		at := r.clk.now()
		if err := r.c.RefreshIfDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(atArrival) != 1 {
			t.Fatalf("when the request arrived the journal held %v, want its one link", atArrival)
		}
		if !present || !stamp.Equal(at) {
			t.Errorf("when the request arrived the stamp was %v (present=%v), want the wall clock at the write, %v", stamp, present, at)
		}
		// And the answer took both away.
		if got, ok := r.j.LastSent(); ok {
			t.Errorf("an answered refresh left the stamp %v behind", got)
		}
	})

	t.Run("a client killed before any response leaves the stamp, and the next one is held by it", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 0, accessLive: true, script: []string{loseResponse}})
		var once sync.Once
		r.e.f.onRefresh = func(presentation) { once.Do(r.c.Kill) }
		at := r.clk.now()
		if err := r.c.RefreshIfDue(context.Background()); err == nil {
			t.Fatal("the killed client's refresh reported success")
		}
		stamp, ok := r.j.LastSent()
		if !ok || !stamp.Equal(at) {
			t.Fatalf("the killed client left stamp %v (present=%v), want %v", stamp, ok, at)
		}
		// The relaunch: a NEW client over the same journal, which has no response
		// of its own yet.
		r.clk.add(time.Hour)
		next := r.client(t, Faults{}, nil)
		if got := next.refreshHold(); got != RefreshHeldUnreachable {
			t.Errorf("the relaunched client's hold is %v, want the gate closed — it has been given a free attempt", got)
		}
		before := len(r.net.tape())
		if err := next.refreshDueWhenAllowed(context.Background()); !errors.Is(err, errRefreshHeld) {
			t.Errorf("the relaunched client's automatic refresh = %v, want errRefreshHeld", err)
		}
		if got := len(r.net.tape()) - before; got != 0 {
			t.Errorf("the relaunched client sent %d requests before hearing anything from Catenary", got)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("a stamp in the future counts as absent", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
		r.buildLink(t)
		// A response, so the gate has something to be opened by, and then a clock
		// set BACKWARDS: the stamp is now in the future.
		r.clk.add(time.Second)
		if _, err := r.c.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		// FAR ENOUGH BACK that the stamp is in the future, and not so far that the
		// access token stops being due — the suppressors are what this measures,
		// not refreshDue.
		r.clk.add(-5 * time.Second)
		if got := r.c.refreshHold(); got != RefreshNotHeld {
			t.Errorf("a stamp in the future holds the client: %v — a wrong clock must not wedge the gate or stretch the wait", got)
		}
		before := len(r.net.tape())
		_ = r.c.refreshDueWhenAllowed(context.Background())
		if got := len(r.net.tape()) - before; got != 1 {
			t.Errorf("%d requests, want the one attempt an absent stamp allows", got)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("a chain with no stamp, as written before this change, reads as absent", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
		r.buildLink(t)
		// A JOURNAL FROM BEFORE CANT-127: a chain, and no stamp beside it.
		r.j.credMu.Lock()
		r.j.lastSent = time.Time{}
		r.j.credMu.Unlock()
		r.clk.add(time.Minute)

		// ABSENT IS NOT A FREE ATTEMPT. A context with no response yet is closed
		// either way, so the legacy journal's first automatic attempt waits for
		// one answer from Catenary — it is not immediate.
		if got := r.c.refreshHold(); got != RefreshHeldUnreachable {
			t.Errorf("hold %v over a stampless chain, want the gate closed until Catenary answers", got)
		}
		before := len(r.net.tape())
		if err := r.c.refreshDueWhenAllowed(context.Background()); !errors.Is(err, errRefreshHeld) {
			t.Errorf("the automatic refresh = %v, want errRefreshHeld", err)
		}
		// One answer, and then the attempt goes — and writes a stamp.
		if _, err := r.c.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if err := r.c.refreshDueWhenAllowed(context.Background()); err == nil || errors.Is(err, errRefreshHeld) {
			t.Fatalf("after one answer the automatic refresh = %v, want the plain failure of an attempt that was made", err)
		}
		if got := len(r.net.tape()) - before; got != 1 {
			t.Errorf("%d requests, want exactly the one the answer allowed", got)
		}
		if stamp, ok := r.j.LastSent(); !ok || !stamp.Equal(r.clk.now()) {
			t.Errorf("the attempt left stamp %v (present=%v), want the clock at its write %v", stamp, ok, r.clk.now())
		}
		assertNoReplays(t, r.e.f)
	})
}

// --- criterion 9 · the gate is derived, so it closes across contexts ---------

func TestTheGateIsDerivedFromTheStamp(t *testing.T) {
	t.Run("a client over a non-empty chain sends nothing until Catenary answers it", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
		r.buildLink(t)
		r.clk.add(time.Minute)
		second := r.client(t, Faults{}, nil)
		before := len(r.net.tape())
		if err := second.refreshDueWhenAllowed(context.Background()); !errors.Is(err, errRefreshHeld) {
			t.Errorf("a fresh context's automatic refresh = %v, want errRefreshHeld", err)
		}
		if got := second.Status().RefreshesHeldUnreachable; got != 1 {
			t.Errorf("held-unreachable %d, want 1", got)
		}
		// One page, and it may.
		if _, err := second.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if err := second.refreshDueWhenAllowed(context.Background()); err == nil || errors.Is(err, errRefreshHeld) {
			t.Fatalf("after a page the automatic refresh = %v, want an attempt that was made", err)
		}
		if got := len(r.net.tape()) - before; got != 1 {
			t.Errorf("%d requests, want the one the page allowed", got)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("another context's attempt closes an open gate", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
		r.buildLink(t)
		first, second := r.c, r.client(t, Faults{}, nil)

		// Both hear from Catenary, a moment after the send, so both gates are open.
		r.clk.add(30 * time.Second)
		for _, c := range []*Client{first, second} {
			if _, err := c.fetch(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
		}
		if got := first.refreshHold(); got != RefreshNotHeld {
			t.Fatalf("the first client's hold is %v after a page of its own, want none", got)
		}
		// THE SECOND CONTEXT ATTEMPTS, and nothing is shared between the two but
		// the journal. Its send moves the stamp past the first client's last
		// answer.
		r.clk.add(30 * time.Second)
		if err := second.refreshDueWhenAllowed(context.Background()); err == nil {
			t.Fatal("the second client's attempt reported success against a black-holed /refresh")
		}
		if got := first.refreshHold(); got != RefreshHeldUnreachable {
			t.Errorf("the first client's hold is %v after another context's attempt, want the gate closed", got)
		}
		before := len(r.net.tape())
		if err := first.refreshDueWhenAllowed(context.Background()); !errors.Is(err, errRefreshHeld) {
			t.Errorf("the first client's automatic refresh = %v, want errRefreshHeld", err)
		}
		// It stays closed until an answer of its OWN, later than the new stamp.
		r.clk.add(10 * time.Minute)
		if _, err := first.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if got := first.refreshHold(); got != RefreshNotHeld {
			t.Errorf("the first client's hold is %v after an answer of its own, want none", got)
		}
		if got := len(r.net.tape()) - before; got != 0 {
			t.Errorf("%d requests went out while the first client was held", got)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("a client over an empty chain refreshes proactively as it always did", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, dead: []string{"/refresh"}})
		// No chain, no response, nothing heard from Catenary at all.
		if got := r.c.refreshHold(); got != RefreshNotHeld {
			t.Fatalf("a settled credential's hold is %v, want none", got)
		}
		before := len(r.net.tape())
		if err := r.c.refreshDueWhenAllowed(context.Background()); err == nil {
			t.Fatal("the attempt reported success against a black-holed /refresh")
		}
		if got := len(r.net.tape()) - before; got != 1 {
			t.Errorf("%d requests, want the one a settled credential is always allowed", got)
		}
		assertNoReplays(t, r.e.f)
	})
}

// --- criterion 10 · the backoff belongs to the credential -------------------

func TestTheBackoffBelongsToTheCredentialNotTheProcess(t *testing.T) {
	// A SIMULATED HOUR, stepped, with every context hearing from Catenary on
	// every step so that only the backoff can hold anything.
	hour := func(t *testing.T, contexts int) int {
		t.Helper()
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
		clients := []*Client{r.c}
		for range contexts - 1 {
			clients = append(clients, r.client(t, Faults{}, nil))
		}
		r.buildLink(t)
		for range 120 { // 120 × 30 s
			r.clk.add(30 * time.Second)
			for _, c := range clients {
				if _, err := c.fetch(context.Background(), 0); err != nil {
					t.Fatal(err)
				}
				_ = c.refreshDueWhenAllowed(context.Background())
			}
		}
		assertNoReplays(t, r.e.f)
		return len(r.net.tape())
	}
	one, two := hour(t, 1), hour(t, 2)
	if one < 5 {
		t.Fatalf("one context sent %d requests in a simulated hour; the measurement is too small to compare", one)
	}
	if two != one {
		t.Errorf("two contexts over one credential sent %d requests in a simulated hour and one sent %d; the delay is the credential's", two, one)
	}

	t.Run("a fresh context mid-backoff waits for the stamp plus the delay", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
		sent := r.buildLink(t)
		// The push worker, or the relaunch: constructed 2 s into a 5 s wait, with
		// a /sync that answers.
		r.clk.add(2 * time.Second)
		worker := r.client(t, Faults{}, nil)
		if _, err := worker.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		before := len(r.net.tape())
		if err := worker.refreshDueWhenAllowed(context.Background()); !errors.Is(err, errRefreshHeld) {
			t.Errorf("the fresh context's automatic refresh = %v, want errRefreshHeld", err)
		}
		if got := worker.Status().RefreshesHeldBackoff; got != 1 {
			t.Errorf("held-backoff %d, want 1 — the gate was open, so it is the delay that held it", got)
		}
		// One second past the delay, and it may.
		r.clk.set(sent.Add(refreshDelay(1) + time.Second))
		if _, err := worker.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if err := worker.refreshDueWhenAllowed(context.Background()); err == nil || errors.Is(err, errRefreshHeld) {
			t.Fatalf("past the delay the automatic refresh = %v, want an attempt that was made", err)
		}
		if got := len(r.net.tape()) - before; got != 1 {
			t.Errorf("%d requests, want the one the elapsed delay allowed", got)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("a context that waited on the single-flight lock is held by what the holder wrote", func(t *testing.T) {
		r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
		sent := r.buildLink(t)
		r.clk.add(time.Minute) // both suppressors would let this one through
		if _, err := r.c.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if got := r.c.refreshHold(); got != RefreshNotHeld {
			t.Fatalf("hold %v before the lock is taken, want none — the cheap check must pass", got)
		}

		// ANOTHER CONTEXT HOLDS THE LOCK. What its failed attempt leaves behind
		// is exactly a link and a stamp, written before its request left; the
		// request that never arrived is not part of what this client reads.
		release, err := r.j.lockRefresh(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		other := r.client(t, Faults{}, nil)
		held := make(chan error, 1)
		go func() { held <- r.c.refreshDueWhenAllowed(context.Background()) }()
		time.Sleep(20 * time.Millisecond) // long enough to be waiting on the lock
		r.clk.add(time.Second)
		chain := r.j.Chain()
		if _, ok, err := other.propose(chain[len(chain)-1].Proposal, false); err != nil || !ok {
			t.Fatalf("the other context's link: ok=%v, %v", ok, err)
		}
		release()

		if err := <-held; !errors.Is(err, errRefreshHeld) {
			t.Errorf("the client that waited on the lock = %v, want errRefreshHeld", err)
		}
		if got := r.c.Status().RefreshesHeldUnreachable; got != 1 {
			t.Errorf("held-unreachable %d, want the one the under-lock check counted", got)
		}
		if stamp, _ := r.j.LastSent(); !stamp.After(sent) {
			t.Errorf("the stamp is %v, want the other context's later write", stamp)
		}
		assertNoReplays(t, r.e.f)
	})
}

// --- criterion 11 · a held refresh is not a failure -------------------------

// fetch HANDS BACK THE 401 IT ALREADY HAD, and does not re-send /sync. A second
// /sync per catch-up attempt would carry the same expired pair to the same
// answer, which is a request this bound exists to remove.
func TestAHeldRefreshIsNotAFailure(t *testing.T) {
	r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, dead: []string{"/refresh"}})
	r.buildLink(t)
	// One second in: Catenary's 401 on /sync opens the gate, and the 5 s delay
	// holds the refresh.
	r.clk.add(time.Second)
	before := len(r.net.tape())
	syncs := r.e.syncCount()

	_, err := r.c.fetch(context.Background(), 0)
	if !errors.Is(err, errUnauthorized) {
		t.Fatalf("fetch = %v, want the 401 it already had", err)
	}
	if got := r.e.syncCount() - syncs; got != 1 {
		t.Errorf("%d /sync requests for one catch-up attempt, want 1 — the retry must not go out with the same pair", got)
	}
	if got := len(r.net.tape()) - before; got != 0 {
		t.Errorf("%d /refresh requests, want none — the refresh was held", got)
	}
	s := r.c.Status()
	if s.RefreshesHeldBackoff != 1 || s.RefreshesHeldUnreachable != 0 {
		t.Errorf("held-backoff %d, held-unreachable %d; want the backoff to be what held it", s.RefreshesHeldBackoff, s.RefreshesHeldUnreachable)
	}
	if s.RefreshErrors != 1 || s.RefreshesSkipped != 0 {
		t.Errorf("refresh errors %d, skipped %d; want only the explicit attempt's failure and nothing counted as skipped", s.RefreshErrors, s.RefreshesSkipped)
	}
	if s.RefreshHold != RefreshHeldBackoff {
		t.Errorf("Status names the hold %v, want %v", s.RefreshHold, RefreshHeldBackoff)
	}
	assertNoReplays(t, r.e.f)
}

// --- criterion 12 · a person can see it -------------------------------------

func TestAPersonCanSeeTheHold(t *testing.T) {
	// The three states Status must keep apart, spelled for a person.
	for hold, want := range map[RefreshHold]string{
		RefreshNotHeld:         "none",
		RefreshHeldUnreachable: "refresh held — server not reached",
		RefreshHeldBackoff:     "refresh backing off",
	} {
		if got := hold.String(); got != want {
			t.Errorf("hold %d reads %q, want %q", hold, got, want)
		}
	}

	r := newRig(t, rigOpts{span: 365 * 24 * time.Hour, accessLive: true, dead: []string{"/refresh"}})
	if s := r.c.Status(); s.ChainLength != 0 || s.RefreshHold != RefreshNotHeld {
		t.Errorf("a settled credential reports chain %d and hold %v, want 0 and none", s.ChainLength, s.RefreshHold)
	}
	r.buildLink(t)
	s := r.c.Status()
	if s.ChainLength != 1 || s.RefreshHold != RefreshHeldUnreachable {
		t.Errorf("chain %d, hold %v; want 1 and the gate closed", s.ChainLength, s.RefreshHold)
	}
	if s.Terminal.Kind != NotTerminal || s.Connected {
		t.Errorf("a held client reports terminal %v and connected %v; a hold is neither a terminal nor a lost socket", s.Terminal.Kind, s.Connected)
	}
	// ONE LINE ON CLOSE, ONE ON OPEN, however many attempts are held between.
	for range 5 {
		_ = r.c.refreshDueWhenAllowed(context.Background())
	}
	if got := r.log.matching("refresh gate closed"); len(got) != 1 {
		t.Errorf("the gate logged %d closes for five held attempts, want 1: %v", len(got), got)
	}
	r.clk.add(time.Minute)
	if _, err := r.c.fetch(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := r.log.matching("refresh gate open"); len(got) != 1 {
		t.Errorf("the gate logged %d opens, want 1: %v", len(got), got)
	}

	// A LONG CHAIN IS ONE WARN, not one per attempt. Built through the client's
	// own propose, which is what an attempt that settles nothing leaves behind.
	for len(r.j.Chain()) <= chainWarnLength {
		chain := r.j.Chain()
		if _, ok, err := r.c.propose(chain[len(chain)-1].Proposal, false); err != nil || !ok {
			t.Fatalf("link %d: ok=%v, %v", len(chain)+1, ok, err)
		}
	}
	for range 3 {
		_ = r.c.refreshHold()
	}
	warns := r.log.matching("the refresh chain is long")
	if len(warns) != 1 {
		t.Errorf("a chain of %d links logged %d WARNs, want exactly 1: %v", len(r.j.Chain()), len(warns), warns)
	}
	if s := r.c.Status(); s.ChainLength != chainWarnLength+1 {
		t.Errorf("Status reports a chain of %d, want %d", s.ChainLength, chainWarnLength+1)
	}
	assertNoReplays(t, r.e.f)
}
