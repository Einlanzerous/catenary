package provision

// CANT-131 criterion 10, this listener's half — THIS SURFACE SERVES ONLY ITS OWN.
//
// The other half (the routed listener does not serve `/accounts`, and serve joins
// the second listener on shutdown) needs the real process and lives in
// cmd/catenary/provision_test.go. What is here is the route table, asserted as a
// closed set rather than as three positives: a surface whose safety argument is
// "the credential, plus a listener that serves nothing else" is only as good as
// the second clause.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The routed listener's whole surface, as internal/api registers it. NONE of
// these exists here.
var wireRoutes = []struct{ method, path string }{
	{http.MethodGet, "/sync"},
	{http.MethodGet, "/ws"},
	{http.MethodPost, "/enroll"},
	{http.MethodPost, "/refresh"},
	{http.MethodGet, "/devices"},
	{http.MethodPost, "/devices/x/revoke"},
	{http.MethodPost, "/conversations/direct"},
	{http.MethodGet, "/healthz"},
	{http.MethodGet, "/readyz"},
}

// TestTheProvisioningSurfaceServesNothingButItsThreeRoutes.
//
// `/healthz` AND `/readyz` ARE ON THE LIST DELIBERATELY, and their absence is not
// an oversight to be fixed later. Both probes exist to be polled by an
// orchestrator on the routed port, where they already are; adding them here would
// put two unauthenticated-looking route names on the one listener whose entire
// design is that every path on it takes the most powerful credential in the
// service — and a probe behind that credential is a probe nothing can call.
func TestTheProvisioningSurfaceServesNothingButItsThreeRoutes(t *testing.T) {
	hit := &reached{}
	srv := testServer(t, &recorder{}, hit)

	for _, r := range wireRoutes {
		req := httptest.NewRequest(r.method, r.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		// WITH THE VALID CREDENTIAL, so this measures the route table and not the
		// door. A 404 here and a 401 without the credential are both correct and
		// they answer different questions.
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the provisioning listener = %d, want 404 — this listener serves three "+
				"routes and a fourth is how the surface stops being the thing its safety argument "+
				"describes", r.method, r.path, rec.Code)
		}
	}
	if hit.n != 0 {
		t.Errorf("an unknown route reached the store %d time(s)", hit.n)
	}
}

// TestAnUnknownRouteIsLoggedOnce. The mux's own 404 writes a response and
// returns, so if ServeHTTP did not log it nothing would: a connector pinned to the
// wrong contract version would get 404s that appear nowhere in this service's log.
func TestAnUnknownRouteIsLoggedOnce(t *testing.T) {
	log := &recorder{}
	srv := testServer(t, log, &reached{})
	req := httptest.NewRequest(http.MethodPost, "/accounts/x/revoke", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	srv.ServeHTTP(httptest.NewRecorder(), req)

	lines := log.lines()
	if len(lines) != 1 || !strings.Contains(lines[0], "does not serve") {
		t.Errorf("an unknown route logged %v, want one line naming it", lines)
	}
	if !strings.Contains(lines[0], "/accounts/x/revoke") {
		t.Errorf("the line does not carry the path that was asked for: %q", lines[0])
	}
}

// TestTheDoorIsAboveTheRouter. Without the credential, an unknown path answers
// 401 and not 404 — so probing the route table costs a working credential, and a
// caller that has none learns nothing about what this listener serves.
func TestTheDoorIsAboveTheRouter(t *testing.T) {
	srv := testServer(t, &recorder{}, &reached{})
	for _, path := range []string{"/accounts", "/sync", "/", "/admin/accounts"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no credential = %d, want 401 — the door decides before the router, "+
				"or the route table is readable by anybody who can reach the port", path, rec.Code)
		}
	}
}

// TestANonCanonicalPathIsStillBehindTheDoor pins what net/http does with a path
// that is not in canonical form, because it is the one answer this surface gives
// that provision/openapi.yaml does not describe as an operation.
//
// MEASURED RATHER THAN ASSUMED: `//accounts` is a 307 to `/accounts` (ServeMux's
// own path cleaning, method and body preserved) and `/accounts/` is a plain 404.
// Neither is a problem, and the reason is the ordering: the door runs BEFORE the
// mux, so a caller with no credential gets 401 for both and never learns that one
// of them would have been redirected. The redirect is recorded here so that a
// reader of the contract who finds a 307 in a proxy log can see it was known.
func TestANonCanonicalPathIsStillBehindTheDoor(t *testing.T) {
	srv := testServer(t, &recorder{}, &reached{})

	for _, tc := range []struct {
		path string
		with int // with the credential
	}{
		{"//accounts", http.StatusTemporaryRedirect},
		{"/accounts/", http.StatusNotFound},
		{"/ACCOUNTS", http.StatusNotFound},
	} {
		// Without the credential: 401, whatever the path would have done.
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no credential = %d, want 401", tc.path, rec.Code)
		}

		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != tc.with {
			t.Errorf("GET %s with the credential = %d, want %d (a MEASURED net/http behaviour — if it "+
				"changed, provision/openapi.yaml's note about non-operation answers needs updating)",
				tc.path, rec.Code, tc.with)
		}
	}
}

// TestADeactivateStillResolvesItsPathValue is a regression pin with a specific
// history: ServeHTTP asks mux.Handler whether any route matched, in order to log
// the ones that did not, and then dispatches through mux.ServeHTTP. Calling the
// handler that mux.Handler returned would be the obvious one-line saving and is
// wrong — ServeMux.Handler does not bind path wildcards, so `{id}` would arrive
// empty and every deactivate would answer 404 for a perfectly good id. It was
// written that way first and this is what caught it.
func TestADeactivateStillResolvesItsPathValue(t *testing.T) {
	hit := &reached{}
	srv := testServer(t, &recorder{}, hit)
	req := httptest.NewRequest(http.MethodPost, "/accounts/6f1d9e2a-3b4c-4d5e-8f70-1a2b3c4d5e6f/deactivate", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// The stub answers ErrPersonNotFound, so 404 is the right status — what
	// matters is that the STORE WAS REACHED, which only happens when the id
	// parsed.
	if hit.n != 1 {
		t.Fatalf("a well-formed id reached the store %d time(s), want 1: the handler read %q from "+
			"the path", hit.n, "{id}")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("= %d, want 404 from the stub's ErrPersonNotFound", rec.Code)
	}
}
