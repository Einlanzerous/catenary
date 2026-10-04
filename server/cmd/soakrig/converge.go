package main

// CANT-46 — the cross-client convergence rig: one person, two devices running
// two DIFFERENT client implementations, each held off the network in turn
// while the other reads and a second person writes, and then judged AGAINST
// EACH OTHER as well as against the server.
//
// The soak judges each client against the server (client.Compare over
// serverLogFor). That cannot see two devices of one person disagreeing about
// state Compare does not read — an unread position, a conversation one of
// them never met — and it never has two devices of one person at all. This
// rig does, and client.SameState is the comparison it adds.
//
// THE CAST IS FRESH FOR EVERY RUN. Person P has two devices, A and B. Person
// Q has one Go client and is the author. A group room holds both. Every run
// provisions its own P, Q and room against the one server the lane started,
// so no run inherits another's conversations — which S2 depends on, since
// POST /conversations/direct is find-or-create and a second run with the same
// pair would open nothing.
//
// THE PARTITION IS THE RIG'S. Each device is handed its own partitionProxy as
// its base URL, so hold() cuts its socket and its catch-up together whatever
// the implementation behind it (partitionproxy.go). While held, the device
// keeps running and keeps redialing; every dial is refused. That is the
// difference from a kill, and why S4 is its own schedule.
//
// EVERY RUN OPENS THE SAME WAY: Q puts one message in the room, the rig
// settles, and both devices must hold the room and that message and agree
// under SameState before anything is held. The room is created by INSERT and
// an empty room with no marker is never on a page, so without the opening a
// held device would have no record for KeepHeldConversation to keep.
//
// SETTLING, at the opening and at the end, in this order:
//
//  1. The rig walks /sync for P from after=0 to has_more:false. The last
//     page's log_seq is H, and the last Conversation served per id is the
//     server's own answer for comparison 3.
//  2. `catchup` on each device.
//  3. A bounded wait for each device to be ready and caught up with a cursor
//     at or above H.
//  4. Snapshot.
//
// Step 2 is not a thumb on the scale. A cursor moves on a page and on nothing
// else, and a message from somebody else is not a catch-up trigger, so a
// device that took the latest messages live sits below H, holding a
// Conversation as of the last page that carried it, until it is asked.
//
// EACH SCHEDULE PROVES ITS PARTITION BIT BEFORE IT HEALS: the proxy has
// refused a dial since hold(), the device is not ready, and the device is
// missing something the server has committed for P. Every schedule sends
// while a device is held, so the third is true by construction, and a
// partition that let traffic through fails it — the soak's snapshotLostCount
// rule ("zero here would mean the kill landed on a quiet log and proved
// nothing").
//
// THREE COMPARISONS, all required to have run: A against B (SameState); each
// device against the server (the unchanged client.Compare); and each device's
// first_unread_seq per conversation against the Conversation the server last
// served in settling's walk — two devices that agree with each other and are
// both wrong pass the first alone. A comparison that did not run is a harness
// failure, never a pass.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// ConvergeVerdict is the rig's answer for one run.
type ConvergeVerdict string

const (
	// VerdictConverged: the opening held, every partition bit was proven,
	// every scheduled comparison ran, and every comparison is clean.
	VerdictConverged ConvergeVerdict = "converged"
	// VerdictDiverged: the run is conclusive and a comparison is not clean.
	VerdictDiverged ConvergeVerdict = "diverged"
	// VerdictConvergeHarnessFailure: the run proved nothing either way.
	VerdictConvergeHarnessFailure ConvergeVerdict = "harness_failure"
)

const (
	scheduleS1 = "S1"
	scheduleS2 = "S2"
	scheduleS3 = "S3"
	scheduleS4 = "S4"

	// convergeComparisons is how many comparisons a run owes: the pair, each
	// device against the server, and each device's markers.
	convergeComparisons = 5

	defaultSettleTimeout = 60 * time.Second
)

