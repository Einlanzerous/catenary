// Command catenary is Catenary's single static binary.
//
// CANT-17 ships `version` and `serve` with the two probes; CANT-13 adds
// `migrate` and the store behind them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/api"
	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/hub"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
	"github.com/magos/catenary/internal/wireview"
)

// version is stamped at build time with -ldflags "-X main.version=...".
//
// It defaults to EMPTY, not to "dev": an -X flag passed with an empty value
// overwrites whatever default is written here, so the fallback has to live in
// code rather than in the variable. buildVersion() is the only reader.
var version = ""

// commit is the full 40-char git SHA, stamped the same way and reported
// verbatim by /healthz, so what is deployed can be compared to what was meant.
var commit = ""

// dbConnectBudget is how long boot waits for Postgres. Generous, because the
// shared container restarting is an ordinary event and crash-looping through
// it severs every open WebSocket on every retry.
const dbConnectBudget = 60 * time.Second

func buildVersion() string {
	if version == "" {
		return "dev"
	}
	return version
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "catenary: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no subcommand given")
	}

	switch args[0] {
	case "version":
		fmt.Println(buildVersion())
		if commit != "" {
			fmt.Println(commit)
		}
		return nil
	case "serve":
		return runServe(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() { fmt.Fprint(os.Stderr, usageText()) }

// usageText is the help text, and the only documentation of the environment
// surface. Split out from usage() so a test can assert it names every variable
// config.Load reads — env-only configuration is documented or it is folklore.
func usageText() string {
	return fmt.Sprintf(`catenary — self-hosted chat for a small trusted group

usage:
  catenary serve            run the HTTP and WebSocket server
  catenary migrate up       apply pending migrations
  catenary migrate down [n] roll back the n newest (default 1; 0 = all)
  catenary migrate status   list applied versions
  catenary version          print the build version and commit

Migrations are embedded and applied automatically on serve. The subcommand
exists for the cases where that is the wrong moment: a rollback, and looking.

configuration is env-only, CATENARY_-prefixed. There are no config files.

  CATENARY_DATABASE_URL    pgx DSN (falls back to DATABASE_URL). Required.
  CATENARY_PORT            listen port. Default 4012.
  CATENARY_LOG_LEVEL       debug | info | warn | error. Default info.
  CATENARY_LOG_FORMAT      json | text. Default json.
  CATENARY_SHUTDOWN_GRACE  how long in-flight work has on SIGTERM. Default 20s.
  CATENARY_MAX_MESSAGE_BYTES  UTF-8 byte bound on a message body. Default 16384.
  CATENARY_MAX_ATTACHMENTS    how many attachments one send may carry. Default 16, at most %d.
  CATENARY_HEARTBEAT_INTERVAL_SEC  how often ready tells the client to ping. Default 35, 5-90.
  CATENARY_MISSED_PONG_LIMIT       missed pings before severance, both ends. Default 2, at least 1.
`, store.MaxAttachmentsCeiling)
}

// serveSync reads one page and maps it, which is the whole of GET /sync's body.
//
// It lives at the composition root rather than in either package because it is
// the only place that legitimately knows about both: the store returns rows and
// wireview turns rows into wire types, and neither should import the other's
// transport concerns.
//
// MediaURL is mediaURL below, the one deriver both transports share.
func serveSync(ctx context.Context, st *store.Store, viewer uuid.UUID, after int64, limit int) (wire.SyncResponse, error) {
	page, err := st.Sync(ctx, viewer, after, limit)
	if err != nil {
		return wire.SyncResponse{}, err
	}
	// NO READ-STATE MAP HERE ANY MORE. This used to reconstruct read_seq from
	// first_unread_seq and hand it to the mapper — an inversion that is lossy
	// in exactly the case the derivation exists for, since first_unread_seq
	// skips the reader's own messages. wireview.Sync takes it off the page's
	// own conversation rows now, so there is nothing left for this function to
	// get wrong. CANT-26.
	return wireview.Sync(page, wireview.SyncViewer{
		UserID:   viewer,
		MediaURL: mediaURL,
	}, store.ServerTime().Format(wireview.TimeLayout)), nil
}

// mediaURL derives a served URL from an opaque storage key, and it is the
// identity for now. CANT-47 decides whether a served url is a presigned R2 GET
// or a Traefik route, and until it does the honest thing is to hand back the
// storage key rather than invent a URL shape that would be wrong the moment
// that lands.
//
// ONE FUNCTION FOR BOTH TRANSPORTS. /sync's page and the hub's `message`
// frame are built by the same wireview.Message, and this is the one input to
// it that the composition root supplies; two derivers here would be the one
// way the socket's Message and REST's could still differ (CANT-84).
func mediaURL(storageKey string) string { return storageKey }

// sessionConn adapts *api.Session to hub.Conn. The session's UserID and
// DeviceID are exported FIELDS, and Go refuses a method of the same name on
// the same type, so the accessors live here — at the one place that knows
// both types — rather than on the session.
type sessionConn struct{ s *api.Session }

func (c sessionConn) SessionID() uuid.UUID { return c.s.ID }
func (c sessionConn) UserID() uuid.UUID    { return c.s.UserID }
func (c sessionConn) DeviceID() uuid.UUID  { return c.s.DeviceID }
func (c sessionConn) Send(ctx context.Context, f wire.ServerFrame) error {
	return c.s.Send(ctx, f)
}
func (c sessionConn) Close(status websocket.StatusCode, reason string) error {
	return c.s.Close(status, reason)
}
func (c sessionConn) CloseNow() error { return c.s.CloseNow() }

// deps is what setup() produces: everything the process needs to serve, built
// once and owned by runServe.
type deps struct {
	cfg    config.Config
	logger *slog.Logger
	store  *store.Store
	router http.Handler

	// hub and listener exist exactly when store does. setup constructs the
	// listener and does not run it: serve runs it, and so does a test, on a
	// context it owns.
	hub      *hub.Hub
	listener *store.Listener[store.NotifyPayload]

	// revocations is the SECOND listener, on its own channel and its own
	// payload (CANT-28 ruling 7, CANT-30). Two channels rather than one
	// discriminated shape, because a revocation and a message decode
	// DIFFERENTLY on a parse failure rather than erroring — notify.go carries
	// the whole argument. Run and stopped exactly as `listener` is.
	revocations *store.Listener[store.RevocationPayload]
}

// setup is the composition root. Every dependency is constructed here and
// passed down; nothing below reaches for a global or reads the environment
// again. That is what makes the wiring readable in one screen and testable
// without a process.
//
// st may be nil, which is what the composition-root test uses: the router is
// buildable without a database, and /readyz reports ready rather than lying
// about a dependency that was never wired.
func setup(cfg config.Config, logger *slog.Logger, st *store.Store) deps {
	var db api.Pinger
	if st != nil {
		db = st
	}
	// CANT-20 left this comment saying CallerID was deliberately unwired —
	// "turns on the moment authentication does". CANT-28 is that moment, so all
	// three now hang off the same condition: a store.
	//
	// GET /sync IS REGISTERED IN A REAL PROCESS FOR THE FIRST TIME HERE. Not
	// behind a header that would quietly have become the auth scheme, which is
	// what CANT-20 refused to ship: behind store.Authenticate, the one seam
	// where an unresolvable credential, an expired one, a revoked device and a
	// deactivated account are all refused.
	var syncFn func(context.Context, uuid.UUID, int64, int) (wire.SyncResponse, error)
	var callerID func(*http.Request) (uuid.UUID, bool)
	var enrollFn func(context.Context, string, string) (store.Enrollment, error)
	// CANT-22: the socket's three seams, all off the same condition. The
	// upgrade calls the SAME Authenticate the REST adapter above calls — one
	// seam, two transports, so a revoked device is refused at both doors by
	// one query.
	var authenticateFn func(context.Context, string) (store.Caller, error)
	var helloFn func(context.Context, store.HelloRequest) (store.HelloResult, error)
	var sendFn func(context.Context, store.NewMessage) (store.Sent, error)
	// CANT-75: the two REST routes' own seams. callerFn answers "who is
	// asking, in full" — callerID below is now built from it, rather than
	// each running its own bearer/Authenticate pair, so there is one place
	// that resolves the header into a caller and two views onto the result.
	var callerFn func(*http.Request) (store.Caller, bool)
	var messageForFanoutFn func(context.Context, uuid.UUID, int64) (store.FanoutMessage, error)
	var findOrCreateDirectFn func(context.Context, uuid.UUID, string) (store.ConversationRow, error)
	var maxFrameBytes int64
	var attachFn func(*api.Session) func()
	var handleFn func(context.Context, *api.Session, wire.ClientFrame)
	var h *hub.Hub
	var listener *store.Listener[store.NotifyPayload]
	var revocations *store.Listener[store.RevocationPayload]
	if st != nil {
		syncFn = func(ctx context.Context, viewer uuid.UUID, after int64, limit int) (wire.SyncResponse, error) {
			return serveSync(ctx, st, viewer, after, limit)
		}
		callerFn = func(r *http.Request) (store.Caller, bool) {
			caller, err := st.Authenticate(r.Context(), bearer(r))
			if err != nil {
				return store.Caller{}, false
			}
			return caller, true
		}
		callerID = func(r *http.Request) (uuid.UUID, bool) {
			caller, ok := callerFn(r)
			if !ok {
				return uuid.Nil, false
			}
			return caller.UserID, true
		}
		enrollFn = st.RedeemEnrollment
		authenticateFn = st.Authenticate
		helloFn = st.Hello
		sendFn = st.SendMessage
		messageForFanoutFn = st.MessageForFanout
		findOrCreateDirectFn = st.FindOrCreateDirect
		// The frame bound follows the message bound, MULTIPLICATIVELY. The
		// store bounds the UTF-8 bytes of `text`; the socket sees that text
		// JSON-encoded, and encoding is not free: a control character
		// becomes `\u00XX`, six bytes for one, and no encoder in any of the
		// three clients does worse. So a message the store would accept
		// occupies at most six times its text on the wire, and the 64 KiB
		// on top covers the ids, the reply reference and the JSON around
		// them. The two must hold together: a frame refused here is severed
		// with 1009 before the store can answer `message_too_large`, and a
		// client whose outbox retries it is a client in a reconnect loop.
		//
		// REST SHARES THIS BOUND (CANT-75's review): a fixed constant on the
		// REST side, independent of this one, let an operator raise
		// CATENARY_MAX_MESSAGE_BYTES and have a message the store would have
		// accepted refused by the REST transport instead — the 400 would have
		// read as a client's JSON bug and been nothing of the kind. One
		// number, both transports.
		maxFrameBytes = 6*int64(st.Limits().MaxMessageBytes) + 64<<10

		// CANT-107: the hub, and the listener that feeds it. The hub's two
		// seams on the door are its own methods over the sessionConn
		// adapter; the listener carries message notifications and nothing
		// else (the revocation channel is CANT-30's to consume).
		h = hub.New(st, logger, mediaURL)
		attachFn = func(s *api.Session) func() { return h.Attach(sessionConn{s}) }
		handleFn = func(ctx context.Context, s *api.Session, f wire.ClientFrame) {
			h.Handle(ctx, sessionConn{s}, f)
		}
		listener = &store.Listener[store.NotifyPayload]{
			DSN: cfg.DatabaseURL, Channel: store.NotifyChannel, Logger: logger,
			OnNotify: h.OnNotify, OnGap: h.OnGap,
		}
		// CANT-30. Its own connection, as every Listener has: LISTEN registers
		// against a session, so two channels on one connection would share a
		// reconnect and a gap between two consumers whose gaps mean different
		// things — one resyncs from a cursor, and this one has no cursor to
		// resync from.
		revocations = &store.Listener[store.RevocationPayload]{
			DSN: cfg.DatabaseURL, Channel: store.RevocationChannel, Logger: logger,
			OnNotify: h.OnRevocation, OnGap: h.OnRevocationGap,
		}
	}

	return deps{
		cfg:         cfg,
		logger:      logger,
		store:       st,
		hub:         h,
		listener:    listener,
		revocations: revocations,
		router: api.NewRouter(api.Deps{
			Logger:   logger,
			DB:       db,
			Sync:     syncFn,
			CallerID: callerID,
			Enroll:   enrollFn,
			Version:  buildVersion(),
			Commit:   commit,

			Authenticate:  authenticateFn,
			Hello:         helloFn,
			Send:          sendFn,
			MaxFrameBytes: maxFrameBytes,
			Attach:        attachFn,
			Handle:        handleFn,

			// CANT-23: validated by heartbeatBoundsFrom in runServe before
			// setup ever runs. api.Deps merges zero with its own defaults,
			// the same convention MaxFrameBytes above already follows.
			HeartbeatIntervalSec: cfg.HeartbeatIntervalSec,
			MissedPongLimit:      cfg.MissedPongLimit,

			Caller:             callerFn,
			MessageForFanout:   messageForFanoutFn,
			FindOrCreateDirect: findOrCreateDirectFn,
			MediaURL:           mediaURL,
			MaxRESTBodyBytes:   maxFrameBytes,
		}),
	}
}

// bearer pulls the access token off a REST request.
//
// `Authorization: Bearer` AND NOT THE SUBPROTOCOL HEADER, and the two are not
// in tension. CANT-28 ruling 1 settles where the credential rides on the
// WEBSOCKET UPGRADE, where a browser can set exactly one header and it is not
// this one. REST has no such constraint in either client, so it uses the
// conventional header — and CANT-22's upgrade will read the same token out of
// `Sec-WebSocket-Protocol` and hand it to the same Authenticate.
//
// Nothing here reads a query parameter, and that is the decision rather than
// an omission: Traefik and Cloudflare log request lines, so a credential in
// one would be written into two access logs on every call.
func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return h[len(prefix):]
}

// limitsFrom merges the two bound variables over the store's defaults and
// validates the result.
//
// Unset variables leave DefaultLimits alone, which is why config carries 0
// rather than a second copy of the numbers. Validation is store.Limits.Validate
// — the same check New panics on — so there is one ceiling and one place it is
// enforced (CANT-85). What this adds is the NAME: the store knows only a
// Limits field, and an operator needs the variable they set.
func limitsFrom(cfg config.Config) (store.Limits, error) {
	limits := store.DefaultLimits()
	if cfg.MaxMessageBytes > 0 {
		limits.MaxMessageBytes = cfg.MaxMessageBytes
	}
	if cfg.MaxAttachments > 0 {
		limits.MaxAttachments = cfg.MaxAttachments
	}
	if err := limits.Validate(); err != nil {
		var le *store.LimitError
		if errors.As(err, &le) {
			if name, ok := limitVariables[le.Field]; ok {
				return store.Limits{}, fmt.Errorf("config: %s: %w", name, err)
			}
		}
		return store.Limits{}, err
	}
	return limits, nil
}

// limitVariables names the environment variable behind each Limits field.
var limitVariables = map[string]string{
	"MaxMessageBytes": "CATENARY_MAX_MESSAGE_BYTES",
	"MaxAttachments":  "CATENARY_MAX_ATTACHMENTS",
}

// missedPongLimitCeiling bounds CATENARY_MISSED_PONG_LIMIT, which the wire
// schema itself leaves unbounded above (ServerReady.missed_pong_limit has no
// maximum). Unbounded is not survivable here: heartbeatWindow
// (internal/api/socket.go) multiplies it by up to 90 seconds in int64
// nanoseconds, and a limit north of roughly 10^8 overflows into a NEGATIVE
// duration — time.AfterFunc fires a negative
// duration at once, so every session would be severed with 4000 the instant
// `ready` goes out, silently, behind a green /readyz. 1000 is nowhere near
// that edge (90 s × 1001 is already a ~25-hour severance window, absurd on
// its own merits) and nowhere near any real deployment either; it exists
// purely so this function's own job — a value `ready` announces must be one
// this server can actually enforce — holds at the boundary too.
const missedPongLimitCeiling = 1000

// heartbeatBoundsFrom validates an operator's CATENARY_HEARTBEAT_INTERVAL_SEC
// / CATENARY_MISSED_PONG_LIMIT against the wire schema's own bounds on
// ServerReady (interval [5, 90], limit >= 1 — and missedPongLimitCeiling
// above, which the schema leaves to us) — CANT-23: a number `ready`
// announces has to be a number its own generated decoders would accept AND
// a number this server can actually enforce, and this is the one place
// either is checked, called before anything slow starts, the same way
// limitsFrom is the one place a send bound is.
//
// UNSET (ZERO) ALWAYS PASSES. config.Load's positiveInt has already floored
// a SET value at 1; api.Deps merges 0 with DefaultHeartbeatIntervalSec /
// DefaultMissedPongLimit itself, so there is nothing here for an unset
// variable to fail.
func heartbeatBoundsFrom(cfg config.Config) error {
	if v := cfg.HeartbeatIntervalSec; v != 0 && (v < 5 || v > 90) {
		return fmt.Errorf("config: CATENARY_HEARTBEAT_INTERVAL_SEC %d is outside the wire schema's bounds [5, 90]", v)
	}
	if v := cfg.MissedPongLimit; v != 0 && v < 1 {
		// Unreachable behind config.Load's positiveInt, which already floors
		// a set value at 1 — kept so a future change to that floor cannot
		// silently let `ready` announce a limit below the wire schema's own.
		return fmt.Errorf("config: CATENARY_MISSED_PONG_LIMIT %d must be at least 1", v)
	}
	if v := cfg.MissedPongLimit; v > missedPongLimitCeiling {
		return fmt.Errorf("config: CATENARY_MISSED_PONG_LIMIT %d is above %d, the most heartbeatWindow can enforce without overflowing", v, missedPongLimitCeiling)
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := cfg.Logger(os.Stdout)

	// The store is handed its bounds; it does not read the environment.
	// Validated HERE, before anything slow starts, so a bound out of range is a
	// startup error naming the variable rather than a panic out of store.New
	// after a sixty-second database wait.
	limits, err := limitsFrom(cfg)
	if err != nil {
		return err
	}
	if err := heartbeatBoundsFrom(cfg); err != nil {
		return err
	}

	// Signals are trapped before anything slow starts. A SIGTERM arriving
	// during a migration or while waiting on a restarting Postgres would
	// otherwise kill the process outright, which on a slow boot is exactly
	// when a deploy sends one.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.ConnectWithRetry(ctx, cfg.DatabaseURL, dbConnectBudget)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Applied on boot, in-process. Idempotent, so this is a no-op on every
	// restart after the first.
	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}

	d := setup(cfg, logger, store.New(pool, limits, logger))

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return serve(ctx, d, ln)
}

// serve runs the process until ctx is cancelled, then drains it.
//
// SPLIT FROM runServe FOR ONE REASON: signal.NotifyContext cancels ctx on
// SIGTERM, and a test that cancels the same context proves the same shutdown
// path without sending a process signal. Everything that needs the world —
// config, signals, the pool — stays in runServe.
//
// THE DRAIN. http.Server.Shutdown does not track hijacked connections, so on
// its own every socket would die with the process by TCP reset. The hub
// holds the set, and closes each with 1001 so the client reconnects
// deliberately. The two run CONCURRENTLY under one grace deadline: Shutdown
// returns only when REST is quiet, and one slow request would otherwise hand
// the drain a deadline already spent — the outcome the drain exists to
// replace. The listener stops LAST, after both: stopped first, it would
// leave attached sessions with a healthy-looking socket and no delivery
// behind it for the length of the drain. An upgrade that races the listen
// socket closing attaches into a hub that has begun shutting down and is
// closed 1001 there.
func serve(ctx context.Context, d deps, ln net.Listener) error {
	srv := &http.Server{
		Handler: d.router,

		// ReadHeaderTimeout only. A read or write deadline on the whole
		// request would sever the WebSocket upgrade E2 hangs off this same
		// server, and R1 measured sockets held for 1h20m on purpose.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The listener runs on its OWN context, not ctx: it must outlive the
	// signal by the length of the drain, and be cancelled by this function
	// rather than by the signal.
	listenerCtx, stopListener := context.WithCancel(context.Background())
	defer stopListener()
	listenerDone := make(chan struct{})
	if d.listener != nil {
		go func() {
			defer close(listenerDone)
			_ = d.listener.Run(listenerCtx)
		}()
	} else {
		close(listenerDone)
	}
	// CANT-30's second listener, on the SAME context and the same terms. It
	// must outlive the signal by the length of the drain for the same reason
	// the first does, and more sharply: a revocation raised while sessions are
	// still being served is still a session that has to be severed, and the
	// drain is exactly when an operator is most likely to be revoking things.
	revocationsDone := make(chan struct{})
	if d.revocations != nil {
		go func() {
			defer close(revocationsDone)
			_ = d.revocations.Run(listenerCtx)
		}()
	} else {
		close(revocationsDone)
	}

	errCh := make(chan error, 1)
	go func() {
		d.logger.Info("listening",
			"addr", ln.Addr().String(),
			"version", buildVersion(),
			"log_format", d.cfg.LogFormat,
		)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	d.logger.Info("shutting down", "grace", d.cfg.ShutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), d.cfg.ShutdownGrace)
	defer cancel()

	var wg sync.WaitGroup
	var srvErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		srvErr = srv.Shutdown(shutdownCtx)
	}()
	if d.hub != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A drain that timed out is logged by the hub; the sessions it
			// did not reach die with the process, as every session did
			// before there was a hub.
			_ = d.hub.Shutdown(shutdownCtx)
		}()
	}
	wg.Wait()

	stopListener()
	<-listenerDone
	<-revocationsDone

	if srvErr != nil {
		return fmt.Errorf("shutdown: %w", srvErr)
	}
	d.logger.Info("stopped")
	return nil
}

