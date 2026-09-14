package main

// CANT-27's counter-proofs: "prove each classification with a deliberately
// broken run" — the out-of-process, harness-level version of what
// TestTheKillTestCatchesABrokenClient does in-process for the kill test
// itself. Each test here runs a REAL, small, fast soak — a real subprocess,
// a real Postgres — with exactly one thing broken, and asserts the harness
// names it correctly: a server failure when a comparison that DID run found
// dirt, a harness failure for everything CLAUDE.md and the ticket list as
// the harness's own (a client that could not provision, a journal — here,
// the report file — write error, a timeout the harness set itself, a
// comparison that never ran).
//
// Gated the same way every other database test in this repository is:
// CATENARY_TEST_DATABASE_URL unset skips rather than fails, and the fixture
// resets the schema the way cmd/catenary's own processFixtureWith does. Small
// N and second-scale phases keep the whole file fast enough to sit inside
// verify.sh's unconditional `go test ./...` for this module.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/store"
)

// soakDBFixture resets a fresh schema on CATENARY_TEST_DATABASE_URL — the
// same MigrateDown(0)+Migrate cycle cmd/catenary's processFixtureWith uses —
// and hands back the DSN. runSoak opens its OWN pool against it, exactly as
// it does against catenary_soak for a real run; this fixture's own pool is
// only for the reset.
func soakDBFixture(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CATENARY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATENARY_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := store.MigrateDown(ctx, pool, 0); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return dsn
}

// tinyConfig is small and fast enough for a test: three clients, second-scale
// phases, and an await bound generous enough for a loaded CI runner.
func tinyConfig(dbURL string) Config {
	return Config{
		DBURL: dbURL, N: 3,
		SteadyDuration: 1500 * time.Millisecond, SendInterval: 150 * time.Millisecond,
		StormRounds: 1, KillMessages: 5, AwaitTimeout: 20 * time.Second,
	}
}

func reportString(rep Report) string {
	var b strings.Builder
	printReport(&b, rep)
	return b.String()
}

// The control every counter-proof below needs: an unmodified, correct, small
// run passes, with every client actually compared. A fault that only
// "works" against a baseline nobody has seen pass proves nothing.
func TestSoakBaselinePasses(t *testing.T) {
	dbURL := soakDBFixture(t)
	res := runSoak(context.Background(), tinyConfig(dbURL))
	if res.Verdict != VerdictPass {
		t.Fatalf("verdict = %s, want pass\n%s", res.Verdict, reportString(res.Report))
	}
	if res.Report.ComparisonsRun != res.Report.N {
		t.Errorf("comparisons run = %d, want %d", res.Report.ComparisonsRun, res.Report.N)
	}
}

// COUNTER-PROOF — a comparison that DID run and found real dirt is reported
// as a server failure, whatever actually caused the dirt. client.Faults
// breaks client 0's OWN bookkeeping (DedupeByLogSeq: a /sync page that
// re-carries a message already held live is counted a second time — and the
// reconnect storm guarantees exactly that page), which is invisible to the
// harness from the outside: Compare ran and is not clean, which is exactly
// what a real server bug would also look like. This is the harness's own
// version of TestTheKillTestCatchesABrokenClient.
func TestBrokenRunCountsAsServerFailure(t *testing.T) {
	dbURL := soakDBFixture(t)
	cfg := tinyConfig(dbURL)
	cfg.debugFaults = map[int]client.Faults{0: {DedupeByLogSeq: true}}
	res := runSoak(context.Background(), cfg)
	if res.Verdict != VerdictServerFailure {
		t.Fatalf("verdict = %s, want server_failure\n%s", res.Verdict, reportString(res.Report))
	}
	if res.Report.ComparisonsRun != res.Report.N {
		t.Errorf("comparisons run = %d, want %d — the fault must not stop the comparison from running", res.Report.ComparisonsRun, res.Report.N)
	}
	dirty := false
	for _, c := range res.Report.Clients {
		if c.Index == 0 && c.Compared && len(c.Compare.Duplicated) > 0 {
			dirty = true
		}
	}
	if !dirty {
		t.Errorf("client 0's own comparison reports no duplicate — this test did not exercise what it claims:\n%s", reportString(res.Report))
	}
}