// convergeConfig is one run: a schedule and who device A and device B are.
type convergeConfig struct {
	Schedule string
	// A and B are the implementation behind each of P's devices: cohortGo or
	// cohortTS. A schedule is run in both role assignments, because a
	// one-directional pass is not convergence.
	A, B string

	// TSDriver is the built driver bundle and Node the node binary, for a
	// cohortTS device. Dir holds the devices' durable journals.
	TSDriver, Node, Dir string

	// FaultsB is set on device B. The fault control runs S1 with
	// KeepHeldConversation here.
	FaultsB client.Faults

	// SettleTimeout bounds every wait in the run; zero is the default.
	SettleTimeout time.Duration

	// The three plants, for the controls that prove the rig can fail. Test
	// only: a hold() that holds nothing, a pairwise comparison that is
	// skipped, and a device B that belongs to somebody who is not in the room.
	debugHoldIsNoOp   bool
	debugSkipPairwise bool
	debugStrangerB    bool
}

// ConvergeReport is one run's outcome.
type ConvergeReport struct {
	Schedule string
	A, B     string
	Verdict  ConvergeVerdict

	// Pairwise is SameState between the two devices; Relaunch is S4's check
	// that the device that came back is the device that left; Markers is each
	// device's first_unread_seq against the server's own last-served record.
	Pairwise, Relaunch, Markers []string
	CompareA, CompareB          client.Report

	ComparisonsRun int
	// Holds is how many times a device was held, and RefusedDials how many
	// dials the proxies refused while it was.
	Holds        int
	RefusedDials int64

	HarnessErrors []string
}

// Differences is every difference a conclusive run found.
func (r ConvergeReport) Differences() []string {
	var d []string
	d = append(d, r.Pairwise...)
	d = append(d, r.Relaunch...)
	d = append(d, r.Markers...)
	if !r.CompareA.Clean() {
		d = append(d, "device A against the server: "+r.CompareA.String())
	}
	if !r.CompareB.Clean() {
		d = append(d, "device B against the server: "+r.CompareB.String())
	}
	return d
}

// Line is the run in the grep-able shape verify.sh extracts.
func (r ConvergeReport) Line() string {
	return fmt.Sprintf("pair=%s roles=A:%s,B:%s schedule=%s verdict=%s differences=%d refused_dials=%d",
		pairName(r.A, r.B), r.A, r.B, r.Schedule, r.Verdict, len(r.Differences()), r.RefusedDials)
}

func (r ConvergeReport) String() string {
	var b strings.Builder
	b.WriteString(r.Line())
	fmt.Fprintf(&b, "\n  comparisons: %d/%d run · holds %d", r.ComparisonsRun, convergeComparisons, r.Holds)
	for _, d := range r.Differences() {
		fmt.Fprintf(&b, "\n  DIFFERS: %s", d)
	}
	for _, e := range r.HarnessErrors {
		fmt.Fprintf(&b, "\n  HARNESS: %s", e)
	}
	return b.String()
}

// pairName names a pair whichever way round its roles are.
func pairName(a, b string) string {
	p := []string{a, b}
	sort.Strings(p)
	return p[0] + "+" + p[1]
}

// classifyConverge is the verdict. HARNESS FAILURE FIRST: a run with a
// harness error, or one that owes a comparison, is inconclusive whatever the
// comparisons that did run say.
func classifyConverge(r *ConvergeReport) ConvergeVerdict {
	switch {
	case len(r.HarnessErrors) > 0 || r.ComparisonsRun < convergeComparisons:
		return VerdictConvergeHarnessFailure
	case len(r.Differences()) > 0:
		return VerdictDiverged
	}
	return VerdictConverged
}

// convergeDevice is one of P's two devices: a client of one implementation,
// behind its own partitionProxy, over a journal that outlives the client.
type convergeDevice struct {
	label  string // "A" or "B"
	impl   string
	enroll wire.EnrollResponse
	faults client.Faults
	proxy  *partitionProxy

	// The durable journal: a *client.Journal for Go, a file for TypeScript.
	journal     *client.Journal
	journalFile string
	script      string
	node        string

	c    cohortClient
	stop context.CancelFunc
	done chan struct{}
}

