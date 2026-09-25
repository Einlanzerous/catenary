package store

// CANT-143 — store criteria 1, 5 and 6 of CANT-140's plan: an offboard and its
// reversal raise the receipt's own notify, once per room the person had read,
// gated on the column moving; and nothing that moved no column raises any.
//
// WHAT IS ASSERTED HERE IS THE PAYLOAD, NOT THE RE-EMISSION. The hub's
// consumer is unchanged (that is ruling 1's whole economy), and what it does
// with a receipt-shaped payload is CANT-92's tests' claim; what is new is that
// two more writers raise one, and this file pins exactly which payloads each
// raises. The re-emission itself, end to end through real sockets, is
// cmd/catenary/readspan_test.go.
//
// CRITERION 5 IS COVERED BY SHAPE: every payload asserted below is decoded as
// the receipt's own five-field NotifyPayload, with `seq` absent, so the widest
// thing an offboard raises is the thing TestPostgresRefusesAPayloadOverThe
// DocumentedLimit already bounds. Nothing here widened the struct.
//
// A MARKER, NOT A SLEEP, on revocationsDuring's own terms: the listener is
// proved subscribed before the act, a sentinel is published after it, and
// everything ahead of the sentinel is what the act published — so "raised
// nothing" is an assertion and not a timeout.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sentinelConversation is a conversation id nothing in this package ever
// creates, so a NotifyPayload naming it can only be a test's own marker.
const sentinelConversation = "00000000-0000-4000-8000-00000000c143"

func messageSentinel() (string, func(NotifyPayload) bool) {
	return `{"conversation_id":"` + sentinelConversation + `","seq":1}`,
		func(p NotifyPayload) bool { return p.ConversationID.String() == sentinelConversation }
}

// notifiesDuring is revocationsDuring for NotifyChannel: every payload the act
// raised on the message channel, in order, and an empty slice when it raised
// none.
func notifiesDuring(ctx context.Context, t *testing.T, pool *pgxpool.Pool, during func()) []NotifyPayload {
	t.Helper()
	got := make(chan NotifyPayload, 64)
	l := &Listener[NotifyPayload]{
		DSN: testDSN(t), Channel: NotifyChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p NotifyPayload) { got <- p },
		OnGap:    func(context.Context) {},
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = l.Run(runCtx) }()

	sentinel, isSentinel := messageSentinel()
	awaitSubscribed(ctx, t, pool, NotifyChannel, got, sentinel, isSentinel)

	during()

	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, sentinel); err != nil {
		t.Fatalf("trailing sentinel: %v", err)
	}
	var out []NotifyPayload
	for {
		select {
		case p := <-got:
			if isSentinel(p) {
				return out
			}
			out = append(out, p)
		case <-time.After(15 * time.Second):
			t.Fatal("the trailing sentinel never arrived")
		}
	}
}

// receiptsOnly keeps the receipt-shaped payloads, keyed by room. A message
// notification in the same window (none is expected here, since nobody sends)
// would be a different claim and is reported rather than dropped.
func receiptsOnly(t *testing.T, got []NotifyPayload) map[uuid.UUID]NotifyPayload {
	t.Helper()
	out := map[uuid.UUID]NotifyPayload{}
	for _, p := range got {
		if !p.IsReceipt() {
			t.Errorf("a MESSAGE notification %+v was raised during an act that sent nothing", p)
			continue
		}
		if _, dup := out[p.ConversationID]; dup {
			t.Errorf("room %s was notified twice in one act; one span per room", p.ConversationID)
		}
		out[p.ConversationID] = p
	}
	return out
}

// withReadSpan puts the person in one room with a co-member and a message they
// have read, so a test asserting "raises no read span" is asserting it about
// somebody an offboard WOULD raise one for. Returns the room.
func withReadSpan(ctx context.Context, t *testing.T, st *Store, pool *pgxpool.Pool, person uuid.UUID) uuid.UUID {
	t.Helper()
	other := mkUser(ctx, t, pool, "co-member-"+uuid.NewString()[:8])
	room := mkGroup(ctx, t, pool, "with-a-span", person, other)
	send(ctx, t, st, room, other, "read by the person")
	if _, err := st.MarkRead(ctx, room, person, 1); err != nil {
		t.Fatalf("fixture: mark read: %v", err)
	}
	return room
}

// readSpans is the fixture both directions are asserted over: the offboarded
// person in three rooms with a co-member, having read all of the first, part of
// the second, and none of the third — and a fourth room where only they ever
// wrote, so their own messages are shown not to make a span.
type readSpans struct {
	f                              offboarded
	theo                           uuid.UUID
	allRead, partRead, unread, own uuid.UUID
}

