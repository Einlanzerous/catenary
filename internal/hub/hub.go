// Package hub is the in-process fan-out between store.Listener and the
// sockets: it holds every attached session on this instance, turns a NOTIFY
// into a per-viewer `message` frame, answers a listener gap with
// `resync_required`, carries `read` and `typing` across sockets, and drains
// the sessions on shutdown. CANT-107, a `full` sub-task of CANT-22.
//
// ONE RULE DECIDES EVERY FAILURE PATH: a frame is never silently dropped.
// Under CANT-24 ruling 5 the client's cursor moves only on a /sync page, and a
// catch-up runs only on a trigger — a reconnect, `ready`, `resync_required` —
// so a `message` frame this package decided not to write would be a message
// the client does not have and will not ask for. Every path here therefore
// ends in one of three ways: the frame is on the session's outbox, in order;
// the session has been told to resync; or the session has been closed so that
// the client reconnects and catches up. The process stopping is the only
// exception, and it is not a branch of delivery at all.
//
// THE HUB HOLDS NO CURSOR AND DEDUPES NOTHING. A CANT-92 re-emission is a
// `message` frame with an old log_seq and it goes through like any other.
//
// MEMBERSHIP IS READ, NEVER CACHED. Sessions are indexed by user; who is in a
// room is read from the store on every fan-out, in the same snapshot as the
// message, because nothing produces `membership_changed` yet (CANT-75) and a
// cached index would keep serving a departed member.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
	"github.com/magos/catenary/internal/wireview"
)

const (
	// outboxBound is how many frames one session may have queued before it
	// is severed as a slow consumer. A peer that is reading never approaches
	// it; a peer that is not will not read a close frame either.
	outboxBound = 256

	// readNotifyCap bounds how many of ONE AUTHOR's own messages CANT-92's
	// receipt re-emission refreshes live, per receipt, newest first — a
	// quarter of outboxBound, and that ratio is the point rather than the
	// number.
	//
	// THE CAP IS BOUNDED FROM BELOW BY outboxBound, NOT CHOSEN FREELY. A
	// first read of a room, or a new member's first receipt, can move
	// read_seq from 0 to last_seq in one call — a 10,000-message room is a
	// 10,000-message span on one receipt. Left uncapped, that burst competes
	// for the same outbox ordinary live traffic shares and can fill it,
	// severing the very author it exists to refresh (ruling 0 above). That
	// author reconnects and catches up over a full /sync bootstrap, which
	// serves the identical read_by a live re-emission would have — a plain
	// page always knows the current count (CANT-26) — so a sever here buys
	// the cap nothing it did not already have another way, at the cost of a
	// disconnect for every session that author held open. A quarter leaves
	// three-quarters of the outbox for the traffic that sever would have
	// disrupted; a different fraction is fine if the reason is written down
	// beside it, which is what this comment is doing for 64.
	//
	// THE REMAINDER OF A SPAN BEYOND THE CAP WAITS FOR THAT BOOTSTRAP. CANT-89
	// established that an ordinary /sync page never re-serves a message it
	// already carried — it pages on log_seq, and a served message is never on
	// a later page — so nothing short of a full bootstrap (cursor 0) closes
	// the gap for the tail this cap leaves live. DeliveryState's schema
	// description states the same guarantee on the wire, beside the catch-up
	// sentence CANT-89 put there.
	readNotifyCap = 64

	// loadBudget bounds the retry of a fan-out load; headBudget bounds the
	// head read at gap. The listener has usually just reconnected when the
	// head read runs, so the pool may still be behind it, and that gets the
	// longer of the two.
	loadBudget = 3 * time.Second
	headBudget = 5 * time.Second

	// The backoff between retry attempts. 100 ms doubling to 1 s makes a
	// spent budget a handful of tries rather than a spin.
	retryInitial = 100 * time.Millisecond
	retryMax     = time.Second

	// membersTimeout bounds the member read a detach may need to emit an
	// updated typing list; detach has no caller context to inherit.
	membersTimeout = 2 * time.Second
)

