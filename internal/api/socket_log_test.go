package api

// CANT-350 — a /ws upgrade that never reaches "session open" leaves one line.
//
// The browser reports only that the socket never opened, and neither the proxy
// nor this service used to say anything about a refused upgrade, so a minute of
// failed dials was undiagnosable. Each refusal below must log exactly one
// "socket upgrade refused" line with its reason, and none may carry the token.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// refusedLines returns the parsed "socket upgrade refused" records.
func refusedLines(t *testing.T, buf *lockedBuf) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if rec["msg"] == "socket upgrade refused" {
			out = append(out, rec)
		}
	}
	return out
}

func TestARefusedUpgradeLeavesOneLineWithItsReason(t *testing.T) {
	other, _, err := store.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		setup  func(*stubs)
		protos func(*stubs) []string
		status int
		reason string
	}{
		{"no token", nil, func(*stubs) []string { return []string{subprotocolV1} },
			401, "no token subprotocol offered"},
		{"unknown token", nil, func(*stubs) []string { return []string{subprotocolV1, tokenProto(other)} },
			401, "credential refused"},
		{"malformed token", nil, func(*stubs) []string { return []string{subprotocolV1, tokenProto("not-a-token")} },
			400, "token subprotocol malformed"},
		{"two tokens", nil, func(s *stubs) []string {
			return []string{subprotocolV1, tokenProto(s.token), tokenProto(other)}
		}, 400, "several token subprotocols"},
		{"no catenary.v1", nil, func(s *stubs) []string { return []string{tokenProto(s.token)} },
			400, "catenary.v1 subprotocol not offered"},
		{"bot", func(s *stubs) { s.caller = store.Caller{UserID: uuid.New(), Kind: "bot"} },
			func(s *stubs) []string { return []string{subprotocolV1, tokenProto(s.token)} },
			401, "bot has no device to bind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubs(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			buf := &lockedBuf{}
			d := s.deps()
			d.Logger = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			srv := serve(t, d)

			if _, _, err := dial(t, srv, tc.protos(s)...); err == nil {
				t.Fatal("the upgrade was accepted")
			}
			lines := refusedLines(t, buf)
			if len(lines) != 1 {
				t.Fatalf("want exactly one refusal line, got %d in:\n%s", len(lines), buf.String())
			}
			if lines[0]["reason"] != tc.reason || lines[0]["status"] != float64(tc.status) {
				t.Errorf("line = %v, want reason %q status %d", lines[0], tc.reason, tc.status)
			}
			for _, secret := range []string{s.token, other} {
				if strings.Contains(buf.String(), secret) {
					t.Errorf("the log carries a token:\n%s", buf.String())
				}
			}
		})
	}
}

// A request the websocket library refuses itself (here: a cross-origin
// browser upgrade) is logged with the caller, the origin and the reason.
func TestAnUpgradeTheLibraryRefusesIsLogged(t *testing.T) {
	s := newStubs(t)
	buf := &lockedBuf{}
	d := s.deps()
	d.Logger = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := serve(t, d)

	hdr := http.Header{}
	hdr.Set("Origin", "https://elsewhere.example")
	_, resp, err := websocket.Dial(testCtx(t), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws",
		&websocket.DialOptions{Subprotocols: []string{subprotocolV1, tokenProto(s.token)}, HTTPHeader: hdr})
	if err == nil {
		t.Fatal("a cross-origin upgrade was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
	lines := refusedLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want one refusal line, got %d in:\n%s", len(lines), buf.String())
	}
	l := lines[0]
	if l["reason"] != "upgrade failed" || l["origin"] != "https://elsewhere.example" ||
		l["device_id"] != s.caller.DeviceID.String() || l["user_id"] != s.caller.UserID.String() {
		t.Errorf("line = %v", l)
	}
	if strings.Contains(buf.String(), s.token) {
		t.Errorf("the log carries the token:\n%s", buf.String())
	}
}
