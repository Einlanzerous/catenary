package main

// CANT-22's three clauses, end to end through the REAL router that setup()
// builds and the real store on Postgres — not the stubs internal/api tests
// against. The claim here is that a real process registers GET /ws behind
// store.Authenticate, that the hello binds to the device the credential
// resolved to, and that a `send` over the socket is the same send /sync then
// serves.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/wire"
)

func wsDial(t *testing.T, srv *httptest.Server, protos ...string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
		&websocket.DialOptions{Subprotocols: protos})
	if conn != nil {
		t.Cleanup(func() { _ = conn.CloseNow() })
	}
	return conn, resp, err
}

func wsSend(ctx context.Context, t *testing.T, conn *websocket.Conn, f wire.ClientFrame) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func wsRead(ctx context.Context, t *testing.T, conn *websocket.Conn) wire.ServerFrame {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	f, err := wire.DecodeServerFrame(data)
	if err != nil || f == nil {
		t.Fatalf("undecodable server frame %s: %v", data, err)
	}
	return f
}

func TestTheSocketIsRegisteredAndRefusesAnUnauthenticatedUpgrade(t *testing.T) {
	_, _, _, h := authFixture(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	_, resp, err := wsDial(t, srv, "catenary.v1")
	if err == nil {
		t.Fatal("an unauthenticated upgrade was accepted")
	}
	if resp == nil {
		t.Fatal("no response")
	}
	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("GET /ws is not registered — the socket seams are unwired in setup()")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated upgrade = %d, want 401", resp.StatusCode)
	}
}

func TestTheSocketIsAbsentWithoutAStore(t *testing.T) {
	d := setup(config.Config{Addr: ":0", LogFormat: "json"}, slog.New(slog.DiscardHandler), nil)
	rec := httptest.NewRecorder()
	d.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /ws with no store = %d, want 404", rec.Code)
	}
}