// Conn is what the hub needs of a session. api.Session satisfies it through a
// wrapper at the composition root (its UserID and DeviceID are exported
// FIELDS, and Go refuses a method of the same name on the same type). An
// interface rather than *api.Session so this package's tests run against an
// in-memory peer that can be made to wedge.
type Conn interface {
	SessionID() uuid.UUID
	UserID() uuid.UUID
	DeviceID() uuid.UUID
	// Send writes one frame; safe from any goroutine.
	Send(ctx context.Context, f wire.ServerFrame) error
	// Close performs the close handshake. For a peer that is alive and a
	// status that is the point: the drain (1001), head-unreadable (1012).
	Close(status websocket.StatusCode, reason string) error
	// CloseNow drops the connection with no handshake. For the slow-consumer
	// sever only: the peer is not reading, and a handshake would wait on the
	// write lock its stalled write holds.
	CloseNow() error
}

// Store is the reads and writes the hub makes. *store.Store satisfies it; the
// hub's tests use a fake.
type Store interface {
	MessageForFanout(ctx context.Context, conv uuid.UUID, seq int64) (store.FanoutMessage, error)
	Members(ctx context.Context, conv uuid.UUID) ([]uuid.UUID, error)
	MarkRead(ctx context.Context, conv, user uuid.UUID, upToSeq int64) (store.ReadReceipt, error)
	Head(ctx context.Context) (int64, error)
	// MessagesForReadNotify is CANT-92's read behind the receipt re-emission
	// — see OnNotify's receipt branch and MessagesForReadNotify's own
	// comment for the cap and the author exclusion.
	MessagesForReadNotify(ctx context.Context, conv, reader uuid.UUID, before, after int64, capPerAuthor int) ([]store.ReadNotifyMessage, error)
}

// Hub is one per process.
type Hub struct {
	st       Store
	logger   *slog.Logger
	mediaURL func(storageKey string) string

	// ctx outlives every session's outbox; Shutdown cancels it last, after
	// the drain, so a close frame can still be written.
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// sessions by user: the one index. A user's devices are the inner set.
	sessions map[uuid.UUID]map[*session]struct{}
	count    int
	// typing entries are KEYED BY SESSION (ruling 2): a session's `stop` or
	// detach removes its own entry and nothing else's, and the wire list is
	// derived from what remains.
	typing  map[uuid.UUID]map[*session]time.Time
	closing bool
	drained chan struct{}
}

// session is one attached Conn and its outbox.
type session struct {
	conn Conn
	out  chan wire.ServerFrame
	// outClosed is written under Hub.mu, as every enqueue is: once the
	// channel is closed nothing can reach it because the session is no
	// longer in the index.
	outClosed bool
	// dead is set by the outbox goroutine when a write fails. The connection
	// is gone; anything still queued has nowhere to go, and the read loop's
	// detach is on its way.
	dead bool
}

// New builds a hub. mediaURL is required, as wireview.Viewer.MediaURL is:
// the composition root passes the same deriver /sync uses, so the socket's
// Message and /sync's cannot derive a URL differently.
func New(st Store, logger *slog.Logger, mediaURL func(string) string) *Hub {
	if mediaURL == nil {
		panic("hub: mediaURL is required — pass the deriver /sync uses")
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		st: st, logger: logger, mediaURL: mediaURL,
		ctx: ctx, cancel: cancel,
		sessions: map[uuid.UUID]map[*session]struct{}{},
		typing:   map[uuid.UUID]map[*session]time.Time{},
		drained:  make(chan struct{}),
	}
}

// Attached is how many sessions this instance holds.
func (h *Hub) Attached() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Attach registers a session and starts its outbox. The door calls it before
// `ready` is written; the returned detach runs when the session ends.
//
// After Shutdown has begun, a session that races the drain is closed 1001 at
// once and never indexed: it must not sit attached to a hub that will never
// deliver to it.
func (h *Hub) Attach(c Conn) (detach func()) {
	s := &session{conn: c, out: make(chan wire.ServerFrame, outboxBound)}
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		go func() { _ = c.Close(websocket.StatusGoingAway, "going away") }()
		return func() {}
	}
	set := h.sessions[c.UserID()]
	if set == nil {
		set = map[*session]struct{}{}
		h.sessions[c.UserID()] = set
	}
	set[s] = struct{}{}
	h.count++
	n := h.count
	h.mu.Unlock()

	go s.run(h)
	h.logger.Debug("session attached", h.attrs(s, "sessions", n)...)
	return func() { h.detach(s) }
}

