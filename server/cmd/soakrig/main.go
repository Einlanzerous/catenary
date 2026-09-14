// Command soakrig is CANT-27's soak and chaos harness: N internal/client
// clients against a real, separate `catenary serve` process, run through
// steady traffic, a reconnect storm and a `kill -9` and restart, then
// compared at the end against the server's own committed log.
//
// It replaces the R1 spike's server/cmd/r1rig and server/cmd/r1client
// (spike/r1-websocket/FINDINGS.md). Those exist to prove the WebSocket
// architecture could work at all — an in-memory rig, ~400 lines, disposable.
// This one runs the real service, at scale, repeatedly, and it is built on
// the reusable client CANT-102 shipped rather than a second one grown
// alongside it.
//
// Two modes:
//
//	soak   the ticket's Done-when. Builds (or takes) a catenary binary, starts
//	       it as a separate OS process against a Postgres database this
//	       process ALSO reads directly, provisions N accounts through the
//	       real POST /enroll path, runs three phases — steady traffic, a
//	       reconnect storm, a kill -9 and restart under continuing traffic —
//	       and compares every client's journal against the server's own
//	       committed log with client.Compare. This is CANT-27.
//
//	idle   CANT-23's remaining clause: one client, no local server, held open
//	       and pinging against a base URL and a real device token an operator
//	       supplies. This process never chooses where to point itself — that
//	       choice, and running it against anything deployed, is a person's
//	       act.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "soak":
		return runSoakCmd(args[1:])
	case "provision":
		return runProvisionCmd(args[1:])
	case "idle":
		return runIdleCmd(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		usage()
		fmt.Fprintf(os.Stderr, "soakrig: unknown mode %q\n", args[0])
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `soakrig — CANT-27's soak and chaos harness, and CANT-109's deployed-path tools

usage:
  soakrig soak [flags]        N clients against a real, local catenary serve:
                               steady traffic, a reconnect storm, a kill -9 and
                               restart, then every client's journal compared
                               against the server's committed log.

  soakrig provision [flags]   create ONE new account and device against a
                               database this process reaches directly, and
                               redeem its enrollment token over the real
                               POST /enroll at -base-url — refuses an existing
                               handle, and writes the credential file
                               'soakrig idle' reads. CANT-109's one-off.

  soakrig idle [flags]        one client, no local server: hold a socket open
                               against a base URL and the credential file
                               'provision' wrote, pinging on the cadence
                               'ready' announces. CANT-23's tunnel-idle
                               measurement. This process never points itself
                               at anything — that choice, and running it
                               against a deployed environment, is a person's
                               act.

run 'soakrig soak -h', 'soakrig provision -h' or 'soakrig idle -h' for that mode's flags.
`)
}

// exit codes: 0 pass, 1 harness failure (inconclusive — the instrument, not
// the system, is what the report doubts), 2 usage error, 3 a server failure —
// real loss, duplication, a phantom, a mismatch, a seq conflict or
// out-of-order delivery found by a comparison that DID run. 3 is deliberately
// its own code: CLAUDE.md says a finding like this is a person's to see, not
// something this ticket patches, and a distinct exit code is what lets a
// caller (a person at a terminal, or a future CI job) tell "the harness is
// unsure" from "the harness is sure something is wrong" without parsing the
// report.
const (
	exitPass           = 0
	exitHarnessFailure = 1
	exitUsage          = 2
	exitServerFailure  = 3
)

