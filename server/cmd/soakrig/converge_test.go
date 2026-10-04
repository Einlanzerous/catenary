package main

// CANT-46 — the convergence rig's lanes and its controls, against a real
// server, with the stand-in pair ruling 3 picked: the TypeScript transport
// through its Node driver against the Go reference client. Every schedule
// runs in both role assignments, and each of the rig's own checks is watched
// failing in the same lane: a verdict nobody has seen say "diverged" or
// "harness_failure" is a claim about the rig.
//
// GATED as tscohort_test.go is: CATENARY_TEST_DATABASE_URL, and
// CATENARY_TS_DRIVER naming the built driver bundle — unset skips, set and
// missing fails. verify.sh's CANT-46 step builds nothing of its own; it reuses
// the bundle the CANT-153 step built.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// convergeLane is one server and the driver bundle, for a test's runs.
func convergeLane(t *testing.T) (*harness, convergeConfig) {
	t.Helper()
	script := tsDriverScript(t)
	h := startLocalServer(t, soakDBFixture(t))
	return h, convergeConfig{TSDriver: script, Dir: t.TempDir()}
}

// bothRoles runs fn once with each implementation as device A.
func bothRoles(t *testing.T, fn func(t *testing.T, a, b string)) {
	t.Helper()
	for _, roles := range [][2]string{{cohortTS, cohortGo}, {cohortGo, cohortTS}} {
		t.Run("A="+roles[0]+",B="+roles[1], func(t *testing.T) { fn(t, roles[0], roles[1]) })
	}
}

// Schedules S1–S4 converge in both role assignments: SameState clean, both
// devices clean against the server, and each device's markers the server's.
func TestConvergenceTSAgainstGo(t *testing.T) {
	h, base := convergeLane(t)
	for _, schedule := range []string{scheduleS1, scheduleS2, scheduleS3, scheduleS4} {
		t.Run(schedule, func(t *testing.T) {
			bothRoles(t, func(t *testing.T, a, b string) {
				cfg := base
				cfg.Schedule, cfg.A, cfg.B = schedule, a, b
				rep := h.runConverge(context.Background(), cfg)
				t.Log(rep.String())
				if rep.Verdict != VerdictConverged {
					t.Fatalf("verdict = %s, want converged", rep.Verdict)
				}
				if rep.ComparisonsRun != convergeComparisons {
					t.Errorf("comparisons run = %d, want %d", rep.ComparisonsRun, convergeComparisons)
				}
				if rep.Holds == 0 || rep.RefusedDials == 0 {
					t.Errorf("holds %d, dials refused %d: a run that held nothing proved nothing", rep.Holds, rep.RefusedDials)
				}
			})
		})
	}
}

// S5 — the outbox inside the test (ruling 1 → option 0): three texts composed
// on each device while both are held, drained on return. TWO TYPESCRIPT
// DEVICES, because the Go reference client has no outbox: this proves the
// schedule and the driver's `compose` and `outbox`, and nothing about two
// implementations agreeing. The Dart lane (CANT-188) is where it meets a
// second one.
func TestConvergenceOfTheOutboxAcrossAPartition(t *testing.T) {
	h, cfg := convergeLane(t)
	cfg.Schedule, cfg.A, cfg.B = scheduleS5, cohortTS, cohortTS
	rep := h.runConverge(context.Background(), cfg)
	t.Log(rep.String())
	if rep.Verdict != VerdictConverged {
		t.Fatalf("verdict = %s, want converged", rep.Verdict)
	}
	if rep.ComparisonsRun != convergeComparisons+1 {
		t.Errorf("comparisons run = %d, want %d: the three comparisons and the outbox's", rep.ComparisonsRun, convergeComparisons+1)
	}
	if rep.Holds != 2 || rep.RefusedDials == 0 {
		t.Errorf("holds %d, dials refused %d: S5 holds both devices", rep.Holds, rep.RefusedDials)
	}
}

