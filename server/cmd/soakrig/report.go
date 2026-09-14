package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/magos/catenary/internal/client"
)

// Verdict is the harness's own answer to "who is this report a finding
// about" — CANT-27's Done-when clause in exit-code form.
type Verdict string

const (
	// VerdictPass: every comparison ran and every one is clean.
	VerdictPass Verdict = "pass"
	// VerdictHarnessFailure: the run is inconclusive — something about the
	// INSTRUMENT (provisioning, a phase timeout, the comparison step itself,
	// or writing the report out) failed, so this report proves nothing about
	// the server either way. Includes "zero comparisons ran", which is never
	// a pass.
	VerdictHarnessFailure Verdict = "harness_failure"
	// VerdictServerFailure: at least one comparison RAN and reported real
	// loss, duplication, a phantom, a mismatch, a seq conflict or
	// out-of-order delivery. CLAUDE.md: stop and report back; this is a
	// finding for a person.
	VerdictServerFailure Verdict = "server_failure"
)

// Result is what one call to runSoak returns.
type Result struct {
	Report  Report
	Verdict Verdict
}

// Report is the whole of CANT-27's evidence for one run.
type Report struct {
	StartedAt time.Time
	Duration  time.Duration
	N         int

	Phases []PhaseReport

	// MissingAtRestart is R1's "something to lose": how many messages,
	// summed across clients, were committed-and-visible but not yet held the
	// moment the server was killed. Zero here means the kill phase proved
	// nothing about resume — there was nothing outstanding to lose.
	MissingAtRestart int

	Clients        []ClientReport
	ComparisonsRun int
	Aggregate      AggregateCompare

	// CloseStatuses is cumulative over the WHOLE run, across every client —
	// not broken out per phase. Keyed the way client.Stats.CloseStatuses is:
	// a WebSocket close code, or -1 for no close frame at all.
	CloseStatuses map[int]int

	Hello HelloHistogram

	// HarnessErrors is every harness-side problem this run hit: a client that
	// could not provision, a phase that never caught up within its bound, a
	// comparison that never ran, a report that could not be written. classify
	// reads this list; nothing here is evidence about the server.
	HarnessErrors []string

	Verdict Verdict
}

// PhaseReport is one phase's own counters.
type PhaseReport struct {
	Name          string
	Duration      time.Duration
	MessagesSent  int
	MessagesAcked int
	SendErrors    int
	Reconnects    int
}

// ClientReport is one client's own outcome. Every index 0..N-1 has an entry,
// whether or not that client ever got as far as a socket — Provisioned and
// Compared say how far it got, so a client dropped along the way is visible
// rather than silently missing from a shorter list.
type ClientReport struct {
	Index          int
	DeviceName     string
	Provisioned    bool
	ProvisionError string `json:",omitempty"`
	Compared       bool
	CompareError   string `json:",omitempty"`
	Compare        client.Report
}

// AggregateCompare sums the COUNTS from every client that was compared. It is
// not a merge of the underlying id lists — a message one client lost is not
// "the same" finding as one a different client lost — just the totals a
// person reads first.
type AggregateCompare struct {
	ServerMessages, ClientMessages, Counted                         int
	Lost, Phantom, Duplicated, Mismatched, SeqConflicts, OutOfOrder int
}

// HelloHistogram is docs/decisions/cant-24-resume.md's "the delta histogram
// is the only evidence that could reopen ruling 0" — read from the server's
// own structured "hello" log lines, not derived or assumed.
type HelloHistogram struct {
	Outcomes map[string]int
	Deltas   []int64 `json:",omitempty"` // raw values, for a caller that wants its own bucketing
	Buckets  []HelloBucket
}

// HelloBucket is one row of a coarse, human-readable histogram over Deltas.
type HelloBucket struct {
	Label string
	Count int
}

func computeAggregate(rep *Report) {
	var a AggregateCompare
	for _, c := range rep.Clients {
		if !c.Compared {
			continue
		}
		a.ServerMessages += c.Compare.ServerMessages
		a.ClientMessages += c.Compare.ClientMessages
		a.Counted += c.Compare.Counted
		a.Lost += len(c.Compare.Lost)
		a.Phantom += len(c.Compare.Phantom)
		a.Duplicated += len(c.Compare.Duplicated)
		a.Mismatched += len(c.Compare.Mismatched)
		a.SeqConflicts += len(c.Compare.SeqConflicts)
		a.OutOfOrder += len(c.Compare.OutOfOrder)
	}
	rep.Aggregate = a
}

