package main

import (
	"errors"
	"fmt"
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

	// Cohort is which client runs each account (CANT-153): "go", the
	// internal/client reference; "ts", the TypeScript transport through its
	// Node driver; or "mixed", the two alternating by index, so N splits
	// evenly and both halves share the same room and one Compare. Empty is
	// "go".
	Cohort string
	// TSDriver is the built driver bundle, driver.js. Empty builds it from the
	// repo root on demand (`npm run build:driver` in web/), the way an empty
	// CatenaryBin builds the server. Node is the node binary; empty is "node".
	TSDriver string
	Node     string

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
	//   debugRejectAllSends    sends steady traffic into a conversation NONE
	//                          of the clients belong to, so the real server
	//                          genuinely refuses every one of them. Proves
	//                          classify's other server-failure path: a
	//                          comparison can be clean (nothing either side
	//                          disagrees about) while the traffic that
	//                          mattered never landed at all.
	//   debugSendAfterRestart  keeps the kill phase's background senders going
	//                          this long AFTER the restart, so sends are acked
	//                          by the new server right up to the final settle.
	//                          Not a failure, the opposite: proves a live
	//                          fan-out still in flight at compare time is not
	//                          reported as loss (CANT-172).
	debugFaults           map[int]client.Faults
	debugFailProvision    map[int]bool
	debugSkipCompareFor   map[int]bool
	debugRejectAllSends   bool
	debugSendAfterRestart time.Duration
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
	switch c.Cohort {
	case "":
		c.Cohort = cohortGo
	case cohortGo, cohortTS, cohortMixed:
	default:
		return fmt.Errorf("cohort %q: want go, ts or mixed", c.Cohort)
	}
	return nil
}

// The three cohorts -cohort names.
const (
	cohortGo    = "go"
	cohortTS    = "ts"
	cohortMixed = "mixed"
)

// cohortOf is which client runs account i. Mixed alternates, so any N splits
// as evenly as it can and neither half is the one the kill phase's messages
// happen to be authored by.
func (c *Config) cohortOf(i int) string {
	switch c.Cohort {
	case cohortTS:
		return cohortTS
	case cohortMixed:
		if i%2 == 1 {
			return cohortTS
		}
	}
	return cohortGo
}

// needsTS is whether any account runs the TypeScript driver.
func (c *Config) needsTS() bool { return c.Cohort == cohortTS || c.Cohort == cohortMixed }

func (c *Config) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}
