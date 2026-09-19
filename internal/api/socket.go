package api

// CANT-22 — the door. Where the transport and the trust boundary meet.
//
// Three things happen here and nothing else: the UPGRADE authenticates the
// connection before the 101 is written, the HANDSHAKE binds the socket to an
// account and a device, and the CODEC carries generated wire types and only
// those. Everything that crosses sockets — fan-out of a committed message, a
// receipt, typing — is the hub's, which is a `full` sub-task of this ticket
// and plugs in through Attach and Handle below. The heartbeat is CANT-23's:
// `ready` announces the dial, this file answers a `ping` with a `pong`, and a
// session that goes quiet longer than the announced window severs itself —
// never the reverse. THE SCHEMA'S Ping IS EXPLICITLY BIDIRECTIONAL and does
// not forbid a server-originated one — this server simply has no use for
// one: the watchdog already proves liveness from the client's own ping, and
// a second, server-originated ping would only double the traffic for
// nothing the watchdog does not already have. This file never touches the
// WebSocket protocol's own ping/pong control frames, though, and THAT is
// invariant 3's ground: those are invisible to application code and
// answerable by an intermediary, so a heartbeat built on them could not
// honestly be called kept.
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

	// DefaultHeartbeatIntervalSec and DefaultMissedPongLimit are R1's dial:
	// 35 s sits under Cloudflare's ~100 s idle window, and 2 missed pings is
	// the client's own severance rule (spike/r1-websocket/FINDINGS.md §3).
	// Exported so cmd/catenary's composition root can merge an operator's
	// CATENARY_HEARTBEAT_INTERVAL_SEC / CATENARY_MISSED_PONG_LIMIT override
	// over them — the same zero-means-unset convention CANT-83 established
	// for the send bounds, so the number is not restated in internal/config.
	DefaultHeartbeatIntervalSec = 35
	DefaultMissedPongLimit      = 2

	// writeTimeout bounds one frame write. A peer that stops reading is a
	// peer that will be severed, not one that is allowed to wedge the
	// goroutine writing to it.
	writeTimeout = 10 * time.Second

	// defaultMaxFrameBytes is the inbound frame bound when Deps carries none.
	// A `send` is a text body plus a handful of upload ids; 256 KiB is far
	// above any configured message bound and far below anything that would
	// trouble the process.
	defaultMaxFrameBytes = 256 << 10

	// DefaultHelloTimeout is how long an accepted socket has to say hello when
	// Deps carries no override: a socket that authenticated and then says
	// nothing is holding a goroutine for nobody.
	//
	// A CONSTANT, AND DELIBERATELY (CANT-115). This was a package-level `var`
	// so a test could shorten it, and the write raced an EARLIER test's server
	// goroutine still reading it inside awaitHello. `go test -race` finds that
	// only when the package runs unfiltered — a `-run` filter removes the
	// second participant and the race cannot occur — so it read as an
	// intermittent flake belonging to nobody. Nothing in the shipped binary
	// ever wrote it, so the fix removes the shared mutable knob rather than
	// synchronising it: the deadline is per-server on Deps.HelloTimeout, the
	// same zero-means-default convention MaxFrameBytes and the heartbeat dial
	// already follow.
	DefaultHelloTimeout = 10 * time.Second
)

// statusHeartbeatTimeout is CANT-23's own severance code: RFC 6455 §7.4.2's
// private-use range (3000-4999) lets each distinct reason a session ends
// carry its own code, which this file already follows for the hub's drain
// (1001) and its head-unreadable sever (1012) — "a status that is the
// point" (Session.Close's doc). It is deliberately not 1008 Policy
// Violation: the client did nothing wrong, and 1008 is this door's own
// signal not to retry the same handshake (refuse, above). A client's
// reconnect table must treat 4000 as reconnect-with-backoff, the same
// bucket 1001 and 1012 are already in.
const statusHeartbeatTimeout websocket.StatusCode = 4000

// statusHelloTimeout is the hello deadline's own code (CANT-122, CANT-31
// ruling 5), the next private-use number after 4000 and hub.StatusRevoked's
// 4001. It used to be a bare 1008, and that made a bare 1008 unclassifiable:
// of the six sites that close 1008 with no preceding `error` frame, five are
// client bugs and this was the one that is not — a socket that went quiet
// between the upgrade and its hello is a slow or stalled network, and the
// same client on the same build gets in on its next attempt. With this code
// carved out, a bare 1008 means only "client bug", which is what lets
// CANT-31's record make it terminal.
//
// RECONNECT WITH BACKOFF, the bucket 1001, 1012 and 4000 are already in. A
// client that predates this code still does the right thing with it, because
// the recorded default for a close code nobody listed is also reconnect.
//
// THE DEPLOYED SERVER CARRIES THIS BEFORE ANY CLIENT APPLIES THE RULE.
// Against a server that still says 1008 here, a client treating a bare 1008
// as terminal stops on a transient stall. A rollback past this commit
// reintroduces that, and a relaunch recovers from it, because a protocol
// terminal keeps the stored credential.
const statusHelloTimeout websocket.StatusCode = 4002

