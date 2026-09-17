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
// A CONVERSATION IS INTRODUCED ON ITS FIRST MESSAGE, STATELESSLY (CANT-114).
// `seq == 1` is a total test on the row itself — seq is dense from 1 and
// last_seq has one production writer — so this package can hand a client the
// `conversation` and `user` records before a `message` naming a conversation
// it may never have held, WITHOUT REMEMBERING WHAT ANY SESSION HAS SEEN. That
// statelessness is the whole design, and its price is that the introduction
// OVER-FIRES: a session that already holds the conversation is told again, and
// applies the record idempotently by id. The alternative — a per-session set —
// is state on the delivery path that has to be kept right on attach, detach
// and every membership change, to save a frame that costs nothing.
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

	// revokeBudget bounds the re-check a revocation gap runs. The listener has
	// usually just reconnected, so the pool may still be behind it — the same
	// reasoning headBudget gets, and the same number.
	revokeBudget = 5 * time.Second

	// attachCheckTimeout bounds the liveness re-check every attach runs, and
	// it is deliberately SHORTER than revokeBudget with no retry behind it.
	// This one is on the path of every socket that opens, so a generous bound
	// here is added latency on every connection and, worse, a queue of
	// half-open sockets waiting on a database that is already struggling. The
	// gap re-check is rare and can afford to be patient; this cannot.
	attachCheckTimeout = 2 * time.Second
)

// StatusRevoked is CANT-30's severance code: the credential behind this
// session stopped being live while the session was open.
//
// ITS OWN CODE, IN RFC 6455's PRIVATE RANGE, on the convention
// `internal/api/socket.go` states for 4000 — each distinct reason a session
// ends carries its own, so that a reader of a close-code histogram can tell
// them apart. Folding this into 1008 would work for a client and would make a
// mid-session revocation indistinguishable from a refused handshake in
// `client.Stats.CloseStatuses`, which is exactly the telemetry that would show
// revocation working.
//
// IT IS THE FIRST CODE IN THE TERMINAL BUCKET, and that is the point of it.
// CANT-35's table puts 1001, 1012, 4000 and an abnormal closure in
// "reconnect with backoff"; 1008 is "do not retry the same handshake, fix the
// credential first". This belongs with 1008: the device has been revoked, the
// door will refuse its next upgrade whatever it presents, and a client that
// reconnects on a timer is the failure CANT-31 names — "a revoked device
// reconnecting forever". A client MUST NOT treat this as a transient drop.
const StatusRevoked websocket.StatusCode = 4001

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
	// DeadDevices names those of the given devices that can no longer
	// authenticate — revoked, OR owned by a deactivated account. Named for the
	// question rather than for one of its two answers: `RevokedDevices` read
	// as `devices.revoked_at` alone, which is the exact misreading the query
	// exists to correct. Two callers: OnRevocationGap and Attach.
	DeadDevices(ctx context.Context, deviceIDs []uuid.UUID) ([]uuid.UUID, error)
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

	// THE WINDOW BETWEEN THE DOOR AND HERE, CLOSED BY CONSTRUCTION.
	//
	// OnRevocation is edge-triggered: it can only close sessions that are in
	// the index when the notification arrives. The door authenticates a device
	// and then waits up to DefaultHelloTimeout — ten seconds — for the hello,
	// with a further database round trip after it, and the session is not in
	// the index for any of that. A revocation delivered in that window walks
	// the index, finds nothing, and is gone: the notification is not queued
	// and it never comes again. The session then attaches and streams for the
	// life of the socket, which is precisely the sentence OnRevocation's own
	// comment says this feature exists to prevent. The gap re-check does not
	// save it either — that runs only when the listener RECONNECTS, which on a
	// healthy deployment may not happen for weeks.
	//
	// The window is not exotic in the case the feature is for: a device you
	// are revoking because it was lost is a device reconnecting on a mobile
	// network, and "revoke it" is the button pressed while it does so.
	//
	// ONE READ AFTER INDEXING CLOSES IT COMPLETELY, rather than narrowing it.
	// A revocation COMMITS BEFORE ITS NOTIFY — pg_notify delivers at commit,
	// which is CANT-18 ruling 2 applied to RevokeDevice — so any revocation
	// whose notification this hub could have missed is already committed, and
	// therefore visible to a read issued now. Any revocation committing after
	// this read finds the session already in the index and is handled by
	// OnRevocation. There is no third case.
	h.severIfDead(s)
	return func() { h.detach(s) }
}

