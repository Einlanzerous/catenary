package client

// CANT-124 against a stub that mints pairs and records the order it was asked
// for things. The same rules against the real router and a real database —
// REST on a live socket, the reactive round trip, single-flight across
// clients — are cmd/catenary/refreshclient_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestTheThresholdIsAThirdOfTheServedLifetimeWithAFloor(t *testing.T) {
	issued := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		lifetime time.Duration
		want     time.Duration
	}{
		{"a person's fifteen minutes", 15 * time.Minute, 5 * time.Minute},
		{"exactly at the floor", 3 * time.Minute, time.Minute},
		{"a short lifetime is held up by the floor", 90 * time.Second, time.Minute},
		{"an hour", time.Hour, 20 * time.Minute},
	} {
		cr := Credential{AccessIssuedAt: issued, AccessExpiresAt: issued.Add(tc.lifetime)}
		if got := refreshThreshold(cr); got != tc.want {
			t.Errorf("%s: threshold %s, want %s", tc.name, got, tc.want)
		}
	}
	// No issue time was ever learned: the lifetime is unknown, and the floor
	// is the only number that does not have to guess it.
	if got := refreshThreshold(Credential{AccessExpiresAt: issued.Add(15 * time.Minute)}); got != refreshFloor {
		t.Errorf("an unknown lifetime: threshold %s, want the floor", got)
	}
}

// The due check reads the device's wall clock THROUGH the offset persisted
// with the credential. A device an hour fast would otherwise refresh on every
// dial for ever, and one an hour slow would never refresh at all.
func TestDueIsDecidedOnTheServersClock(t *testing.T) {
	server := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	pair := func(deviceAtIssue time.Time) Credential {
		return Credential{AccessExpiresAt: server.Add(15 * time.Minute)}.WithServerDate(server, deviceAtIssue)
	}
	for _, tc := range []struct {
		name    string
		skew    time.Duration // device minus server, constant
		elapsed time.Duration // real time since issue
		want    bool
	}{
		{"a correct clock, fresh", 0, time.Minute, false},
		{"a correct clock, inside the last third", 0, 10*time.Minute + time.Second, true},
		{"an hour fast, fresh — not due, though the raw wall clock says long expired", time.Hour, time.Minute, false},
		{"an hour fast, inside the last third", time.Hour, 11 * time.Minute, true},
		{"an hour slow, inside the last third — due, though the raw wall clock says an hour left", -time.Hour, 11 * time.Minute, true},
		{"an hour slow, fresh", -time.Hour, time.Minute, false},
	} {
		cr := pair(server.Add(tc.skew))
		deviceNow := server.Add(tc.skew).Add(tc.elapsed)
		if got := refreshDue(cr, deviceNow); got != tc.want {
			t.Errorf("%s: due = %v, want %v (offset %s)", tc.name, got, tc.want, cr.ClockOffset)
		}
	}
	if refreshDue(Credential{}, server) {
		t.Error("a credential with no expiry was called due; there is nothing to measure against")
	}
}

