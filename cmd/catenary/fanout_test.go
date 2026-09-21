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
	// since is the database's clock when the rig was built. waitForListen
	// counts only LISTEN backends started after it: the previous test's
	// listener is stopped in its cleanup, but Postgres reaps the backend a
	// moment later, and a wait that matched it would let this test commit
	// before its own listener was registered — a lost notification and a
	// ten-second timeout, once in a while.
	since time.Time
}

// newRig builds the process over a fresh database, serves it, and runs the
// listener until the test ends.
func newRig(t *testing.T) *rig {
	t.Helper()
	log := &recorder{}
	ctx, pool, st, d := processFixture(t, slog.New(log))
	srv := httptest.NewServer(d.router)
	t.Cleanup(srv.Close)
	r := &rig{t: t, ctx: ctx, pool: pool, st: st, d: d, log: log, base: srv.URL, since: dbNow(ctx, t, pool)}
	r.runListener()
	return r
}

func dbNow(ctx context.Context, t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
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
			   AND query ILIKE 'LISTEN %'
			   AND backend_start >= $1`, r.since).Scan(&n); err != nil {
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

// message reads the next frame and requires it to be a `message`, TOLERATING
// the introduction records that precede a conversation's first one (CANT-114).
//
// IT HOLDS EVERY RECORD IT SKIPS TO THE MESSAGE'S OWN CONVERSATION, so the
// marker technique above keeps its teeth: a `conversation` record for a room
// this reader should never have heard of fails here, exactly as the message
// that would have followed it does, and a `user` record with no conversation
// record in front of it fails too. A test that is ABOUT the introduction
// reads the frames raw, through conversationFrame and userFrame.
func (s *session) message() wire.Message {
	s.t.Helper()
	var records []wire.Uuid
	for {
		f := s.next()
		switch v := f.(type) {
		case wire.ServerMessageFrame:
			for _, id := range records {
				if id != v.Message.ConversationID {
					s.t.Fatalf("a conversation record for %s arrived ahead of a message in %s", id, v.Message.ConversationID)
				}
			}
			return v.Message
		case wire.ServerConversationFrame:
			records = append(records, v.Conversation.ID)
		case wire.ServerUserFrame:
			// A user record carries no conversation of its own, so its bound
			// is the conversation record it MUST follow — CHECKED, not
			// assumed. Skipping it unconditionally would let user records
			// reach a session with no record in front of them and still pass
			// every test that reads through here, marker idiom included,
			// which is the sentence above being false for half the records it
			// claims to hold.
			if len(records) == 0 {
				s.t.Fatalf("a user record arrived with no conversation record in front of it: %+v", v)
			}
		default:
			s.t.Fatalf("next frame = %T %+v, want message", f, f)
		}
	}
}

// conversationFrame and userFrame read ONE frame each and require the
// introduction records, in the order the hub enqueued them.
func (s *session) conversationFrame() wire.Conversation {
	s.t.Helper()
	f := s.next()
	c, ok := f.(wire.ServerConversationFrame)
	if !ok {
		s.t.Fatalf("next frame = %T %+v, want a conversation record", f, f)
	}
	return c.Conversation
}

func (s *session) userFrame() wire.User {
	s.t.Helper()
	f := s.next()
	u, ok := f.(wire.ServerUserFrame)
	if !ok {
		s.t.Fatalf("next frame = %T %+v, want a user record", f, f)
	}
	return u.User
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
		case wire.ServerConversationFrame, wire.ServerUserFrame:
			// This is groupA's FIRST message, so it is introduced (CANT-114).
			// The order is asserted by that ticket's own tests; this one is
			// about the message and its ack.
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

// assertReceiptFrame checks one ServerReceipt's content; the caller has
// already picked it out of whatever else arrived alongside it.
func assertReceiptFrame(t *testing.T, f wire.ServerFrame, conv wire.Uuid, user uuid.UUID, upToSeq int64) {
	t.Helper()
	rc, ok := f.(wire.ServerReceipt)
	if !ok || string(rc.UserID) != user.String() || int64(rc.UpToSeq) != upToSeq || rc.ConversationID != conv {
		t.Errorf("frame = %T %+v, want receipt{%s, %s, %d}", f, f, conv, user, upToSeq)
	}
}

// readsReceiptAndReEmissions reads exactly 1+wantSeqs frames from s and
// requires exactly one ServerReceipt (returned) and one ServerMessageFrame,
// each read, per seq in wantSeqs (CANT-92: the author's own re-emissions,
// crossing to `read` at read_by 2 — the reader plus the author by identity).
func readsReceiptAndReEmissions(t *testing.T, s *session, wantSeqs ...int64) wire.ServerReceipt {
	t.Helper()
	want := map[int64]bool{}
	for _, seq := range wantSeqs {
		want[seq] = true
	}
	var receipt *wire.ServerReceipt
	got := map[int64]bool{}
	for i := 0; i < 1+len(wantSeqs); i++ {
		switch f := s.next().(type) {
		case wire.ServerReceipt:
			if receipt != nil {
				t.Fatalf("%s: a second receipt arrived: %+v", s.user, f)
			}
			rc := f
			receipt = &rc
		case wire.ServerMessageFrame:
			seq := int64(f.Message.Seq)
			got[seq] = true
			if f.Message.State != wire.DeliveryStateRead || f.Message.ReadBy == nil || *f.Message.ReadBy != 2 {
				t.Errorf("%s: re-emission for seq %d = %+v, want state read, read_by 2", s.user, seq, f.Message)
			}
		default:
			t.Fatalf("%s: unexpected frame %T %+v", s.user, f, f)
		}
	}
	if receipt == nil {
		t.Fatalf("%s: no receipt arrived among %d frames", s.user, 1+len(wantSeqs))
	}
	if len(got) != len(want) {
		t.Fatalf("%s: re-emitted seqs = %v, want %v", s.user, got, want)
	}
	for seq := range want {
		if !got[seq] {
			t.Errorf("%s: seq %d was never re-emitted", s.user, seq)
		}
	}
	return *receipt
}

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

	// CANT-92: Ada authored the one message this receipt covers (seq 1), so
	// her session ALSO gets it re-emitted with its new read_by (1 -> 2,
	// crossing to `read`) — alongside the receipt every attached member
	// gets. Theo authored nothing in the span, so his socket sees only the
	// receipt, exactly as before.
	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: 1})
	assertReceiptFrame(t, theoS.next(), convA, theo, 1)
	assertReceiptFrame(t, readsReceiptAndReEmissions(t, adaS, 1), convA, theo, 1)

	// The same read again moves nothing and emits nothing — not a receipt,
	// not a re-emission. The pong proves Handle returned; the committed
	// message is the next hub frame on both sockets.
	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: 1})
	theoS.pingPong()
	marker := r.commit(groupA, ada, "marker")
	for _, s := range []*session{adaS, theoS} {
		if m := s.message(); string(m.ID) != marker.ID.String() {
			t.Errorf("next hub frame = %+v, want the marker — a receipt was produced for a mark that did not move", m)
		}
	}

	// Above head: the receipt carries the clamped mark, and CANT-92 re-emits
	// every one of Ada's messages the clamp newly covers — seq 2 and 3 from
	// the setup loop, plus the "marker" just committed at seq 4, all still
	// authored by Ada.
	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: 999})
	var lastSeq int64
	if err := r.pool.QueryRow(r.ctx, `SELECT last_seq FROM conversations WHERE id = $1`, groupA).Scan(&lastSeq); err != nil {
		t.Fatal(err)
	}
	assertReceiptFrame(t, theoS.next(), convA, theo, lastSeq)
	assertReceiptFrame(t, readsReceiptAndReEmissions(t, adaS, 2, 3, 4), convA, theo, lastSeq)

	// A non-member's read: no receipt anywhere and, under ruling 1 option 0,
	// nothing on the sender's own session — and, on the same terms, no
	// CANT-92 re-emission either, since a refused MarkRead never reaches the
	// notify. Mallory's marker is a message in B; A's members get one in A.
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
	// The clock is read BEFORE serve starts its listener, or a fast connect
	// could register a backend older than the mark and never be counted.
	since := dbNow(ctx, t, pool)
	serveCtx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	// No provisioning listener: this fixture's config carries neither variable,
	// so setup built no provisioning handler and serve is handed no second
	// socket. CANT-131's own test drives the shutdown with one.
	go func() { served <- serve(serveCtx, d, ln, nil) }()
	base := "http://" + ln.Addr().String()

	r := &rig{t: t, ctx: ctx, pool: pool, st: d.store, d: d, log: log, base: base, since: since}
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

// --- CANT-114: criteria 11, 13 and 16 -------------------------------------------
//
// THE NUMBERS IN THIS FILE ARE THE PLAN'S POSITIONS. CANT-103's plan rev 3
// numbers these rows 11, 13 and 16; the ticket's own `Done when` numbers the
// same three 12, 14 and 17. Each header below states both, because the code
// outlives the pull request that carried the mapping.

// wireJSON renders a wire value the way a client receives it, so two records
// are compared as the bytes that cross the wire rather than as Go structs.
func wireJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(raw)
}

// syncConversation is the record GET /sync serves this device for one
// conversation — the record the introduction claims to be carrying.
func (r *rig) syncConversation(token string, id uuid.UUID) wire.Conversation {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	code, body := do(r.t, r.d.router, req)
	if code != http.StatusOK {
		r.t.Fatalf("GET /sync = %d: %s", code, body)
	}
	var page wire.SyncResponse
	if err := json.Unmarshal(body, &page); err != nil {
		r.t.Fatalf("sync page: %v", err)
	}
	for _, c := range page.Conversations {
		if string(c.ID) == id.String() {
			return c
		}
	}
	r.t.Fatalf("/sync carried no record for %s: %s", id, body)
	return wire.Conversation{}
}

// CRITERION 11 AND 13 (the plan's numbering; 12 and 14 in the ticket's
// `Done when`), over real sockets against Postgres through the router
// setup() builds. On a conversation's FIRST message every attached member
// session reads the `conversation` record, then that conversation's `user`
// records, then the `message` — in that order, on that one session. A
// non-member's session reads none of the three. A SECOND message into the same
// conversation introduces nothing.
func TestAFirstMessageIntroducesTheConversationAndItsUsers(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	mallory := mkUser(r.ctx, t, r.pool, "mallory", "Mallory")
	groupA := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	groupB := mkGroup(r.ctx, t, r.pool, "B", theo, mallory)

	adaS, theoS := r.open(ada, "laptop"), r.open(theo, "phone")
	mal := r.open(mallory, "laptop")

	first := r.commit(groupA, ada, "the first message in A")
	for _, s := range []*session{adaS, theoS} {
		c := s.conversationFrame()
		if string(c.ID) != groupA.String() || c.Name != "A" || c.MemberCount != 2 || int64(c.HeadSeq) != first.Seq {
			t.Errorf("%s: conversation record = %+v, want A at seq %d with 2 members", s.user, c, first.Seq)
		}
		names := map[string]string{}
		for i := 0; i < 2; i++ {
			u := s.userFrame()
			names[string(u.ID)] = u.Name
		}
		if len(names) != 2 || names[ada.String()] != "Ada" || names[theo.String()] != "Theo" {
			t.Errorf("%s: user records = %v, want Ada and Theo", s.user, names)
		}
		f := s.next()
		m, ok := f.(wire.ServerMessageFrame)
		if !ok || string(m.Message.ID) != first.ID.String() {
			t.Errorf("%s: the frame after the records = %T %+v, want the message they introduce", s.user, f, f)
		}
	}

	// CRITERION 13: the gate is the ordinal, so the next message is alone.
	second := r.commit(groupA, ada, "the second message in A")
	for _, s := range []*session{adaS, theoS} {
		f := s.next()
		m, ok := f.(wire.ServerMessageFrame)
		if !ok || string(m.Message.ID) != second.ID.String() {
			t.Errorf("%s: frame = %T %+v, want the second message with no record in front of it", s.user, f, f)
		}
	}

	// THE MARKER, and the non-member half of criterion 11. Mallory is in B with
	// Theo and never in A. B's own first message introduces B to her, and it is
	// delivered by an OnNotify that ran strictly after both of A's — so
	// anything A produced for her would already have arrived.
	marker := r.commit(groupB, theo, "the first message in B")
	if c := mal.conversationFrame(); string(c.ID) != groupB.String() {
		t.Fatalf("mallory's first frame = the record for %s, want B — a record from A reached a non-member", c.ID)
	}
	mal.userFrame()
	mal.userFrame()
	if m := mal.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("mallory's frame = %+v, want B's first message", m)
	}
	theoS.message() // Theo is in B as well; drain B's introduction and message.
}

// CRITERION 16 (the plan's numbering; 17 in the ticket's `Done when`) — the
// over-fire, asserted rather than assumed. A session that
// ALREADY holds the conversation is introduced to it again on its first
// message, and the record is the same one /sync serves, so a client that
// replaces by id changes nothing by applying it. That is the stated price of
// the hub holding no per-session state.
//
// It also proves criterion 12 end to end: one statement, and the direct is
// named for the OTHER member on each of the two sockets.
func TestASessionThatAlreadyHoldsTheConversationIsIntroducedAgain(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")

	// CANT-75's find-or-create: the conversation exists with no messages in it,
	// and its metadata marker moved, so /sync carries it before anything is
	// sent — which is what makes "already holds it" true here.
	direct, err := r.st.FindOrCreateDirect(r.ctx, ada, "theo")
	if err != nil {
		t.Fatalf("find-or-create direct: %v", err)
	}

	theoEnrolled := r.enroll(theo, "phone")
	theoS := openAt(t, r.ctx, r.base, theoEnrolled, theo)
	adaEnrolled := r.enroll(ada, "laptop")
	adaS := openAt(t, r.ctx, r.base, adaEnrolled, ada)

	held := r.syncConversation(string(theoEnrolled.AccessToken), direct.ID)
	if held.Name != "Ada" {
		t.Fatalf("/sync names theo's direct %q, want Ada", held.Name)
	}

	first := r.commit(direct.ID, ada, "hello")

	got := theoS.conversationFrame()
	want := r.syncConversation(string(theoEnrolled.AccessToken), direct.ID)
	if wireJSON(t, got) != wireJSON(t, want) {
		t.Errorf("the introduction record = %s, want the record /sync serves for the same id %s",
			wireJSON(t, got), wireJSON(t, want))
	}
	if got.Name != "Ada" {
		t.Errorf("theo's introduction names the direct %q, want Ada", got.Name)
	}
	theoS.userFrame()
	theoS.userFrame()
	if m := theoS.message(); string(m.ID) != first.ID.String() {
		t.Errorf("theo's frame after the records = %+v, want %s", m, first.ID)
	}

	adaC := adaS.conversationFrame()
	if adaC.ID != got.ID || adaC.Name != "Theo" {
		t.Errorf("ada's introduction = %+v, want %s named Theo — the statement named it for both members from one side", adaC, direct.ID)
	}
}
