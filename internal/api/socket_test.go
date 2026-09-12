package api

// CANT-22 — the door, without a database.
//
// Every seam the socket needs is a function on Deps, so this file can stand
// the whole upgrade, handshake and frame loop up against stubs and drive it
// over a REAL socket: httptest.Server on one end, coder/websocket's Dial on the
// other. What it cannot claim is that the stubs are the store's real answers;
// cmd/catenary/socket_test.go makes that claim through setup() and Postgres.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
	"github.com/magos/catenary/internal/wireview"
)

const testHead = int64(4120)

// stubs is one caller, one token, and a channel per seam so a test can read
// what the door handed the store.
type stubs struct {
	token  string
	caller store.Caller

	hellos   chan store.HelloRequest
	sends    chan store.NewMessage
	sent     store.Sent
	sendErr  error
	attached chan *Session
	detached chan *Session
	handled  chan wire.ClientFrame
}

func newStubs(t *testing.T) *stubs {
	t.Helper()
	tok, _, err := store.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	return &stubs{
		token:    tok,
		caller:   store.Caller{UserID: uuid.New(), DeviceID: uuid.New(), Kind: "person"},
		hellos:   make(chan store.HelloRequest, 8),
		sends:    make(chan store.NewMessage, 8),
		attached: make(chan *Session, 8),
		detached: make(chan *Session, 8),
		handled:  make(chan wire.ClientFrame, 8),
	}
}

func (s *stubs) deps() Deps {
	return Deps{
		Logger: discardLogger(),
		Authenticate: func(_ context.Context, token string) (store.Caller, error) {
			if token != s.token {
				return store.Caller{}, store.ErrUnauthorized
			}
			return s.caller, nil
		},
		Hello: func(_ context.Context, req store.HelloRequest) (store.HelloResult, error) {
			s.hellos <- req
			return store.HelloResult{Outcome: store.HelloBehind, Head: testHead, Cursor: req.Cursor}, nil
		},
		Send: func(_ context.Context, m store.NewMessage) (store.Sent, error) {
			s.sends <- m
			if s.sendErr != nil {
				return store.Sent{}, s.sendErr
			}
			return s.sent, nil
		},
		Attach: func(sess *Session) func() {
			s.attached <- sess
			return func() { s.detached <- sess }
		},
		Handle: func(_ context.Context, _ *Session, f wire.ClientFrame) { s.handled <- f },
	}
}

func serve(t *testing.T, d Deps) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(d))
	t.Cleanup(srv.Close)
	return srv
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dial opens the socket with the given subprotocols. A non-101 comes back as
// an error with the response attached, body included.
func dial(t *testing.T, srv *httptest.Server, protos ...string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	conn, resp, err := websocket.Dial(testCtx(t), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
		&websocket.DialOptions{Subprotocols: protos})
	if conn != nil {
		t.Cleanup(func() { _ = conn.CloseNow() })
	}
	return conn, resp, err
}

func tokenProto(token string) string { return tokenSubprotocolPrefix + token }

func refusalBody(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("refusal body %q is not JSON: %v", raw, err)
	}
	return body
}

