package hub

// The hub against an in-memory peer and a fake store: every decision it
// makes, with the one peer it exists for — the one that stops reading. The
// real path, over real sockets and Postgres, is cmd/catenary's.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// --- fakes -----------------------------------------------------------------

// peer is an in-memory Conn. Frames land on `got`; Close and CloseNow are
// recorded and, as the door's read loop would, run the detach the test
// registered so the hub sees the session end.
type peer struct {
	id, user, device uuid.UUID
	got              chan wire.ServerFrame
	// block, when non-nil, makes Send wait on it: the peer that stopped
	// reading.
	block chan struct{}

	mu       sync.Mutex
	closes   []websocket.StatusCode
	closeNow int
	detach   func()
}

func newPeer(user uuid.UUID) *peer {
	return &peer{id: uuid.New(), user: user, device: uuid.New(), got: make(chan wire.ServerFrame, 1024)}
}

func (p *peer) SessionID() uuid.UUID { return p.id }
func (p *peer) UserID() uuid.UUID    { return p.user }
func (p *peer) DeviceID() uuid.UUID  { return p.device }

func (p *peer) Send(ctx context.Context, f wire.ServerFrame) error {
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.got <- f
	return nil
}

func (p *peer) Close(status websocket.StatusCode, _ string) error {
	p.mu.Lock()
	p.closes = append(p.closes, status)
	d := p.detach
	p.mu.Unlock()
	if d != nil {
		d()
	}
	return nil
}

func (p *peer) CloseNow() error {
	p.mu.Lock()
	p.closeNow++
	d := p.detach
	p.mu.Unlock()
	if d != nil {
		d()
	}
	return nil
}

func (p *peer) closedWith() []websocket.StatusCode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]websocket.StatusCode(nil), p.closes...)
}

func (p *peer) closedNow() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeNow
}

// next reads the peer's next frame or fails.
func (p *peer) next(t *testing.T) wire.ServerFrame {
	t.Helper()
	select {
	case f := <-p.got:
		return f
	case <-time.After(5 * time.Second):
		t.Fatalf("peer %s: no frame", p.user)
		return nil
	}
}

// nothing asserts no frame arrives within a short window. Used only after a
// marker has proved the hub is past the point that would have produced one.
func (p *peer) nothing(t *testing.T) {
	t.Helper()
	select {
	case f := <-p.got:
		t.Fatalf("peer %s: unexpected frame %+v", p.user, f)
	case <-time.After(100 * time.Millisecond):
	}
}

type fakeStore struct {
	fanout   func(ctx context.Context, conv uuid.UUID, seq int64) (store.FanoutMessage, error)
	members  func(ctx context.Context, conv uuid.UUID) ([]uuid.UUID, error)
	markRead func(ctx context.Context, conv, user uuid.UUID, upToSeq int64) (store.ReadReceipt, error)
	head     func(ctx context.Context) (int64, error)

	fanouts, heads atomic.Int32
}

func (f *fakeStore) MessageForFanout(ctx context.Context, conv uuid.UUID, seq int64) (store.FanoutMessage, error) {
	f.fanouts.Add(1)
	return f.fanout(ctx, conv, seq)
}
func (f *fakeStore) Members(ctx context.Context, conv uuid.UUID) ([]uuid.UUID, error) {
	return f.members(ctx, conv)
}
func (f *fakeStore) MarkRead(ctx context.Context, conv, user uuid.UUID, upToSeq int64) (store.ReadReceipt, error) {
	return f.markRead(ctx, conv, user, upToSeq)
}
func (f *fakeStore) Head(ctx context.Context) (int64, error) {
	f.heads.Add(1)
	return f.head(ctx)
}

// recorder keeps every log record so a test can assert on levels.
type recorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

func (r *recorder) atLeast(level slog.Level) []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []slog.Record
	for _, rec := range r.recs {
		if rec.Level >= level {
			out = append(out, rec)
		}
	}
	return out
}

