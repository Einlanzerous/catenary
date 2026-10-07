package api

// CANT-241 — the web client on the routed listener: CANT-231's plan criteria
// Absent client, Served client, Cache policy, No fallback (ruling 1 → A),
// Baseline headers and the row-0 half of Interim, then full (ruling 2 → B).
//
// Every test here builds its bundle from an in-memory fixture, so they run on a
// checkout that never ran `npm`. The real bundle is internal/webui's test and
// cmd/catenary's second-port test.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/webui"
	"github.com/magos/catenary/internal/wire"
)

var fixtureFiles = map[string]struct {
	body, contentType string
}{
	"index.html":                {`<!doctype html><div id="app"></div><script type="module" src="/assets/index-AAAAAAAA.js"></script>`, "text/html; charset=utf-8"},
	"favicon.svg":               {`<svg xmlns="http://www.w3.org/2000/svg"></svg>`, "image/svg+xml"},
	"assets/index-AAAAAAAA.js":  {`console.log("fixture")`, "text/javascript; charset=utf-8"},
	"assets/index-BBBBBBBB.css": {`body{color:#000}`, "text/css; charset=utf-8"},
}

// urlOf is the path a bundle file is served at.
func urlOf(p string) string {
	if p == webui.Index {
		return "/"
	}
	return "/" + p
}

func fixtureBundle(t *testing.T) *webui.Bundle {
	t.Helper()
	fsys := fstest.MapFS{}
	for p, f := range fixtureFiles {
		fsys[p] = &fstest.MapFile{Data: []byte(f.body)}
	}
	b, err := webui.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func serveStatic(h http.Handler, method, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// everySeam is a Deps with every optional route registered — so the static
// patterns are proved beside the whole API, not beside an empty mux.
func everySeam(web *webui.Bundle) Deps {
	return Deps{
		Logger: discardLogger(),
		Sync: func(context.Context, uuid.UUID, int64, int) (wire.SyncResponse, error) {
			return wire.SyncResponse{}, nil
		},
		CallerID: func(*http.Request) (uuid.UUID, bool) { return uuid.Nil, false },
		Enroll:   func(context.Context, string, string) (store.Enrollment, error) { return store.Enrollment{}, nil },
		Refresh:  func(context.Context, string, string) (store.Rotated, error) { return store.Rotated{}, nil },
		Devices:  func(context.Context, uuid.UUID) ([]store.DeviceRow, error) { return nil, nil },
		RevokeOwnDevice: func(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
			return false, nil
		},
		Authenticate: func(context.Context, string) (store.Caller, error) { return store.Caller{}, nil },
		Hello: func(context.Context, store.HelloRequest) (store.HelloResult, error) {
			return store.HelloResult{}, nil
		},
		Send:   func(context.Context, store.NewMessage) (store.Sent, error) { return store.Sent{}, nil },
		Caller: func(*http.Request) (store.Caller, bool) { return store.Caller{}, false },
		MessageForFanout: func(context.Context, uuid.UUID, int64) (store.FanoutMessage, error) {
			return store.FanoutMessage{}, nil
		},
		FindOrCreateDirect: func(context.Context, uuid.UUID, string) (store.ConversationRow, error) {
			return store.ConversationRow{}, nil
		},
		Web: web,
	}
}

// Absent client: no bundle, no static route, and the API answers as it did.
func TestWithNoClientThereIsNoStaticRoute(t *testing.T) {
	h := NewRouter(everySeam(nil))
	for _, p := range []string{"/", "/favicon.svg", "/assets/x.js"} {
		if rec := serveStatic(h, http.MethodGet, p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with no client = %d, want 404", p, rec.Code)
		}
	}
	if rec := serveStatic(h, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d", rec.Code)
	}
	if rec := serveStatic(h, http.MethodGet, "/enroll", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /enroll = %d, want 405", rec.Code)
	}
	if rec := serveStatic(h, http.MethodGet, "/sync", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /sync with no credential = %d, want 401", rec.Code)
	}
}

// Served client: every file at its own path, with its bytes and its type.
func TestTheClientIsServedFromTheBundle(t *testing.T) {
	h := NewRouter(everySeam(fixtureBundle(t)))
	for p, f := range fixtureFiles {
		rec := serveStatic(h, http.MethodGet, urlOf(p), nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", urlOf(p), rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != f.contentType {
			t.Errorf("GET %s Content-Type = %q, want %q", urlOf(p), got, f.contentType)
		}
		if !bytes.Equal(rec.Body.Bytes(), []byte(f.body)) {
			t.Errorf("GET %s body = %q, want the fixture's bytes", urlOf(p), rec.Body.String())
		}
	}

	rec := serveStatic(h, http.MethodHead, "/", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD / = %d with %d body bytes, want 200 and none", rec.Code, rec.Body.Len())
	}
}

// Cache policy: the unhashed files revalidate against a strong ETag; the hashed
// ones are immutable.
func TestTheCachePolicyFollowsTheHash(t *testing.T) {
	h := NewRouter(everySeam(fixtureBundle(t)))
	for _, p := range []string{"/", "/favicon.svg"} {
		rec := serveStatic(h, http.MethodGet, p, nil)
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", p, got)
		}
		etag := rec.Header().Get("ETag")
		if len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' {
			t.Errorf("GET %s ETag = %q, want a quoted strong validator", p, etag)
			continue
		}
		again := serveStatic(h, http.MethodGet, p, http.Header{"If-None-Match": {etag}})
		if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
			t.Errorf("GET %s with If-None-Match = %d with %d body bytes, want 304 and none", p, again.Code, again.Body.Len())
		}
	}
	for p := range fixtureFiles {
		if !strings.HasPrefix(p, "assets/") {
			continue
		}
		rec := serveStatic(h, http.MethodGet, urlOf(p), nil)
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("GET %s Cache-Control = %q", urlOf(p), got)
		}
	}
}

