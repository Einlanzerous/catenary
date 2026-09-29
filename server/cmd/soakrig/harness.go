package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/store"
)

// soakClient is one provisioned account's device: the client running it and
// the identity the harness needs to query its own view of the log. c is a
// *client.Client for the Go cohort and a *tsDriver for the TypeScript one
// (CANT-153); j, the Go client's journal, is nil for a TypeScript client,
// whose journal lives in its own process.
type soakClient struct {
	index  int
	name   string
	cohort string
	c      cohortClient
	j      *client.Journal
	userID uuid.UUID
}

// snapshot reads a client's journal for a comparison. A TypeScript client's is
// read over stdio, and a read that failed is an error here — never an empty
// journal, which Compare would report as every message lost.
func (sc *soakClient) snapshot() (client.Snapshot, error) {
	if t, ok := sc.c.(tsSnapshotter); ok {
		return t.TrySnapshot()
	}
	return sc.c.Snapshot(), nil
}

// harness is the state one call to runSoak builds and tears down. Not reused
// across runs — a fresh harness per call, the way a fresh killRig backs every
// CANT-102 test.
type harness struct {
	cfg Config

	pool  *pgxpool.Pool
	store *store.Store

	bin     string
	driver  string
	port    int
	baseURL string

	proc       *exec.Cmd
	procDone   <-chan struct{}
	waitErr    error
	procStderr *syncBuffer
	tailDone   <-chan struct{}
	instance   int
	logFiles   []closer

	hello *helloHistogram

	mu   sync.Mutex
	errs []string
}

type closer interface{ Close() error }

