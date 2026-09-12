package api

// CANT-22 — the door. Where the transport and the trust boundary meet.
//
// Three things happen here and nothing else: the UPGRADE authenticates the
// connection before the 101 is written, the HANDSHAKE binds the socket to an
// account and a device, and the CODEC carries generated wire types and only
// those. Everything that crosses sockets — fan-out of a committed message, a
// receipt, typing — is the hub's, which is a `full` sub-task of this ticket
// and plugs in through Attach and Handle below. The heartbeat CLOCK is
// CANT-23's; `ready` announces the dial, and this file answers a ping, and
// that is all it does about liveness.
//
// THE CREDENTIAL RIDES ON Sec-WebSocket-Protocol (CANT-28 ruling 1), because it
// is the one request header `new WebSocket(url, protocols)` lets a browser
// set. The client offers two values — `catenary.v1` and
// `catenary.token.<token>` — and the server verifies the second and echoes
// back ONLY the first. Echoing the credential would put it in a response
// header on the 101, which proxies log; that is the objection that ruled out a
// query-string token, arriving from the other direction.
//
// AUTHORIZED ONCE, AT ACCEPT (ruling 2). The session outlives the access token
// that opened it. A revocation severs it through the fanout — CANT-30's — and
// an access token expiring under a live socket is not an event.
//
// ONE REFUSAL SHAPE. An upgrade that fails authentication gets the same 401
// with the same body GET /sync writes, whatever the cause. A malformed request
// is a 400 and is not part of that rule: the shape of a token says nothing
// about whether a credential exists.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
	"github.com/magos/catenary/internal/wireview"
)

const (
	// subprotocolV1 is the one value the server echoes. It names the
	// protocol, not the wire schema version — that is negotiated by the
	// hello, below, and the two are allowed to move independently.
	subprotocolV1 = "catenary.v1"

	// tokenSubprotocolPrefix is the credential's ride. What follows it is
	// checked against the wire's Token alphabet before anything looks it up:
	// `/` and `=` are not legal RFC 7230 token characters and an intermediary
	// is entitled to mangle them, which is why the encoding is base64url
	// without padding and why the check is the alphabet rather than a
	// blocklist.
	tokenSubprotocolPrefix = "catenary.token."

	// wireVersionMin is the oldest schema version this server still speaks.
	// The window a hello must land in is [wireVersionMin, wire.WireVersion],
	// and today the two are equal. It lives beside its first use rather than
	// in the schema because it is a fact about THIS server's tolerance, not
	// about the contract — CANT-74 dropped the generated version of it for
	// exactly that reason.
	wireVersionMin = 1

	// R1's dial. 35 s sits under Cloudflare's ~100 s idle window; 2 missed
	// pongs is the client's severance rule. Constants here rather than
	// config because CANT-23 owns turning them into a deployed dial, and a
	// number announced in `ready` has to be the number the server will
	// enforce — which is also CANT-23's.
	heartbeatIntervalSec = 35
	missedPongLimit      = 2

	// writeTimeout bounds one frame write. A peer that stops reading is a
	// peer that will be severed, not one that is allowed to wedge the
	// goroutine writing to it.
	writeTimeout = 10 * time.Second

	// defaultMaxFrameBytes is the inbound frame bound when Deps carries none.
	// A `send` is a text body plus a handful of upload ids; 256 KiB is far
	// above any configured message bound and far below anything that would
	// trouble the process.
	defaultMaxFrameBytes = 256 << 10
)

// helloTimeout is how long an accepted socket has to say hello. A variable
// rather than a constant so a test can shorten it; nothing else writes it.
var helloTimeout = 10 * time.Second

// Session is one accepted, authenticated, hello'd socket: what the hub holds.
//
// Send is safe to call from any goroutine — the hub's, the listener's — and
// the read side is this package's alone. That split is what lets a fan-out
// write to a session without knowing anything about its read loop.
type Session struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	DeviceID uuid.UUID

	conn *websocket.Conn
}

// Send writes one server frame. A write that fails has already ended the
// session; the read loop sees the same closed connection and returns.
func (s *Session) Send(ctx context.Context, f wire.ServerFrame) error {
	return writeFrame(ctx, s.conn, f)
}

