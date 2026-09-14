package main

import (
	"context"
	"fmt"
	"maps"
	"os/exec"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/store"
)

// soakClient is one provisioned account's device: the client.Client running
// it and the identity the harness needs to query its own view of the log.
type soakClient struct {
	index  int
	name   string
	c      *client.Client
	j      *client.Journal
	userID uuid.UUID
}

// harness is the state one call to runSoak builds and tears down. Not reused
// across runs — a fresh harness per call, the way a fresh killRig backs every
// CANT-102 test.
type harness struct {
	cfg Config

	pool  *pgxpool.Pool
	store *store.Store

	bin     string
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
	rep := Report{StartedAt: time.Now(), N: cfg.N, Clients: make([]ClientReport, cfg.N)}
	for i := range rep.Clients {
		rep.Clients[i] = ClientReport{Index: i}
	}

	h := &harness{cfg: cfg, hello: newHelloHistogram()}
	defer h.cleanup()

	if err := h.cfg.validate(); err != nil {
		h.harnessError("config: %v", err)
		return h.finish(&rep)
	}

	bin, err := resolveBinary(h.cfg)
	if err != nil {
		h.harnessError("resolve the catenary binary: %v", err)
		return h.finish(&rep)
	}
	h.bin = bin

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
			_ = sc.c.Run(runCtx)
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
		if p.err != nil {
			h.harnessError("provision client %d (%s): %v", p.index, p.name, p.err)
			rep.Clients[p.index] = ClientReport{Index: p.index, DeviceName: p.name, ProvisionError: p.err.Error()}
			continue
		}
		j := client.NewJournal()
		c, err := client.New(client.Config{
			BaseURL: h.baseURL, AccessToken: string(p.enroll.AccessToken), DeviceID: p.enroll.DeviceID,
			ClientInfo: "cant-27-soakrig", Journal: j,
			Faults:     h.cfg.debugFaults[p.index],
			BackoffMin: 100 * time.Millisecond, BackoffMax: 2 * time.Second,
			// A client's own internal narration is per-heartbeat noise at N
			// clients; the report's Stats already summarize it. The
			// harness's own logger (h.cfg.Logger) is for THIS process's
			// progress, never handed down to a client.
			Logger: nil,
		})
		if err != nil {
			h.harnessError("construct client %d (%s): %v", p.index, p.name, err)
			rep.Clients[p.index] = ClientReport{Index: p.index, DeviceName: p.name, ProvisionError: err.Error()}
			continue
		}
		clients = append(clients, &soakClient{index: p.index, name: p.name, c: c, j: j, userID: p.userID})
		rep.Clients[p.index] = ClientReport{Index: p.index, DeviceName: p.name, Provisioned: true}
	}
	return clients
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