func (r *recorder) attr(rec slog.Record, key string) any {
	var v any
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.Any()
			return false
		}
		return true
	})
	return v
}

// --- fixture ---------------------------------------------------------------

type fixture struct {
	t     *testing.T
	hub   *Hub
	st    *fakeStore
	log   *recorder
	conv  uuid.UUID
	ada   uuid.UUID
	theo  uuid.UUID
	mal   uuid.UUID
	seq   int64
	peers []*peer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, conv: uuid.New(), ada: uuid.New(), theo: uuid.New(), mal: uuid.New(), log: &recorder{}}
	f.st = &fakeStore{
		members: func(_ context.Context, conv uuid.UUID) ([]uuid.UUID, error) {
			if conv != f.conv {
				return nil, nil
			}
			return []uuid.UUID{f.ada, f.theo}, nil
		},
		head: func(context.Context) (int64, error) { return 900, nil },
		markRead: func(_ context.Context, _, user uuid.UUID, upTo int64) (store.ReadReceipt, error) {
			return store.ReadReceipt{UserID: user, UpToSeq: upTo, Advanced: true}, nil
		},
	}
	f.st.fanout = func(_ context.Context, conv uuid.UUID, seq int64) (store.FanoutMessage, error) {
		return f.message(seq), nil
	}
	f.hub = New(f.st, slog.New(f.log), func(k string) string { return k })
	return f
}

// message is a row Ada wrote, with Theo's mark at 5 and read_by 1.
func (f *fixture) message(seq int64) store.FanoutMessage {
	text := "hello"
	cid := uuid.New()
	return store.FanoutMessage{
		Message: store.MessageRow{
			ID: uuid.New(), ConversationID: f.conv, AuthorID: f.ada, Seq: seq, LogSeq: seq * 10,
			At: time.Now(), Text: &text, ClientID: &cid,
		},
		ReadBy:  1,
		Members: []store.Member{{UserID: f.ada, ReadSeq: 0}, {UserID: f.theo, ReadSeq: 5}},
	}
}

// attach registers a peer and wires its Close/CloseNow to the detach, as
// the door's read loop ending would.
func (f *fixture) attach(user uuid.UUID) *peer {
	p := newPeer(user)
	detach := f.hub.Attach(p)
	p.mu.Lock()
	p.detach = detach
	p.mu.Unlock()
	f.peers = append(f.peers, p)
	return p
}

func (f *fixture) notify(seq int64) {
	f.hub.OnNotify(context.Background(), store.NotifyPayload{ConversationID: f.conv, Seq: seq})
}

func asMessage(t *testing.T, fr wire.ServerFrame) wire.Message {
	t.Helper()
	m, ok := fr.(wire.ServerMessageFrame)
	if !ok {
		t.Fatalf("frame = %T, want message", fr)
	}
	return m.Message
}

// --- fan-out ---------------------------------------------------------------

// One Message per member, state per viewer, client_id to the author only,
// nothing to a non-member.
func TestFanOutIsPerViewerAndReachesOnlyMembers(t *testing.T) {
	f := newFixture(t)
	ada1, ada2 := f.attach(f.ada), f.attach(f.ada)
	theo := f.attach(f.theo)
	mal := f.attach(f.mal)

	f.notify(3)
	for _, p := range []*peer{ada1, ada2} {
		m := asMessage(t, p.next(t))
		if m.State != wire.DeliveryStateSent || m.ClientID == nil || m.ReadBy == nil || *m.ReadBy != 1 {
			t.Errorf("author's view = state %s client_id %v read_by %v; want sent, echoed, 1", m.State, m.ClientID, m.ReadBy)
		}
	}
	// seq 3 is under Theo's mark of 5: read, and no client_id.
	m := asMessage(t, theo.next(t))
	if m.State != wire.DeliveryStateRead || m.ClientID != nil {
		t.Errorf("theo's view = state %s client_id %v; want read and no client_id", m.State, m.ClientID)
	}
	f.notify(7)
	if m := asMessage(t, theo.next(t)); m.State != wire.DeliveryStateDelivered {
		t.Errorf("theo's view of seq 7 = %s, want delivered (above his mark)", m.State)
	}
	ada1.next(t)
	ada2.next(t)
	mal.nothing(t)
}

