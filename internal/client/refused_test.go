package client

// CANT-129: a refused access token, measured.
//
// THE CLAIM IS A NUMBER, as CANT-127's was. Over an hour in which Catenary
// refuses the access token and /refresh is black-holed, the requests the server
// refuses fall from ~1,440 to about a dozen — the refresh attempts CANT-127's
// curve allows, plus the one dial that taught the client — while THE ATTEMPTS
// THEMSELVES ARE UNCHANGED, which is the whole difference between a rate limit
// and a stall. So the rigs below simulate an hour rather than labelling one, and
// both halves of that claim have a control watched failing with them:
// Faults.PresentRefusedToken for the rule, and Faults.NeverPresentRefusedToken —
// the withdrawn "never again" — for the one request the rule still allows.
//
// THE ELAPSED FAKE TIME IS WHAT IS ASSERTED, not the dial count. hold_test.go
// takes its hours from dials, at simPerDial each, because that is what its client
// does all day; a ruled client here makes ONE dial, so the dial count says nothing
// about how long it ran. The clock is carried by the transport for the control
// that dials and by the pause seam for the client that does not, and the two runs
// are compared by simulated duration.
//
// WHY THERE IS A SECOND RIG BESIDE hold_test.go's, rather than an option on that
// one. Three things this file needs that it cannot have: a clock that moves while
// the client is SILENT, since a refused client sends nothing for up to fifteen
// minutes; a server that COUNTS WHAT IT REFUSED, by path, which is this file's
// stand-in for the WARN lines the ticket exists to remove; and a network that can
// lose the answer to a request it did deliver, because "at most one refusal per
// hold" and "at most one refusal the client SAW per hold" are different claims and
// criterion 6 asks for both. Everything else is that file's and is reused as it
// stands: the estate and its refresh family, the fake clock, countingRand,
// logLines, and the no-replay floor every test here is also held to.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

const (
	// realQuantum is these rigs' REAL pacing: what one dial backoff, one
	// catch-up retry and one poll of the rule's wait each cost in wall-clock
	// time. Milliseconds rather than hold_test.go's microseconds, for a reason
	// the rule itself creates — the mark this file is about is learned from the
	// /sync BESIDE a dial, so the client must have that answer in hand before
	// its next dial goes out. A real client's five-second backoff guarantees
	// that against a round trip; fifty microseconds would make these tests
	// measure the race instead of the rule. At 5 ms the loopback round trip has
	// a ~25× margin and a simulated hour still costs under two seconds.
	realQuantum = 5 * time.Millisecond
	// fastQuantum paces the runs with no such ordering to protect: the controls,
	// which ignore the mark entirely, and whose simulated hour is carried by 720
	// dials rather than by the wait.
	fastQuantum = 200 * time.Microsecond
)

// --- the instruments ---------------------------------------------------------

// request is one request the client made, as the wire carried it: the path, the
// access token it presented — the bearer on /sync, the subprotocol on the upgrade
// — and, on /refresh, the refresh token with the successor proposed for it. The
// simulated time it left at rides along, because criterion 4 asserts WHEN an
// attempt went out against the curve that allowed it.
type request struct {
	path     string
	token    string
	proposal string
	at       time.Time
}

// hop is the network between the client and Catenary: hold_test.go's drive with
// the three differences this ticket forces. It records EVERY request rather than
// only the refresh attempts, because criterion 8's oracle is about the dial and
// the page as much as the attempt. What it drops is switchable while the client
// runs, because criterion 6 needs a /sync that fails and then heals. And it can
// FREEZE simulated time, so that a measurement window cannot be ended from
// underneath by a hold elapsing inside it.
type hop struct {
	clk *fakeClock

	mu sync.Mutex
	// dead names paths that fail before they arrive — nothing reaches Catenary,
	// so nothing is refused and nothing is logged. dropFirst is the same for the
	// first n requests to a path, which is how /refresh heals after a stated
	// number of failed attempts rather than at a stated time.
	dead      map[string]bool
	dropFirst map[string]int
	sent      []request
	frozen    bool
}

func newHop(clk *fakeClock, dead []string, dropFirst map[string]int) *hop {
	h := &hop{clk: clk, dead: map[string]bool{}, dropFirst: map[string]int{}}
	for _, p := range dead {
		h.dead[p] = true
	}
	for p, n := range dropFirst {
		h.dropFirst[p] = n
	}
	return h
}

func (h *hop) RoundTrip(r *http.Request) (*http.Response, error) {
	rq := request{path: r.URL.Path, at: h.clk.now()}
	switch r.URL.Path {
	case "/refresh":
		rq.token, rq.proposal = refreshBody(r)
	case "/sync":
		rq.token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	case "/ws":
		rq.token = upgradeToken(r.Header.Get("Sec-WebSocket-Protocol"))
	}
	h.mu.Lock()
	h.sent = append(h.sent, rq)
	drop := h.dead[r.URL.Path]
	if n := h.dropFirst[r.URL.Path]; n > 0 {
		h.dropFirst[r.URL.Path], drop = n-1, true
	}
	h.mu.Unlock()
	// THE CONTROL'S HOUR IS STILL THE TRANSPORT'S. A client that dials pays
	// simPerDial for each one, exactly as hold_test.go's drive charges it, which
	// is what carries the unruled run's simulated hour. The ruled client makes one
	// dial and its hour is carried by the pause seam instead.
	if r.URL.Path == "/ws" {
		h.charge(simPerDial)
	}
	if drop {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("the network is down")}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// charge advances the fake clock unless simulated time is frozen. FREEZING IS AN
// INSTRUMENT, NOT A FAULT: with the clock still, no hold can elapse, so a window
// in which a test pushes triggers at a client and then asserts that no page
// request went out cannot be spoiled by the hold ending inside it.
func (h *hop) charge(d time.Duration) {
	h.mu.Lock()
	frozen := h.frozen
	h.mu.Unlock()
	if !frozen {
		h.clk.add(d)
	}
}

func (h *hop) freeze(still bool) {
	h.mu.Lock()
	h.frozen = still
	h.mu.Unlock()
}

func (h *hop) tape() []request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]request(nil), h.sent...)
}

// on is every request to one path, in order — the per-path tape criterion 8's
// oracle compares.
func (h *hop) on(path string) []request {
	var out []request
	for _, rq := range h.tape() {
		if rq.path == path {
			out = append(out, rq)
		}
	}
	return out
}

func (h *hop) count(path string) int { return len(h.on(path)) }

func (h *hop) setDead(path string, dead bool) {
	h.mu.Lock()
	h.dead[path] = dead
	h.mu.Unlock()
}

// refreshBody reads a /refresh with the generated decoder and puts it back, so
// the request is untouched and the tape is what the wire carried.
func refreshBody(r *http.Request) (token, proposal string) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", ""
	}
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	var req wire.RefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", ""
	}
	if req.ProposedRefreshToken != nil {
		proposal = string(*req.ProposedRefreshToken)
	}
	return string(req.RefreshToken), proposal
}

func upgradeToken(header string) string {
	for _, sp := range strings.Split(header, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sp), tokenSubprotocolPrefix); ok {
			return v
		}
	}
	return ""
}

