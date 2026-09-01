// Command catenary is Catenary's single static binary.
//
// CANT-17 ships `version` and `serve` with the two probes; `migrate` arrives
// with CANT-13, which is what creates the schema there would be to migrate.
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
	"syscall"
	"time"

	"github.com/magos/catenary/internal/api"
	"github.com/magos/catenary/internal/config"
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
  catenary serve     run the HTTP and WebSocket server
  catenary version   print the build version and commit

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
	router http.Handler
}

// setup is the composition root. Every dependency is constructed here and
// passed down; nothing below reaches for a global or reads the environment
// again. That is what makes the wiring readable in one screen and testable
// without a process.
//
// CANT-13 adds the store: it opens the pool from cfg.DatabaseURL and passes it
// as api.Deps.DB, at which point /readyz starts reporting a real dependency
// rather than the absence of one. Until then the DSN is required, validated
// and unopened — required because every later boot needs it and a variable
// that becomes mandatory in a later release is a variable that breaks a deploy.
func setup(cfg config.Config) deps {
	logger := cfg.Logger(os.Stdout)

	return deps{
		cfg:    cfg,
		logger: logger,
		router: api.NewRouter(api.Deps{
			Logger:  logger,
			DB:      nil, // CANT-13
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
	d := setup(cfg)

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: d.router,

		// ReadHeaderTimeout only. A read or write deadline on the whole
		// request would sever the WebSocket upgrade E2 hangs off this same
		// server, and R1 measured sockets held for 1h20m on purpose.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Signals are trapped before the listener opens. A SIGTERM arriving during
	// startup would otherwise kill the process outright rather than being
	// handled, which on a slow boot is exactly when a deploy sends one.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
