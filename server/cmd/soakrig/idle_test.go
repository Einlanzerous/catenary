package main

// CANT-109 — `soakrig idle` against a real local server: passes a short,
// clean run; fails with the close status named when the socket is severed;
// fails on any reconnect even when it then succeeds; and names an expired
// access token as the cause of a failed reconnect rather than a bare
// "unauthorized" — all deterministic, over loopback, no real-time waiting.
//
// TWO SMALL TCP PROXIES stand in for what a real deployment's tunnel does to
// an idle connection: severingProxy cuts an established connection with no
// close frame (the network-level event Cloudflare/Traefik actually produce,
// not a clean WebSocket close), and flakyStartProxy refuses the first few
// connection attempts outright, forcing a real reconnect before the client
// ever reaches ready even once. Both dial straight through to the SAME real
// local `catenary serve` subprocess startLocalServer already started.
//
// Gated on CATENARY_TEST_DATABASE_URL, the same convention broken_test.go
// uses.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// severingProxy is a plain TCP pass-through the test dials instead of the
// real server, so it can cut every connection it is currently carrying on
// command — an abrupt network cut, the same shape a real idle timeout in
// front of the service produces, and deterministic rather than waited for.
type severingProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newSeveringProxy(t *testing.T, target string) *severingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &severingProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.accept()
	t.Cleanup(func() { _ = ln.Close(); p.severAll() })
	return p
}

func (p *severingProxy) base() string { return "http://" + p.ln.Addr().String() }

func (p *severingProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c], p.conns[up] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go func() { _, _ = io.Copy(up, c); _ = up.Close(); _ = c.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = c.Close(); _ = up.Close() }()
	}
}

// severAll closes every connection currently proxied — no close frame, an
// abrupt cut — without stopping the listener: a NEW connection afterward is
// still accepted and proxied normally, exactly as a real reconnect through a
// tunnel would be.
func (p *severingProxy) severAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

// waitForProxyConnection blocks until at least one TCP connection has been
// proxied through — proof the client's dial reached the real server, so a
// caller that then mutates state (e.g. backdating a token) knows it happens
// AFTER the dial already in flight, never before it.
func waitForProxyConnection(t *testing.T, p *severingProxy) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.conns)
		p.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the client never established a connection through the proxy")
}

// flakyStartProxy refuses the first refuseFirst TCP connections outright —
// closed before a single byte is read, the shape of a cold edge refusing a
// connection — then passes every one after that straight through.
type flakyStartProxy struct {
	ln     net.Listener
	target string
	refuse atomic.Int64
}

func newFlakyStartProxy(t *testing.T, target string, refuseFirst int) *flakyStartProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &flakyStartProxy{ln: ln, target: target}
	p.refuse.Store(int64(refuseFirst))
	go p.accept()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *flakyStartProxy) base() string { return "http://" + p.ln.Addr().String() }

func (p *flakyStartProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if n := p.refuse.Add(-1); n >= 0 {
			_ = c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		go func() { _, _ = io.Copy(up, c); _ = up.Close(); _ = c.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = c.Close(); _ = up.Close() }()
	}
}

// targetHost strips the http:// a harness base URL carries, for a proxy
// that dials the raw TCP address.
func targetHost(baseURL string) string { return strings.TrimPrefix(baseURL, "http://") }