// Criterion 3: no cursor, no seen-set. A frame with an older log_seq than
// one already delivered goes through.
func TestAReEmissionWithAnOldLogSeqIsDelivered(t *testing.T) {
	f := newFixture(t)
	theo := f.attach(f.theo)
	f.notify(9)
	f.notify(2)
	f.notify(9)
	for _, want := range []int64{90, 20, 90} {
		if m := asMessage(t, theo.next(t)); int64(m.LogSeq) != want {
			t.Errorf("log_seq = %d, want %d", m.LogSeq, want)
		}
	}
}

// Criterion 10.
func TestDetachRemovesTheSession(t *testing.T) {
	f := newFixture(t)
	theo := f.attach(f.theo)
	f.notify(1)
	theo.next(t)
	f.peers[0].detach()
	if n := f.hub.Attached(); n != 0 {
		t.Fatalf("attached = %d after detach, want 0", n)
	}
	f.notify(2)
	theo.nothing(t)
	if recs := f.log.atLeast(slog.LevelWarn); len(recs) != 0 {
		t.Errorf("%d line(s) at WARN or above after a clean detach: %v", len(recs), recs[0].Message)
	}
}

// Criterion 8, ruling 0: the peer that stopped reading is severed with
// CloseNow once its outbox is full, the readers get every frame in order,
// and no enqueue blocked.
func TestASlowConsumerIsSeveredWithoutBlockingTheEnqueuer(t *testing.T) {
	f := newFixture(t)
	stuck := f.attach(f.theo)
	stuck.block = make(chan struct{})
	r1, r2 := f.attach(f.ada), f.attach(f.ada)

	const frames = outboxBound + 20
	var slowest time.Duration
	for i := 1; i <= frames; i++ {
		start := time.Now()
		f.notify(int64(i))
		if d := time.Since(start); d > slowest {
			slowest = d
		}
	}
	if slowest > 100*time.Millisecond {
		t.Errorf("an enqueue blocked for %v; the sever must not wait on the peer", slowest)
	}
	deadline := time.Now().Add(5 * time.Second)
	for stuck.closedNow() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := stuck.closedNow(); n != 1 {
		t.Errorf("CloseNow called %d time(s) on the stuck peer, want 1", n)
	}
	if got := stuck.closedWith(); len(got) != 0 {
		t.Errorf("the stuck peer got a close handshake %v; a peer that is not reading gets CloseNow", got)
	}
	if n := f.hub.Attached(); n != 2 {
		t.Errorf("attached = %d after the sever, want the two readers", n)
	}
	for _, r := range []*peer{r1, r2} {
		for i := 1; i <= frames; i++ {
			if m := asMessage(t, r.next(t)); int64(m.Seq) != int64(i) {
				t.Fatalf("reader got seq %d, want %d: order or loss", m.Seq, i)
			}
		}
	}
	warns := f.log.atLeast(slog.LevelWarn)
	if len(warns) != 1 || warns[0].Message != "slow consumer severed" {
		t.Errorf("warn lines = %d, want exactly the sever", len(warns))
	}
	close(stuck.block)
}

// --- retries and the gap path ------------------------------------------------

// Criterion 5, first line: a transient failure that then succeeds delivers
// the frame and runs no gap.
func TestATransientLoadFailureIsRetried(t *testing.T) {
	f := newFixture(t)
	theo := f.attach(f.theo)
	var calls atomic.Int32
	f.st.fanout = func(_ context.Context, _ uuid.UUID, seq int64) (store.FanoutMessage, error) {
		if calls.Add(1) == 1 {
			return store.FanoutMessage{}, context.DeadlineExceeded // transient by the classifier
		}
		return f.message(seq), nil
	}
	f.notify(4)
	if m := asMessage(t, theo.next(t)); m.Seq != 4 {
		t.Errorf("seq = %d, want 4", m.Seq)
	}
	if f.st.heads.Load() != 0 {
		t.Error("a gap ran for a failure that was one retry away")
	}
	if recs := f.log.atLeast(slog.LevelWarn); len(recs) != 0 {
		t.Errorf("%d line(s) at WARN or above for a recovered retry", len(recs))
	}
}

