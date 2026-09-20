package client

// CANT-126: record §3, the client half of the proposed-successor exchange,
// against a stub that keeps a refresh family the way the real server does and
// loses requests and responses on instruction. The same rules against the real
// router, a real database and a proxy that drops what the real server said are
// cmd/catenary/refreshchain_test.go.
//
// THE STUB HAS NO GRACE WINDOW. Every presentation of a spent token is counted
// as a replay, which is what the real server concludes once ten seconds have
// passed — so a recovery that passes here never presented a spent token AT
// ALL, rather than presenting one quickly enough to be forgiven.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

// What the stub does with the next /refresh. Anything past the end of the
// script is answered honestly.
const (
	answer       = "answer"
	loseResponse = "rotate, then lose the response"
	loseRequest  = "lose the request before it arrives"
	proxy401     = "a 401 from a hop in front"
	bare503      = "a 503 that says nothing"
	sayPresent   = "503 present_proposal, whatever is true"
	sayFresh     = "503 fresh_proposal, whatever is true"
)

type presentation struct{ token, proposal string }

type family struct {
	t   *testing.T
	srv *httptest.Server
	// onRefresh, when set, runs as each /refresh arrives and before it is
	// answered, with what it carried.
	onRefresh func(presentation)

	mu     sync.Mutex
	script []string
	// successor is every token the family has held: "" while it is live, and
	// what it was rotated into once it is spent.
	successor map[string]string
	// ignoreProposals is a server that predates the field.
	ignoreProposals bool
	revoked         bool
	minted          int

	seen     []presentation
	statuses []int
	replays  int
}