func sendText(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	var b []byte
	switch x := v.(type) {
	case string:
		b = []byte(x)
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Write(testCtx(t), websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readServer(t *testing.T, conn *websocket.Conn) wire.ServerFrame {
	t.Helper()
	_, data, err := conn.Read(testCtx(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	f, err := wire.DecodeServerFrame(data)
	if err != nil {
		t.Fatalf("server sent a frame the generated decoder refuses: %v\n%s", err, data)
	}
	if f == nil {
		t.Fatalf("server sent a frame of unknown type: %s", data)
	}
	return f
}

// expectClose reads until the connection closes and asserts the status.
func expectClose(t *testing.T, conn *websocket.Conn, want websocket.StatusCode) string {
	t.Helper()
	_, data, err := conn.Read(testCtx(t))
	if err == nil {
		t.Fatalf("expected the socket to close, got a frame: %s", data)
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("expected a close, got: %v", err)
	}
	if ce.Code != want {
		t.Errorf("close status = %d (%s), want %d", ce.Code, ce.Reason, want)
	}
	return ce.Reason
}

func helloFor(s *stubs, cursor *int64) wire.ClientHello {
	info := "catenary-test/0"
	return wire.ClientHello{
		WireVersion:      wire.WireVersion,
		DeviceID:         wire.Uuid(s.caller.DeviceID.String()),
		ResumeFromLogSeq: cursor,
		ClientInfo:       &info,
	}
}

// open is the happy path up to and including `ready`.
func open(t *testing.T, srv *httptest.Server, s *stubs) (*websocket.Conn, wire.ServerReady) {
	t.Helper()
	conn, _, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cursor := int64(41207)
	sendText(t, conn, helloFor(s, &cursor))
	ready, ok := readServer(t, conn).(wire.ServerReady)
	if !ok {
		t.Fatalf("first server frame was not ready")
	}
	return conn, ready
}

// --- the upgrade -----------------------------------------------------------

func TestUpgradeRefusesWithoutACredential(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	for name, protos := range map[string][]string{
		"only catenary.v1 offered": {subprotocolV1},
		"no subprotocol at all":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, resp, err := dial(t, srv, protos...)
			if err == nil {
				t.Fatal("an unauthenticated upgrade was accepted")
			}
			if resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %v, want 401", resp)
			}
			// THE SAME BODY GET /sync WRITES. One way to say no.
			if body := refusalBody(t, resp); body["code"] != "unauthorized" {
				t.Errorf("refusal body = %v", body)
			}
		})
	}
}

func TestUpgradeRefusesAnUnknownCredential(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	other, _, err := store.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := dial(t, srv, subprotocolV1, tokenProto(other))
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("well-formed unknown token: err=%v status=%v, want 401", err, resp)
	}
	if body := refusalBody(t, resp); body["code"] != "unauthorized" {
		t.Errorf("refusal body = %v", body)
	}
}

// A malformed request is a 400 and says nothing about credentials — the same
// rule POST /enroll follows.
func TestUpgradeRefusesAMalformedOffer(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	for name, protos := range map[string][]string{
		"token not shaped like one": {subprotocolV1, tokenProto("not-a-token")},
		"token with padding":        {subprotocolV1, tokenProto(s.token[:42] + "=")},
		"two tokens":                {subprotocolV1, tokenProto(s.token), tokenProto(s.token)},
		"catenary.v1 not offered":   {tokenProto(s.token)},
	} {
		t.Run(name, func(t *testing.T) {
			_, resp, err := dial(t, srv, protos...)
			if err == nil {
				t.Fatal("a malformed offer was accepted")
			}
			if resp == nil || resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %v, want 400", resp)
			}
			if body := refusalBody(t, resp); body["error"] == "" || body["code"] != "" {
				t.Errorf("a 400 carries an error and no code: %v", body)
			}
		})
	}
}

func TestUpgradeRefusesABot(t *testing.T) {
	s := newStubs(t)
	s.caller = store.Caller{UserID: uuid.New(), Kind: "bot"}
	srv := serve(t, s.deps())

	_, resp, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bot's upgrade: err=%v status=%v, want 401 — a bot has no device to bind", err, resp)
	}
}

// CANT-28: two values offered, ONE echoed. The credential must not appear in
// any response header on the 101, because proxies log those.
func TestTheTokenIsNotEchoed(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	_, resp, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := resp.Header.Values("Sec-WebSocket-Protocol"); len(got) != 1 || got[0] != subprotocolV1 {
		t.Errorf("Sec-WebSocket-Protocol on the 101 = %q, want exactly [%q]", got, subprotocolV1)
	}
	for name, values := range resp.Header {
		for _, v := range values {
			if strings.Contains(v, s.token) {
				t.Errorf("the token is echoed in response header %s", name)
			}
		}
	}
}

func TestTheRouteIsAbsentWithoutAllThreeSeams(t *testing.T) {
	s := newStubs(t)
	d := s.deps()
	d.Send = nil
	h := NewRouter(d)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /ws with Send unwired = %d, want 404: a half-wired socket is worse than none", rec.Code)
	}
}

// --- the handshake ---------------------------------------------------------

