package main

// CANT-107's `Done when`, end to end through the REAL router that setup()
// builds, the real store on Postgres, the real listener on its own
// connection, and real sockets — not the in-memory peer internal/hub tests
// against. The claim here is that a message committed by one member reaches
// every other attached member's session, and nobody else's, with the state
// that member is entitled to see; that a planted listener gap tells every
// session to resync and closes none; that `read` and `typing` cross sockets;
// and that a cancelled serve context — what SIGTERM produces — closes every
// session with 1001.
//
// MARKERS, NOT SLEEPS. "Nothing arrived" is proved by a frame that travels
// the same path as the one that must not: a message committed through the
// store AFTER the action under test, whose OnNotify runs strictly after any
// enqueue the action made, on the one listener goroutine, so it must be the
// tested session's next hub-originated frame. A `pong` proves only that the
// session's read goroutine finished `Handle`, and is used only for that.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// recorder keeps every log record so a test can assert on levels and attrs.
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

func (r *recorder) find(msg string) []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []slog.Record
	for _, rec := range r.recs {
		if rec.Message == msg {
			out = append(out, rec)
		}
	}
	return out
}

func (r *recorder) atLeast(level slog.Level) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.recs {
		if rec.Level >= level {
			out = append(out, rec.Message)
		}
	}
	return out
}

func attrOf(rec slog.Record, key string) any {
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

// --- fixture -----------------------------------------------------------------

type rig struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	st   *store.Store
	d    deps
	log  *recorder
	base string // http://host:port
}

// newRig builds the process over a fresh database, serves it, and runs the
// listener until the test ends.
func newRig(t *testing.T) *rig {
	t.Helper()
	log := &recorder{}
	ctx, pool, st, d := processFixture(t, slog.New(log))
	srv := httptest.NewServer(d.router)
	t.Cleanup(srv.Close)
	r := &rig{t: t, ctx: ctx, pool: pool, st: st, d: d, log: log, base: srv.URL}
	r.runListener()
	return r
}

func (r *rig) runListener() {
	r.t.Helper()
	lctx, stop := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.d.listener.Run(lctx)
	}()
	r.t.Cleanup(func() { stop(); <-done })
	r.waitForListen()
}