// severIfDead closes a just-attached session whose credential died while the
// socket was still in the door's hands. See Attach for why it exists.
//
// A FAILED CHECK ALLOWS THE SESSION, which is the opposite of what the gap
// re-check does with its failure, and the asymmetry is deliberate. There, the
// sessions are already established and the alternative to severing is serving
// them blind for an unbounded time. Here the device was authenticated seconds
// ago — the door reads devices.revoked_at on every upgrade — so the exposure
// is one hello timeout, while refusing on a failed read would turn a momentary
// database blip into "nobody can open a socket at all". Logged at WARN so a
// persistent failure is visible rather than inferred.
func (h *Hub) severIfDead(s *session) {
	ctx, cancel := context.WithTimeout(h.ctx, attachCheckTimeout)
	defer cancel()

	dead, err := h.st.DeadDevices(ctx, []uuid.UUID{s.conn.DeviceID()})
	if err != nil {
		h.logger.Warn("attach liveness re-check failed; session allowed",
			h.attrs(s, "error", err)...)
		return
	}
	if len(dead) == 0 {
		return
	}

	h.mu.Lock()
	removed := h.removeLocked(s)
	h.mu.Unlock()
	if !removed {
		// Already gone — OnRevocation got there first, which is the benign
		// race between the two paths and needs no second close.
		return
	}
	go func() { _ = s.conn.Close(StatusRevoked, "credential revoked") }()
	h.logger.Warn("session severed at attach: its credential died in the door's hands",
		h.attrs(s)...)
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
//
// ON A CONVERSATION'S FIRST MESSAGE IT INTRODUCES THE CONVERSATION FIRST
// (CANT-114): `conversation` → `user`(s) → `message`, on each member session,
// enqueued under the one lock this fan-out already takes so nothing can come
// between a record and the message it introduces. THE GATE IS THE MESSAGE'S
// OWN ORDINAL AND THIS BRANCH ONLY — a CANT-92 receipt re-emission returns
// above and can never reach it, which is deliberate: a re-emission goes to the
// author alone, who holds the conversation by definition, and its burst of up
// to readNotifyCap frames must not carry 1 + N introduction frames with it.
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

	// The `user` records are the same for everybody, so they are mapped ONCE
	// and BEFORE THE LOCK. h.mu is held while every attached session on this
	// instance is enqueued to, on the goroutine that serialises delivery for
	// all of them; pure mapping has no business inside it.
	introduce := fm.Message.Seq == store.FirstMessageSeq
	var users []wire.ServerFrame
	if introduce {
		users = make([]wire.ServerFrame, 0, len(fm.Users))
		for _, u := range fm.Users {
			users = append(users, wire.ServerUserFrame{User: wireview.User(u)})
		}
	}

	h.mu.Lock()
	n, records := 0, 0
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

		// The conversation record is PER MEMBER — a direct's name and
		// first_unread_seq are the reader's — so it is built here, while the
		// viewer-independent user frames are shared from above.
		var intro []wire.ServerFrame
		if introduce {
			if row, ok := fm.Conversations[mem.UserID]; ok {
				intro = append(intro, wire.ServerConversationFrame{Conversation: wireview.Conversation(row)})
				intro = append(intro, users...)
			} else {
				// UNREACHABLE, AND THE MESSAGE STILL GOES. The rows were read
				// from the same snapshot and over the same member list as this
				// message, so a member without one would be the store
				// contradicting itself within one transaction.
				//
				// If it ever does happen, the frame the client is OWED is the
				// message: withholding it to protect an introduction would hold
				// back a delivered message, which is the one thing this file's
				// header forbids, and /sync still carries the conversation
				// record exactly as it did for every message before this
				// ticket. So this degrades to the old behaviour and says so,
				// loudly enough to find.
				h.logger.Warn("no conversation record for a member on a first message; delivering the message alone",
					"conversation_id", fm.Message.ConversationID, "user_id", mem.UserID)
			}
		}

		for s := range set {
			// ORDER ON THE OUTBOX IS ORDER ON THE WIRE: one queue per session,
			// drained by one goroutine, so enqueuing the record before the
			// message is what puts it in front of the message. A sever between
			// the two (a full outbox) takes the session out of the index and
			// the message with it — the session is closed, which is one of this
			// package's three endings, and the client catches up on reconnect.
			for _, f := range intro {
				if h.enqueueLocked(s, f) {
					records++
				}
			}
			if h.enqueueLocked(s, frame) {
				n++
			}
		}
	}
	h.mu.Unlock()
	h.logger.Debug("fan-out",
		"conversation_id", fm.Message.ConversationID, "seq", fm.Message.Seq,
		"log_seq", fm.Message.LogSeq, "sessions", n,
		"introduced", introduce, "records", records)
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
// MEMBERSHIP IS STILL READ, NEVER CACHED (package header), even though this
// function never calls Store.Members: MessagesForReadNotify itself excludes
// an author who is no longer a current member, in the same snapshot as
// everything else it reads, which is what the header's rule actually asks
// for. Without that a departed author's still-attached session would get a
// message frame for a room they have left — messages.author_id survives a
// departure (ON DELETE RESTRICT), so the row does not disappear with them.
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