func TestHelloBindsTheSocketToTheAccountAndDevice(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	conn, ready := open(t, srv, s)
	defer conn.CloseNow()

	if ready.Resumed {
		t.Error("ready.resumed = true; the server never sets it in wire version 1 (CANT-24 ruling 0)")
	}
	if ready.LogSeq != testHead {
		t.Errorf("ready.log_seq = %d, want head %d", ready.LogSeq, testHead)
	}
	if ready.WireVersion == nil || *ready.WireVersion != wire.WireVersion {
		t.Errorf("ready.wire_version = %v, want %d", ready.WireVersion, wire.WireVersion)
	}
	if ready.HeartbeatIntervalSec != heartbeatIntervalSec || ready.MissedPongLimit != missedPongLimit {
		t.Errorf("ready announces %d s / %d, want %d s / %d",
			ready.HeartbeatIntervalSec, ready.MissedPongLimit, heartbeatIntervalSec, missedPongLimit)
	}
	if _, err := time.Parse(wireview.TimeLayout, string(ready.ServerTime)); err != nil {
		t.Errorf("ready.server_time %q is not the wire format: %v", ready.ServerTime, err)
	}
	sessionID, err := uuid.Parse(string(ready.SessionID))
	if err != nil {
		t.Fatalf("ready.session_id %q: %v", ready.SessionID, err)
	}

	// What the store was asked: this device, this session, the client's cursor.
	req := <-s.hellos
	if req.DeviceID != s.caller.DeviceID || req.SessionID != sessionID {
		t.Errorf("Hello was asked about device %s session %s, want %s / %s",
			req.DeviceID, req.SessionID, s.caller.DeviceID, sessionID)
	}
	if req.Cursor == nil || *req.Cursor != 41207 {
		t.Errorf("Hello cursor = %v, want 41207", req.Cursor)
	}

	// And the hub was handed the same binding, BEFORE ready reached the client.
	select {
	case sess := <-s.attached:
		if sess.ID != sessionID || sess.UserID != s.caller.UserID || sess.DeviceID != s.caller.DeviceID {
			t.Errorf("attached session %+v does not match the binding", sess)
		}
	default:
		t.Error("ready arrived before Attach was called")
	}
}

func TestHelloWithoutACursorIsAHelloWithoutACursor(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	conn, _, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
	if err != nil {
		t.Fatal(err)
	}
	sendText(t, conn, helloFor(s, nil))
	if _, ok := readServer(t, conn).(wire.ServerReady); !ok {
		t.Fatal("no ready")
	}
	if req := <-s.hellos; req.Cursor != nil {
		t.Errorf("an absent resume_from_log_seq reached the store as %d", *req.Cursor)
	}
}

func TestHelloRefusesAnUnsupportedWireVersion(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	for _, v := range []int64{wireVersionMin - 1, wire.WireVersion + 1} {
		conn, _, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
		if err != nil {
			t.Fatal(err)
		}
		h := helloFor(s, nil)
		h.WireVersion = v
		sendText(t, conn, h)

		e, ok := readServer(t, conn).(wire.ServerError)
		if !ok {
			t.Fatalf("wire_version %d: expected an error frame", v)
		}
		if e.Code != wire.ErrorCodeWireVersionUnsupported || e.Retryable {
			t.Errorf("wire_version %d: got %s retryable=%v, want wire_version_unsupported non-retryable", v, e.Code, e.Retryable)
		}
		expectClose(t, conn, websocket.StatusPolicyViolation)
	}
	select {
	case <-s.hellos:
		t.Error("the store was asked about a hello the version check refused")
	default:
	}
}

func TestHelloRefusesAnotherDevicesID(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	conn, _, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
	if err != nil {
		t.Fatal(err)
	}
	h := helloFor(s, nil)
	h.DeviceID = wire.Uuid(uuid.New().String())
	sendText(t, conn, h)

	e, ok := readServer(t, conn).(wire.ServerError)
	if !ok {
		t.Fatal("expected an error frame")
	}
	if e.Code != wire.ErrorCodeUnauthorized || e.Retryable {
		t.Errorf("got %s retryable=%v, want unauthorized non-retryable", e.Code, e.Retryable)
	}
	expectClose(t, conn, websocket.StatusPolicyViolation)
	select {
	case <-s.attached:
		t.Error("a session that failed its binding was attached")
	default:
	}
}

func TestTheFirstKnownFrameMustBeAHello(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	t.Run("a ping first closes the socket", func(t *testing.T) {
		conn, _, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
		if err != nil {
			t.Fatal(err)
		}
		sendText(t, conn, wire.Ping{ID: "p-0"})
		expectClose(t, conn, websocket.StatusPolicyViolation)
	})

	t.Run("an unknown frame type first is ignored", func(t *testing.T) {
		conn, _, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
		if err != nil {
			t.Fatal(err)
		}
		sendText(t, conn, `{"type":"dance","tempo":120}`)
		sendText(t, conn, helloFor(s, nil))
		if _, ok := readServer(t, conn).(wire.ServerReady); !ok {
			t.Error("a hello after an unknown frame was not answered with ready")
		}
	})
}