// waitForListen returns once a LISTEN is registered and waiting.
func (r *rig) waitForListen() {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := r.pool.QueryRow(r.ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event = 'ClientRead'
			   AND query ILIKE 'LISTEN %'`).Scan(&n); err != nil {
			r.t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatal("the listener did not register within the deadline")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// enroll mints a device for a user through POST /enroll.
func (r *rig) enroll(user uuid.UUID, name string) wire.EnrollResponse {
	r.t.Helper()
	issued, err := r.st.IssueEnrollmentToken(r.ctx, user)
	if err != nil {
		r.t.Fatal(err)
	}
	code, body := do(r.t, r.d.router, enrollRequest(r.t, issued.Plaintext, name))
	if code != http.StatusOK {
		r.t.Fatalf("enroll = %d: %s", code, body)
	}
	var enrolled wire.EnrollResponse
	if err := json.Unmarshal(body, &enrolled); err != nil {
		r.t.Fatal(err)
	}
	return enrolled
}

// session is one open, hello'd socket.
type session struct {
	t    *testing.T
	ctx  context.Context
	conn *websocket.Conn
	user uuid.UUID
}

// open enrolls a device, upgrades, says hello and reads `ready`.
func (r *rig) open(user uuid.UUID, device string) *session {
	r.t.Helper()
	return openAt(r.t, r.ctx, r.base, r.enroll(user, device), user)
}

func openAt(t *testing.T, ctx context.Context, base string, enrolled wire.EnrollResponse, user uuid.UUID) *session {
	t.Helper()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(base, "http")+"/ws",
		&websocket.DialOptions{Subprotocols: []string{"catenary.v1", "catenary.token." + string(enrolled.AccessToken)}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	s := &session{t: t, ctx: ctx, conn: conn, user: user}
	s.send(wire.ClientHello{WireVersion: wire.WireVersion, DeviceID: enrolled.DeviceID})
	if _, ok := s.next().(wire.ServerReady); !ok {
		t.Fatal("no ready")
	}
	return s
}

func (s *session) send(f wire.ClientFrame) {
	s.t.Helper()
	wsSend(s.ctx, s.t, s.conn, f)
}

// next reads one frame, with a bound.
func (s *session) next() wire.ServerFrame {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	return wsRead(ctx, s.t, s.conn)
}

// message reads the next frame and requires it to be a `message`.
func (s *session) message() wire.Message {
	s.t.Helper()
	f := s.next()
	m, ok := f.(wire.ServerMessageFrame)
	if !ok {
		s.t.Fatalf("next frame = %T %+v, want message", f, f)
	}
	return m.Message
}

// pingPong proves the session's read goroutine has finished everything
// before the ping — and nothing more. The next frame MUST be the pong: a
// caller that expects hub frames reads them first.
func (s *session) pingPong() {
	s.t.Helper()
	id := uuid.NewString()
	s.send(wire.Ping{ID: id})
	f := s.next()
	if p, ok := f.(wire.Pong); !ok || p.ID != id {
		s.t.Fatalf("next frame after ping = %T %+v, want its pong", f, f)
	}
}

// close performs the client's close handshake and lets the server's detach
// run.
func (s *session) close() {
	s.t.Helper()
	_ = s.conn.Close(websocket.StatusNormalClosure, "bye")
}

// expectClose reads until the server closes the socket and returns the
// status.
func (s *session) expectClose() websocket.StatusCode {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	_, data, err := s.conn.Read(ctx)
	if err == nil {
		s.t.Fatalf("expected a close, got a frame: %s", data)
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		s.t.Fatalf("expected a close, got: %v", err)
	}
	return ce.Code
}

func (r *rig) commit(conv, author uuid.UUID, text string) store.Sent {
	r.t.Helper()
	return send(r.ctx, r.t, r.st, conv, author, text)
}

func (r *rig) head() int64 {
	r.t.Helper()
	h, err := r.st.Head(r.ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

// --- criteria 0 and 1 ----------------------------------------------------------

// A send over the socket reaches every other attached member with the state
// that member sees, the author's other device with client_id echoed, and a
// non-member not at all. A message committed through the store — REST's
// call — fans out identically.
func TestAMessageReachesEveryAttachedMemberAndNobodyElse(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	mallory := mkUser(r.ctx, t, r.pool, "mallory", "Mallory")
	groupA := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	groupB := mkGroup(r.ctx, t, r.pool, "B", theo, mallory)

	ada1, ada2 := r.open(ada, "laptop"), r.open(ada, "phone")
	theoS := r.open(theo, "phone")
	mal := r.open(mallory, "laptop")

	clientID := uuid.New()
	text := "over the socket"
	ada1.send(wire.ClientSend{ClientID: wire.Uuid(clientID.String()), ConversationID: wire.Uuid(groupA.String()), Text: &text})

	// The author's own session gets the ack (from the read loop) and the
	// message (from the outbox), in either order.
	var ack *wire.ServerAck
	var own *wire.Message
	for ack == nil || own == nil {
		switch f := ada1.next().(type) {
		case wire.ServerAck:
			a := f
			ack = &a
		case wire.ServerMessageFrame:
			m := f.Message
			own = &m
		default:
			t.Fatalf("unexpected frame on the author's session: %T", f)
		}
	}
	if own.ID != ack.MessageID || own.State != wire.DeliveryStateSent || own.ClientID == nil || string(*own.ClientID) != clientID.String() {
		t.Errorf("author's own frame = %+v, want the acked message, state sent, client_id echoed", own)
	}

	m := ada2.message()
	if m.ID != ack.MessageID || m.State != wire.DeliveryStateSent || m.ClientID == nil {
		t.Errorf("author's other device = state %s client_id %v, want sent and echoed", m.State, m.ClientID)
	}
	m = theoS.message()
	if m.ID != ack.MessageID || m.State != wire.DeliveryStateDelivered || m.ClientID != nil || m.ReadBy == nil || *m.ReadBy != 1 {
		t.Errorf("theo's frame = state %s client_id %v read_by %v, want delivered / none / 1", m.State, m.ClientID, m.ReadBy)
	}

	// Criterion 1: the store's own send — REST's call — fans out the same.
	direct := r.commit(groupA, theo, "committed through the store")
	m = theoS.message()
	if string(m.ID) != direct.ID.String() || m.State != wire.DeliveryStateSent {
		t.Errorf("theo's own store-committed message = %+v, want %s as sent", m, direct.ID)
	}
	for _, s := range []*session{ada1, ada2} {
		m = s.message()
		if string(m.ID) != direct.ID.String() || m.State != wire.DeliveryStateDelivered || m.ClientID != nil {
			t.Errorf("ada's frame for the store-committed message = %+v, want delivered and no client_id", m)
		}
	}

	// THE MARKER. Mallory is in B with Theo. A message committed to B now
	// is delivered by an OnNotify that runs strictly after A's two, so it
	// must be the first hub frame Mallory ever reads.
	marker := r.commit(groupB, theo, "marker in B")
	if m := mal.message(); string(m.ID) != marker.ID.String() || string(m.ConversationID) != groupB.String() {
		t.Errorf("mallory's first frame = %+v, want the marker in B — something from A reached a non-member", m)
	}
	theoS.message()
}

// --- criterion 2 -------------------------------------------------------------------

// First deliveries arrive in ascending log_seq under concurrent inserters,
// each exactly once.
func TestFirstDeliveriesArriveInCommitOrder(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	theoS := r.open(theo, "phone")

	const writers, each = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := r.st.SendMessage(r.ctx, store.NewMessage{
					ClientID: uuid.New(), ConversationID: group, AuthorID: ada, Text: ptrStr("m"),
				}); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	seen := map[wire.Uuid]bool{}
	var last int64
	for i := 0; i < writers*each; i++ {
		m := theoS.message()
		if int64(m.LogSeq) <= last {
			t.Fatalf("frame %d: log_seq %d after %d — not ascending", i, m.LogSeq, last)
		}
		last = int64(m.LogSeq)
		if seen[m.ID] {
			t.Fatalf("message %s delivered twice", m.ID)
		}
		seen[m.ID] = true
	}
	if len(seen) != writers*each {
		t.Errorf("%d distinct messages, want %d", len(seen), writers*each)
	}
}

func ptrStr(s string) *string { return &s }

// --- criterion 4 -------------------------------------------------------------------

// A planted listener gap: every attached session gets exactly one
// resync_required at head, none is closed, one WARN names the count and the
// head, and delivery resumes.
func TestAListenerGapTellsEverySessionToResyncAndClosesNone(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	sessions := []*session{r.open(ada, "laptop"), r.open(ada, "phone"), r.open(theo, "phone")}
	r.commit(group, ada, "before the gap")
	for _, s := range sessions {
		s.message()
	}
	head := r.head()

	if _, err := r.pool.Exec(r.ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = current_database() AND query ILIKE 'LISTEN %'`); err != nil {
		t.Fatalf("terminate the listener's backend: %v", err)
	}

	for _, s := range sessions {
		f := s.next()
		rs, ok := f.(wire.ServerResyncRequired)
		if !ok || rs.Reason != wire.ResyncReasonCursorTooOld || int64(rs.LogSeq) != head {
			t.Errorf("frame after the gap = %T %+v, want resync_required{cursor_too_old, %d}", f, f, head)
		}
		// Exactly one, and the session is open: the next frame is the pong.
		s.pingPong()
	}

	gaps := r.log.find("listener gap")
	if len(gaps) != 1 || attrOf(gaps[0], "sessions_notified") != int64(3) || attrOf(gaps[0], "head") != head {
		t.Errorf("listener gap lines = %d %+v, want one with sessions_notified=3 head=%d", len(gaps), gaps, head)
	}
	if gaps[0].Level != slog.LevelWarn {
		t.Errorf("gap logged at %s, want WARN", gaps[0].Level)
	}

	// The resync frames prove the re-LISTEN happened (OnGap is raised after
	// it), so a message committed now is delivered live.
	after := r.commit(group, theo, "after the reconnect")
	for _, s := range sessions {
		if m := s.message(); string(m.ID) != after.ID.String() {
			t.Errorf("post-gap frame = %s, want %s", m.ID, after.ID)
		}
	}
}

