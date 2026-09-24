package main

// CANT-143 — store criteria 2 and 3 of CANT-140's plan, end to end: the real
// router, the real store on Postgres, both real listeners, real sockets. An
// offboard and its reversal re-emit, to each live AUTHOR and to nobody else,
// every message of theirs the person had read — with `read_by` recomputed and
// the `state` rung moved with it — exactly as a receipt does (CANT-92), and
// through the same hub path, which this ticket does not touch.
//
// THE TWO-DEVICE CASE IS THE TICKET'S §2 AND THE FAILING FORM IS KEPT. On
// `main` a connected device holds `read` for a message the server now serves
// as `sent`, and a device enrolled afterwards bootstraps `sent`: one account,
// two answers, until the first one bootstraps. After this change the connected
// device is re-emitted `sent` before the second one ever asks. The assertion
// is written so that `main` fails it at "next frame is the marker, not the
// re-emission" — which is what it did, before the store change landed.
//
// AND THE OFFLINE WINDOW IS ASSERTED AS WHAT RULING 1 PICKED, not hidden: a
// device detached for the offboard is not told by its catch-up, because a
// catch-up never re-serves a held message (CANT-89 ruling 2), and it learns
// on its next bootstrap. Option A accepted that knowingly; the last test here
// is the record of it, and if a later ticket closes the window it is the test
// to invert.
//
// MARKERS, NOT SLEEPS, on readnotify_test.go's idiom: "nothing arrived" is
// a message committed after the act, seen as the very next hub frame.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/hub"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// person is a user made through EnsurePerson rather than mkUser, because the
// reversal is EnsurePerson on the same email and mkUser writes none.
func person(r *rig, email, name string) uuid.UUID {
	r.t.Helper()
	ep, err := r.st.EnsurePerson(r.ctx, email, name)
	if err != nil {
		r.t.Fatalf("ensure %s: %v", email, err)
	}
	return ep.Account.UserID
}

// readReEmissions reads exactly n `message` frames from s, fails on anything
// else, and returns them by seq. A re-emission is an ordinary message frame;
// what tells it from a first delivery is that the test knows nobody sent.
func readReEmissions(t *testing.T, s *session, n int) map[int64]wire.Message {
	t.Helper()
	out := map[int64]wire.Message{}
	for i := 0; i < n; i++ {
		f := s.next()
		m, ok := f.(wire.ServerMessageFrame)
		if !ok {
			t.Fatalf("%s: frame %d = %T %+v, want a re-emitted message", s.user, i, f, f)
		}
		out[int64(m.Message.Seq)] = m.Message
	}
	return out
}

// wantRung asserts a re-emitted own message carries the count and the word
// derived from it — the two are one answer (CANT-90 ruling 2), so a test
// that checked one would be trusting the other.
func wantRung(t *testing.T, what string, m wire.Message, readBy int64, state wire.DeliveryState) {
	t.Helper()
	if m.ReadBy == nil || *m.ReadBy != readBy {
		t.Errorf("%s: seq %d read_by = %v, want %d", what, m.Seq, m.ReadBy, readBy)
	}
	if m.State != state {
		t.Errorf("%s: seq %d state = %s, want %s", what, m.Seq, m.State, state)
	}
}

// syncAfter is GET /sync?after=N for one device — the catch-up a reconnecting
// client runs, as opposed to syncPage's bootstrap from 0.
func (r *rig) syncAfter(token string, after int64) wire.SyncResponse {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/sync?after="+strconv.FormatInt(after, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	code, raw := do(r.t, r.d.router, req)
	if code != http.StatusOK {
		r.t.Fatalf("GET /sync?after=%d = %d: %s", after, code, raw)
	}
	var page wire.SyncResponse
	if err := json.Unmarshal(raw, &page); err != nil {
		r.t.Fatalf("decode /sync: %v", err)
	}
	return page
}