// Criterion 5, second line: a permanent error is a gap at once.
func TestAPermanentLoadFailureIsAGap(t *testing.T) {
	f := newFixture(t)
	theo, ada := f.attach(f.theo), f.attach(f.ada)
	f.st.fanout = func(context.Context, uuid.UUID, int64) (store.FanoutMessage, error) {
		return store.FanoutMessage{}, errors.New("column does not exist")
	}
	start := time.Now()
	f.notify(1)
	if time.Since(start) > time.Second {
		t.Error("a permanent error was retried")
	}
	for _, p := range []*peer{theo, ada} {
		r, ok := p.next(t).(wire.ServerResyncRequired)
		if !ok || r.Reason != wire.ResyncReasonCursorTooOld || r.LogSeq != 900 {
			t.Errorf("frame = %+v, want resync_required{cursor_too_old, 900}", r)
		}
	}
	if f.st.fanouts.Load() != 1 {
		t.Errorf("fan-out attempted %d times, want 1", f.st.fanouts.Load())
	}
}

// Criterion 5, third line: a transient failure past the budget is a gap,
// reached in a handful of attempts rather than a spin.
func TestALoadFailingPastTheBudgetIsAGap(t *testing.T) {
	f := newFixture(t)
	theo := f.attach(f.theo)
	f.st.fanout = func(context.Context, uuid.UUID, int64) (store.FanoutMessage, error) {
		return store.FanoutMessage{}, context.DeadlineExceeded
	}
	start := time.Now()
	f.notify(1)
	if d := time.Since(start); d < loadBudget-retryMax || d > loadBudget+time.Second {
		t.Errorf("gap after %v, want about the %v budget", d, loadBudget)
	}
	if n := f.st.fanouts.Load(); n >= 10 {
		t.Errorf("%d attempts in the budget; the loop spun", n)
	}
	if _, ok := theo.next(t).(wire.ServerResyncRequired); !ok {
		t.Error("no resync_required after the budget")
	}
}

// Criterion 5, fourth line: head unreadable at gap severs with 1012 and
// writes no frame.
func TestHeadUnreadableAtGapSeversWith1012(t *testing.T) {
	f := newFixture(t)
	theo, ada := f.attach(f.theo), f.attach(f.ada)
	f.st.head = func(context.Context) (int64, error) { return 0, errors.New("relation does not exist") }
	f.hub.OnGap(context.Background())
	for _, p := range []*peer{theo, ada} {
		deadline := time.Now().Add(5 * time.Second)
		for len(p.closedWith()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := p.closedWith(); len(got) != 1 || got[0] != websocket.StatusServiceRestart {
			t.Errorf("peer closed with %v, want [1012]", got)
		}
		p.nothing(t)
	}
	if recs := f.log.atLeast(slog.LevelError); len(recs) != 1 {
		t.Errorf("error lines = %d, want the one sever line", len(recs))
	}
}

// Criterion 5, last line: a cancelled context is the process stopping, not
// a gap. Nothing is severed, nothing is logged above debug, and the call
// returns at once.
func TestACancelledContextIsNeitherARetryNorAGap(t *testing.T) {
	f := newFixture(t)
	theo := f.attach(f.theo)
	f.st.fanout = func(context.Context, uuid.UUID, int64) (store.FanoutMessage, error) {
		return store.FanoutMessage{}, context.Canceled
	}
	f.st.head = func(context.Context) (int64, error) { return 0, context.Canceled }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	f.hub.OnNotify(ctx, store.NotifyPayload{ConversationID: f.conv, Seq: 1})
	f.hub.OnGap(ctx)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("took %v on a cancelled context, want at once", d)
	}
	if n := f.st.fanouts.Load() + f.st.heads.Load(); n > 2 {
		t.Errorf("%d store calls on a cancelled context; nothing should be retried", n)
	}
	theo.nothing(t)
	if got := theo.closedWith(); len(got) != 0 || theo.closedNow() != 0 {
		t.Errorf("a session was closed (%v, now=%d) on a cancelled context", got, theo.closedNow())
	}
	if recs := f.log.atLeast(slog.LevelWarn); len(recs) != 0 {
		t.Errorf("%d line(s) at WARN or above on a cancelled context: %s", len(recs), recs[0].Message)
	}
}

