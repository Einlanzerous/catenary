package main

// Pure tests of the harness's own logic — no database, no subprocess. The
// counter-proof tests that run a REAL broken soak (the way
// TestTheKillTestCatchesABrokenClient does, out of process) live in
// broken_test.go, gated on CATENARY_TEST_DATABASE_URL. These are the fast,
// always-on half: classify() is CANT-27's whole harness-vs-server
// distinction, so it earns a table of its own independent of anything that
// needs a live server.

import (
	"testing"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

func cleanCompare() client.Report {
	return client.Report{ServerMessages: 3, ClientMessages: 3, Counted: 3}
}

func dirtyCompare() client.Report {
	return client.Report{ServerMessages: 3, ClientMessages: 2, Counted: 2, Lost: []wire.Uuid{"x"}}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  Report
		want Verdict
	}{
		{
			name: "every comparison ran and every one is clean",
			rep: Report{N: 2, ComparisonsRun: 2, Clients: []ClientReport{
				{Index: 0, Provisioned: true, Compared: true, Compare: cleanCompare()},
				{Index: 1, Provisioned: true, Compared: true, Compare: cleanCompare()},
			}},
			want: VerdictPass,
		},
		{
			name: "zero comparisons ran is never a pass, even with no errors on record",
			rep:  Report{N: 3, ComparisonsRun: 0, Clients: []ClientReport{{Index: 0}, {Index: 1}, {Index: 2}}},
			want: VerdictHarnessFailure,
		},
		{
			name: "fewer comparisons than clients is a harness failure",
			rep: Report{N: 3, ComparisonsRun: 2, Clients: []ClientReport{
				{Index: 0, Provisioned: true, Compared: true, Compare: cleanCompare()},
				{Index: 1, Provisioned: true, Compared: true, Compare: cleanCompare()},
				{Index: 2, Provisioned: false, ProvisionError: "planted"},
			}},
			want: VerdictHarnessFailure,
		},
		{
			name: "a HarnessErrors entry alone, with every comparison run and clean, is still a harness failure",
			rep: Report{N: 1, ComparisonsRun: 1, HarnessErrors: []string{"write the report: disk full"},
				Clients: []ClientReport{{Index: 0, Provisioned: true, Compared: true, Compare: cleanCompare()}}},
			want: VerdictHarnessFailure,
		},
		{
			name: "a comparison that ran and is dirty is a server failure",
			rep: Report{N: 1, ComparisonsRun: 1, Clients: []ClientReport{
				{Index: 0, Provisioned: true, Compared: true, Compare: dirtyCompare()},
			}},
			want: VerdictServerFailure,
		},
		{
			name: "server failure wins even alongside a harness error elsewhere",
			rep: Report{N: 2, ComparisonsRun: 1, HarnessErrors: []string{"client 1 could not provision"},
				Clients: []ClientReport{
					{Index: 0, Provisioned: true, Compared: true, Compare: dirtyCompare()},
					{Index: 1, ProvisionError: "planted"},
				}},
			want: VerdictServerFailure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(&tc.rep); got != tc.want {
				t.Errorf("classify() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBucketizeDeltas(t *testing.T) {
	got := bucketizeDeltas([]int64{-5, 0, 0, 3, 3, 3, 50, 500, 5000, 50000})
	want := map[string]int{
		"cursor_ahead (<0)": 1,
		"0 (at_head)":       2,
		"1-10":              3,
		"11-100":            1,
		"101-1000":          1,
		"1001-10000":        1,
		">10000":            1,
	}
	if len(got) != len(want) {
		t.Fatalf("bucketizeDeltas returned %d buckets, want %d", len(got), len(want))
	}
	for _, b := range got {
		if b.Count != want[b.Label] {
			t.Errorf("bucket %q = %d, want %d", b.Label, b.Count, want[b.Label])
		}
	}
}

func TestComputeAggregateSkipsUncomparedClients(t *testing.T) {
	rep := Report{Clients: []ClientReport{
		{Index: 0, Compared: true, Compare: dirtyCompare()},
		{Index: 1, Compared: false, Compare: cleanCompare()}, // never actually compared; must not count
	}}
	computeAggregate(&rep)
	if rep.Aggregate.ServerMessages != 3 || rep.Aggregate.Lost != 1 {
		t.Errorf("aggregate = %+v, want only the compared client's counts (server=3, lost=1)", rep.Aggregate)
	}
}

func TestConfigValidate(t *testing.T) {
	c := Config{}
	if err := c.validate(); err == nil {
		t.Error("an empty Config must be refused: no DBURL")
	}
	c = Config{DBURL: "postgres://x", N: 0}
	if err := c.validate(); err == nil {
		t.Error("N < 1 must be refused")
	}
	c = Config{DBURL: "postgres://x", N: 5}
	if err := c.validate(); err != nil {
		t.Fatalf("a minimal valid config was refused: %v", err)
	}
	if c.SteadyDuration <= 0 || c.SendInterval <= 0 || c.StormRounds < 1 || c.AwaitTimeout <= 0 {
		t.Errorf("validate left a zero-value field unfilled: %+v", c)
	}
}