// --- criterion 6 -------------------------------------------------------------------

func TestReadFansOutAReceiptOnlyWhenTheMarkMoves(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	mallory := mkUser(r.ctx, t, r.pool, "mallory", "Mallory")
	groupA := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	groupB := mkGroup(r.ctx, t, r.pool, "B", theo, mallory)
	adaS, theoS, mal := r.open(ada, "laptop"), r.open(theo, "phone"), r.open(mallory, "laptop")
	for i := 0; i < 3; i++ {
		r.commit(groupA, ada, "m")
		adaS.message()
		theoS.message()
	}
	convA := wire.Uuid(groupA.String())

	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: 1})
	for _, s := range []*session{adaS, theoS} {
		f := s.next()
		rc, ok := f.(wire.ServerReceipt)
		if !ok || string(rc.UserID) != theo.String() || rc.UpToSeq != 1 || rc.ConversationID != convA {
			t.Errorf("frame = %T %+v, want receipt{A, theo, 1}", f, f)
		}
	}

	// The same read again moves nothing and emits nothing. The pong proves
	// Handle returned; the committed message is the next hub frame.
	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: 1})
	theoS.pingPong()
	marker := r.commit(groupA, ada, "marker")
	for _, s := range []*session{adaS, theoS} {
		if m := s.message(); string(m.ID) != marker.ID.String() {
			t.Errorf("next hub frame = %+v, want the marker — a receipt was produced for a mark that did not move", m)
		}
	}

	// Above head: the receipt carries the clamped mark.
	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: 999})
	var lastSeq int64
	if err := r.pool.QueryRow(r.ctx, `SELECT last_seq FROM conversations WHERE id = $1`, groupA).Scan(&lastSeq); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*session{adaS, theoS} {
		rc, ok := s.next().(wire.ServerReceipt)
		if !ok || int64(rc.UpToSeq) != lastSeq {
			t.Errorf("receipt for a claim above head = %+v, want up_to_seq %d", rc, lastSeq)
		}
	}

	// A non-member's read: no receipt anywhere and, under ruling 1 option 0,
	// nothing on the sender's own session. Mallory's marker is a message in
	// B; A's members get one in A.
	mal.send(wire.ClientRead{ConversationID: convA, UpToSeq: 1})
	mal.pingPong()
	inB := r.commit(groupB, theo, "marker in B")
	if m := mal.message(); string(m.ID) != inB.ID.String() {
		t.Errorf("mallory's next frame = %+v, want the marker in B", m)
	}
	theoS.message()
	inA := r.commit(groupA, ada, "marker in A")
	for _, s := range []*session{adaS, theoS} {
		if m := s.message(); string(m.ID) != inA.ID.String() {
			t.Errorf("next hub frame = %+v, want the marker in A — a non-member's read produced a receipt", m)
		}
	}
}