// The Go reference client has no outbox, and S5 says so rather than passing a
// schedule it did not run.
func TestS5RefusesADeviceWithNoOutbox(t *testing.T) {
	h, cfg := convergeLane(t)
	cfg.Schedule, cfg.A, cfg.B = scheduleS5, cohortTS, cohortGo
	rep := h.runConverge(context.Background(), cfg)
	t.Log(rep.String())
	if rep.Verdict != VerdictConvergeHarnessFailure {
		t.Fatalf("verdict = %s, want harness_failure", rep.Verdict)
	}
	if !strings.Contains(strings.Join(rep.HarnessErrors, "\n"), "has no outbox") {
		t.Errorf("the harness error does not say why: %q", rep.HarnessErrors)
	}
}

// `compose` answering means the entry is on disk: a driver killed with a text
// in its outbox and relaunched over the same journal file still holds it,
// under the same client_id, and sends it once it has a network.
func TestAComposedTextSurvivesTheDriversDeath(t *testing.T) {
	h, cfg := convergeLane(t)
	cfg.A, cfg.B, cfg.SettleTimeout = cohortTS, cohortTS, 30*time.Second
	r := &convergeRun{h: h, cfg: cfg}
	defer r.teardown()
	ctx := context.Background()
	if !r.setup(ctx) {
		t.Fatalf("the opening: %q", r.rep.HarnessErrors)
	}
	d := r.a
	d.proxy.hold()
	if err := r.await(ctx, d.c, func() bool { return !d.c.Status().Ready }); err != nil {
		t.Fatal(err)
	}
	id, err := d.c.(outboxClient).Compose(ctx, wid(r.room), "composed, then the process died")
	if err != nil {
		t.Fatal(err)
	}
	d.kill()
	if err := d.launch(); err != nil {
		t.Fatal(err)
	}
	// Asked of the relaunched driver once its transport is up: `outbox` is
	// refused before `start`, which Run sends.
	var left []outboxEntry
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if left, err = d.c.(outboxClient).Outbox(ctx); err == nil {
			break
		}
	}
	if err != nil || len(left) != 1 || left[0].ClientID != id || left[0].State != "queued" {
		t.Fatalf("the relaunched driver's outbox = %+v (err %v), want the one entry %s, queued", left, err, id)
	}
	if n, err := r.committed(ctx, []wire.Uuid{id}); err != nil || n != 0 {
		t.Fatalf("%d rows committed for a text composed behind a held proxy (err %v)", n, err)
	}

	d.proxy.heal()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		n, err := r.committed(ctx, []wire.Uuid{id})
		left, lerr := d.c.(outboxClient).Outbox(ctx)
		if err == nil && lerr == nil && n == 1 && len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the heal: %d rows committed (err %v), outbox %+v (err %v); want exactly one row and an empty outbox", n, err, left, lerr)
		}
	}
}

// THE FAULT. Device B keeps the Conversation it held before the partition, so
// S1 must end diverged, naming FirstUnreadSeq — and both devices must still be
// clean under client.Compare, which is the demonstration that the existing
// comparison cannot see this and the new one can.
func TestConvergenceCatchesAHeldConversation(t *testing.T) {
	h, base := convergeLane(t)
	bothRoles(t, func(t *testing.T, a, b string) {
		cfg := base
		cfg.Schedule, cfg.A, cfg.B = scheduleS1, a, b
		cfg.FaultsB = client.Faults{KeepHeldConversation: true}
		rep := h.runConverge(context.Background(), cfg)
		t.Log(rep.String())
		if rep.Verdict != VerdictDiverged {
			t.Fatalf("verdict = %s, want diverged", rep.Verdict)
		}
		if !strings.Contains(strings.Join(rep.Pairwise, "\n"), "FirstUnreadSeq") {
			t.Errorf("the pairwise report does not name FirstUnreadSeq: %q", rep.Pairwise)
		}
		if !rep.CompareA.Clean() || !rep.CompareB.Clean() {
			t.Errorf("client.Compare saw the fault (A: %s; B: %s); it reads no state this fault touches", rep.CompareA, rep.CompareB)
		}
		if len(rep.Markers) == 0 || !strings.Contains(rep.Markers[0], "device B") {
			t.Errorf("the marker comparison did not name device B: %q", rep.Markers)
		}
	})
}