// The gap itself: one resync_required per attached session, one WARN.
func TestAGapNotifiesEverySessionOnce(t *testing.T) {
	f := newFixture(t)
	ps := []*peer{f.attach(f.theo), f.attach(f.ada), f.attach(f.ada)}
	f.hub.OnGap(context.Background())
	for _, p := range ps {
		r, ok := p.next(t).(wire.ServerResyncRequired)
		if !ok || r.LogSeq != 900 {
			t.Errorf("frame = %+v, want resync_required at head 900", r)
		}
		p.nothing(t)
		if len(p.closedWith()) != 0 || p.closedNow() != 0 {
			t.Error("a gap closed a session")
		}
	}
	warns := f.log.atLeast(slog.LevelWarn)
	if len(warns) != 1 || warns[0].Message != "listener gap" ||
		f.log.attr(warns[0], "sessions_notified") != int64(3) || f.log.attr(warns[0], "head") != int64(900) {
		t.Errorf("warn line = %+v, want listener gap with sessions_notified=3 head=900", warns)
	}
}

// --- read and typing -----------------------------------------------------------

// A receipt goes to every attached member session, the origin included, and
// only when the mark moved. A refusal puts nothing on the wire (ruling 1).
func TestReadFansOutAReceiptOnlyWhenTheMarkMoved(t *testing.T) {
	f := newFixture(t)
	theo, ada := f.attach(f.theo), f.attach(f.ada)
	mal := f.attach(f.mal)
	frame := wire.ClientRead{ConversationID: wire.Uuid(f.conv.String()), UpToSeq: 3}

	f.hub.Handle(context.Background(), theo, frame)
	for _, p := range []*peer{theo, ada} {
		r, ok := p.next(t).(wire.ServerReceipt)
		if !ok || string(r.UserID) != f.theo.String() || r.UpToSeq != 3 {
			t.Errorf("frame = %+v, want receipt{theo, 3}", r)
		}
	}
	mal.nothing(t)

	f.st.markRead = func(_ context.Context, _, user uuid.UUID, _ int64) (store.ReadReceipt, error) {
		return store.ReadReceipt{UserID: user, UpToSeq: 3, Advanced: false}, nil
	}
	f.hub.Handle(context.Background(), theo, frame)
	theo.nothing(t)
	ada.nothing(t)

	f.st.markRead = func(context.Context, uuid.UUID, uuid.UUID, int64) (store.ReadReceipt, error) {
		return store.ReadReceipt{}, errors.New("not a member")
	}
	f.hub.Handle(context.Background(), mal, frame)
	mal.nothing(t)
	theo.nothing(t)
	if len(mal.closedWith()) != 0 {
		t.Error("a refused read closed the session")
	}
}

