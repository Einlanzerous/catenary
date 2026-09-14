//go:build soaksmoke

package main

// TestSmokeSoak is the "short, bounded smoke run" CANT-27 asks verify.sh to
// decide about. It is real: a real subprocess, a real Postgres, a real
// reconnect storm and a real kill -9 — just small and fast rather than the
// ticket's N>=20 floor, because verify.sh has to stay reasonably fast and
// deterministic.
//
// BEHIND A BUILD TAG so it runs exactly once per verify.sh invocation, named
// by a dedicated step, rather than a second time inside this package's
// ordinary `go test ./...` sweep (which already runs broken_test.go's
// counter-proofs against the same database). See verify.sh's own step for
// the call.
import (
	"context"
	"testing"
	"time"
)

func TestSmokeSoak(t *testing.T) {
	dbURL := soakDBFixture(t)
	cfg := Config{
		DBURL: dbURL, N: 5,
		SteadyDuration: 2 * time.Second, SendInterval: 150 * time.Millisecond,
		StormRounds: 2, KillMessages: 8, AwaitTimeout: 20 * time.Second,
	}
	res := runSoak(context.Background(), cfg)
	t.Log(reportString(res.Report))
	if res.Verdict != VerdictPass {
		t.Fatalf("smoke soak verdict = %s, want pass", res.Verdict)
	}
	if res.Report.ComparisonsRun != res.Report.N {
		t.Fatalf("comparisons run = %d, want %d", res.Report.ComparisonsRun, res.Report.N)
	}
	if res.Report.Hello.Outcomes == nil {
		t.Fatalf("no hello outcomes were captured from the server's own log — the histogram instrument produced nothing")
	}
}
