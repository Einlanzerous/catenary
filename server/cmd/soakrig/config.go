package main

import (
	"errors"
	"log/slog"
	"time"

	"github.com/magos/catenary/internal/client"
)

// Config is everything one `soak` run needs. Every exported field is settable
// from a flag in main.go; the zero value of every unexported field below is a
// correct, unmodified harness.
type Config struct {
	DBURL       string
	CatenaryBin string
	RepoRoot    string

	N int

	SteadyDuration time.Duration
	SendInterval   time.Duration
	StormRounds    int
	KillMessages   int
	AwaitTimeout   time.Duration

	ReportPath   string
	ServerLogDir string

	// Logger receives the harness's OWN progress narration. Nil discards it.
	// Never handed to a client — see harness.go's newClient, which always
	// gives a client a discarding logger: N clients narrating their own
	// heartbeats is noise the report already summarizes as Stats.
	Logger *slog.Logger

	// debugFaults, debugFailProvision and debugSkipCompareFor exist ONLY for
	// CANT-27's counter-proof tests (broken_test.go), the way client.Faults
	// exists only for TestTheKillTestCatchesABrokenClient: each one plants
	// exactly one failure this harness has to classify correctly, deliberately,
	// so that a report saying "clean" has been watched failing to say that
	// when something really is wrong. No flag reaches these — a real soak run
	// is not a test of the test.
	//
	//   debugFaults[i]         breaks client i's own obligations (CANT-24's
	//                          five), so the FINAL COMPARE — which DOES run —
	//                          reports loss or duplication. Proves the
	//                          server-failure classification: a comparison
	//                          that ran and found dirt is reported as one,
	//                          whatever actually caused the dirt.
	//   debugFailProvision[i]  makes client i's enrollment fail before it ever
	//                          gets a socket. Proves "a client that could not
	//                          provision" is a harness failure, not silently
	//                          dropped from N.
	//   debugSkipCompareFor[i] skips the final Compare for client i, as if the
	//                          comparison step itself had errored. Proves "a
	//                          comparison that never ran" is a harness
	//                          failure and never folds into a clean report.
	debugFaults         map[int]client.Faults
	debugFailProvision  map[int]bool
	debugSkipCompareFor map[int]bool
}

// validate fills in defaults and checks what it can before anything slow
// starts — the same convention cmd/catenary's own heartbeatBoundsFrom and
// limitsFrom follow: a bad config is a startup error, not a panic three
// phases in.
func (c *Config) validate() error {
	if c.DBURL == "" {
		return errors.New("a database URL is required (-db-url)")
	}
	if c.N < 1 {
		return errors.New("N must be at least 1")
	}
	if c.SteadyDuration <= 0 {
		c.SteadyDuration = 10 * time.Second
	}
	if c.SendInterval <= 0 {
		c.SendInterval = 300 * time.Millisecond
	}
	if c.StormRounds < 1 {
		c.StormRounds = 1
	}
	if c.KillMessages < 0 {
		c.KillMessages = 0
	}
	if c.AwaitTimeout <= 0 {
		c.AwaitTimeout = 30 * time.Second
	}
	return nil
}

func (c *Config) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}