// run is the outbox: one goroutine per session, draining the queue into the
// connection in order. A failed write marks the session dead and stops;
// whatever remains queued has nowhere to go.
func (s *session) run(h *Hub) {
	for f := range s.out {
		if s.dead {
			continue
		}
		if err := s.conn.Send(h.ctx, f); err != nil {
			s.dead = true
		}
	}
}

// detach removes a session. Idempotent: a session severed as a slow consumer
// is detached again when its read loop ends.
func (h *Hub) detach(s *session) {
	h.mu.Lock()
	removed := h.removeLocked(s)
	// Typing entries are dropped HERE and not at a sever: the sever runs
	// under the enqueuer's lock and must not turn around and emit, and the
	// read loop's detach follows a sever promptly anyway.
	changed := h.dropTypingLocked(s)
	n := h.count
	h.mu.Unlock()
	if removed {
		h.logger.Debug("session detached", h.attrs(s, "sessions", n)...)
	}
	for _, conv := range changed {
		h.emitTyping(conv, nil)
	}
}

// removeLocked takes a session out of the index and closes its outbox.
// Returns false if it was not there.
func (h *Hub) removeLocked(s *session) bool {
	set := h.sessions[s.conn.UserID()]
	if _, ok := set[s]; !ok {
		return false
	}
	delete(set, s)
	if len(set) == 0 {
		delete(h.sessions, s.conn.UserID())
	}
	h.count--
	if !s.outClosed {
		s.outClosed = true
		close(s.out)
	}
	if h.closing && h.count == 0 {
		close(h.drained)
	}
	return true
}

// enqueueLocked puts a frame on a session's outbox, or severs the session
// if the outbox is full (ruling 0). NEVER BLOCKS: the caller may be the
// listener goroutine, which serializes delivery for everyone on the
// instance. The sever removes the session from the index — so no later
// enqueue reaches it — and drops the connection without a handshake, in a
// goroutine, because the peer is not reading and CloseNow's own bookkeeping
// is not this goroutine's to wait on.
func (h *Hub) enqueueLocked(s *session, f wire.ServerFrame) bool {
	if s.outClosed {
		return false
	}
	select {
	case s.out <- f:
		return true
	default:
		queued := len(s.out)
		h.removeLocked(s)
		h.logger.Warn("slow consumer severed", h.attrs(s, "queued", queued)...)
		go func() { _ = s.conn.CloseNow() }()
		return false
	}
}

// OnNotify is the listener's per-notification callback. Two shapes cross the
// one channel (CANT-92's NotifyPayload) and this is where they part ways:
// IsReceipt routes a receipt notification to onReadNotify and returns; a
// message notification falls through to the fan-out below exactly as before.
//
// A message notification loads the message and every member in one snapshot,
// builds one Message per attached member through wireview, enqueues it on
// each of their sessions. In commit order, because the listener delivers in
// commit order and this enqueues in arrival order.
func (h *Hub) OnNotify(ctx context.Context, p store.NotifyPayload) {
	if p.IsReceipt() {
		h.onReadNotify(ctx, p)
		return
	}

	var fm store.FanoutMessage
	err := h.retry(ctx, loadBudget, func() error {
		var e error
		fm, e = h.st.MessageForFanout(ctx, p.ConversationID, p.Seq)
		return e
	})
	switch {
	case ctx.Err() != nil:
		// The process is stopping. Not a gap: the sessions are drained or
		// draining, and there is nobody to resync.
		h.logger.Debug("fan-out abandoned on a cancelled context",
			"conversation_id", p.ConversationID, "seq", p.Seq)
		return
	case errors.Is(err, store.ErrNotFound):
		// The row is gone, or something else NOTIFYed on the channel. There
		// is nothing to deliver; a retention purge is CANT-67's resync
		// producer.
		h.logger.Warn("notify for a missing row", "conversation_id", p.ConversationID, "seq", p.Seq)
		return
	case err != nil:
		// The message committed and this instance cannot say who it was
		// for. A skipped log line here would be a hole; a gap is the one
		// answer that closes it.
		//
		// THE VOLUME IS KNOWN AND ACCEPTED. A permanent failure — a column
		// missing after a bad migration, say — raises a gap on EVERY
		// notification, and each gap is one /sync from every attached
		// client. The transient path is floored by its budget (one gap per
		// ~3 s per instance); this one is floored by nothing but the send
		// rate. It is still right: /sync does not go through this read, so
		// the clients do catch up, and a floor here would be a window in
		// which committed messages reach nobody live and nobody is told.
		// The ERROR line per notification is what says the migration is
		// bad.
		h.logger.Error("fan-out load failed; treating as a listener gap",
			"conversation_id", p.ConversationID, "seq", p.Seq, "error", err)
		h.gap(ctx)
		return
	}

	// The reply source is scoped to this conversation by the store's query
	// (a membership guard), and the same-thread rule is checked here on the
	// same field wireview.Sync checks, so a source from elsewhere is no ref.
	var src *store.ReplySource
	if fm.ReplySource != nil && fm.ReplySource.ConversationID == fm.Message.ConversationID {
		src = fm.ReplySource
	}

	h.mu.Lock()
	n := 0
	for _, mem := range fm.Members {
		set := h.sessions[mem.UserID]
		if len(set) == 0 {
			continue
		}
		// ONE Message per member, not per session: state, read_by and the
		// echoed client_id are the reader's, and a reader's devices are the
		// same reader.
		readBy := fm.ReadBy
		frame := wire.ServerMessageFrame{Message: wireview.Message(fm.Message, fm.Attachments, src, wireview.Viewer{
			UserID:   mem.UserID,
			State:    wireview.DeliveryState(fm.Message, mem.UserID, mem.ReadSeq, fm.ReadBy),
			ReadBy:   &readBy,
			MediaURL: h.mediaURL,
		})}
		for s := range set {
			if h.enqueueLocked(s, frame) {
				n++
			}
		}
	}
	h.mu.Unlock()
	h.logger.Debug("fan-out",
		"conversation_id", fm.Message.ConversationID, "seq", fm.Message.Seq,
		"log_seq", fm.Message.LogSeq, "sessions", n)
}