// launch starts a client over the device's journal. Called again after kill,
// it is the relaunch: a new Client over the same Journal, or a new driver
// process over the same file.
func (d *convergeDevice) launch() error {
	switch d.impl {
	case cohortGo:
		c, err := client.New(client.Config{
			BaseURL: d.proxy.base(), ClientInfo: "cant-46-converge", Journal: d.journal,
			Faults: d.faults, BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax,
		})
		if err != nil {
			return err
		}
		d.c = c
	case cohortTS:
		cfg := tsDriverConfig{
			Node: d.node, Script: d.script, BaseURL: d.proxy.base(), Faults: d.faults,
			BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax, JournalFile: d.journalFile,
		}
		cfg.enrolled(d.enroll)
		c, err := newTSDriver(cfg)
		if err != nil {
			return err
		}
		d.c = c
	default:
		return fmt.Errorf("no client implementation named %q", d.impl)
	}
	ctx, stop := context.WithCancel(context.Background())
	d.stop, d.done = stop, make(chan struct{})
	go func(c cohortClient, done chan struct{}) {
		defer close(done)
		_ = c.Run(ctx)
	}(d.c, d.done)
	return nil
}

// kill is `kill -9` on the device: nothing it had in flight reaches its
// journal afterward.
func (d *convergeDevice) kill() {
	if d.c == nil {
		return
	}
	d.c.Kill()
	d.stop()
	<-d.done
	d.c = nil
}

func (d *convergeDevice) snapshot() (client.Snapshot, error) {
	if d.c == nil {
		return client.Snapshot{}, errors.New("the device is not running")
	}
	if t, ok := d.c.(tsSnapshotter); ok {
		return t.TrySnapshot()
	}
	return d.c.Snapshot(), nil
}

// convergeRun is the state of one run.
type convergeRun struct {
	h   *harness
	cfg convergeConfig
	rep ConvergeReport

	p, q     uuid.UUID
	pHandle  string
	room     uuid.UUID
	a, b     *convergeDevice
	author   cohortClient
	stopQ    context.CancelFunc
	doneQ    chan struct{}
	rigToken wire.Token // P's third device: the rig's own view of what P is served
	qToken   wire.Token
}

// runConverge is one schedule, one role assignment, against the server h is
// already running.
func (h *harness) runConverge(ctx context.Context, cfg convergeConfig) ConvergeReport {
	if cfg.SettleTimeout == 0 {
		cfg.SettleTimeout = defaultSettleTimeout
	}
	r := &convergeRun{h: h, cfg: cfg, rep: ConvergeReport{Schedule: cfg.Schedule, A: cfg.A, B: cfg.B}}
	defer r.teardown()
	if r.setup(ctx) && r.schedule(ctx) {
		r.judge(ctx)
	}
	r.rep.Verdict = classifyConverge(&r.rep)
	return r.rep
}

func (r *convergeRun) fail(format string, args ...any) bool {
	r.rep.HarnessErrors = append(r.rep.HarnessErrors, fmt.Sprintf(format, args...))
	return false
}

func (r *convergeRun) teardown() {
	for _, d := range []*convergeDevice{r.a, r.b} {
		if d == nil {
			continue
		}
		d.kill()
		if d.proxy != nil {
			d.proxy.close()
		}
	}
	if r.author != nil {
		r.author.Kill()
		r.stopQ()
		<-r.doneQ
	}
}

