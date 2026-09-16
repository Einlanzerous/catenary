package store

// CANT-21 — fanout between instances, over Postgres LISTEN/NOTIFY.
//
// No Redis. At this deployment's scale Postgres does the whole job and it is
// one fewer moving part, one fewer thing to secure, and one fewer thing that
// can be up while the database is down.
//
// TWO LAYERS, AND THEY MUST NOT BE BLURRED. This one is server-internal,
// between Go instances, and carries ids only. The WebSocket frame is
// server-to-client and carries the whole message. Merge them and you get one of
// two failures: a notify payload that overflows Postgres's 8,000-byte limit, or
// a socket that has degraded into a ping telling a client to go and fetch —
// which doubles latency for every message in the service.
//
// CANT-18's ruling 2 puts the pg_notify call inside SendMessage's own
// transaction, so a notification cannot exist without its message or the
// reverse. That is the MESSAGE call site: attemptSend, position 12, in
// messages.go. The REVOCATION call site is RevokeDevice in tokens.go, and the
// two are byte-identical in shape. CANT-92 added a THIRD: markRead in
// readstate.go, on the same reasoning, for the receipt shape below. All three
// are transactions in this service whose commit takes Postgres's
// instance-wide notify lock — messages.go's lock-order note names the first
// two; readstate.go's own comment reasons about markRead's, which reaches
// commit holding conversation_members FOR UPDATE plus the metadata bump's
// locks, and does not go on to take another after it. Everything below the
// call is here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NotifyChannel carries NotifyPayload's two shapes — a message notification
// and, since CANT-92, a receipt notification — and nothing else. One channel
// for both is the same decision as the one struct that crosses it: two
// shapes on two channels would be a second thing to keep in step for no
// reason the RevocationChannel split above does not already cover (that
// split exists because a revocation and a message decode DIFFERENTLY on a
// parse failure, which is not true of the two NotifyPayload shapes).
const NotifyChannel = "catenary_message"

// RevocationChannel carries "this credential is no longer live, sever it".
//
// A SECOND CHANNEL RATHER THAN A SECOND SHAPE ON THE FIRST — CANT-28 ruling 7,
// and the reason is that the alternative fails SILENTLY. The listener below
// decodes with a plain json.Unmarshal: Go ignores unknown fields and zeroes
// absent ones, so a revocation sent down catenary_message would not reach the
// "did not parse" branch at all — and CANT-92's widened NotifyPayload changes
// WHICH silent thing happens, because the two structs now share a JSON tag.
// A device revocation (`device_id` only) decodes with UserID nil: IsReceipt()
// is false, and it is delivered as a MESSAGE notification for the nil
// conversation at seq 0 — the original failure this paragraph described. A
// USER revocation (`user_id` set, CANT-33's) decodes with NotifyPayload's own
// UserID field populated, because `"user_id"` is the tag both structs use:
// IsReceipt() is now TRUE, and it is delivered as a RECEIPT notification for
// the nil conversation, span (0, 0] — which MessagesForReadNotify reads as
// empty and answers with zero rows, so this particular branch is a silent
// no-op rather than a wrong delivery. Neither outcome is an error, which is
// the actual point: nothing anywhere would say a misrouted payload happened,
// on either shape.
//
// It also left one thing alone that was not this ticket's to settle: the AST
// guard over NotifyPayload's field list, so the ids-only guarantee was not
// renegotiated in passing. CANT-92 later settled the question this comment
// deferred — widen NotifyPayload rather than add a second payload type — and
// the guard's `want` moved with it; see NotifyPayload below.
const RevocationChannel = "catenary_device_revoked"

// NotifyPayloadMax is Postgres's own limit on a NOTIFY payload.
//
// EXCEEDING IT DOES NOT LOSE A NOTIFICATION, IT LOSES THE MESSAGE. pg_notify
// raises on the connection that called it, and that call is inside
// SendMessage's transaction — so an oversized payload fails the insert, and a
// member's message is refused because of a broadcast detail they cannot see.
// That asymmetry is why the ticket asks for a test rather than a comment.
const NotifyPayloadMax = 8000

