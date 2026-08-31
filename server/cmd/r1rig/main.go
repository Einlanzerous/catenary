// Command r1rig is the R1 (IDEA-24) test server: the smallest thing that can
// answer "does a WebSocket survive the Cloudflare tunnel, and does a forced
// kill resume with zero loss and zero duplication?"
//
// It is a SPIKE RIG, not P1 code. State is in memory, there is no auth beyond a
// shared bearer token, and it is meant to be run behind an ephemeral tunnel and
// then thrown away. What it does share with the real thing is the wire
// protocol: every frame is a generated type from schema/catenary.wire.v1.schema.json,
// so the rig cannot accidentally prove a protocol nobody is going to build.
//
// The append-only log with a server-assigned monotonic ordinal is the whole
// design under test. Resume-from-cursor is what makes reconnection a query
// rather than a guess.
//
//	go run ./cmd/r1rig -addr 127.0.0.1:8099 -token <secret>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/magos/catenary/server/internal/wire"
)

const (
	convID   = "aa11bb22-cc33-4d44-9e55-ff66aa77bb88"
	authorID = "8d9e0f1a-2b3c-4d4e-9f5a-6b7c8d9e0f1a"
)

// logStore is the append-only message log. Per IDEA-23 this is the core idea:
// not a chat app, a log with a sync protocol on top.
type logStore struct {
	mu       sync.RWMutex
	msgs     []wire.Message // ordered by LogSeq, which is also the index+1
	seq      int64          // per-conversation ordinal (one conversation in the rig)
	logSeq   int64          // account-global cursor
	byClient map[string]wire.ServerAck
	subs     map[chan wire.Message]struct{}
}

func newLogStore() *logStore {
	return &logStore{
		byClient: map[string]wire.ServerAck{},
		subs:     map[chan wire.Message]struct{}{},
	}
}

func nowStamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// append adds a message, or replays the original ack when clientID has been
// seen before. Idempotency is the property that makes a retry after an
// ambiguous failure free, which is what an offline outbox is built on.
func (s *logStore) append(clientID, text string) (wire.ServerAck, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ack, ok := s.byClient[clientID]; ok {
		dup := true
		ack.Duplicate = &dup
		return ack, true
	}

	s.seq++
	s.logSeq++
	cid := clientID
	m := wire.Message{
		ID:             fmt.Sprintf("%08x-0000-4000-8000-%012x", s.logSeq, s.logSeq),
		Seq:            s.seq,
		LogSeq:         s.logSeq,
		ConversationID: convID,
		AuthorID:       authorID,
		At:             nowStamp(),
		Text:           &text,
		State:          wire.DeliveryStateSent,
		ClientID:       &cid,
	}
	s.msgs = append(s.msgs, m)

	ack := wire.ServerAck{
		ClientID:       clientID,
		MessageID:      m.ID,
		ConversationID: convID,
		Seq:            m.Seq,
		LogSeq:         m.LogSeq,
		At:             m.At,
	}
	s.byClient[clientID] = ack

	for ch := range s.subs {
		select {
		case ch <- m:
		default: // slow subscriber: it will catch up over /sync, which is the point
		}
	}
	return ack, false
}

