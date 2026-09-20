package main

// CANT-126 against the real router and a real database: the client half of the
// proposed-successor exchange (CANT-31's record §3), with the real
// RotateRefreshProposing on the other end and a proxy between them that loses
// what was really said.
//
// internal/client's chain_test.go pins the walk against a stub with no grace
// window. Here the grace window is real, so every test AGES THE FAMILY PAST IT
// between attempts: by the time the client tries again, any spent token it
// presented would be a replay, and the real reuse detector would revoke the
// real device. "No revocation" below is that detector's verdict.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// What the proxy does with the next POST /refresh. Past the end of the script
// it forwards honestly.
const (
	forward      = "forward"
	loseResponse = "forward, let the server commit and answer, then drop the response"
	loseRequest  = "drop the request before the server sees it"
	parkRequest  = "hold the request back; the client gives up on it"
	// unparkFirst delivers the parked request and waits for the server to
	// answer it BEFORE forwarding this one: the delayed original wins the race.
	unparkFirst = "deliver the parked request first, then forward"
)

type refreshSeen struct {
	token, proposal string
	status          int // what the SERVER answered; 0 when it never saw the request
}

type lossyProxy struct {
	t        *testing.T
	srv      *httptest.Server
	upstream string
	// onArrival runs as each /refresh reaches the proxy, before anything else.
	onArrival func(refreshSeen)

	mu     sync.Mutex
	script []string
	seen   []refreshSeen
	parked []byte
}

