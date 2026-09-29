package main

// CANT-153 — the TypeScript transport as the client under test in CANT-102's
// kill-test and restore-test rigs (CANT-35 criterion 19). The soak's three
// phases never plant a listener gap or truncate the log, so they cannot reach
// `endCatchUpEarly` or `skipWipe`; these rigs can, and the Go client proves
// its controls here. So the TypeScript client runs through the SAME rigs, clean
// and with each fault, and each fault is watched costing exactly what the Go
// test requires it to cost. That puts obligations 3 and 4 against a real
// server for the TypeScript client, not only against fakes.
//
// The client is a tsDriver: web/src/transport/driver/driver.ts in a Node
// process, driven through tsdriver_test.go — a symlink to soakrig's
// tsdriver.go, so the soak and these rigs share one adapter.
//
// ONLY `serverStops`, NOT `clientDies`. CANT-35 ruling 2 → B keeps the
// TypeScript journal in memory, so a TypeScript client that dies takes its
// journal with it, and a relaunch bootstraps from nothing: clean by
// construction, and proof of nothing. A client relaunch over a durable journal
// is the durable journal's lane (ruling 2A's row, and its follow-up under 2B).
// The server's death is the lane that means something here, and it is also
// what CANT-35's Done-when asks: resume from the cursor after the network
// drops.
//
// Gated like tscohort_test.go in soakrig: CATENARY_TS_DRIVER unset skips, set
// and missing fails. verify.sh's CANT-153 step builds the bundle and sets it.

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/wire"
)

// tsDriverScript is the built driver bundle, or a skip.
func tsDriverScript(t *testing.T) string {
	t.Helper()
	p := os.Getenv("CATENARY_TS_DRIVER")
	if p == "" {
		t.Skip("CATENARY_TS_DRIVER not set; skipping the TypeScript lane (verify.sh builds the driver and sets it)")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("CATENARY_TS_DRIVER=%s: %v — the lane was asked to run the TypeScript driver and there is none; build it with `npm run build:driver` in web/", p, err)
	}
	return p
}

// newTSClient starts a driver for an enrolled device, with the Go rigs' own
// backoff, and kills it when the test ends. The transport is not started.
func newTSClient(t *testing.T, base string, dev wire.EnrollResponse, faults client.Faults) *tsDriver {
	t.Helper()
	cfg := tsDriverConfig{
		Script: tsDriverScript(t), BaseURL: base, Faults: faults,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 250 * time.Millisecond,
	}
	cfg.enrolled(dev)
	d, err := newTSDriver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Kill)
	return d
}

