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
	fanout     func(ctx context.Context, conv uuid.UUID, seq int64) (store.FanoutMessage, error)
	members    func(ctx context.Context, conv uuid.UUID) ([]uuid.UUID, error)
	markRead   func(ctx context.Context, conv, user uuid.UUID, upToSeq int64) (store.ReadReceipt, error)
	head       func(ctx context.Context) (int64, error)
	readNotify func(ctx context.Context, conv, reader uuid.UUID, before, after int64, capPerAuthor int) ([]store.ReadNotifyMessage, error)

	fanouts, heads, readNotifies atomic.Int32
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
func (f *fakeStore) MessagesForReadNotify(ctx context.Context, conv, reader uuid.UUID, before, after int64, capPerAuthor int) ([]store.ReadNotifyMessage, error) {
	f.readNotifies.Add(1)
	if f.readNotify == nil {
		return nil, nil
	}
	return f.readNotify(ctx, conv, reader, before, after, capPerAuthor)
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
//
// ON SEQ 1 IT CARRIES THE INTRODUCTION, because MessageForFanout does: the
// conversation as each member sees it (a direct, so each is named for the
// other) and both members' user records. A fake that answered otherwise would
// let the hub's gate pass a test the store could not.
func (f *fixture) message(seq int64) store.FanoutMessage {
	text := "hello"
	cid := uuid.New()
	fm := store.FanoutMessage{
		Message: store.MessageRow{
			ID: uuid.New(), ConversationID: f.conv, AuthorID: f.ada, Seq: seq, LogSeq: seq * 10,
			At: time.Now(), Text: &text, ClientID: &cid,
		},
		ReadBy:  1,
		Members: []store.Member{{UserID: f.ada, ReadSeq: 0}, {UserID: f.theo, ReadSeq: 5}},
	}
	if seq == store.FirstMessageSeq {
		fm.Conversations = map[uuid.UUID]store.ConversationRow{
			f.ada:  {ID: f.conv, Kind: "direct", LastSeq: seq, MemberCount: 2, OtherMemberName: strptr("Theo")},
			f.theo: {ID: f.conv, Kind: "direct", LastSeq: seq, MemberCount: 2, OtherMemberName: strptr("Ada")},
		}
		fm.Users = []store.UserRow{{ID: f.ada, DisplayName: "Ada"}, {ID: f.theo, DisplayName: "Theo"}}
	}
	return fm
}

func strptr(s string) *string { return &s }

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

// readNotify simulates the NOTIFY a real MarkRead would have raised, on
// CANT-92's receipt shape.
func (f *fixture) readNotify(reader uuid.UUID, before, after int64) {
	u := reader
	f.hub.OnNotify(context.Background(), store.NotifyPayload{
		ConversationID: f.conv, UserID: &u, Before: before, After: after,
	})
}

// readNotifyMessage is one row MessagesForReadNotify might return: one of
// Ada's own messages, at the given read_by.
func (f *fixture) readNotifyMessage(seq, readBy int64) store.ReadNotifyMessage {
	text := "hello"
	cid := uuid.New()
	return store.ReadNotifyMessage{
		Message: store.MessageRow{
			ID: uuid.New(), ConversationID: f.conv, AuthorID: f.ada, Seq: seq, LogSeq: seq * 10,
			At: time.Now(), Text: &text, ClientID: &cid,
		},
		ReadBy: readBy,
	}
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
	// Seq 2 and 3: this is about the index, and a conversation's first message
	// would put an introduction in front of the frame being counted (CANT-114,
	// which has its own tests).
	f.notify(2)
	theo.next(t)
	f.peers[0].detach()
	if n := f.hub.Attached(); n != 0 {
		t.Fatalf("attached = %d after detach, want 0", n)
	}
	f.notify(3)
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

	// FROM SEQ 2, for the same reason TestDetachRemovesTheSession does: this
	// test is about volume and ordering under an overflowing outbox, and a
	// conversation's first message would prepend an introduction to the burst.
	const frames = outboxBound + 20
	const firstBurstSeq = store.FirstMessageSeq + 1
	var slowest time.Duration
	for i := firstBurstSeq; i < firstBurstSeq+frames; i++ {
		start := time.Now()
		f.notify(i)
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
		for i := firstBurstSeq; i < firstBurstSeq+frames; i++ {
			if m := asMessage(t, r.next(t)); int64(m.Seq) != i {
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

// --- CANT-92: the receipt re-emission ---------------------------------------

// A receipt notification re-emits each returned row ONLY to its own author's
// attached sessions — every one of them, and nobody else's — carrying the
// state and read_by MessagesForReadNotify says it now has. Theo is the
// reader; Mallory is attached to the same conversation and gets nothing.
func TestReadNotifyReEmitsOnlyToTheAuthorsOwnSessions(t *testing.T) {
	f := newFixture(t)
	ada1, ada2 := f.attach(f.ada), f.attach(f.ada)
	theo := f.attach(f.theo)
	mal := f.attach(f.mal)

	var gotConv, gotReader uuid.UUID
	var gotBefore, gotAfter int64
	var gotCap int
	f.st.readNotify = func(_ context.Context, conv, reader uuid.UUID, before, after int64, capPerAuthor int) ([]store.ReadNotifyMessage, error) {
		gotConv, gotReader, gotBefore, gotAfter, gotCap = conv, reader, before, after, capPerAuthor
		// read_by 2: Ada (by identity) plus Theo, whose mark just passed it —
		// crosses CANT-90's threshold, so DeliveryState must answer `read`.
		return []store.ReadNotifyMessage{f.readNotifyMessage(4, 2), f.readNotifyMessage(5, 2)}, nil
	}

	f.readNotify(f.theo, 3, 5)

	if gotConv != f.conv || gotReader != f.theo || gotBefore != 3 || gotAfter != 5 || gotCap != readNotifyCap {
		t.Errorf("MessagesForReadNotify called with (%s, %s, %d, %d, %d), want (%s, %s, 3, 5, %d)",
			gotConv, gotReader, gotBefore, gotAfter, gotCap, f.conv, f.theo, readNotifyCap)
	}

	for _, p := range []*peer{ada1, ada2} {
		for _, wantSeq := range []wire.Seq{4, 5} {
			m := asMessage(t, p.next(t))
			if m.Seq != wantSeq || m.State != wire.DeliveryStateRead || m.ReadBy == nil || *m.ReadBy != 2 {
				t.Errorf("ada's frame = %+v, want seq %d, state read, read_by 2", m, wantSeq)
			}
			// CLIENT_ID IS ECHOED TO ITS AUTHOR ONLY, and Ada is the author of
			// her own re-emission — message.go's rule, exercised here rather
			// than assumed.
			if m.ClientID == nil {
				t.Error("ada's own re-emission carries no client_id")
			}
		}
	}
	// Neither the reader nor a third attached member is the author of
	// anything in the span: nothing arrives for either.
	theo.nothing(t)
	mal.nothing(t)
}

// A load failure is logged loud and is NOT a gap: OnNotify's own reasoning
// for treating a permanent load failure as resync_required does not apply
// here, because a resync cannot re-serve a message already on an earlier
// /sync page (CANT-89) — see onReadNotify's own comment. So nobody is told to
// resync and nobody is closed; the failure costs exactly what the cap's own
// remainder costs, a stale read_by until the next bootstrap.
func TestAReadNotifyLoadFailureIsLoggedAndIsNotAGap(t *testing.T) {
	f := newFixture(t)
	ada := f.attach(f.ada)
	theo := f.attach(f.theo)
	f.st.readNotify = func(context.Context, uuid.UUID, uuid.UUID, int64, int64, int) ([]store.ReadNotifyMessage, error) {
		return nil, errors.New("boom")
	}

	f.readNotify(f.theo, 0, 5)
	ada.nothing(t)
	theo.nothing(t)
	if len(ada.closedWith()) != 0 || ada.closedNow() != 0 || len(theo.closedWith()) != 0 || theo.closedNow() != 0 {
		t.Error("a read notify load failure closed a session; it is not a gap")
	}
	errs := f.log.atLeast(slog.LevelError)
	if len(errs) != 1 || errs[0].Message != "read notify fan-out failed" {
		t.Errorf("log lines at ERROR = %+v, want exactly the one failure", errs)
	}
}

// THE CAP IS THE STORE'S DECISION IN CONTENT AND THE HUB'S IN NUMBER: the hub
// passes readNotifyCap as capPerAuthor and forwards however many rows come
// back without truncating further. A span at exactly the cap for one author
// enqueues every one of them and does not sever that author's session — 64 is
// a quarter of outboxBound (256), so there is headroom to spare. Which rows
// are the newest is MessagesForReadNotify's own claim, proved against real
// Postgres in internal/store; this proves the hub does not lose or drop any
// of what the store already narrowed down to.
func TestReadNotifyForwardsExactlyTheCapWithoutSeveringTheAuthor(t *testing.T) {
	f := newFixture(t)
	ada := f.attach(f.ada)

	rows := make([]store.ReadNotifyMessage, readNotifyCap)
	for i := range rows {
		// Newest first, as the store's own ordering promises: seq counts down
		// from the cap so the highest seq is rows[0].
		rows[i] = f.readNotifyMessage(int64(readNotifyCap-i), 2)
	}
	f.st.readNotify = func(context.Context, uuid.UUID, uuid.UUID, int64, int64, int) ([]store.ReadNotifyMessage, error) {
		return rows, nil
	}

	f.readNotify(f.theo, 0, int64(readNotifyCap))

	seen := map[wire.Seq]bool{}
	for i := 0; i < readNotifyCap; i++ {
		m := asMessage(t, ada.next(t))
		seen[m.Seq] = true
	}
	if len(seen) != readNotifyCap {
		t.Errorf("ada received %d distinct messages, want exactly the cap %d", len(seen), readNotifyCap)
	}
	for seq := int64(1); seq <= int64(readNotifyCap); seq++ {
		if !seen[wire.Seq(seq)] {
			t.Errorf("seq %d never arrived", seq)
		}
	}
	ada.nothing(t)
	if n := f.hub.Attached(); n != 1 {
		t.Errorf("attached = %d after a receipt at exactly the cap, want 1 — the author must not be severed", n)
	}
	if len(ada.closedWith()) != 0 || ada.closedNow() != 0 {
		t.Error("the author's session was closed by a receipt exactly at the cap")
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

// --- CANT-114: the introduction ------------------------------------------------

func asConversation(t *testing.T, fr wire.ServerFrame) wire.Conversation {
	t.Helper()
	c, ok := fr.(wire.ServerConversationFrame)
	if !ok {
		t.Fatalf("frame = %T %+v, want a conversation record", fr, fr)
	}
	return c.Conversation
}

func asUser(t *testing.T, fr wire.ServerFrame) wire.User {
	t.Helper()
	u, ok := fr.(wire.ServerUserFrame)
	if !ok {
		t.Fatalf("frame = %T %+v, want a user record", fr, fr)
	}
	return u.User
}

// CRITERION 11 (the plan's numbering; 12 in the ticket's `Done when`), in
// memory — cmd/catenary proves the same thing over real
// sockets. On a conversation's first message every attached member session
// reads the conversation record, then the user records, then the message, in
// that order on that one session; a non-member's session reads none of them.
//
// The conversation record is PER VIEWER: this is a direct, so each member is
// introduced to it named for the OTHER one.
func TestAFirstMessageIntroducesTheConversationBeforeIt(t *testing.T) {
	f := newFixture(t)
	ada, theo, mal := f.attach(f.ada), f.attach(f.theo), f.attach(f.mal)

	f.notify(store.FirstMessageSeq)

	for _, tc := range []struct {
		who      string
		p        *peer
		wantName string
	}{
		{"ada", ada, "Theo"},
		{"theo", theo, "Ada"},
	} {
		c := asConversation(t, tc.p.next(t))
		if string(c.ID) != f.conv.String() || c.Name != tc.wantName || c.MemberCount != 2 {
			t.Errorf("%s: conversation = %+v, want %s named %s with 2 members", tc.who, c, f.conv, tc.wantName)
		}
		names := map[string]string{}
		for i := 0; i < 2; i++ {
			u := asUser(t, tc.p.next(t))
			names[string(u.ID)] = u.Name
		}
		if len(names) != 2 || names[f.ada.String()] != "Ada" || names[f.theo.String()] != "Theo" {
			t.Errorf("%s: user records = %v, want both members", tc.who, names)
		}
		if m := asMessage(t, tc.p.next(t)); int64(m.Seq) != store.FirstMessageSeq {
			t.Errorf("%s: the frame after the records = seq %d, want the message it introduced", tc.who, m.Seq)
		}
	}
	mal.nothing(t)
}

// CRITERION 13 (the plan's numbering; 14 in the ticket's `Done when`): the
// gate is the ordinal. A second message into the same
// conversation puts nothing in front of itself.
//
// THE FAKE OFFERS THE RECORDS ON EVERY MESSAGE, and that is what makes this a
// test of the HUB's gate. The store has a gate of its own — MessageForFanout
// reads nothing on a later message — and a fake that copied it would leave
// this passing against a hub that introduced every message it was handed
// records for, which is exactly what it did until this fake stopped copying
// it.
func TestOnlyAConversationsFirstMessageIntroduces(t *testing.T) {
	f := newFixture(t)
	theo := f.attach(f.theo)
	f.st.fanout = func(_ context.Context, _ uuid.UUID, seq int64) (store.FanoutMessage, error) {
		fm, records := f.message(seq), f.message(store.FirstMessageSeq)
		fm.Conversations, fm.Users = records.Conversations, records.Users
		return fm, nil
	}

	f.notify(store.FirstMessageSeq)
	asConversation(t, theo.next(t))
	asUser(t, theo.next(t))
	asUser(t, theo.next(t))
	asMessage(t, theo.next(t))

	f.notify(store.FirstMessageSeq + 1)
	if m := asMessage(t, theo.next(t)); int64(m.Seq) != store.FirstMessageSeq+1 {
		t.Errorf("the next frame = seq %d, want the second message with no record in front of it", m.Seq)
	}
	theo.nothing(t)
}

// THE GATE IS A MESSAGE NOTIFICATION, NEVER A CANT-92 RECEIPT RE-EMISSION.
//
// A re-emission reaches only the author, who holds the conversation by
// definition, and its burst is bounded by readNotifyCap — 1 + N introduction
// frames per re-emitted message is exactly what must not ride along with it.
// The two shapes part ways at the top of OnNotify, so this is the test that
// says the introduction sits on the far side of that branch.
func TestAReceiptReEmissionCarriesNoIntroduction(t *testing.T) {
	f := newFixture(t)
	ada := f.attach(f.ada)
	f.st.readNotify = func(context.Context, uuid.UUID, uuid.UUID, int64, int64, int) ([]store.ReadNotifyMessage, error) {
		return []store.ReadNotifyMessage{f.readNotifyMessage(store.FirstMessageSeq, 2)}, nil
	}

	f.readNotify(f.theo, 0, store.FirstMessageSeq)

	m := asMessage(t, ada.next(t))
	if int64(m.Seq) != store.FirstMessageSeq || m.State != wire.DeliveryStateRead {
		t.Errorf("re-emission = %+v, want the seq 1 message refreshed as read", m)
	}
	ada.nothing(t)
}

// CRITERION 15 (the plan's numbering; 16 in the ticket's `Done when`): a
// failed load on a conversation's first message still ends in
// a gap for every attached session, and delivers nothing — not the message,
// and not a record either. There is no per-session failure path to test,
// because there is no per-session state.
func TestAFailedLoadOnAFirstMessageIsStillAGapAndDeliversNothing(t *testing.T) {
	f := newFixture(t)
	theo, ada := f.attach(f.theo), f.attach(f.ada)
	f.st.fanout = func(context.Context, uuid.UUID, int64) (store.FanoutMessage, error) {
		return store.FanoutMessage{}, errors.New("column does not exist")
	}

	f.notify(store.FirstMessageSeq)

	for _, p := range []*peer{theo, ada} {
		r, ok := p.next(t).(wire.ServerResyncRequired)
		if !ok || r.Reason != wire.ResyncReasonCursorTooOld || r.LogSeq != 900 {
			t.Errorf("frame = %+v, want resync_required{cursor_too_old, 900}", r)
		}
		p.nothing(t)
		if len(p.closedWith()) != 0 || p.closedNow() != 0 {
			t.Error("a failed introduction closed a session")
		}
	}
}

// THE MESSAGE IS NEVER HELD BACK BY ITS RECORD. A member the store somehow
// returned no conversation row for still receives the message — withholding it
// would be this package breaking its own header — and the omission is logged
// rather than swallowed. Unreachable through the real store, which reads both
// from one snapshot over one member list; pinned because the branch is written
// rather than assumed.
func TestAMemberWithNoConversationRecordStillGetsTheMessage(t *testing.T) {
	f := newFixture(t)
	theo := f.attach(f.theo)
	f.st.fanout = func(_ context.Context, _ uuid.UUID, seq int64) (store.FanoutMessage, error) {
		fm := f.message(seq)
		delete(fm.Conversations, f.theo)
		return fm, nil
	}

	f.notify(store.FirstMessageSeq)

	if m := asMessage(t, theo.next(t)); int64(m.Seq) != store.FirstMessageSeq {
		t.Errorf("frame = seq %d, want the message delivered without its record", m.Seq)
	}
	warns := f.log.atLeast(slog.LevelWarn)
	if len(warns) != 1 || warns[0].Message != "no conversation record for a member on a first message; delivering the message alone" {
		t.Errorf("warn lines = %+v, want the one missing-record line", warns)
	}
}
