// Command r1client is the R1 (IDEA-24) test client.
//
// It exists to be left running for an hour through a real Cloudflare tunnel,
// and to be killed rudely in the middle of a message stream.
//
// Two behaviours are under test, and they fail differently:
//
//   - Cloudflare closes an IDLE socket at ~100 s. Frames must keep flowing.
//   - A mobile network drops TCP without a FIN, leaving BOTH ENDS believing
//     the socket is open. That one is silent: the client shows no error,
//     receives nothing, and looks exactly like a quiet conversation. Only an
//     unanswered application-level ping surfaces it.
//
// So: ping every heartbeat_interval_sec (the server tells us, per R4 — it is
// not a client constant), and after missed_pong_limit unanswered pings, SEVER
// DELIBERATELY rather than waiting for the OS to notice.
//
// -no-heartbeat is the control. Without it the experiment shows a socket
// surviving an hour and proves nothing about why.
//
// Every received message is appended to a journal file before it is counted, so
// a `kill -9` cannot lose the evidence of what had arrived. verify compares the
// journal against the server's own log.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/magos/catenary/internal/wire"
)

type journal struct {
	mu sync.Mutex
	f  *os.File
}

func openJournal(path string) (*journal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &journal{f: f}, nil
}

// record writes and fsyncs before the caller counts the message. Ordering
// matters: a journal written after the fact would under-report on a hard kill,
// which is the one moment the journal exists for.
func (j *journal) record(m wire.Message) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := fmt.Fprintf(j.f, "%d\t%s\n", m.LogSeq, m.ID); err != nil {
		return err
	}
	return j.f.Sync()
}

// cursorFile persists the resume point. It is deliberately separate from the
// journal: the journal is the evidence, this is the protocol state.
func readCursor(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var v int64
	_, _ = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &v)
	return v
}

