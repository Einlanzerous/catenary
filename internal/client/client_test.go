package client

// The hello is the first frame on every socket, whatever the caller is doing.
// The server closes a session whose first known frame is anything else with
// 1008 (internal/api's awaitHello), so a Send that raced a reconnect would
// sever a fresh session — and CANT-27's harness sends on a loop while sessions
// drop and redial. PR #51's review found the window: the socket was published
// to Send before the hello was written.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

func TestNoFrameCanPrecedeTheHello(t *testing.T) {
	accepted := make(chan struct{})
	first := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.Error(w, "no /sync here", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{subprotocolV1}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		close(accepted)
		_, data, err := conn.Read(context.Background())
		if err != nil {
			first <- "read error: " + err.Error()
			return
		}
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &probe)
		first <- probe.Type
		_, _, _ = conn.Read(context.Background()) // hold the socket until the client goes
	}))
	t.Cleanup(srv.Close)

	j := NewJournal()
	c, err := New(Config{BaseURL: srv.URL, AccessToken: "token", DeviceID: wire.Uuid(uuid.NewString()), Journal: j})
	if err != nil {
		t.Fatal(err)
	}

	// HOLD THE JOURNAL: the session dials, then cannot read the cursor its
	// hello carries. It is parked exactly between the upgrade and the hello.
	j.mu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			j.mu.Unlock()
		}
	}
	t.Cleanup(unlock)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(context.Background())
	}()
	t.Cleanup(func() {
		unlock()
		c.Kill()
		<-done
	})

	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the client never upgraded")
	}

	// Every Send in this window must find no socket. One that finds it writes
	// a `send` frame before the hello.
	send := wire.ClientSend{ClientID: wire.Uuid(uuid.NewString()), ConversationID: wire.Uuid(uuid.NewString())}
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := c.Send(ctx, send)
		cancel()
		if !errors.Is(err, ErrNotConnected) {
			t.Fatalf("Send between the upgrade and the hello = %v, want ErrNotConnected", err)
		}
	}

	unlock()
	select {
	case got := <-first:
		if got != "hello" {
			t.Errorf("the server's first frame is %q, want hello", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no frame reached the server")
	}
}