// COUNTER-PROOF — a server that refuses every send is a server failure even
// though nothing either side holds ever disagrees. debugRejectAllSends
// targets steady traffic at a conversation none of the clients belong to, so
// the REAL server refuses every one of them with a real membership error —
// client.Compare stays clean (there is nothing on either side to compare),
// and classify's OTHER server-failure path — sent > 0, acked == 0 — is what
// has to catch it. Found by review: the first version of classify only ever
// looked at Compare.
func TestAllSendsRefusedCountsAsServerFailure(t *testing.T) {
	dbURL := soakDBFixture(t)
	cfg := tinyConfig(dbURL)
	cfg.debugRejectAllSends = true
	res := runSoak(context.Background(), cfg)
	if res.Verdict != VerdictServerFailure {
		t.Fatalf("verdict = %s, want server_failure\n%s", res.Verdict, reportString(res.Report))
	}
	for _, p := range res.Report.Phases {
		if p.Name == steadyTrafficPhase {
			if p.MessagesSent == 0 {
				t.Fatalf("steady traffic sent 0 — this test proves nothing without an attempt:\n%s", reportString(res.Report))
			}
			if p.MessagesAcked != 0 {
				t.Errorf("steady traffic acked %d, want 0 — the target conversation should refuse every send", p.MessagesAcked)
			}
		}
	}
	for _, c := range res.Report.Clients {
		if c.Compared && !c.Compare.Clean() {
			t.Errorf("client %d's own comparison is dirty (%s) — this test is about a clean comparison that still misses a real failure", c.Index, c.Compare)
		}
	}
}

// COUNTER-PROOF — a client that could not provision is a harness failure,
// not a client silently dropped out of N.
func TestUnprovisionableClientCountsAsHarnessFailure(t *testing.T) {
	dbURL := soakDBFixture(t)
	cfg := tinyConfig(dbURL)
	cfg.debugFailProvision = map[int]bool{1: true}
	res := runSoak(context.Background(), cfg)
	if res.Verdict != VerdictHarnessFailure {
		t.Fatalf("verdict = %s, want harness_failure\n%s", res.Verdict, reportString(res.Report))
	}
	if res.Report.Clients[1].Provisioned {
		t.Errorf("client 1 reports Provisioned=true; the planted failure never reached the report")
	}
	if res.Report.ComparisonsRun != res.Report.N-1 {
		t.Errorf("comparisons run = %d, want %d (every OTHER client still ran)", res.Report.ComparisonsRun, res.Report.N-1)
	}
}

// COUNTER-PROOF — a comparison that never ran is a harness failure and never
// folds into a clean-looking report, even though every OTHER client that was
// compared is clean.
func TestSkippedComparisonCountsAsHarnessFailure(t *testing.T) {
	dbURL := soakDBFixture(t)
	cfg := tinyConfig(dbURL)
	cfg.debugSkipCompareFor = map[int]bool{2: true}
	res := runSoak(context.Background(), cfg)
	if res.Verdict != VerdictHarnessFailure {
		t.Fatalf("verdict = %s, want harness_failure\n%s", res.Verdict, reportString(res.Report))
	}
	if res.Report.Clients[2].Compared {
		t.Errorf("client 2 reports Compared=true; the planted skip never reached the report")
	}
	if res.Report.ComparisonsRun != res.Report.N-1 {
		t.Errorf("comparisons run = %d, want %d", res.Report.ComparisonsRun, res.Report.N-1)
	}
}

// COUNTER-PROOF — a timeout the harness set itself (an -await-timeout too
// small for anything to answer inside) is a harness failure, not a false
// pass and not mistaken for a server problem. The server here is entirely
// healthy; only the harness's own patience is at fault, and the run is
// given enough real wall-clock time afterward (the other phases still run)
// that the clients actually do converge — proving the report calls this a
// harness failure on the timeout alone, not because anything was truly lost.
func TestOwnTimeoutCountsAsHarnessFailure(t *testing.T) {
	dbURL := soakDBFixture(t)
	cfg := tinyConfig(dbURL)
	cfg.AwaitTimeout = 1 * time.Millisecond
	res := runSoak(context.Background(), cfg)
	if res.Verdict != VerdictHarnessFailure {
		t.Fatalf("verdict = %s, want harness_failure\n%s", res.Verdict, reportString(res.Report))
	}
	if len(res.Report.HarnessErrors) == 0 {
		t.Error("no harness errors recorded for a 1ms await bound — the bound was never actually exercised")
	}
}

// COUNTER-PROOF — a report that cannot be written to disk is a harness
// failure, even when the run underneath it was entirely clean. CLAUDE.md's
// "a journal write error" example, applied to the one thing this harness
// itself persists.
func TestUnwritableReportPathCountsAsHarnessFailure(t *testing.T) {
	dbURL := soakDBFixture(t)
	cfg := tinyConfig(dbURL)
	cfg.ReportPath = "/nonexistent-directory-for-cant-27-tests/report.json"
	res := runSoak(context.Background(), cfg)
	if res.Verdict != VerdictHarnessFailure {
		t.Fatalf("verdict = %s, want harness_failure\n%s", res.Verdict, reportString(res.Report))
	}
	found := false
	for _, e := range res.Report.HarnessErrors {
		if strings.Contains(e, "write the report") {
			found = true
		}
	}
	if !found {
		t.Errorf("no harness error mentions writing the report: %v", res.Report.HarnessErrors)
	}
}