// runSoak is CANT-27's Done-when, end to end. It never panics on an
// operational failure — every stage funnels a problem into h.harnessError and
// keeps going where it can, or returns the partial report where it cannot,
// because classify() already treats "fewer comparisons than clients" as
// disqualifying on its own; there is no separate fatal-error path to keep in
// sync with it.
func runSoak(ctx context.Context, cfg Config) Result {
	rep := Report{StartedAt: time.Now(), N: cfg.N, Cohort: cfg.Cohort, Clients: make([]ClientReport, cfg.N)}
	for i := range rep.Clients {
		rep.Clients[i] = ClientReport{Index: i}
	}

	h := &harness{cfg: cfg, hello: newHelloHistogram()}
	defer h.cleanup()

	if err := h.cfg.validate(); err != nil {
		h.harnessError("config: %v", err)
		return h.finish(&rep)
	}
	rep.Cohort = h.cfg.Cohort

	bin, err := resolveBinary(h.cfg)
	if err != nil {
		h.harnessError("resolve the catenary binary: %v", err)
		return h.finish(&rep)
	}
	h.bin = bin

	if h.cfg.needsTS() {
		script, err := resolveDriver(h.cfg)
		if err != nil {
			h.harnessError("resolve the TypeScript driver: %v", err)
			return h.finish(&rep)
		}
		h.driver = script
	}

	port, err := freePort()
	if err != nil {
		h.harnessError("pick a free port: %v", err)
		return h.finish(&rep)
	}
	h.port = port
	h.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	pool, err := store.ConnectWithRetry(ctx, h.cfg.DBURL, 30*time.Second)
	if err != nil {
		h.harnessError("connect to the database: %v", err)
		return h.finish(&rep)
	}
	h.pool = pool
	h.store = store.New(pool, store.DefaultLimits(), h.cfg.logger())

	if err := h.startServer(ctx); err != nil {
		h.harnessError("start the server: %v", err)
		return h.finish(&rep)
	}

	provisioned, room := h.provisionUsers(ctx)
	clients := h.buildClients(provisioned, &rep)
	if len(clients) == 0 {
		h.harnessError("no client provisioned; nothing to run")
		return h.finish(&rep)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, sc := range clients {
		wg.Add(1)
		go func(sc *soakClient) {
			defer wg.Done()
			// Run used to end only on a cancelled context or a Kill. Since
			// CANT-123 it also ends when the client must not reconnect, and a
			// client that stopped for THAT reason has to be named: every
			// phase after it would otherwise read as "never caught up" with
			// messages missing, which is a symptom and not the cause. The
			// likeliest one here is a server that predates CANT-122's 4002,
			// whose hello timeout is still a bare 1008.
			//
			// A TypeScript client can also stop because its driver PROCESS
			// died, which a Go client in this process cannot. That is named
			// too, with the driver's own last words, for the same reason.
			var term *client.TerminalError
			switch err := sc.c.Run(runCtx); {
			case errors.As(err, &term):
				h.harnessError("client %d (%s) went terminal and will not reconnect: %v", sc.index, sc.name, err)
			case errors.Is(err, errTSDriverGone):
				h.harnessError("client %d (%s): the TypeScript driver process died: %v", sc.index, sc.name, err)
			}
		}(sc)
	}
	defer func() {
		for _, sc := range clients {
			sc.c.Kill()
		}
		cancelRun()
		wg.Wait()
	}()

	h.awaitAllCaughtUp(ctx, clients, "the first connect")

	rep.Phases = append(rep.Phases, h.steadyTraffic(ctx, clients, room))
	rep.Phases = append(rep.Phases, h.reconnectStorm(ctx, clients))
	killPhase, missing := h.killAndRestart(ctx, clients, room)
	rep.Phases = append(rep.Phases, killPhase)
	rep.MissingAtRestart = missing

	h.awaitHeld(ctx, clients)
	h.compareAll(ctx, clients, &rep)

	rep.CloseStatuses = map[int]int{}
	for _, sc := range clients {
		for code, n := range sc.c.Status().CloseStatuses {
			rep.CloseStatuses[code] += n
		}
	}

	return h.finish(&rep)
}

// buildClients turns a provisioned enrollment into a running client.Client
// for every index that succeeded, and records a ClientReport for every index
// either way — a provisioning failure stays visible in the report rather than
// silently shrinking N.
func (h *harness) buildClients(provisioned []provisionedClient, rep *Report) []*soakClient {
	var clients []*soakClient
	for _, p := range provisioned {
		cohort := h.cfg.cohortOf(p.index)
		if p.err != nil {
			h.harnessError("provision client %d (%s): %v", p.index, p.name, p.err)
			rep.Clients[p.index] = ClientReport{Index: p.index, DeviceName: p.name, Cohort: cohort, ProvisionError: p.err.Error()}
			continue
		}
		j := client.NewJournal()
		cred, err := client.CredentialFromEnroll(p.enroll)
		if err == nil {
			err = j.Enroll(cred)
		}
		if err != nil {
			h.harnessError("enroll client %d's journal (%s): %v", p.index, p.name, err)
			rep.Clients[p.index] = ClientReport{Index: p.index, DeviceName: p.name, Cohort: cohort, ProvisionError: err.Error()}
			continue
		}
		var c cohortClient
		if cohort == cohortTS {
			c, err = h.newTSClient(p)
			j = nil
		} else {
			c, err = client.New(client.Config{
				BaseURL:    h.baseURL,
				ClientInfo: "cant-27-soakrig", Journal: j,
				Faults:     h.cfg.debugFaults[p.index],
				BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax,
				// A client's own internal narration is per-heartbeat noise at N
				// clients; the report's Stats already summarize it. The
				// harness's own logger (h.cfg.Logger) is for THIS process's
				// progress, never handed down to a client.
				Logger: nil,
			})
		}
		if err != nil {
			h.harnessError("construct client %d (%s, %s): %v", p.index, p.name, cohort, err)
			rep.Clients[p.index] = ClientReport{Index: p.index, DeviceName: p.name, Cohort: cohort, ProvisionError: err.Error()}
			continue
		}
		clients = append(clients, &soakClient{index: p.index, name: p.name, cohort: cohort, c: c, j: j, userID: p.userID})
		rep.Clients[p.index] = ClientReport{Index: p.index, DeviceName: p.name, Cohort: cohort, Provisioned: true}
	}
	return clients
}

// soakBackoffMin and soakBackoffMax are BOTH cohorts' dial ramp, so the two
// storm alike: Go's Config.BackoffMin/BackoffMax, and the TypeScript
// transport's backoffMinMs/backoffMaxMs, which exist for rigs only.
const (
	soakBackoffMin = 100 * time.Millisecond
	soakBackoffMax = 2 * time.Second
)

// newTSClient starts one TypeScript client's driver process over an
// enrollment. The credential rides the start command inline: `soak` enrolls
// in memory and writes no credential file.
func (h *harness) newTSClient(p provisionedClient) (*tsDriver, error) {
	cfg := tsDriverConfig{
		Node: h.cfg.Node, Script: h.driver, BaseURL: h.baseURL,
		Faults:     h.cfg.debugFaults[p.index],
		BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax,
	}
	cfg.enrolled(p.enroll)
	if h.cfg.ServerLogDir != "" {
		if f, err := os.Create(filepath.Join(h.cfg.ServerLogDir, fmt.Sprintf("driver-%d.stderr.log", p.index))); err == nil {
			cfg.Stderr = f
			h.logFiles = append(h.logFiles, f)
		}
	}
	return newTSDriver(cfg)
}

// awaitAllCaughtUp waits, per client and in parallel, for ready+caught-up,
// bounded by cfg.AwaitTimeout. A client that never gets there within the
// bound is a harness error: CLAUDE.md's "a clock or timeout the harness set
// itself" is exactly this wait, named as what it is. These interior
// checkpoints are diagnostics — a phase that did not settle within the
// operator's own patience — and NOT what compareAll reads from: a client
// still catches up on its own in the background regardless of whether this
// particular wait gave up on it.
func (h *harness) awaitAllCaughtUp(ctx context.Context, clients []*soakClient, phase string) {
	h.awaitAllCaughtUpBounded(ctx, clients, phase, h.cfg.AwaitTimeout)
}

// minFinalSettle floors the ONE wait that precedes compareAll, regardless of
// how small cfg.AwaitTimeout is set. Every OTHER checkpoint is free to time
// out early as pure diagnostics — a client still converges on its own in the
// background — but Compare reads a Snapshot taken right after this call, and
// a Compare run before real convergence would report loss that is not
// permanent, only premature: a harness artifact wearing a server failure's
// clothes. -await-timeout tunes how eagerly a slow phase gets FLAGGED; it
// must never be able to make the comparison itself unfair.
const minFinalSettle = 30 * time.Second

// awaitFinalSettle is the last wait before compareAll. Its own timeout is
// still recorded as a harness error if it fires — CLAUDE.md's timeout case
// applies here too — but the bound itself can only ever be as tight as
// cfg.AwaitTimeout AT MOST as generous as minFinalSettle, never tighter.
func (h *harness) awaitFinalSettle(ctx context.Context, clients []*soakClient) {
	h.awaitAllCaughtUpBounded(ctx, clients, "final settle before compare", max(h.cfg.AwaitTimeout, minFinalSettle))
}

func (h *harness) awaitAllCaughtUpBounded(ctx context.Context, clients []*soakClient, phase string, timeout time.Duration) {
	var wg sync.WaitGroup
	for _, sc := range clients {
		wg.Add(1)
		go func(sc *soakClient) {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := sc.c.Await(actx, func() bool { s := sc.c.Status(); return s.Ready && s.CaughtUp }); err != nil {
				h.harnessError("client %d (%s) did not reach ready+caught-up after %s (bound %s): %v — status %+v",
					sc.index, sc.name, phase, timeout, err, sc.c.Status())
			}
		}(sc)
	}
	wg.Wait()
}

// heldPoll is how often awaitHeld re-reads a client that is still missing
// something. Convergence is normally immediate, so one read is the usual cost.
const heldPoll = 50 * time.Millisecond

// awaitHeld is the second half of the final settle (CANT-172): it waits, per
// client and under the same bound as awaitFinalSettle, until the client holds
// every message the server's log has for it.
//
// READY+CAUGHT-UP IS NOT ENOUGH ON ITS OWN. A kill-phase send can be acked by
// the restarted server just before the background senders stop, and its live
// fan-out — commit, pg_notify, the hub's own fetch, a socket write per member —
// runs behind that ack. A live frame is not a catch-up trigger, so every client
// whose reconnect /sync already finished reads as settled while the frame is
// still on its way, and a Compare taken then reports the message Lost:
// verdict=server_failure for a message that arrives a few milliseconds later.
//
// ONLY LOST IS WAITED ON, because only Lost can heal by waiting; a phantom, a
// duplicate or a seq conflict is final the moment it is held. And a timeout
// here is NOT a harness error: a message still missing when the bound expires
// is the finding itself, and compareAll reports it as Lost. The wait can only
// take away a comparison made too early, never a real loss.
func (h *harness) awaitHeld(ctx context.Context, clients []*soakClient) {
	actx, cancel := context.WithTimeout(ctx, max(h.cfg.AwaitTimeout, minFinalSettle))
	defer cancel()
	var wg sync.WaitGroup
	for _, sc := range clients {
		wg.Add(1)
		go func(sc *soakClient) {
			defer wg.Done()
			for {
				rows, err := h.serverLogFor(actx, sc.userID)
				if err == nil {
					var snap client.Snapshot
					if snap, err = sc.snapshot(); err == nil && len(client.Compare(rows, snap).Lost) == 0 {
						return
					}
				}
				// A read that failed is compareAll's to record; this wait
				// just tries again until its bound, like any other miss.
				select {
				case <-actx.Done():
					h.logf("a client still misses messages at the end of the final settle; compareAll reports them",
						"client", sc.index, "name", sc.name)
					return
				case <-time.After(heldPoll):
				}
			}
		}(sc)
	}
	wg.Wait()
}

func (h *harness) harnessError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	h.cfg.logger().Warn("harness error", "detail", msg)
	h.mu.Lock()
	h.errs = append(h.errs, msg)
	h.mu.Unlock()
}