// onReadNotify is CANT-92's re-emission: a MarkRead receipt that advanced the
// mark notifies (conversation_id, user_id, before, after) on the same channel
// a message notification uses (NotifyPayload widened rather than a second
// shape — CANT-92 decision 2), and this is where a listening instance re-reads
// the span to find which of its own attached authors need their Message
// refreshed.
//
// ONLY THE AUTHOR EVER RECEIVES ONE (CANT-90 ruling 2: the server derives the
// word and the count, and only the author's own view of a message renders
// either). MessagesForReadNotify already excludes the reader's own messages
// from the span — a reader's own read_by never changes when their own read_seq
// passes their own message, because readByExpr counts the author by identity
// regardless — so every row this returns belongs to a DIFFERENT member, and
// each gets exactly ONE Message, from their own point of view, the same way
// OnNotify builds one per viewer above.
//
// THE HUB DEDUPES NOTHING (package header). A re-emission is an ordinary
// `message` frame carrying the row's original log_seq — unchanged by a
// receipt, which never rewrites a message — and it passes through
// enqueueLocked exactly like a first delivery, cap included: an author with
// more attached devices than one gets the same frame on each, and a slow one
// among them is severed on the same terms TestASlowConsumerIsSeveredWithout
// BlockingTheEnqueuer proves for a first delivery.
func (h *Hub) onReadNotify(ctx context.Context, p store.NotifyPayload) {
	reader := *p.UserID
	var msgs []store.ReadNotifyMessage
	err := h.retry(ctx, loadBudget, func() error {
		var e error
		msgs, e = h.st.MessagesForReadNotify(ctx, p.ConversationID, reader, p.Before, p.After, readNotifyCap)
		return e
	})
	switch {
	case ctx.Err() != nil:
		// The process is stopping, on OnNotify's own reasoning: the sessions
		// are drained or draining, and there is nobody left to refresh.
		h.logger.Debug("read notify abandoned on a cancelled context",
			"conversation_id", p.ConversationID, "user_id", reader)
		return
	case err != nil:
		// NOT A GAP, AND DELIBERATELY SO — unlike OnNotify's load failure,
		// h.gap(ctx) would not fix this. A gap tells every attached session
		// to resync from ITS OWN cursor over /sync, and CANT-89 established
		// that a page never re-serves a message already served on an earlier
		// one: every message in this span was served before this receipt
		// existed, so an ordinary catch-up will never re-fetch it regardless
		// of how many sessions are told to run one. The only thing that would
		// is a full bootstrap, which nothing here can force. So a failure
		// here costs exactly what the cap's own remainder costs on purpose —
		// the affected authors see the stale read_by until they bootstrap
		// fresh — and it is logged loud, at ERROR, because that cost should
		// be rare rather than silent.
		h.logger.Error("read notify fan-out failed",
			"conversation_id", p.ConversationID, "user_id", reader, "error", err)
		return
	}

	h.mu.Lock()
	n := 0
	for _, rm := range msgs {
		set := h.sessions[rm.Message.AuthorID]
		if len(set) == 0 {
			continue
		}
		// Viewer is always the row's own author: MessagesForReadNotify
		// already excluded every other case, so DeliveryState's own-message
		// branch is the only one this ever reaches and viewerReadSeq is
		// unused on that branch.
		readBy := rm.ReadBy
		frame := wire.ServerMessageFrame{Message: wireview.Message(rm.Message, rm.Attachments, rm.ReplySource, wireview.Viewer{
			UserID:   rm.Message.AuthorID,
			State:    wireview.DeliveryState(rm.Message, rm.Message.AuthorID, 0, rm.ReadBy),
			ReadBy:   &readBy,
			MediaURL: h.mediaURL,
		})}
		for s := range set {
			if h.enqueueLocked(s, frame) {
				n++
			}
		}
	}
	h.mu.Unlock()
	h.logger.Debug("read receipt re-emission",
		"conversation_id", p.ConversationID, "reader", reader,
		"before", p.Before, "after", p.After, "messages", len(msgs), "sessions", n)
}