// THE ANCHOR COUNTS A SUSPEND. Go's monotonic reading stops while the machine
// sleeps and Sub prefers it when both operands have one, so the clock the due
// check reads must not carry one — even when the configured clock does.
func TestTheClientsClockCarriesNoMonotonicReading(t *testing.T) {
	for name, now := range map[string]func() time.Time{"the default clock": nil, "a configured time.Now": time.Now} {
		c, err := New(Config{BaseURL: "http://example.invalid", Journal: enrolledJournal(t, "a"), Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.wallNow(); strings.Contains(got.String(), "m=") {
			t.Errorf("%s: wallNow() = %s carries a monotonic reading", name, got)
		}
	}
	// And WithServerDate strips it from what it is handed, so an offset can
	// never be computed across a suspend on the clock that paused for it.
	cr := Credential{}.WithServerDate(time.Now(), time.Now())
	if strings.Contains(cr.AccessIssuedAt.String(), "m=") {
		t.Errorf("AccessIssuedAt %s carries a monotonic reading", cr.AccessIssuedAt)
	}
}

// --- a stub that mints pairs ----------------------------------------------------

type minter struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	events    []string       // "refresh", "ws:<token>", "sync:<token>", in arrival order
	presented map[string]int // refresh token → times presented
	valid     map[string]bool
	n         int
	// date, when set, is what /refresh stamps as its Date header. noDate
	// suppresses the header altogether.
	date   time.Time
	noDate bool
	// holdRefresh, when non-nil, is closed by the test to let /refresh answer.
	holdRefresh chan struct{}
	lifetime    time.Duration
	// deadOnArrival mints pairs the stub itself will refuse.
	deadOnArrival bool
}

func newMinter(t *testing.T, firstAccess string) *minter {
	t.Helper()
	m := &minter{t: t, presented: map[string]int{}, valid: map[string]bool{firstAccess: true}, lifetime: 15 * time.Minute}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *minter) log(e string) {
	m.mu.Lock()
	m.events = append(m.events, e)
	m.mu.Unlock()
}

func (m *minter) expire(access string) {
	m.mu.Lock()
	delete(m.valid, access)
	m.mu.Unlock()
}

func (m *minter) ok(access string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.valid[access]
}

func (m *minter) snapshot() (events []string, presented map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	presented = map[string]int{}
	for k, v := range m.presented {
		presented[k] = v
	}
	return append([]string(nil), m.events...), presented
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"code":"unauthorized"}`))
}

func (m *minter) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/refresh":
		var req wire.RefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.events = append(m.events, "refresh")
		m.presented[unpad(string(req.RefreshToken))]++
		hold := m.holdRefresh
		m.mu.Unlock()
		if hold != nil {
			<-hold
		}
		m.mu.Lock()
		m.n++
		access, refresh := fmt.Sprintf("access-%d", m.n), fmt.Sprintf("refresh-%d", m.n)
		m.valid[access] = !m.deadOnArrival
		date, noDate, lifetime := m.date, m.noDate, m.lifetime
		m.mu.Unlock()
		if date.IsZero() {
			date = time.Now()
		}
		if noDate {
			w.Header()["Date"] = nil
		} else {
			w.Header().Set("Date", date.UTC().Format(http.TimeFormat))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":       padToken(access),
			"access_expires_at":  date.Add(lifetime).UTC().Format(wireTimestampLayout),
			"refresh_token":      padToken(refresh),
			"refresh_expires_at": date.Add(24 * time.Hour).UTC().Format(wireTimestampLayout),
		})
	case "/sync":
		tok := unpad(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		m.log("sync:" + tok)
		if !m.ok(tok) {
			unauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"log_seq":0,"messages":[],"conversations":[],"users":[],"has_more":false,"server_time":"2026-01-01T00:00:00.000Z"}`))
	case "/ws":
		var tok string
		for _, sp := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(sp), tokenSubprotocolPrefix); ok {
				tok = unpad(v)
			}
		}
		m.log("ws:" + tok)
		if !m.ok(tok) {
			unauthorized(w)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(context.Background()); err != nil {
			return
		}
		if err := conn.Write(context.Background(), websocket.MessageText, readyFrame(m.t)); err != nil {
			return
		}
		_, _, _ = conn.Read(context.Background())
	default:
		http.NotFound(w, r)
	}
}

// The wire's Token is exactly 43 base64url characters, and the generated
// decoder enforces it, so the stub's readable names are padded out to it.
func padToken(name string) string { return name + strings.Repeat("_", 43-len(name)) }
func unpad(tok string) string     { return strings.TrimRight(tok, "_") }

func (m *minter) journal(access string, expires time.Time) *Journal {
	m.t.Helper()
	j := NewJournal()
	if err := j.Enroll(Credential{
		DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken(access), AccessExpiresAt: expires,
		RefreshToken: padToken("refresh-0"), RefreshExpiresAt: expires.Add(24 * time.Hour),
	}); err != nil {
		m.t.Fatal(err)
	}
	return j
}

func fastBackoff(cfg Config) Config {
	cfg.BackoffMin, cfg.BackoffMax = 5*time.Millisecond, 20*time.Millisecond
	return cfg
}

// PROACTIVE, AND BEFORE BOTH. A pair inside its last third is rotated before
// the dial, so neither the upgrade nor the /sync beside it ever presents it.
func TestADuePairIsRotatedBeforeTheDialAndTheSyncBesideIt(t *testing.T) {
	m := newMinter(t, "access-0")
	j := m.journal("access-0", time.Now().Add(30*time.Second)) // well inside the floor
	c, err := New(fastBackoff(Config{BaseURL: m.srv.URL, Journal: j, Refresh: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer runUntilCaughtUp(t, c)()

	events, _ := m.snapshot()
	if len(events) == 0 || events[0] != "refresh" {
		t.Fatalf("the first thing the server saw was %v, want the refresh", events)
	}
	for _, e := range events[1:] {
		if strings.HasSuffix(e, ":access-0") {
			t.Errorf("%s: the pair that was due was presented anyway; events %v", e, events)
		}
	}
	if s := c.Status(); s.Refreshes != 1 || s.DialErrors != 0 || s.SyncErrors != 0 {
		t.Errorf("stats %+v, want one refresh and no errors", s.Stats)
	}
}

// NOT DUE, NOT REFRESHED — and with Refresh off, never, which is what the rigs
// rely on (CANT-31 criterion 39).
func TestNoRefreshWhenNotDueOrNotEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expires time.Duration
		enabled bool
	}{
		{"enabled, with most of its life left", 14 * time.Minute, true},
		{"due, but Refresh is off", 10 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMinter(t, "access-0")
			c, err := New(fastBackoff(Config{BaseURL: m.srv.URL, Journal: m.journal("access-0", time.Now().Add(tc.expires)), Refresh: tc.enabled}))
			if err != nil {
				t.Fatal(err)
			}
			defer runUntilCaughtUp(t, c)()
			if _, presented := m.snapshot(); len(presented) != 0 {
				t.Errorf("the server saw a refresh: %v", presented)
			}
		})
	}
}