// --- criterion 7 -------------------------------------------------------------------

func TestTypingCrossesSocketsInStartOrderAndClearsOnDetach(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	mallory := mkUser(r.ctx, t, r.pool, "mallory", "Mallory")
	groupA := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	convA := wire.Uuid(groupA.String())
	adaID, theoID := wire.Uuid(ada.String()), wire.Uuid(theo.String())
	start := wire.ClientTyping{ConversationID: convA, State: wire.TypingStateStart}
	stop := wire.ClientTyping{ConversationID: convA, State: wire.TypingStateStop}
	typing := func(s *session) []wire.Uuid {
		t.Helper()
		f := s.next()
		ty, ok := f.(wire.ServerTyping)
		if !ok || ty.ConversationID != convA {
			t.Fatalf("frame = %T %+v, want typing in A", f, f)
		}
		return ty.UserIds
	}
	same := func(got []wire.Uuid, want ...wire.Uuid) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	laptop, theoS := r.open(ada, "laptop"), r.open(theo, "phone")
	mal := r.open(mallory, "laptop")

	laptop.send(start)
	if got := typing(theoS); !same(got, adaID) {
		t.Errorf("theo sees %v, want [ada]", got)
	}
	// The originating session hears nothing: pong, then a committed message
	// is its next hub frame.
	laptop.pingPong()
	marker := r.commit(groupA, theo, "marker")
	if m := laptop.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("laptop's next frame = %+v, want the marker — the originating session received its own typing", m)
	}
	theoS.message()

	theoS.send(start)
	if got := typing(laptop); !same(got, adaID, theoID) {
		t.Errorf("laptop sees %v, want [ada, theo] in start order", got)
	}
	laptop.send(stop)
	if got := typing(theoS); !same(got, theoID) {
		t.Errorf("theo sees %v after ada's stop, want [theo]", got)
	}
	theoS.close()
	if got := typing(laptop); !same(got) {
		t.Errorf("laptop sees %v after theo's socket closed, want []", got)
	}

	// Two devices: Ada types on the laptop with the phone attached; the
	// laptop closes; the phone holds no entry, so Theo sees [].
	theoS = r.open(theo, "phone")
	phone := r.open(ada, "phone")
	laptop.send(start)
	if got := typing(theoS); !same(got, adaID) {
		t.Errorf("theo sees %v, want [ada]", got)
	}
	typing(phone)
	laptop.close()
	if got := typing(theoS); !same(got) {
		t.Errorf("theo sees %v after the typing device closed, want []", got)
	}
	typing(phone)

	// A non-member's typing reaches nobody and logs one WARN.
	mal.send(start)
	mal.pingPong()
	inA := r.commit(groupA, ada, "marker")
	for _, s := range []*session{theoS, phone} {
		if m := s.message(); string(m.ID) != inA.ID.String() {
			t.Errorf("next frame = %+v, want the marker — a non-member's typing was fanned out", m)
		}
	}
	if warns := r.log.find("typing from a non-member"); len(warns) != 1 {
		t.Errorf("non-member typing warn lines = %d, want 1", len(warns))
	}
}