// socketHandler serves GET /ws.
//
// Everything before websocket.Accept is HTTP and answers as HTTP: a refusal
// here is a status code and a JSON body, because there is no socket yet to
// carry a frame. Everything after it is frames.
func socketHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			hasV1  bool
			tokens []string
		)
		for _, p := range offeredSubprotocols(r.Header) {
			switch {
			case p == subprotocolV1:
				hasV1 = true
			case strings.HasPrefix(p, tokenSubprotocolPrefix):
				tokens = append(tokens, strings.TrimPrefix(p, tokenSubprotocolPrefix))
			}
		}

		// THE ORDER IS THE POLICY. Malformed first, because a 400 says only
		// that the request is not one this server understands. Then the
		// credential, because without one nothing else about the request
		// matters and the 401 must not vary by what else was wrong with it.
		switch {
		case len(tokens) > 1:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "offer one catenary.token.<token> subprotocol, not several"})
			return
		case len(tokens) == 1 && !wire.TokenPattern.MatchString(tokens[0]):
			// Same rule as POST /enroll: a token that is not shaped like one
			// is the sender's malformed request, and saying so leaks nothing.
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "catenary.token.<token> is not shaped like a token"})
			return
		case len(tokens) == 0:
			// THE SAME BODY GET /sync WRITES. There is one way for this
			// service to say no to a credential, and "you presented none" is
			// not a different way.
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		case !hasV1:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "subprotocol " + subprotocolV1 + " is required"})
			return
		}

		caller, err := d.Authenticate(r.Context(), tokens[0])
		switch {
		case errors.Is(err, store.ErrUnauthorized):
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		case err != nil:
			d.Logger.ErrorContext(r.Context(), "socket authentication failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "authentication failed"))
			return
		}

		// A BOT HAS NO DEVICE, so there is nothing for the handshake to bind
		// the socket to, and the hello's required device_id could only ever be
		// a fabrication. Refused at the door rather than at the hello, and
		// with the credential refusal's own body: store.Authenticate names "a
		// bot attempted a device-only operation" as one of its four, and this
		// is that operation. A bot's write path is REST (CANT-75).
		if caller.IsBot() {
			d.Logger.WarnContext(r.Context(), "socket refused",
				"reason", "bot has no device to bind", "user_id", caller.UserID)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}

		// Subprotocols lists ONLY catenary.v1, so that is the only value the
		// library can echo. The token never appears in a response header, and
		// TestTheTokenIsNotEchoed reads every header on the 101 to prove it.
		//
		// No OriginPatterns and no InsecureSkipVerify: the library refuses a
		// browser request whose Origin host is not the request Host. The web
		// client is served by this same binary, so same-origin holds, and the
		// desktop and Android clients send no Origin at all.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{subprotocolV1},
		})
		if err != nil {
			// Accept has already written its own 4xx. Logged, not answered.
			d.Logger.WarnContext(r.Context(), "socket upgrade failed",
				"user_id", caller.UserID, "device_id", caller.DeviceID, "error", err)
			return
		}
		limit := d.MaxFrameBytes
		if limit <= 0 {
			limit = defaultMaxFrameBytes
		}
		conn.SetReadLimit(limit)

		serveSession(r.Context(), d, conn, caller)
	}
}