// NotifyPayload is what crosses between instances. IDS ONLY — never content.
// TWO SHAPES, ONE STRUCT, on CANT-92's decision: a message notification is
// (conversation_id, seq); a receipt notification (CANT-92) is
// (conversation_id, user_id, before, after). One struct rather than a second
// payload type on the same channel — two shapes on one channel is a second
// thing to keep in step, which is the whole premise this project is built
// against, and the ids-only guard below covers both without a second copy of
// itself.
//
// UserID IS THE DISCRIMINATOR: IsReceipt reports whether it is set. No
// message notification ever sets it, and every receipt notification names the
// member whose mark moved. Seq is meaningless on a receipt (omitted rather
// than zero, since a real message seq is never zero — CANT-14's ordinals are
// dense from 1) and Before/After are meaningless on a message.
//
// (conversation_id, seq) identifies a message exactly: UNIQUE (conversation_id,
// seq) is the constraint the whole thread ordering hangs off. So a receiver has
// everything it needs to read the row it is being told about, and nothing it
// could render without reading it. That is the point rather than an economy:
// the instant this struct can carry text, a message body is travelling through
// a channel with no retention story and landing in pg_stat_activity, which is
// exactly what D1's honesty about what the server can see would have to be
// rewritten to admit.
//
// (conversation_id, user_id, before, after) identifies exactly the messages a
// receipt changed the read_by of: seq IN (before, after] in that conversation,
// per MarkRead's own comment. A receiving instance re-reads that span itself
// — internal/hub's MessagesForReadNotify — rather than being handed anything
// it could render without a query, on the same reasoning as the message half.
//
// The field names are spelled out rather than shortened to `c` and `s`. The
// encoded payload is under a hundred bytes either way, a small fraction of the
// cap, so the saving is imaginary and the cost is a human reading a notify in a
// log and having to guess.
type NotifyPayload struct {
	ConversationID uuid.UUID `json:"conversation_id"`

	// Seq identifies one message. Absent on a receipt notification.
	Seq int64 `json:"seq,omitempty"`

	// UserID, Before and After are CANT-92's receipt notification: the member
	// whose mark moved, and the span seq IN (before, after] whose read_by
	// changed for them. Absent, together, on a message notification.
	UserID *uuid.UUID `json:"user_id,omitempty"`
	Before int64      `json:"before,omitempty"`
	After  int64      `json:"after,omitempty"`
}

// IsReceipt reports whether this is CANT-92's receipt shape rather than a
// message notification. UserID is the discriminator — see the type comment.
func (p NotifyPayload) IsReceipt() bool { return p.UserID != nil }

// RevocationPayload says which credentials stopped being live. IDS ONLY, on
// exactly the same terms as NotifyPayload, and guarded the same way.
//
// THE SUBJECT IS A DEVICE OR A USER, AND BOTH ARE HERE FROM THE START.
// CANT-30's `Done when` covers a revoked device. It does not cover a
// DEACTIVATED ACCOUNT — and under CANT-28 ruling 2 a socket is authorized once
// at accept, so a disabled person's session keeps streaming until it happens to
// drop. R6's sentence about a disabled account does not claim otherwise: it
// speaks about refreshing and enrolling, which are request paths.
//
// So the shape carries both today and the user half has no publisher yet: the
// write that sets users.deactivated_at is CANT-33's connector surface over an
// admin API that does not exist. Carrying the field now costs one nullable id
// and saves migrating a payload type across a running deployment later — the
// same argument refresh_tokens.family_id gets in 0007.
type RevocationPayload struct {
	DeviceID *uuid.UUID `json:"device_id,omitempty"`
	UserID   *uuid.UUID `json:"user_id,omitempty"`
}