// heartbeatWindow is how long a session may go without a `ping` before this
// server severs it, derived from the exact numbers `ready` announced.
//
// ONLY `ping` RESETS IT (readLoop's case wire.Ping) — not `send`, not
// `read`, not any other traffic. The wire schema is explicit that the
// client "must send `ping`" on the announced cadence regardless of what
// else it is doing (R1's hb client pings on its own schedule throughout,
// independent of message traffic), so a client that stops pinging while
// still sending has stopped keeping its half of the contract `ready`
// stated, and inferring liveness from other frames would silently loosen
// it. It is also the only rule simple enough to state as an invariant a
// test can pin deterministically.
//
// THE MULTIPLE IS missed_pong_limit + 1, not missed_pong_limit alone. R1
// measured the client's own worst-case self-detection latency at
// heartbeat × (missed_pong_limit + 1) (FINDINGS.md §3) — the server's own
// backstop uses the identical formula from its side of the same clock, so
// it is never tighter than a well-behaved client's own patience. A bare
// missed_pong_limit multiple would let a single slow round trip sever a
// client before the client's own rule ever would, which is the false
// severance this margin exists to rule out; R1's measured round trip
// (8-12 ms) is negligible next to either number, so the margin costs
// nothing when the network is healthy.
func heartbeatWindow(intervalSec, missedPongLimit int) time.Duration {
	return time.Duration(intervalSec) * time.Second * time.Duration(missedPongLimit+1)
}

// heartbeatWatchdog is the severance timer serveSession starts the moment
// `ready` goes out and readLoop's case wire.Ping resets.
//
// severed IS SET BEFORE THE CLOSE, so readLoop's read error branch can tell
// "this connection ended because the watchdog fired" from every other
// closure cause and log the event exactly ONCE — the same problem
// awaitHello's own timedOut solves for the hello deadline, and the same
// fix: the timer records the cause and says nothing, and the reader that
// actually observes the resulting error is the one that logs, instead of
// both the timer and endSession logging "session closed" for one event at
// two different levels.
type heartbeatWatchdog struct {
	timer   *time.Timer
	window  time.Duration
	severed atomic.Bool
}

// startHeartbeatWatchdog arms a watchdog against conn. Armed, never running:
// nothing here blocks, and nothing here has an opinion about the session
// before this call — see serveSession for why that call is exactly at
// `ready`.
func startHeartbeatWatchdog(conn *websocket.Conn, window time.Duration) *heartbeatWatchdog {
	w := &heartbeatWatchdog{window: window}
	w.timer = time.AfterFunc(window, func() {
		w.severed.Store(true)
		closeWith(conn, statusHeartbeatTimeout, "heartbeat timeout: no ping within the announced window")
	})
	return w
}

// ping re-arms the watchdog at its original window. The only caller is
// readLoop's case wire.Ping — see heartbeatWindow's doc for why nothing else
// resets it.
func (w *heartbeatWatchdog) ping() { w.timer.Reset(w.window) }

// stop disarms the watchdog. serveSession defers it so nothing outlives the
// session it was armed for.
func (w *heartbeatWatchdog) stop() { w.timer.Stop() }

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

// Close performs the close handshake with a status the client can act on:
// the hub's drain (1001), its head-unreadable sever (1012), and this file's
// own heartbeat timeout (4000, statusHeartbeatTimeout) — the peer is alive
// in every case and the code is the point. It is closeWith with a receiver,
// and the read loop sees the handshake complete and runs detach.
func (s *Session) Close(status websocket.StatusCode, reason string) error {
	closeWith(s.conn, status, reason)
	return nil
}