// setup provisions the cast, launches the three clients and runs the opening.
func (r *convergeRun) setup(ctx context.Context) bool {
	h, run := r.h, uuid.NewString()[:8]
	r.p, r.q, r.room = uuid.New(), uuid.New(), uuid.New()
	r.pHandle = "converge-p-" + run
	if _, err := h.pool.Exec(ctx, `INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', $2)`, r.room, "converge room "+run); err != nil {
		return r.fail("create the room: %v", err)
	}
	members := []struct {
		id     uuid.UUID
		handle string
	}{{r.p, r.pHandle}, {r.q, "converge-q-" + run}}
	for _, m := range members {
		if err := insertUser(ctx, h.pool, m.id, m.handle, m.handle); err != nil {
			return r.fail("create %s: %v", m.handle, err)
		}
		if _, err := h.pool.Exec(ctx, `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`, r.room, m.id); err != nil {
			return r.fail("join %s to the room: %v", m.handle, err)
		}
	}

	// THE PLANTED STRANGER: device B enrolled for somebody who is not in the
	// room, so the opening leaves one device without it.
	bOwner := r.p
	if r.cfg.debugStrangerB {
		bOwner = uuid.New()
		if err := insertUser(ctx, h.pool, bOwner, "converge-stranger-"+run, "stranger"); err != nil {
			return r.fail("create the stranger: %v", err)
		}
	}

	rig, err := mintAndRedeem(ctx, h.store, h.baseURL, nil, r.p, "the rig")
	if err != nil {
		return r.fail("enroll the rig's own device for P: %v", err)
	}
	r.rigToken = rig.AccessToken

	var ok bool
	if r.a, ok = r.device(ctx, "A", r.cfg.A, r.p, client.Faults{}); !ok {
		return false
	}
	if r.b, ok = r.device(ctx, "B", r.cfg.B, bOwner, r.cfg.FaultsB); !ok {
		return false
	}
	r.b.proxy.debugHoldIsNoOp = r.cfg.debugHoldIsNoOp

	qDev, err := mintAndRedeem(ctx, h.store, h.baseURL, nil, r.q, "Q")
	if err != nil {
		return r.fail("enroll Q: %v", err)
	}
	r.qToken = qDev.AccessToken
	qj := client.NewJournal()
	cred, err := client.CredentialFromEnroll(qDev)
	if err == nil {
		err = qj.Enroll(cred)
	}
	if err != nil {
		return r.fail("enroll Q's journal: %v", err)
	}
	q, err := client.New(client.Config{BaseURL: h.baseURL, ClientInfo: "cant-46-converge-author", Journal: qj, BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax})
	if err != nil {
		return r.fail("construct Q: %v", err)
	}
	qctx, stopQ := context.WithCancel(context.Background())
	r.author, r.stopQ, r.doneQ = q, stopQ, make(chan struct{})
	go func() { defer close(r.doneQ); _ = q.Run(qctx) }()

	for _, d := range []*convergeDevice{r.a, r.b} {
		if err := d.launch(); err != nil {
			return r.fail("launch device %s (%s): %v", d.label, d.impl, err)
		}
	}
	for who, c := range map[string]cohortClient{"device A": r.a.c, "device B": r.b.c, "Q": r.author} {
		if err := r.await(ctx, c, func() bool { return c.Status().Ready }); err != nil {
			return r.fail("%s never became ready: %v", who, err)
		}
	}
	return r.opening(ctx)
}

// device enrolls one of P's devices and gives it a proxy and a journal.
func (r *convergeRun) device(ctx context.Context, label, impl string, owner uuid.UUID, faults client.Faults) (*convergeDevice, bool) {
	enrolled, err := mintAndRedeem(ctx, r.h.store, r.h.baseURL, nil, owner, "device "+label)
	if err != nil {
		return nil, r.fail("enroll device %s: %v", label, err)
	}
	proxy, err := newPartitionProxy(targetHost(r.h.baseURL))
	if err != nil {
		return nil, r.fail("start device %s's proxy: %v", label, err)
	}
	d := &convergeDevice{label: label, impl: impl, enroll: enrolled, faults: faults, proxy: proxy, script: r.cfg.TSDriver, node: r.cfg.Node}
	switch impl {
	case cohortGo:
		d.journal = client.NewJournal()
		cred, err := client.CredentialFromEnroll(enrolled)
		if err == nil {
			err = d.journal.Enroll(cred)
		}
		if err != nil {
			proxy.close()
			return nil, r.fail("enroll device %s's journal: %v", label, err)
		}
	case cohortTS:
		d.journalFile = filepath.Join(r.cfg.Dir, fmt.Sprintf("converge-%s-%s.json", label, uuid.NewString()[:8]))
	}
	return d, true
}