// Ruling 2: entries by session, list derived per user in start order.
func TestTypingIsKeyedBySessionAndDerivedPerUser(t *testing.T) {
	f := newFixture(t)
	laptop, phone := f.attach(f.ada), f.attach(f.ada)
	theo := f.attach(f.theo)
	mal := f.attach(f.mal)
	conv := wire.Uuid(f.conv.String())
	start := wire.ClientTyping{ConversationID: conv, State: wire.TypingStateStart}
	stop := wire.ClientTyping{ConversationID: conv, State: wire.TypingStateStop}
	ids := func(fr wire.ServerFrame) []wire.Uuid {
		ty, ok := fr.(wire.ServerTyping)
		if !ok {
			t.Fatalf("frame = %T, want typing", fr)
		}
		return ty.UserIds
	}
	adaID, theoID := wire.Uuid(f.ada.String()), wire.Uuid(f.theo.String())
	ctx := context.Background()

	// Ada starts on the laptop: Theo and Ada's phone hear; the laptop does not.
	f.hub.Handle(ctx, laptop, start)
	if got := ids(theo.next(t)); len(got) != 1 || got[0] != adaID {
		t.Errorf("theo sees %v, want [ada]", got)
	}
	phone.next(t)
	laptop.nothing(t)
	mal.nothing(t)

	// Theo starts: order is start order.
	time.Sleep(2 * time.Millisecond)
	f.hub.Handle(ctx, theo, start)
	if got := ids(laptop.next(t)); len(got) != 2 || got[0] != adaID || got[1] != theoID {
		t.Errorf("laptop sees %v, want [ada, theo]", got)
	}
	phone.next(t)

	// Ada stops on the laptop.
	f.hub.Handle(ctx, laptop, stop)
	if got := ids(theo.next(t)); len(got) != 1 || got[0] != theoID {
		t.Errorf("theo sees %v after ada's stop, want [theo]", got)
	}
	phone.next(t)

	// Theo's socket closes: his entry goes with it.
	f.peers[2].detach()
	if got := ids(laptop.next(t)); len(got) != 0 {
		t.Errorf("laptop sees %v after theo detached, want []", got)
	}
	phone.next(t)

	// Two devices: Ada types on the laptop with the phone attached; the
	// laptop detaches; the phone holds no entry, so the list is empty.
	theo = f.attach(f.theo)
	f.hub.Handle(ctx, laptop, start)
	theo.next(t)
	phone.next(t)
	f.peers[0].detach()
	if got := ids(theo.next(t)); len(got) != 0 {
		t.Errorf("theo sees %v after the typing device detached, want []", got)
	}
	phone.next(t) // Ada's other device hears the room's list too

	// A non-member's typing reaches nobody and logs one WARN.
	f.hub.Handle(ctx, mal, start)
	theo.nothing(t)
	phone.nothing(t)
	warns := f.log.atLeast(slog.LevelWarn)
	if len(warns) != 1 || warns[0].Message != "typing from a non-member" {
		t.Errorf("warn lines = %d, want the one non-member line", len(warns))
	}
}

// --- shutdown ------------------------------------------------------------------

func TestShutdownCloses1001AndWaitsForEveryDetach(t *testing.T) {
	f := newFixture(t)
	ps := []*peer{f.attach(f.theo), f.attach(f.ada)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.hub.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	for _, p := range ps {
		if got := p.closedWith(); len(got) != 1 || got[0] != websocket.StatusGoingAway {
			t.Errorf("peer closed with %v, want [1001]", got)
		}
	}
	if n := f.hub.Attached(); n != 0 {
		t.Errorf("attached = %d after shutdown, want 0", n)
	}
	// A session racing the drain is closed at once and never indexed.
	late := newPeer(f.theo)
	detach := f.hub.Attach(late)
	deadline := time.Now().Add(5 * time.Second)
	for len(late.closedWith()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := late.closedWith(); len(got) != 1 || got[0] != websocket.StatusGoingAway {
		t.Errorf("late attach closed with %v, want [1001]", got)
	}
	detach()
	if n := f.hub.Attached(); n != 0 {
		t.Errorf("attached = %d after a late attach, want 0", n)
	}
}

// A peer that never completes the handshake does not hold the drain past
// its deadline.
func TestShutdownGivesUpAtTheDeadline(t *testing.T) {
	f := newFixture(t)
	p := f.attach(f.theo)
	p.mu.Lock()
	p.detach = nil // the read loop never ends
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := f.hub.Shutdown(ctx); err == nil {
		t.Error("shutdown returned nil with a session that never detached")
	}
}