// CloseNow drops the connection with NO close handshake. It exists for one
// caller, the hub's slow-consumer sever: a peer that is not reading data
// frames will not read a close frame either, and the handshake would wait on
// the write lock the peer's stalled write is holding. The peer sees the
// connection end and reconnects under its abnormal-closure rule.
func (s *Session) CloseNow() error {
	return s.conn.CloseNow()
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
	// the hello deadline: a socket that authenticated and then says nothing is
	// holding a goroutine for nobody. Zero means Deps carries no override, so
	// the default stands — the same convention MaxFrameBytes and the deployed
	// dial below already follow, and the reason the deadline is read here per
	// session rather than from a package variable a test can write (CANT-115).
	helloTimeout := d.HelloTimeout
	if helloTimeout <= 0 {
		helloTimeout = DefaultHelloTimeout
	}
	hello, ok := awaitHello(ctx, conn, logger, helloTimeout)
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

	// THE DEPLOYED DIAL (CANT-23). Zero means Deps carries no override, so
	// the default stands — the same convention MaxFrameBytes follows below.
	// Whatever these resolve to is exactly what `ready` announces AND
	// exactly what heartbeatWindow enforces: one pair of numbers, read once,
	// so the server can never enforce a window `ready` did not announce.
	heartbeatInterval := d.HeartbeatIntervalSec
	if heartbeatInterval <= 0 {
		heartbeatInterval = DefaultHeartbeatIntervalSec
	}
	missedPongLimit := d.MissedPongLimit
	if missedPongLimit <= 0 {
		missedPongLimit = DefaultMissedPongLimit
	}

	ready := wire.ServerReady{
		SessionID:            wire.Uuid(sess.ID.String()),
		WireVersion:          ptrInt64(wire.WireVersion),
		ServerTime:           stamp(store.ServerTime()),
		HeartbeatIntervalSec: int64(heartbeatInterval),
		MissedPongLimit:      int64(missedPongLimit),
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

	// THE HEARTBEAT CLOCK STARTS HERE, AND NOWHERE EARLIER (CANT-24's "What
	// CANT-102 inherits", docs/decisions/cant-24-resume.md). A session that
	// takes the whole of the hello deadline to say hello is not a session this
	// watchdog has an opinion about yet — only a `ready`'d session can go
	// quiet on a clock it was never told about.
	watchdog := startHeartbeatWatchdog(conn, heartbeatWindow(heartbeatInterval, missedPongLimit))
	defer watchdog.stop()

	readLoop(ctx, d, sess, logger, watchdog)
}

// awaitHello reads until the first known frame and requires it to be a hello.
// Unknown frame types are ignored, as the schema says every decoder must; a
// known frame that is not a hello is a protocol error and closes the socket.
//
// timeout is the caller's already-resolved deadline — serveSession merges
// Deps.HelloTimeout with DefaultHelloTimeout once per session, so this
// function reads no package state and two servers in one test binary can hold
// two different deadlines without sharing a variable (CANT-115).
func awaitHello(ctx context.Context, conn *websocket.Conn, logger *slog.Logger, timeout time.Duration) (wire.ClientHello, bool) {
	// A TIMER AND A CLOSE HANDSHAKE, not a context deadline. coder/websocket
	// closes the whole connection when a Read's context expires, which leaves
	// the client with a bare EOF and no close frame saying why. Closing from a
	// timer instead sends a status the client can log and act on — its own,
	// statusHelloTimeout, and not the 1008 the two client-bug closes below
	// share — and unblocks the read below with the peer's echo of it.
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		closeWith(conn, statusHelloTimeout, "hello expected")
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
//
// watchdog is the heartbeat severance timer started the moment `ready` went
// out (serveSession).
func readLoop(ctx context.Context, d Deps, sess *Session, logger *slog.Logger, watchdog *heartbeatWatchdog) {
	for {
		f, err := readFrame(ctx, sess.conn)
		if err != nil {
			if watchdog.severed.Load() {
				// LOGGED HERE, NOT BY THE TIMER (heartbeatWatchdog's doc):
				// the same event endSession would otherwise also log, at a
				// different level and with different fields, splitting one
				// severance into two "session closed" lines.
				logger.WarnContext(ctx, "session closed", "phase", "open",
					"reason", "heartbeat timeout", "status", int(statusHeartbeatTimeout),
					"window", watchdog.window.String())
				return
			}
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
			// THE ONLY FRAME THAT RESETS THE WATCHDOG (heartbeatWindow's
			// doc). Reset before the reply: a session that pinged has proven
			// itself live even if the reply that follows fails to write.
			watchdog.ping()
			if err := sess.Send(ctx, wire.Pong{ID: v.ID, At: ptrStamp(store.ServerTime())}); err != nil {
				endSession(ctx, sess.conn, logger, "open", err)
				return
			}
		case wire.Pong:
			// The schema allows Pong in either direction, and this server
			// could send a Ping of its own without breaking anything it
			// states — it simply does not, because the watchdog already has
			// what it needs from the client's own ping (see the file doc).
			// So an inbound Pong here answers a ping this server never
			// sent; accepted and, like every frame but Ping, it does not
			// touch the watchdog above.
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
		case wire.ClientRead, wire.ClientTyping:
			// Both cross sockets, so both are the hub's. Handed over when a
			// hub is wired; dropped, and said so at debug, when none is —
			// neither frame is acknowledged on the wire, so an unwired hub
			// owes the client nothing it could otherwise notice.
			if d.Handle != nil {
				d.Handle(ctx, sess, v)
				continue
			}
			logger.DebugContext(ctx, "frame not handled", "type", wireTag(v))
		default:
			// NAMED, NOT INHERITED. A frame the generated decoder knows and
			// this switch does not is a client frame the schema gained after
			// this loop was written, and where it goes — the hub, a handler
			// here, a refusal — is a decision, not a default. Until it is
			// made the frame is dropped like an unknown tag, and the log
			// line says so at warn, because unlike an unknown tag this one
			// is the server's omission rather than the client's novelty.
			logger.WarnContext(ctx, "frame type known to the schema but not to this loop",
				"type", wireTag(v))
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
