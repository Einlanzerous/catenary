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
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/api"
	"github.com/magos/catenary/internal/config"
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
	return `catenary — self-hosted chat for a small trusted group

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
  CATENARY_MAX_ATTACHMENTS    how many attachments one send may carry. Default 16.
`
}

// serveSync reads one page and maps it, which is the whole of GET /sync's body.
//
// It lives at the composition root rather than in either package because it is
// the only place that legitimately knows about both: the store returns rows and
// wireview turns rows into wire types, and neither should import the other's
// transport concerns.
//
// MediaURL is the identity for now. CANT-47 decides whether a served url is a
// presigned R2 GET or a Traefik route, and until it does the honest thing is to
// hand back the storage key rather than invent a URL shape that would be wrong
// the moment that lands.
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
		MediaURL: func(storageKey string) string { return storageKey },
	}, store.ServerTime().Format(wireview.TimeLayout)), nil
}

// deps is what setup() produces: everything the process needs to serve, built
// once and owned by runServe.
type deps struct {
	cfg    config.Config
	logger *slog.Logger
	store  *store.Store
	router http.Handler
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
	// CANT-20. Sync is wired whenever there is a store; CallerID is NOT, because
	// nothing can answer "who is asking" yet — CANT-22's handshake and CANT-29's
	// refresh rotation own that, both Mode C and both unbuilt.
	//
	// So GET /sync is not registered in a real process today. That is the
	// deliberate outcome: the route exists, is tested end to end against an
	// injected caller, and turns on the moment authentication does — rather
	// than shipping now behind a header that would quietly become the auth
	// scheme.
	var syncFn func(context.Context, uuid.UUID, int64, int) (wire.SyncResponse, error)
	if st != nil {
		syncFn = func(ctx context.Context, viewer uuid.UUID, after int64, limit int) (wire.SyncResponse, error) {
			return serveSync(ctx, st, viewer, after, limit)
		}
	}

	return deps{
		cfg:    cfg,
		logger: logger,
		store:  st,
		router: api.NewRouter(api.Deps{
			Logger:  logger,
			DB:      db,
			Sync:    syncFn,
			Version: buildVersion(),
			Commit:  commit,
		}),
	}
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

	// The store is handed its bounds; it does not read the environment. Unset
	// variables leave DefaultLimits alone, which is why config carries 0 rather
	// than a second copy of the numbers.
	limits := store.DefaultLimits()
	if cfg.MaxMessageBytes > 0 {
		limits.MaxMessageBytes = cfg.MaxMessageBytes
	}
	if cfg.MaxAttachments > 0 {
		limits.MaxAttachments = cfg.MaxAttachments
	}

	d := setup(cfg, logger, store.New(pool, limits, logger))

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: d.router,

		// ReadHeaderTimeout only. A read or write deadline on the whole
		// request would sever the WebSocket upgrade E2 hangs off this same
		// server, and R1 measured sockets held for 1h20m on purpose.
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		d.logger.Info("listening",
			"addr", cfg.Addr,
			"version", buildVersion(),
			"log_format", cfg.LogFormat,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	d.logger.Info("shutting down", "grace", cfg.ShutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
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
