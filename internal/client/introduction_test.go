package client

// CANT-171: CANT-103's rules 1–4 in the reference client, numbered as CANT-35's
// record and the TypeScript client number them. These are the Go twins of
// web/src/transport/test/journal.test.ts's "criterion 4" (obligation3) and
// "criterion 6" tests and its ruling 4 → B receipt tests, against a stub server
// whose socket the test writes frames to and whose /sync it answers page by
// page — so a page can be held in flight while a frame lands beside it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

// introRig is one stub server and one client over it.
type introRig struct {
	t      *testing.T
	c      *Client
	frames chan []byte

	mu          sync.Mutex
	answer      func(after int64) wire.SyncResponse
	requests    []int64
	inFlight    int
	maxInFlight int
	// hold, when set, is consulted before answer: a request it returns a
	// channel for waits on that channel before it is answered.
	hold func(after int64) chan struct{}
}

func newIntroRig(t *testing.T, faults Faults, self wire.Uuid, answer func(after int64) wire.SyncResponse) *introRig {
	t.Helper()
	r := &introRig{t: t, frames: make(chan []byte, 16), answer: answer}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/sync":
			r.sync(w, req)
		case "/ws":
			r.ws(w, req)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)

	j := NewJournal()
	if err := j.Enroll(Credential{
		DeviceID: wire.Uuid(uuid.NewString()), UserID: self, AccessToken: "token", RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{
		BaseURL: srv.URL, Journal: j, Faults: faults,
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(context.Background()) }()
	t.Cleanup(func() { c.Kill(); <-done })
	return r
}

func (r *introRig) sync(w http.ResponseWriter, req *http.Request) {
	after, err := strconv.ParseInt(req.URL.Query().Get("after"), 10, 64)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.requests = append(r.requests, after)
	r.inFlight++
	r.maxInFlight = max(r.maxInFlight, r.inFlight)
	var wait chan struct{}
	if r.hold != nil {
		wait = r.hold(after)
	}
	answer := r.answer
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}()
	if wait != nil {
		select {
		case <-wait:
		case <-req.Context().Done():
			return
		}
	}
	p := answer(after)
	p.ServerTime = "2026-01-01T00:00:00.000Z"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func (r *introRig) ws(w http.ResponseWriter, req *http.Request) {
	conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := req.Context()
	if _, _, err := conn.Read(ctx); err != nil { // the hello
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, r.ready()); err != nil {
		return
	}
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil { // pongs, pings: discarded
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-r.frames:
			if err := conn.Write(ctx, websocket.MessageText, f); err != nil {
				return
			}
		}
	}
}

// ready announces the head every test here bootstraps to (CANT-179). The shared
// readyFrame announces 0, and a head behind the cursor is obligation 4's wipe: if
// the `ready` lands after the bootstrap page, the client wipes that page and
// catches up again from 0. The clean client pays one more page for that, but
// under IgnoreRetrigger the wipe's trigger falls in the tail of the pass that
// fetched the page and is dropped, so the control is left wiped at cursor 0 and
// never gets as far as the resync it exists to fail on. A server at head 3 does
// not claim to be behind a client it has just served log_seq 3, and which of
// the page and the `ready` lands first then changes nothing.
func (r *introRig) ready() []byte {
	r.t.Helper()
	var f wire.ServerReady
	if err := json.Unmarshal(readyFrame(r.t), &f); err != nil {
		r.t.Fatal(err)
	}
	f.LogSeq = bootstrap().LogSeq
	b, err := json.Marshal(f)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// frame sends one server frame, after checking the generated decoder accepts
// it: a frame the client refuses would pass these tests for the wrong reason.
func (r *introRig) frame(v any) {
	r.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		r.t.Fatal(err)
	}
	if f, err := wire.DecodeServerFrame(b); err != nil || f == nil {
		r.t.Fatalf("the test's own frame is not one the decoder accepts: %v: %s", err, b)
	}
	r.frames <- b
}

