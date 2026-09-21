// Package provision is Catenary's internal-only provisioning surface: the door
// Purser's connector calls to create, look up and deactivate accounts.
//
// CANT-131, row 2 of CANT-33's approved plan (rev 2), rulings 1 and 2.
//
// WHY THIS IS ITS OWN PACKAGE AND NOT THREE ROUTES IN internal/api. Ruling 1
// puts this surface on a SECOND http.Server, on its own net.Listener, on an
// address that no Traefik router and no label points at — and a listener is only
// worth that if the handler behind it serves nothing else. Routes added to
// api.NewRouter would be reachable from the routed port the moment somebody
// wired the same handler twice, and the thing that makes this door safe is
// exactly that there is no such path: `/accounts` does not exist on the main
// listener, and `/sync`, `/ws`, `/enroll`, `/refresh`, `/healthz` and `/readyz`
// do not exist here. Two muxes, two listeners, one credential each, and neither
// can grow into the other by accident.
//
// WHAT THE CREDENTIAL THAT OPENS THIS CAN DO, because it is the reason this
// ticket is read line by line: `ensure` mints a fresh enrollment token for ANY
// person the service already holds, and an enrollment token redeems into a
// device enrolled as them — every conversation they are in, every message in it.
// Re-invite is the only way anybody ever gets a second device, so the surface
// cannot refuse it; that makes CATENARY_PROVISION_TOKEN the most powerful
// credential in the service. Nothing in the database can grant it (ruling 2), so
// no SQL injection and no restored backup can mint one, and rotation is a Signet
// change and a restart of both services.
//
// THREE OPERATIONS, AND NOT R6'S FIVE. The stub Purser was written against
// needed a device-level revoke and a second call to disable an account, because
// an in-memory fake cannot do two writes atomically. A real server can:
// CANT-134's DeactivateUser is one transaction, so the offboard is one request,
// and there are NO device-level operations on this surface at all.
//
// EVERY ANSWER THIS PACKAGE GIVES COMES FROM THE STORE. It decodes, it bounds,
// it maps a sentinel error to a status code, and it encodes — and it decides
// nothing else. `kind = 'person'`, the never-adopt rule, the handle derivation,
// which credentials an offboard revokes and in what order: all of it is
// CANT-130's and CANT-134's, behind the three function seams in Deps, and this
// package cannot reach around them because it holds no pool.
package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
)

// Deps is what the surface needs. Every field is required — see New, which
// panics rather than building a half-wired door.
//
// FUNCTIONS RATHER THAN A *store.Store, on api.Deps' own pattern: the three
// seams are the whole of this package's access to the database, so a reader can
// see at a glance that there is no fourth thing it could do, and the contract
// test can drive every status code without a Postgres.
type Deps struct {
	// Logger receives one line per call. Required.
	Logger *slog.Logger

	// Token is the service credential, already validated by config.Load:
	// present, and at least config.MinProvisionTokenBytes long. It is never
	// logged, never echoed in a response, and never compared other than through
	// door.opens.
	Token string

	// Ensure is store.EnsurePerson: create, or find, or reactivate, and hand
	// back a fresh enrollment token either way. ONE IDEMPOTENT CALL — there is
	// no 409-then-reissue second step, which is what makes Purser's Provision
	// idempotent without the connector holding state.
	Ensure func(ctx context.Context, email, displayName string) (store.EnsuredPerson, error)

	// Lookup is store.PersonByEmail: Purser's Reconcile seam. READ-ONLY, and
	// the store's own tests are the oracle for that — it mints nothing,
	// supersedes nothing and takes no lock, because PRSR-15 says a look-up
	// changes nothing and a connector that reconciled a hundred people would
	// otherwise have invalidated a hundred invitations.
	Lookup func(ctx context.Context, email string) (store.PersonLookup, error)

	// Deactivate is store.DeactivateUser: the offboard, one transaction, all or
	// none, publishing the revocation inside it. Purser's Deprovision means
	// revoke and never delete (PRSR-17).
	Deactivate func(ctx context.Context, userID uuid.UUID) (store.Offboard, error)
}