// offeredSubprotocols reads every value the client offered, whether it sent
// them on one header line separated by commas or on several lines.
func offeredSubprotocols(h http.Header) []string {
	var out []string
	for _, line := range h.Values("Sec-WebSocket-Protocol") {
		for _, v := range strings.Split(line, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// serveSession runs one socket from accept to close. It returns when the
// connection is gone, and the connection is always gone when it returns.
func serveSession(ctx context.Context, d Deps, conn *websocket.Conn, caller store.Caller) {
	// A close after a close is the expected no-op, so the error is not read.
	defer conn.CloseNow()

	logger := d.Logger.With("user_id", caller.UserID, "device_id", caller.DeviceID)

	// THE HELLO. The first known frame must be it, and it must arrive within
	// helloTimeout: a socket that authenticated and then says nothing is
	// holding a goroutine for nobody.
	hello, ok := awaitHello(ctx, conn, logger)
	if !ok {
		return
	}

	// [wireVersionMin, WireVersion], or `error{wire_version_unsupported}` and
	// close. This is the one place wire_version_unsupported is produced —
	// REST negotiates no wire version — which is why the CANT-83 guard exempts
	// it by name: with one producer there is no second decision to diverge.
	if hello.WireVersion < wireVersionMin || hello.WireVersion > wire.WireVersion {
		logger.WarnContext(ctx, "hello refused",
			"reason", "wire_version_unsupported", "wire_version", hello.WireVersion)
		refuse(ctx, conn, wire.ServerError{
			Code:      wire.ErrorCodeWireVersionUnsupported,
			Message:   fmt.Sprintf("this server speaks wire versions %d to %d", wireVersionMin, wire.WireVersion),
			Retryable: false,
		})
		return
	}

	// THE BINDING. The token resolved to a device at the door; the hello names
	// one. They agree or the session does not exist. A device presenting
	// another device's id is not a different caller — the token already said
	// who it is — but it is a client that is lying about which install it is,
	// and the per-device model (revocation, the hello log line, the
	// per-device cursor histogram) is only honest if this holds.
	device, err := uuid.Parse(string(hello.DeviceID))
	if err != nil || device != caller.DeviceID {
		logger.WarnContext(ctx, "hello refused",
			"reason", "device mismatch", "hello_device_id", string(hello.DeviceID))
		refuse(ctx, conn, wire.ServerError{
			Code:      wire.ErrorCodeUnauthorized,
			Message:   "device_id does not match the credential's device",
			Retryable: false,
		})
		return
	}

	sess := &Session{ID: uuid.New(), UserID: caller.UserID, DeviceID: caller.DeviceID, conn: conn}
	logger = logger.With("session_id", sess.ID)

	// The one thing the server does with the cursor: compare it to head and
	// log one line (CANT-24). Head becomes ready.log_seq.
	res, err := d.Hello(ctx, store.HelloRequest{
		DeviceID: caller.DeviceID, SessionID: sess.ID, Cursor: hello.ResumeFromLogSeq,
	})
	switch {
	case errors.Is(err, store.ErrNegativeCursor):
		// Unreachable behind the generated decoder, which enforces LogSeq's
		// minimum; answered anyway, as the malformed hello it is.
		closeWith(conn, websocket.StatusPolicyViolation, "resume_from_log_seq must not be negative")
		return
	case err != nil:
		logger.ErrorContext(ctx, "hello failed", "error", err)
		refuse(ctx, conn, serverError(err, "hello failed"))
		return
	}

	// ATTACH BEFORE READY. A message committing after this point reaches the
	// session live; one committing before it is on the /sync page the client
	// issues on receiving `ready` (CANT-24 obligation 3: `ready` is a trigger
	// and catch-up ends on a page requested after it). Attaching after `ready`
	// would open a window where a message is neither.
	if d.Attach != nil {
		detach := d.Attach(sess)
		defer detach()
	}

	ready := wire.ServerReady{
		SessionID:            wire.Uuid(sess.ID.String()),
		WireVersion:          ptrInt64(wire.WireVersion),
		ServerTime:           stamp(store.ServerTime()),
		HeartbeatIntervalSec: heartbeatIntervalSec,
		MissedPongLimit:      missedPongLimit,
		LogSeq:               res.Head,
		// FALSE, ALWAYS, IN WIRE VERSION 1 (CANT-24 ruling 0): the socket
		// does not resume, /sync does. Written as a literal beside the
		// sentence that says why, rather than as a field the store could one
		// day set from the wrong place.
		Resumed: false,
	}
	if err := sess.Send(ctx, ready); err != nil {
		logger.InfoContext(ctx, "session closed", "phase", "ready", "error", err)
		return
	}
	logger.InfoContext(ctx, "session open",
		"client_info", derefString(hello.ClientInfo), "hello_outcome", string(res.Outcome))

	readLoop(ctx, d, sess, logger)
}

// awaitHello reads until the first known frame and requires it to be a hello.
// Unknown frame types are ignored, as the schema says every decoder must; a
// known frame that is not a hello is a protocol error and closes the socket.
func awaitHello(ctx context.Context, conn *websocket.Conn, logger *slog.Logger) (wire.ClientHello, bool) {
	// A TIMER AND A CLOSE HANDSHAKE, not a context deadline. coder/websocket
	// closes the whole connection when a Read's context expires, which leaves
	// the client with a bare EOF and no close frame saying why. Closing from a
	// timer instead sends the 1008 the client can log, and unblocks the read
	// below with the peer's echo of it.
	var timedOut atomic.Bool
	timer := time.AfterFunc(helloTimeout, func() {
		timedOut.Store(true)
		closeWith(conn, websocket.StatusPolicyViolation, "hello expected")
	})
	defer timer.Stop()

	for {
		f, err := readFrame(ctx, conn)
		switch {
		case err != nil && timedOut.Load():
			logger.InfoContext(ctx, "session closed", "phase", "hello", "reason", "no hello before the deadline")
			return wire.ClientHello{}, false
		case err != nil:
			endSession(ctx, conn, logger, "hello", err)
			return wire.ClientHello{}, false
		case f == nil:
			continue
		}
		hello, ok := f.(wire.ClientHello)
		if !ok {
			logger.InfoContext(ctx, "session closed", "phase", "hello", "reason", "first frame was not a hello")
			closeWith(conn, websocket.StatusPolicyViolation, "first frame must be hello")
			return wire.ClientHello{}, false
		}
		return hello, true
	}
}

// readLoop is the session after `ready`: one frame at a time, until the
// connection ends.
func readLoop(ctx context.Context, d Deps, sess *Session, logger *slog.Logger) {
	for {
		f, err := readFrame(ctx, sess.conn)
		if err != nil {
			endSession(ctx, sess.conn, logger, "open", err)
			return
		}
		if f == nil {
			// An unrecognised tag: ignore and carry on, as every generated
			// decoder is told to. A newer client speaking a frame this
			// server does not know is a client the compatibility policy
			// promised not to disconnect.
			continue
		}
		switch v := f.(type) {
		case wire.Ping:
			if err := sess.Send(ctx, wire.Pong{ID: v.ID, At: ptrStamp(store.ServerTime())}); err != nil {
				endSession(ctx, sess.conn, logger, "open", err)
				return
			}
		case wire.Pong:
			// The answer to a server ping. CANT-23 sends those and counts
			// these; until then a pong is accepted and nothing is owed.
		case wire.ClientHello:
			// A second hello is a client bug, and ignoring it would hide the
			// bug behind a session that looks fine.
			logger.InfoContext(ctx, "session closed", "phase", "open", "reason", "second hello")
			closeWith(sess.conn, websocket.StatusPolicyViolation, "hello already received")
			return
		case wire.ClientSend:
			if !handleSend(ctx, d, sess, logger, v) {
				return
			}
		default:
			// `read` and `typing`: both cross sockets, so both are the hub's.
			// Handed over when a hub is wired; dropped, and said so at debug,
			// when none is — neither frame is acknowledged on the wire, so an
			// unwired hub owes the client nothing it could otherwise notice.
			if d.Handle != nil {
				d.Handle(ctx, sess, v)
				continue
			}
			logger.DebugContext(ctx, "frame not handled", "type", wireTag(v))
		}
	}
}

// handleSend is the `send` frame: one store call, one `ack` or one `error`.
// Returns false when the session is over.
func handleSend(ctx context.Context, d Deps, sess *Session, logger *slog.Logger, v wire.ClientSend) bool {
	m := store.NewMessage{
		ConversationID: mustUUID(v.ConversationID),
		AuthorID:       sess.UserID,
		ClientID:       mustUUID(v.ClientID),
		SenderDeviceID: &sess.DeviceID,
		Text:           v.Text,
	}
	if v.ReplyToMessageID != nil {
		id := mustUUID(*v.ReplyToMessageID)
		m.ReplyTo = &id
	}
	for _, a := range v.Attachments {
		m.Attachments = append(m.Attachments, store.NewAttachment{Kind: a.Kind, UploadID: mustUUID(a.UploadID)})
	}

	sent, err := d.Send(ctx, m)
	if err != nil {
		// THE CODE COMES FROM THE STORE. SendErrorFor is the one door the
		// CANT-83 guard leaves open, and this transport does not decide which
		// of the six send codes a refusal gets — REST and the socket must
		// answer the identical cause with the identical code, and the only way
		// to guarantee that is for neither of them to choose.
		var se *store.SendError
		_ = errors.As(store.SendErrorFor(err), &se)
		logger.Log(ctx, se.Level(), "send refused",
			append([]any{"client_id", v.ClientID, "conversation_id", v.ConversationID}, se.LogAttrs()...)...)
		frame := se.Wire("send failed")
		cid := v.ClientID
		frame.ClientID = &cid
		// A refused send ends nothing. The client marks the message and the
		// session carries on; a session severed for a non-member send would
		// punish every other conversation the person is in.
		if err := sess.Send(ctx, frame); err != nil {
			endSession(ctx, sess.conn, logger, "open", err)
			return false
		}
		return true
	}

	// FROM Sent, NEVER FROM THE REQUEST. Deduplication is per author and not
	// per conversation, so a replayed key can resolve to a row in another
	// conversation than the one this frame named; acking the request's
	// conversation with that row's seq would tell the client a seq exists
	// where it does not. messages.go says this at length.
	ack := wire.ServerAck{
		ClientID:       v.ClientID,
		MessageID:      wire.Uuid(sent.ID.String()),
		ConversationID: wire.Uuid(sent.ConversationID.String()),
		Seq:            sent.Seq,
		LogSeq:         sent.LogSeq,
		At:             stamp(sent.At),
	}
	if sent.Duplicate {
		dup := true
		ack.Duplicate = &dup
	}
	if err := sess.Send(ctx, ack); err != nil {
		endSession(ctx, sess.conn, logger, "open", err)
		return false
	}
	return true
}

// malformedFrame is a frame that arrived but did not decode: the peer's
// fault, distinguishable from a transport error, which is nobody's.
type malformedFrame struct{ err error }

func (m *malformedFrame) Error() string { return "malformed frame: " + m.err.Error() }
func (m *malformedFrame) Unwrap() error { return m.err }

// errBinaryFrame: the wire is JSON text. A binary frame is not a frame this
// protocol has.
var errBinaryFrame = &malformedFrame{errors.New("binary frame; the wire is text")}

// readFrame reads one inbound frame and decodes it. See decodeInbound for
// what comes back.
func readFrame(ctx context.Context, conn *websocket.Conn) (wire.ClientFrame, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return decodeInbound(typ, data)
}

// decodeInbound is the socket's decoder: the GENERATED one, so every
// constraint the schema states is enforced here by the same rule the
// TypeScript and Dart clients enforce it by. Factored from readFrame so the
// codec test can hold it to every vector after reading the bytes itself.
//
// A nil frame with a nil error is an unrecognised tag, which the caller
// ignores. A *malformedFrame is a decode failure, the peer's fault.
func decodeInbound(typ websocket.MessageType, data []byte) (wire.ClientFrame, error) {
	if typ != websocket.MessageText {
		return nil, errBinaryFrame
	}
	f, err := wire.DecodeClientFrame(data)
	if err != nil {
		return nil, &malformedFrame{err}
	}
	return f, nil
}

// writeFrame encodes one server frame with the generated encoder — which
// injects the `type` tag, so no call site can forget it — and writes it as one
// text message.
func writeFrame(ctx context.Context, conn *websocket.Conn, f wire.ServerFrame) error {
	b, err := encodeFrame(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

// encodeFrame is the socket's encoder, factored so the codec test can hold it
// to every vector: json.Marshal over a generated type, and nothing else.
func encodeFrame(f any) ([]byte, error) {
	return json.Marshal(f)
}

// refuse writes an error frame and closes. The frame is what the client acts
// on; the close status says the client should not simply retry the same
// handshake.
func refuse(ctx context.Context, conn *websocket.Conn, e wire.ServerError) {
	_ = writeFrame(ctx, conn, e)
	closeWith(conn, websocket.StatusPolicyViolation, string(e.Code))
}

// endSession logs how a session ended and closes whatever is left of it.
//
// A malformed frame is answered with a close that names it, because a client
// that sent one has a bug and deserves to be told. A transport error — the
// peer closed, the network dropped — is logged and the connection is dropped;
// there is nobody to tell.
func endSession(ctx context.Context, conn *websocket.Conn, logger *slog.Logger, phase string, err error) {
	var mf *malformedFrame
	if errors.As(err, &mf) {
		logger.WarnContext(ctx, "session closed", "phase", phase, "reason", "malformed frame", "error", mf.err)
		closeWith(conn, websocket.StatusPolicyViolation, mf.Error())
		return
	}
	status := websocket.CloseStatus(err)
	switch status {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		logger.InfoContext(ctx, "session closed", "phase", phase, "status", int(status))
	default:
		logger.InfoContext(ctx, "session closed", "phase", phase, "status", int(status), "error", err)
	}
}

// closeWith performs the close handshake with a reason the frame can carry —
// RFC 6455 bounds the reason at 123 bytes, and coder/websocket refuses a
// longer one rather than truncating it.
func closeWith(conn *websocket.Conn, status websocket.StatusCode, reason string) {
	const maxReason = 123
	if len(reason) > maxReason {
		reason = reason[:maxReason]
	}
	_ = conn.Close(status, reason)
}

// mustUUID parses a wire Uuid the generated decoder has already validated. A
// failure here is a decoder bug, not input, and the zero value is what the
// store refuses rather than what it stores.
func mustUUID(u wire.Uuid) uuid.UUID {
	id, err := uuid.Parse(string(u))
	if err != nil {
		return uuid.Nil
	}
	return id
}

func stamp(t time.Time) wire.Timestamp {
	return wire.Timestamp(t.UTC().Format(wireview.TimeLayout))
}

func ptrStamp(t time.Time) *wire.Timestamp {
	s := stamp(t)
	return &s
}

func ptrInt64(v int64) *int64 { return &v }

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// wireTag names a frame for a log line without a type switch over the union.
func wireTag(f wire.ClientFrame) string {
	if t, ok := f.(interface{ WireTag() string }); ok {
		return t.WireTag()
	}
	return fmt.Sprintf("%T", f)
}