// tsRun runs one session of d until the test ends or end is called.
func tsRun(t *testing.T, ctx context.Context, d *tsDriver) (end func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	var once bool
	end = func() {
		if once {
			return
		}
		once = true
		if err := d.Stop(); err != nil {
			t.Errorf("stop the TypeScript client: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("the TypeScript client's session ended with %v, want nil after a stop", err)
		}
	}
	t.Cleanup(func() {
		d.Kill()
		if !once {
			<-done
		}
	})
	return end
}

// tsTheo is theoFactory for the TypeScript client.
func tsTheo(k *killRig, dev wire.EnrollResponse, _ *client.Journal, faults client.Faults) cohortClient {
	d := newTSClient(k.t, k.base(), dev, faults)
	tsRun(k.t, k.ctx, d)
	return d
}

// tsDevice is restoreDevice for the TypeScript client: one driver process for
// the whole test, holding the one journal, and a new transport per session.
type tsDevice struct {
	t    *testing.T
	ctx  context.Context
	d    *tsDriver
	stop func()
}

func (s *tsDevice) session() cohortClient {
	s.stop = tsRun(s.t, s.ctx, s.d)
	return s.d
}

func (s *tsDevice) end(cohortClient) { s.stop() }

func sameIDs(a, b []wire.Uuid) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// Criterion 6's five phases, the TypeScript client resuming through a server
// that stops without draining: clean at every checkpoint, each message counted
// once, and something to lose at the restart.
func TestTheTSClientResumesThroughTheKillTest(t *testing.T) {
	res := runKillTestWith(t, serverStops, client.Faults{}, tsTheo)
	for _, cp := range []struct {
		name string
		r    client.Report
	}{
		{"after the reconnect", res.afterReconnect},
		{"after the gap", res.afterGap},
		{"after the replay", res.final},
	} {
		if !cp.r.Clean() {
			t.Errorf("%s: %s\nlost %v\nduplicated %v\nphantom %v\nmismatched %v\nseq conflicts %v\nout of order %v",
				cp.name, cp.r, cp.r.Lost, cp.r.Duplicated, cp.r.Phantom, cp.r.Mismatched, cp.r.SeqConflicts, cp.r.OutOfOrder)
		}
	}
	if res.final.Counted != res.final.ServerMessages {
		t.Errorf("counted %d, server log %d: each message counted exactly once", res.final.Counted, res.final.ServerMessages)
	}
	t.Logf("the TypeScript client, %s — missing at restart %d · %s · %d replays, head unchanged", serverStops, res.missingAtRestart, res.final, res.replays)
}

// Criterion 19's kill-test controls, the TypeScript client broken in one
// obligation at a time. Each loses EXACTLY the gap messages — the ones Theo
// can see that committed while the listener was held down, with a /sync in
// flight — at the checkpoint TestTheKillTestCatchesABrokenClient reads for Go.
func TestTheKillTestCatchesABrokenTSClient(t *testing.T) {
	t.Run("a cursor moved by live frames loses the gap", func(t *testing.T) {
		res := runKillTestWith(t, serverStops, client.Faults{CursorOnLiveFrames: true}, tsTheo)
		if len(res.gap) == 0 || !sameIDs(res.final.Lost, res.gap) {
			t.Errorf("lost %v, want exactly the gap messages %v: %s", res.final.Lost, res.gap, res.final)
		}
		t.Logf("cursorOnLiveFrames: lost %d, the gap is %d: %s", len(res.final.Lost), len(res.gap), res.final)
	})
	t.Run("catch-up ended on a page requested before the resync loses the gap", func(t *testing.T) {
		res := runKillTestWith(t, serverStops, client.Faults{EndCatchUpEarly: true}, tsTheo)
		if len(res.gap) == 0 || !sameIDs(res.afterGap.Lost, res.gap) {
			t.Errorf("lost %v, want exactly the gap messages %v: %s", res.afterGap.Lost, res.gap, res.afterGap)
		}
		t.Logf("endCatchUpEarly: lost %d, the gap is %d: %s", len(res.afterGap.Lost), len(res.gap), res.afterGap)
	})
}

func tsRestoreDevice(t *testing.T, faults client.Faults) func(r *rig, dev wire.EnrollResponse) restoreDevice {
	return func(r *rig, dev wire.EnrollResponse) restoreDevice {
		return &tsDevice{t: t, ctx: r.ctx, d: newTSClient(t, r.base, dev, faults)}
	}
}

// Criterion 7 and criterion 19's `skipWipe`, the TypeScript client through the
// truncated-and-regrown log.
func TestTheTSClientDiscardsALogTruncatedBelowItsCursor(t *testing.T) {
	t.Run("the client wipes and bootstraps from 0", func(t *testing.T) {
		res := runRestoreWith(t, tsRestoreDevice(t, client.Faults{}))
		d := res.atDiscard
		if d.status.Discards != 1 || d.status.Wipes != 1 {
			t.Errorf("at the discard: %d discards, %d wipes — want one of each", d.status.Discards, d.status.Wipes)
		}
		if !d.report.Clean() {
			t.Errorf("at the discard: %s\nlost %v\nphantom %v\nseq conflicts %v", d.report, d.report.Lost, d.report.Phantom, d.report.SeqConflicts)
		}
		if len(d.view) != 0 {
			t.Errorf("at the discard the store differs from a fresh install's:\n%v", d.view)
		}
		if !res.cursorAheadLogged {
			t.Error("the hello below the cursor was not logged as cursor_ahead")
		}
		a := res.afterRegrowth
		if a.status.Discards != 0 {
			t.Errorf("after the regrowth: %d discards, want none — the cursor was below head", a.status.Discards)
		}
		if !a.report.Clean() || len(a.view) != 0 {
			t.Errorf("after the regrowth: %s, view differences %v", a.report, a.view)
		}
	})

	t.Run("a client that re-syncs from 0 without wiping ends missing exactly the regrown messages", func(t *testing.T) {
		res := runRestoreWith(t, tsRestoreDevice(t, client.Faults{SkipWipe: true}))
		d := res.atDiscard
		if d.status.Discards != 1 || d.status.Wipes != 0 {
			t.Fatalf("at the discard: %d discards, %d wipes — the fault must see the signal and not wipe", d.status.Discards, d.status.Wipes)
		}
		if len(d.report.Phantom) == 0 || len(d.view) == 0 {
			t.Errorf("the kept store matches the server's view at the discard: %s", d.report)
		}
		a := res.afterRegrowth
		want := res.regrownBelowOldCursor
		if int64(len(want)) != res.oldCursor-res.headAtDiscard {
			t.Fatalf("%d messages regrown between head %d and the old cursor %d", len(want), res.headAtDiscard, res.oldCursor)
		}
		if !sameIDs(a.report.Lost, want) {
			t.Errorf("lost %v, want exactly the regrown messages %v: %s", a.report.Lost, want, a.report)
		}
		t.Logf("skipWipe: %s", a.report)
	})
}

// `blackhole` makes a real half-dead socket — nothing crosses, nothing closes
// — which only the heartbeat can find: the TypeScript client severs it at
// missed_pong_limit outstanding pings and redials, against a real server.
// The shortest interval the wire allows — `ready.heartbeat_interval_sec` has a
// schema minimum of 5, and the generated decoder refuses a `ready` below it —
// and a limit of one, so the whole thing takes seconds.
func TestTheTSClientSeversAHalfDeadSocketOnTheHeartbeat(t *testing.T) {
	const interval, limit = 5, 1
	k := newKillRig(t, func(c *config.Config) {
		c.HeartbeatIntervalSec, c.MissedPongLimit = interval, limit
	})
	theo := mkUser(k.ctx, t, k.pool, "theo", "Theo")
	d := newTSClient(t, k.base(), k.enroll(theo, "phone"), client.Faults{})
	tsRun(t, k.ctx, d)
	awaitClient(t, d, "ready", func() bool { s := d.Status(); return s.Ready && s.PongsReceived >= 1 })

	d.Blackhole()
	awaitClient(t, d, "the heartbeat's sever, and a new session", func() bool {
		s := d.Status()
		return s.HeartbeatSevers >= 1 && s.Readys >= 2 && s.Ready
	})
	s := d.Status()
	if s.HeartbeatInterval != interval*time.Second || s.MissedPongLimit != limit {
		t.Errorf("the client runs %v / %d, want the announced %ds / %d", s.HeartbeatInterval, s.MissedPongLimit, interval, limit)
	}
	t.Logf("after the black hole: %d heartbeat severs, %d readys over %d dials, last close %q", s.HeartbeatSevers, s.Readys, s.Dials, s.LastClose)
}