// No fallback (ruling 1 → A): an unknown path is a 404, and the static
// patterns change no answer the API gave.
func TestAnUnknownPathIsA404AndNeverTheDocument(t *testing.T) {
	doc := []byte(fixtureFiles["index.html"].body)
	h := NewRouter(everySeam(fixtureBundle(t)))

	unwired := everySeam(fixtureBundle(t))
	unwired.Sync, unwired.CallerID = nil, nil
	hUnwired := NewRouter(unwired)

	for _, tc := range []struct {
		h            http.Handler
		method, path string
		want         int
	}{
		{h, http.MethodGet, "/nope", http.StatusNotFound},
		{h, http.MethodGet, "/index.html", http.StatusNotFound},
		{h, http.MethodGet, "/assets/", http.StatusNotFound},
		{h, http.MethodGet, "/assets/nope.js", http.StatusNotFound},
		{h, http.MethodPost, "/nope", http.StatusNotFound},
		{h, http.MethodPost, "/", http.StatusMethodNotAllowed},
		{h, http.MethodGet, "/enroll", http.StatusMethodNotAllowed},
		{hUnwired, http.MethodGet, "/sync", http.StatusNotFound},
	} {
		rec := serveStatic(tc.h, tc.method, tc.path, nil)
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
		if bytes.Equal(rec.Body.Bytes(), doc) {
			t.Errorf("%s %s answered with the document", tc.method, tc.path)
		}
		if tc.path == "/enroll" {
			if got := rec.Header().Get("Allow"); got != "POST" {
				t.Errorf("GET /enroll Allow = %q, want POST", got)
			}
		}
	}
}

// documentCSPWant is the row-0 policy, ruling 2 → B. CANT-242 replaces this
// constant with the full string from CANT-231's routes-and-headers section.
const documentCSPWant = "frame-ancestors 'none'"

// Baseline headers, and the interim policy on the document only.
func TestStaticResponsesCarryTheBaselineHeaders(t *testing.T) {
	h := NewRouter(everySeam(fixtureBundle(t)))
	for p := range fixtureFiles {
		rec := serveStatic(h, http.MethodGet, urlOf(p), nil)
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q, want nosniff", urlOf(p), got)
		}
		csp := rec.Header().Values("Content-Security-Policy")
		if p == webui.Index {
			if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("GET / Referrer-Policy = %q, want no-referrer", got)
			}
			if len(csp) != 1 || csp[0] != documentCSPWant {
				t.Errorf("GET / Content-Security-Policy = %q, want exactly %q", csp, documentCSPWant)
			}
		} else if len(csp) != 0 {
			t.Errorf("GET %s carries a Content-Security-Policy %q; only the document does", urlOf(p), csp)
		}
	}

	rec := serveStatic(h, http.MethodGet, "/healthz", nil)
	for _, k := range []string{"X-Content-Type-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if v := rec.Header().Get(k); v != "" {
			t.Errorf("GET /healthz carries %s: %q; API responses are unchanged", k, v)
		}
	}
}

// A duplicate pattern is a panic at boot, never a silent shadow — which is
// what a bundle file whose path equalled an API route would hit.
func TestADuplicateStaticPatternPanicsAtRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering one bundle twice on one mux did not panic")
		}
	}()
	mux := http.NewServeMux()
	b := fixtureBundle(t)
	registerWebClient(mux, b)
	registerWebClient(mux, b)
}