// Encode renders a revocation and refuses one over the cap, on the same terms
// as NotifyPayload.Encode and for the same reason: the call is inside the
// transaction that performs the revocation, so an oversized payload would fail
// the revocation itself.
func (p RevocationPayload) Encode() (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("store: revocation payload: %w", err)
	}
	if err := withinNotifyCap(raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// Encode renders the payload and refuses one that would exceed the cap.
//
// STILL UNREACHABLE AT FIVE FIXED-WIDTH FIELDS, and that is the reason this
// is a function rather than an assumption: CANT-92 was the day somebody added
// a third (then a fourth, then a fifth), and the check was already here to
// hold, rather than something that had to be remembered on the way in. The
// failure it prevents surfaces as messages — or receipts — being refused
// rather than as notifications going missing.
func (p NotifyPayload) Encode() (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("store: notify payload: %w", err)
	}
	if err := withinNotifyCap(raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// withinNotifyCap is the check itself, separated so it can be tested against a
// payload that is genuinely over. Encode cannot produce one — two fixed-width
// fields do not reach 8,000 bytes — and a branch nothing can reach is a branch
// nothing has checked, which is not what you want standing between a widened
// struct and a refused message.
func withinNotifyCap(raw []byte) error {
	if len(raw) >= NotifyPayloadMax {
		return fmt.Errorf("store: notify payload is %d bytes, limit is %d: %w",
			len(raw), NotifyPayloadMax, ErrNotifyTooLarge)
	}
	return nil
}

// ErrNotifyTooLarge is the refusal above, exported so the message call site in
// messages.go can tell it from a database error and fail loudly rather than
// silently widening. senderror.go carries its row: `internal`, not retryable.
var ErrNotifyTooLarge = errors.New("store: notify payload exceeds the limit")

// Listener runs LISTEN on its own connection and calls OnNotify for each
// notification.
//
// ITS OWN CONNECTION, NOT ONE FROM THE POOL. LISTEN registers against a
// session, and a pooled connection handed back is a registration lost — so a
// listener on the pool works until the first idle moment and then silently
// stops. Holding a pooled connection forever instead would work and would take
// a connection out of the pool for the life of the process without saying so.
type Listener[P any] struct {
	// DSN is the same one the pool uses. A separate connection rather than a
	// separate database.
	DSN     string
	Channel string
	Logger  *slog.Logger

	// OnNotify receives each notification, in arrival order, on the Run
	// goroutine. A slow handler blocks the next notification rather than
	// buffering an unbounded queue — the caller owns its own fan-out.
	//
	// GENERIC OVER THE PAYLOAD because this service now has two channels
	// carrying two shapes (CANT-28 ruling 7), and a listener bound to one of
	// them would have meant either a second copy of the reconnect-and-gap logic
	// below or a discriminator on a struct whose field list is deliberately
	// frozen. The type parameter costs nothing and changes no behaviour: at the
	// time it was added nothing outside this file and its test constructed a
	// Listener at all.
	OnNotify func(context.Context, P)

	// OnGap says "you may have missed some", and it is not optional detail.
	//
	// SYNCHRONOUS, on the Run goroutine, between LISTEN succeeding and the wait
	// loop starting — which is the ordering that makes it correct, so it cannot
	// be moved off the goroutine to make it faster. It delays entry to the wait
	// loop and costs nothing: pgx buffers notifications that arrive during any
	// other operation and drains them before it reads the socket, so a message
	// that lands while a resync is running is delivered when the loop starts.
	//
	// What it must not do is wait on something this listener has to deliver.
	// That is the one shape that wedges, and it is self-referential rather than
	// accidental — but OnNotify states its blocking contract and this is the
	// callback that will be handed a resync, so it says so too.
	//
	// POSTGRES DOES NOT QUEUE NOTIFICATIONS FOR A DISCONNECTED LISTENER. Every
	// notification raised while this connection was down is gone, permanently
	// and with no record of how many. So a reconnect is not a recovery, it is a
	// hole — and the layer above has to close it the only way it can, by
	// resyncing over /sync from its cursor. A listener that reconnected quietly
	// would leave a client's socket looking healthy while it silently missed
	// every message sent during the outage, which is the exact failure the two
	// ordinals exist to make impossible.
	OnGap func(context.Context)
}

// Run listens until ctx is cancelled, reconnecting through outages.
//
// The shared Postgres restarting is an ordinary event here rather than an
// incident — the same reasoning as ConnectWithRetry — so giving up on the first
// disconnect would take fanout down for the whole estate's restart window.
func (l *Listener[P]) Run(ctx context.Context) error {
	backoff := initialListenBackoff

	// connected is "has a subscription ever succeeded", NOT "is this the first
	// loop iteration". At boot against a Postgres that is still starting, the
	// first attempt fails and the second is still the first subscription —
	// counting iterations would announce a gap to a consumer that has never
	// received anything, which is the every-boot-looks-like-an-outage case this
	// is supposed to avoid.
	connected := false

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// THE GAP IS RAISED AFTER LISTEN, NOT BEFORE IT, and the ordering is the
		// whole correctness of this callback.
		//
		// The layer above answers a gap by resyncing from its cursor. Announce
		// it before subscribing and the sequence is: resync reads to head N,
		// THEN a message commits with nobody listening, THEN the subscription
		// registers — and that message is on neither path, with no second gap
		// to say so. The window is a TCP connect, TLS, auth and a round trip
		// against a database that has just been restarting, so it is not
		// theoretical.
		//
		// Subscribed first, the window is closed by construction: anything
		// committed before the resync is in the resync, anything after it is on
		// the channel, and the overlap is a duplicate rather than a hole.
		err := l.listen(ctx, func() {
			if connected && l.OnGap != nil {
				l.OnGap(ctx)
			}
			connected = true
			// Reset here rather than at the top of the loop. A process that
			// rode out one outage hours ago should not wait the 5s cap for its
			// next reconnect — the cap describes an outage in progress, not the
			// health of a connection that has been up since.
			backoff = initialListenBackoff
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.logger().Warn("notify listener disconnected; reconnecting",
			"channel", l.Channel, "error", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxListenBackoff {
			backoff = maxListenBackoff
		}
	}
}

const (
	initialListenBackoff = 250 * time.Millisecond
	maxListenBackoff     = 5 * time.Second
)

// logger is never nil. Logger is an exported field with no constructor to
// default it, so a Listener built without one would panic on its first
// disconnect — in the reconnect path, which is the least observed code here and
// the worst place to discover a nil pointer. slog.Default rather than a discard
// handler: a listener whose reconnects are silent is worse than a noisy one.
func (l *Listener[P]) logger() *slog.Logger {
	if l.Logger == nil {
		return slog.Default()
	}
	return l.Logger
}

// listen holds one connection for as long as it lives.
func (l *Listener[P]) listen(ctx context.Context, ready func()) error {
	conn, err := pgx.Connect(ctx, l.DSN)
	if err != nil {
		return fmt.Errorf("store: listener connect: %w", err)
	}
	defer func() {
		// A fresh context, because the usual reason we are here is that ctx was
		// cancelled — and a close on a cancelled context leaks the connection
		// until the server times it out.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	// Quoted through pgx rather than interpolated. The channel is a constant
	// today; a LISTEN whose channel came from anywhere else would be the one
	// statement in this service built by string concatenation.
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{l.Channel}.Sanitize()); err != nil {
		return fmt.Errorf("store: listen %q: %w", l.Channel, err)
	}
	l.logger().Info("notify listener ready", "channel", l.Channel)
	ready()

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("store: wait for notification: %w", err)
		}
		var p P
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			// Logged and skipped rather than fatal. Anything may NOTIFY on a
			// channel name, and one malformed payload from somewhere else must
			// not take this service's fanout down.
			//
			// THE PAYLOAD ITSELF IS NOT LOGGED, and the reason is the branch:
			// this runs only when the bytes did NOT parse as a NotifyPayload,
			// so "it is ids only" is exactly what is not known here. Anything
			// that can reach this channel could have written up to 8,000 bytes
			// of arbitrary text, and logging it verbatim is a message body in
			// the service log by another route. The length and the channel are
			// what a reader needs to find the source.
			l.logger().Warn("notify payload did not parse",
				"channel", n.Channel, "bytes", len(n.Payload), "error", err)
			continue
		}
		if l.OnNotify != nil {
			l.OnNotify(ctx, p)
		}
	}
}