// Server is the provisioning surface: the door, then the three routes.
//
// It implements http.Handler, so cmd/catenary hands it to an http.Server the
// same way it hands the router to the other one.
type Server struct {
	d   Deps
	mux *http.ServeMux

	// want is the digest of Deps.Token, computed once at New so that no request
	// path ever hashes the configured secret again. Fixed length, which is what
	// makes the comparison in door.go constant-time over EQUAL lengths.
	want []byte

	// fault breaks the credential comparison on purpose, for criterion 9's
	// negative controls. Zero in every Server cmd/catenary builds — nothing
	// outside this package's tests can set it — and PER SERVER rather than a
	// package variable, on store.collisionFault's own reasoning and CANT-115's:
	// a shared mutable test knob is a data race between one test's write and
	// another test's still-running server.
	fault doorFault
}

// New builds the surface.
//
// IT PANICS ON A HALF-WIRED DOOR, and the empty token is the case that matters.
// A Server with Token == "" would hold the digest of the empty string, and
// door.opens would then return true for a request carrying NO credential at all:
// the surface would be wide open, on a listener whose whole safety argument is
// the credential, and every test that presented a token would still pass. So it
// is a panic at composition rather than an error nobody checks — store.New's own
// argument for a non-positive bound, applied to the one wiring mistake here that
// cannot be noticed from the outside. config.Load refuses an empty or short
// token before cmd/catenary ever reaches this, so this cannot fire from a real
// configuration; it fires when something builds a Server by hand.
func New(d Deps) *Server {
	switch {
	case d.Logger == nil:
		panic("provision.New: logger must not be nil (every call to this surface logs once, and a door whose refusals are invisible is a door nobody watches)")
	case d.Token == "":
		panic("provision.New: token must not be empty (the door would then open for a request carrying no credential at all)")
	case d.Ensure == nil, d.Lookup == nil, d.Deactivate == nil:
		panic("provision.New: Ensure, Lookup and Deactivate are all required (a surface missing one of them answers 404 for an operation the connector was told exists)")
	}

	s := &Server{
		d: d,
		// store.HashToken, not a second hash written here. Its own doc comment
		// carries the argument for SHA-256 over a password KDF, and it holds for
		// this credential for the same reason and more strongly: it is 32-plus
		// bytes generated by Signet, so there is no candidate set for a slow hash
		// to slow anybody down over, and the verification runs on every call.
		// What this use adds is only the fixed LENGTH — see door.go.
		want: store.HashToken(d.Token),
		mux:  http.NewServeMux(),
	}

	// THE WHOLE ROUTE TABLE, AND IT IS THE POINT OF THE PACKAGE. Three patterns,
	// each with its method, and no catch-all: a `mux.HandleFunc("/", …)` here
	// would match every path, which would both invent answers for routes this
	// surface does not serve and take the 405 below away (a pattern with no
	// method matches every method, so net/http would stop reporting a method
	// mismatch).
	//
	// THE 405 IS net/http's, NOT OURS. Since Go 1.22 a ServeMux whose registered
	// patterns match a request's PATH but not its METHOD answers 405 with an
	// Allow header, which is exactly what `DELETE /accounts` deserves and is one
	// fewer hand-written branch than writing it here would be.
	s.mux.HandleFunc("POST /accounts", s.ensure)
	s.mux.HandleFunc("GET /accounts", s.lookup)
	s.mux.HandleFunc("POST /accounts/{id}/deactivate", s.deactivate)

	return s
}