// OnRevocation is the revocation listener's per-notification callback: a
// credential stopped being live, so every session on this instance that it
// authorized is closed at once.
//
// THIS IS WHAT "IMMEDIATELY RATHER THAN AT ITS NEXT RECONNECT" MEANS. Under
// CANT-28 ruling 2 a socket is authorized once at accept and outlives the
// access token that opened it, so nothing about a revocation reaches a live
// session by itself — `store.Authenticate` guards the next REQUEST, and a
// session that is already streaming makes no further requests. Without this
// callback a revoked phone keeps receiving every message in every room it was
// in until it happens to drop, which is not what the person who clicked the
// button believes they did.
//
// THE SUBJECT IS A DEVICE OR A USER, matching RevocationPayload. A device
// revocation closes that device's sessions and leaves the person's other
// devices alone — "the other devices are untouched" is in this ticket's own
// `Done when`. A user revocation closes all of them, which is the deactivated
// account the payload has carried a field for since CANT-28 and which
// CANT-33's connector surface will one day publish.
//
// Close, not CloseNow: the peer is alive and the status is the point, exactly
// as the drain and the head-unreadable sever already argue. Collected under
// the lock and closed OUTSIDE it, in a goroutine each, because a close
// handshake waits for the peer's echo and this runs on the listener goroutine
// that serialises delivery for every session on the instance.
func (h *Hub) OnRevocation(ctx context.Context, p store.RevocationPayload) {
	if p.DeviceID == nil && p.UserID == nil {
		// Something NOTIFYed on the channel with neither subject set. Not
		// fatal — anything may notify on a channel name — but nothing here can
		// act on it, and a silent return would hide a publisher that is wrong.
		h.logger.Warn("revocation with no subject", "channel", store.RevocationChannel)
		return
	}

	h.mu.Lock()
	var doomed []*session
	// ONE ENTRY PER SESSION EVEN WHEN BOTH SUBJECTS NAME IT. A payload may
	// carry a device AND a user: CANT-33's deprovision is the natural producer,
	// since revoking a person's device and deactivating their account is one
	// action. A session matched by both would be appended twice, closed by two
	// goroutines, and counted twice on the log line below — and because
	// removeLocked is idempotent the COUNT would stay right while only the
	// telemetry lied, which is the kind of wrong that is found late or never.
	seen := map[*session]struct{}{}
	take := func(s *session) {
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		doomed = append(doomed, s)
	}
	if p.UserID != nil {
		// Every session this account holds, on every device.
		for s := range h.sessions[*p.UserID] {
			take(s)
		}
	}
	if p.DeviceID != nil {
		// Indexed by user, so the device is found by walking. The set is one
		// instance's attached sessions, and a revocation is rare.
		for _, set := range h.sessions {
			for s := range set {
				if s.conn.DeviceID() == *p.DeviceID {
					take(s)
				}
			}
		}
	}
	// Removed from the index under the same lock that found them, so nothing
	// enqueues to a session that is on its way out.
	for _, s := range doomed {
		h.removeLocked(s)
	}
	n := h.count
	h.mu.Unlock()

	for _, s := range doomed {
		go func(s *session) { _ = s.conn.Close(StatusRevoked, "credential revoked") }(s)
	}
	h.logger.Info("sessions severed by revocation",
		"device_id", p.DeviceID, "user_id", p.UserID,
		"sessions_severed", len(doomed), "sessions", n)
}