func newLossyProxy(t *testing.T, upstream string, script ...string) *lossyProxy {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	p := &lossyProxy{t: t, upstream: upstream, script: script}
	rest := httputil.NewSingleHostReverseProxy(u) // /sync and the WebSocket upgrade, untouched
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			p.refresh(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func dropConn(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// deliver is one real POST /refresh to the real server, read to the end — so
// when it returns, the rotation it asked for has committed or been refused.
func (p *lossyProxy) deliver(body []byte) (*http.Response, []byte) {
	resp, err := http.Post(p.upstream+"/refresh", "application/json", bytes.NewReader(body))
	if err != nil {
		p.t.Errorf("proxy: upstream /refresh: %v", err)
		return nil, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (p *lossyProxy) refresh(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req wire.RefreshRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.t.Errorf("proxy: the client sent a request the generated decoder refuses: %v", err)
	}
	seen := refreshSeen{token: string(req.RefreshToken)}
	if req.ProposedRefreshToken != nil {
		seen.proposal = string(*req.ProposedRefreshToken)
	}
	if p.onArrival != nil {
		p.onArrival(seen)
	}

	p.mu.Lock()
	step := forward
	if len(p.script) > 0 {
		step, p.script = p.script[0], p.script[1:]
	}
	var parked []byte
	switch step {
	case parkRequest:
		p.parked = body
	case unparkFirst:
		parked, p.parked = p.parked, nil
	}
	p.mu.Unlock()

	switch step {
	case loseRequest:
		p.record(seen)
		dropConn(w)
		return
	case parkRequest:
		p.record(seen)
		<-r.Context().Done() // until the client hangs up
		return
	case unparkFirst:
		if resp, _ := p.deliver(parked); resp == nil || resp.StatusCode != http.StatusOK {
			p.t.Errorf("proxy: the parked request did not rotate: %v", resp)
		}
	}

	resp, raw := p.deliver(body)
	if resp == nil {
		dropConn(w)
		return
	}
	seen.status = resp.StatusCode
	p.record(seen)
	if step == loseResponse {
		dropConn(w)
		return
	}
	for _, h := range []string{"Content-Type", "Date", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)
}

func (p *lossyProxy) record(s refreshSeen) {
	p.mu.Lock()
	p.seen = append(p.seen, s)
	p.mu.Unlock()
}

func (p *lossyProxy) log() []refreshSeen {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]refreshSeen(nil), p.seen...)
}

func statuses(seen []refreshSeen) []int {
	out := make([]int, len(seen))
	for i, s := range seen {
		out[i] = s.status
	}
	return out
}

// ageRotations moves every refresh token the device holds a minute into the
// past, which puts every rotation so far OUTSIDE ReuseGraceWindow: from here
// on, presenting a spent token is a replay and not an echo.
func (k *killRig) ageRotations(device wire.Uuid) {
	k.t.Helper()
	if _, err := k.pool.Exec(k.ctx,
		`UPDATE refresh_tokens SET issued_at = issued_at - interval '1 minute' WHERE device_id = $1`, string(device)); err != nil {
		k.t.Fatal(err)
	}
}

// chainClient is a refreshing client that reaches the server through base, with
// a clock `ahead` of the real one so its pair reads as due.
func (k *killRig) chainClient(base string, j *client.Journal, ahead time.Duration, tune func(*client.Config)) *client.Client {
	k.t.Helper()
	cfg := client.Config{
		BaseURL: base, Journal: j, ClientInfo: "cant-126-test", Refresh: true,
		Now:        func() time.Time { return time.Now().Add(ahead) },
		BackoffMin: 20 * time.Millisecond, BackoffMax: 250 * time.Millisecond,
	}
	if tune != nil {
		tune(&cfg)
	}
	c, err := client.New(cfg)
	if err != nil {
		k.t.Fatal(err)
	}
	return c
}

func enrolledJournal(t *testing.T, dev wire.EnrollResponse) *client.Journal {
	t.Helper()
	cred, err := client.CredentialFromEnroll(dev)
	if err != nil {
		t.Fatal(err)
	}
	j := client.NewJournal()
	if err := j.Enroll(cred); err != nil {
		t.Fatal(err)
	}
	return j
}

// late is fourteen minutes into a fifteen-minute access token: due.
const late = 14 * time.Minute

// getsIn proves the pair the journal ended up with is one the real server
// honours: a new client over it, straight to the server, reaches `ready`.
func (k *killRig) getsIn(j *client.Journal, what string) {
	k.t.Helper()
	c := k.refreshingClient(wire.EnrollResponse{}, j, false, nil, true)
	awaitClient(k.t, c, what, func() bool { s := c.Status(); return s.Ready && s.CaughtUp })
}

// LOST RESPONSES ARE SURVIVABLE (plan criteria 21 and 22). The real server
// commits each rotation and the proxy drops what it answered. The client is
// asked again only after the grace window has passed.
func TestLostRefreshResponsesCostNothing(t *testing.T) {
	for _, tc := range []struct {
		name          string
		script        []string
		wantStatuses  []int
		wantWalkBacks int
		wantTokens    int
	}{
		{
			name:         "criterion 21 — one lost response: the newest token is presented, and there is no 401",
			script:       []string{loseResponse},
			wantStatuses: []int{200, 200}, wantTokens: 3,
		},
		{
			name:         "criterion 22 — two in a row, the second during recovery: still no 401",
			script:       []string{loseResponse, loseResponse},
			wantStatuses: []int{200, 200, 200}, wantTokens: 4,
		},
		{
			name:   "criterion 22 — the second loss is of the REQUEST: one 401 in the walk-back, not terminal, original proposal reused",
			script: []string{loseResponse, loseRequest},
			// (r0,P0) 200 lost · (P0,P1) never seen · (P1,P2) 401 · (P0,P1) 200
			wantStatuses: []int{200, 0, 401, 200}, wantWalkBacks: 1, wantTokens: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKillRig(t, nil)
			dev := k.enroll(k.cast().theo, "theo's phone")
			proxy := newLossyProxy(t, k.base(), tc.script...)
			j := enrolledJournal(t, dev)
			c := k.chainClient(proxy.srv.URL, j, late, nil)

			for attempt := range len(tc.script) {
				err := c.RefreshIfDue(k.ctx)
				var term *client.TerminalError
				if err == nil || errors.As(err, &term) {
					t.Fatalf("attempt %d = %v, want an outcome nobody learned", attempt+1, err)
				}
				k.ageRotations(dev.DeviceID)
			}
			if err := c.RefreshIfDue(k.ctx); err != nil {
				t.Fatalf("the recovering refresh: %v", err)
			}

			seen := proxy.log()
			if got := statuses(seen); !slices.Equal(got, tc.wantStatuses) {
				t.Errorf("the server answered %v, want %v", got, tc.wantStatuses)
			}
			// NEWEST FIRST, AND THE ORIGINAL PROPOSAL ON A RETURN VISIT: every
			// token presented twice carried the same proposal both times.
			first := map[string]string{}
			for _, s := range seen {
				if was, again := first[s.token]; again && was != s.proposal {
					t.Errorf("token presented twice with two proposals: %q then %q", was, s.proposal)
				}
				first[s.token] = s.proposal
			}
			s := c.Status()
			if s.RefreshWalkBacks != tc.wantWalkBacks || s.Refreshes != 1 || s.Terminal.Kind != client.NotTerminal {
				t.Errorf("walk-backs %d, refreshes %d, terminal %v; want %d, 1, none", s.RefreshWalkBacks, s.Refreshes, s.Terminal.Kind, tc.wantWalkBacks)
			}
			if tokens, revoked, dead := k.deviceHealth(dev.DeviceID); tokens != tc.wantTokens || revoked != 0 || dead {
				t.Errorf("the device holds %d refresh tokens, %d revoked, device revoked=%v; want %d, 0, false", tokens, revoked, dead, tc.wantTokens)
			}
			if got := j.Chain(); len(got) != 0 {
				t.Errorf("the chain is %v after an answer, want empty", got)
			}
			k.getsIn(j, "the recovered pair gets in")
		})
	}
}

// NEGATIVE CONTROL for the above, recorded because a survivability claim
// nobody has seen fail is a claim about the proxy. The same single lost
// response, against a client that forgets its proposal: it presents the spent
// token again, the REAL detector reads a replay, and the device is revoked.
func TestWithoutTheChainALostResponseRevokesTheDevice(t *testing.T) {
	k := newKillRig(t, nil)
	dev := k.enroll(k.cast().theo, "theo's phone")
	proxy := newLossyProxy(t, k.base(), loseResponse)
	c := k.chainClient(proxy.srv.URL, enrolledJournal(t, dev), late, func(cfg *client.Config) { cfg.Faults.NoChain = true })

	if err := c.RefreshIfDue(k.ctx); err == nil {
		t.Fatal("the lost response was not noticed")
	}
	k.ageRotations(dev.DeviceID)
	err := c.RefreshIfDue(k.ctx)
	var term *client.TerminalError
	if !errors.As(err, &term) {
		t.Fatalf("second refresh = %v, want a terminal", err)
	}
	if _, revoked, dead := k.deviceHealth(dev.DeviceID); revoked == 0 || !dead {
		t.Errorf("%d tokens revoked, device revoked=%v; the control shows nothing", revoked, dead)
	}
}

// KILLED BETWEEN PERSISTING THE CHAIN AND THE RESPONSE (plan criterion 29). The
// proposal is durable before the request leaves — checked at the moment the
// request ARRIVES — and then the process dies without ever learning what the
// server did. A new client over the same journal recovers, whichever it was.
func TestAClientKilledMidRefreshRecoversAgainstTheRealServer(t *testing.T) {
	for _, tc := range []struct {
		name, fate    string
		wantStatuses  []int
		wantWalkBacks int
	}{
		{"the rotation committed", loseResponse, []int{200, 200}, 0},
		{"the request never arrived", loseRequest, []int{0, 401, 200}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKillRig(t, nil)
			dev := k.enroll(k.cast().theo, "theo's phone")
			proxy := newLossyProxy(t, k.base(), tc.fate)
			j := enrolledJournal(t, dev)
			first := k.chainClient(proxy.srv.URL, j, late, nil)

			var once sync.Once
			proxy.onArrival = func(s refreshSeen) {
				once.Do(func() {
					if got := j.Chain(); len(got) != 1 || got[0].Token != s.token || got[0].Proposal != s.proposal || s.proposal == "" {
						t.Errorf("when the request arrived the journal held %v; the request carried (%q, %q)", got, s.token, s.proposal)
					}
					first.Kill()
				})
			}
			if err := first.RefreshIfDue(k.ctx); err == nil {
				t.Fatal("the killed client's refresh reported success")
			}
			enrolled, _ := client.CredentialFromEnroll(dev)
			if held, _ := j.Credential(); held.RefreshToken != enrolled.RefreshToken {
				t.Fatal("the killed client wrote a rotation it never saw answered")
			}
			k.ageRotations(dev.DeviceID)

			second := k.chainClient(proxy.srv.URL, j, late, nil)
			if err := second.RefreshIfDue(k.ctx); err != nil {
				t.Fatalf("the restarted client: %v", err)
			}
			if got := statuses(proxy.log()); !slices.Equal(got, tc.wantStatuses) {
				t.Errorf("the server answered %v, want %v", got, tc.wantStatuses)
			}
			if s := second.Status(); s.RefreshWalkBacks != tc.wantWalkBacks || s.Terminal.Kind != client.NotTerminal {
				t.Errorf("walk-backs %d, terminal %v; want %d, none", s.RefreshWalkBacks, s.Terminal.Kind, tc.wantWalkBacks)
			}
			if _, revoked, dead := k.deviceHealth(dev.DeviceID); revoked != 0 || dead {
				t.Errorf("%d tokens revoked, device revoked=%v", revoked, dead)
			}
			k.getsIn(j, "the restarted client's pair gets in")
		})
	}
}