// settle is a round trip on the socket: the server pings, and the pong is
// counted only after every frame before the ping has been handled, because
// session handles frames one at a time in order.
func (r *introRig) settle() {
	r.t.Helper()
	before := r.c.Status().PongsReceived
	r.frame(wire.Pong{ID: "settle-" + uuid.NewString()})
	// Polled rather than awaited: a pong changes a counter and notifies nobody.
	for deadline := time.Now().Add(10 * time.Second); r.c.Status().PongsReceived <= before; {
		if time.Now().After(deadline) {
			r.t.Fatalf("the settle frame was never handled; status %+v", r.c.Status())
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *introRig) await(what string, pred func(Status) bool) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.c.Await(ctx, func() bool { return pred(r.c.Status()) }); err != nil {
		r.t.Fatalf("waiting for %s: %v; status %+v", what, err, r.c.Status())
	}
}

// awaitIdle waits for the catch-up to be between passes: no trigger outstanding,
// and the pass that answered the last one returned. Both are read under the one
// lock that writes them, so no pass can be half-way through its epilogue here.
// Polled, because a pass returning changes nothing Await watches.
func (r *introRig) awaitIdle() {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		r.c.mu.Lock()
		idle := !r.c.inPass && r.c.gen == r.c.doneGen
		r.c.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the catch-up never went idle; status %+v", r.c.Status())
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *introRig) gen() uint64 {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.c.gen
}

func (r *introRig) syncRequests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// --- fixtures ---------------------------------------------------------------------

func id(n int) wire.Uuid { return wire.Uuid(fmt.Sprintf("00000000-0000-4000-8000-%012d", n)) }

var (
	roomA = id(101)
	ada   = id(201)
	theo  = id(202)
)

func conversationOf(cid wire.Uuid, name string) wire.Conversation {
	return wire.Conversation{ID: cid, Kind: wire.ConversationKindGroup, Name: name, MemberCount: 2, HeadSeq: 1}
}

func userOf(uid wire.Uuid, name string) wire.User { return wire.User{ID: uid, Name: name} }

func messageAt(logSeq int64, cid, author wire.Uuid, seq int64) wire.Message {
	return wire.Message{
		ID: id(1000 + int(logSeq)), Seq: wire.Seq(seq), LogSeq: wire.LogSeq(logSeq),
		ConversationID: cid, AuthorID: author, At: "2026-01-01T00:00:00.000Z", State: wire.DeliveryStateSent,
	}
}

// bootstrap is the first page: room A, its two members, and messages 1–3.
func bootstrap() wire.SyncResponse {
	return wire.SyncResponse{
		LogSeq:        3,
		Messages:      []wire.Message{messageAt(1, roomA, ada, 1), messageAt(2, roomA, ada, 2), messageAt(3, roomA, ada, 3)},
		Conversations: []wire.Conversation{conversationOf(roomA, "A")},
		Users:         []wire.User{userOf(ada, "Ada"), userOf(theo, "Theo")},
	}
}

func emptyAt(logSeq int64) wire.SyncResponse {
	return wire.SyncResponse{LogSeq: wire.LogSeq(logSeq), Messages: []wire.Message{}, Conversations: []wire.Conversation{}, Users: []wire.User{}}
}

func (r *introRig) holdsCount(mid wire.Uuid) (held, counted int) {
	s := r.c.Snapshot()
	for _, m := range s.Messages {
		if m.ID == mid {
			held++
		}
	}
	for _, c := range s.Counted {
		if c == mid {
			counted++
		}
	}
	return held, counted
}

// --- rules 1–3: discard, trigger, re-arm ----------------------------------------------

type rearmResult struct {
	discards, held, counted, maxInFlight int
	caughtUp                             bool
}

// rearm is obligation3 in journal.test.ts. A catch-up page is in flight,
// issued before the trigger and bounded at log_seq 4. A new room's first
// message (log_seq 5) arrives live, naming a conversation the client does not
// hold: it is discarded and pulls a trigger. Only a catch-up that re-arms its
// end condition asks again once the held page lands, and finds the message.
func rearm(t *testing.T, faults Faults) rearmResult {
	t.Helper()
	roomB := id(102)
	newRoom := messageAt(5, roomB, ada, 1)
	release := make(chan struct{})
	var armed bool
	r := newIntroRig(t, faults, theo, func(after int64) wire.SyncResponse {
		switch after {
		case 0:
			return bootstrap()
		case 3:
			return emptyAt(3)
		case 4:
			// Asked again after the trigger: the new room, introduced on its page.
			p := emptyAt(5)
			p.Messages = []wire.Message{newRoom}
			p.Conversations = []wire.Conversation{conversationOf(roomB, "B")}
			return p
		default:
			return emptyAt(after)
		}
	})
	r.await("ready and caught up", func(s Status) bool { return s.Ready && s.CaughtUp && s.Cursor == 3 })
	// AND THE PASS THAT CAUGHT UP HAS RETURNED (CANT-179). CaughtUp is set on the
	// last page, inside the pass, and the pass runs on until catchUp returns. A
	// resync landing in that tail is a trigger pulled while a pass is running,
	// which IgnoreRetrigger drops outright, so under load the control could fail
	// before the scenario began and never ask for the held page below. What is
	// under test is a trigger arriving while a page it did not issue is in
	// flight, and that page is the one this rig holds.
	r.awaitIdle()

	// The next request from the cursor is held, and answered with message 4
	// only — a page bounded below the new room's message.
	inFlight := make(chan struct{})
	r.mu.Lock()
	r.hold = func(after int64) chan struct{} {
		if after != 3 || armed {
			return nil
		}
		armed = true
		close(inFlight)
		return release
	}
	r.answer = func(prev func(int64) wire.SyncResponse) func(int64) wire.SyncResponse {
		first := true
		return func(after int64) wire.SyncResponse {
			if after == 3 && first {
				first = false
				p := emptyAt(4)
				p.Messages = []wire.Message{messageAt(4, roomA, ada, 4)}
				return p
			}
			return prev(after)
		}
	}(r.answer)
	r.mu.Unlock()

	r.frame(wire.ServerResyncRequired{Reason: wire.ResyncReasonCursorTooOld, LogSeq: 4})
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("the resync never sent its /sync")
	}
	r.frame(wire.ServerMessageFrame{Message: newRoom})
	r.await("the discard", func(s Status) bool { return s.IntroductionDiscards == 1 })
	if held, _ := r.holdsCount(newRoom.ID); held != 0 {
		t.Fatal("the message naming an unheld conversation was held, not discarded")
	}
	if s := r.c.Status(); s.Cursor != 3 {
		t.Fatalf("the discard moved the cursor to %d", s.Cursor)
	}
	close(release)
	r.await("the catch-up to end", func(s Status) bool { return s.CaughtUp && s.Cursor >= 4 })

	res := rearmResult{discards: r.c.Status().IntroductionDiscards, caughtUp: r.c.Status().CaughtUp}
	res.held, res.counted = r.holdsCount(newRoom.ID)
	r.mu.Lock()
	res.maxInFlight = r.maxInFlight
	r.mu.Unlock()
	return res
}