func newReadSpans(ctx context.Context, t *testing.T, st *Store, pool *pgxpool.Pool) readSpans {
	t.Helper()
	f := newOffboarded(ctx, t, st)
	theo := mkUser(ctx, t, pool, "theo")
	r := readSpans{
		f: f, theo: theo,
		allRead:  mkGroup(ctx, t, pool, "all-read", f.UserID, theo),
		partRead: mkGroup(ctx, t, pool, "part-read", f.UserID, theo),
		unread:   mkGroup(ctx, t, pool, "unread", f.UserID, theo),
		own:      mkGroup(ctx, t, pool, "own", f.UserID, theo),
	}
	for i := 0; i < 3; i++ {
		send(ctx, t, st, r.allRead, theo, "read")
		send(ctx, t, st, r.partRead, theo, "some read")
	}
	send(ctx, t, st, r.unread, theo, "never read")
	send(ctx, t, st, r.own, f.UserID, "mine")
	if _, err := st.MarkRead(ctx, r.allRead, f.UserID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkRead(ctx, r.partRead, f.UserID, 1); err != nil {
		t.Fatal(err)
	}
	return r
}

// wantSpans is the set both directions must raise: `(0, read_seq]` for each
// room with a span, nothing for the others.
func (r readSpans) assertSpans(t *testing.T, got []NotifyPayload, direction string) {
	t.Helper()
	spans := receiptsOnly(t, got)
	want := map[uuid.UUID]int64{r.allRead: 3, r.partRead: 1}
	for room, upTo := range want {
		p, ok := spans[room]
		if !ok {
			t.Errorf("%s: no receipt-shaped notify for room %s; the messages the person had read there "+
				"keep their old read_by on every live author's screen", direction, room)
			continue
		}
		if *p.UserID != r.f.UserID || p.Before != 0 || p.After != upTo {
			t.Errorf("%s: room %s notified as (user %s, %d, %d], want (user %s, 0, %d] — the span is "+
				"everything the person had read, from the start", direction, room, *p.UserID, p.Before, p.After,
				r.f.UserID, upTo)
		}
		if p.Seq != 0 {
			t.Errorf("%s: room %s's payload carries seq %d; a receipt shape carries none", direction, room, p.Seq)
		}
	}
	for _, room := range []uuid.UUID{r.unread, r.own} {
		if p, ok := spans[room]; ok {
			t.Errorf("%s: room %s was notified %+v; the person had read nothing there, so no message's "+
				"read_by counted them and there is nothing to re-emit", direction, room, p)
		}
	}
	if len(spans) != len(want) {
		t.Errorf("%s: %d rooms notified, want %d", direction, len(spans), len(want))
	}
}

// CRITERION 1 — one offboard across four rooms raises exactly the two spans;
// its reversal raises the identical two; and the reversal raises them even
// though it has nothing live to revoke, because the gate is the column and
// not the credential sweep.
func TestAnOffboardAndItsReversalEachRaiseOneReadSpanPerRoomThePersonHadRead(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	r := newReadSpans(ctx, t, st, pool)

	var out Offboard
	got := notifiesDuring(ctx, t, pool, func() {
		var err error
		if out, err = st.DeactivateUser(ctx, r.f.UserID); err != nil {
			t.Errorf("deactivate: %v", err)
		}
	})
	if !out.Deactivated {
		t.Fatal("the offboard did not move the column; nothing below is about anything")
	}
	r.assertSpans(t, got, "offboard")

	// THE REVERSAL. Every credential is already revoked, so the revocation
	// channel stays silent (TestAReactivationWithNothingLiveToRevokePublishes
	// Nothing) — and the read spans are raised regardless, because what they
	// announce is the column moving and not a socket to sever.
	var back EnsuredPerson
	var revocations []RevocationPayload
	got = notifiesDuring(ctx, t, pool, func() {
		revocations = revocationsDuring(ctx, t, pool, func() {
			var err error
			if back, err = st.EnsurePerson(ctx, r.f.Email, "Ada Lovelace"); err != nil {
				t.Errorf("reactivate: %v", err)
			}
		})
	})
	if back.Outcome != PersonReactivated {
		t.Fatalf("outcome = %q, want reactivated", back.Outcome)
	}
	if len(revocations) != 0 {
		t.Errorf("%d revocations published by a reversal with nothing live to revoke; the read-span "+
			"notify must not be gated on that", len(revocations))
	}
	r.assertSpans(t, got, "reversal")
}

// CANT-146 — the payloads one offboard raises are ONE COMMIT'S, and they say
// so. The hub bounds what it re-emits to an author across a batch, so what has
// to hold here is that every room's payload in one call names the same batch,
// that the reversal is a batch of its own, and that a single room's MarkRead
// names none — the hub's per-payload cap is that receipt's whole rule, and a
// batch on it would be a budget nothing else in the commit shares.
func TestAnOffboardsReadSpansShareOneBatchAndItsReversalDrawsAnother(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	r := newReadSpans(ctx, t, st, pool)

	batchOf := func(direction string, got []NotifyPayload) uuid.UUID {
		t.Helper()
		spans := receiptsOnly(t, got)
		if len(spans) < 2 {
			t.Fatalf("%s raised %d spans; the fixture has two rooms with one, and one room cannot share a batch with anything",
				direction, len(spans))
		}
		var batch *uuid.UUID
		for room, p := range spans {
			if p.Batch == nil {
				t.Fatalf("%s: room %s's payload names no batch; the hub would spend a budget per payload again", direction, room)
			}
			if *p.Batch == uuid.Nil {
				t.Fatalf("%s: room %s's batch is the zero id, which every unbatched payload would also read as", direction, room)
			}
			if batch != nil && *batch != *p.Batch {
				t.Errorf("%s: rooms named batches %s and %s; one commit is one batch", direction, batch, p.Batch)
			}
			batch = p.Batch
		}
		return *batch
	}

	got := notifiesDuring(ctx, t, pool, func() {
		if _, err := st.DeactivateUser(ctx, r.f.UserID); err != nil {
			t.Errorf("deactivate: %v", err)
		}
	})
	offboard := batchOf("offboard", got)

	got = notifiesDuring(ctx, t, pool, func() {
		if _, err := st.EnsurePerson(ctx, r.f.Email, "Ada Lovelace"); err != nil {
			t.Errorf("reactivate: %v", err)
		}
	})
	if reversal := batchOf("reversal", got); reversal == offboard {
		t.Errorf("the offboard and its reversal named the same batch %s; each owes the hub its own budget", offboard)
	}

	// A SINGLE ROOM'S RECEIPT NAMES NONE. The person is active again, so their
	// mark can move: the room they had read none of.
	got = notifiesDuring(ctx, t, pool, func() {
		if _, err := st.MarkRead(ctx, r.unread, r.f.UserID, 1); err != nil {
			t.Errorf("mark read: %v", err)
		}
	})
	for room, p := range receiptsOnly(t, got) {
		if p.Batch != nil {
			t.Errorf("a plain MarkRead in room %s named batch %s; only an offboard's per-room fan-out is a batch", room, p.Batch)
		}
	}
}

// AND NOTHING MOVED THE PERSON'S OWN MARK. The reversal's spans equal the
// offboard's because `read_seq` cannot move while the account is disabled;
// this is the row-level fact behind that sentence.
func TestAnOffboardAndItsReversalLeaveEveryReadSeqWhereItWas(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	r := newReadSpans(ctx, t, st, pool)

	marks := func() map[uuid.UUID]int64 {
		rows, err := pool.Query(ctx, `SELECT conversation_id, read_seq FROM conversation_members WHERE user_id = $1`, r.f.UserID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[uuid.UUID]int64{}
		for rows.Next() {
			var c uuid.UUID
			var n int64
			if err := rows.Scan(&c, &n); err != nil {
				t.Fatal(err)
			}
			out[c] = n
		}
		return out
	}
	before := marks()
	if _, err := st.DeactivateUser(ctx, r.f.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsurePerson(ctx, r.f.Email, "Ada Lovelace"); err != nil {
		t.Fatal(err)
	}
	after := marks()
	for room, n := range before {
		if after[room] != n {
			t.Errorf("room %s: read_seq %d -> %d across an offboard and its reversal; neither direction "+
				"writes a receipt (ruling 7)", room, n, after[room])
		}
	}
}

// CRITERION 1, THE NEGATIVE HALF — a plain re-invite of an ACTIVE person moves
// no column and raises no span. The offboard's two negatives (a converged
// retry and a credential-repairing retry) are asserted in offboard_test.go,
// beside the revocation assertions they extend.
func TestAPlainReInviteOfAnActivePersonRaisesNoReadSpan(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	r := newReadSpans(ctx, t, st, pool)

	var again EnsuredPerson
	got := notifiesDuring(ctx, t, pool, func() {
		var err error
		if again, err = st.EnsurePerson(ctx, r.f.Email, "Ada Lovelace"); err != nil {
			t.Errorf("re-invite: %v", err)
		}
	})
	if again.Outcome != PersonExisting {
		t.Fatalf("outcome = %q, want existing — this test is about a person who was never offboarded", again.Outcome)
	}
	if len(got) != 0 {
		t.Errorf("%d payloads raised by a re-invite of an active person, want 0: %+v — no served value "+
			"changed, so there is nothing to re-emit and the hub must not be woken to find that out",
			len(got), got)
	}
}