// WHY THE ORIGINAL PROPOSAL IS REUSED: A RACING COMMIT COLLIDES INSTEAD OF
// FORKING. The first request is held up in the network and the client gives up
// on it. Recovering, it walks back to the same token — and at that moment the
// delayed original lands and wins. Because both carry the SAME proposal, the
// loser collides with the winner's successor and the real server answers 503
// `present_proposal`; the client stops presenting that token, presents the
// proposal, and is in. With a second proposal the loser would simply have been
// a spent token: a 401 on the oldest link, and a person re-enrolling.
func TestADelayedOriginalCollidesWithItsOwnRetry(t *testing.T) {
	k := newKillRig(t, nil)
	dev := k.enroll(k.cast().theo, "theo's phone")
	proxy := newLossyProxy(t, k.base(), parkRequest, forward, unparkFirst)
	j := enrolledJournal(t, dev)
	c := k.chainClient(proxy.srv.URL, j, late, nil)

	impatient, cancel := context.WithTimeout(k.ctx, 300*time.Millisecond)
	defer cancel()
	if err := c.RefreshIfDue(impatient); err == nil {
		t.Fatal("the parked request was answered")
	}
	if err := c.RefreshIfDue(k.ctx); err != nil {
		t.Fatalf("the recovering refresh: %v", err)
	}

	// parked · (P0,P1) 401, P0 does not exist yet · (r0,P0) 503, the original
	// just won · (P0,P1) 200.
	seen := proxy.log()
	if got, want := statuses(seen), []int{0, 401, 503, 200}; !slices.Equal(got, want) {
		t.Fatalf("the server answered %v, want %v", got, want)
	}
	if seen[2].token != seen[0].token || seen[2].proposal != seen[0].proposal {
		t.Errorf("the retry was (%q, %q), want the original's (%q, %q)", seen[2].token, seen[2].proposal, seen[0].token, seen[0].proposal)
	}
	if seen[3].token != seen[0].proposal || seen[3].proposal != seen[1].proposal {
		t.Errorf("after the 503 the client presented (%q, %q), want the proposal, with ITS original proposal", seen[3].token, seen[3].proposal)
	}
	if s := c.Status(); s.Terminal.Kind != client.NotTerminal || s.Refreshes != 1 {
		t.Errorf("terminal %v, refreshes %d", s.Terminal.Kind, s.Refreshes)
	}
	if tokens, revoked, dead := k.deviceHealth(dev.DeviceID); tokens != 3 || revoked != 0 || dead {
		t.Errorf("the device holds %d refresh tokens, %d revoked, device revoked=%v; want 3, 0, false — one family, never forked", tokens, revoked, dead)
	}
	k.getsIn(j, "the pair that came out of the race gets in")
}

