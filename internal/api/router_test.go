package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubPinger struct{ err error }

func (s stubPinger) Ping(context.Context) error { return s.err }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body %q is not JSON: %v", path, rec.Body.String(), err)
	}
	return rec, body
}

// The whole point of /healthz being dependency-free: a database that is down
// must not make the liveness probe fail, because the remedy for a down
// database is not restarting every Catenary instance.
func TestHealthzIgnoresADeadDatabase(t *testing.T) {
	h := NewRouter(Deps{
		Logger:  discardLogger(),
		DB:      stubPinger{err: errors.New("connection refused")},
		Version: "1.2.3",
		Commit:  "abc123",
	})

	rec, body := get(t, h, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("healthz with a dead database = %d, want 200", rec.Code)
	}
	if body["status"] != "ok" || body["version"] != "1.2.3" || body["commit"] != "abc123" {
		t.Errorf("healthz body = %v", body)
	}
}

func TestHealthzOmitsAnUnstampedCommit(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), Version: "dev"})
	_, body := get(t, h, "/healthz")
	if _, ok := body["commit"]; ok {
		t.Errorf("healthz reported a commit for an unstamped build: %v", body)
	}
}

func TestReadyz(t *testing.T) {
	cases := []struct {
		name string
		db   Pinger
		want int
	}{
		{"database reachable", stubPinger{}, http.StatusOK},
		{"database down", stubPinger{err: errors.New("connection refused")}, http.StatusServiceUnavailable},
		{"no database wired", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewRouter(Deps{Logger: discardLogger(), DB: tc.db})
			rec, body := get(t, h, "/readyz")
			if rec.Code != tc.want {
				t.Errorf("readyz = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusServiceUnavailable && body["check"] != "database" {
				t.Errorf("unready body does not name the failing check: %v", body)
			}
		})
	}
}

// Structured, to stdout, in a shape Dozzle and Datadog can read: one JSON
// object per line with the fields parsed as attributes rather than a message
// somebody has to regex.
func TestRequestLogIsStructuredJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewRouter(Deps{Logger: logger})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no log line emitted")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q", line)
	}
	for _, k := range []string{"time", "level", "msg", "method", "path", "status", "duration_ms"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("log line has no %q attribute: %v", k, entry)
		}
	}
	if entry["path"] != "/healthz" || entry["status"] != float64(200) {
		t.Errorf("log line = %v", entry)
	}
}

// Probes are polled forever. At info they are the log, which is how a real
// error gets scrolled past.
func TestProbesAreLoggedAtDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := NewRouter(Deps{Logger: logger})

	for _, p := range []string{"/healthz", "/readyz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	if buf.Len() != 0 {
		t.Errorf("probes logged at info level: %q", buf.String())
	}

	// A 404 is not a probe and must still be logged.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))
	if buf.Len() == 0 {
		t.Error("a 404 was not logged")
	}
	if !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Errorf("a 404 was not logged at warn: %q", buf.String())
	}
}

// The /readyz 503 is the most important line this service emits. Suppressing
// probe traffic by path alone would hide it.
func TestFailingProbeIsNotSuppressed(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := NewRouter(Deps{Logger: logger, DB: stubPinger{err: errors.New("connection refused")}})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))

	out := buf.String()
	if !strings.Contains(out, `"path":"/readyz"`) || !strings.Contains(out, `"status":503`) {
		t.Errorf("a failing /readyz was suppressed to debug: %q", out)
	}
}

// A POST to a GET-only probe is a 405, not a 200. Go 1.22+ method-scoped
// patterns give this for free; the test is here so a route rewrite cannot
// quietly widen the probes into a write surface.
func TestProbesAreGetOnly(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger()})
	for _, p := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, p, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", p, rec.Code)
		}
	}
}

// E2's WebSocket upgrade goes through this middleware, and both websocket
// libraries begin with `w.(http.Hijacker)`. A wrapper that embeds only
// http.ResponseWriter fails that assertion at runtime, in the upgrade handler,
// two files from the cause. CANT-22 is Mode C and should not spend its review
// on this.
func TestRequestLoggerPreservesHijacker(t *testing.T) {
	var (
		sawHijacker bool
		sawUnwrap   bool
	)
	h := requestLogger(discardLogger(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHijacker = w.(http.Hijacker)
		// http.NewResponseController follows Unwrap; without it the controller
		// cannot reach the real writer either.
		_, sawUnwrap = w.(interface{ Unwrap() http.ResponseWriter })
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if !sawHijacker {
		t.Error("the handler cannot type-assert http.Hijacker through the request logger; the WebSocket upgrade will fail at runtime")
	}
	if !sawUnwrap {
		t.Error("the wrapper has no Unwrap, so http.NewResponseController cannot reach the real writer")
	}
}