// --- criterion 9 -------------------------------------------------------------------

// Cancelling serve's context — what signal.NotifyContext does on SIGTERM —
// closes every session with 1001 while a REST request is still in flight,
// then serve returns nil once it is released, with nothing logged at WARN or
// above.
func TestACancelledServeClosesEverySessionWith1001(t *testing.T) {
	log := &recorder{}
	ctx, pool, _, d := processFixture(t, slog.New(log))
	ada := mkUser(ctx, t, pool, "ada", "Ada")
	theo := mkUser(ctx, t, pool, "theo", "Theo")
	mkGroup(ctx, t, pool, "A", ada, theo)

	// A REST request held open across the cancel, by a wrapper around the
	// router: /hold blocks until released.
	entered, release := make(chan struct{}), make(chan struct{})
	router := d.router
	d.router = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/hold" {
			close(entered)
			<-release
			w.WriteHeader(http.StatusOK)
			return
		}
		router.ServeHTTP(w, req)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- serve(serveCtx, d, ln) }()
	base := "http://" + ln.Addr().String()

	r := &rig{t: t, ctx: ctx, pool: pool, st: d.store, d: d, log: log, base: base}
	r.waitForListen()
	sessions := []*session{r.open(ada, "laptop"), r.open(theo, "phone")}

	holdDone := make(chan struct{})
	go func() {
		defer close(holdDone)
		resp, err := http.Get(base + "/hold")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered

	start := time.Now()
	cancel()
	for _, s := range sessions {
		if code := s.expectClose(); code != websocket.StatusGoingAway {
			t.Errorf("session closed with %d, want 1001", code)
		}
	}
	select {
	case <-holdDone:
		t.Fatal("the held REST request finished before it was released; the drain was not proved concurrent")
	default:
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the sessions took %v to close while REST was held; the drain waited on it", d)
	}
	close(release)
	<-holdDone

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve returned %v, want nil", err)
		}
	case <-time.After(d.cfg.ShutdownGrace + 2*time.Second):
		t.Fatal("serve did not return within the grace")
	}
	if lines := log.atLeast(slog.LevelWarn); len(lines) != 0 {
		t.Errorf("lines at WARN or above on an ordinary shutdown: %v", lines)
	}
}
