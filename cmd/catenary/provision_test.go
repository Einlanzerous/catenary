package main

// CANT-131 criteria 9, 10 and 11, through the REAL process: setup()'s own
// composition, serve()'s two listeners on two real sockets, the real store on
// Postgres, the real revocation listener on its own connection, and real
// WebSockets.
//
// WHY THE REAL serve() RATHER THAN TWO httptest SERVERS. Criterion 10 is about
// the SEPARATION — `/accounts` unreachable on the routed listener, the wire
// routes unreachable on the provisioning one, and serve's shutdown joining the
// second — and every one of those is a property of the composition rather than of
// a handler. Two httptest servers would prove it about a wiring this test wrote,
// which is the wiring that cannot be wrong.
//
// THE CREDENTIALS THAT MUST NOT OPEN THE DOOR ARE MINTED FOR REAL, for the same
// reason: a device access token, a refresh token, a bot token and an enrollment
// token, each issued by the store and each presented at the provisioning surface.
// A hand-written string proves that a wrong credential is refused; a real one
// proves that no OTHER authentication path in this service reaches this door.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/hub"
	"github.com/magos/catenary/internal/wire"
)

// testProvisionToken is what these tests configure as CATENARY_PROVISION_TOKEN:
// 32 bytes, config.MinProvisionTokenBytes, and a literal so the assertions that
// search log lines for it are the same every run.
//
// IT READS AS A SENTENCE RATHER THAN AS A CREDENTIAL, on ci.yml's own argument
// about a password-shaped literal that a secret scanner reports forever and
// somebody dismisses every time. internal/config's copy carries the whole reason.
const testProvisionToken = "not-a-secret-just-a-Test-token-1"

// --- the fixture -------------------------------------------------------------

type provisionRig struct {
	*rig

	// provisionBase is the SECOND listener's URL. Nothing routes to it and no
	// label points at it; here it is a port the kernel chose.
	provisionBase string

	served chan error
	cancel context.CancelFunc

	// stopped and stopErr make stop() idempotent — see there.
	stopped bool
	stopErr error
}

// stop cancels serve's context and waits for it to return, ONCE.
//
// IDEMPOTENT ON PURPOSE. The shutdown test calls this itself, because the return
// value is the thing it is asserting, and the fixture's own cleanup calls it for
// every other test. Without the flag the cleanup would wait a second time on a
// channel nothing will ever send to again, and fail ten seconds later with a
// message about a shutdown that had in fact worked — which is what happened the
// first time this was written.
func (p *provisionRig) stop() error {
	p.t.Helper()
	if p.stopped {
		return p.stopErr
	}
	p.stopped = true
	p.cancel()
	select {
	case p.stopErr = <-p.served:
	case <-time.After(p.d.cfg.ShutdownGrace + 5*time.Second):
		p.stopErr = errors.New("serve did not return within the grace")
	}
	return p.stopErr
}

// newProvisionRig boots the real process with the provisioning surface
// configured, on two real sockets, and serves it until the test ends.
func newProvisionRig(t *testing.T) *provisionRig {
	t.Helper()
	log := &recorder{}
	ctx, pool, st, d := processFixtureWith(t, slog.New(log), func(c *config.Config) {
		// The addresses these tests actually listen on are chosen below, by the
		// kernel; what the config has to carry is that the surface is ENABLED, which
		// is the condition setup() gates the handler on.
		c.ProvisionAddr = "127.0.0.1:0"
		c.ProvisionToken = testProvisionToken
	})
	if d.provision == nil {
		t.Fatal("setup() built no provisioning handler for a config that carries both variables — " +
			"CANT-131's surface would be absent from a deployment that had configured it")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	provisionLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Read before serve starts its listener, for the reason
	// TestACancelledServeClosesEverySessionWith1001 gives: a fast connect could
	// otherwise register a backend older than the mark and never be counted.
	since := dbNow(ctx, t, pool)

	serveCtx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- serve(serveCtx, d, ln, provisionLn) }()

	r := &rig{t: t, ctx: ctx, pool: pool, st: st, d: d, log: log,
		base: "http://" + ln.Addr().String(), since: since}
	r.waitForListen()

	p := &provisionRig{rig: r, provisionBase: "http://" + provisionLn.Addr().String(),
		served: served, cancel: cancel}
	t.Cleanup(func() {
		if err := p.stop(); err != nil {
			t.Errorf("serve did not shut down cleanly: %v", err)
		}
	})
	return p
}