// runMigrate is the manual half. `serve` migrates on boot, so `up` here is for
// running the schema forward without starting a server; `down` is the only way
// to go backwards and is deliberately not something boot can do by accident.
func runMigrate(args []string) error {
	if len(args) == 0 {
		return errors.New("migrate: expected up, down or status")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := cfg.Logger(os.Stdout)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.ConnectWithRetry(ctx, cfg.DatabaseURL, dbConnectBudget)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch args[0] {
	case "up":
		if err := store.Migrate(ctx, pool); err != nil {
			return err
		}
		logger.Info("migrations applied")
		return nil

	case "down":
		// Default 1, not all. `migrate down` with no argument is a thing
		// somebody types at 2am; rolling the whole schema back is not what
		// they meant, and 0003 down drops the log.
		n := 1
		if len(args) > 1 {
			n, err = strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("migrate down: %q is not a number", args[1])
			}
		}
		if err := store.MigrateDown(ctx, pool, n); err != nil {
			return err
		}
		logger.Info("migrations rolled back", "count", scope(n))
		return nil

	case "status":
		applied, err := store.AppliedVersions(ctx, pool)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("no migrations applied")
			return nil
		}
		for _, v := range applied {
			fmt.Println(v)
		}
		return nil

	default:
		return fmt.Errorf("migrate: unknown action %q (want up, down or status)", args[0])
	}
}

func scope(n int) string {
	if n <= 0 {
		return "all"
	}
	return strconv.Itoa(n)
}