// OnRevocationGap is the revocation listener's reconnect callback, and it is
// not optional detail.
//
// A REVOCATION HAS NO CURSOR, so unlike the message path there is nothing to
// replay. Postgres queues nothing for a disconnected listener, so every
// revocation raised while this listener was down is gone with no record of how
// many — and each one is a session this instance is still serving. The only
// thing that closes that hole is asking the database about the sessions this
// instance actually holds, which is what store.DeadDevices does.
//
// Synchronous on the listener goroutine, as notify.go requires, and it never
// waits on anything that listener has to deliver.
func (h *Hub) OnRevocationGap(ctx context.Context) {
	h.mu.Lock()
	byDevice := map[uuid.UUID][]*session{}
	var all []*session
	for _, set := range h.sessions {
		for s := range set {
			d := s.conn.DeviceID()
			byDevice[d] = append(byDevice[d], s)
			all = append(all, s)
		}
	}
	h.mu.Unlock()
	if len(all) == 0 {
		return
	}

	ids := make([]uuid.UUID, 0, len(byDevice))
	for d := range byDevice {
		ids = append(ids, d)
	}

	var dead []uuid.UUID
	err := h.retry(ctx, revokeBudget, func() error {
		var e error
		dead, e = h.st.DeadDevices(ctx, ids)
		return e
	})
	if ctx.Err() != nil {
		h.logger.Debug("revocation gap abandoned on a cancelled context")
		return
	}
	if err != nil {
		// THE RE-CHECK IS THE ONLY THING THAT KNEW, so a failure here leaves
		// this instance unable to say whether any of its sessions is revoked.
		// Every one of them is closed rather than kept: the door re-runs
		// store.Authenticate on the next upgrade, which refuses a revoked
		// device and a deactivated account, so a reconnect turns an unknown
		// back into a known state. That is the same trade the head-unreadable
		// sever already makes.
		//
		// 1012 AND NOT StatusRevoked, WHICH WOULD BE THE BUG. These sessions
		// are not known to be revoked — most of them certainly are not — and
		// StatusRevoked is terminal by CANT-35's table. Telling every healthy
		// client on the instance never to reconnect would turn a transient
		// database failure into a permanent outage for all of them.
		h.mu.Lock()
		for _, s := range all {
			h.removeLocked(s)
		}
		h.mu.Unlock()
		for _, s := range all {
			go func(s *session) {
				_ = s.conn.Close(websocket.StatusServiceRestart, "revocation state unreadable after a listener gap")
			}(s)
		}
		h.logger.Error("revoked-device re-check failed after a gap; sessions severed for reconnect",
			"sessions_severed", len(all), "error", err)
		return
	}

	h.mu.Lock()
	var doomed []*session
	for _, d := range dead {
		doomed = append(doomed, byDevice[d]...)
	}
	for _, s := range doomed {
		h.removeLocked(s)
	}
	h.mu.Unlock()
	for _, s := range doomed {
		go func(s *session) { _ = s.conn.Close(StatusRevoked, "credential revoked") }(s)
	}
	if len(doomed) > 0 {
		h.logger.Warn("revocation gap: sessions severed on re-check",
			"checked", len(ids), "dead", len(dead), "sessions_severed", len(doomed))
		return
	}
	h.logger.Debug("revocation gap: nothing to sever", "checked", len(ids))
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