// provisionClient is deliberately its own client with a short timeout: a test
// that hangs on a listener that stopped accepting is a test that fails ten
// minutes later with no information.
var provisionClient = &http.Client{Timeout: 10 * time.Second}

// call makes one real request to the provisioning listener. An empty token means
// no Authorization header at all.
func (p *provisionRig) call(method, path, body, token string) (int, []byte) {
	p.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(p.ctx, method, p.provisionBase+path, rdr)
	if err != nil {
		p.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := provisionClient.Do(req)
	if err != nil {
		p.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// ensure posts one ensure with the service credential and decodes the answer.
func (p *provisionRig) ensure(email, displayName string) (int, map[string]any) {
	p.t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "display_name": displayName})
	if err != nil {
		p.t.Fatal(err)
	}
	code, raw := p.call(http.MethodPost, "/accounts", string(body), testProvisionToken)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			p.t.Fatalf("ensure answered %d with a body that is not JSON: %q", code, raw)
		}
	}
	return code, out
}

// accountOf reads the nested account object out of an ensure response.
func accountOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	acct, ok := body["account"].(map[string]any)
	if !ok {
		t.Fatalf("the ensure response carries no account object: %v", body)
	}
	return acct
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("%q is missing or not a string in %v", key, m)
	}
	return v
}

// --- criterion 9: the door fails closed, against REAL credentials -------------

// TestNoOtherCredentialInThisServiceOpensTheProvisioningDoor.
//
// Ruling 2's picked option says nothing in the database can grant this power. That
// is a claim about every credential the database DOES hold, and this is the list:
// a device's access token, its refresh token, an unredeemed enrollment token, and a
// bot token — the one credential that exists precisely to let a machine call this
// service.
func TestNoOtherCredentialInThisServiceOpensTheProvisioningDoor(t *testing.T) {
	p := newProvisionRig(t)

	ada := mkUser(p.ctx, t, p.pool, "ada", "Ada Lovelace")
	device := p.enroll(ada, "Pixel 8 Pro")
	pending, err := p.st.IssueEnrollmentToken(p.ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	bot, err := p.st.CreateBot(p.ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	botToken, err := p.st.IssueBotToken(p.ctx, bot)
	if err != nil {
		t.Fatal(err)
	}

	usersBefore := countUsers(p.ctx, t, p.pool)

	var bodies [][]byte
	for _, tc := range []struct{ name, token string }{
		{"no credential at all", ""},
		{"a device's access token", string(device.AccessToken)},
		{"a device's refresh token", string(device.RefreshToken)},
		{"an unredeemed enrollment token", pending.Plaintext},
		{"a bot token", botToken.Plaintext},
		{"a token-shaped string that is nobody's", strings.Repeat("A", 43)},
	} {
		code, body := p.call(http.MethodPost, "/accounts",
			`{"email":"intruder@example.com","display_name":"Intruder"}`, tc.token)
		if code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401: %s", tc.name, code, body)
		}
		bodies = append(bodies, body)
	}

	// BYTE-IDENTICAL, including the one with no header, because telling them apart
	// would tell a caller which of its credentials the service recognises.
	for i := 1; i < len(bodies); i++ {
		if !bytes.Equal(bodies[i], bodies[0]) {
			t.Errorf("two refusals differ:\n  %q\n  %q", bodies[0], bodies[i])
		}
	}
	// And nothing was created. A door that refused after the write would have made
	// the account it then declined to talk about.
	if after := countUsers(p.ctx, t, p.pool); after != usersBefore {
		t.Errorf("a refused provisioning call created %d user row(s)", after-usersBefore)
	}
}

// TestWithNeitherVariableThereIsNoProvisioningSurfaceAtAll is criterion 9's first
// clause, and it is the clause every OTHER deployment of this service depends on:
// with the two variables unset, setup builds no handler, serve is handed no second
// socket, and the process is byte for byte the one that ran before this ticket.
//
// Not "exists and refuses everything". A listener that accepts connections is a
// listener somebody can reach, and the cheapest way for a service not to have its
// most dangerous door attacked is not to open it.
func TestWithNeitherVariableThereIsNoProvisioningSurfaceAtAll(t *testing.T) {
	ctx, _, _, d := processFixture(t, slog.New(&recorder{}))
	if d.cfg.ProvisioningEnabled() {
		t.Fatal("the plain fixture config has provisioning enabled; this test is measuring nothing")
	}
	if d.provision != nil {
		t.Error("setup built a provisioning handler for a config that carries neither variable")
	}

	// And serve runs, and stops, with no second listener — the shape every other
	// test in this package already exercises, asserted here so that the nil path
	// through serve's new branches is covered deliberately rather than incidentally.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- serve(serveCtx, d, ln, nil) }()

	resp, err := provisionClient.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("the routed listener did not come up: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve returned %v with no provisioning listener, want nil", err)
		}
	case <-time.After(d.cfg.ShutdownGrace + 5*time.Second):
		t.Fatal("serve did not return")
	}
}