func TestADiscardMidPageReArmsTheCatchUp(t *testing.T) {
	clean := rearm(t, Faults{})
	if clean.discards != 1 {
		t.Errorf("introduction discards = %d, want 1", clean.discards)
	}
	if clean.held != 1 || clean.counted != 1 {
		t.Errorf("the discarded message is held %d times and counted %d, want it fetched and counted exactly once", clean.held, clean.counted)
	}
	if !clean.caughtUp {
		t.Error("not caught up at the end")
	}
	if clean.maxInFlight != 1 {
		t.Errorf("%d /sync requests in flight at once, want one catch-up at a time", clean.maxInFlight)
	}
}

// The negative controls: each fault ends the catch-up on the held page, which
// is bounded below the discarded message, so the message is never returned.
func TestTheReArmHasTeeth(t *testing.T) {
	for name, f := range map[string]Faults{
		"IgnoreRetrigger": {IgnoreRetrigger: true},
		"EndCatchUpEarly": {EndCatchUpEarly: true},
	} {
		t.Run(name, func(t *testing.T) {
			if broken := rearm(t, f); broken.held != 0 {
				t.Errorf("held %d with %s set; the fault should have lost the discarded message", broken.held, name)
			}
		})
	}
}

// --- rules 4 and 1: introductions apply by id; an unheld author is discarded --------

func TestIntroductionsApplyByIDAndAnUnheldAuthorIsFetched(t *testing.T) {
	stranger := id(250)
	byStranger := messageAt(4, roomA, stranger, 4)
	var committed atomic.Bool
	r := newIntroRig(t, Faults{}, theo, func(after int64) wire.SyncResponse {
		if after == 0 {
			return bootstrap()
		}
		if !committed.Load() {
			return emptyAt(3)
		}
		p := emptyAt(4)
		p.Messages = []wire.Message{byStranger}
		p.Users = []wire.User{userOf(stranger, "Stranger")}
		return p
	})
	r.await("ready and caught up", func(s Status) bool { return s.Ready && s.CaughtUp && s.Cursor == 3 })

	roomC, once := id(103), id(260)
	gen := r.gen()
	r.frame(wire.ServerConversationFrame{Conversation: conversationOf(roomC, "first")})
	r.frame(wire.ServerConversationFrame{Conversation: conversationOf(roomC, "second")})
	r.frame(wire.ServerUserFrame{User: userOf(once, "Once")})
	r.frame(wire.ServerUserFrame{User: userOf(once, "Twice")})
	r.settle()

	snap := r.c.Snapshot()
	var convs, users []string
	for _, c := range snap.Conversations {
		if c.ID == roomC {
			convs = append(convs, c.Name)
		}
	}
	for _, u := range snap.Users {
		if u.ID == once {
			users = append(users, u.Name)
		}
	}
	if len(convs) != 1 || convs[0] != "second" || len(users) != 1 || users[0] != "Twice" {
		t.Errorf("conversation %v, user %v; want one of each, the later record replacing the earlier", convs, users)
	}
	if snap.Cursor != 3 {
		t.Errorf("introductions moved the cursor to %d", snap.Cursor)
	}
	if r.gen() != gen {
		t.Error("an introduction pulled a trigger; it is applied, and nothing else")
	}
	if s := r.c.Status(); s.LiveFrames != 0 || s.IntroductionDiscards != 0 {
		t.Errorf("live frames %d, discards %d; an introduction is neither", s.LiveFrames, s.IntroductionDiscards)
	}

	requests := r.syncRequests()
	committed.Store(true)
	r.frame(wire.ServerMessageFrame{Message: byStranger})
	r.await("the unheld author's message, fetched", func(Status) bool { return r.c.Holds(byStranger.ID) })
	if s := r.c.Status(); s.IntroductionDiscards != 1 || s.LiveFrames != 0 {
		t.Errorf("discards %d, live frames %d; want the one discard and no live apply", s.IntroductionDiscards, s.LiveFrames)
	}
	if r.syncRequests() <= requests {
		t.Error("the discard pulled no catch-up")
	}
	if held, counted := r.holdsCount(byStranger.ID); held != 1 || counted != 1 {
		t.Errorf("held %d, counted %d; want exactly once", held, counted)
	}

	// And a message for a conversation and an author the journal does hold is
	// applied live, as it always was.
	mine := messageAt(5, roomA, ada, 5)
	r.frame(wire.ServerMessageFrame{Message: mine})
	r.await("a held conversation's message, live", func(s Status) bool { return s.LiveFrames == 1 && r.c.Holds(mine.ID) })
	if s := r.c.Status(); s.IntroductionDiscards != 1 || s.Cursor != 4 {
		t.Errorf("discards %d, cursor %d; the live apply discarded nothing and moved no cursor", s.IntroductionDiscards, s.Cursor)
	}
}