func writeCursor(path string, v int64) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", v)), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func main() {
	base := flag.String("url", "", "base URL, e.g. https://x.trycloudflare.com")
	token := flag.String("token", "", "bearer token")
	dir := flag.String("dir", ".", "where to keep the journal and cursor")
	name := flag.String("name", "hb", "label for this client's files and logs")
	noHB := flag.Bool("no-heartbeat", false, "CONTROL: never ping, never sever. Shows what the tunnel does to an idle socket.")
	deafAfter := flag.Int("deaf-after", 0, "ask the server to stop answering pings after N seconds, to exercise the missed-pong severance path")
	statusEvery := flag.Duration("status-every", 5*time.Minute, "how often to print a liveness line")
	runFor := flag.Duration("run-for", 0, "exit cleanly after this long (0 = until signalled)")
	flag.Parse()

	if *base == "" || *token == "" {
		log.Fatal("r1client: -url and -token are required")
	}

	journalPath := fmt.Sprintf("%s/%s.journal", *dir, *name)
	cursorPath := fmt.Sprintf("%s/%s.cursor", *dir, *name)

	j, err := openJournal(journalPath)
	if err != nil {
		log.Fatalf("journal: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *runFor > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *runFor)
		defer cancel()
	}

	wsURL := strings.Replace(strings.Replace(*base, "https://", "wss://", 1), "http://", "ws://", 1) + "/ws"
	if *noHB {
		// Ask the server to stay quiet too. A control that still receives a
		// server ping every 35 s is not an idle socket, and would "survive" the
		// edge timeout for reasons that have nothing to do with the client.
		wsURL += "?silent=1"
	}
	if *deafAfter > 0 {
		// Ask the server to go deaf mid-connection, so the missed-pong
		// severance path is exercised rather than assumed.
		wsURL += fmt.Sprintf("%sdeaf_after=%d&silent=1", map[bool]string{true: "&", false: "?"}[*noHB], *deafAfter)
	}

	started := time.Now()
	var reconnects, received int
	var longest time.Duration

	for attempt := 0; ctx.Err() == nil; attempt++ {
		if attempt > 0 {
			reconnects++
			// Bounded backoff. Reconnecting is safe at any time because the
			// cursor makes it a query, not a guess.
			backoff := time.Duration(min(attempt, 6)) * 2 * time.Second
			log.Printf("[%s] reconnect #%d in %s", *name, reconnects, backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if ctx.Err() != nil {
				break
			}
		}

		connStart := time.Now()
		n, err := runOnce(ctx, wsURL, *token, cursorPath, j, *noHB, *statusEvery, *name, &received)
		up := time.Since(connStart)
		if up > longest {
			longest = up
		}
		log.Printf("[%s] socket closed after %s (%d messages this connection): %v",
			*name, up.Round(time.Second), n, err)
	}

	log.Printf("[%s] DONE total=%s reconnects=%d received=%d longest_single_socket=%s",
		*name, time.Since(started).Round(time.Second), reconnects, received, longest.Round(time.Second))
}

func runOnce(
	ctx context.Context, wsURL, token, cursorPath string, j *journal,
	noHB bool, statusEvery time.Duration, name string, received *int,
) (int, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		return 0, fmt.Errorf("dial: %w", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(8 << 20)

	cursor := readCursor(cursorPath)

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

	resume := cursor
	hello := wire.ClientHello{
		WireVersion:      wire.WireVersion,
		DeviceID:         "6f1a2c3d-4e5b-4a7c-9d8e-0f1a2b3c4d5e",
		ResumeFromLogSeq: &resume,
		ClientInfo:       ptr("r1client/" + name),
	}
	if err := send(hello); err != nil {
		return 0, fmt.Errorf("hello: %w", err)
	}

	// Outstanding pings, by id. Matching by id rather than by arrival order is
	// what makes "two missed pongs" a countable thing.
	var pingMu sync.Mutex
	outstanding := map[string]time.Time{}
	var lastRTT time.Duration

	type frameOrErr struct {
		f   wire.ServerFrame
		err error
	}
	frames := make(chan frameOrErr, 64)
	go func() {
		defer close(frames)
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				frames <- frameOrErr{err: err}
				return
			}
			f, err := wire.DecodeServerFrame(data)
			if err != nil {
				log.Printf("[%s] undecodable frame: %v", name, err)
				continue
			}
			if f == nil {
				continue // unknown tag: ignore and carry on
			}
			frames <- frameOrErr{f: f}
		}
	}()

	hbInterval := 35 * time.Second
	missedLimit := 2
	count := 0
	connStart := time.Now()

	var hbTicker *time.Ticker
	var hbC <-chan time.Time
	statusTicker := time.NewTicker(statusEvery)
	defer statusTicker.Stop()
	defer func() {
		if hbTicker != nil {
			hbTicker.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			_ = c.Close(websocket.StatusNormalClosure, "done")
			return count, ctx.Err()

		case <-statusTicker.C:
			pingMu.Lock()
			out := len(outstanding)
			rtt := lastRTT
			pingMu.Unlock()
			log.Printf("[%s] alive %s · msgs=%d · cursor=%d · outstanding_pings=%d · last_rtt=%s",
				name, time.Since(connStart).Round(time.Second), count, cursor, out, rtt.Round(time.Millisecond))

		case <-hbC:
			pingMu.Lock()
			if len(outstanding) >= missedLimit {
				n := len(outstanding)
				pingMu.Unlock()
				// This is the whole point. The socket looks fine and is not.
				_ = c.Close(websocket.StatusGoingAway, "missed pongs")
				return count, fmt.Errorf("severed: %d unanswered pings (limit %d)", n, missedLimit)
			}
			id := fmt.Sprintf("c-%s-%d", name, time.Now().UnixMilli())
			outstanding[id] = time.Now()
			pingMu.Unlock()
			if err := send(wire.Ping{ID: id, At: ptr(nowStamp())}); err != nil {
				return count, fmt.Errorf("ping write: %w", err)
			}

		case fe, ok := <-frames:
			if !ok {
				return count, fmt.Errorf("reader stopped")
			}
			if fe.err != nil {
				return count, fmt.Errorf("read: %w", fe.err)
			}
			switch v := fe.f.(type) {
			case wire.ServerReady:
				hbInterval = time.Duration(v.HeartbeatIntervalSec) * time.Second
				missedLimit = int(v.MissedPongLimit)
				log.Printf("[%s] ready · resumed=%v · server_head=%d · our_cursor=%d · heartbeat=%s · missed_limit=%d",
					name, v.Resumed, v.LogSeq, cursor, hbInterval, missedLimit)
				if !noHB {
					hbTicker = time.NewTicker(hbInterval)
					hbC = hbTicker.C
				} else {
					log.Printf("[%s] CONTROL: heartbeat disabled; this socket is expected to die at the edge idle timeout", name)
				}

			case wire.Ping:
				// Server-initiated half of the heartbeat.
				if err := send(wire.Pong{ID: v.ID, At: ptr(nowStamp())}); err != nil {
					return count, fmt.Errorf("pong write: %w", err)
				}

			case wire.Pong:
				pingMu.Lock()
				if t, ok := outstanding[v.ID]; ok {
					lastRTT = time.Since(t)
					delete(outstanding, v.ID)
				}
				pingMu.Unlock()

			case wire.ServerMessageFrame:
				m := v.Message
				if m.LogSeq <= cursor {
					// Already have it. A duplicate delivery is DETECTABLE rather
					// than a design flaw, which is the property that makes
					// at-least-once delivery safe to build on.
					log.Printf("[%s] duplicate suppressed log_seq=%d", name, m.LogSeq)
					continue
				}
				if err := j.record(m); err != nil {
					return count, fmt.Errorf("journal: %w", err)
				}
				cursor = m.LogSeq
				writeCursor(cursorPath, cursor)
				count++
				*received++

			case wire.ServerResyncRequired:
				log.Printf("[%s] resync required (%s); server head=%d", name, v.Reason, v.LogSeq)

			case wire.ServerError:
				log.Printf("[%s] server error %s: %s (retryable=%v)", name, v.Code, v.Message, v.Retryable)
			}
		}
	}
}

func nowStamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func ptr[T any](v T) *T { return &v }