// OnGap is the listener's reconnect callback: some number of notifications
// were lost with no record of how many, so every attached session is told
// to catch up over /sync from its cursor. Nothing is closed.
//
// Synchronous on the listener goroutine, as notify.go requires; it enqueues
// and returns, and never waits on anything the listener has to deliver.
func (h *Hub) OnGap(ctx context.Context) { h.gap(ctx) }

func (h *Hub) gap(ctx context.Context) {
	var head int64
	err := h.retry(ctx, headBudget, func() error {
		var e error
		head, e = h.st.Head(ctx)
		return e
	})
	if ctx.Err() != nil {
		h.logger.Debug("gap abandoned on a cancelled context")
		return
	}
	if err != nil {
		// NO FRAME EVER CARRIES AN INVENTED HEAD. Each client reconnects
		// into a hello that reads head itself. Close, not CloseNow: these
		// peers are alive and 1012 is the point; in a goroutine each, so
		// this goroutine does not wait on a handshake.
		h.mu.Lock()
		var all []*session
		for _, set := range h.sessions {
			for s := range set {
				all = append(all, s)
			}
		}
		h.mu.Unlock()
		for _, s := range all {
			go func(s *session) {
				_ = s.conn.Close(websocket.StatusServiceRestart, "head unreadable after a listener gap")
			}(s)
		}
		h.logger.Error("head unreadable at gap; sessions severed", "sessions_severed", len(all), "error", err)
		return
	}

	frame := wire.ServerResyncRequired{Reason: wire.ResyncReasonCursorTooOld, LogSeq: wire.LogSeq(head)}
	h.mu.Lock()
	n := 0
	for _, set := range h.sessions {
		for s := range set {
			if h.enqueueLocked(s, frame) {
				n++
			}
		}
	}
	h.mu.Unlock()
	h.logger.Warn("listener gap", "sessions_notified", n, "head", head)
}

// retry runs op until it succeeds, fails permanently, spends its budget, or
// the context is cancelled. IsTransient classifies context.Canceled as
// transient — rightly, for a send — so the context is checked here, first,
// and a cancellation is returned to the caller at once rather than retried
// until the budget is spent.
func (h *Hub) retry(ctx context.Context, budget time.Duration, op func() error) error {
	deadline := time.Now().Add(budget)
	backoff := retryInitial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op()
		if err == nil || ctx.Err() != nil || !store.IsTransient(err) {
			return err
		}
		if time.Now().Add(backoff).After(deadline) {
			return err
		}
		h.logger.Debug("retrying after a transient failure", "backoff", backoff, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > retryMax {
			backoff = retryMax
		}
	}
}