func TestASilentSocketIsClosedAtTheHelloDeadline(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	prev := helloTimeout
	helloTimeout = 200 * time.Millisecond
	t.Cleanup(func() { helloTimeout = prev })

	conn, _, err := dial(t, srv, subprotocolV1, tokenProto(s.token))
	if err != nil {
		t.Fatal(err)
	}
	if reason := expectClose(t, conn, websocket.StatusPolicyViolation); !strings.Contains(reason, "hello") {
		t.Errorf("close reason %q does not say what was expected", reason)
	}
}

// --- the frame loop --------------------------------------------------------

func TestPingIsAnsweredWithPong(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())
	conn, _ := open(t, srv, s)

	sendText(t, conn, wire.Ping{ID: "p-1"})
	pong, ok := readServer(t, conn).(wire.Pong)
	if !ok {
		t.Fatal("expected a pong")
	}
	if pong.ID != "p-1" {
		t.Errorf("pong.id = %q, want the ping's id", pong.ID)
	}
	if pong.At == nil {
		t.Error("pong carries no server time")
	}
}

func TestSendIsAckedFromWhatTheStoreWrote(t *testing.T) {
	s := newStubs(t)
	// The store answers with a DIFFERENT conversation than the frame names —
	// the replayed-key case messages.go describes — and the ack must follow
	// the store, never the request.
	s.sent = store.Sent{
		ID: uuid.New(), ConversationID: uuid.New(), Seq: 7, LogSeq: testHead + 1,
		At: time.Date(2026, 9, 12, 9, 30, 0, 250_000_000, time.UTC), Duplicate: true,
	}
	srv := serve(t, s.deps())
	conn, _ := open(t, srv, s)

	clientID, conv, reply, upload := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	text := "hello from the door"
	replyID := wire.Uuid(reply.String())
	sendText(t, conn, wire.ClientSend{
		ClientID: wire.Uuid(clientID.String()), ConversationID: wire.Uuid(conv.String()),
		Text: &text, ReplyToMessageID: &replyID,
		Attachments: []wire.OutboundAttachment{{Kind: "image", UploadID: wire.Uuid(upload.String())}},
	})

	ack, ok := readServer(t, conn).(wire.ServerAck)
	if !ok {
		t.Fatal("expected an ack")
	}
	if string(ack.ClientID) != clientID.String() || string(ack.MessageID) != s.sent.ID.String() ||
		string(ack.ConversationID) != s.sent.ConversationID.String() ||
		ack.Seq != 7 || ack.LogSeq != testHead+1 || string(ack.At) != "2026-09-12T09:30:00.250Z" {
		t.Errorf("ack = %+v, not built from Sent %+v", ack, s.sent)
	}
	if ack.Duplicate == nil || !*ack.Duplicate {
		t.Error("a replayed send was acked without duplicate: true")
	}

	m := <-s.sends
	switch {
	case m.AuthorID != s.caller.UserID:
		t.Errorf("author = %s, want the bound account", m.AuthorID)
	case m.SenderDeviceID == nil || *m.SenderDeviceID != s.caller.DeviceID:
		t.Errorf("sender device = %v, want the bound device", m.SenderDeviceID)
	case m.ClientID != clientID || m.ConversationID != conv:
		t.Errorf("ids: client %s conv %s", m.ClientID, m.ConversationID)
	case m.Text == nil || *m.Text != text:
		t.Errorf("text = %v", m.Text)
	case m.ReplyTo == nil || *m.ReplyTo != reply:
		t.Errorf("reply_to = %v", m.ReplyTo)
	case len(m.Attachments) != 1 || m.Attachments[0].Kind != "image" || m.Attachments[0].UploadID != upload:
		t.Errorf("attachments = %+v", m.Attachments)
	}
}