// TestServeRefusesAHalfWiredProvisioningSurface. config.Load and setup between them
// make this unreachable from an environment, so what is being guarded is a mistake
// in cmd/catenary itself — and both halves of it are silent: a listener with no
// handler accepts connections nothing answers, and a handler with no listener is a
// deployment that believes it has a provisioning surface and does not.
func TestServeRefusesAHalfWiredProvisioningSurface(t *testing.T) {
	ctx, _, _, plain := processFixture(t, slog.New(&recorder{}))
	_, _, _, wired := processFixtureWith(t, slog.New(&recorder{}), func(c *config.Config) {
		c.ProvisionAddr = "127.0.0.1:0"
		c.ProvisionToken = testProvisionToken
	})

	for _, tc := range []struct {
		name         string
		d            deps
		withListener bool
	}{
		{"a handler with no listener", wired, false},
		{"a listener with no handler", plain, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			var provisionLn net.Listener
			if tc.withListener {
				provisionLn, err = net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer provisionLn.Close()
			}
			// A context that is ALREADY cancelled, so a serve that wrongly
			// accepted this pair returns from its drain rather than hanging the
			// test: the assertion is then about the error and not about a timeout.
			dead, cancel := context.WithCancel(ctx)
			cancel()
			err = serve(dead, tc.d, ln, provisionLn)
			if err == nil || !strings.Contains(err.Error(), "half-wired") {
				t.Errorf("serve(%s) = %v, want a refusal naming the half-wired surface", tc.name, err)
			}
		})
	}
}

// TestTheProvisioningCredentialIsNotACredentialAnywhereElse is the same claim
// pointed the other way: the most powerful credential in the service authenticates
// NOTHING on the routed listener. It is not an access token, there is no account
// behind it, and store.Authenticate has never heard of it.
func TestTheProvisioningCredentialIsNotACredentialAnywhereElse(t *testing.T) {
	p := newProvisionRig(t)

	for _, path := range []string{"/sync?after=0", "/devices"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testProvisionToken)
		if code, body := do(t, p.d.router, req); code != http.StatusUnauthorized {
			t.Errorf("GET %s with the provisioning credential = %d, want 401: %s", path, code, body)
		}
	}
	if _, err := p.st.Authenticate(p.ctx, testProvisionToken); err == nil {
		t.Error("store.Authenticate resolved the provisioning credential — it is a static secret " +
			"in the environment and must name no account at all")
	}
}

// --- criterion 10: each listener serves only its own -------------------------