// THE PARTITION THAT WAS NOT ONE. hold() planted as a no-op must end
// harness_failure on the partition-bit check, never converged.
func TestAPartitionThatHeldNothingIsAHarnessFailure(t *testing.T) {
	h, cfg := convergeLane(t)
	cfg.Schedule, cfg.A, cfg.B = scheduleS1, cohortTS, cohortGo
	cfg.debugHoldIsNoOp, cfg.SettleTimeout = true, 5*time.Second
	rep := h.runConverge(context.Background(), cfg)
	t.Log(rep.String())
	if rep.Verdict != VerdictConvergeHarnessFailure {
		t.Fatalf("verdict = %s, want harness_failure", rep.Verdict)
	}
	if !strings.Contains(strings.Join(rep.HarnessErrors, "\n"), "partition bit was never proven") {
		t.Errorf("the harness error does not name the partition bit: %q", rep.HarnessErrors)
	}
}

// THE SKIPPED COMPARISON, in the shape of the soak's debugSkipCompareFor: a
// run that is clean in everything it did compare must not pass.
func TestASkippedPairwiseComparisonIsAHarnessFailure(t *testing.T) {
	h, cfg := convergeLane(t)
	cfg.Schedule, cfg.A, cfg.B = scheduleS1, cohortTS, cohortGo
	cfg.debugSkipPairwise = true
	rep := h.runConverge(context.Background(), cfg)
	t.Log(rep.String())
	if rep.Verdict != VerdictConvergeHarnessFailure {
		t.Fatalf("verdict = %s, want harness_failure", rep.Verdict)
	}
	if rep.ComparisonsRun != convergeComparisons-1 || len(rep.Differences()) != 0 {
		t.Errorf("comparisons run %d, differences %q: want every other comparison run and clean, so the skip alone is what failed it", rep.ComparisonsRun, rep.Differences())
	}
}

// THE OPENING. A run whose opening leaves one device without the room ends
// harness_failure before anything is held.
func TestAnOpeningThatLeavesADeviceWithoutTheRoomIsAHarnessFailure(t *testing.T) {
	h, cfg := convergeLane(t)
	cfg.Schedule, cfg.A, cfg.B = scheduleS1, cohortTS, cohortGo
	cfg.debugStrangerB, cfg.SettleTimeout = true, 5*time.Second
	rep := h.runConverge(context.Background(), cfg)
	t.Log(rep.String())
	if rep.Verdict != VerdictConvergeHarnessFailure {
		t.Fatalf("verdict = %s, want harness_failure", rep.Verdict)
	}
	if !strings.Contains(strings.Join(rep.HarnessErrors, "\n"), "the opening left device B without the room") {
		t.Errorf("the harness error does not name device B and the room: %q", rep.HarnessErrors)
	}
	if rep.Holds != 0 {
		t.Errorf("%d devices were held after an opening that did not hold", rep.Holds)
	}
}