// ServeHTTP is the door and then the routes.
//
// NO REQUEST-LOG MIDDLEWARE, DELIBERATELY, and it is not an omission. api's
// requestLogger writes one line per request for the member-facing transport,
// where the interesting fact is method, path and status; here the interesting
// fact is WHICH OPERATION RAN AND WHAT IT DID TO WHICH ACCOUNT, and each handler
// writes that line itself. Wrapping this in a second logger would put two lines
// in the log for every provisioning call and make "one structured line per
// accepted call" false.
//
// A ROUTE THIS LISTENER DOES NOT SERVE IS LOGGED ONCE, HERE, because nothing
// downstream can: the mux's own 404 and 405 handlers write a response and return.
// mux.Handler reports the pattern that matched, and an empty pattern is exactly
// "no registered route served this" — a connector calling `/sync` or
// `/accounts/{id}/revoke` is a version skew worth seeing in Dozzle rather than a
// silent 404.
//
// AND THEN THE DISPATCH GOES THROUGH mux.ServeHTTP, NOT THROUGH THE HANDLER
// mux.Handler JUST RETURNED. Matching twice is deliberate and it is not a
// tidiness question: ServeMux.Handler resolves a handler but does NOT bind the
// request's path WILDCARDS, so a handler invoked that way reads "" from
// r.PathValue("id") — every deactivate would answer 404 for a perfectly good id,
// and it would do so only in production, since a test that called the handler
// directly would have the same hole. Two tree lookups on a surface that serves a
// handful of calls a day is not a cost worth thinking about.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	if _, pattern := s.mux.Handler(r); pattern == "" {
		s.d.Logger.WarnContext(r.Context(), "provisioning call for a route this listener does not serve",
			"method", r.Method, "path", r.URL.Path)
	}
	s.mux.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Bodies

// maxRequestBody bounds what a caller may post.
//
// The largest legal body on this surface is an email and a display name, and
// 4 KiB is the bound api's own unauthenticated credential routes use for the
// same shape of request. It is deliberately NOT derived from
// CATENARY_MAX_MESSAGE_BYTES the way the REST send bound is: nothing here
// carries a message, so there is no bound the store would disagree with, and a
// provisioning surface that read a megabyte before deciding it did not like it
// would be doing so on behalf of the one credential that can empty the service.
const maxRequestBody = 4 << 10

// decodeStrict reads one JSON object and refuses everything else.
//
// DisallowUnknownFields IS THE POINT. PRSR-50 pins this contract by tag, so the
// connector and this surface agree on a field set — and a field the connector
// sends that this surface silently ignores is the failure that agreement exists
// to prevent: a `handle` or a `role` in the request body would read, at the
// Purser end, as something this service honoured. It does not honour them, and
// now it says so.
//
// dec.More() catches the second half: a body of `{}{}` decodes its first object
// happily and would leave the rest unread.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("provision: request body carries more than one JSON value")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Responses

// errorBody is every non-2xx answer this surface writes, and the messages are
// a CLOSED SET listed in provision/openapi.yaml.
//
// NOT A wire.ServerError, and that is a decision rather than laziness. The wire
// schema is the contract between this server and its three generated clients,
// and no client ever calls this surface — putting these three operations in it
// would generate TypeScript and Dart for an API a client may not touch (the
// plan's own tradeoff). So this surface has its own contract file and its own
// error shape, and `{"error": "…"}` is that shape everywhere, including the
// places api would have written `{"code": "unauthorized"}`.
type errorBody struct {
	Error string `json:"error"`
}

// The closed set. Each of these is in the contract file; the contract test
// fails if a handler writes a body whose fields the file does not describe.
const (
	// errUnauthorized is the ONE refusal the door writes, byte-identical for a
	// missing credential, a malformed header and a wrong token. See door.go.
	errUnauthorized = "unauthorized"

	// errNoSuchAccount is the ONE not-found, byte-identical for an unknown id,
	// a bot's id and an id that is not a UUID at all. See deactivate.
	errNoSuchAccount = "no such account"

	errBadRequest   = "request is not valid"
	errInvalidEmail = "email is not a valid address"
	errEmailMissing = "the email query parameter is required"
	errInternal     = "internal error"
)

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, errorBody{Error: message})
}

// ---------------------------------------------------------------------------
// Errors that are this package's own

// describeDecodeFailure names the one decode failure worth telling apart in the
// log: http.MaxBytesReader's error, which means the caller sent more than
// maxRequestBody rather than sending something unparseable.
func describeDecodeFailure(err error) string {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fmt.Sprintf("body over %d bytes", maxRequestBody)
	}
	return "unparseable body"
}