// A COLD START USES THE PERSISTED OFFSET. The device's clock is an hour fast,
// which the pair's own offset records; a new Client over that journal reads
// its expiry through it and does not refresh a pair with most of its life
// left. Without the offset the same clock says the pair died 46 minutes ago.
func TestAColdStartReadsItsExpiryThroughThePersistedOffset(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keepOffset  bool
		wantRefresh int
	}{
		{"with the offset: not due", true, 0},
		{"negative control — offset discarded: the fast clock forces a refresh", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMinter(t, "access-0")
			server := time.Now()
			fast := func() time.Time { return time.Now().Add(time.Hour) }

			cr := Credential{
				DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken("access-0"), RefreshToken: padToken("refresh-0"),
				AccessExpiresAt: server.Add(14 * time.Minute),
			}.WithServerDate(server.Add(-time.Minute), fast().Add(-time.Minute))
			if !tc.keepOffset {
				cr.ClockOffset = 0
			}
			j := NewJournal()
			if err := j.Enroll(cr); err != nil {
				t.Fatal(err)
			}
			// the stub stamps the real time, so a refresh here re-learns the offset
			c, err := New(fastBackoff(Config{BaseURL: m.srv.URL, Journal: j, Refresh: true, Now: fast}))
			if err != nil {
				t.Fatal(err)
			}
			defer runUntilCaughtUp(t, c)()
			if got := c.Status().Refreshes; got != tc.wantRefresh {
				t.Errorf("%d refreshes, want %d", got, tc.wantRefresh)
			}
		})
	}
}

// REACTIVE: ONE REFRESH, ONE RETRY, NO ERROR SURFACED. The device's clock was
// stepped back an hour, so the proactive check is confidently wrong and the
// dead pair goes out. Catenary's own 401 on /sync is answered by exactly one
// refresh and one retry.
func TestAStaleSyncIsRecoveredInOneRoundTrip(t *testing.T) {
	m := newMinter(t, "access-0")
	j := m.journal("access-0", time.Now().Add(14*time.Minute))
	m.expire("access-0") // the server's view: dead already
	slow := func() time.Time { return time.Now().Add(-time.Hour) }
	c, err := New(fastBackoff(Config{BaseURL: m.srv.URL, Journal: j, Refresh: true, Now: slow}))
	if err != nil {
		t.Fatal(err)
	}
	defer runUntilCaughtUp(t, c)()

	s := c.Status()
	if s.Refreshes != 1 || s.SyncErrors != 0 {
		t.Errorf("refreshes %d, sync errors %d; want exactly one refresh and the 401 never surfaced as a failed catch-up", s.Refreshes, s.SyncErrors)
	}
	events, presented := m.snapshot()
	if presented["refresh-0"] != 1 || len(presented) != 1 {
		t.Errorf("refresh tokens presented %v, want refresh-0 exactly once", presented)
	}
	var syncs []string
	for _, e := range events {
		if strings.HasPrefix(e, "sync:") {
			syncs = append(syncs, e)
		}
	}
	if len(syncs) < 2 || syncs[0] != "sync:access-0" || syncs[1] != "sync:access-1" {
		t.Errorf("/sync saw %v, want the dead pair once and then the rotated one", syncs)
	}
	// The stub stamped the real time while the device read an hour slow, so
	// the rotation re-learned the offset: about plus one hour.
	if got, _ := j.Credential(); got.ClockOffset < 59*time.Minute || got.ClockOffset > 61*time.Minute {
		t.Errorf("the rotated pair's offset is %s, want about +1h", got.ClockOffset)
	}
}