// WHY SETTLING CATCHES UP BEFORE IT WAITS. A device that has taken a message
// live has a cursor below the server's high-water mark and stays there: a
// cursor moves on a page and on nothing else, and somebody else's message is
// not a catch-up trigger. It reaches the mark only once it is asked.
func TestALiveDeviceReachesTheHighWaterMarkOnlyAfterACatchUp(t *testing.T) {
	h, cfg := convergeLane(t)
	cfg.Schedule, cfg.A, cfg.B, cfg.SettleTimeout = scheduleS1, cohortTS, cohortGo, defaultSettleTimeout
	r := &convergeRun{h: h, cfg: cfg}
	defer r.teardown()
	ctx := context.Background()
	if !r.setup(ctx) {
		t.Fatalf("the opening: %q", r.rep.HarnessErrors)
	}
	ack, ok := r.send(ctx, r.room)
	if !ok {
		t.Fatalf("send: %q", r.rep.HarnessErrors)
	}
	for _, d := range []*convergeDevice{r.a, r.b} {
		if err := r.awaitSnapshot(ctx, d, func(s client.Snapshot) bool { return holdsMessage(s, ack.MessageID) }); err != nil {
			t.Fatalf("device %s (%s) never took the message live: %v", d.label, d.impl, err)
		}
	}
	high, _, err := syncWalk(ctx, h.baseURL, r.rigToken)
	if err != nil {
		t.Fatal(err)
	}
	// Long enough for a catch-up nobody asked for to have landed, were one
	// coming.
	time.Sleep(300 * time.Millisecond)
	for _, d := range []*convergeDevice{r.a, r.b} {
		if s := d.c.Status(); s.Cursor >= high {
			t.Fatalf("device %s (%s) holds the message and its cursor is %d, at or above the high-water mark %d, with no catch-up asked for", d.label, d.impl, s.Cursor, high)
		}
	}
	for _, d := range []*convergeDevice{r.a, r.b} {
		d.c.CatchUp()
		if err := r.await(ctx, d.c, func() bool { s := d.c.Status(); return s.CaughtUp && s.Cursor >= high }); err != nil {
			t.Errorf("device %s (%s) did not reach %d after a catch-up: %v", d.label, d.impl, high, err)
		}
	}
}

// partitionProxy against a real server, once with each client: before hold()
// the client reaches ready through it and a /sync through it succeeds; after,
// the open socket is gone, a new dial is refused, a /sync through it fails,
// and the refused-dial count rises; after heal() both succeed again.
func TestThePartitionProxyHoldsAndHeals(t *testing.T) {
	h, cfg := convergeLane(t)
	for _, impl := range []string{cohortGo, cohortTS} {
		t.Run(impl, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cfg := cfg
			cfg.A, cfg.B, cfg.SettleTimeout = impl, impl, 30*time.Second
			r := &convergeRun{h: h, cfg: cfg}
			defer r.teardown()
			if !r.setup(ctx) {
				t.Fatalf("the opening: %q", r.rep.HarnessErrors)
			}
			d := r.a
			syncThrough := func() error {
				_, _, err := syncWalk(ctx, d.proxy.base(), d.enroll.AccessToken)
				return err
			}
			if err := syncThrough(); err != nil {
				t.Fatalf("before hold() a /sync through the proxy failed: %v", err)
			}

			d.proxy.hold()
			if err := r.await(ctx, d.c, func() bool { return !d.c.Status().Ready }); err != nil {
				t.Fatalf("after hold() the open socket is still there: %v", err)
			}
			if err := syncThrough(); err == nil {
				t.Error("after hold() a /sync through the proxy succeeded")
			}
			before := d.proxy.refusedDials()
			if before == 0 {
				t.Error("after hold() and a refused /sync the proxy counts no refused dial")
			}
			// A new socket dial: refused before a byte of answer.
			resp, err := http.Get(d.proxy.base() + "/healthz")
			if err == nil {
				_ = resp.Body.Close()
				t.Error("after hold() a new request through the proxy was answered")
			}
			conn, err := net.Dial("tcp", strings.TrimPrefix(d.proxy.base(), "http://"))
			if err == nil {
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, rerr := conn.Read(make([]byte, 1)); !errors.Is(rerr, io.EOF) && rerr == nil {
					t.Error("after hold() a new connection through the proxy carried bytes")
				}
				_ = conn.Close()
			}
			if after := d.proxy.refusedDials(); after <= before {
				t.Errorf("refused dials %d → %d across two more dials, want a rise", before, after)
			}
			if d.c.Status().Ready {
				t.Error("the client became ready through a held proxy")
			}

			d.proxy.heal()
			if err := r.await(ctx, d.c, func() bool { return d.c.Status().Ready }); err != nil {
				t.Fatalf("after heal() the client never came back: %v", err)
			}
			if err := syncThrough(); err != nil {
				t.Errorf("after heal() a /sync through the proxy failed: %v", err)
			}
		})
	}
}