func runSoakCmd(args []string) int {
	fs := flag.NewFlagSet("soak", flag.ContinueOnError)
	var cfg Config
	var n int
	fs.StringVar(&cfg.DBURL, "db-url", firstNonEmpty(os.Getenv("CATENARY_DATABASE_URL"), os.Getenv("DATABASE_URL")),
		"Postgres URL for BOTH this process's own provisioning/comparison queries AND the server subprocess "+
			"(CATENARY_DATABASE_URL, DATABASE_URL). Point this at a dedicated catenary_soak database — "+
			"never at catenary_test, which other agents' runs share. Required.")
	fs.StringVar(&cfg.CatenaryBin, "catenary-bin", "",
		"path to a prebuilt catenary binary. Empty builds ./cmd/catenary from the repo root on demand.")
	fs.StringVar(&cfg.RepoRoot, "catenary-root", "",
		"the catenary repo root, for the on-demand build. Empty auto-detects from this binary's own source location.")
	fs.IntVar(&n, "n", 20, "number of clients — the ticket's floor is 20")
	fs.DurationVar(&cfg.SteadyDuration, "steady", 10*time.Second, "duration of the steady-traffic phase")
	fs.DurationVar(&cfg.SendInterval, "rate", 300*time.Millisecond, "interval between one client's sends during steady traffic and during the kill phase")
	fs.IntVar(&cfg.StormRounds, "storm-rounds", 3, "sever-then-reconnect rounds in the reconnect-storm phase")
	fs.IntVar(&cfg.KillMessages, "kill-messages", 20, "messages committed directly to the store while the server is dead — R1's 'something to lose'")
	fs.DurationVar(&cfg.AwaitTimeout, "await-timeout", 30*time.Second, "bound on every phase-completion wait (a client reaching ready and caught-up)")
	fs.StringVar(&cfg.ReportPath, "out", "", "also write the JSON report here")
	fs.StringVar(&cfg.ServerLogDir, "server-log-dir", "", "write the server subprocess's stdout/stderr here, one file per instance (across the restart)")
	var quiet bool
	fs.BoolVar(&quiet, "quiet", false, "suppress the harness's own progress logging on stderr (the report on stdout is unaffected)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "soakrig soak — run N clients through steady traffic, a reconnect storm and a kill -9\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg.N = n
	if !quiet {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	if cfg.DBURL == "" {
		fmt.Fprintln(os.Stderr, "soakrig soak: -db-url (or CATENARY_DATABASE_URL / DATABASE_URL) is required")
		fs.Usage()
		return exitUsage
	}
	if cfg.N < 1 {
		fmt.Fprintln(os.Stderr, "soakrig soak: -n must be at least 1")
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	res := runSoak(ctx, cfg)
	printReport(os.Stdout, res.Report)

	switch res.Verdict {
	case VerdictPass:
		return exitPass
	case VerdictServerFailure:
		return exitServerFailure
	default:
		return exitHarnessFailure
	}
}

func runProvisionCmd(args []string) int {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	var dbURL, baseURL, handle, displayName, credFile, cfID, cfSecret string
	fs.StringVar(&dbURL, "db-url", firstNonEmpty(os.Getenv("CATENARY_DATABASE_URL"), os.Getenv("DATABASE_URL")),
		"Postgres URL this process uses directly to create the account (CATENARY_DATABASE_URL, DATABASE_URL). Required.")
	fs.StringVar(&baseURL, "base-url", "", "the server's origin to redeem the enrollment token against, e.g. https://catenary.example.org. Required.")
	fs.StringVar(&handle, "handle", "", "the new account's handle. Refused if it already exists. Required.")
	fs.StringVar(&displayName, "display-name", "", "the new account's display name, and this device's name. Required.")
	fs.StringVar(&credFile, "cred-file", "", "where to write the device id, access token and refresh token, at mode 0600. Required. Never printed.")
	fs.StringVar(&cfID, "cf-access-client-id", os.Getenv("CF_ACCESS_CLIENT_ID"),
		"Cloudflare Access service token client ID, sent as the CF-Access-Client-Id header (env CF_ACCESS_CLIENT_ID)")
	fs.StringVar(&cfSecret, "cf-access-client-secret", os.Getenv("CF_ACCESS_CLIENT_SECRET"),
		"Cloudflare Access service token client secret, sent as the CF-Access-Client-Secret header (env CF_ACCESS_CLIENT_SECRET). Never logged.")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `soakrig provision — CANT-109's one-off device

Creates ONE new account and device against a database this process reaches
directly, through the real POST /enroll path at -base-url — the same path a
real install uses. Refuses a handle that already exists rather than reusing
an account. Writes the device id, access token and refresh token to
-cred-file at mode 0600; nothing secret ever reaches stdout or a log line.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case dbURL == "":
		fmt.Fprintln(os.Stderr, "soakrig provision: -db-url (or CATENARY_DATABASE_URL / DATABASE_URL) is required")
		fs.Usage()
		return exitUsage
	case baseURL == "":
		fmt.Fprintln(os.Stderr, "soakrig provision: -base-url is required")
		fs.Usage()
		return exitUsage
	case handle == "":
		fmt.Fprintln(os.Stderr, "soakrig provision: -handle is required")
		fs.Usage()
		return exitUsage
	case displayName == "":
		fmt.Fprintln(os.Stderr, "soakrig provision: -display-name is required")
		fs.Usage()
		return exitUsage
	case credFile == "":
		fmt.Fprintln(os.Stderr, "soakrig provision: -cred-file is required")
		fs.Usage()
		return exitUsage
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cred, err := runProvision(ctx, dbURL, baseURL, handle, displayName, credFile, cfAccessHeaders(cfID, cfSecret), logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soakrig provision: %v\n", err)
		return exitHarnessFailure
	}
	// NEVER cred.AccessToken or cred.RefreshToken here or anywhere else in
	// this command: user_id and device_id are identifiers, not secrets, and
	// the two tokens live only in -cred-file, at 0600.
	fmt.Fprintf(os.Stderr, "soakrig provision: created %q (user %s, device %s); credentials written to %s\n",
		handle, cred.UserID, cred.DeviceID, credFile)
	return exitPass
}

func runIdleCmd(args []string) int {
	fs := flag.NewFlagSet("idle", flag.ContinueOnError)
	var baseURL, credFile, clientInfo, cfID, cfSecret string
	var runFor time.Duration
	var quiet bool
	fs.StringVar(&baseURL, "base-url", "", "the server's origin, e.g. https://catenary.example.org. Required.")
	fs.StringVar(&credFile, "cred-file", "", "the credential file 'soakrig provision' wrote (device id, access token). Required.")
	fs.StringVar(&clientInfo, "client-info", "cant-23-idle", "sent on the hello, for the server's own log")
	fs.DurationVar(&runFor, "for", time.Hour, "how long to hold the socket open. The measurement passes only if it survives this whole duration on one socket; 0 runs until signalled (Ctrl-C) instead.")
	fs.BoolVar(&quiet, "quiet", false, "suppress the periodic liveness line")
	fs.StringVar(&cfID, "cf-access-client-id", os.Getenv("CF_ACCESS_CLIENT_ID"),
		"Cloudflare Access service token client ID, sent as the CF-Access-Client-Id header (env CF_ACCESS_CLIENT_ID)")
	fs.StringVar(&cfSecret, "cf-access-client-secret", os.Getenv("CF_ACCESS_CLIENT_SECRET"),
		"Cloudflare Access service token client secret, sent as the CF-Access-Client-Secret header (env CF_ACCESS_CLIENT_SECRET). Never logged.")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `soakrig idle — CANT-23's tunnel-idle measurement

Holds ONE socket open against a base URL and the credential file
'soakrig provision' wrote, pinging on the cadence the server's own 'ready'
frame announces, and prints a liveness line every five minutes. Passes only
if that one socket survives the whole -for duration with no reconnect — any
reconnect fails the measurement even when it succeeds, because the claim
under test is that ONE socket survived, not that the client recovered. This
process never runs itself against a deployed environment — running it there
is your act, not this tool's.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if baseURL == "" || credFile == "" {
		fmt.Fprintln(os.Stderr, "soakrig idle: -base-url and -cred-file are both required")
		fs.Usage()
		return exitUsage
	}
	cred, err := readCredentials(credFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soakrig idle: %v\n", err)
		return exitUsage
	}

	var logger *slog.Logger
	if quiet {
		logger = slog.New(slog.DiscardHandler)
	} else {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}

	fmt.Fprintf(os.Stderr, "soakrig idle: holding one socket open against %s for %s — this is YOUR choice to make against a deployed server, not this tool's\n", baseURL, runFor)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rep := runIdle(ctx, baseURL, cred, cfAccessHeaders(cfID, cfSecret), clientInfo, runFor, logger)
	printIdleReport(os.Stderr, rep)
	switch {
	case rep.Pass:
		return exitPass
	case rep.Readys == 0:
		// Never established a session at all — ambiguous why (a bad
		// credential, a network problem reaching -base-url, the server
		// down), the same "the instrument is unsure" case exitHarnessFailure
		// names for `soak`.
		return exitHarnessFailure
	default:
		// Reached ready at least once, then the one socket did not survive:
		// a real finding about the deployed path, the same weight
		// exitServerFailure carries for `soak`.
		return exitServerFailure
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
