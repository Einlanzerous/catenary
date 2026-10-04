package main

// CANT-46 (CANT-185) — `read` through cohortClient, against a real server, for
// each client that exists: the `first_unread_seq` the server then serves that
// person has moved, and a read with no ready session is refused with
// client.ErrNotConnected — before the client has started, and behind a held
// partitionProxy. The TypeScript half runs when CATENARY_TS_DRIVER names
// the built driver, as tscohort_test.go's lanes do.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// servedConversation is the Conversation the server last serves one
// credential for id, from the rig's own /sync walk (converge.go).
func servedConversation(ctx context.Context, baseURL string, token wire.Token, id wire.Uuid) (wire.Conversation, bool, error) {
	_, served, err := syncWalk(ctx, baseURL, token)
	c, ok := served[id]
	return c, ok, err
}

func TestReadMovesTheServedMarkerAndIsRefusedWithoutAReadySession(t *testing.T) {
	h := startLocalServer(t, soakDBFixture(t))

	goReader := func(t *testing.T, baseURL string, dev wire.EnrollResponse) cohortClient {
		j := client.NewJournal()
		cred, err := client.CredentialFromEnroll(dev)
		if err == nil {
			err = j.Enroll(cred)
		}
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(client.Config{BaseURL: baseURL, ClientInfo: "cant-185-test", Journal: j, BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	tsReader := func(t *testing.T, baseURL string, dev wire.EnrollResponse) cohortClient {
		cfg := tsDriverConfig{Script: tsDriverScript(t), BaseURL: baseURL, BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax}
		cfg.enrolled(dev)
		d, err := newTSDriver(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	for _, tc := range []struct {
		name   string
		reader func(t *testing.T, baseURL string, dev wire.EnrollResponse) cohortClient
	}{
		{"the Go client", goReader},
		{"the TypeScript driver", tsReader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			// A room, a reader and an author, fresh for this subtest.
			run := uuid.NewString()[:8]
			room, readerID, authorID := uuid.New(), uuid.New(), uuid.New()
			if _, err := h.pool.Exec(ctx, `INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', $2)`, room, "read room "+run); err != nil {
				t.Fatal(err)
			}
			for id, handle := range map[uuid.UUID]string{readerID: "reader-" + run, authorID: "author-" + run} {
				if err := insertUser(ctx, h.pool, id, handle, handle); err != nil {
					t.Fatal(err)
				}
				if _, err := h.pool.Exec(ctx, `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`, room, id); err != nil {
					t.Fatal(err)
				}
			}
			readerDev, err := h.enroll(ctx, 0, readerID, "reader")
			if err != nil {
				t.Fatal(err)
			}
			authorDev, err := h.enroll(ctx, 1, authorID, "author")
			if err != nil {
				t.Fatal(err)
			}

			// The reader's network is a proxy the test can take away.
			proxy := newSeveringProxy(t, targetHost(h.baseURL))
			reader := tc.reader(t, proxy.base(), readerDev)
			author := goReader(t, h.baseURL, authorDev)
			read := wire.ClientRead{ConversationID: wid(room), UpToSeq: 2}

			// BEFORE THE CLIENT HAS STARTED there is no session to write on.
			if err := reader.Read(ctx, read); !errors.Is(err, client.ErrNotConnected) {
				t.Fatalf("a read before the client started = %v, want ErrNotConnected", err)
			}

			runCtx, stop := context.WithCancel(context.Background())
			done := make(chan struct{}, 2)
			for _, c := range []cohortClient{reader, author} {
				go func() { _ = c.Run(runCtx); done <- struct{}{} }()
			}
			t.Cleanup(func() { reader.Kill(); author.Kill(); stop(); <-done; <-done })
			for who, c := range map[string]cohortClient{"reader": reader, "author": author} {
				if err := c.Await(ctx, func() bool { return c.Status().Ready }); err != nil {
					t.Fatalf("the %s never became ready: %v", who, err)
				}
			}

			for i := 1; i <= 3; i++ {
				text := fmt.Sprintf("message %d", i)
				if _, err := author.Send(ctx, wire.ClientSend{ClientID: wid(uuid.New()), ConversationID: wid(room), Text: &text}); err != nil {
					t.Fatalf("send %d: %v", i, err)
				}
			}
			before, ok, err := servedConversation(ctx, h.baseURL, readerDev.AccessToken, wid(room))
			if err != nil || !ok || before.FirstUnreadSeq == nil || *before.FirstUnreadSeq != 1 {
				t.Fatalf("before the read the server serves the reader %+v (served %v, err %v), want first_unread_seq 1", before, ok, err)
			}

			// ON A READY SESSION the frame is written, and the marker the
			// server serves this person moves past what was read.
			if err := reader.Read(ctx, read); err != nil {
				t.Fatalf("a read on a ready session: %v", err)
			}
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				after, _, err := servedConversation(ctx, h.baseURL, readerDev.AccessToken, wid(room))
				if err != nil {
					t.Fatal(err)
				}
				if after.FirstUnreadSeq != nil && *after.FirstUnreadSeq == 3 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("after reading up to seq 2 the server still serves the reader %+v, want first_unread_seq 3", after)
				}
			}

			// BEHIND A HELD PROXY: the open socket is cut and every redial is
			// refused, so the session is not ready and the read
			// is refused rather than dropped.
			proxy.hold()
			if err := reader.Await(ctx, func() bool { return !reader.Status().Ready }); err != nil {
				t.Fatalf("the reader never noticed its network had gone: %v", err)
			}
			if err := reader.Read(ctx, wire.ClientRead{ConversationID: wid(room), UpToSeq: 3}); !errors.Is(err, client.ErrNotConnected) {
				t.Fatalf("a read behind a held proxy = %v, want ErrNotConnected", err)
			}
			if after, _, err := servedConversation(ctx, h.baseURL, readerDev.AccessToken, wid(room)); err != nil || after.FirstUnreadSeq == nil || *after.FirstUnreadSeq != 3 {
				t.Errorf("the refused read moved the marker: the server serves %+v (err %v), want first_unread_seq still 3", after, err)
			}
		})
	}
}
