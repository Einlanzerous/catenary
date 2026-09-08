// Package api is Catenary's HTTP and WebSocket surface. CANT-17 lands the two
// probes and the request log; the sync, auth and socket routes arrive with E2.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// Pinger is the readiness check's only dependency. An interface rather than a
// *pgxpool.Pool so /readyz needs no store, and so a test can
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
	//
	// SO WHILE THIS IS NIL, /readyz IS A SECOND ALWAYS-200 ROUTE. No state of
	// any database produces a 503 from it, because it never looks at one. The
	// 503 path below is real and tested, but until CANT-13 passes a store it is
	// reachable only from a test — which is worth saying out loud, because
	// "/readyz reports database health" is exactly the sort of claim that gets
	// made about a release where it is not yet true.
	DB Pinger

	// Version and Commit are stamped at build time and reported by /healthz,
	// so a deploy can be checked against what was meant to ship.
	Version string
	Commit  string

	// Sync serves GET /sync. Nil means the route is not registered at all.
	//
	// A function rather than the store, so this package still does not import
	// it: the handler's job is query parsing, status codes and encoding, and
	// all three are testable without a database. CANT-20 owns what is behind
	// it.
	Sync func(ctx context.Context, viewer uuid.UUID, after int64, limit int) (wire.SyncResponse, error)

	// CallerID answers "who is asking" for an authenticated route. Nil means
	// those routes are not registered.
	//
	// THERE IS NO IMPLEMENTATION OF THIS YET, and that is the honest state
	// rather than an oversight. Authentication is CANT-22's handshake and
	// CANT-29's refresh rotation, both Mode C and both unbuilt. Injecting it
	// keeps CANT-20 from inventing an auth scheme in passing — the failure mode
	// where a temporary header becomes permanent.
	//
	// SO WHILE THIS IS NIL THERE IS NO GET /sync. Not a 401, not an empty page:
	// the route does not exist. A sync endpoint that cannot identify its caller
	// would serve one member's log to another, and the safe absence is better
	// than a placeholder that looks wired.
	CallerID func(r *http.Request) (uuid.UUID, bool)
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

	// Registered only when BOTH are supplied. See CallerID.
	if d.Sync != nil && d.CallerID != nil {
		mux.HandleFunc("GET /sync", syncHandler(d))
	}

	return requestLogger(d.Logger, mux)
}

// syncHandler serves the reconnect and catch-up read.
//
// `after` is the client's cursor and defaults to 0, which is "I have nothing" —
// a fresh client and one that has lost its state are the same request. `limit`
// defaults and is bounded by the store, so a caller cannot ask for an unbounded
// read on a shared Postgres.
func syncHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		viewer, ok := d.CallerID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}

		// A MALFORMED QUERY STRING IS NOT A wire ErrorCode, and this body is
		// deliberately not a ServerError. The enum has no member for "your
		// query string is unparseable" — the closest, `internal`, would be a
		// lie about whose fault it is, and adding one is a wire change under
		// CANT-74's unresolved compatibility policy. An HTTP 400 with a plain
		// body says the true thing without touching the contract.
		after, err := intParam(r, "after", 0)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "after must be a non-negative integer"})
			return
		}
		limit, err := intParam(r, "limit", 0)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "limit must be a non-negative integer"})
			return
		}

		page, err := d.Sync(r.Context(), viewer, after, int(limit))
		if err != nil {
			// The ids are what a reader needs to find the request again. No
			// message body reaches a log line, at any level.
			d.Logger.ErrorContext(r.Context(), "sync failed",
				"viewer_id", viewer, "after", after, "error", err)
			// THE CODE COMES FROM THE STORE, never from here. store.SendErrorFor
			// is the one door the CANT-83 guard leaves open, and it caught this
			// line writing "internal" as a literal — which is the second
			// decision about a code that the guard exists to forbid, arriving
			// in exactly the transport it was written for.
			//
			// THE BODY IS A wire.ServerError AND NOT THE store.SendError. Handing
			// the store type to the encoder emitted its Go field names —
			// `{"Code":"internal","Retryable":false,"RetryAfterSec":null,
			// "Cause":{}}` — which is PascalCase, carries no `type` tag, and
			// leaks the cause. ServerError is `additionalProperties: false`, so
			// every generated client would refuse to decode that: the one shape
			// the schema exists to guarantee, absent from the one response that
			// most needs a client to understand it. Translating here is what
			// makes SendError a store type rather than a wire type wearing the
			// wrong name.
			writeJSON(w, http.StatusInternalServerError, serverError(err, "sync failed"))
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}

// serverError is the 500 body: whatever the store refused with, as the frame
// the schema promises.
//
// errors.As rather than a type assertion, because the store wraps. The
// TRANSLATION ITSELF lives on store.SendError — the code is CANT-83's decision
// and building the frame here would mean naming a fallback code in this
// package, which is exactly the second decision the guard forbids. So this
// reads "who refused" and the store answers "what the client is told".
//
// A nil se is unreachable — SendErrorFor classifies every non-nil error — and
// Wire is nil-safe anyway rather than this relying on that.
func serverError(err error, message string) wire.ServerError {
	var se *store.SendError
	_ = errors.As(store.SendErrorFor(err), &se)
	return se.Wire(message)
}

// intParam reads a non-negative integer query parameter. Absent is the default;
// present-but-unparseable is an ERROR rather than the default, because a client
// sending ?after=abc has a bug and silently rewinding it to 0 would replay the
// entire log at them.
func intParam(r *http.Request, name string, def int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("api: %s %q is not a non-negative integer", name, raw)
	}
	return v, nil
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

// Unwrap and Hijack exist for E2's WebSocket upgrade, which does not exist yet.
//
// Embedding http.ResponseWriter satisfies exactly that interface and nothing
// else, and EVERY request passes through this wrapper. Both websocket libraries
// begin with `w.(http.Hijacker)`, and http.NewResponseController needs Unwrap to
// follow — so without these, the first upgrade handler added under this router
// fails at runtime with "does not implement http.Hijacker", two files away from
// the middleware that caused it. CANT-22 is Mode C; that hour should not be
// spent on this.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("api: %T does not implement http.Hijacker", s.ResponseWriter)
	}
	return h.Hijack()
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