// opening gives every schedule one known start, and refuses a run that does
// not start equal.
func (r *convergeRun) opening(ctx context.Context) bool {
	first, ok := r.send(ctx, r.room)
	if !ok {
		return false
	}
	// Judged even when the settle timed out: a device that never got the room
	// is what the timeout was, and the report should say so.
	snaps, _, settled := r.settle(ctx)
	if !settled {
		var err error
		if snaps[0], err = r.a.snapshot(); err != nil {
			return r.fail("the opening: read device A: %v", err)
		}
		if snaps[1], err = r.b.snapshot(); err != nil {
			return r.fail("the opening: read device B: %v", err)
		}
	}
	ok = true
	for i, d := range []*convergeDevice{r.a, r.b} {
		if !holdsConversation(snaps[i], wid(r.room)) {
			ok = r.fail("the opening left device %s without the room", d.label)
		} else if !holdsMessage(snaps[i], first.MessageID) {
			ok = r.fail("the opening left device %s without the opening message", d.label)
		}
	}
	if !ok {
		return false
	}
	if d := client.SameState(snaps[0], snaps[1]); len(d) > 0 {
		return r.fail("the run does not start equal: %s", strings.Join(d, "; "))
	}
	return settled
}

// schedule is the part between the opening and the judgement.
func (r *convergeRun) schedule(ctx context.Context) bool {
	switch r.cfg.Schedule {
	case scheduleS1:
		return r.s1(ctx)
	case scheduleS2:
		return r.s2(ctx)
	case scheduleS3:
		return r.s3(ctx)
	case scheduleS4:
		return r.s4(ctx)
	}
	return r.fail("no schedule named %q", r.cfg.Schedule)
}

// S1 — B is held. Q sends two. A marks read up to the first of them. Q sends
// two more. B is healed. B learns an unread position it never saw move, from
// a catch-up alone (CANT-35 ruling 4 → B): its held record says
// first_unread_seq 1, and the truth after is 3.
func (r *convergeRun) s1(ctx context.Context) bool {
	r.hold(r.b)
	acks, ok := r.sendN(ctx, r.room, 2)
	if !ok {
		return false
	}
	next := acks[0].Seq + 1
	if !r.read(ctx, r.a, acks[0], func(m *wire.Seq) bool { return m != nil && *m == next }) {
		return false
	}
	if _, ok := r.sendN(ctx, r.room, 2); !ok {
		return false
	}
	return r.heal(ctx, r.b)
}

// S2 — B is held; A is not. The rig, with Q's credential, opens the direct
// P–Q conversation and Q sends its first message. A meets a new conversation
// LIVE, through the CANT-103 introduction its first message fires; B meets
// the same conversation on a catch-up page.
func (r *convergeRun) s2(ctx context.Context) bool {
	r.hold(r.b)
	direct, err := r.openDirect(ctx)
	if err != nil {
		return r.fail("S2: open the direct conversation: %v", err)
	}
	conv, err := uuid.Parse(string(direct))
	if err != nil {
		return r.fail("S2: the direct conversation's id: %v", err)
	}
	first, ok := r.send(ctx, conv)
	if !ok {
		return false
	}
	if err := r.awaitSnapshot(ctx, r.a, func(s client.Snapshot) bool {
		return holdsConversation(s, direct) && holdsMessage(s, first.MessageID)
	}); err != nil {
		return r.fail("S2: device A never held the direct conversation and its first message: %v", err)
	}
	held, err := r.b.snapshot()
	if err != nil {
		return r.fail("S2: read device B: %v", err)
	}
	if holdsConversation(held, direct) || holdsMessage(held, first.MessageID) {
		return r.fail("S2: device B holds the direct conversation or its first message while held")
	}
	return r.heal(ctx, r.b)
}

// S3 — both held. Q sends MaxSyncLimit + 1 messages: a backlog no single
// page can carry whatever limit a client asks for, so both catch up across
// has_more. A is healed, then B.
func (r *convergeRun) s3(ctx context.Context) bool {
	r.hold(r.a)
	r.hold(r.b)
	if _, ok := r.sendN(ctx, r.room, store.MaxSyncLimit+1); !ok {
		return false
	}
	if !r.heal(ctx, r.a) {
		return false
	}
	if err := r.await(ctx, r.a.c, func() bool { return r.a.c.Status().Ready }); err != nil {
		return r.fail("S3: device A never came back: %v", err)
	}
	return r.heal(ctx, r.b)
}