// TestTheRoutedListenerDoesNotServeTheProvisioningRoutes.
//
// WITH the credential and WITHOUT it, because the two would hide different
// mistakes: without it, a 401 from a provisioning door accidentally mounted on the
// routed listener would look like the 404 this asserts if only the status were
// compared; with it, a route that existed would answer 200 and create an account on
// the port Traefik routes.
func TestTheRoutedListenerDoesNotServeTheProvisioningRoutes(t *testing.T) {
	p := newProvisionRig(t)
	ada := mkUser(p.ctx, t, p.pool, "ada", "Ada")

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/accounts"},
		{http.MethodGet, "/accounts?email=ada@example.com"},
		{http.MethodPost, "/accounts/" + ada.String() + "/deactivate"},
		// The shape CANT-69's admin page will use, which this ticket deliberately
		// does not build: ruling 1's alternative was these routes under /admin on
		// the routed listener, and it was not picked.
		{http.MethodPost, "/admin/accounts"},
	} {
		for _, token := range []string{"", testProvisionToken} {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"email":"x@example.com"}`))
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			code, body := do(t, p.d.router, req)
			if code != http.StatusNotFound {
				t.Errorf("%s %s on the ROUTED listener = %d, want 404: %s", tc.method, tc.path, code, body)
			}
		}
	}
}

// TestTheProvisioningListenerDoesNotServeTheWireRoutes, over the real socket and
// with the real credential.
//
// `/healthz` AND `/readyz` ARE IN THE LIST ON PURPOSE. They are the two routes
// somebody would add here out of habit — a second listener wants a probe — and
// adding them would put a probe behind the service's most powerful credential,
// where nothing can call it, or (worse) invite an exception to the door.
func TestTheProvisioningListenerDoesNotServeTheWireRoutes(t *testing.T) {
	p := newProvisionRig(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/sync?after=0"},
		{http.MethodGet, "/ws"},
		{http.MethodPost, "/enroll"},
		{http.MethodPost, "/refresh"},
		{http.MethodGet, "/devices"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/readyz"},
	} {
		code, body := p.call(tc.method, tc.path, `{}`, testProvisionToken)
		if code != http.StatusNotFound {
			t.Errorf("%s %s on the PROVISIONING listener = %d, want 404: %s", tc.method, tc.path, code, body)
		}
	}

	// And a method the surface does not serve on a path it does: net/http's 405,
	// which is the mux's answer and not a handler's.
	if code, _ := p.call(http.MethodDelete, "/accounts", "", testProvisionToken); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /accounts = %d, want 405", code)
	}
}

// TestShutdownJoinsTheProvisioningListener is criterion 10's last clause.
//
// A listener serve() started and does not join is a goroutine holding a socket
// after the process believes it has stopped — and on this port, a socket that
// still answers `POST /accounts` after the drain. So: cancel the context, then
// assert BOTH that the port stops accepting AND that serve returns, because either
// one alone is satisfied by a bug (a listener closed by the process exiting, or a
// serve that returned while the listener ran on).
func TestShutdownJoinsTheProvisioningListener(t *testing.T) {
	p := newProvisionRig(t)

	// Live first, or the assertion below is about a port that never worked.
	if code, body := p.call(http.MethodGet, "/accounts?email=nobody@example.com", "", testProvisionToken); code != http.StatusNotFound {
		t.Fatalf("precondition: the provisioning listener should answer 404 for an unknown email, got %d %s", code, body)
	}

	if err := p.stop(); err != nil {
		t.Fatalf("serve returned %v, want nil — a second listener serve does not join is a goroutine "+
			"holding an account-creating socket after the process believes it has stopped", err)
	}

	// The port is closed. A dial, not a request, because a request through the
	// client would report the same error for a closed port and for a hung one.
	addr := strings.TrimPrefix(p.provisionBase, "http://")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		conn.Close()
		t.Errorf("the provisioning port %s still accepts connections after serve returned", addr)
	}
}

// --- criterion 11: the three operations, over HTTP, are the store's ----------

// TestEnsureOverTheSurfaceCreatesThenFindsAndSupersedesTheFirstToken.
func TestEnsureOverTheSurfaceCreatesThenFindsAndSupersedesTheFirstToken(t *testing.T) {
	p := newProvisionRig(t)

	code, first := p.ensure("Ada@Example.com", "Ada Lovelace")
	if code != http.StatusCreated {
		t.Fatalf("the first ensure = %d, want 201: %v", code, first)
	}
	if got := first["outcome"]; got != "created" {
		t.Errorf("outcome = %v, want created", got)
	}
	acct := accountOf(t, first)
	if got := str(t, acct, "email"); got != "ada@example.com" {
		t.Errorf("email = %q, want it lower-cased by the store", got)
	}
	if got := str(t, acct, "handle"); got != "ada" {
		t.Errorf("handle = %q, want ada — derived from the local part", got)
	}
	if got := str(t, acct, "status"); got != "active" {
		t.Errorf("status = %q, want active", got)
	}
	if got := str(t, acct, "display_name"); got != "Ada Lovelace" {
		t.Errorf("display_name = %q", got)
	}
	if _, has := first["note"]; has {
		t.Errorf("a plain creation carried a note: %v", first["note"])
	}
	// THE ID IS THE USER'S UUID, and it is what deactivate takes.
	id, err := uuid.Parse(str(t, acct, "id"))
	if err != nil {
		t.Fatalf("account.id is not a uuid: %v", err)
	}
	firstToken := str(t, first, "enrollment_token")
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", str(t, first, "enrollment_expires_at")); err != nil {
		t.Errorf("enrollment_expires_at is not in the wire's own layout: %v", err)
	}

	// SECOND CALL, SAME EMAIL: the same account, a NEW token, 200 rather than 201.
	code, second := p.ensure("ada@example.com", "Somebody Else")
	if code != http.StatusOK {
		t.Fatalf("the second ensure = %d, want 200: %v", code, second)
	}
	if got := second["outcome"]; got != "existing" {
		t.Errorf("outcome = %v, want existing", got)
	}
	secondAcct := accountOf(t, second)
	if str(t, secondAcct, "id") != id.String() {
		t.Errorf("the second ensure returned a different account: %v", secondAcct)
	}
	// `ensure` NEVER RENAMES — the store's rule, asserted here because the surface
	// is what carries a name it will not use.
	if got := str(t, secondAcct, "display_name"); got != "Ada Lovelace" {
		t.Errorf("display_name = %q, want the original: ensure must not rename", got)
	}
	secondToken := str(t, second, "enrollment_token")
	if secondToken == firstToken {
		t.Fatal("the second ensure handed back the same enrollment token")
	}

	// THE FIRST TOKEN NO LONGER REDEEMS, and the second does — through POST
	// /enroll on the ROUTED listener, which is the only place an enrollment token
	// is ever spent.
	if code, body := do(t, p.d.router, enrollRequest(t, firstToken, "a device that must not exist")); code != http.StatusUnauthorized {
		t.Errorf("POST /enroll with the superseded token = %d, want 401: %s", code, body)
	}
	if code, body := do(t, p.d.router, enrollRequest(t, secondToken, "Pixel 8 Pro")); code != http.StatusOK {
		t.Errorf("POST /enroll with the fresh token = %d, want 200: %s", code, body)
	}
}

// TestEnsureRefusesAMalformedEmailAndAnUndescribedField.
func TestEnsureRefusesAMalformedEmailAndAnUndescribedField(t *testing.T) {
	p := newProvisionRig(t)
	before := countUsers(p.ctx, t, p.pool)

	for _, tc := range []struct{ name, body string }{
		{"no email", `{"display_name":"Nobody"}`},
		{"an email with no domain", `{"email":"ada"}`},
		{"an email with a space", `{"email":"ada lovelace@example.com"}`},
		{"a field the contract does not describe", `{"email":"ada@example.com","handle":"ada"}`},
		{"not JSON at all", `{`},
		{"two JSON values", `{"email":"ada@example.com"}{"email":"b@example.com"}`},
		{"a body over the bound", `{"email":"ada@example.com","display_name":"` + strings.Repeat("A", 5000) + `"}`},
	} {
		code, raw := p.call(http.MethodPost, "/accounts", tc.body, testProvisionToken)
		if code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", tc.name, code, raw)
		}
	}
	if after := countUsers(p.ctx, t, p.pool); after != before {
		t.Errorf("a refused ensure created %d user row(s)", after-before)
	}
}

// TestEnsureReactivatesAndTheOldDevicesStayRefused — ruling 5 over HTTP.
//
// THE PART THAT NEEDS THE WHOLE PROCESS: the reversal revokes for itself, so a
// device enrolled before the offboard must still be refused after the account is
// live again. The store proves that at its own seam; what this proves is that the
// surface reaches that code and not some other path that merely clears the column.
func TestEnsureReactivatesAndTheOldDevicesStayRefused(t *testing.T) {
	p := newProvisionRig(t)

	_, created := p.ensure("ada@example.com", "Ada Lovelace")
	acct := accountOf(t, created)
	id := uuid.MustParse(str(t, acct, "id"))

	// A real device, enrolled through the routed listener with the token the
	// surface handed out.
	code, body := do(t, p.d.router, enrollRequest(t, str(t, created, "enrollment_token"), "Pixel 8 Pro"))
	if code != http.StatusOK {
		t.Fatalf("enroll = %d: %s", code, body)
	}
	var device wire.EnrollResponse
	if err := json.Unmarshal(body, &device); err != nil {
		t.Fatal(err)
	}
	if code, _ := p.syncWith(string(device.AccessToken)); code != http.StatusOK {
		t.Fatalf("precondition: the new device should be able to sync, got %d", code)
	}

	if code, raw := p.call(http.MethodPost, "/accounts/"+id.String()+"/deactivate", "", testProvisionToken); code != http.StatusNoContent {
		t.Fatalf("deactivate = %d: %s", code, raw)
	}
	if code, _ := p.syncWith(string(device.AccessToken)); code != http.StatusUnauthorized {
		t.Fatalf("a deactivated person's device could still sync: %d", code)
	}

	// RE-INVITE. Purser skips a service only while its account row is active, so
	// this is what any later invite naming Catenary does.
	code, again := p.ensure("ada@example.com", "Ada Lovelace")
	if code != http.StatusOK {
		t.Fatalf("ensure on a deactivated person = %d, want 200: %v", code, again)
	}
	if got := again["outcome"]; got != "reactivated" {
		t.Fatalf("outcome = %v, want reactivated — an operator who re-invited somebody has to be "+
			"told that they just undid an offboard", got)
	}
	if got := str(t, accountOf(t, again), "status"); got != "active" {
		t.Errorf("status = %q, want active after a reactivation", got)
	}

	// THE OLD DEVICE IS STILL REFUSED. This is the clause rev 1 of the plan got
	// wrong: clearing the column without sweeping would bring it back to life.
	if code, _ := p.syncWith(string(device.AccessToken)); code != http.StatusUnauthorized {
		t.Errorf("the device from before the offboard works again after the reactivation (%d) — "+
			"the reversal did not sweep", code)
	}
	if code, body := do(t, p.d.router, refreshRequest(t, string(device.RefreshToken))); code != http.StatusUnauthorized {
		t.Errorf("the old device could refresh after the reactivation: %d %s", code, body)
	}

	// And the fresh invitation works, so the person can get back in from nothing.
	if code, body := do(t, p.d.router, enrollRequest(t, str(t, again, "enrollment_token"), "a new laptop")); code != http.StatusOK {
		t.Errorf("the reactivation's own token does not enroll: %d %s", code, body)
	}
}

// TestTheNeverAdoptNoteReachesTheSurface. CANT-130 refuses to hand back an
// existing email-less person on a handle match — adopting by handle would give
// somebody else's account away — and the operator learns through `note`. The
// connector carries it into Result.Instructions, and this is the only hop where it
// could be dropped.
func TestTheNeverAdoptNoteReachesTheSurface(t *testing.T) {
	p := newProvisionRig(t)
	// One of soakrig's people: a real person, no email.
	mkUser(p.ctx, t, p.pool, "ada", "Ada Lovelace")

	code, body := p.ensure("ada@example.com", "Ada L")
	if code != http.StatusCreated {
		t.Fatalf("= %d, want 201: %v", code, body)
	}
	if got := str(t, accountOf(t, body), "handle"); got != "ada-2" {
		t.Errorf("handle = %q, want ada-2 — the email-less `ada` must not be adopted", got)
	}
	note, ok := body["note"].(string)
	if !ok {
		t.Fatalf("no note on the never-adopt path: %v", body)
	}
	for _, want := range []string{"ada-2", "ada", "set-email"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not name %q: %q", want, note)
		}
	}
}

// TestLookUpOverTheSurfaceChangesNothing — criterion 11's read-only clause, at the
// transport rather than at the store seam.
//
// A ROW DUMP BEFORE AND AFTER, over `users` and all four credential tables, which
// is what the store's own test does: "read-only" has to mean no enrollment token
// minted, no token superseded and no column touched, and a count would miss all
// three.
func TestLookUpOverTheSurfaceChangesNothing(t *testing.T) {
	p := newProvisionRig(t)

	_, created := p.ensure("ada@example.com", "Ada Lovelace")
	acct := accountOf(t, created)
	if code, body := do(t, p.d.router, enrollRequest(t, str(t, created, "enrollment_token"), "Pixel 8 Pro")); code != http.StatusOK {
		t.Fatalf("enroll = %d: %s", code, body)
	}

	before := dumpTables(p.ctx, t, p.pool)

	code, raw := p.call(http.MethodGet, "/accounts?email=ADA@example.com", "", testProvisionToken)
	if code != http.StatusOK {
		t.Fatalf("look up = %d, want 200 (and case-insensitively): %s", code, raw)
	}
	var found map[string]any
	if err := json.Unmarshal(raw, &found); err != nil {
		t.Fatal(err)
	}
	if str(t, found, "id") != str(t, acct, "id") {
		t.Errorf("the look-up returned a different account: %v", found)
	}
	if got := str(t, found, "status"); got != "active" {
		t.Errorf("status = %q, want active", got)
	}
	if got, ok := found["live_devices"].(float64); !ok || got != 1 {
		t.Errorf("live_devices = %v, want 1", found["live_devices"])
	}
	// FLAT, not nested: PRSR-50 is written against exactly this shape.
	if _, nested := found["account"]; nested {
		t.Errorf("the look-up nested its account; the contract is flat: %v", found)
	}

	// Nobody, and a malformed address — both 404, and both read-only.
	for _, query := range []string{"?email=nobody@example.com", "?email=not-an-address"} {
		if code, body := p.call(http.MethodGet, "/accounts"+query, "", testProvisionToken); code != http.StatusNotFound {
			t.Errorf("look up %s = %d, want 404: %s", query, code, body)
		}
	}
	// A missing parameter is the one 400 here.
	if code, body := p.call(http.MethodGet, "/accounts", "", testProvisionToken); code != http.StatusBadRequest {
		t.Errorf("look up with no email parameter = %d, want 400: %s", code, body)
	}
	if code, body := p.call(http.MethodGet, "/accounts?email=%20", "", testProvisionToken); code != http.StatusBadRequest {
		t.Errorf("look up with a blank email = %d, want 400: %s", code, body)
	}

	if after := dumpTables(p.ctx, t, p.pool); after != before {
		t.Error("a look-up changed the database. Purser's Reconcile runs for every person on every " +
			"bundle run, so a look-up that minted or superseded anything would invalidate a " +
			"working invitation every time somebody reconciled")
	}
}

// TestDeactivateOverTheSurfaceSeversEverySocketAndIsIdempotent — and the three
// 404s are byte-identical.
func TestDeactivateOverTheSurfaceSeversEverySocketAndIsIdempotent(t *testing.T) {
	p := newProvisionRig(t)
	runRevocations(p.rig)

	_, created := p.ensure("ada@example.com", "Ada Lovelace")
	id := uuid.MustParse(str(t, accountOf(t, created), "id"))

	// TWO sockets, so "every device at once" is a claim rather than a sample of
	// one, and a third account's socket, because an account-wide revocation is
	// exactly the shape that could take the estate down with it.
	phone := openAt(t, p.ctx, p.base, p.enroll(id, "Pixel 8 Pro"), id)
	laptop := openAt(t, p.ctx, p.base, p.enroll(id, "Framework 13"), id)
	theo := mkUser(p.ctx, t, p.pool, "theo", "Theo")
	theoSession := openAt(t, p.ctx, p.base, p.enroll(theo, "Theo's phone"), theo)
	phone.pingPong()
	laptop.pingPong()
	theoSession.pingPong()

	if code, body := p.call(http.MethodPost, "/accounts/"+id.String()+"/deactivate", "", testProvisionToken); code != http.StatusNoContent {
		t.Fatalf("deactivate = %d, want 204: %s", code, body)
	}
	for _, tc := range []struct {
		name string
		s    *session
	}{{"the phone", phone}, {"the laptop", laptop}} {
		if got := tc.s.expectClose(); got != hub.StatusRevoked {
			t.Errorf("%s closed with %d, want %d — a deprovision that does not reach the socket leaves "+
				"a deactivated person receiving every message in every room they were in until the "+
				"connection happens to drop", tc.name, got, hub.StatusRevoked)
		}
	}
	theoSession.pingPong()

	// IDEMPOTENT: 204 again, with no body either time.
	code, body := p.call(http.MethodPost, "/accounts/"+id.String()+"/deactivate", "", testProvisionToken)
	if code != http.StatusNoContent || len(body) != 0 {
		t.Errorf("the second deactivate = %d with body %q, want 204 and nothing", code, body)
	}

	// THE THREE 404s. An unknown id, a BOT's id, and something that is not a uuid
	// — all the same status and the same bytes, because a caller that could tell
	// them apart could learn which ids name service accounts.
	bot, err := p.st.CreateBot(p.ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	botToken, err := p.st.IssueBotToken(p.ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	var notFound [][]byte
	for _, tc := range []struct{ name, id string }{
		{"an id nothing holds", uuid.NewString()},
		{"a bot's id", bot.String()},
		{"an id that is not a uuid", "not-a-uuid"},
	} {
		code, body := p.call(http.MethodPost, "/accounts/"+tc.id+"/deactivate", "", testProvisionToken)
		if code != http.StatusNotFound {
			t.Errorf("deactivate %s = %d, want 404: %s", tc.name, code, body)
		}
		notFound = append(notFound, body)
	}
	for i := 1; i < len(notFound); i++ {
		if !bytes.Equal(notFound[i], notFound[0]) {
			t.Errorf("two not-founds differ, so the surface tells them apart:\n  %q\n  %q", notFound[0], notFound[i])
		}
	}
	// AND THE BOT STILL WORKS. A deprovision that reached a service account would
	// take Argosy off the estate, silently, on somebody's offboarding day.
	if _, err := p.st.Authenticate(p.ctx, botToken.Plaintext); err != nil {
		t.Errorf("the bot's token stopped working: %v", err)
	}
}

// --- the log -----------------------------------------------------------------

// TestNoProvisioningLogLineCarriesAnEmailOrACredential.
//
// Every operation is exercised and then EVERY line the process wrote is searched.
// The enrollment token is a credential that redeems into a device enrolled as
// somebody; the email is the one field invariant 3's honesty argument is about, and
// it is held per person now — a log is read by more people than the database is,
// shipped to Dozzle and Datadog, and kept.
func TestNoProvisioningLogLineCarriesAnEmailOrACredential(t *testing.T) {
	p := newProvisionRig(t)

	// `zora` RATHER THAN `ada`, AND THE REASON IS THE UUID. The assertion below
	// searches for the email's LOCAL PART as well as the whole address, and `ada`
	// is valid hexadecimal — it can appear inside an account id by chance, which
	// would make this test fail once in a while for no reason. `zora` cannot.
	const email = "zora@example.com"
	const localPart = "zora"

	_, created := p.ensure(email, "Zora Neale Hurston")
	token := str(t, created, "enrollment_token")
	id := str(t, accountOf(t, created), "id")
	p.call(http.MethodGet, "/accounts?email="+email, "", testProvisionToken)
	p.call(http.MethodGet, "/accounts?email=nobody@example.com", "", testProvisionToken)
	p.call(http.MethodPost, "/accounts/"+id+"/deactivate", "", testProvisionToken)
	p.call(http.MethodPost, "/accounts", `{"email":"`+email+`"}`, "")

	for _, line := range p.log.rendered() {
		for _, forbidden := range []struct{ what, value string }{
			{"an email address", email},
			{"an email address", "nobody@example.com"},
			{"an enrollment token", token},
			{"the service credential", testProvisionToken},
		} {
			if strings.Contains(line, forbidden.value) {
				t.Errorf("a log line carries %s: %q", forbidden.what, line)
			}
		}
		// AND THE PROVISIONING SURFACE'S OWN LINES CARRY NOT EVEN THE LOCAL PART.
		// The store's `person ensured` line records the handle it chose, which is
		// where that belongs; this surface is the one that is handed the address,
		// and a handle is derived from it, so its lines are ids and counts only.
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] == "provisioning" && strings.Contains(line, localPart) {
			t.Errorf("a provisioning line carries the email's local part: %q", line)
		}
	}

	// And the INFO line each accepted call owes is there, with the account id.
	calls := p.log.find("provisioning call")
	if len(calls) < 4 {
		t.Errorf("four accepted calls wrote %d `provisioning call` lines", len(calls))
	}
	var sawEnsure bool
	for _, rec := range calls {
		if attrOf(rec, "op") == "ensure" {
			sawEnsure = true
			if got := fmt.Sprint(attrOf(rec, "account_id")); got != id {
				t.Errorf("the ensure line's account_id = %q, want %q", got, id)
			}
			if got := attrOf(rec, "outcome"); got != "created" {
				t.Errorf("the ensure line's outcome = %v, want created", got)
			}
		}
	}
	if !sawEnsure {
		t.Error("no `provisioning call` line names the ensure")
	}
	// The refused call is at WARN and names no operation it did not reach.
	if refusals := p.log.find("provisioning credential refused"); len(refusals) != 1 {
		t.Errorf("the refused call wrote %d refusal lines, want 1", len(refusals))
	}
}

// --- helpers -----------------------------------------------------------------

// syncWith asks GET /sync on the ROUTED listener with a device's access token,
// which is the cheapest way to ask store.Authenticate a question through the
// transport a client would use.
func (p *provisionRig) syncWith(accessToken string) (int, []byte) {
	p.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	return do(p.t, p.d.router, req)
}

// rendered renders every captured record — level, message and every attribute —
// as one searchable string per line.
//
// A METHOD ON fanout_test.go's recorder, DEFINED HERE because this is the only
// file that needs it. `find` answers "was this message logged"; the assertion
// above needs the opposite question — "does ANY line anywhere carry this string" —
// and that one has to see the attributes, since an email would arrive as one.
func (r *recorder) rendered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.recs))
	for _, rec := range r.recs {
		var b strings.Builder
		b.WriteString(rec.Level.String())
		b.WriteByte(' ')
		b.WriteString(rec.Message)
		rec.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&b, " %s=%s", a.Key, a.Value.String())
			return true
		})
		out = append(out, b.String())
	}
	return out
}

func countUsers(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// dumpTables is the store's own tablesSnapshot, which is unexported there: every
// row of `users` and the four credential tables, rendered so that two snapshots of
// identical rows compare equal.
func dumpTables(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var b strings.Builder
	for _, table := range []string{"users", "enrollment_tokens", "devices", "refresh_tokens", "access_tokens"} {
		rows, err := pool.Query(ctx, `SELECT * FROM `+table+` ORDER BY id`)
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		vals, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		b.WriteString(table)
		b.WriteByte(':')
		for _, v := range vals {
			// %v on a map is key-sorted, so this is stable across two snapshots.
			fmt.Fprintf(&b, "%v;", v)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
