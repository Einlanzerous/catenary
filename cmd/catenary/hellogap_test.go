package main

// CANT-102 — CANT-24's criteria 1 to 4, each for the part no earlier test
// proves, over real sockets through setup() on Postgres. What is already
// proved is cited in the PR and on the ticket rather than proved twice.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// dialAs upgrades as an enrolled device and says nothing.
func dialAs(ctx context.Context, t *testing.T, base string, dev wire.EnrollResponse) *websocket.Conn {
	t.Helper()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(base, "http")+"/ws",
		&websocket.DialOptions{Subprotocols: []string{"catenary.v1", "catenary.token." + string(dev.AccessToken)}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func readFrame(ctx context.Context, t *testing.T, conn *websocket.Conn) wire.ServerFrame {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsRead(rctx, t, conn)
}

// Criterion 1. A hello with no cursor, one below head, one at head and one
// above: every frame is read until `ready`, and `ready` is the only one —
// resumed false, log_seq head, nothing streamed and no resync_required, then
// or straight after. A refused hello gets `error` and no `ready`.
func TestEveryHelloIsAnsweredByReadyAtHeadAndNothingElse(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	room := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	// QUIET BEFORE ANY HELLO. A commit's notification reaches the hub after
	// the commit returns, and a session attaching in between receives it —
	// legitimately, and possibly before its `ready`, since the door attaches
	// first. An observer attached before the commits reads each one's frame;
	// once it has, every fan-out for them is done.
	observer := openAt(t, r.ctx, r.base, r.enroll(ada, "observer"), ada)
	for i := 0; i < 3; i++ {
		r.commit(room, ada, fmt.Sprintf("m%d", i))
		observer.message()
	}
	head := r.head()
	dev := r.enroll(theo, "phone")
	at := func(v int64) *int64 { return &v }

	for _, tc := range []struct {
		name    string
		cursor  *int64
		outcome store.HelloOutcome
	}{
		{"no cursor", nil, store.HelloNoCursor},
		{"below head", at(head - 2), store.HelloBehind},
		{"at head", at(head), store.HelloAtHead},
		{"above head", at(head + 5), store.HelloCursorAhead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opened := len(r.log.find("session open"))
			conn := dialAs(r.ctx, t, r.base, dev)
			wsSend(r.ctx, t, conn, wire.ClientHello{WireVersion: wire.WireVersion, DeviceID: dev.DeviceID, ResumeFromLogSeq: tc.cursor})

			var ready wire.ServerReady
			for i := 0; ; i++ {
				f := readFrame(r.ctx, t, conn)
				if rd, ok := f.(wire.ServerReady); ok {
					ready = rd
					break
				}
				t.Errorf("frame %d before ready is %T %+v; a hello is answered by ready alone", i, f, f)
			}
			if ready.Resumed || ready.LogSeq != head {
				t.Errorf("ready = resumed %v log_seq %d, want false / head %d", ready.Resumed, ready.LogSeq, head)
			}
			// Nothing follows it that answers the hello: the next frame is the
			// pong to a ping sent after it.
			wsSend(r.ctx, t, conn, wire.Ping{ID: "after-ready"})
			if f := readFrame(r.ctx, t, conn); f == nil || f.WireTag() != "pong" || f.(wire.Pong).ID != "after-ready" {
				t.Errorf("the frame after ready is %T %+v, want the pong", f, f)
			}

			// And the server heard the cursor this case sent. The door logs
			// "session open" with the outcome before its read loop starts, so
			// once the pong is back this session's line is the one new line.
			if lines := r.log.find("session open"); len(lines) != opened+1 {
				t.Errorf("%d new session open lines, want 1", len(lines)-opened)
			} else if got := attrOf(lines[opened], "hello_outcome"); got != string(tc.outcome) {
				t.Errorf("hello_outcome %v for this session, want %s", got, tc.outcome)
			}
		})
	}

	other := r.enroll(theo, "laptop")
	for name, hello := range map[string]wire.ClientHello{
		"refused: an unsupported wire version": {WireVersion: wire.WireVersion + 1, DeviceID: dev.DeviceID},
		"refused: another device's id":         {WireVersion: wire.WireVersion, DeviceID: other.DeviceID},
	} {
		t.Run(name, func(t *testing.T) {
			conn := dialAs(r.ctx, t, r.base, dev)
			wsSend(r.ctx, t, conn, hello)
			var frames []wire.ServerFrame
			for {
				rctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
				_, data, err := conn.Read(rctx)
				cancel()
				if err != nil {
					if st := websocket.CloseStatus(err); st != websocket.StatusPolicyViolation {
						t.Errorf("the socket ended with %v, want close 1008", err)
					}
					break
				}
				f, err := wire.DecodeServerFrame(data)
				if err != nil {
					t.Fatalf("undecodable frame %s: %v", data, err)
				}
				frames = append(frames, f)
			}
			if len(frames) != 1 {
				t.Fatalf("frames before the close = %+v, want exactly one error", frames)
			}
			if _, ok := frames[0].(wire.ServerError); !ok {
				t.Errorf("the one frame is %T, want error", frames[0])
			}
		})
	}
}

// Criterion 2. A `send`, a `read` and a `ping` written straight after the hello,
// before `ready` has been read, are acked, applied and answered exactly as
// the same frames after it. That the heartbeat clock starts only at `ready` is
// internal/api's TestHeartbeatClockDoesNotStartBeforeReady.
func TestFramesWrittenBeforeReadyAreAcceptedAndAnswered(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	room := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	// Quiet before the hello, as above: the observer has read both frames, so
	// neither can reach the session under test.
	observer := openAt(t, r.ctx, r.base, r.enroll(ada, "observer"), ada)
	r.commit(room, ada, "one")
	r.commit(room, ada, "two")
	observer.message()
	observer.message()
	head := r.head()
	dev := r.enroll(theo, "phone")
	conn := dialAs(r.ctx, t, r.base, dev)

	type outcome struct {
		ack     wire.ServerAck
		own     wire.Message
		receipt wire.ServerReceipt
	}
	// collect reads until one of each has arrived, in whatever order the read
	// loop and the hub's outbox produce them.
	collect := func(clientID uuid.UUID, pingID string) outcome {
		t.Helper()
		var o outcome
		var gotAck, gotOwn, gotReceipt, gotPong bool
		for !gotAck || !gotOwn || !gotReceipt || !gotPong {
			switch f := readFrame(r.ctx, t, conn).(type) {
			case wire.ServerAck:
				o.ack, gotAck = f, true
			case wire.ServerMessageFrame:
				o.own, gotOwn = f.Message, true
			case wire.ServerReceipt:
				o.receipt, gotReceipt = f, true
			case wire.Pong:
				gotPong = f.ID == pingID
			default:
				t.Fatalf("unexpected frame %T %+v", f, f)
			}
		}
		if string(o.ack.ClientID) != clientID.String() || o.ack.Duplicate != nil {
			t.Errorf("ack = %+v, want client_id %s and no duplicate", o.ack, clientID)
		}
		if o.own.ID != o.ack.MessageID || o.own.State != wire.DeliveryStateSent || o.own.ClientID == nil {
			t.Errorf("own message frame = %+v, want the acked message, sent, client_id echoed", o.own)
		}
		if string(o.receipt.UserID) != theo.String() || string(o.receipt.ConversationID) != room.String() {
			t.Errorf("receipt = %+v, want theo in A", o.receipt)
		}
		return o
	}
	readSeq := func() int64 {
		t.Helper()
		var v int64
		if err := r.pool.QueryRow(r.ctx, `SELECT read_seq FROM conversation_members WHERE conversation_id = $1 AND user_id = $2`, room, theo).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// Four frames on the wire before one is read.
	before := uuid.New()
	text := "sent before ready"
	wsSend(r.ctx, t, conn, wire.ClientHello{WireVersion: wire.WireVersion, DeviceID: dev.DeviceID})
	wsSend(r.ctx, t, conn, wire.ClientSend{ClientID: wid(before), ConversationID: wid(room), Text: &text})
	wsSend(r.ctx, t, conn, wire.ClientRead{ConversationID: wid(room), UpToSeq: 2})
	wsSend(r.ctx, t, conn, wire.Ping{ID: "before-ready"})

	if f, ok := readFrame(r.ctx, t, conn).(wire.ServerReady); !ok || f.LogSeq != head {
		t.Fatalf("first frame = %+v, want ready at head %d", f, head)
	}
	pre := collect(before, "before-ready")
	if pre.ack.Seq != 3 || pre.ack.LogSeq != head+1 {
		t.Errorf("pre-ready ack = seq %d log_seq %d, want 3 / %d", pre.ack.Seq, pre.ack.LogSeq, head+1)
	}
	if pre.receipt.UpToSeq != 2 || readSeq() != 2 {
		t.Errorf("pre-ready read: receipt up_to_seq %d, stored read_seq %d — want 2 and 2", pre.receipt.UpToSeq, readSeq())
	}

	// The same three frames after `ready`, on the same socket: the same
	// answers, one seq on.
	after := uuid.New()
	text = "sent after ready"
	wsSend(r.ctx, t, conn, wire.ClientSend{ClientID: wid(after), ConversationID: wid(room), Text: &text})
	wsSend(r.ctx, t, conn, wire.ClientRead{ConversationID: wid(room), UpToSeq: 3})
	wsSend(r.ctx, t, conn, wire.Ping{ID: "after-ready"})
	post := collect(after, "after-ready")
	if post.ack.Seq != pre.ack.Seq+1 || post.ack.LogSeq <= pre.ack.LogSeq {
		t.Errorf("post-ready ack = seq %d log_seq %d, want seq %d and a later log_seq than %d", post.ack.Seq, post.ack.LogSeq, pre.ack.Seq+1, pre.ack.LogSeq)
	}
	if post.receipt.UpToSeq != 3 || readSeq() != 3 {
		t.Errorf("post-ready read: receipt up_to_seq %d, stored read_seq %d — want 3 and 3", post.receipt.UpToSeq, readSeq())
	}
}

// Criterion 3, the half TestAListenerGapTellsEverySessionToResyncAndClosesNone
// does not reach: messages COMMIT while the listener is down. The proxy holds
// the listener's connection severed while they do, so they are missed by
// construction. Every session gets exactly one resync_required at a head at or
// above the head when the connection dropped, none is closed, no message frame
// is fabricated for what was missed, and one WARN names the count and the head.
func TestAListenerGapWhileMessagesCommitFabricatesNothing(t *testing.T) {
	k := newKillRig(t, nil)
	ada := mkUser(k.ctx, t, k.pool, "ada", "Ada")
	theo := mkUser(k.ctx, t, k.pool, "theo", "Theo")
	mal := mkUser(k.ctx, t, k.pool, "mallory", "Mallory")
	roomA := mkGroup(k.ctx, t, k.pool, "A", ada, theo)
	roomB := mkGroup(k.ctx, t, k.pool, "B", ada, theo, mal)
	sessions := []*session{
		openAt(t, k.ctx, k.base(), k.enroll(ada, "laptop"), ada),
		openAt(t, k.ctx, k.base(), k.enroll(ada, "phone"), ada),
		openAt(t, k.ctx, k.base(), k.enroll(theo, "phone"), theo),
	}
	send(k.ctx, t, k.st, roomA, ada, "before the gap")
	for _, s := range sessions {
		s.message()
	}

	k.proxy.sever()
	headAtGap := k.head()
	for i, room := range []uuid.UUID{roomA, roomB, roomA} {
		author := ada
		if room == roomB {
			author = mal
		}
		send(k.ctx, t, k.st, room, author, fmt.Sprintf("missed %d", i))
	}
	headAfter := k.head()
	k.proxy.restore()

	for _, s := range sessions {
		f := s.next()
		rs, ok := f.(wire.ServerResyncRequired)
		if !ok || rs.Reason != wire.ResyncReasonCursorTooOld || rs.LogSeq < headAtGap || rs.LogSeq != headAfter {
			t.Errorf("frame after the gap = %T %+v, want resync_required{cursor_too_old} at %d (at or above %d, the head at the drop)", f, f, headAfter, headAtGap)
		}
		s.pingPong() // exactly one, and the socket is open
	}

	gaps := k.log.find("listener gap")
	if len(gaps) != 1 {
		t.Fatalf("listener gap lines = %d, want one", len(gaps))
	}
	if gaps[0].Level != slog.LevelWarn || attrOf(gaps[0], "sessions_notified") != int64(3) || attrOf(gaps[0], "head") != headAfter {
		t.Errorf("listener gap line = %s %+v, want WARN with sessions_notified=3 head=%d", gaps[0].Level, gaps[0], headAfter)
	}

	// The marker is delivered by an OnNotify that runs after any frame the
	// gap could have produced, so it must be every session's next frame.
	marker := send(k.ctx, t, k.st, roomA, theo, "after the gap")
	for _, s := range sessions {
		if m := s.message(); string(m.ID) != marker.ID.String() {
			t.Errorf("next frame = %s (log_seq %d), want the marker — a frame was fabricated for a missed message", m.ID, m.LogSeq)
		}
	}
}

// Criterion 4, the socket side. The hub-level test
// (TestAReEmissionWithAnOldLogSeqIsDelivered) proves a re-emission is not
// dropped; nothing produces one yet (CANT-92), so this raises the notification
// CANT-92 will raise — by hand, through the real listener and hub — after a
// read has changed read_by. A raw session receives the message again with its
// original log_seq; the Go client replaces its record without counting it
// again or moving its cursor; and a client that dedupes by log_seq keeps the
// stale record, which is the refresh the schema's ServerFrame text says it
// would lose.
func TestAReEmissionCrossesTheRealStackAndReplacesTheRecord(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	room := mkGroup(r.ctx, t, r.pool, "A", ada, theo)

	raw := openAt(t, r.ctx, r.base, r.enroll(theo, "laptop"), theo)
	good := runClient(t, r.ctx, r.base, r.enroll(theo, "phone"), nil, client.Faults{})
	broken := runClient(t, r.ctx, r.base, r.enroll(theo, "tablet"), nil, client.Faults{DedupeByLogSeq: true})

	// Both clients attached before the commits (`ready` follows attach), so
	// each gets both frames live, and both have before the baseline is read.
	for _, c := range []*client.Client{good, broken} {
		awaitClient(t, c, "ready before the commits", func() bool { return c.Status().Ready })
	}
	first := r.commit(room, ada, "one")
	r.commit(room, ada, "two")
	raw.message()
	raw.message()
	head := r.head()
	for _, c := range []*client.Client{good, broken} {
		c.CatchUp()
		awaitClient(t, c, "both live frames, and caught up at head", func() bool {
			s := c.Status()
			return s.LiveFrames == 2 && s.CaughtUp && s.Cursor == head && s.Messages == 2
		})
	}
	id := wid(first.ID)
	if m, _ := good.Message(id); m.ReadBy == nil || *m.ReadBy != 1 {
		t.Fatalf("read_by before the read = %v, want 1 (the author)", m.ReadBy)
	}
	goodBefore, brokenBefore := good.Status(), broken.Status()
	counted := len(good.Snapshot().Counted)

	if _, err := r.st.MarkRead(r.ctx, room, theo, first.Seq); err != nil {
		t.Fatal(err)
	}
	payload, err := store.NotifyPayload{ConversationID: room, Seq: first.Seq}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(r.ctx, `SELECT pg_notify($1, $2)`, store.NotifyChannel, payload); err != nil {
		t.Fatal(err)
	}

	m := raw.message()
	if m.ID != id || m.LogSeq != first.LogSeq || m.ReadBy == nil || *m.ReadBy != 2 {
		t.Fatalf("re-emitted frame = id %s log_seq %d read_by %v, want %s at its original log_seq %d with read_by 2", m.ID, m.LogSeq, m.ReadBy, id, first.LogSeq)
	}

	awaitClient(t, good, "the refreshed record", func() bool {
		m, _ := good.Message(id)
		return m.ReadBy != nil && *m.ReadBy == 2
	})
	after := good.Status()
	if after.LiveFrames != goodBefore.LiveFrames+1 || after.Cursor != goodBefore.Cursor || len(good.Snapshot().Counted) != counted {
		t.Errorf("the re-emission: live frames %d→%d, cursor %d→%d, counted %d→%d — want one frame, no cursor move, no second count",
			goodBefore.LiveFrames, after.LiveFrames, goodBefore.Cursor, after.Cursor, counted, len(good.Snapshot().Counted))
	}

	awaitClient(t, broken, "the re-emission at the broken client", func() bool {
		return broken.Status().LiveFrames > brokenBefore.LiveFrames
	})
	if m, _ := broken.Message(id); m.ReadBy == nil || *m.ReadBy != 1 {
		t.Errorf("a client deduping by log_seq holds read_by %v, want the stale 1 — the refresh it drops", m.ReadBy)
	}
}