// --- CANT-35 ruling 4 → B: receipts -----------------------------------------------------

func TestOnlyAnOwnReceiptIsATrigger(t *testing.T) {
	r := newIntroRig(t, Faults{}, theo, func(after int64) wire.SyncResponse {
		if after == 0 {
			return bootstrap()
		}
		return emptyAt(3)
	})
	r.await("ready and caught up", func(s Status) bool { return s.Ready && s.CaughtUp && s.Cursor == 3 })
	before := r.c.Snapshot()

	gen := r.gen()
	r.frame(wire.ServerReceipt{ConversationID: roomA, UserID: ada, UpToSeq: 3})
	r.settle()
	if r.gen() != gen {
		t.Error("another user's receipt pulled a trigger")
	}
	if after := r.c.Snapshot(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("another user's receipt changed the store")
	}

	pages := r.c.Status().Pages
	r.frame(wire.ServerReceipt{ConversationID: roomA, UserID: theo, UpToSeq: 3})
	r.await("the own receipt's catch-up", func(s Status) bool { return s.Pages > pages && s.CaughtUp })
	if r.gen() == gen {
		t.Error("this user's own receipt pulled no trigger")
	}
}

// A pair built by hand has no UserID, and knows no self: every receipt is then
// a no-op, rather than an empty user_id matching an empty self.
func TestAReceiptIsInertWithoutAUser(t *testing.T) {
	r := newIntroRig(t, Faults{}, "", func(after int64) wire.SyncResponse {
		if after == 0 {
			return bootstrap()
		}
		return emptyAt(3)
	})
	r.await("ready and caught up", func(s Status) bool { return s.Ready && s.CaughtUp })
	gen := r.gen()
	r.frame(wire.ServerReceipt{ConversationID: roomA, UserID: theo, UpToSeq: 3})
	r.settle()
	if r.gen() != gen {
		t.Error("a receipt pulled a trigger on a client that knows no self")
	}
}

// A rotation keeps who the device speaks as: a refresh response carries no
// user, so an empty UserID inherits the held one, and a different one is
// refused.
func TestARotationKeepsTheUser(t *testing.T) {
	j := NewJournal()
	dev := wire.Uuid(uuid.NewString())
	if err := j.Enroll(Credential{DeviceID: dev, UserID: theo, AccessToken: "a1", RefreshToken: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Rotate(Credential{DeviceID: dev, AccessToken: "a2", RefreshToken: "r2"}); err != nil {
		t.Fatal(err)
	}
	if cred, _ := j.Credential(); cred.UserID != theo {
		t.Errorf("after a rotation the user is %q, want %q", cred.UserID, theo)
	}
	if err := j.Rotate(Credential{DeviceID: dev, UserID: ada, AccessToken: "a3", RefreshToken: "r3"}); err == nil {
		t.Error("a rotation to a different user was accepted")
	}
	cred, err := CredentialFromEnroll(wire.EnrollResponse{
		UserID: theo, DeviceID: dev, AccessToken: "a", RefreshToken: "r",
		AccessExpiresAt: "2026-01-01T00:00:00.000Z", RefreshExpiresAt: "2026-01-01T00:00:00.000Z",
	})
	if err != nil || cred.UserID != theo {
		t.Errorf("CredentialFromEnroll = %q, %v; want the enrollment's user", cred.UserID, err)
	}
}