func newFamily(t *testing.T, first string, script ...string) *family {
	t.Helper()
	f := &family{t: t, script: script, successor: map[string]string{first: ""}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func drop(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func (f *family) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/refresh" {
		http.NotFound(w, r)
		return
	}
	var req wire.RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p := presentation{token: string(req.RefreshToken)}
	if req.ProposedRefreshToken != nil {
		p.proposal = string(*req.ProposedRefreshToken)
	}
	if f.onRefresh != nil {
		f.onRefresh(p)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	step := answer
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	if step == loseRequest {
		drop(w)
		return
	}
	f.seen = append(f.seen, p)
	write := func(status int, body map[string]string) {
		f.statuses = append(f.statuses, status)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch step {
	case proxy401:
		f.statuses = append(f.statuses, http.StatusUnauthorized)
		http.Error(w, "access: session expired", http.StatusUnauthorized)
		return
	case bare503:
		write(http.StatusServiceUnavailable, map[string]string{"error": "upstream unavailable"})
		return
	case sayPresent:
		write(http.StatusServiceUnavailable, map[string]string{"retry": "present_proposal"})
		return
	case sayFresh:
		w.Header().Set("Retry-After", "0")
		write(http.StatusServiceUnavailable, map[string]string{"retry": "fresh_proposal"})
		return
	}

	next, known := f.successor[p.token]
	_, collides := f.successor[p.proposal]
	switch {
	case !known || f.revoked:
		write(http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
		return
	case next != "":
		// A SPENT TOKEN, AND NO GRACE: a replay, and the family is gone.
		f.replays++
		f.revoked = true
		write(http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
		return
	case collides && !f.ignoreProposals:
		w.Header().Set("Retry-After", "0")
		write(http.StatusServiceUnavailable, map[string]string{"retry": "fresh_proposal"})
		return
	}
	f.minted++
	issued := p.proposal
	if f.ignoreProposals || issued == "" {
		issued = padToken("minted-by-the-server-" + string(rune('0'+f.minted)))
	}
	f.successor[p.token], f.successor[issued] = issued, ""
	if step == loseResponse {
		f.statuses = append(f.statuses, http.StatusOK)
		drop(w)
		return
	}
	now := time.Now()
	write(http.StatusOK, map[string]string{
		"access_token":       padToken("access-" + string(rune('0'+f.minted))),
		"access_expires_at":  now.Add(15 * time.Minute).UTC().Format(wireTimestampLayout),
		"refresh_token":      issued,
		"refresh_expires_at": now.Add(24 * time.Hour).UTC().Format(wireTimestampLayout),
	})
}

// live is the one token the family would rotate.
func (f *family) live() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for tok, next := range f.successor {
		if next == "" {
			return tok
		}
	}
	return ""
}

func (f *family) record() (seen []presentation, statuses []int, replays int, revoked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]presentation(nil), f.seen...), append([]int(nil), f.statuses...), f.replays, f.revoked
}

var firstRefresh = padToken("refresh-0")

// dueClient is a client over a pair well inside the floor, so every
// RefreshIfDue refreshes until one is answered.
func dueClient(t *testing.T, f *family, j *Journal, faults Faults) *Client {
	t.Helper()
	if j == nil {
		j = NewJournal()
		if err := j.Enroll(Credential{
			DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken("access-0"), AccessExpiresAt: time.Now().Add(10 * time.Second),
			RefreshToken: firstRefresh, RefreshExpiresAt: time.Now().Add(24 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := New(Config{BaseURL: f.srv.URL, Journal: j, Refresh: true, Faults: faults})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// refreshUntilAnswered calls RefreshIfDue until one succeeds, and returns how
// many attempts settled nothing first. A terminal is a test failure.
func refreshUntilAnswered(t *testing.T, c *Client, atMost int) (unknown int) {
	t.Helper()
	for range atMost {
		err := c.RefreshIfDue(context.Background())
		if err == nil {
			return unknown
		}
		var term *TerminalError
		if errors.As(err, &term) {
			t.Fatalf("attempt %d: the client went terminal: %v", unknown+1, err)
		}
		unknown++
	}
	t.Fatalf("no refresh was answered in %d attempts", atMost)
	return unknown
}

// 32 CSPRNG BYTES IN THE Token SHAPE (criterion 29), checked by the generated
// decoder — the same rule the server holds a proposal to — and never the same
// twice. A generator that fails sends nothing and writes nothing.
func TestAProposalIsAWireToken(t *testing.T) {
	c := dueClient(t, newFamily(t, firstRefresh), nil, Faults{})
	seen := map[string]bool{}
	for range 64 {
		p, err := c.mintProposal()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 43 {
			t.Fatalf("proposal %q is %d characters, want the wire's 43", p, len(p))
		}
		var req wire.RefreshRequest
		body := `{"refresh_token":"` + firstRefresh + `","proposed_refresh_token":"` + p + `"}`
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("the generated decoder refuses proposal %q: %v", p, err)
		}
		if seen[p] {
			t.Fatalf("proposal %q was minted twice", p)
		}
		seen[p] = true
	}

	f := newFamily(t, firstRefresh)
	broken := dueClient(t, f, nil, Faults{})
	broken.cfg.Rand = io.LimitReader(strings.NewReader("too short"), 9)
	if err := broken.RefreshIfDue(context.Background()); err == nil {
		t.Fatal("a refresh went ahead with a generator that could not fill 32 bytes")
	}
	if seen, _, _, _ := f.record(); len(seen) != 0 || len(broken.j.Chain()) != 0 {
		t.Errorf("a failed mint still sent %v and persisted %v", seen, broken.j.Chain())
	}
}

// PERSISTED BEFORE SENDING (criterion 29). When the request ARRIVES, the
// journal already holds the token it presents and the proposal it carries.
func TestTheChainIsPersistedBeforeTheRequestLeaves(t *testing.T) {
	f := newFamily(t, firstRefresh)
	c := dueClient(t, f, nil, Faults{})
	var (
		mu        sync.Mutex
		atArrival []ChainLink
	)
	f.onRefresh = func(presentation) {
		mu.Lock()
		atArrival = c.j.Chain()
		mu.Unlock()
	}
	if err := c.RefreshIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	seen, _, _, _ := f.record()
	if len(seen) != 1 || seen[0].proposal == "" {
		t.Fatalf("the server saw %v, want one refresh carrying a proposal", seen)
	}
	want := []ChainLink{{Token: firstRefresh, Proposal: seen[0].proposal}}
	if len(atArrival) != 1 || atArrival[0] != want[0] {
		t.Errorf("when the request arrived the journal held %v, want %v", atArrival, want)
	}
	// And an answer collapses it.
	if got := c.j.Chain(); len(got) != 0 {
		t.Errorf("after an answered refresh the chain is %v, want empty", got)
	}
	if cr, _ := c.j.Credential(); cr.RefreshToken != seen[0].proposal {
		t.Errorf("the journal holds refresh token %q, want the proposal the server adopted", cr.RefreshToken)
	}
}

// THE RESPONSE'S refresh_token IS AUTHORITATIVE (criterion 28). A server that
// predates the field ignores the proposal and mints its own; the client holds
// what it was GIVEN, and the next refresh presents that.
func TestAServerThatIgnoresTheProposalIsBelieved(t *testing.T) {
	f := newFamily(t, firstRefresh)
	f.ignoreProposals = true
	c := dueClient(t, f, nil, Faults{})
	if err := c.RefreshIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen, _, _, _ := f.record()
	cr, _ := c.j.Credential()
	if cr.RefreshToken == seen[0].proposal {
		t.Fatal("the client kept its own proposal; the server never stored it")
	}
	if cr.RefreshToken != f.live() {
		t.Errorf("the client holds %q, the server's live token is %q", cr.RefreshToken, f.live())
	}
	if got := c.j.Chain(); len(got) != 0 {
		t.Errorf("the chain is %v after an answer, want empty", got)
	}
}

// THE WALK, case by case. Each script is what happens to successive requests;
// the client is asked to refresh until one is answered.
func TestAnUnknownOutcomeIsRecoveredByWalkingTheChain(t *testing.T) {
	for _, tc := range []struct {
		name          string
		script        []string
		wantUnknown   int
		wantWalkBacks int
		want401s      int
		// check sees every presentation that ARRIVED, in order.
		check func(t *testing.T, seen []presentation)
	}{
		{
			name:   "one lost response (criterion 21): the newest token is the live one, and there is no 401",
			script: []string{loseResponse}, wantUnknown: 1,
			check: func(t *testing.T, seen []presentation) {
				if len(seen) != 2 || seen[1].token != seen[0].proposal {
					t.Errorf("presentations %v: want the lost rotation's proposal presented next", seen)
				}
			},
		},
		{
			name:   "two lost responses, the second during recovery (criterion 22): still no 401",
			script: []string{loseResponse, loseResponse}, wantUnknown: 2,
			check: func(t *testing.T, seen []presentation) {
				if len(seen) != 3 || seen[1].token != seen[0].proposal || seen[2].token != seen[1].proposal {
					t.Errorf("presentations %v: want each attempt to present the one before's proposal", seen)
				}
			},
		},
		{
			name:   "the first REQUEST never arrived: the newest token does not exist, the 401 is not terminal, and the walk steps back",
			script: []string{loseRequest}, wantUnknown: 1, wantWalkBacks: 1, want401s: 1,
			check: func(t *testing.T, seen []presentation) {
				// seen[0] is the never-minted proposal being refused; seen[1]
				// is refresh-0 again, WITH THE PROPOSAL IT WAS FIRST SENT WITH
				// — which is the token seen[0] presented.
				if len(seen) != 2 || seen[1].token != firstRefresh || seen[1].proposal != seen[0].token {
					t.Errorf("presentations %v: want refresh-0 re-presented with its original proposal", seen)
				}
			},
		},
		{
			name:   "a response lost, then a request lost during recovery (criterion 22): one step back, original proposal reused",
			script: []string{loseResponse, loseRequest}, wantUnknown: 2, wantWalkBacks: 1, want401s: 1,
			check: func(t *testing.T, seen []presentation) {
				// (r0,P0) lost after commit · (P0,P1) never arrived ·
				// (P1,P2) refused · (P0,P1) again.
				if len(seen) != 3 || seen[2].token != seen[0].proposal || seen[2].proposal != seen[1].token {
					t.Errorf("presentations %v: want the live token re-presented with the proposal it was first sent with", seen)
				}
			},
		},
		{
			name:   "a 503 that names no `retry` is an unknown outcome, not an answer",
			script: []string{bare503}, wantUnknown: 1, wantWalkBacks: 1, want401s: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFamily(t, firstRefresh, tc.script...)
			c := dueClient(t, f, nil, Faults{})
			if got := refreshUntilAnswered(t, c, 6); got != tc.wantUnknown {
				t.Errorf("%d attempts settled nothing, want %d", got, tc.wantUnknown)
			}
			seen, statuses, replays, revoked := f.record()
			if replays != 0 || revoked {
				t.Errorf("a spent token was presented %d times and the family is revoked=%v; presentations %v", replays, revoked, seen)
			}
			var refused int
			for _, s := range statuses {
				if s == http.StatusUnauthorized {
					refused++
				}
			}
			if refused != tc.want401s {
				t.Errorf("%d 401s (statuses %v), want %d", refused, statuses, tc.want401s)
			}
			s := c.Status()
			if s.RefreshWalkBacks != tc.wantWalkBacks || s.Refreshes != 1 || s.Terminal.Kind != NotTerminal {
				t.Errorf("walk-backs %d, refreshes %d, terminal %v; want %d, 1, none", s.RefreshWalkBacks, s.Refreshes, s.Terminal.Kind, tc.wantWalkBacks)
			}
			if cr, _ := c.j.Credential(); cr.RefreshToken != f.live() {
				t.Errorf("the client holds %q and the server's live token is %q", cr.RefreshToken, f.live())
			}
			if got := c.j.Chain(); len(got) != 0 {
				t.Errorf("the chain is %v after an answer, want empty", got)
			}
			if tc.check != nil {
				tc.check(t, seen)
			}
		})
	}
}

// NEGATIVE CONTROL. The same single lost response against a client that mints
// a proposal and forgets it: its next refresh presents refresh-0 again, which
// is spent. The server calls it a replay and the client goes terminal — a
// device lost to one dropped packet, which is the hazard the chain closes.
func TestWithoutTheChainOneLostResponseCostsTheDevice(t *testing.T) {
	f := newFamily(t, firstRefresh, loseResponse)
	c := dueClient(t, f, nil, Faults{NoChain: true})
	if err := c.RefreshIfDue(context.Background()); err == nil {
		t.Fatal("the lost response was not noticed")
	}
	err := c.RefreshIfDue(context.Background())
	var term *TerminalError
	if !errors.As(err, &term) || term.Terminal.Kind != TerminalCredential {
		t.Fatalf("second refresh = %v, want a credential terminal", err)
	}
	if _, _, replays, revoked := f.record(); replays != 1 || !revoked {
		t.Errorf("replays %d, revoked %v; want the spent token presented once and the family gone", replays, revoked)
	}
}

// ONLY THE OLDEST TOKEN'S 401 IS TERMINAL, and it is. A revoked device is
// refused at every link; the walk reaches the token the server last confirmed,
// and that refusal — Catenary's own, for the credential still stored — stops
// the client. Nothing is deleted (record §6).
func TestARevokedDeviceWalksToTheOldestTokenAndStops(t *testing.T) {
	f := newFamily(t, firstRefresh, loseResponse, loseResponse)
	c := dueClient(t, f, nil, Faults{})
	for range 2 {
		if err := c.RefreshIfDue(context.Background()); err == nil {
			t.Fatal("a lost response was not noticed")
		}
	}
	f.mu.Lock()
	f.revoked = true
	f.mu.Unlock()

	err := c.RefreshIfDue(context.Background())
	var term *TerminalError
	if !errors.As(err, &term) || term.Terminal.Kind != TerminalCredential {
		t.Fatalf("refresh = %v, want a credential terminal", err)
	}
	// Three links: the newest proposal, then one step back per 401, down to
	// refresh-0 — two walk-backs, and the third 401 is the terminal one.
	if s := c.Status(); s.RefreshWalkBacks != 2 {
		t.Errorf("%d walk-backs, want 2", s.RefreshWalkBacks)
	}
	seen, _, _, _ := f.record()
	if last := seen[len(seen)-1]; last.token != firstRefresh {
		t.Errorf("the terminal 401 was for %q, want the oldest token", last.token)
	}
	if cr, held := c.j.Credential(); !held || cr.RefreshToken != firstRefresh {
		t.Errorf("terminal changed the stored credential: %+v", cr)
	}
	if got := c.j.Chain(); len(got) != 3 {
		t.Errorf("terminal left a chain of %d links, want all 3 kept", len(got))
	}
}

// A 401 THAT IS NOT CATENARY'S NEVER STEPS THE WALK. It says nothing about any
// token, so stepping back on it would present a token that may well be spent —
// on the word of a proxy.
func TestAProxys401DoesNotStepTheWalkBack(t *testing.T) {
	f := newFamily(t, firstRefresh, loseResponse, proxy401)
	c := dueClient(t, f, nil, Faults{})
	for attempt := range 2 {
		if err := c.RefreshIfDue(context.Background()); err == nil {
			t.Fatalf("attempt %d settled something", attempt+1)
		}
	}
	// The second attempt met the proxy. It sent ONE request and stopped:
	// refresh-0, which is spent, was not reached for.
	seen, _, replays, revoked := f.record()
	if s := c.Status(); len(seen) != 2 || s.RefreshWalkBacks != 0 || s.Terminal.Kind != NotTerminal {
		t.Fatalf("after the proxy's 401: presentations %v, walk-backs %d, terminal %v; want one request per attempt and no step", seen, s.RefreshWalkBacks, s.Terminal.Kind)
	}
	if replays != 0 || revoked {
		t.Fatalf("replays %d, revoked %v", replays, revoked)
	}
	// And the device is still recoverable from there.
	refreshUntilAnswered(t, c, 2)
	if _, _, replays, revoked := f.record(); replays != 0 || revoked {
		t.Errorf("recovering afterwards: replays %d, revoked %v", replays, revoked)
	}
	if cr, _ := c.j.Credential(); cr.RefreshToken != f.live() {
		t.Errorf("the client holds %q and the server's live token is %q", cr.RefreshToken, f.live())
	}
}

// 503 `present_proposal` (record §5): the rotation already committed. The
// client STOPS presenting that token and presents its proposal — it never
// sends the same request again.
func TestPresentProposalMovesTheWalkForward(t *testing.T) {
	f := newFamily(t, firstRefresh, sayPresent)
	c := dueClient(t, f, nil, Faults{})
	// What the stub is pretending: refresh-0 was already rotated into the
	// proposal it is about to be shown. Make that true as the request arrives.
	var once sync.Once
	f.onRefresh = func(p presentation) {
		once.Do(func() {
			f.mu.Lock()
			f.successor[p.token], f.successor[p.proposal] = p.proposal, ""
			f.mu.Unlock()
		})
	}
	if err := c.RefreshIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen, _, replays, _ := f.record()
	if len(seen) != 2 || seen[1].token != seen[0].proposal {
		t.Fatalf("presentations %v: want the proposal presented straight after the 503", seen)
	}
	if replays != 0 {
		t.Errorf("the spent token was presented again %d times", replays)
	}
}

// 503 `fresh_proposal` (criterion 23's client half): the token is still good
// and its proposal collides with a stored token — here because the device's
// generator repeats itself. Same token, a NEW proposal, and the link is
// rewritten rather than extended. It is the one exception to reusing the
// original, and it is bounded.
func TestFreshProposalMintsAgainForTheSameToken(t *testing.T) {
	t.Run("a generator that repeats itself once", func(t *testing.T) {
		f := newFamily(t, firstRefresh)
		c := dueClient(t, f, nil, Faults{})
		// 32 bytes that decode to refresh-0 itself would be refused before
		// the lookup; collide with a DIFFERENT stored token instead.
		other := strings.Repeat("A", 43)
		f.successor[other] = "some-later-token"
		c.cfg.Rand = io.MultiReader(strings.NewReader(strings.Repeat("\x00", 32)), rand.Reader)
		if err := c.RefreshIfDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		seen, statuses, replays, _ := f.record()
		if len(seen) != 2 || seen[0].proposal != other || seen[1].token != firstRefresh || seen[1].proposal == other {
			t.Fatalf("presentations %v (statuses %v): want refresh-0 twice, the second with a new proposal", seen, statuses)
		}
		if replays != 0 || c.Status().Terminal.Kind != NotTerminal {
			t.Errorf("replays %d, terminal %v", replays, c.Status().Terminal.Kind)
		}
	})
	t.Run("a server that says it to everything is given up on, and not terminally", func(t *testing.T) {
		f := newFamily(t, firstRefresh, sayFresh, sayFresh, sayFresh, sayFresh, sayFresh, sayFresh)
		c := dueClient(t, f, nil, Faults{})
		err := c.RefreshIfDue(context.Background())
		var term *TerminalError
		if err == nil || errors.As(err, &term) {
			t.Fatalf("refresh = %v, want a plain failure", err)
		}
		if seen, _, _, _ := f.record(); len(seen) != maxFreshProposals+1 {
			t.Errorf("%d requests, want the first and %d more", len(seen), maxFreshProposals)
		}
		// One link, rewritten each time — never a chain of dead proposals.
		if got := c.j.Chain(); len(got) != 1 || got[0].Token != firstRefresh {
			t.Errorf("the chain is %v, want the one link for refresh-0", got)
		}
	})
}

// KILLED BETWEEN PERSISTING AND THE RESPONSE (criterion 29). The proposal is in
// the journal, the process is gone, and whether the rotation committed is
// something nobody on the device ever learned. A new Client over the same
// journal recovers either way.
func TestAClientKilledMidRefreshRecoversOnRestart(t *testing.T) {
	for _, tc := range []struct {
		name, fate    string
		wantWalkBacks int
	}{
		{"the rotation committed", loseResponse, 0},
		{"the request never arrived", loseRequest, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFamily(t, firstRefresh, tc.fate)
			first := dueClient(t, f, nil, Faults{})
			// Kill it while the request is in the server's hands.
			var once sync.Once
			f.onRefresh = func(presentation) { once.Do(first.Kill) }
			if err := first.RefreshIfDue(context.Background()); err == nil {
				t.Fatal("the killed client's refresh reported success")
			}
			j := first.Journal()
			if got := j.Chain(); len(got) != 1 {
				t.Fatalf("the killed client left a chain of %v, want its one proposal", got)
			}

			second := dueClient(t, f, j, Faults{})
			if err := second.RefreshIfDue(context.Background()); err != nil {
				t.Fatalf("the restarted client: %v", err)
			}
			_, _, replays, revoked := f.record()
			if replays != 0 || revoked {
				t.Errorf("replays %d, revoked %v", replays, revoked)
			}
			if s := second.Status(); s.RefreshWalkBacks != tc.wantWalkBacks {
				t.Errorf("%d walk-backs, want %d", s.RefreshWalkBacks, tc.wantWalkBacks)
			}
			if cr, _ := j.Credential(); cr.RefreshToken != f.live() {
				t.Errorf("the journal holds %q and the server's live token is %q", cr.RefreshToken, f.live())
			}
		})
	}
}

// A KILLED CLIENT PROPOSES NOTHING, as it rotates nothing: the Kill guard
// covers the chain like every other journal write.
func TestAKilledClientWritesNoProposal(t *testing.T) {
	f := newFamily(t, firstRefresh)
	c := dueClient(t, f, nil, Faults{})
	c.Kill()
	if err := c.RefreshIfDue(context.Background()); !errors.Is(err, ErrKilled) {
		t.Fatalf("refresh after Kill = %v, want ErrKilled", err)
	}
	if seen, _, _, _ := f.record(); len(seen) != 0 || len(c.j.Chain()) != 0 {
		t.Errorf("a killed client sent %v and persisted %v", seen, c.j.Chain())
	}
}

// THE CHAIN BELONGS TO THE CREDENTIAL IT EXTENDS. A rotation or a
// re-enrollment through any door clears it, and a refresher whose token is no
// longer the chain's newest is told the chain moved rather than given a link.
func TestTheChainFollowsTheCredential(t *testing.T) {
	mint := func() (string, error) { return padToken("p-" + uuid.NewString()[:8]), nil }
	j := enrolledJournal(t, "access-0")
	held, _ := j.Credential()

	p0, ok, err := j.propose(held.RefreshToken, false, mint)
	if err != nil || !ok {
		t.Fatalf("propose: %v, ok=%v", err, ok)
	}
	if again, _, _ := j.propose(held.RefreshToken, false, mint); again != p0 {
		t.Errorf("presented again, the token got proposal %q, want its original %q", again, p0)
	}
	p1, _, _ := j.propose(p0, false, mint)
	if _, ok, _ := j.propose("some-other-token", false, mint); ok {
		t.Error("a token that is neither in the chain nor its newest was given a link")
	}
	if older, ok := j.older(p0); !ok || older != held.RefreshToken {
		t.Errorf("older(%q) = %q, %v", p0, older, ok)
	}
	if _, ok := j.older(held.RefreshToken); ok {
		t.Error("the oldest token has something older")
	}

	// fresh_proposal: the link is rewritten and everything newer goes.
	p0b, _, _ := j.propose(held.RefreshToken, true, mint)
	if got := j.Chain(); len(got) != 1 || got[0].Proposal != p0b || p0b == p0 {
		t.Errorf("after a replaced proposal the chain is %v (was %q then %q)", got, p0, p1)
	}

	next := held
	next.AccessToken, next.RefreshToken = "access-1", "refresh-1"
	if err := j.Rotate(next); err != nil {
		t.Fatal(err)
	}
	if got := j.Chain(); len(got) != 0 {
		t.Errorf("a rotation left the chain %v", got)
	}
	if _, ok, _ := j.propose(held.RefreshToken, false, mint); ok {
		t.Error("the rotated-away token was given a link; the chain moved and the caller should be told")
	}
	if _, _, err := j.propose("refresh-1", false, mint); err != nil {
		t.Fatal(err)
	}
	if err := j.Reenroll(Credential{DeviceID: wire.Uuid(uuid.NewString()), AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if got := j.Chain(); len(got) != 0 {
		t.Errorf("a re-enrollment left the old device's chain %v", got)
	}
}