// Enroll, upgrade, hello, send, and see the message on /sync. Then revoke and
// see the door close.
func TestADeviceEnrollsOpensASocketAndSends(t *testing.T) {
	ctx, pool, st, h := authFixture(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ada := mkUser(ctx, t, pool, "ada", "Ada Lovelace")
	theo := mkUser(ctx, t, pool, "theo", "Theo")
	issued, err := st.IssueEnrollmentToken(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	code, body := do(t, h, enrollRequest(t, issued.Plaintext, "Framework 13"))
	if code != http.StatusOK {
		t.Fatalf("enroll = %d: %s", code, body)
	}
	var enrolled wire.EnrollResponse
	if err := json.Unmarshal(body, &enrolled); err != nil {
		t.Fatal(err)
	}
	access := string(enrolled.AccessToken)

	// The upgrade, with the credential riding as the second subprotocol and
	// only the first echoed.
	conn, resp, err := wsDial(t, srv, "catenary.v1", "catenary.token."+access)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "catenary.v1" {
		t.Errorf("echoed subprotocol = %q, want catenary.v1 alone", got)
	}

	// The hello binds to the device /enroll minted.
	wsSend(ctx, t, conn, wire.ClientHello{WireVersion: wire.WireVersion, DeviceID: enrolled.DeviceID})
	ready, ok := wsRead(ctx, t, conn).(wire.ServerReady)
	if !ok {
		t.Fatal("no ready")
	}
	if ready.Resumed || ready.LogSeq != 0 {
		t.Errorf("ready on an empty log = resumed=%v log_seq=%d, want false / 0", ready.Resumed, ready.LogSeq)
	}

	// A send over the socket is the real send path: acked with the ordinals
	// the store drew, and visible on /sync afterwards.
	group := mkGroup(ctx, t, pool, "Engine room", ada, theo)
	clientID := uuid.New()
	text := "first message over the socket"
	wsSend(ctx, t, conn, wire.ClientSend{
		ClientID: wire.Uuid(clientID.String()), ConversationID: wire.Uuid(group.String()), Text: &text,
	})
	ack, ok := wsRead(ctx, t, conn).(wire.ServerAck)
	if !ok {
		t.Fatal("no ack")
	}
	if string(ack.ClientID) != clientID.String() || ack.Seq != 1 || ack.LogSeq != 1 || ack.Duplicate != nil {
		t.Errorf("ack = %+v, want seq 1 / log_seq 1 for the first message in a fresh log", ack)
	}

	// The same key again: the original comes back, marked as a replay, and
	// no second row is drawn.
	wsSend(ctx, t, conn, wire.ClientSend{
		ClientID: wire.Uuid(clientID.String()), ConversationID: wire.Uuid(group.String()), Text: &text,
	})
	replay, ok := wsRead(ctx, t, conn).(wire.ServerAck)
	if !ok {
		t.Fatal("no ack for the replay")
	}
	if replay.MessageID != ack.MessageID || replay.Duplicate == nil || !*replay.Duplicate {
		t.Errorf("replay ack = %+v, want the original %s with duplicate: true", replay, ack.MessageID)
	}

	req := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	code, body = do(t, h, req)
	if code != http.StatusOK {
		t.Fatalf("GET /sync = %d: %s", code, body)
	}
	var page wire.SyncResponse
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("sync page: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != ack.MessageID {
		t.Errorf("/sync after a socket send = %d message(s), want the acked one %s", len(page.Messages), ack.MessageID)
	}

	// A send into a conversation the account is not in is refused with the
	// store's code, and the session survives it.
	other := mkGroup(ctx, t, pool, "Not yours", theo)
	wsSend(ctx, t, conn, wire.ClientSend{
		ClientID: wire.Uuid(uuid.New().String()), ConversationID: wire.Uuid(other.String()), Text: &text,
	})
	refused, ok := wsRead(ctx, t, conn).(wire.ServerError)
	if !ok {
		t.Fatal("no error frame for a non-member send")
	}
	if string(refused.Code) != "not_a_member" || refused.Retryable {
		t.Errorf("non-member send over the socket = %s retryable=%v", refused.Code, refused.Retryable)
	}

	// Revocation: the next upgrade is refused at the door. Severing THIS
	// socket is CANT-30's.
	device, err := uuid.Parse(string(enrolled.DeviceID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	_, resp, err = wsDial(t, srv, "catenary.v1", "catenary.token."+access)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("upgrade after revocation: err=%v resp=%v, want 401", err, resp)
	}
}

// The hello names a device the credential did not resolve to. A real device
// row exists for it, so this is not "unknown device" — it is the binding rule.
func TestTheHelloMustNameTheCredentialsOwnDevice(t *testing.T) {
	ctx, pool, st, h := authFixture(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ada := mkUser(ctx, t, pool, "ada", "Ada Lovelace")
	var enrolled [2]wire.EnrollResponse
	for i, name := range []string{"phone", "laptop"} {
		issued, err := st.IssueEnrollmentToken(ctx, ada)
		if err != nil {
			t.Fatal(err)
		}
		code, body := do(t, h, enrollRequest(t, issued.Plaintext, name))
		if code != http.StatusOK {
			t.Fatalf("enroll %s = %d: %s", name, code, body)
		}
		if err := json.Unmarshal(body, &enrolled[i]); err != nil {
			t.Fatal(err)
		}
	}

	conn, _, err := wsDial(t, srv, "catenary.v1", "catenary.token."+string(enrolled[0].AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	// The phone's token, the laptop's device id.
	wsSend(ctx, t, conn, wire.ClientHello{WireVersion: wire.WireVersion, DeviceID: enrolled[1].DeviceID})
	e, ok := wsRead(ctx, t, conn).(wire.ServerError)
	if !ok {
		t.Fatal("expected an error frame")
	}
	if e.Code != wire.ErrorCodeUnauthorized {
		t.Errorf("code = %s, want unauthorized", e.Code)
	}
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("socket after a refused binding: %v, want close 1008", err)
	}
}