func TestIdleAgainstALocalServer(t *testing.T) {
	dbURL := soakDBFixture(t)
	h := startLocalServer(t, dbURL)
	logger := slog.New(slog.DiscardHandler)

	provisionFor := func(t *testing.T, handle string) Credentials {
		t.Helper()
		cred, err := runProvision(context.Background(), dbURL, h.baseURL, handle, handle, filepath.Join(t.TempDir(), "creds.json"), nil, logger)
		if err != nil {
			t.Fatalf("provision %s: %v", handle, err)
		}
		return cred
	}

	t.Run("passes a short clean run", func(t *testing.T) {
		cred := provisionFor(t, "idle-clean-run")
		rep := runIdle(context.Background(), h.baseURL, cred, nil, "cant-109-test", 2*time.Second, logger)
		if !rep.Pass {
			t.Fatalf("idle did not pass a clean short run: %+v", rep)
		}
		if rep.Dials != 1 || rep.Readys != 1 {
			t.Errorf("dials=%d readys=%d, want exactly 1 each for a clean run", rep.Dials, rep.Readys)
		}
		if len(rep.CloseStatuses) != 0 {
			t.Errorf("close statuses = %v, want none", rep.CloseStatuses)
		}
	})

	t.Run("fails with the close status named when the server severs the socket", func(t *testing.T) {
		cred := provisionFor(t, "idle-severed")
		proxy := newSeveringProxy(t, targetHost(h.baseURL))

		go func() {
			time.Sleep(500 * time.Millisecond)
			proxy.severAll()
		}()

		rep := runIdle(context.Background(), proxy.base(), cred, nil, "cant-109-test", 4*time.Second, logger)
		if rep.Pass {
			t.Fatalf("idle reported PASS after the socket was severed: %+v", rep)
		}
		if len(rep.CloseStatuses) == 0 {
			t.Fatalf("no close status recorded: %+v", rep)
		}
		if !strings.Contains(rep.Reason, "did not survive") {
			t.Errorf("reason does not name the close: %q", rep.Reason)
		}
		// A TCP-level sever (no WebSocket close frame at all) is the -1
		// bucket, named closeCodeName(-1) — a hard assertion, since the
		// PR's own evidence table cites this exact string as proof.
		if !strings.Contains(rep.Reason, "no-close-frame(-1)") {
			t.Errorf("reason does not name the close status as no-close-frame(-1): %q", rep.Reason)
		}
	})

	t.Run("fails on any reconnect even when the socket then survives", func(t *testing.T) {
		cred := provisionFor(t, "idle-flaky-start")
		proxy := newFlakyStartProxy(t, targetHost(h.baseURL), 2)

		rep := runIdle(context.Background(), proxy.base(), cred, nil, "cant-109-test", 2*time.Second, logger)
		if rep.Pass {
			t.Fatalf("idle reported PASS after more than one dial: %+v", rep)
		}
		if rep.Dials <= 1 {
			t.Fatalf("Dials = %d, want > 1 — this test did not exercise the reconnect path", rep.Dials)
		}
		if !strings.Contains(rep.Reason, "reconnect was attempted") {
			t.Errorf("reason does not name the reconnect: %q", rep.Reason)
		}
	})

	t.Run("names expiry rather than a bare unauthorized on a reconnect after expiry", func(t *testing.T) {
		cred := provisionFor(t, "idle-expired")
		proxy := newSeveringProxy(t, targetHost(h.baseURL))

		// cred.AccessExpiresAt is set to the past RIGHT NOW, even though the
		// server's own row is not backdated until below — this field is
		// purely local bookkeeping runIdle reads at the very end to decide
		// what to call a failed reconnect, and it plays no part in the
		// server's own decision to accept or refuse a request. Only the
		// database row below governs whether the FIRST connect (which must
		// still succeed) or the reconnect (which must not) is authenticated.
		past := time.Now().Add(-1 * time.Hour)
		cred.AccessExpiresAt = past

		resultCh := make(chan idleReport, 1)
		go func() {
			resultCh <- runIdle(context.Background(), proxy.base(), cred, nil, "cant-109-test", 6*time.Second, logger)
		}()

		waitForProxyConnection(t, proxy)
		time.Sleep(300 * time.Millisecond) // let the handshake that used the still-valid server row finish

		// NOW really expire the token server-side, so the reconnect this
		// test forces next gets a GENUINE 401 — the same refusal
		// Authenticate gives any request past access_tokens.expires_at.
		ct, err := h.pool.Exec(context.Background(),
			`UPDATE access_tokens SET expires_at = $1 WHERE device_id = $2`, past, asUUID(t, cred.DeviceID))
		if err != nil {
			t.Fatalf("backdate the access token: %v", err)
		}
		if ct.RowsAffected() != 1 {
			t.Fatalf("backdated %d rows, want exactly 1", ct.RowsAffected())
		}
		proxy.severAll()

		var rep idleReport
		select {
		case rep = <-resultCh:
		case <-time.After(10 * time.Second):
			t.Fatal("runIdle never returned")
		}
		if rep.Pass {
			t.Fatalf("idle reported PASS after the token expired mid-run: %+v", rep)
		}
		if !rep.TokenExpired {
			t.Fatalf("TokenExpired = false, want true: %+v", rep)
		}
		if !strings.Contains(rep.Reason, "expired") {
			t.Errorf("reason does not name expiry: %q", rep.Reason)
		}
	})

	t.Run("reports the raw reason when a reconnect fails for another reason", func(t *testing.T) {
		cred := provisionFor(t, "idle-revoked")
		proxy := newSeveringProxy(t, targetHost(h.baseURL))

		// The FIRST connect must succeed (the device is not yet revoked) —
		// only the reconnect this test forces should hit the revocation —
		// so runIdle starts in the background and revocation happens only
		// after a connection through the proxy is observed.
		resultCh := make(chan idleReport, 1)
		go func() {
			resultCh <- runIdle(context.Background(), proxy.base(), cred, nil, "cant-109-test", 6*time.Second, logger)
		}()

		waitForProxyConnection(t, proxy)
		time.Sleep(300 * time.Millisecond)

		if _, err := h.store.RevokeDevice(context.Background(), asUUID(t, cred.DeviceID)); err != nil {
			t.Fatalf("revoke the device: %v", err)
		}
		proxy.severAll()

		var rep idleReport
		select {
		case rep = <-resultCh:
		case <-time.After(10 * time.Second):
			t.Fatal("runIdle never returned")
		}
		if rep.Pass {
			t.Fatalf("idle reported PASS after the device was revoked: %+v", rep)
		}
		if rep.TokenExpired {
			t.Errorf("TokenExpired = true for a revoked (not expired) device: %+v", rep)
		}
		if strings.Contains(rep.Reason, "had expired") {
			t.Errorf("reason wrongly claims expiry for a revocation: %q", rep.Reason)
		}
		if rep.Dials <= 1 {
			t.Errorf("Dials = %d, want > 1 — this test did not exercise the reconnect path", rep.Dials)
		}
	})

	t.Run("the CLI never prints a secret", func(t *testing.T) {
		cred := provisionFor(t, "idle-cli-secret")
		credFile := filepath.Join(t.TempDir(), "creds.json")
		if err := writeCredentials(credFile, cred); err != nil {
			t.Fatal(err)
		}
		const cfID, cfSecret = "unit-test-cf-id", "unit-test-cf-secret-xyz"
		args := []string{
			"-base-url", h.baseURL, "-cred-file", credFile, "-for", "1s", "-quiet",
			"-cf-access-client-id", cfID, "-cf-access-client-secret", cfSecret,
		}
		var code int
		stdout, stderr := captureStdIO(t, func() { code = runIdleCmd(args) })
		if code != exitPass {
			t.Fatalf("runIdleCmd = %d, want %d; stderr=%s", code, exitPass, stderr)
		}
		for name, secret := range map[string]string{
			"access token": cred.AccessToken, "refresh token": cred.RefreshToken, "cf-access-client-secret": cfSecret,
		} {
			if strings.Contains(stdout, secret) {
				t.Errorf("the %s appears on stdout", name)
			}
			if strings.Contains(stderr, secret) {
				t.Errorf("the %s appears on stderr: %q", name, stderr)
			}
		}
	})
}

// asUUID parses a wire.Uuid (a plain string alias) back into a uuid.UUID for
// a direct store/pool call — the tests here are the one place in this
// package that reach past the wire boundary to check the database's own
// state.
func asUUID(t *testing.T, id string) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parse %q as a uuid: %v", id, err)
	}
	return u
}