// Handle receives the two client frames the door does not answer itself.
// On the session's read goroutine, like `send`.
func (h *Hub) Handle(ctx context.Context, c Conn, f wire.ClientFrame) {
	switch v := f.(type) {
	case wire.ClientRead:
		h.read(ctx, c, v)
	case wire.ClientTyping:
		h.typingFrame(ctx, c, v)
	default:
		// The door names the two frames it hands over; anything else here
		// is a door change this switch did not follow.
		h.logger.Warn("frame handed to the hub that it does not handle", "type", fmt.Sprintf("%T", f))
	}
}

// read is `store.MarkRead`, then — only if the mark moved — a `receipt` on
// every attached member session, the origin included: UpToSeq is the mark
// after the clamp, and the device that claimed above head is the one that
// most needs to learn the real one.
//
// A REFUSAL PUTS NOTHING ON THE WIRE (ruling 1). The store has logged it; a
// receipt is a high-water mark the next one supersedes, and a non-member's
// client learns it is a non-member at its next catch-up.
func (h *Hub) read(ctx context.Context, c Conn, v wire.ClientRead) {
	conv := mustUUID(v.ConversationID)
	r, err := h.st.MarkRead(ctx, conv, c.UserID(), int64(v.UpToSeq))
	if errors.Is(err, store.ErrSeqOutOfRange) {
		// Unreachable behind the generated decoder (Seq minimum 1). If it
		// arrives, the frame was one the decoder should have refused, and
		// the door's rule for a malformed frame is followed: 1008.
		_ = c.Close(websocket.StatusPolicyViolation, "up_to_seq must be at least 1")
		return
	}
	if err != nil || !r.Advanced {
		return
	}
	members, err := h.st.Members(ctx, conv)
	if err != nil {
		// The mark is written. The live receipt is a nicety the next one
		// supersedes; nothing is lost that a catch-up would carry.
		h.logger.Warn("receipt fan-out skipped", h.connAttrs(c, "conversation_id", conv, "error", err)...)
		return
	}
	frame := wire.ServerReceipt{
		ConversationID: v.ConversationID,
		UserID:         wire.Uuid(c.UserID().String()),
		UpToSeq:        wire.Seq(r.UpToSeq),
	}
	h.mu.Lock()
	h.broadcastLocked(members, frame, nil)
	h.mu.Unlock()
}

// typingFrame updates this session's typing entry and emits the derived
// list to every attached member session except the originating one.
//
// A NON-MEMBER'S TYPING IS DROPPED AT WARN: a non-member must neither
// announce into a room nor learn who is attached to it.
func (h *Hub) typingFrame(ctx context.Context, c Conn, v wire.ClientTyping) {
	conv := mustUUID(v.ConversationID)
	members, err := h.st.Members(ctx, conv)
	if err != nil {
		h.logger.Warn("typing fan-out skipped", h.connAttrs(c, "conversation_id", conv, "error", err)...)
		return
	}
	if !contains(members, c.UserID()) {
		h.logger.Warn("typing from a non-member", h.connAttrs(c, "conversation_id", conv)...)
		return
	}
	h.mu.Lock()
	s := h.findLocked(c)
	if s == nil {
		h.mu.Unlock()
		return
	}
	entries := h.typing[conv]
	switch v.State {
	case wire.TypingStateStart:
		if entries == nil {
			entries = map[*session]time.Time{}
			h.typing[conv] = entries
		}
		if _, ok := entries[s]; !ok {
			entries[s] = time.Now()
		}
	case wire.TypingStateStop:
		delete(entries, s)
		if len(entries) == 0 {
			delete(h.typing, conv)
		}
	}
	frame := wire.ServerTyping{ConversationID: v.ConversationID, UserIds: h.typingListLocked(conv)}
	h.broadcastLocked(members, frame, s)
	h.mu.Unlock()
}

// emitTyping sends a conversation's derived list to its attached members,
// for a detach that removed an entry. Detach has no caller context, so the
// member read runs on the hub's own with a short bound.
func (h *Hub) emitTyping(conv uuid.UUID, except *session) {
	ctx, cancel := context.WithTimeout(h.ctx, membersTimeout)
	defer cancel()
	members, err := h.st.Members(ctx, conv)
	if err != nil {
		h.logger.Debug("typing list not emitted on detach", "conversation_id", conv, "error", err)
		return
	}
	h.mu.Lock()
	frame := wire.ServerTyping{ConversationID: wire.Uuid(conv.String()), UserIds: h.typingListLocked(conv)}
	h.broadcastLocked(members, frame, except)
	h.mu.Unlock()
}

