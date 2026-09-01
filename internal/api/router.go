// Package api is Catenary's HTTP and WebSocket surface. CANT-17 lands the two
// probes and the request log; the sync, auth and socket routes arrive with E2.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Pinger is the readiness check's only dependency. An interface rather than a
// *pgxpool.Pool so this package does not import the store, and so a test can
// hand it a database that fails on demand — the state /readyz exists to report
// is the one that is hardest to arrange with a real pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// readyTimeout bounds the readiness probe's database check. A probe that hangs
// is worse than one that fails: an orchestrator waiting on a response cannot
// tell "slow" from "wedged", and holds the instance in rotation either way.
const readyTimeout = 2 * time.Second

// Deps is what the router needs. Every field is optional except Logger, so the
// surface can be stood up before the things behind it exist.
type Deps struct {
	// Logger receives the request log. Required.
	Logger *slog.Logger

	// DB is pinged by /readyz. Nil reports ready — a build with no database
	// wired is a build that has nothing to be unready about, and CANT-13 is
	// what fills this in.
	DB Pinger

	// Version and Commit are stamped at build time and reported by /healthz,
	// so a deploy can be checked against what was meant to ship.
	Version string
	Commit  string
}

// NewRouter builds the HTTP handler.
//
// The two probes answer different questions and must not be collapsed:
//
//	/healthz — is this process alive? No dependencies. If it answers, the
//	           binary is running, and a restart is the wrong remedy for a
//	           database that is merely down.
//	/readyz  — can this process serve traffic? Pings the database. A load
//	           balancer takes an unready instance out of rotation; it does
//	           not kill it.
//
// Collapsing them means a Postgres restart restarts every Catenary instance
// too, which turns a recoverable dependency outage into a reconnect storm
// across every open WebSocket.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{"status": "ok", "version": d.Version}
		if d.Commit != "" {
			body["commit"] = d.Commit
		}
		writeJSON(w, http.StatusOK, body)
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if d.DB != nil {
			ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
			defer cancel()
			if err := d.DB.Ping(ctx); err != nil {
				d.Logger.Warn("readiness probe failed", "check", "database", "error", err)
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "unready",
					"check":  "database",
				})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	return requestLogger(d.Logger, mux)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// statusRecorder captures the status code so the request log can report it.
// net/http gives the middleware no other way to see what the handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// requestLogger emits one structured line per request.
//
// SUCCESSFUL probe requests are logged at debug rather than info. They are
// polled every few seconds forever, and at info they are most of the log by
// volume — which is how a real error gets scrolled past. They are still
// emitted, because a probe that started failing is worth being able to find.
//
// The order of the switch is load bearing: a probe that answers 4xx or 5xx is
// not routine traffic and keeps its level. Suppressing by path alone would
// hide the /readyz 503 that is the single most important line this service
// can emit.
func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
			level = slog.LevelDebug
		}

		logger.LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			// Milliseconds as a float, not slog.Duration: that emits bare
			// nanoseconds under a key with no unit in it, which is a number
			// every reader has to be told how to interpret.
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}
