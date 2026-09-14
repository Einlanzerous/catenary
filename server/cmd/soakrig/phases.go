package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// jitter spreads N clients' sends instead of a thundering herd every tick:
// +/- 20% of d, uniformly. A non-positive d is returned unchanged.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	n := int64(d)
	delta := n / 5
	if delta <= 0 {
		return d
	}
	return time.Duration(n - delta + rand.Int64N(2*delta+1))
}

// steadyTraffic is phase one: every ready client sends into the shared room
// on its own jittered ticker for cfg.SteadyDuration, then the phase waits for
// everyone to catch up. A send error here — with no chaos running — is
// recorded but does not itself fail the harness; the final Compare is what
// decides whether anything was actually lost.
func (h *harness) steadyTraffic(ctx context.Context, clients []*soakClient, room uuid.UUID) PhaseReport {
	start := time.Now()
	var sent, acked, errs int64

	pctx, cancel := context.WithTimeout(ctx, h.cfg.SteadyDuration)
	defer cancel()

	var wg sync.WaitGroup
	for _, sc := range clients {
		wg.Add(1)
		go func(sc *soakClient) {
			defer wg.Done()
			for {
				select {
				case <-pctx.Done():
					return
				case <-time.After(jitter(h.cfg.SendInterval)):
				}
				if !sc.c.Status().Ready {
					continue
				}
				atomic.AddInt64(&sent, 1)
				sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
				text := fmt.Sprintf("steady traffic from %s", sc.name)
				_, err := sc.c.Send(sctx, wire.ClientSend{ClientID: wid(uuid.New()), ConversationID: wid(room), Text: &text})
				scancel()
				if err != nil {
					atomic.AddInt64(&errs, 1)
					continue
				}
				atomic.AddInt64(&acked, 1)
			}
		}(sc)
	}
	wg.Wait()

	h.awaitAllCaughtUp(ctx, clients, "steady traffic")
	return PhaseReport{Name: "steady traffic", Duration: time.Since(start),
		MessagesSent: int(sent), MessagesAcked: int(acked), SendErrors: int(errs)}
}

// reconnectStorm is phase two: cfg.StormRounds rounds of severing EVERY
// client's socket at once — client.Sever, CANT-27's addition to
// internal/client — then waiting for the whole cohort to reconnect and catch
// up before the next round. The server never restarts here; only the network
// does, cfg.StormRounds times.
func (h *harness) reconnectStorm(ctx context.Context, clients []*soakClient) PhaseReport {
	start := time.Now()

	before := make(map[int]int, len(clients))
	for _, sc := range clients {
		before[sc.index] = sc.c.Status().Dials
	}

	for round := 1; round <= h.cfg.StormRounds; round++ {
		var wg sync.WaitGroup
		for _, sc := range clients {
			wg.Add(1)
			go func(sc *soakClient) { defer wg.Done(); sc.c.Sever() }(sc)
		}
		wg.Wait()
		h.awaitAllCaughtUp(ctx, clients, fmt.Sprintf("reconnect storm round %d/%d", round, h.cfg.StormRounds))
	}

	reconnects := 0
	for _, sc := range clients {
		reconnects += sc.c.Status().Dials - before[sc.index]
	}
	return PhaseReport{Name: "reconnect storm", Duration: time.Since(start), Reconnects: reconnects}
}

// killAndRestart is phase three: a real `kill -9` of the server subprocess
// while clients keep trying to send (most fail — there is no socket, which is
// the point), KillMessages committed DIRECTLY through this process's own
// store connection while the server is down (R1's "something to lose" — the
// messages the sockets could not carry), a restart on the same port, and a
// wait for every client to reconnect and catch up.
func (h *harness) killAndRestart(ctx context.Context, clients []*soakClient, room uuid.UUID) (PhaseReport, int) {
	start := time.Now()

	stopBg := make(chan struct{})
	var bgSent, bgErrs int64
	var wg sync.WaitGroup
	for _, sc := range clients {
		wg.Add(1)
		go func(sc *soakClient) {
			defer wg.Done()
			for {
				select {
				case <-stopBg:
					return
				case <-time.After(jitter(h.cfg.SendInterval)):
				}
				atomic.AddInt64(&bgSent, 1)
				sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				text := "sent during the kill"
				_, err := sc.c.Send(sctx, wire.ClientSend{ClientID: wid(uuid.New()), ConversationID: wid(room), Text: &text})
				cancel()
				if err != nil {
					atomic.AddInt64(&bgErrs, 1)
				}
			}
		}(sc)
	}

	h.killServer()

	for i := 0; i < h.cfg.KillMessages; i++ {
		text := fmt.Sprintf("committed while the server was dead, %d", i)
		author := clients[i%len(clients)].userID
		if _, err := h.store.SendMessage(ctx, store.NewMessage{
			ClientID: uuid.New(), ConversationID: room, AuthorID: author, Text: &text,
		}); err != nil {
			h.harnessError("commit a message while the server is dead: %v", err)
		}
	}

	missing := h.snapshotLostCount(ctx, clients)

	if err := h.startServer(ctx); err != nil {
		h.harnessError("restart the server after kill -9: %v", err)
	}

	close(stopBg)
	wg.Wait()

	// GENEROUSLY BOUNDED, NOT cfg.AwaitTimeout: this is the wait compareAll's
	// fairness depends on. See awaitFinalSettle's own comment.
	h.awaitFinalSettle(ctx, clients)

	return PhaseReport{Name: "kill -9 and restart", Duration: time.Since(start),
		MessagesSent: int(bgSent), SendErrors: int(bgErrs)}, missing
}

// snapshotLostCount is R1's control, taken mid-phase: the messages committed
// while the server was down that the OWN database connection (independent of
// the dead subprocess) can already see, and that no client has yet held.
// Zero here would mean the kill landed on a quiet log and proved nothing —
// the counter-proof this ticket's own text asks for is that this number is
// NOT always zero.
func (h *harness) snapshotLostCount(ctx context.Context, clients []*soakClient) int {
	total := 0
	for _, sc := range clients {
		rows, err := h.serverLogFor(ctx, sc.userID)
		if err != nil {
			continue // best-effort evidence; the final compareAll is the claim of record
		}
		total += len(client.Compare(rows, sc.c.Snapshot()).Lost)
	}
	return total
}

// serverLogFor is the server's own committed record for one viewer: every
// message in every conversation they belong to. Queried directly — `messages`
// and `conversation_members` are not credential tables — the same shape
// cmd/catenary's own killrig_test.go uses for the in-process kill test.
func (h *harness) serverLogFor(ctx context.Context, viewer uuid.UUID) ([]client.LogEntry, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT m.id, m.conversation_id, m.seq, m.log_seq
		  FROM messages m
		  JOIN conversation_members cm
		    ON cm.conversation_id = m.conversation_id AND cm.user_id = $1
		 ORDER BY m.log_seq`, viewer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []client.LogEntry
	for rows.Next() {
		var id, conv uuid.UUID
		var e client.LogEntry
		if err := rows.Scan(&id, &conv, &e.Seq, &e.LogSeq); err != nil {
			return nil, err
		}
		e.ID, e.ConversationID = wid(id), wid(conv)
		out = append(out, e)
	}
	return out, rows.Err()
}