// NEGATIVE CONTROL for the above: the same race, against a client that mints a
// new proposal whenever it presents a token again. Nothing collides, so the
// server has no way to know the loser is a retry: it is a spent token, the
// answer is Catenary's own 401 on the oldest link, and the client stops with a
// working device behind it — holding a successor it does not know.
func TestWithASecondProposalTheDelayedOriginalCostsTheDevice(t *testing.T) {
	k := newKillRig(t, nil)
	dev := k.enroll(k.cast().theo, "theo's phone")
	proxy := newLossyProxy(t, k.base(), parkRequest, forward, unparkFirst)
	c := k.chainClient(proxy.srv.URL, enrolledJournal(t, dev), late, func(cfg *client.Config) { cfg.Faults.ProposeAfresh = true })

	impatient, cancel := context.WithTimeout(k.ctx, 300*time.Millisecond)
	defer cancel()
	if err := c.RefreshIfDue(impatient); err == nil {
		t.Fatal("the parked request was answered")
	}
	err := c.RefreshIfDue(k.ctx)
	var term *client.TerminalError
	if !errors.As(err, &term) || term.Terminal.Kind != client.TerminalCredential {
		t.Fatalf("the recovering refresh = %v, want a credential terminal; the control shows nothing", err)
	}
	if got, want := statuses(proxy.log()), []int{0, 401, 401}; !slices.Equal(got, want) {
		t.Errorf("the server answered %v, want %v", got, want)
	}
}