// CRITERION 2 — the offboard re-emits, to the live author, each of their own
// messages the person had read, with the count lowered and the rung dropped;
// the reversal re-emits them with both restored; a third attached member gets
// nothing either time; and the person's own socket is severed by the
// revocation the same commit publishes, in whichever order the two
// notifications drain.
func TestAnOffboardAndItsReversalReEmitTheAuthorsReadMessagesToTheAuthorAlone(t *testing.T) {
	r := newRig(t)
	runRevocations(r)
	const adaEmail = "ada@example.com"
	ada := person(r, adaEmail, "Ada Lovelace")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	mallory := mkUser(r.ctx, t, r.pool, "mallory", "Mallory")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo, mallory)
	theoS, malS, adaS := r.open(theo, "phone"), r.open(mallory, "laptop"), r.open(ada, "tablet")

	// Theo writes two; Ada reads both; Mallory reads neither. read_by on each
	// goes 1 → 2 (Theo by identity + Ada), so both cross to `read` at Theo.
	first := sendAndAwaitOwn(t, theoS, group, "one")
	adaS.message()
	malS.message()
	second := sendAndAwaitOwn(t, theoS, group, "two")
	adaS.message()
	malS.message()

	conv := wire.Uuid(group.String())
	adaS.send(wire.ClientRead{ConversationID: conv, UpToSeq: wire.Seq(second.Seq)})
	readsReceiptAndReEmissions(t, theoS, first.Seq, second.Seq)
	drainReceiptAndReEmissions(t, malS, 0)
	drainReceiptAndReEmissions(t, adaS, 0)

	// THE OFFBOARD. Nobody sends anything.
	out, err := r.st.DeactivateUser(r.ctx, ada)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if !out.Deactivated {
		t.Fatal("the offboard moved no column; nothing below is about anything")
	}

	// Ada's socket ends with the terminal code. Her own re-emissions do not
	// exist to race this: MessagesForReadNotify excludes the reader's own
	// messages, and she authored nothing here anyway.
	if code := adaS.expectClose(); code != hub.StatusRevoked {
		t.Errorf("ada's socket closed with %d, want %d", code, hub.StatusRevoked)
	}

	// Theo: both messages again, read_by back to 1 (himself), state `sent` —
	// THE LADDER GOES BACKWARDS, which nothing in this service could do before
	// CANT-137 and which the wire now says it may.
	got := readReEmissions(t, theoS, 2)
	for _, seq := range []int64{first.Seq, second.Seq} {
		m, ok := got[seq]
		if !ok {
			t.Fatalf("seq %d was not re-emitted to its author after the offboard; his screen still says "+
				"READ 2/3 about a room of two", seq)
		}
		wantRung(t, "after the offboard", m, 1, wire.DeliveryStateSent)
	}

	// Mallory: nothing — she authored nothing in the span. Proved by the
	// marker, which is also Theo's very next frame.
	marker := r.commit(group, mallory, "marker")
	if m := malS.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("mallory's next frame = %+v, want the marker — a non-author received a re-emission", m)
	}
	if m := theoS.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("theo's next frame after his two re-emissions = %+v, want the marker", m)
	}

	// THE REVERSAL, on the same terms: the count and the rung come back.
	back, err := r.st.EnsurePerson(r.ctx, adaEmail, "Ada Lovelace")
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if back.Outcome != store.PersonReactivated {
		t.Fatalf("outcome = %q, want reactivated", back.Outcome)
	}
	got = readReEmissions(t, theoS, 2)
	for _, seq := range []int64{first.Seq, second.Seq} {
		m, ok := got[seq]
		if !ok {
			t.Fatalf("seq %d was not re-emitted to its author after the reversal; his screen says READ 1/3 "+
				"about a message two people have read, indefinitely", seq)
		}
		wantRung(t, "after the reversal", m, 2, wire.DeliveryStateRead)
	}
	marker = r.commit(group, mallory, "marker after the reversal")
	if m := malS.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("mallory's next frame = %+v, want the marker", m)
	}
	if m := theoS.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("theo's next frame after the reversal's re-emissions = %+v, want the marker", m)
	}
}