func TestARefusedSendCarriesTheStoresCodeAndTheClientID(t *testing.T) {
	s := newStubs(t)
	s.sendErr = store.ErrNotAMember
	srv := serve(t, s.deps())
	conn, _ := open(t, srv, s)

	clientID := uuid.New()
	sendText(t, conn, wire.ClientSend{
		ClientID: wire.Uuid(clientID.String()), ConversationID: wire.Uuid(uuid.New().String()),
	})
	e, ok := readServer(t, conn).(wire.ServerError)
	if !ok {
		t.Fatal("expected an error frame")
	}
	// THE CODE IS THE STORE'S. This test may name it; the door may not.
	if string(e.Code) != "not_a_member" || e.Retryable {
		t.Errorf("got %s retryable=%v", e.Code, e.Retryable)
	}
	if e.ClientID == nil || string(*e.ClientID) != clientID.String() {
		t.Errorf("error.client_id = %v, want %s so the outbox can mark the right message", e.ClientID, clientID)
	}

	// A refused send does not end the session.
	sendText(t, conn, wire.Ping{ID: "still-here"})
	if pong, ok := readServer(t, conn).(wire.Pong); !ok || pong.ID != "still-here" {
		t.Error("the session did not survive a refused send")
	}
}

func TestAMalformedFrameClosesTheSession(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())

	t.Run("a frame the generated decoder refuses", func(t *testing.T) {
		conn, _ := open(t, srv, s)
		sendText(t, conn, `{"type":"ping"}`) // no id
		if reason := expectClose(t, conn, websocket.StatusPolicyViolation); !strings.Contains(reason, "malformed") {
			t.Errorf("close reason %q does not name the fault", reason)
		}
	})

	t.Run("a binary frame", func(t *testing.T) {
		conn, _ := open(t, srv, s)
		if err := conn.Write(testCtx(t), websocket.MessageBinary, []byte(`{"type":"ping","id":"b"}`)); err != nil {
			t.Fatal(err)
		}
		expectClose(t, conn, websocket.StatusPolicyViolation)
	})

	t.Run("a second hello", func(t *testing.T) {
		conn, _ := open(t, srv, s)
		sendText(t, conn, helloFor(s, nil))
		expectClose(t, conn, websocket.StatusPolicyViolation)
	})
}

func TestAnUnknownFrameTypeIsIgnored(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())
	conn, _ := open(t, srv, s)

	sendText(t, conn, `{"type":"dance","tempo":120}`)
	sendText(t, conn, wire.Ping{ID: "after"})
	if pong, ok := readServer(t, conn).(wire.Pong); !ok || pong.ID != "after" {
		t.Error("an unknown frame type cost the session")
	}
}

func TestAnOversizeFrameIsRefused(t *testing.T) {
	s := newStubs(t)
	d := s.deps()
	d.MaxFrameBytes = 512
	srv := serve(t, d)
	conn, _ := open(t, srv, s)

	big := strings.Repeat("x", 2048)
	sendText(t, conn, wire.ClientSend{
		ClientID: wire.Uuid(uuid.New().String()), ConversationID: wire.Uuid(uuid.New().String()), Text: &big,
	})
	expectClose(t, conn, websocket.StatusMessageTooBig)
	select {
	case <-s.sends:
		t.Error("an oversize frame reached the store")
	default:
	}
}

func TestReadAndTypingGoToTheHub(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())
	conn, _ := open(t, srv, s)

	sendText(t, conn, wire.ClientRead{ConversationID: wire.Uuid(uuid.New().String()), UpToSeq: 3})
	if _, ok := (<-s.handled).(wire.ClientRead); !ok {
		t.Error("read did not reach Handle")
	}
	sendText(t, conn, wire.ClientTyping{ConversationID: wire.Uuid(uuid.New().String()), State: wire.TypingStateStart})
	if _, ok := (<-s.handled).(wire.ClientTyping); !ok {
		t.Error("typing did not reach Handle")
	}
}

func TestDetachRunsWhenTheSessionEnds(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())
	conn, ready := open(t, srv, s)

	attached := <-s.attached
	_ = conn.Close(websocket.StatusNormalClosure, "bye")

	select {
	case detached := <-s.detached:
		if detached != attached || detached.ID.String() != string(ready.SessionID) {
			t.Error("detach was handed a different session than attach")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detach did not run after the client closed")
	}
}

// The hub's write side: a frame pushed into a session from outside its read
// loop reaches the client, decodes, and is the frame that was sent.
func TestASessionCanBeWrittenToFromOutsideItsLoop(t *testing.T) {
	s := newStubs(t)
	srv := serve(t, s.deps())
	conn, _ := open(t, srv, s)
	sess := <-s.attached

	want := wire.ServerResyncRequired{Reason: wire.ResyncReasonCursorTooOld, LogSeq: testHead}
	if err := sess.Send(testCtx(t), want); err != nil {
		t.Fatal(err)
	}
	got, ok := readServer(t, conn).(wire.ServerResyncRequired)
	if !ok || got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