// typingListLocked derives the wire list: a user is typing while at least
// one of their sessions has an entry, ordered by the earliest live `start`
// among them. The wire calls the order normative.
func (h *Hub) typingListLocked(conv uuid.UUID) []wire.Uuid {
	type entry struct {
		user uuid.UUID
		at   time.Time
	}
	earliest := map[uuid.UUID]time.Time{}
	for s, at := range h.typing[conv] {
		u := s.conn.UserID()
		if cur, ok := earliest[u]; !ok || at.Before(cur) {
			earliest[u] = at
		}
	}
	list := make([]entry, 0, len(earliest))
	for u, at := range earliest {
		list = append(list, entry{u, at})
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].at.Equal(list[j].at) {
			return list[i].at.Before(list[j].at)
		}
		return list[i].user.String() < list[j].user.String()
	})
	// Non-nil, so an empty list encodes as [] — the field is required.
	out := make([]wire.Uuid, 0, len(list))
	for _, e := range list {
		out = append(out, wire.Uuid(e.user.String()))
	}
	return out
}

// dropTypingLocked removes every entry a session holds and returns the
// conversations whose derived list changed.
func (h *Hub) dropTypingLocked(s *session) []uuid.UUID {
	var changed []uuid.UUID
	for conv, entries := range h.typing {
		if _, ok := entries[s]; !ok {
			continue
		}
		before := h.typingListLocked(conv)
		delete(entries, s)
		if len(entries) == 0 {
			delete(h.typing, conv)
		}
		if !sameList(before, h.typingListLocked(conv)) {
			changed = append(changed, conv)
		}
	}
	return changed
}

// broadcastLocked enqueues a frame on every attached session of every
// listed member, except one. Returns how many were enqueued.
func (h *Hub) broadcastLocked(members []uuid.UUID, f wire.ServerFrame, except *session) int {
	n := 0
	for _, u := range members {
		for s := range h.sessions[u] {
			if s == except {
				continue
			}
			if h.enqueueLocked(s, f) {
				n++
			}
		}
	}
	return n
}

// findLocked resolves a Conn to its session by id.
func (h *Hub) findLocked(c Conn) *session {
	for s := range h.sessions[c.UserID()] {
		if s.conn.SessionID() == c.SessionID() {
			return s
		}
	}
	return nil
}

// Shutdown closes every attached session with 1001 and waits until each has
// detached or ctx expires. Concurrent per session: a close handshake waits
// for the peer's echo, and the sessions are independent.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closing = true
	var all []*session
	for _, set := range h.sessions {
		for s := range set {
			all = append(all, s)
		}
	}
	if h.count == 0 {
		select {
		case <-h.drained:
		default:
			close(h.drained)
		}
	}
	h.mu.Unlock()
	h.logger.Info("draining sessions", "sessions", len(all))

	for _, s := range all {
		go func(s *session) { _ = s.conn.Close(websocket.StatusGoingAway, "going away") }(s)
	}
	var err error
	select {
	case <-h.drained:
	case <-ctx.Done():
		err = fmt.Errorf("hub: drain: %w", ctx.Err())
	}
	// Cancel the outbox context LAST, after the drain: a close frame is
	// written on the same connection an outbox may still be writing to.
	h.cancel()
	h.logger.Info("sessions drained", "drained", len(all)-h.Attached(), "timed_out", err != nil)
	return err
}

func (h *Hub) attrs(s *session, extra ...any) []any {
	return h.connAttrs(s.conn, extra...)
}

func (h *Hub) connAttrs(c Conn, extra ...any) []any {
	return append([]any{
		"session_id", c.SessionID(), "user_id", c.UserID(), "device_id", c.DeviceID(),
	}, extra...)
}

func contains(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func sameList(a, b []wire.Uuid) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mustUUID parses a wire Uuid the generated decoder has already validated;
// the zero value on failure is what the store refuses.
func mustUUID(u wire.Uuid) uuid.UUID {
	id, err := uuid.Parse(string(u))
	if err != nil {
		return uuid.Nil
	}
	return id
}