// 503 `fresh_proposal`, THE CLIENT'S HALF (plan criterion 23). A device whose
// generator repeats itself proposes a string the server already stores, against
// a token that is perfectly good. The real server says so, and the client mints
// again and succeeds — no 401, nothing terminal, nothing revoked.
func TestAReusedProposalIsAnsweredWithAFreshOne(t *testing.T) {
	k := newKillRig(t, nil)
	dev := k.enroll(k.cast().theo, "theo's phone")
	proxy := newLossyProxy(t, k.base())
	j := enrolledJournal(t, dev)

	a, b := strings.Repeat("\x01", 32), strings.Repeat("\x02", 32)
	repeating := io.MultiReader(strings.NewReader(a+b+a), rand.Reader)
	// Three refreshes, each by a clock far enough ahead to find the last
	// rotation's pair due: proposals A, then B, then A AGAIN — which by then is
	// a stored, spent token — and then whatever the real generator says.
	for i, ahead := range []time.Duration{late, 2 * late, 3 * late} {
		c := k.chainClient(proxy.srv.URL, j, ahead, func(cfg *client.Config) { cfg.Rand = repeating })
		if err := c.RefreshIfDue(k.ctx); err != nil {
			t.Fatalf("refresh %d: %v", i+1, err)
		}
		if s := c.Status(); s.Refreshes != 1 || s.Terminal.Kind != client.NotTerminal {
			t.Fatalf("refresh %d: refreshes %d, terminal %v", i+1, s.Refreshes, s.Terminal.Kind)
		}
	}
	seen := proxy.log()
	if got, want := statuses(seen), []int{200, 200, 503, 200}; !slices.Equal(got, want) {
		t.Fatalf("the server answered %v, want %v", got, want)
	}
	if seen[2].proposal != seen[0].proposal {
		t.Fatal("the third proposal was not the first one again; the test reused nothing")
	}
	if seen[3].token != seen[2].token || seen[3].proposal == seen[2].proposal {
		t.Errorf("after fresh_proposal the client sent (%q, %q), want the same token with a new proposal", seen[3].token, seen[3].proposal)
	}
	if tokens, revoked, dead := k.deviceHealth(dev.DeviceID); tokens != 4 || revoked != 0 || dead {
		t.Errorf("the device holds %d refresh tokens, %d revoked, device revoked=%v; want 4, 0, false", tokens, revoked, dead)
	}
	k.getsIn(j, "the pair minted after the collision gets in")
}
