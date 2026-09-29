package main

// CANT-153 — the TypeScript cohort through the soak itself: a clean run, and
// CANT-35 criterion 19's `dedupeByLogSeq` control watched failing in the same
// lane, in the shape of broken_test.go's TestBrokenRunCountsAsServerFailure.
//
// GATED ON TWO THINGS. CATENARY_TEST_DATABASE_URL, as every database test is,
// and CATENARY_TS_DRIVER, the built driver bundle: unset skips, so the
// ordinary `go test ./...` sweep on a machine with no bundle stays green, but
// SET AND MISSING FAILS — a lane asked to run the TypeScript client and handed
// nothing to run must not report itself skipped. verify.sh's CANT-153 step
// builds the bundle and sets the variable.

import (
	"context"
	"os"
	"testing"

	"github.com/magos/catenary/internal/client"
)

// tsDriverScript is the built driver bundle, or a skip.
func tsDriverScript(t *testing.T) string {
	t.Helper()
	p := os.Getenv("CATENARY_TS_DRIVER")
	if p == "" {
		t.Skip("CATENARY_TS_DRIVER not set; skipping the TypeScript cohort (verify.sh builds the driver and sets it)")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("CATENARY_TS_DRIVER=%s: %v — the lane was asked to run the TypeScript driver and there is none; build it with `npm run build:driver` in web/", p, err)
	}
	return p
}

func tsConfig(t *testing.T, cohort string) Config {
	t.Helper()
	script := tsDriverScript(t)
	cfg := tinyConfig(soakDBFixture(t))
	cfg.Cohort, cfg.TSDriver = cohort, script
	return cfg
}

// The control the fault below needs: the same lane, unbroken, passes clean,
// with every client compared and something to lose at the kill.
func TestTSSoakBaselinePasses(t *testing.T) {
	res := runSoak(context.Background(), tsConfig(t, cohortTS))
	t.Log(reportString(res.Report))
	if res.Verdict != VerdictPass {
		t.Fatalf("verdict = %s, want pass", res.Verdict)
	}
	if res.Report.ComparisonsRun != res.Report.N {
		t.Errorf("comparisons run = %d, want %d", res.Report.ComparisonsRun, res.Report.N)
	}
	for _, c := range res.Report.Clients {
		if c.Cohort != cohortTS {
			t.Errorf("client %d ran as %q, want ts", c.Index, c.Cohort)
		}
	}
}

// Both cohorts in one room, judged by one Compare.
func TestMixedSoakBaselinePasses(t *testing.T) {
	cfg := tsConfig(t, cohortMixed)
	cfg.N = 4
	res := runSoak(context.Background(), cfg)
	t.Log(reportString(res.Report))
	if res.Verdict != VerdictPass {
		t.Fatalf("verdict = %s, want pass", res.Verdict)
	}
	n := map[string]int{}
	for _, c := range res.Report.Clients {
		n[c.Cohort]++
	}
	if n[cohortGo] != 2 || n[cohortTS] != 2 {
		t.Errorf("cohorts %v, want two of each", n)
	}
}

// COUNTER-PROOF — criterion 19's `dedupeByLogSeq`, through the soak. The
// TypeScript client 0 counts a record as new exactly when its log_seq is above
// the cursor, so a /sync page re-carrying a message it already held live —
// which the reconnect storm guarantees — counts it twice. Compare ran, and is
// dirty, and the harness says so.
func TestBrokenTSRunCountsAsServerFailure(t *testing.T) {
	cfg := tsConfig(t, cohortTS)
	cfg.debugFaults = map[int]client.Faults{0: {DedupeByLogSeq: true}}
	res := runSoak(context.Background(), cfg)
	t.Log(reportString(res.Report))
	if res.Verdict != VerdictServerFailure {
		t.Fatalf("verdict = %s, want server_failure", res.Verdict)
	}
	if res.Report.ComparisonsRun != res.Report.N {
		t.Errorf("comparisons run = %d, want %d — the fault must not stop the comparison from running", res.Report.ComparisonsRun, res.Report.N)
	}
	for _, c := range res.Report.Clients {
		switch {
		case c.Index == 0 && (!c.Compared || len(c.Compare.Duplicated) == 0):
			t.Errorf("client 0's own comparison reports no duplicate — this test did not exercise what it claims: %s", c.Compare)
		case c.Index != 0 && c.Compared && !c.Compare.Clean():
			t.Errorf("client %d, unbroken, is dirty: %s", c.Index, c.Compare)
		}
	}
}