// ONCE, NOT A LOOP. A pair that is refused again straight after being minted
// is not stale, and a second refresh would not cure it.
func TestASecond401IsNotAnsweredWithASecondRefresh(t *testing.T) {
	m := newMinter(t, "access-0")
	j := m.journal("access-0", time.Now().Add(14*time.Minute))
	m.expire("access-0")
	m.deadOnArrival = true
	c, err := New(Config{BaseURL: m.srv.URL, Journal: j, Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.fetch(context.Background(), 0); !errors.Is(err, errUnauthorized) {
		t.Fatalf("fetch = %v, want the second 401 handed back as the failure it is", err)
	}
	if s := c.Status(); s.Refreshes != 1 {
		t.Errorf("%d refreshes for one fetch, want exactly 1", s.Refreshes)
	}
}

// A 401 that is not Catenary's says nothing about the Catenary credential.
func TestAProxys401DoesNotTriggerARefresh(t *testing.T) {
	var refreshes int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			mu.Lock()
			refreshes++
			mu.Unlock()
		}
		http.Error(w, "cloudflare access: session expired", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Journal: enrolledJournal(t, "access-0"), Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.fetch(context.Background(), 0); err == nil {
		t.Fatal("a 401 came back as a page")
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshes != 0 {
		t.Errorf("a proxy's 401 caused %d refreshes; the Catenary pair was never judged", refreshes)
	}
}

// A response with no Date keeps the offset the device already had.
func TestARefreshWithNoDateKeepsTheOffset(t *testing.T) {
	m := newMinter(t, "access-0")
	m.noDate = true
	j := NewJournal()
	cr := Credential{
		DeviceID: wire.Uuid(uuid.NewString()), AccessToken: padToken("access-0"), RefreshToken: padToken("refresh-0"),
		AccessExpiresAt: time.Now().Add(10 * time.Second), ClockOffset: 7 * time.Minute,
	}
	cr.AccessExpiresAt = cr.AccessExpiresAt.Add(7 * time.Minute) // due, on the corrected clock
	if err := j.Enroll(cr); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{BaseURL: m.srv.URL, Journal: j, Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := j.Credential()
	if unpad(got.AccessToken) == "access-0" {
		t.Fatal("nothing was rotated")
	}
	if got.ClockOffset != 7*time.Minute {
		t.Errorf("offset %s after a Date-less refresh, want the 7m it had", got.ClockOffset)
	}
}

// SINGLE-FLIGHT IS PER CREDENTIAL, NOT PER PROCESS. Eight independent Clients
// over one Journal — eight tabs over one origin's storage — all find the pair
// due at the same moment. One refreshes; seven wait, re-read, and skip.
//
// The negative control runs the same eight with the lock faulted out, and the
// server is shown the same refresh token more than once — which outside the
// grace window is a replay, and a replay revokes the device.
func TestEightClientsOverOneCredentialRefreshItOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unlocked bool
	}{
		{"single-flight", false},
		{"negative control — the lock faulted out", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const n = 8
			m := newMinter(t, "access-0")
			m.holdRefresh = make(chan struct{})
			j := m.journal("access-0", time.Now().Add(10*time.Second))

			clients := make([]*Client, n)
			for i := range clients {
				c, err := New(Config{BaseURL: m.srv.URL, Journal: j, Refresh: true, Faults: Faults{RefreshUnlocked: tc.unlocked}})
				if err != nil {
					t.Fatal(err)
				}
				clients[i] = c
			}
			var wg sync.WaitGroup
			errs := make([]error, n)
			for i, c := range clients {
				wg.Add(1)
				go func() { defer wg.Done(); errs[i] = c.RefreshIfDue(context.Background()) }()
			}
			// Hold the first refresh open until every client has had time
			// to reach the lock — or, faulted, to reach the server.
			time.Sleep(200 * time.Millisecond)
			close(m.holdRefresh)
			wg.Wait()

			var refreshed, skipped int
			for i, c := range clients {
				if errs[i] != nil {
					t.Errorf("client %d: %v", i, errs[i])
				}
				s := c.Status()
				refreshed, skipped = refreshed+s.Refreshes, skipped+s.RefreshesSkipped
			}
			_, presented := m.snapshot()
			switch {
			case !tc.unlocked:
				if refreshed != 1 || skipped != n-1 {
					t.Errorf("%d refreshed and %d skipped, want 1 and %d", refreshed, skipped, n-1)
				}
				if len(presented) != 1 || presented["refresh-0"] != 1 {
					t.Errorf("refresh tokens presented %v, want refresh-0 exactly once", presented)
				}
			case presented["refresh-0"] < 2:
				t.Errorf("with the lock faulted out refresh-0 was presented %d times; the control shows nothing", presented["refresh-0"])
			}
		})
	}
}