// door is the estate behind a counter: every request CATENARY REFUSED, by path.
// That count is this file's stand-in for the server's WARN lines, and it is taken
// here rather than at the transport because of the one case where the two
// disagree — a request that was delivered and refused and whose answer was lost
// on the way back. Catenary logged that one; the client never saw it.
//
// It also owns the refusals the estate would not make on its own: criterion 3
// needs a socket that is UP while /sync is refused, which is the real shape of
// record §5 (a session is authorized once at accept and outlives its access
// token) and not something the estate's single token check can produce.
type door struct {
	e   *estate
	srv *httptest.Server

	mu      sync.Mutex
	refused map[string]int
	// refuseSync and refuseWS force a refusal the token check would not make.
	// answerSync replaces the answer with something that is not Catenary
	// answering at all: a hop's 401, a captive portal's 200, a 502.
	refuseSync func(*http.Request) bool
	refuseWS   func(*http.Request) bool
	answerSync func(http.ResponseWriter)
	// loseAnswers is how many more refusals to DELIVER, count, and then lose the
	// answer to.
	loseAnswers int
}

func newDoor(t *testing.T, e *estate) *door {
	t.Helper()
	d := &door{e: e, refused: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/refresh", e.f.serve)
	mux.HandleFunc("/sync", d.sync)
	mux.HandleFunc("/ws", d.ws)
	// The estate's own listener goes unused: the client talks to this one, which
	// wraps the same handlers. Its /refresh is the family's, verbatim.
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func (d *door) sync(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	answer, force, lose := d.answerSync, d.refuseSync, d.loseAnswers
	d.mu.Unlock()
	if answer != nil {
		// NOT CATENARY AT ALL, so nothing here is a refusal and nothing is counted.
		answer(w)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	live := d.e.liveAccess()
	if (force != nil && force(r)) || live == "" || tok != live {
		d.mu.Lock()
		d.refused["/sync"]++
		if lose > 0 {
			d.loseAnswers--
		}
		d.mu.Unlock()
		if lose > 0 {
			// DELIVERED, REFUSED, LOGGED — AND THE ANSWER LOST. The client sees a
			// dead connection and learns nothing.
			drop(w)
			return
		}
		unauthorized(w)
		return
	}
	d.e.sync(w, r)
}

func (d *door) ws(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	force := d.refuseWS
	d.mu.Unlock()
	tok := upgradeToken(r.Header.Get("Sec-WebSocket-Protocol"))
	live := d.e.liveAccess()
	if (force != nil && force(r)) || live == "" || tok != live {
		d.mu.Lock()
		d.refused["/ws"]++
		d.mu.Unlock()
		unauthorized(w)
		return
	}
	d.e.ws(w, r)
}

func (d *door) count(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refused[path]
}

// refusals is every authenticated request the server refused, whatever carried
// the token: the measurement the Done when asks for.
func (d *door) refusals() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int
	for _, c := range d.refused {
		n += c
	}
	return n
}

func (d *door) set(f func(*door)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(d)
}

// --- the rig -----------------------------------------------------------------

type refusedOpts struct {
	// accessLive keeps the enrollment access token good at the door; the default
	// is a device whose token Catenary refuses, which is this ticket's case.
	accessLive bool
	dead       []string
	dropFirst  map[string]int
	faults     Faults
	rand       io.Reader
	script     []string
	// quantum is the real pacing; zero is realQuantum.
	quantum time.Duration
	// cred bends the enrolled pair — an expiry the clock reads as live, an offset
	// an hour out (criterion 7).
	cred func(Credential) Credential
}

type refusedRig struct {
	t       *testing.T
	e       *estate
	d       *door
	clk     *fakeClock
	net     *hop
	log     *logLines
	j       *Journal
	c       *Client
	start   time.Time
	quantum time.Duration
}

func newRefusedRig(t *testing.T, o refusedOpts) *refusedRig {
	t.Helper()
	clk := newClock(time.Now())
	first := ""
	if o.accessLive {
		first = padToken("access-0")
	}
	e := newEstate(t, first, o.script...)
	r := &refusedRig{
		t: t, e: e, d: newDoor(t, e), clk: clk, log: &logLines{},
		net: newHop(clk, o.dead, o.dropFirst), start: clk.now(), quantum: o.quantum,
	}
	if r.quantum == 0 {
		r.quantum = realQuantum
	}
	cred := Credential{
		DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken("access-0"),
		AccessExpiresAt: clk.now().Add(10 * time.Second),
		RefreshToken:    firstRefresh, RefreshExpiresAt: clk.now().Add(60 * 24 * time.Hour),
	}
	if o.cred != nil {
		cred = o.cred(cred)
	}
	r.j = NewJournal()
	if err := r.j.Enroll(cred); err != nil {
		t.Fatal(err)
	}
	r.c = r.client(o.faults, o.rand)
	return r
}

func (r *refusedRig) client(faults Faults, rnd io.Reader) *Client {
	r.t.Helper()
	c, err := New(Config{
		BaseURL: r.d.srv.URL, Journal: r.j, Refresh: true, Now: r.clk.now, Logger: r.log.logger(),
		HTTPClient: &http.Client{Transport: r.net}, Rand: rnd, Faults: faults,
		BackoffMin: r.quantum, BackoffMax: r.quantum, Pause: r.pause,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// second is another context over the same credential: a second tab, or an app
// beside its push worker. ITS OWN TRANSPORT, because criterion 5's point is that
// a rotation reaching the JOURNAL ends the first client's wait — the two share
// storage and nothing else, and this one's /refresh is not the black-holed one.
func (r *refusedRig) second() *Client {
	r.t.Helper()
	c, err := New(Config{
		BaseURL: r.d.srv.URL, Journal: r.j, Refresh: true, Now: r.clk.now, Logger: r.log.logger(),
		HTTPClient: &http.Client{Transport: newHop(r.clk, nil, nil)},
		BackoffMin: r.quantum, BackoffMax: r.quantum, Pause: r.pause,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// pause is the rig's Config.Pause, and it strikes hold_test.go's bargain for the
// rule's wait: SLEEP THE REAL, COMPRESSED BACKOFF AND CHARGE THE SIMULATED ONE.
// It cannot charge the duration it was asked for — that is c.backoffMax, which
// these rigs compress to milliseconds so that simulated hours cost seconds, so a
// fifteen-minute hold would never end. simPerDial is the same accounting the
// transport uses for a dial: one poll of the wait stands in for one dial not
// made, and costs what that dial would have cost.
func (r *refusedRig) pause(ctx context.Context, _ time.Duration) bool {
	time.Sleep(r.quantum)
	r.net.charge(simPerDial)
	return ctx.Err() == nil
}

func (r *refusedRig) run() (stop func()) {
	done := make(chan struct{})
	go func() { defer close(done); _ = r.c.Run(context.Background()) }()
	return sync.OnceFunc(func() { r.c.Kill(); <-done })
}

func (r *refusedRig) elapsed() time.Duration { return r.clk.now().Sub(r.start) }

// awaitSim waits for d of SIMULATED time, which is how a test here waits for an
// hour: the transport carries it for a client that dials and the pause seam for
// one that does not, so it is the one measure both runs share.
func (r *refusedRig) awaitSim(d time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for r.elapsed() < d {
		if time.Now().After(deadline) {
			r.t.Fatalf("only %s of simulated time passed in the real time allowed; status %+v",
				r.elapsed(), r.c.Status())
		}
		time.Sleep(time.Millisecond)
	}
}

// await polls for a state, for the same reason hold_test.go's awaitFor does: some
// of what is watched here — a chain that grew, a request the server counted — is
// not a change the client notifies anybody about.
func (r *refusedRig) await(what string, pred func() bool) {
	r.t.Helper()
	awaitFor(r.t, what, pred)
}

func (r *refusedRig) chainLen() int { return len(r.j.Chain()) }

// attemptsWithin is how many automatic refresh attempts CANT-127's curve allows
// in d: the first, which an empty chain is always allowed, and then one each time
// `min(15 min, 5 s × 2^(n−1))` has elapsed from the one before. Arithmetic over
// refreshDelay rather than a table of literals, so it cannot disagree with the
// curve it quotes. For an hour it is 11 — at 0, 5, 15, 35, 75, 155, 315, 635,
// 1275, 2175 and 3075 seconds.
func attemptsWithin(d time.Duration) int {
	n, at := 0, time.Duration(0)
	for at <= d {
		n++
		at += refreshDelay(n)
	}
	return n
}

// subsequenceOf is hold_test.go's subsequence oracle over this file's richer
// tape: want's elements appear in all, in order, with nothing added or reordered —
// all with some elements REMOVED, which is the only thing the rule is allowed to
// do. Applied PER PATH, because the dial and the page come from two goroutines
// whose interleaving is not a property of the rule.
func subsequenceOf(sub, all []request) bool {
	i := 0
	for _, rq := range all {
		if i < len(sub) && sub[i].path == rq.path && sub[i].token == rq.token && sub[i].proposal == rq.proposal {
			i++
		}
	}
	return i == len(sub)
}

func carried(reqs []request) []string {
	out := make([]string, 0, len(reqs))
	for _, rq := range reqs {
		out = append(out, unpad(rq.token)+"→"+unpad(rq.proposal))
	}
	return out
}

// --- criterion 1 · the ticket's own test --------------------------------------

// AN HOUR OF A REFUSED TOKEN COSTS THE REFRESH ATTEMPTS AND THE ONE DIAL THAT
// TAUGHT THE CLIENT. /sync and the upgrade both answer Catenary's own 401,
// /refresh is black-holed, and Run drives it for one simulated hour: the server
// refuses about a dozen requests instead of ~1,440, and the ATTEMPTS ARE PINNED IN
// BOTH DIRECTIONS against CANT-127's curve — no more than it allows, and no fewer,
// so a client that stalls after its second attempt fails here rather than looking
// like a very quiet success.
func TestARefusedTokenIsPresentedOncePerRefreshHold(t *testing.T) {
	r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
	stop := r.run()
	defer stop()
	r.awaitSim(time.Hour)
	stop()

	elapsed, attempts := r.elapsed(), r.net.count("/refresh")
	allowed := attemptsWithin(elapsed)
	refused, s := r.d.refusals(), r.c.Status()
	// THE MEASUREMENT THE `Done when` ASKS FOR, in the CI output rather than only
	// in a green tick: a number nobody can read is a number nobody checks.
	t.Logf("over %s of simulated time the server refused %d requests carrying the token "+
		"(%d upgrades, %d pages) for %d refresh attempts; %d dials and %d pages were withheld",
		elapsed, refused, r.d.count("/ws"), r.d.count("/sync"), attempts, s.DialsWithheld, s.SyncsWithheld)

	// (a) THE BOUND, COMPUTED FROM THE RUN AND NOT A LITERAL.
	if refused > attempts+1 {
		t.Errorf("the server refused %d requests carrying the token over %s (%d upgrades, %d pages), "+
			"want at most the %d refresh attempts plus the one dial that taught the client",
			refused, elapsed, r.d.count("/ws"), r.d.count("/sync"), attempts)
	}
	if got := r.d.count("/ws"); got != 1 {
		t.Errorf("the server refused %d upgrades, want the 1 that taught the client; a second means "+
			"the rig let a dial outrun the /sync it is meant to learn from", got)
	}
	if got := r.d.count("/sync"); got != attempts-1 {
		t.Errorf("the server refused %d pages for %d attempts, want one per attempt after the first — "+
			"the first is Run's own pre-dial attempt, and every later one is driven by a hold's one /sync",
			got, attempts)
	}
	if s.Dials != 1 {
		t.Errorf("the client dialed %d times over %s, want the 1 it made before it had been refused", s.Dials, elapsed)
	}

	// THE ATTEMPTS, PINNED BOTH WAYS. One attempt of slack downward, and only
	// that: the wait polls at the dial cadence, so an attempt can land up to one
	// poll after the curve allowed it. A stall fails this by miles — criterion 2's
	// second control is watched doing exactly that.
	if attempts > allowed {
		t.Errorf("%d refresh attempts in %s, more than the %d CANT-127's curve allows", attempts, elapsed, allowed)
	}
	if attempts < allowed-1 {
		t.Errorf("only %d refresh attempts in %s, want the %d the curve allows — the rule must rate-limit "+
			"the requests that cannot succeed, not stop the ones that can", attempts, elapsed, allowed)
	}
	if attempts != r.chainLen() {
		t.Errorf("%d attempts left a chain of %d; an attempt that settles nothing is exactly one link",
			attempts, r.chainLen())
	}

	// AND A PERSON CAN SEE WHY IT WENT QUIET.
	if !s.TokenRefused || s.DialsWithheld == 0 || s.SyncsWithheld == 0 {
		t.Errorf("status reports refused=%v, dials withheld %d, syncs withheld %d; want the mark and both counters",
			s.TokenRefused, s.DialsWithheld, s.SyncsWithheld)
	}
	if marks := r.log.matching("access token refused by Catenary"); len(marks) != 1 {
		t.Errorf("%d lines saying the token was refused, want exactly 1 over %d withheld dials: %v",
			len(marks), s.DialsWithheld, marks)
	}
	if about := r.log.matching("refused"); len(about) > 2 {
		t.Errorf("%d log lines about a refused token over %s, want O(1):\n%s",
			len(about), elapsed, strings.Join(about, "\n"))
	}
	assertNoReplays(t, r.e.f)
	if _, _, _, revoked := r.e.f.record(); revoked {
		t.Error("the family was revoked")
	}
}

// --- criterion 2 · the controls, watched failing -----------------------------

// THE SAME HOUR WITH THE RULE FAULTED OFF refuses two requests per dial, which is
// what every client did before this ticket and what criterion 1 cannot be allowed
// to pass against.
func TestWithoutTheRuleARefusedTokenIsPresentedOnEveryDial(t *testing.T) {
	r := newRefusedRig(t, refusedOpts{
		dead: []string{"/refresh"}, quantum: fastQuantum,
		faults: Faults{PresentRefusedToken: true},
	})
	stop := r.run()
	defer stop()
	r.awaitSim(time.Hour)
	stop()

	elapsed, dials, refused := r.elapsed(), r.c.Status().Dials, r.d.refusals()
	t.Logf("the control: over %s of simulated time the server refused %d requests carrying the token "+
		"(%d upgrades, %d pages) over %d dials", elapsed, refused, r.d.count("/ws"), r.d.count("/sync"), dials)
	if want := int(time.Hour / simPerDial); dials < want-2 {
		t.Errorf("the control dialed %d times in %s, want about the %d an hour at %s a dial",
			dials, elapsed, want, simPerDial)
	}
	// OF THE ORDER OF TWICE THE DIAL COUNT: the upgrade, and the /sync beside it.
	// Asserted as an order rather than a ratio — how many catch-up retries fit
	// between two dials is the real backoff's business and not the rule's.
	if refused <= 1000 {
		t.Errorf("the control's server refused %d requests in %s over %d dials, want the >1,000 "+
			"this ticket exists to remove", refused, elapsed, dials)
	}
	if refused < dials {
		t.Errorf("the control's server refused %d requests over %d dials, want at least one per dial", refused, dials)
	}
	if got := r.c.Status().DialsWithheld; got != 0 {
		t.Errorf("the control withheld %d dials; with the rule faulted off it must withhold none", got)
	}
	assertNoReplays(t, r.e.f)
}

// AND WITH THE ONE /sync PER HOLD FAULTED OFF — the withdrawn "never again" — the
// client is watched STALLING. Its attempts stop at two: the pre-dial one, and the
// one the teaching 401 drove. Nothing then reopens CANT-127's gate, so a /refresh
// that heals is never noticed, and criterion 1(a)'s lower bound is what catches it.
func TestWithoutTheOneSyncPerHoldTheClientStalls(t *testing.T) {
	r := newRefusedRig(t, refusedOpts{
		dead:   []string{"/refresh"},
		faults: Faults{NeverPresentRefusedToken: true},
	})
	stop := r.run()
	defer stop()
	r.awaitSim(time.Hour)
	stop()

	elapsed, attempts := r.elapsed(), r.net.count("/refresh")
	if attempts != 2 {
		t.Errorf("the stalled client made %d attempts in %s, want the 2 it can: its own pre-dial one, "+
			"and the one the teaching 401 drove", attempts, elapsed)
	}
	// THE LOWER BOUND CRITERION 1(a) ASSERTS IS WHAT FAILS HERE, and that is the
	// point of running this at all: an upper bound alone passes against a stall.
	if allowed := attemptsWithin(elapsed); attempts >= allowed-1 {
		t.Errorf("the stalled client made %d attempts against the %d the curve allows in %s; "+
			"criterion 1(a)'s lower bound would not have caught it", attempts, allowed, elapsed)
	}
	if got := r.c.Status().SyncsWithheld; got == 0 {
		t.Error("nothing was withheld; the control did not exercise the rule at all")
	}
	assertNoReplays(t, r.e.f)
}

// --- criterion 3 · the socket that stays up ----------------------------------

// A HELD REFRESH WITH THE SOCKET UP IS ENDED BY catchUpLoop, AND THE SOCKET IS
// NEVER CLOSED. The session was authorized once at accept and outlives its access
// token (CANT-28 ruling 2), so /sync is refused while the socket carries frames
// and sends. Run is parked inside session and sends nothing; the catch-up waits
// out the hold and issues one page request, whose 401 drives the reactive refresh.
// Triggers arriving during the hold — `resync_required`, a frame, CatchUp — cost
// nothing.
func TestASocketThatStaysUpHasItsHoldEndedByTheCatchUp(t *testing.T) {
	r := newRefusedRig(t, refusedOpts{
		accessLive: true,                          // the upgrade accepts, and keeps accepting
		dropFirst:  map[string]int{"/refresh": 2}, // healed only after the hold has been renewed once
	})
	// /sync IS REFUSED WHILE THE PAIR IS THE ENROLLED ONE, which no single token
	// check can produce: the door refuses it until the family has rotated.
	r.d.set(func(d *door) {
		d.refuseSync = func(*http.Request) bool { return mintedSoFar(r.e.f) == 0 }
	})
	stop := r.run()
	defer stop()

	r.await("the socket to come up and the catch-up to be refused", func() bool {
		s := r.c.Status()
		return s.Ready && s.TokenRefused && s.ChainLength >= 1
	})

	// THE HOLD, HELD STILL. With simulated time frozen no hold can elapse, so the
	// only thing that could send a page request in this window is a trigger — and
	// none may.
	r.net.freeze(true)
	syncs, dials, readys := r.net.count("/sync"), r.c.Status().Dials, r.c.Status().Readys
	r.e.frames <- []byte(`{"type":"resync_required","reason":"membership_changed","log_seq":0}`)
	r.e.frames <- []byte(`{"type":"pong","id":"from-the-server"}`)
	r.c.CatchUp()
	r.await("the frames to arrive", func() bool {
		s := r.c.Status()
		return s.Resyncs >= 1 && s.PongsReceived >= 1
	})
	time.Sleep(10 * realQuantum) // several polls of the wait, at the dial cadence
	if got := r.net.count("/sync"); got != syncs {
		t.Errorf("%d page requests went out while the refresh was held, want none: a trigger during a "+
			"hold is a no-op, whichever of the four it is", got-syncs)
	}
	// AND THE SOCKET IS STILL WRITABLE. ErrNotConnected or ErrSessionEnded here
	// would mean this rule had closed a session it must not; the deadline is this
	// fake never acking.
	sctx, scancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := r.c.Send(sctx, wire.ClientSend{ClientID: wire.Uuid(uuid.NewString()), ConversationID: wire.Uuid(uuid.NewString())})
	scancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a send during the hold = %v, want the socket to have carried it and only the ack to be missing", err)
	}
	r.net.freeze(false)

	// THE HOLD ENDS, THE PROBE GOES OUT, AND THE THIRD ATTEMPT IS ANSWERED.
	r.await("the refresh to be answered and the catch-up to complete", func() bool {
		s := r.c.Status()
		return s.Refreshes == 1 && s.CaughtUp && !s.TokenRefused
	})
	s := r.c.Status()
	if s.Dials != dials || s.Readys != readys {
		t.Errorf("dials %d→%d and readys %d→%d; the socket must not have dropped or been redialed",
			dials, s.Dials, readys, s.Readys)
	}
	if !s.Connected || !s.Ready {
		t.Errorf("connected %v ready %v after the recovery; the established socket is never closed by this rule",
			s.Connected, s.Ready)
	}
	// Three attempts — two that settled nothing and one answered — and the answered
	// one's walk back down the chain it left, which is exchange's business and not
	// this rule's.
	if got, walks := r.net.count("/refresh"), s.RefreshWalkBacks; got != 3+walks || walks != 2 {
		t.Errorf("%d /refresh requests with %d walk-backs, want 3 attempts and the 2 steps the last one owes",
			got, walks)
	}
	assertNoReplays(t, r.e.f)
}

// mintedSoFar is how many rotations the family has answered. Read under its own
// lock, as liveAccess reads it.
func mintedSoFar(f *family) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minted
}

// --- criterion 4 · it recovers from a closed gate, with no input -------------

// /refresh HEALS AFTER THREE FAILED ATTEMPTS, AT A MOMENT WHEN THE GATE IS
// CLOSED, and the client recovers with no input and no later than the curve
// allowed. The gate being closed is asserted where it is decidable — as the third
// attempt leaves, with its own stamp already written and no Catenary answer later
// than it — and the attempt times are asserted against the fake clock on the
// SECOND AND LATER holds, not only the first, because the first hold's gate is
// open either way.
func TestItRecoversFromAClosedGateWithNoInput(t *testing.T) {
	r := newRefusedRig(t, refusedOpts{dropFirst: map[string]int{"/refresh": 3}})

	var (
		mu     sync.Mutex
		atHeal *struct{ answered, stamp time.Time }
	)
	r.e.f.onRefresh = func(presentation) {}
	// The hook is on the DOOR's family rather than the transport, so it runs for
	// the request that arrives; the three that are dropped never do. Instead the
	// gate is read as the third attempt LEAVES, from the client itself.
	go func() {
		awaitFor(t, "the third attempt to leave", func() bool { return r.net.count("/refresh") >= 3 })
		r.c.mu.Lock()
		answered := r.c.answeredAt
		r.c.mu.Unlock()
		stamp, _ := r.j.LastSent()
		mu.Lock()
		atHeal = &struct{ answered, stamp time.Time }{answered, stamp}
		mu.Unlock()
	}()

	stop := r.run()
	defer stop()
	r.await("the client to rotate, dial with the new pair and catch up", func() bool {
		s := r.c.Status()
		return s.Refreshes == 1 && s.Ready && s.CaughtUp
	})
	stop()

	mu.Lock()
	heal := atHeal
	mu.Unlock()
	if heal == nil {
		t.Fatal("the third attempt never left")
	}
	// LATER THAN IS STRICT, so an answer at or before the stamp leaves the gate
	// closed: at the moment /refresh healed, nothing could have reopened it but a
	// request the client had yet to make.
	if heal.answered.After(heal.stamp) {
		t.Errorf("when /refresh healed the last Catenary answer was %v and the stamp %v; the gate was open, "+
			"so this measures a client that was already allowed to attempt", heal.answered, heal.stamp)
	}

	// THE ATTEMPT TIMES, AGAINST THE CURVE, ON EVERY HOLD.
	//
	// THE FIRST FOUR REQUESTS ARE THE FOUR ATTEMPTS, and the rest are one attempt's
	// walk. Three attempts were dropped, one request each; the fourth presents the
	// chain's newest token and then steps back one link per 401 until it reaches the
	// one the server confirmed — which is exchange's, not this rule's, so only the
	// request that STARTED that attempt is timed here.
	sent := r.net.on("/refresh")
	if len(sent) < 4 {
		t.Fatalf("%d /refresh requests, want at least the 4 the arrangement asks for", len(sent))
	}
	if got := r.c.Status().RefreshWalkBacks; got != 3 {
		t.Errorf("%d walk-backs for a chain of 4; the recovery is one attempt that steps back per link", got)
	}
	attempts := sent[:4]
	// One poll of slack per hold, for the same reason criterion 1 allows one
	// attempt: the wait notices at the dial cadence.
	slack := 8 * simPerDial
	for k := 2; k <= len(attempts); k++ {
		gap, want := attempts[k-1].at.Sub(attempts[k-2].at), refreshDelay(k-1)
		if gap < want {
			t.Errorf("attempt %d went out %s after attempt %d, sooner than the %s a chain of %d owes",
				k, gap, k-1, want, k-1)
		}
		if gap > want+slack {
			t.Errorf("attempt %d went out %s after attempt %d, later than the %s the curve allowed (+%s of slack) — "+
				"the rule must not delay a refresh beyond what CANT-127 already did", k, gap, k-1, want, slack)
		}
	}
	s := r.c.Status()
	if s.TokenRefused || s.RefreshHold != RefreshNotHeld {
		t.Errorf("after the recovery status reports refused=%v hold=%v, want neither", s.TokenRefused, s.RefreshHold)
	}
	if cr, _ := r.j.Credential(); cr.RefreshToken != r.e.f.live() {
		t.Errorf("the client holds %q, the server's live token is %q", cr.RefreshToken, r.e.f.live())
	}
	assertNoReplays(t, r.e.f)
}

// --- criterion 5 · what else ends the wait ----------------------------------

func TestAnotherContextsRotationEndsTheWait(t *testing.T) {
	t.Run("with no socket, another context's rotation resumes the dial", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
		stop := r.run()
		defer stop()
		r.await("the token to be refused and a hold to be in force", func() bool {
			s := r.c.Status()
			return s.TokenRefused && s.ChainLength >= 2
		})

		// SIMULATED TIME STANDS STILL FROM HERE, so nothing this client does next
		// can be the hold ending: what resumes it is the pair changing.
		r.net.freeze(true)
		at, attempts, syncs := r.clk.now(), r.net.count("/refresh"), r.d.count("/sync")
		if err := r.second().RefreshIfDue(context.Background()); err != nil {
			t.Fatalf("the second context's refresh: %v", err)
		}
		r.await("the first client to dial with the new pair and catch up", func() bool {
			s := r.c.Status()
			return s.Ready && s.CaughtUp && !s.TokenRefused
		})
		if !r.clk.now().Equal(at) {
			t.Errorf("simulated time moved from %v to %v; the resume must have come from the pair changing", at, r.clk.now())
		}
		if got := r.net.count("/refresh"); got != attempts {
			t.Errorf("the first client sent %d more /refresh requests of its own; the rotation was somebody else's",
				got-attempts)
		}
		if got := r.d.count("/sync"); got != syncs {
			t.Errorf("the server refused %d more pages after the rotation; the new token is not refused", got-syncs)
		}
		if got := r.c.Status().Refreshes; got != 0 {
			t.Errorf("the first client performed %d refreshes, want 0 — it used what the other context wrote", got)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("with the socket up, the parked catch-up completes within one poll", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{accessLive: true, dead: []string{"/refresh"}})
		r.d.set(func(d *door) {
			d.refuseSync = func(*http.Request) bool { return mintedSoFar(r.e.f) == 0 }
		})
		stop := r.run()
		defer stop()
		r.await("the socket up, the catch-up refused and parked", func() bool {
			s := r.c.Status()
			return s.Ready && s.TokenRefused && s.ChainLength >= 1 && !s.CaughtUp
		})
		r.net.freeze(true)
		at, dials, readys := r.clk.now(), r.c.Status().Dials, r.c.Status().Readys

		if err := r.second().RefreshIfDue(context.Background()); err != nil {
			t.Fatalf("the second context's refresh: %v", err)
		}
		r.await("the parked catch-up to complete", func() bool {
			s := r.c.Status()
			return s.CaughtUp && !s.TokenRefused
		})
		if !r.clk.now().Equal(at) {
			t.Errorf("simulated time moved from %v to %v; the catch-up must have resumed on the pair, not the clock",
				at, r.clk.now())
		}
		s := r.c.Status()
		if s.Dials != dials || s.Readys != readys || !s.Connected {
			t.Errorf("dials %d→%d readys %d→%d connected %v; the socket must have stayed up throughout",
				dials, s.Dials, readys, s.Readys, s.Connected)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("an answered explicit RefreshIfDue on this client ends it too", func(t *testing.T) {
		// Black-holed for the first page only: otherwise the reactive refresh inside
		// that one fetch rotates the pair before there is a wait to end.
		r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the first page = %v, want Catenary's own 401", err)
		}
		r.net.setDead("/refresh", false)
		if !r.c.Status().TokenRefused || r.chainLen() != 1 {
			t.Fatalf("after the 401: refused=%v chain=%d, want the mark and the reactive attempt's link",
				r.c.Status().TokenRefused, r.chainLen())
		}
		if !r.c.refusedHold() {
			t.Fatal("the wait is over already; the reactive attempt's own stamp should hold the next page")
		}
		// AN EXPLICIT REFRESH IS NEVER HELD (CANT-127), and this one is answered.
		if err := r.c.RefreshIfDue(context.Background()); err != nil {
			t.Fatalf("the explicit refresh: %v", err)
		}
		if r.c.refusedHold() || r.c.refusedDial() {
			t.Error("the wait outlived the pair it was about")
		}
		if got := r.log.matching("the pair changed"); len(got) != 1 {
			t.Errorf("%d lines saying the pair changed, want exactly 1: %v", len(got), got)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("an unanswered explicit RefreshIfDue extends it, and costs no page", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the first page = %v, want Catenary's own 401", err)
		}
		links := r.chainLen()
		stamp, _ := r.j.LastSent()
		r.clk.set(stamp.Add(refreshDelay(links) + time.Second))
		if r.c.refusedHold() {
			t.Fatal("the hold outlasted its own deadline")
		}
		syncs := r.d.count("/sync")

		// AN ATTEMPT THAT SETTLES NOTHING MOVES THE STAMP, so the wait does not end
		// — it is extended to the new deadline, and no page goes out in between.
		if err := r.c.RefreshIfDue(context.Background()); err == nil {
			t.Fatal("the black-holed refresh reported success")
		}
		if !r.c.refusedHold() {
			t.Error("the wait ended although the explicit attempt moved the stamp forward")
		}
		moved, _ := r.j.LastSent()
		if want := moved.Add(refreshDelay(r.chainLen())); !r.c.Status().NextRefreshAt.Equal(want) {
			t.Errorf("the next page is due at %v, want the new stamp plus the new delay, %v",
				r.c.Status().NextRefreshAt, want)
		}
		if got := r.d.count("/sync"); got != syncs {
			t.Errorf("%d more pages were refused while the wait was extended, want none", got-syncs)
		}
		assertNoReplays(t, r.e.f)
	})
}

// --- criterion 6 · what marks a token, and what spends the hold's request ----

// ONLY CATENARY'S OWN 401 ON /sync MARKS A TOKEN REFUSED. Everything else that
// can happen to a request leaves the client dialing, because none of them is
// evidence from Catenary about this token — and neither is the device's own clock.
func TestOnlyCatenarysOwn401OnSyncMarksATokenRefused(t *testing.T) {
	always := func(*http.Request) bool { return true }
	for _, tc := range []struct {
		name string
		// arrange leaves the upgrade refused in every case, so that "it keeps
		// dialing" is observable at all.
		arrange func(r *refusedRig)
		opts    refusedOpts
	}{
		{
			name: "a 401 on the upgrade alone, with the page answering honestly",
			opts: refusedOpts{accessLive: true, dead: []string{"/refresh"}},
			arrange: func(r *refusedRig) {
				r.d.set(func(d *door) { d.refuseWS = always })
			},
		},
		{
			name: "a 401 from a hop in front, on /sync",
			opts: refusedOpts{accessLive: true, dead: []string{"/refresh"}},
			arrange: func(r *refusedRig) {
				r.d.set(func(d *door) {
					d.refuseWS = always
					d.answerSync = func(w http.ResponseWriter) {
						http.Error(w, "access: session expired", http.StatusUnauthorized)
					}
				})
			},
		},
		{
			name: "a 502 from a hop in front",
			opts: refusedOpts{accessLive: true, dead: []string{"/refresh"}},
			arrange: func(r *refusedRig) {
				r.d.set(func(d *door) {
					d.refuseWS = always
					d.answerSync = func(w http.ResponseWriter) { http.Error(w, "bad gateway", http.StatusBadGateway) }
				})
			},
		},
		{
			name: "a network error on /sync",
			opts: refusedOpts{accessLive: true, dead: []string{"/refresh", "/sync"}},
			arrange: func(r *refusedRig) {
				r.d.set(func(d *door) { d.refuseWS = always })
			},
		},
		{
			name: "the corrected clock passing access_expires_at",
			opts: refusedOpts{
				accessLive: true, dead: []string{"/refresh"},
				cred: func(c Credential) Credential {
					c.AccessExpiresAt = c.AccessExpiresAt.Add(-time.Hour) // long gone, by the device's own clock
					return c
				},
			},
			arrange: func(r *refusedRig) {
				r.d.set(func(d *door) { d.refuseWS = always })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedRig(t, tc.opts)
			tc.arrange(r)
			stop := r.run()
			defer stop()
			r.await("the client to keep dialing", func() bool { return r.c.Status().Dials >= 3 })
			stop()

			s := r.c.Status()
			if s.TokenRefused {
				t.Error("the token was marked refused; only Catenary's own 401 on /sync may do that")
			}
			if s.DialsWithheld != 0 || s.SyncsWithheld != 0 {
				t.Errorf("%d dials and %d pages were withheld with no mark at all", s.DialsWithheld, s.SyncsWithheld)
			}
			assertNoReplays(t, r.e.f)
		})
	}

	t.Run("the mark is keyed on the access token, so a new pair is presented at once", func(t *testing.T) {
		// /refresh IS BLACK-HOLED FOR THE FIRST PAGE ONLY. Otherwise the reactive
		// refresh inside that one fetch rotates the pair before the mark can be
		// looked at, and the test would be about a client that was never held.
		r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the first page = %v, want Catenary's own 401", err)
		}
		before, _ := r.j.Credential()
		if !r.c.refusedDial() {
			t.Fatal("the token Catenary refused is not marked")
		}
		r.net.setDead("/refresh", false)
		if err := r.c.RefreshIfDue(context.Background()); err != nil {
			t.Fatalf("the refresh: %v", err)
		}
		after, _ := r.j.Credential()
		if after.AccessToken == before.AccessToken {
			t.Fatal("the pair did not change")
		}
		if r.c.refusedDial() || r.c.Status().TokenRefused {
			t.Error("the new token is refused; the mark is on the one that was refused and on nothing else")
		}
		// AND IT IS PRESENTED AT ONCE: the page that follows carries the new token
		// and is answered.
		if _, err := r.c.fetch(context.Background(), 0); err != nil {
			t.Errorf("the page after the rotation: %v", err)
		}
		if last := r.net.on("/sync"); last[len(last)-1].token != after.AccessToken {
			t.Errorf("the page carried %q, want the rotated token %q", unpad(last[len(last)-1].token), unpad(after.AccessToken))
		}
		assertNoReplays(t, r.e.f)
	})
}

// THE HOLD'S ONE REQUEST IS SPENT BY CATENARY'S ANSWER, NOT BY THE SENDING. The
// guarantee is one refusal THE CLIENT SAW per hold, and the two ways that can
// differ from one refusal per hold are both here, each with its own count: a
// request that never arrived is retried as any failed page and costs the server
// nothing, and one that arrived and was refused but whose answer was lost costs
// the server a second line while the client still learns only once.
func TestTheHoldsOneSyncIsRetriedUntilCatenaryAnswers(t *testing.T) {
	t.Run("nothing reached Catenary: retried on the ordinary backoff, and one refusal in the end", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
		stop := r.run()
		defer stop()
		r.await("the token to be refused and a hold to be in force", func() bool {
			s := r.c.Status()
			return s.TokenRefused && s.ChainLength >= 1
		})

		r.net.setDead("/sync", true)
		refusals, syncs, links := r.d.count("/sync"), r.net.count("/sync"), r.chainLen()
		r.await("the probe to be retried on the catch-up backoff", func() bool {
			return r.net.count("/sync") >= syncs+3
		})
		// FROZEN FROM HERE: no further hold can end, so what follows is one hold's
		// worth of requests and nothing else.
		r.net.freeze(true)
		if got := r.d.count("/sync"); got != refusals {
			t.Errorf("the server counted %d refusals for requests that never arrived", got-refusals)
		}
		if got := r.chainLen(); got != links {
			t.Errorf("the chain grew by %d with no answer from Catenary", got-links)
		}

		r.net.setDead("/sync", false)
		r.await("the probe to be delivered and to drive its attempt", func() bool { return r.chainLen() > links })
		time.Sleep(10 * realQuantum)
		if got := r.d.count("/sync") - refusals; got != 1 {
			t.Errorf("the server refused %d requests for this hold, want the 1 the client finally got an answer to", got)
		}
		if got := r.chainLen(); got != links+1 {
			t.Errorf("the chain grew by %d for one answered probe, want 1", got-links)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("delivered and refused, and the answer lost: the server counts a second, the client sees one", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
		stop := r.run()
		defer stop()
		r.await("the token to be refused and a hold to be in force", func() bool {
			s := r.c.Status()
			return s.TokenRefused && s.ChainLength >= 1
		})
		refusals, links := r.d.count("/sync"), r.chainLen()
		r.d.set(func(d *door) { d.loseAnswers = 1 })

		r.await("the lost answer, then the one the client sees, then its attempt", func() bool {
			return r.chainLen() > links
		})
		r.net.freeze(true)
		if got := r.d.count("/sync") - refusals; got != 2 {
			t.Errorf("the server refused %d requests for this hold, want 2 — the one whose answer was lost, "+
				"and the one the client saw", got)
		}
		if got := r.chainLen() - links; got != 1 {
			t.Errorf("the client made %d attempts for this hold, want the 1 its single answer drove", got)
		}
		assertNoReplays(t, r.e.f)
	})
}

// --- criterion 7 · the clock is never what decides --------------------------

// A REFUSED TOKEN REACHES ITS REFRESH WHATEVER THE CLOCK SAYS, because the hold's
// one /sync drives refreshAfter401 and that call site is not due-gated. A device
// revoked while disconnected therefore still goes terminal, and a device whose
// clock is an hour out still rotates — neither waits for the clock to call the
// pair due, and in both arrangements it never does.
func TestARefusedTokenReachesItsRefreshWhateverTheClockSays(t *testing.T) {
	t.Run("a device revoked while disconnected, whose clock says ten minutes are left", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{cred: func(c Credential) Credential {
			c.AccessExpiresAt = c.AccessExpiresAt.Add(10 * time.Minute)
			return c
		}})
		held, _ := r.j.Credential()
		if refreshDue(held, r.clk.now()) {
			t.Fatal("the arrangement is wrong: the clock already calls this pair due")
		}
		// A NON-EMPTY CHAIN AND A HELD REFRESH, built the way an attempt that
		// settled nothing leaves them — a link and the stamp that rides with it.
		if _, ok, err := r.c.propose(held.RefreshToken, false); err != nil || !ok {
			t.Fatalf("the link: ok=%v, %v", ok, err)
		}
		r.e.f.mu.Lock()
		r.e.f.revoked = true
		r.e.f.mu.Unlock()

		stop := r.run()
		defer stop()
		r.await("the client to stop", func() bool { return r.c.Status().Terminal.Kind != NotTerminal })
		stop()

		s := r.c.Status()
		if s.Terminal.Kind != TerminalCredential {
			t.Errorf("the client stopped with %v, want the credential terminal a revoked device owes", s.Terminal.Kind)
		}
		if refreshDue(held, r.clk.now()) {
			t.Errorf("the clock came to call the pair due after %s; this must not be what drove the refresh",
				r.elapsed())
		}
		if got := r.d.count("/sync"); got != 1 {
			t.Errorf("the server refused %d pages before the client stopped, want the 1 hold's worth", got)
		}
		if cr, held := r.j.Credential(); !held || cr.RefreshToken != firstRefresh {
			t.Errorf("terminal changed the stored credential: %+v", cr)
		}
	})

	t.Run("a clock an hour out, which says the token is live", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{cred: func(c Credential) Credential {
			c.ClockOffset = -time.Hour // the device is an hour ahead of the server
			return c
		}})
		held, _ := r.j.Credential()
		if refreshDue(held, r.clk.now()) {
			t.Fatal("the arrangement is wrong: the clock already calls this pair due")
		}
		if _, ok, err := r.c.propose(held.RefreshToken, false); err != nil || !ok {
			t.Fatalf("the link: ok=%v, %v", ok, err)
		}

		stop := r.run()
		defer stop()
		r.await("the client to rotate and reconnect", func() bool {
			s := r.c.Status()
			return s.Refreshes == 1 && s.Ready
		})
		stop()

		if refreshDue(held, r.clk.now()) {
			t.Errorf("the clock came to call the pair due after %s; the refresh must not have waited for it",
				r.elapsed())
		}
		if cr, _ := r.j.Credential(); cr.RefreshToken != r.e.f.live() {
			t.Errorf("the client holds %q, the server's live token is %q", cr.RefreshToken, r.e.f.live())
		}
		assertNoReplays(t, r.e.f)
	})
}

// --- criterion 8 · it only removes requests ---------------------------------

// THE RULED CLIENT'S REQUESTS ARE THE UNRULED ONE'S WITH SOME REMOVED, PATH BY
// PATH. Both runs are the same simulated hour over the same deterministic
// generator, so the tapes are comparable request for request. Per path rather than
// over one interleaved tape, because the dial and the page come from two
// goroutines and their interleaving is not a property of the rule; what IS the
// rule's is that within a path nothing is added, nothing is reordered, and the
// refresh attempts are not merely a subsequence but IDENTICAL — same token, same
// proposal, same order — which is the claim that exchange's walk is untouched.
func TestTheRuleOnlyRemovesRequests(t *testing.T) {
	hour := func(faults Faults, quantum time.Duration) (*refusedRig, []request) {
		r := newRefusedRig(t, refusedOpts{
			dead: []string{"/refresh"}, faults: faults, quantum: quantum, rand: &countingRand{},
		})
		stop := r.run()
		defer stop()
		r.awaitSim(time.Hour)
		stop()
		assertNoReplays(t, r.e.f)
		return r, r.net.tape()
	}
	ruled, held := hour(Faults{}, realQuantum)
	unruled, all := hour(Faults{PresentRefusedToken: true}, fastQuantum)

	if len(held) >= len(all) {
		t.Fatalf("the ruled client sent %d requests and the unruled one %d; the comparison proves nothing "+
			"unless fewer went out", len(held), len(all))
	}
	for _, path := range []string{"/ws", "/sync", "/refresh"} {
		sub, sup := ruled.net.on(path), unruled.net.on(path)
		if len(sub) > len(sup) {
			t.Errorf("the ruled client sent %d %s requests and the unruled one %d", len(sub), path, len(sup))
		}
		if !subsequenceOf(sub, sup) {
			t.Errorf("the ruled %s tape is not the unruled one with requests removed:\nruled   %v\nunruled %v",
				path, carried(sub), carried(sup[:min(len(sup), 8)]))
		}
	}
	// THE ATTEMPTS ARE THE SAME ATTEMPTS. CANT-127's backoff bounds them in both
	// runs, so this rule removed none of them — and each presents the token it
	// would have, with the proposal it would have.
	a, b := ruled.net.on("/refresh"), unruled.net.on("/refresh")
	if len(a) != len(b) {
		t.Errorf("%d refresh attempts ruled against %d unruled; this rule removes dials and pages, not attempts",
			len(a), len(b))
	}
	for i := range min(len(a), len(b)) {
		if a[i].token != b[i].token || a[i].proposal != b[i].proposal {
			t.Errorf("attempt %d presented %s ruled and %s unruled", i+1, carried(a[i:i+1]), carried(b[i:i+1]))
		}
	}
	for i, rq := range a {
		if rq.token == "" || rq.proposal == "" {
			t.Errorf("attempt %d carried %+v; every /refresh presents a token with a proposal", i+1, rq)
		}
	}

	// AND CANT-127'S GATE IS UNTOUCHED: a /refresh RESPONSE still does not open it.
	// The page's 401 opens it, one attempt goes out and is answered with a 503 that
	// settles nothing — Catenary's own response, on the /refresh path — and the gate
	// is closed again behind it.
	t.Run("a /refresh response does not open the gate", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{script: []string{bare503}})
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the page = %v, want Catenary's own 401", err)
		}
		if r.chainLen() != 1 {
			t.Fatalf("the chain is %d links, want the reactive attempt's one", r.chainLen())
		}
		if got := r.c.refreshHoldQuiet(); got != RefreshHeldUnreachable {
			t.Errorf("the hold is %v after a 503 to the attempt, want the gate closed: a /refresh response "+
				"is not Catenary answering this context, and CANT-129 does not change that", got)
		}
		assertNoReplays(t, r.e.f)
	})
}

// --- criterion 9 · a person can see it, and it is inert without refresh -----

func TestAPersonCanSeeARefusedToken(t *testing.T) {
	t.Run("the mark and the hold are readable at once, in every combination", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
		if s := r.c.Status(); s.TokenRefused || s.RefreshHold != RefreshNotHeld || !s.NextRefreshAt.IsZero() {
			t.Errorf("a settled credential reports refused=%v hold=%v next=%v, want false, none and the zero time",
				s.TokenRefused, s.RefreshHold, s.NextRefreshAt)
		}
		// The first 401 marks the token, and the reactive attempt it drove leaves a
		// link stamped at the same instant — which CANT-127's gate reads as a tie,
		// and a tie is closed.
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the page = %v, want Catenary's own 401", err)
		}
		first, _ := r.j.LastSent()
		if r.chainLen() != 1 {
			t.Fatalf("the chain is %d links, want the reactive attempt's one", r.chainLen())
		}

		// REFUSED, AND THE BACKOFF IS WHAT HOLDS: a second answer, a second later,
		// is strictly later than that stamp and opens the gate — and the attempt it
		// would allow is held by the delay.
		r.clk.add(time.Second)
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the second page = %v, want Catenary's own 401", err)
		}
		s := r.c.Status()
		if !s.TokenRefused || s.RefreshHold != RefreshHeldBackoff {
			t.Errorf("refused=%v hold=%v, want the mark and the backoff", s.TokenRefused, s.RefreshHold)
		}
		if want := first.Add(refreshDelay(1)); !s.NextRefreshAt.Equal(want) {
			t.Errorf("next refresh at %v, want last_sent_at plus the delay, %v", s.NextRefreshAt, want)
		}
		if s.Terminal.Kind != NotTerminal || s.Connected {
			t.Errorf("terminal %v connected %v; a refused token is neither a terminal nor a lost socket",
				s.Terminal.Kind, s.Connected)
		}
		if !r.c.refusedHold() {
			t.Error("the wait is over while the delay still holds")
		}

		// REFUSED, AND THE GATE IS WHAT HOLDS: one more link, stamped after the
		// last answer.
		r.clk.add(time.Second)
		chain := r.j.Chain()
		if _, ok, err := r.c.propose(chain[len(chain)-1].Proposal, false); err != nil || !ok {
			t.Fatalf("the second link: ok=%v, %v", ok, err)
		}
		second, _ := r.j.LastSent()
		s = r.c.Status()
		if !s.TokenRefused || s.RefreshHold != RefreshHeldUnreachable {
			t.Errorf("refused=%v hold=%v, want the mark and the gate", s.TokenRefused, s.RefreshHold)
		}
		if want := second.Add(refreshDelay(2)); !s.NextRefreshAt.Equal(want) {
			t.Errorf("next refresh at %v under the gate, want %v — it is defined in BOTH hold states",
				s.NextRefreshAt, want)
		}

		// REFUSED, AND NOTHING HOLDS THE REFRESH AT ALL: an answer later than that
		// send reopens the gate, and then the delay elapses. This is the moment the
		// hold's one page request goes out.
		r.clk.add(time.Second)
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the third page = %v, want Catenary's own 401", err)
		}
		r.clk.set(second.Add(refreshDelay(2) + time.Second))
		if s := r.c.Status(); !s.TokenRefused || s.RefreshHold != RefreshNotHeld {
			t.Errorf("refused=%v hold=%v, want the mark with no hold at all — a single enum could not say both",
				s.TokenRefused, s.RefreshHold)
		}
		if r.c.refusedHold() {
			t.Error("the wait outlasted its deadline")
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("each hold without the mark, and an exhausted dial backoff without either", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{accessLive: true, dead: []string{"/refresh", "/ws"}})
		// A PAGE THAT IS ANSWERED marks nothing, and the attempt it allows leaves a
		// hold behind: both states, neither with a mark.
		if _, err := r.c.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if err := r.c.refreshDueWhenAllowed(context.Background()); err == nil {
			t.Fatal("the black-holed refresh reported success")
		}
		stamp, _ := r.j.LastSent()
		r.clk.set(stamp.Add(time.Second))
		if _, err := r.c.fetch(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if s := r.c.Status(); s.TokenRefused || s.RefreshHold != RefreshHeldBackoff {
			t.Errorf("refused=%v hold=%v, want the backoff and no mark", s.TokenRefused, s.RefreshHold)
		}
		r.clk.add(-2 * time.Second) // the answer is now older than the send
		if s := r.c.Status(); s.TokenRefused || s.RefreshHold != RefreshNotHeld && s.RefreshHold != RefreshHeldUnreachable {
			t.Errorf("refused=%v hold=%v, want a hold with no mark", s.TokenRefused, s.RefreshHold)
		}
		// AND A DIAL BACKOFF THAT HAS GROWN is neither: the dials are failing and
		// nothing has refused anything.
		r.clk.set(stamp.Add(time.Second))
		stop := r.run()
		defer stop()
		r.await("the dial backoff to have grown", func() bool { return r.c.Status().DialErrors >= 3 })
		stop()
		if s := r.c.Status(); s.TokenRefused || s.DialsWithheld != 0 {
			t.Errorf("refused=%v dials withheld %d; an exhausted dial backoff is a third thing",
				s.TokenRefused, s.DialsWithheld)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("the counters are in requests, and one poll is one request", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{dead: []string{"/refresh"}})
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the page = %v, want Catenary's own 401", err)
		}
		// A HOLD FOUR POLLS LONG: a chain of three owes 20 s, and the wait polls at
		// the 5 s dial cadence.
		chain := r.j.Chain()
		for range 2 {
			chain = r.j.Chain()
			if _, ok, err := r.c.propose(chain[len(chain)-1].Proposal, false); err != nil || !ok {
				t.Fatalf("a link: ok=%v, %v", ok, err)
			}
		}
		stamp, _ := r.j.LastSent()
		r.clk.set(stamp)
		if want := 4 * simPerDial; refreshDelay(3) != want {
			t.Fatalf("a chain of 3 owes %s, and this test is written for %s", refreshDelay(3), want)
		}
		if !r.c.waitWhileRefused(context.Background(), withheldSync) {
			t.Fatal("the wait reported a context that had ended")
		}
		if got := r.c.Status().SyncsWithheld; got != 4 {
			t.Errorf("%d pages withheld over a %s hold at %s a poll, want 4", got, refreshDelay(3), simPerDial)
		}
		// THE DIAL'S WAIT HAS NO DEADLINE — it ends on the pair, so what ends it
		// here is the context.
		ctx, cancel := context.WithCancel(context.Background())
		polls := 0
		r.c.cfg.Pause = func(context.Context, time.Duration) bool {
			if polls++; polls == 3 {
				cancel()
				return false
			}
			return true
		}
		if r.c.waitWhileRefused(ctx, withheldDial) {
			t.Error("the dial's wait ended without the pair changing")
		}
		if got := r.c.Status().DialsWithheld; got != 3 {
			t.Errorf("%d dials withheld over %d polls, want one each", got, polls)
		}
		if lines := r.log.matching("withheld"); len(lines) != 0 {
			t.Errorf("%d log lines for %d withheld requests, want none — that stream is what this ticket removes: %v",
				len(lines), 7, lines)
		}
		assertNoReplays(t, r.e.f)
	})

	t.Run("with Config.Refresh off the rule is inert", func(t *testing.T) {
		r := newRefusedRig(t, refusedOpts{})
		r.c.cfg.Refresh = false
		if _, err := r.c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
			t.Fatalf("the page = %v, want Catenary's own 401", err)
		}
		if s := r.c.Status(); s.TokenRefused {
			t.Error("a client that cannot refresh marked its token refused; withholding its requests would be a stop")
		}
		if r.c.refusedDial() || r.c.refusedHold() {
			t.Error("a client that cannot refresh is waiting for a pair nothing can change")
		}
		stop := r.run()
		defer stop()
		r.await("the client to dial as it always did", func() bool { return r.c.Status().Dials >= 3 })
		stop()
		if s := r.c.Status(); s.DialsWithheld != 0 || s.SyncsWithheld != 0 {
			t.Errorf("%d dials and %d pages withheld with refresh off", s.DialsWithheld, s.SyncsWithheld)
		}
		assertNoReplays(t, r.e.f)
	})
}