// S4 — B is held. Q sends two. The rig snapshots B, kills it, and relaunches
// it over its durable journal WHILE STILL HELD. Q sends two more and A marks
// read up to the last. B is healed. What S1 does not hold a client to: the
// client that returns is not the client that left, so it must resume from
// what it had persisted — the same snapshot under the strict SameView, cursor
// included, with the same wipe count — and then catch up from that cursor
// rather than bootstrap.
func (r *convergeRun) s4(ctx context.Context) bool {
	r.hold(r.b)
	if _, ok := r.sendN(ctx, r.room, 2); !ok {
		return false
	}
	before, err := r.b.snapshot()
	if err != nil {
		return r.fail("S4: read device B before the kill: %v", err)
	}
	r.b.kill()
	// From zero, so the partition bit below is proven by the RELAUNCHED
	// client's own refused dial and not by one the dead client made.
	r.rep.RefusedDials += r.b.proxy.resetRefused()
	if err := r.b.launch(); err != nil {
		return r.fail("S4: relaunch device B: %v", err)
	}
	acks, ok := r.sendN(ctx, r.room, 2)
	if !ok {
		return false
	}
	last := acks[1].Seq
	if !r.read(ctx, r.a, acks[1], func(m *wire.Seq) bool { return m == nil || *m > last }) {
		return false
	}
	// The partition bit first: it waits for the relaunched device to have
	// dialed and been refused, so the snapshot below is of a client that is
	// up and running over its journal, not one still opening it.
	if !r.partitionBit(ctx, r.b) {
		return false
	}
	after, err := r.b.snapshot()
	if err != nil {
		return r.fail("S4: read device B after the relaunch: %v", err)
	}
	r.rep.Relaunch = append(r.rep.Relaunch, prefix("S4 relaunch: ", client.SameView(before, after))...)
	if before.Wipes != after.Wipes {
		r.rep.Relaunch = append(r.rep.Relaunch, fmt.Sprintf("S4 relaunch: wipes %d before the kill, %d after", before.Wipes, after.Wipes))
	}
	r.b.proxy.heal()
	return true
}

func prefix(p string, lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = p + l
	}
	return out
}

func (r *convergeRun) hold(d *convergeDevice) {
	d.proxy.hold()
	r.rep.Holds++
}

// heal proves the device's partition bit and only then gives its network back.
func (r *convergeRun) heal(ctx context.Context, d *convergeDevice) bool {
	if !r.partitionBit(ctx, d) {
		return false
	}
	d.proxy.heal()
	return true
}

