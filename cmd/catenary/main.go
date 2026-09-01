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

	"github.com/magos/catenary/internal/api"
	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/store"
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
`
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
	return deps{
		cfg:    cfg,
		logger: logger,
		store:  st,
		router: api.NewRouter(api.Deps{
			Logger:  logger,
			DB:      db,
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

	d := setup(cfg, logger, store.New(pool))

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