// classify is the whole of CANT-27's harness-vs-server distinction, stated as
// one function so the counter-proof tests can drive it directly.
//
// SERVER FAILURE WINS. If any comparison that ran is dirty, the run is a
// server failure even if harness-side problems also occurred elsewhere —
// CLAUDE.md's "stop and report back" is the more urgent thing a reader needs
// to see, and HarnessErrors still lists the rest for a person to notice.
//
// ZERO COMPARISONS IS NEVER A PASS. rep.ComparisonsRun < rep.N covers every
// earlier-stage failure uniformly — the server never starting, every client
// failing to provision, a fatal config error before anything ran — without a
// separate "fatal" path: whatever stopped the run short, fewer comparisons
// ran than clients existed, and that alone is disqualifying.
func classify(rep *Report) Verdict {
	for _, c := range rep.Clients {
		if c.Compared && !c.Compare.Clean() {
			return VerdictServerFailure
		}
	}
	if len(rep.HarnessErrors) > 0 || rep.ComparisonsRun < rep.N {
		return VerdictHarnessFailure
	}
	return VerdictPass
}

// deltaBuckets partitions a hello's Delta — head minus cursor — into ranges a
// person reads at a glance. A negative delta is cursor_ahead (see
// internal/store/hello.go); everything else is how far behind a reconnect
// was, in messages.
var deltaBuckets = []struct {
	label  string
	lo, hi int64
}{
	{"cursor_ahead (<0)", math.MinInt64, -1},
	{"0 (at_head)", 0, 0},
	{"1-10", 1, 10},
	{"11-100", 11, 100},
	{"101-1000", 101, 1000},
	{"1001-10000", 1001, 10000},
	{">10000", 10001, math.MaxInt64},
}

func bucketizeDeltas(deltas []int64) []HelloBucket {
	out := make([]HelloBucket, len(deltaBuckets))
	for i, b := range deltaBuckets {
		out[i] = HelloBucket{Label: b.label}
	}
	for _, d := range deltas {
		for i, b := range deltaBuckets {
			if d >= b.lo && d <= b.hi {
				out[i].Count++
				break
			}
		}
	}
	return out
}

func writeReportFile(path string, rep *Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// printReport is the human-readable rendering: what a person reads at the
// terminal, and what gets pasted into the ticket.
func printReport(w io.Writer, rep Report) {
	fmt.Fprintf(w, "\n=== CANT-27 soak report ===\n")
	fmt.Fprintf(w, "N=%d duration=%s verdict=%s\n", rep.N, rep.Duration.Round(time.Second), rep.Verdict)

	fmt.Fprintf(w, "\nphases:\n")
	for _, p := range rep.Phases {
		fmt.Fprintf(w, "  %-22s duration=%-9s sent=%-5d acked=%-5d send_errors=%-4d reconnects=%d\n",
			p.Name, p.Duration.Round(100*time.Millisecond), p.MessagesSent, p.MessagesAcked, p.SendErrors, p.Reconnects)
	}
	fmt.Fprintf(w, "missing at restart (R1's \"something to lose\"): %d\n", rep.MissingAtRestart)

	fmt.Fprintf(w, "\ncomparisons: %d/%d run\n", rep.ComparisonsRun, rep.N)
	a := rep.Aggregate
	fmt.Fprintf(w, "aggregate: server=%d client=%d counted=%d — LOST=%d DUPLICATED=%d PHANTOM=%d mismatched=%d seq_conflicts=%d out_of_order=%d\n",
		a.ServerMessages, a.ClientMessages, a.Counted, a.Lost, a.Duplicated, a.Phantom, a.Mismatched, a.SeqConflicts, a.OutOfOrder)

	fmt.Fprintf(w, "\nclose statuses seen (cumulative over the whole run): %s\n", formatIntCounts(rep.CloseStatuses))

	fmt.Fprintf(w, "\nhello outcomes: %s\n", formatStringCounts(rep.Hello.Outcomes))
	fmt.Fprintf(w, "hello delta histogram (docs/decisions/cant-24-resume.md — the only evidence that could reopen ruling 0):\n")
	for _, b := range rep.Hello.Buckets {
		fmt.Fprintf(w, "  %-20s %d\n", b.Label, b.Count)
	}

	if len(rep.HarnessErrors) > 0 {
		fmt.Fprintf(w, "\nharness errors (%d):\n", len(rep.HarnessErrors))
		for _, e := range rep.HarnessErrors {
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}

	fmt.Fprintf(w, "\nper-client:\n")
	for _, c := range rep.Clients {
		switch {
		case !c.Provisioned:
			fmt.Fprintf(w, "  %2d %-14s NOT PROVISIONED: %s\n", c.Index, c.DeviceName, c.ProvisionError)
		case !c.Compared:
			fmt.Fprintf(w, "  %2d %-14s NOT COMPARED: %s\n", c.Index, c.DeviceName, c.CompareError)
		case !c.Compare.Clean():
			fmt.Fprintf(w, "  %2d %-14s DIRTY — %s\n", c.Index, c.DeviceName, c.Compare.String())
		default:
			fmt.Fprintf(w, "  %2d %-14s clean — %s\n", c.Index, c.DeviceName, c.Compare.String())
		}
	}
	fmt.Fprintln(w)
}

func formatIntCounts(m map[int]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += " "
		}
		label := fmt.Sprintf("%d", k)
		if k == -1 {
			label = "no-close-frame"
		}
		s += fmt.Sprintf("%s=%d", label, m[k])
	}
	return s
}

func formatStringCounts(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%s=%d", k, m[k])
	}
	return s
}