// partitionBit waits, bounded, for the proof that the partition was one: a
// dial refused since hold(), a device that is not ready, and a device missing
// something the server has committed for P.
func (r *convergeRun) partitionBit(ctx context.Context, d *convergeDevice) bool {
	var refused int64
	var ready bool
	lost := -1
	deadline := time.Now().Add(r.cfg.SettleTimeout)
	for {
		refused, ready = d.proxy.refusedDials(), d.c.Status().Ready
		if rows, err := r.h.serverLogFor(ctx, r.p); err == nil {
			if snap, err := d.snapshot(); err == nil {
				lost = len(client.Compare(rows, snap).Lost)
			}
		}
		if refused >= 1 && !ready && lost > 0 {
			r.rep.RefusedDials += refused
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return r.fail("device %s was held and its partition bit was never proven: %d dials refused, ready %v, %d committed messages missing — a partition that let traffic through, or one nothing was sent across",
				d.label, refused, ready, lost)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// send is one acked message from Q.
func (r *convergeRun) send(ctx context.Context, conv uuid.UUID) (wire.ServerAck, bool) {
	acks, ok := r.sendN(ctx, conv, 1)
	if !ok {
		return wire.ServerAck{}, false
	}
	return acks[0], true
}

func (r *convergeRun) sendN(ctx context.Context, conv uuid.UUID, n int) ([]wire.ServerAck, bool) {
	acks := make([]wire.ServerAck, 0, n)
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("%s · %d", r.cfg.Schedule, i)
		sctx, cancel := context.WithTimeout(ctx, r.cfg.SettleTimeout)
		ack, err := r.author.Send(sctx, wire.ClientSend{ClientID: wid(uuid.New()), ConversationID: wid(conv), Text: &text})
		cancel()
		if err != nil {
			return nil, r.fail("Q's send %d of %d: %v", i+1, n, err)
		}
		acks = append(acks, ack)
	}
	return acks, true
}

// read has device d mark the room read up to a message, once it holds that
// message, and waits until the marker the server serves P has moved past it,
// as `moved` judges. The wait is what makes the
// schedule's order real — a `read` is answered to nobody, so without it the
// next step could run before the server had taken the receipt.
func (r *convergeRun) read(ctx context.Context, d *convergeDevice, upTo wire.ServerAck, moved func(*wire.Seq) bool) bool {
	if err := r.awaitSnapshot(ctx, d, func(s client.Snapshot) bool { return holdsMessage(s, upTo.MessageID) }); err != nil {
		return r.fail("device %s never held the message it is to read up to: %v", d.label, err)
	}
	rctx, cancel := context.WithTimeout(ctx, r.cfg.SettleTimeout)
	defer cancel()
	if err := d.c.Read(rctx, wire.ClientRead{ConversationID: upTo.ConversationID, UpToSeq: upTo.Seq}); err != nil {
		return r.fail("device %s's read up to seq %d: %v", d.label, upTo.Seq, err)
	}
	for {
		_, served, err := syncWalk(rctx, r.h.baseURL, r.rigToken)
		if err == nil {
			if moved(served[upTo.ConversationID].FirstUnreadSeq) {
				return true
			}
		}
		if rctx.Err() != nil {
			return r.fail("device %s read up to seq %d and the server never served P the marker that follows (last error %v)", d.label, upTo.Seq, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// openDirect is POST /conversations/direct with Q's credential, naming P.
func (r *convergeRun) openDirect(ctx context.Context) (wire.Uuid, error) {
	body, err := json.Marshal(wire.DirectConversationRequest{Handle: r.pHandle})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.h.baseURL+"/conversations/direct", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(r.qToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, raw)
	}
	var c wire.Conversation
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("decode Conversation: %w", err)
	}
	return c.ID, nil
}

// settle is the one settling procedure, in the order the file header gives.
// It returns the two snapshots, A's then B's, and the Conversation the server
// last served P per id.
func (r *convergeRun) settle(ctx context.Context) ([2]client.Snapshot, map[wire.Uuid]wire.Conversation, bool) {
	var snaps [2]client.Snapshot
	sctx, cancel := context.WithTimeout(ctx, r.cfg.SettleTimeout)
	defer cancel()
	high, served, err := syncWalk(sctx, r.h.baseURL, r.rigToken)
	if err != nil {
		return snaps, nil, r.fail("settle: walk /sync for P: %v", err)
	}
	for _, d := range []*convergeDevice{r.a, r.b} {
		d.c.CatchUp()
	}
	for i, d := range []*convergeDevice{r.a, r.b} {
		if err := r.await(sctx, d.c, func() bool {
			s := d.c.Status()
			return s.Ready && s.CaughtUp && s.HasCursor && s.Cursor >= high
		}); err != nil {
			s := d.c.Status()
			return snaps, served, r.fail("settle: device %s never reached the server's high-water mark %d: ready %v, caught up %v, cursor %d (set %v)",
				d.label, high, s.Ready, s.CaughtUp, s.Cursor, s.HasCursor)
		}
		if snaps[i], err = d.snapshot(); err != nil {
			return snaps, served, r.fail("settle: read device %s: %v", d.label, err)
		}
	}
	return snaps, served, true
}

// judge settles and makes the three comparisons.
func (r *convergeRun) judge(ctx context.Context) {
	snaps, served, ok := r.settle(ctx)
	if !ok {
		return
	}
	if r.cfg.debugSkipPairwise {
		r.fail("the pairwise comparison was skipped: planted (test)")
	} else {
		r.rep.Pairwise = client.SameState(snaps[0], snaps[1])
		r.rep.ComparisonsRun++
	}

	rows, err := r.h.serverLogFor(ctx, r.p)
	if err != nil {
		r.fail("read the server log for P: %v", err)
		return
	}
	r.rep.CompareA, r.rep.CompareB = client.Compare(rows, snaps[0]), client.Compare(rows, snaps[1])
	r.rep.ComparisonsRun += 2

	for i, d := range []*convergeDevice{r.a, r.b} {
		r.rep.Markers = append(r.rep.Markers, markerDiffs("device "+d.label, snaps[i], served)...)
		r.rep.ComparisonsRun++
	}
}

// markerDiffs holds a device's first_unread_seq, per conversation, to the
// Conversation the server last served that person.
func markerDiffs(who string, s client.Snapshot, served map[wire.Uuid]wire.Conversation) []string {
	held := map[wire.Uuid]wire.Conversation{}
	for _, c := range s.Conversations {
		held[c.ID] = c
	}
	var diffs []string
	for id, want := range served {
		got, ok := held[id]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s does not hold conversation %s, which the server serves", who, id))
		case !sameSeq(got.FirstUnreadSeq, want.FirstUnreadSeq):
			diffs = append(diffs, fmt.Sprintf("%s holds conversation %s with FirstUnreadSeq %s; the server serves %s", who, id, showSeq(got.FirstUnreadSeq), showSeq(want.FirstUnreadSeq)))
		}
	}
	sort.Strings(diffs)
	return diffs
}

func sameSeq(a, b *wire.Seq) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func showSeq(s *wire.Seq) string {
	if s == nil {
		return "absent"
	}
	return fmt.Sprint(*s)
}

func holdsMessage(s client.Snapshot, id wire.Uuid) bool {
	for _, m := range s.Messages {
		if m.ID == id {
			return true
		}
	}
	return false
}

func holdsConversation(s client.Snapshot, id wire.Uuid) bool {
	for _, c := range s.Conversations {
		if c.ID == id {
			return true
		}
	}
	return false
}

// await is cohortClient.Await under the run's bound.
func (r *convergeRun) await(ctx context.Context, c cohortClient, pred func() bool) error {
	actx, cancel := context.WithTimeout(ctx, r.cfg.SettleTimeout)
	defer cancel()
	return c.Await(actx, pred)
}

// awaitSnapshot polls a device's journal until pred holds. Polled, because a
// TypeScript journal is read over stdio and nothing notifies across it.
func (r *convergeRun) awaitSnapshot(ctx context.Context, d *convergeDevice, pred func(client.Snapshot) bool) error {
	deadline := time.Now().Add(r.cfg.SettleTimeout)
	for {
		s, err := d.snapshot()
		if err == nil && pred(s) {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if err == nil {
				err = errors.New("timed out")
			}
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// syncWalk is the rig's own /sync for one credential, from after=0 followed
// page by page to has_more:false. It returns the last page's log_seq —
// "always the caller's new high-water mark", so it covers a receipt's marker
// as well as the messages — and the last Conversation served per id: the
// server's own answer, not any client's copy of it.
func syncWalk(ctx context.Context, baseURL string, token wire.Token) (int64, map[wire.Uuid]wire.Conversation, error) {
	served := map[wire.Uuid]wire.Conversation{}
	var after int64
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/sync?after=%d", baseURL, after), nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return 0, nil, fmt.Errorf("GET /sync: %s: %s", resp.Status, body)
		}
		var page wire.SyncResponse
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, nil, fmt.Errorf("decode SyncResponse: %w", err)
		}
		for _, c := range page.Conversations {
			served[c.ID] = c
		}
		if !page.HasMore {
			return int64(page.LogSeq), served, nil
		}
		if int64(page.LogSeq) <= after {
			return 0, nil, fmt.Errorf("GET /sync?after=%d: has_more with log_seq %d — the walk is not advancing", after, page.LogSeq)
		}
		after = int64(page.LogSeq)
	}
}
