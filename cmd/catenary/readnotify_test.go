package main

// CANT-92's `Done when`, end to end through the real router, the real store
// on Postgres, the real listener and real sockets — the same rig
// fanout_test.go uses for CANT-107's.
//
// MARKERS, NOT SLEEPS, on the same idiom as fanout_test.go: "nothing arrived"
// is proved by a message committed after the action under test, whose
// OnNotify runs strictly after any enqueue the action made, on the one
// listener goroutine.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

// sendAndAwaitOwn sends text into conv over s and reads until the author's
// own ack and `message` frame have both arrived — the two race, as
// TestAMessageReachesEveryAttachedMemberAndNobodyElse already established —
// and returns the message.
func sendAndAwaitOwn(t *testing.T, s *session, conv uuid.UUID, text string) wire.Message {
	t.Helper()
	s.send(wire.ClientSend{ClientID: wire.Uuid(uuid.NewString()), ConversationID: wire.Uuid(conv.String()), Text: &text})
	var ack *wire.ServerAck
	var own *wire.Message
	for ack == nil || own == nil {
		switch f := s.next().(type) {
		case wire.ServerAck:
			a := f
			ack = &a
		case wire.ServerMessageFrame:
			m := f.Message
			own = &m
		default:
			t.Fatalf("unexpected frame on the author's own socket: %T", f)
		}
	}
	if own.ID != ack.MessageID {
		t.Fatalf("own message id %s, ack id %s: mismatch", own.ID, ack.MessageID)
	}
	return *own
}

// readNotifyCap mirrors internal/hub's own readNotifyCap. It cannot be
// imported — the hub keeps it unexported, and CLAUDE.md's Mechanics say
// nothing here should reach across that boundary — so it is restated with a
// pointer back to the constant it has to stay equal to. Not silently: if
// hub.go's number moves, this file's tests keep the OLD number and start
// asserting the wrong count, which is a loud failure rather than a quiet one.
const readNotifyCap = 64

// A member reading a room re-emits each of the author's own messages in the
// span with the new read_by, crossing CANT-90's threshold to `read`; nobody
// else attached to the room receives a re-emission, only the ServerReceipt
// every member already gets.
func TestAReceiptReEmitsTheAuthorsMessagesAndReachesOnlyTheAuthor(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	mallory := mkUser(r.ctx, t, r.pool, "mallory", "Mallory")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo, mallory)
	ada1, theoS, mal := r.open(ada, "laptop"), r.open(theo, "phone"), r.open(mallory, "laptop")

	text1, text2 := "one", "two"
	first := sendAndAwaitOwn(t, ada1, group, text1)
	theoS.message()
	mal.message()

	second := sendAndAwaitOwn(t, ada1, group, text2)
	theoS.message()
	mal.message()

	// Theo reads both. Read_by on each goes from 1 (Ada, by identity) to 2
	// (Ada + Theo, whose mark now passes both) — crossing CANT-90's `> 1`
	// threshold, so both must arrive back at Ada as `read`.
	convA := wire.Uuid(group.String())
	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: wire.Seq(second.Seq)})

	// Ada's socket: the ServerReceipt every member gets, plus exactly two
	// re-emissions — one per message she authored in the span. Order between
	// the synchronous receipt broadcast and the async notify-driven
	// re-emission is not guaranteed, so frames are classified rather than
	// read positionally.
	var receipts, reemissions int
	seen := map[wire.Uuid]wire.Message{}
	for i := 0; i < 3; i++ {
		switch f := ada1.next().(type) {
		case wire.ServerReceipt:
			receipts++
			if string(f.UserID) != theo.String() || int64(f.UpToSeq) != second.Seq {
				t.Errorf("receipt = %+v, want theo at %d", f, second.Seq)
			}
		case wire.ServerMessageFrame:
			reemissions++
			seen[f.Message.ID] = f.Message
		default:
			t.Fatalf("unexpected frame on ada's socket: %T %+v", f, f)
		}
	}
	if receipts != 1 || reemissions != 2 {
		t.Fatalf("ada saw %d receipt(s) and %d re-emission(s), want 1 and 2", receipts, reemissions)
	}
	for _, id := range []wire.Uuid{first.ID, second.ID} {
		m, ok := seen[id]
		if !ok {
			t.Fatalf("message %s was never re-emitted to its author", id)
		}
		if m.State != wire.DeliveryStateRead {
			t.Errorf("message %s state = %s, want read (read_by crossed > 1)", id, m.State)
		}
		if m.ReadBy == nil || *m.ReadBy != 2 {
			t.Errorf("message %s read_by = %v, want 2", id, m.ReadBy)
		}
		if m.ClientID == nil {
			t.Errorf("message %s carries no client_id on its own author's re-emission", id)
		}
	}

	// Theo (the reader) and Mallory (a third attached member) get only the
	// receipt — never a re-emission, because neither authored anything in
	// the span. Proved by the marker: a message committed now is the very
	// next hub frame either of them sees.
	theoS.next() // the ServerReceipt
	mal.next()   // the ServerReceipt
	marker := r.commit(group, ada, "marker")
	for _, s := range []*session{theoS, mal} {
		if m := s.message(); string(m.ID) != marker.ID.String() {
			t.Errorf("%s's next frame after the receipt = %+v, want the marker — "+
				"a non-author received a re-emission", s.user, m)
		}
	}
	// And ada's own next frame, after her two re-emissions, is the marker
	// too — nothing further arrived from the receipt.
	if m := ada1.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("ada's next frame = %+v, want the marker — a third re-emission arrived", m)
	}
}