func (s *logStore) after(cursor int64, limit int) ([]wire.Message, int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []wire.Message{}
	for _, m := range s.msgs {
		if m.LogSeq > cursor {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	next := cursor
	if len(out) > 0 {
		next = out[len(out)-1].LogSeq
	}
	return out, next, next < s.head()
}

func (s *logStore) head() int64 {
	if len(s.msgs) == 0 {
		return 0
	}
	return s.msgs[len(s.msgs)-1].LogSeq
}

func (s *logStore) headLocked() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.head()
}

func (s *logStore) subscribe() (chan wire.Message, func()) {
	ch := make(chan wire.Message, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
		close(ch)
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address (bind loopback; the tunnel is the only way in)")
	token := flag.String("token", "", "shared bearer token, required")
	hb := flag.Int("heartbeat", 35, "heartbeat_interval_sec advertised to clients")
	missed := flag.Int("missed-pong-limit", 2, "missed_pong_limit advertised to clients")
	flag.Parse()

	if *token == "" {
		log.Fatal("r1rig: -token is required; this rig gets a public hostname and must not be open")
	}

	store := newLogStore()
	var connCount, liveConns int64
	var statMu sync.Mutex

	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+*token && r.URL.Query().Get("t") != *token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		statMu.Lock()
		defer statMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "head": store.headLocked(),
			"connections_total": connCount, "connections_live": liveConns,
			"server_time": nowStamp(),
		})
	})

	// /publish injects a message. The driver uses it to produce traffic across a
	// client's kill window, which is how the zero-loss claim gets something to
	// actually lose.
	mux.HandleFunc("/publish", authed(func(w http.ResponseWriter, r *http.Request) {
		clientID := r.URL.Query().Get("client_id")
		text := r.URL.Query().Get("text")
		if clientID == "" {
			http.Error(w, "client_id required", http.StatusBadRequest)
			return
		}
		ack, dup := store.append(clientID, text)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ack": ack, "duplicate": dup})
	}))

	// /sync is the reconnect and catch-up path, not the steady state.
	mux.HandleFunc("/sync", authed(func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit := 500
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
			limit = l
		}
		msgs, next, more := store.after(after, limit)
		head := store.headLocked()
		convs := []wire.Conversation{}
		if head > 0 {
			convs = append(convs, wire.Conversation{
				ID: convID, Kind: wire.ConversationKindGroup, Name: "R1 rig",
				MemberCount: 2, HeadSeq: head,
			})
		}
		// `users` is required: without it there is no path from a message's
		// author_id to a display name. The rig has exactly one author.
		users := []wire.User{{ID: authorID, Name: "R1 rig", Initials: ptr("R1")}}
		resp := wire.SyncResponse{
			LogSeq: next, Messages: msgs, Conversations: convs, Users: users,
			HasMore: more, ServerTime: nowStamp(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	mux.HandleFunc("/ws", authed(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		defer c.CloseNow()

		statMu.Lock()
		connCount++
		liveConns++
		id := connCount
		statMu.Unlock()
		defer func() {
			statMu.Lock()
			liveConns--
			statMu.Unlock()
		}()

		ctx := r.Context()
		log.Printf("conn %d: open from %s", id, r.Header.Get("Cf-Connecting-Ip"))

		// --- hello ---
		_, first, err := c.Read(ctx)
		if err != nil {
			log.Printf("conn %d: no hello: %v", id, err)
			return
		}
		cf, err := wire.DecodeClientFrame(first)
		if err != nil || cf == nil {
			log.Printf("conn %d: bad hello: %v", id, err)
			return
		}
		hello, ok := cf.(wire.ClientHello)
		if !ok {
			log.Printf("conn %d: first frame was %s, not hello", id, cf.WireTag())
			return
		}

		var cursor int64
		if hello.ResumeFromLogSeq != nil {
			cursor = *hello.ResumeFromLogSeq
		}

		sub, unsub := store.subscribe()
		defer unsub()

		var writeMu sync.Mutex
		send := func(v any) error {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			writeMu.Lock()
			defer writeMu.Unlock()
			wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			return c.Write(wctx, websocket.MessageText, b)
		}

		if err := send(wire.ServerReady{
			SessionID:            fmt.Sprintf("%08x-0000-4000-8000-%012x", id, id),
			ServerTime:           nowStamp(),
			HeartbeatIntervalSec: int64(*hb),
			MissedPongLimit:      int64(*missed),
			LogSeq:               store.headLocked(),
			Resumed:              hello.ResumeFromLogSeq != nil,
		}); err != nil {
			return
		}

		// Stream the gap the client missed, then go live. Backlog first, in
		// order, is what makes "everything after N" a complete statement.
		if hello.ResumeFromLogSeq != nil {
			backlog, next, _ := store.after(cursor, 10000)
			for _, m := range backlog {
				if err := send(wire.ServerMessageFrame{Message: m}); err != nil {
					return
				}
			}
			cursor = next
		}

		// ?silent=1 suppresses the server's own pings. It exists for R1's
		// CONTROL client: with the server heartbeating, even a client that
		// never pings has frames arriving every 35 s, so the socket is not
		// idle and the experiment would "pass" without proving anything about
		// the edge timeout. Silent mode is the only way to make a genuinely
		// idle socket through the tunnel.
		silent := r.URL.Query().Get("silent") == "1"
		if silent {
			log.Printf("conn %d: silent mode — server will not ping", id)
		}

		// ?deaf_after=<sec> makes the server stop answering pings after N
		// seconds while holding the TCP connection open.
		//
		// This is the ONLY way to reproduce the failure R1 actually cares about.
		// A mobile network dropping TCP without a FIN leaves both ends believing
		// the socket is fine: the client shows no error, receives nothing, and
		// looks exactly like a quiet conversation. You cannot get there by
		// killing a process — that produces a clean close the client notices
		// immediately. You have to go deaf while staying connected.
		var deafAt time.Time
		if v := r.URL.Query().Get("deaf_after"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil {
				deafAt = time.Now().Add(time.Duration(secs) * time.Second)
				log.Printf("conn %d: will go deaf at %s", id, deafAt.Format(time.TimeOnly))
			}
		}
		isDeaf := func() bool { return !deafAt.IsZero() && time.Now().After(deafAt) }

		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				_, data, err := c.Read(ctx)
				if err != nil {
					log.Printf("conn %d: read end: %v", id, err)
					return
				}
				f, err := wire.DecodeClientFrame(data)
				if err != nil || f == nil {
					continue // unknown tag: ignore and carry on
				}
				switch v := f.(type) {
				case wire.Ping:
					if isDeaf() {
						log.Printf("conn %d: deaf — swallowing ping %s", id, v.ID)
						continue
					}
					// The application-level heartbeat. Answering with the same
					// id is what lets the client count a MISSED pong rather
					// than guess from arrival order.
					_ = send(wire.Pong{ID: v.ID, At: ptr(nowStamp())})
				case wire.Pong:
					// client answering our ping; nothing to do
				}
			}
		}()

		ticker := time.NewTicker(time.Duration(*hb) * time.Second)
		defer ticker.Stop()
		n := 0
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case m := <-sub:
				if m.LogSeq <= cursor {
					continue
				}
				cursor = m.LogSeq
				// The frame carries the WHOLE message. Degrading this into a
				// nudge would double latency on every message in steady state.
				if err := send(wire.ServerMessageFrame{Message: m}); err != nil {
					return
				}
			case <-ticker.C:
				if silent {
					continue
				}
				n++
				if err := send(wire.Ping{ID: fmt.Sprintf("s-%d-%d", id, n), At: ptr(nowStamp())}); err != nil {
					return
				}
			}
		}
	}))

	srv := &http.Server{Addr: *addr, Handler: mux}
	log.Printf("r1rig listening on %s (heartbeat %ds, missed-pong-limit %d)", *addr, *hb, *missed)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func ptr[T any](v T) *T { return &v }