// CRITERION 3 — two serves, two devices, one account: the ticket's §2 for a
// CONNECTED device is reachable before this change and not after.
//
// Device A is attached and holds `read`. Ada is offboarded. Device B is
// enrolled afterwards and bootstraps. On `main`, A was never told and holds
// `read` while B is served `sent`; here A is re-emitted `sent` before B ever
// asks, and the two agree. The failing form on `main` is the first assertion:
// A's next frame after the offboard was the marker, not the re-emission.
func TestTwoDevicesOfOneAccountAgreeAboutARungTheOffboardLowered(t *testing.T) {
	r := newRig(t)
	runRevocations(r)
	ada := person(r, "ada@example.com", "Ada Lovelace")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	deviceA := r.open(theo, "phone")
	adaS := r.open(ada, "tablet")

	m := sendAndAwaitOwn(t, deviceA, group, "hello")
	adaS.message()
	adaS.send(wire.ClientRead{ConversationID: wire.Uuid(group.String()), UpToSeq: wire.Seq(m.Seq)})
	readsReceiptAndReEmissions(t, deviceA, m.Seq) // A now holds `read`, read_by 2
	drainReceiptAndReEmissions(t, adaS, 0)

	if _, err := r.st.DeactivateUser(r.ctx, ada); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	adaS.expectClose()

	// A IS TOLD, BEFORE B EXISTS. This is the line `main` fails.
	marker := r.commit(group, theo, "marker")
	f := deviceA.next()
	re, ok := f.(wire.ServerMessageFrame)
	switch {
	case !ok:
		t.Fatalf("device A's next frame after the offboard = %T %+v, want its own message re-emitted", f, f)
	case string(re.Message.ID) == marker.ID.String():
		t.Fatalf("device A's next frame after the offboard is the marker: the message it holds as `read` " +
			"was never re-emitted, so it keeps a rung the server no longer backs until it bootstraps — " +
			"and a second device enrolled now would bootstrap `sent` and disagree with it")
	case re.Message.ID != m.ID:
		t.Fatalf("device A's next frame = %+v, want seq %d re-emitted", re.Message, m.Seq)
	}
	wantRung(t, "device A after the offboard", re.Message, 1, wire.DeliveryStateSent)
	if next := deviceA.message(); string(next.ID) != marker.ID.String() {
		t.Errorf("device A's frame after the re-emission = %+v, want the marker", next)
	}

	// B ENROLLS NOW AND BOOTSTRAPS. What it is served is what A was just told.
	deviceB := r.enroll(theo, "laptop")
	page := r.syncPage(string(deviceB.AccessToken))
	var served *wire.Message
	for i := range page.Messages {
		if page.Messages[i].ID == m.ID {
			served = &page.Messages[i]
		}
	}
	if served == nil {
		t.Fatalf("device B's bootstrap did not carry seq %d", m.Seq)
	}
	wantRung(t, "device B's bootstrap", *served, 1, wire.DeliveryStateSent)
	if served.State != re.Message.State || *served.ReadBy != *re.Message.ReadBy {
		t.Errorf("one account, two answers: device A holds %s %d, device B holds %s %d",
			re.Message.State, *re.Message.ReadBy, served.State, *served.ReadBy)
	}
}

// THE OFFLINE WINDOW, AS RULING 1 OPTION A ACCEPTED IT. A device detached for
// the offboard is not told by its catch-up — `/sync?after=cursor` carries the
// room's new count and no message, because a catch-up never re-serves a held
// message — and it holds `read` until it bootstraps. Not a defect of this
// ticket; the record that the window is real, reachable, and known. Invert
// this test if a later ticket closes it.
func TestADeviceOfflineForTheOffboardKeepsItsRungUntilItBootstraps(t *testing.T) {
	r := newRig(t)
	runRevocations(r)
	ada := person(r, "ada@example.com", "Ada Lovelace")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	cred := r.enroll(theo, "phone")
	phone := openAt(t, r.ctx, r.base, cred, theo)
	adaS := r.open(ada, "tablet")

	m := sendAndAwaitOwn(t, phone, group, "hello")
	adaS.message()
	adaS.send(wire.ClientRead{ConversationID: wire.Uuid(group.String()), UpToSeq: wire.Seq(m.Seq)})
	readsReceiptAndReEmissions(t, phone, m.Seq)
	drainReceiptAndReEmissions(t, adaS, 0)

	// The phone is caught up to head and goes away holding `read`.
	cursor := r.head()
	phone.close()

	if _, err := r.st.DeactivateUser(r.ctx, ada); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	adaS.expectClose()

	// ITS CATCH-UP: the room at member_count 1, and no message. The held
	// `read` is not repaired by this page, and there is nothing else a
	// reconnecting client runs.
	page := r.syncAfter(string(cred.AccessToken), cursor)
	if len(page.Messages) != 0 {
		t.Fatalf("the catch-up carried %d message(s); if a page can now re-serve a held message, ruling 1's "+
			"offline window has been closed and this test should be inverted", len(page.Messages))
	}
	var room *wire.Conversation
	for i := range page.Conversations {
		if string(page.Conversations[i].ID) == group.String() {
			room = &page.Conversations[i]
		}
	}
	if room == nil || room.MemberCount != 1 {
		t.Errorf("the catch-up's conversation = %+v, want the room at member_count 1", room)
	}

	// ITS BOOTSTRAP IS WHERE IT LEARNS.
	fresh := r.syncPage(string(cred.AccessToken))
	for _, served := range fresh.Messages {
		if served.ID == m.ID {
			wantRung(t, "the phone's bootstrap", served, 1, wire.DeliveryStateSent)
		}
	}
}