// A receipt that does not advance the mark — a duplicate or a claim at or
// below the mark already held — puts nothing on the wire for the author
// either, on the same terms CANT-107 already proves for the ServerReceipt
// broadcast (TestReadFansOutAReceiptOnlyWhenTheMarkMoves).
func TestANonAdvancingReceiptReEmitsNothing(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	ada1, theoS := r.open(ada, "laptop"), r.open(theo, "phone")

	sent := r.commit(group, ada, "one")
	ada1.message()
	theoS.message()

	convA := wire.Uuid(group.String())
	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: wire.Seq(sent.Seq)})
	// The receipt, then the one re-emission (read_by 1 -> 2, crossing to
	// read) — consumed so the non-advancing claim below starts from a clean
	// queue on both sockets.
	drainReceiptAndReEmissions(t, theoS, 0)
	drainReceiptAndReEmissions(t, ada1, 1)

	// The identical claim again: Advanced is false. Nothing at all — not a
	// receipt, not a re-emission — reaches either socket. Proved by the
	// marker: a message committed now is the very next hub frame on both.
	theoS.send(wire.ClientRead{ConversationID: convA, UpToSeq: wire.Seq(sent.Seq)})
	theoS.pingPong()
	marker := r.commit(group, ada, "marker")
	for _, s := range []*session{ada1, theoS} {
		if m := s.message(); string(m.ID) != marker.ID.String() {
			t.Errorf("%s's next frame = %+v, want the marker — a non-advancing receipt put something on the wire", s.user, m)
		}
	}
}

// drainReceiptAndReEmissions reads exactly one ServerReceipt and `wantMsgs`
// ServerMessageFrames from s, in any order, and fails on anything else or on
// a short read.
func drainReceiptAndReEmissions(t *testing.T, s *session, wantMsgs int) {
	t.Helper()
	var receipts, msgs int
	for i := 0; i < 1+wantMsgs; i++ {
		switch s.next().(type) {
		case wire.ServerReceipt:
			receipts++
		case wire.ServerMessageFrame:
			msgs++
		}
	}
	if receipts != 1 || msgs != wantMsgs {
		t.Fatalf("%s saw %d receipt(s) and %d message(s), want 1 and %d", s.user, receipts, msgs, wantMsgs)
	}
}

// THE CAP BITES. A span far larger than readNotifyCap — and large enough
// that, uncapped, it would overflow the hub's own outbox bound and sever the
// very author it means to refresh — re-emits exactly the cap, newest first,
// and leaves the author's session alive and reading live traffic afterward.
func TestASpanBeyondTheCapReEmitsOnlyTheNewestCapAndDoesNotSeverTheAuthor(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo)

	// Large enough to overflow outboxBound (256) if the cap did not apply —
	// this is what makes "the author is not severed" a claim a broken cap
	// would actually fail, rather than one that happens to hold at any size.
	// Nobody is attached yet, so these commit with no live fan-out at all.
	const total = 5 * readNotifyCap
	var last int64
	for i := 0; i < total; i++ {
		last = r.commit(group, ada, "m").Seq
	}

	ada1, theoS := r.open(ada, "laptop"), r.open(theo, "phone")
	theoS.send(wire.ClientRead{ConversationID: wire.Uuid(group.String()), UpToSeq: wire.Seq(last)})

	// Theo: only the receipt — he authored nothing in the span.
	if _, ok := theoS.next().(wire.ServerReceipt); !ok {
		t.Fatal("theo's first frame was not the receipt")
	}
	theoS.pingPong()

	// Ada: the receipt plus exactly the cap, classified rather than counted
	// positionally, since the two frame kinds race.
	seen := map[int64]bool{}
	var receipts int
	for i := 0; i < 1+readNotifyCap; i++ {
		switch f := ada1.next().(type) {
		case wire.ServerReceipt:
			receipts++
		case wire.ServerMessageFrame:
			seen[int64(f.Message.Seq)] = true
		default:
			t.Fatalf("unexpected frame: %T %+v", f, f)
		}
	}
	if receipts != 1 {
		t.Fatalf("ada saw %d receipt(s), want 1", receipts)
	}
	if len(seen) != readNotifyCap {
		t.Fatalf("ada received %d distinct re-emissions, want exactly the cap %d", len(seen), readNotifyCap)
	}
	for seq := int64(total-readNotifyCap) + 1; seq <= int64(total); seq++ {
		if !seen[seq] {
			t.Errorf("seq %d (in the newest %d) never arrived", seq, readNotifyCap)
		}
	}
	for seq := int64(1); seq <= int64(total-readNotifyCap); seq++ {
		if seen[seq] {
			t.Errorf("seq %d (older than the newest %d) arrived — the cap did not keep the newest", seq, readNotifyCap)
		}
	}

	// AND ADA'S SESSION IS ALIVE: a ping round-trips, and a message committed
	// now is delivered live. A severed session would answer neither.
	ada1.pingPong()
	marker := r.commit(group, theo, "after the cap")
	if m := ada1.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("ada's next frame after the cap = %+v, want the marker — her session was not severed", m)
	}
}