// finish assembles the final Report from whatever state the run reached,
// classifies it, and — if -out was given — tries to persist it. A write
// failure there is itself a harness failure (CLAUDE.md's "a journal write
// error"): it cannot retroactively rewrite the file that failed to hold it,
// so it can only escalate an otherwise-clean in-memory verdict, which is
// still the honest answer — a clean run this harness could not record is not
// a run anyone can point to as evidence.
func (h *harness) finish(rep *Report) Result {
	rep.Duration = time.Since(rep.StartedAt)
	computeAggregate(rep)

	h.mu.Lock()
	rep.HarnessErrors = append([]string(nil), h.errs...)
	h.mu.Unlock()

	h.hello.mu.Lock()
	rep.Hello = HelloHistogram{
		Outcomes: maps.Clone(h.hello.outcomes),
		Deltas:   append([]int64(nil), h.hello.deltas...),
	}
	h.hello.mu.Unlock()
	rep.Hello.Buckets = bucketizeDeltas(rep.Hello.Deltas)

	rep.Verdict = classify(rep)

	if h.cfg.ReportPath != "" {
		if err := writeReportFile(h.cfg.ReportPath, rep); err != nil {
			msg := fmt.Sprintf("write the report to %s: %v", h.cfg.ReportPath, err)
			h.cfg.logger().Warn("harness error", "detail", msg)
			rep.HarnessErrors = append(rep.HarnessErrors, msg)
			if rep.Verdict == VerdictPass {
				rep.Verdict = VerdictHarnessFailure
			}
		}
	}
	return Result{Report: *rep, Verdict: rep.Verdict}
}

func (h *harness) cleanup() {
	if h.proc != nil {
		h.killServer()
	}
	for _, f := range h.logFiles {
		_ = f.Close()
	}
	if h.pool != nil {
		h.pool.Close()
	}
}
