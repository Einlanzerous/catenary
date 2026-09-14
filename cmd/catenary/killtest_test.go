package main

// CANT-102 — CANT-24's criterion 6. R1's five phases, in-process against the
// real setup() on Postgres, with the Go client in internal/client:
//
//	1. messages before an abrupt close, the last of them committing while
//	   the close happens;
//	2. messages while closed;
//	3. reconnect with the cursor, the /sync issued beside the upgrade;
//	4. a listener gap planted while a /sync is in flight;
//	5. a replay of every client_id.
//
// "The abrupt close" is run twice: the client dies (Kill — no close frame,
// nothing written after death) and the server stops without draining (every
// connection cut, no hub.Shutdown, a new process from setup()).
//
// THE CLAIM IS ABOUT THE INSTRUMENT FIRST. client.Compare says zero lost, zero
// duplicated, zero phantom; TestTheKillTestCatchesABrokenClient runs the same
// five phases with a client broken in one obligation at a time and watches
// that same Compare say otherwise. Phase 4 is built so each of those failures
// is deterministic rather than a race it might win:
//
//	the page is read          the held /sync has served its head (h0)
//	the listener is severed   by the proxy, and held down
//	G commits                 no listener anywhere — missed by construction
//	the listener re-LISTENs   OnGap: resync_required reaches the client
//	L commits                 and arrives live, above G
//	the page is released      the client applies a page that stops below G
//
// A correct client asks again from its cursor (h0, obligations 2 and 3) and
// gets G. One that moved its cursor to L asks from L and never does; one that
// ended catch-up on the held page never asks; one that dedupes by log_seq
// counts L twice when the second page carries it again.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

type killMode string

const (
	clientDies  killMode = "the client dies"
	serverStops killMode = "the server stops without draining"
)

// cast: A is Ada and Theo, B adds Mallory, and C is Ada and Mallory — a room
// Theo is not in, so his view of the log is sparse and a phantom is possible.
type cast struct {
	ada, theo, mal uuid.UUID
	a, b, c        uuid.UUID
}

func (k *killRig) cast() cast {
	ada := mkUser(k.ctx, k.t, k.pool, "ada", "Ada")
	theo := mkUser(k.ctx, k.t, k.pool, "theo", "Theo")
	mal := mkUser(k.ctx, k.t, k.pool, "mallory", "Mallory")
	return cast{ada: ada, theo: theo, mal: mal,
		a: mkGroup(k.ctx, k.t, k.pool, "A", ada, theo),
		b: mkGroup(k.ctx, k.t, k.pool, "B", ada, theo, mal),
		c: mkGroup(k.ctx, k.t, k.pool, "C", ada, mal),
	}
}

// ledger is every idempotency key the run committed, for phase 5.
type ledger struct {
	mu      sync.Mutex
	sockets []socketSend
	stores  []storeSend
}

type socketSend struct {
	who   string
	frame wire.ClientSend
	ack   wire.ServerAck
}

type storeSend struct {
	msg  store.NewMessage
	sent store.Sent
}

// socket sends over a Go client's socket and records the ack.
func (l *ledger) socket(t *testing.T, c *client.Client, who string, conv uuid.UUID, text string) wire.ServerAck {
	t.Helper()
	f := wire.ClientSend{ClientID: wire.Uuid(uuid.NewString()), ConversationID: wid(conv), Text: &text}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ack, err := c.Send(ctx, f)
	if err != nil {
		t.Fatalf("%s's send: %v", who, err)
	}
	if ack.Duplicate != nil {
		t.Fatalf("a first send was acked as a replay: %+v", ack)
	}
	l.mu.Lock()
	l.sockets = append(l.sockets, socketSend{who: who, frame: f, ack: ack})
	l.mu.Unlock()
	return ack
}

// commit is REST's call — store.SendMessage — recorded for the replay.
func (l *ledger) commit(ctx context.Context, st *store.Store, conv, author uuid.UUID, text string) (store.Sent, error) {
	m := store.NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: &text}
	s, err := st.SendMessage(ctx, m)
	if err != nil {
		return store.Sent{}, err
	}
	l.mu.Lock()
	l.stores = append(l.stores, storeSend{msg: m, sent: s})
	l.mu.Unlock()
	return s, nil
}

func (l *ledger) mustCommit(t *testing.T, k *killRig, conv, author uuid.UUID, text string) store.Sent {
	t.Helper()
	s, err := l.commit(k.ctx, k.st, conv, author, text)
	if err != nil {
		t.Fatalf("commit %q: %v", text, err)
	}
	return s
}

// writer commits into A, B and C in a loop — the stream the kill lands in.
type writer struct {
	stop, done chan struct{}
	n          atomic.Int64
	err        atomic.Value
}

func (k *killRig) startWriter(l *ledger, cs cast) *writer {
	w := &writer{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		rooms := []uuid.UUID{cs.a, cs.b, cs.c}
		for i := 0; ; i++ {
			select {
			case <-w.stop:
				return
			default:
			}
			if _, err := l.commit(k.ctx, k.st, rooms[i%3], cs.ada, fmt.Sprintf("mid-stream %d", i)); err != nil {
				w.err.Store(err)
				return
			}
			w.n.Add(1)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return w
}

func (w *writer) awaitCommits(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for w.n.Load() < n {
		if err, _ := w.err.Load().(error); err != nil {
			t.Fatalf("mid-stream writer: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the writer committed %d of %d", w.n.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (w *writer) halt(t *testing.T) {
	t.Helper()
	close(w.stop)
	<-w.done
	if err, _ := w.err.Load().(error); err != nil {
		t.Fatalf("mid-stream writer: %v", err)
	}
}

type killResult struct {
	// Compare at the three points the client reports itself caught up.
	afterReconnect, afterGap, final client.Report
	// gap is the messages Theo can see that committed while the listener was
	// held down; live is the one that arrived live while the /sync was held.
	gap  []wire.Uuid
	live wire.Uuid
	// missingAtRestart is what Theo's journal lacked when it came back: the
	// "something to lose" R1 insists on.
	missingAtRestart int
	replays          int
}

func runKillTest(t *testing.T, mode killMode, faults client.Faults) killResult {
	t.Helper()
	k := newKillRig(t, nil)
	cs := k.cast()
	l := &ledger{}
	var res killResult

	theoDev, adaDev := k.enroll(cs.theo, "theo's phone"), k.enroll(cs.ada, "ada's laptop")
	theoTok := string(theoDev.AccessToken)
	journal := client.NewJournal()
	theo := k.client(theoDev, journal, faults)
	ada := k.client(adaDev, nil, client.Faults{})
	awaitClient(t, theo, "theo's first ready and catch-up", func() bool {
		s := theo.Status()
		return s.Readys >= 1 && s.CaughtUp
	})
	awaitClient(t, ada, "ada's first ready", func() bool { return ada.Status().Ready })

	// PHASE 1 — messages before an abrupt close, over both sockets and REST's
	// path, one of them into a room Theo is not in.
	l.socket(t, ada, "ada", cs.a, "one")
	l.socket(t, ada, "ada", cs.b, "two")
	l.socket(t, theo, "theo", cs.a, "three, theo's own")
	l.mustCommit(t, k, cs.b, cs.mal, "four")
	l.mustCommit(t, k, cs.c, cs.ada, "five, not theo's room")
	marker := l.mustCommit(t, k, cs.a, cs.ada, "phase 1 marker")
	awaitClient(t, theo, "phase 1 delivered live", func() bool { return theo.Holds(wid(marker.ID)) })

	// ... and the close lands mid-stream.
	w := k.startWriter(l, cs)
	w.awaitCommits(t, 4)
	switch mode {
	case clientDies:
		theo.Kill()
	case serverStops:
		k.proc.kill()
	}
	w.awaitCommits(t, 8)
	w.halt(t)

	// PHASE 2 — messages while closed.
	for i := 0; i < 10; i++ {
		switch i % 3 {
		case 0:
			l.mustCommit(t, k, cs.a, cs.ada, fmt.Sprintf("while closed %d", i))
		case 1:
			l.mustCommit(t, k, cs.b, cs.mal, fmt.Sprintf("while closed %d", i))
		case 2:
			l.mustCommit(t, k, cs.c, cs.ada, fmt.Sprintf("while closed %d, not theo's room", i))
		}
	}
	res.missingAtRestart = len(client.Compare(serverLog(k.ctx, t, k.pool, cs.theo), journal.Snapshot()).Lost)
	if res.missingAtRestart < 7 {
		t.Fatalf("theo's journal lacks %d visible messages at restart, want at least the 7 committed while closed — there is nothing to lose", res.missingAtRestart)
	}

	// PHASE 3 — reconnect with the cursor. The upgrade is held until Theo's
	// /sync has reached the server, so a client that waited for `ready`
	// before asking would never get its socket.
	k.gates.upgradeAfterSync(theoTok)
	readys := theo.Status().Readys
	switch mode {
	case clientDies:
		theo = k.client(theoDev, journal, faults)
		readys = 0
	case serverStops:
		k.start()
	}
	awaitClient(t, theo, "theo's reconnect and catch-up", func() bool {
		s := theo.Status()
		return s.Readys > readys && s.CaughtUp
	})
	if held, failure := k.gates.upgradesHeld(); failure != "" || held != 1 {
		t.Fatalf("the upgrade gate: held %d, failure %q — want exactly one upgrade released by its client's own /sync", held, failure)
	}
	res.afterReconnect = client.Compare(serverLog(k.ctx, t, k.pool, cs.theo), theo.Snapshot())

	// PHASE 4 — a listener gap planted while a /sync is in flight.
	res.gap, res.live = k.gapWhileSyncInFlight(t, theo, theoTok, l, cs)
	res.afterGap = client.Compare(serverLog(k.ctx, t, k.pool, cs.theo), theo.Snapshot())

	// PHASE 5 — replay every client_id, over the sockets and through the
	// store. The head does not move and every replayed ack says so.
	res.replays = k.replay(t, l, map[string]*client.Client{"ada": ada, "theo": theo})
	res.final = client.Compare(serverLog(k.ctx, t, k.pool, cs.theo), theo.Snapshot())
	return res
}

func (k *killRig) gapWhileSyncInFlight(t *testing.T, theo *client.Client, theoTok string, l *ledger, cs cast) (gap []wire.Uuid, live wire.Uuid) {
	t.Helper()
	awaitClient(t, theo, "caught up before the gap", func() bool { return theo.Status().CaughtUp })
	resyncs := theo.Status().Resyncs

	held := k.gates.holdNextSync(theoTok)
	theo.CatchUp()
	select {
	case <-held.read:
	case <-time.After(10 * time.Second):
		t.Fatal("theo's catch-up /sync never reached the server")
	}

	k.proxy.sever()
	for i, room := range []uuid.UUID{cs.a, cs.b, cs.c, cs.a} {
		s := l.mustCommit(t, k, room, cs.ada, fmt.Sprintf("during the gap %d", i))
		if room != cs.c {
			gap = append(gap, wid(s.ID))
		}
	}
	k.proxy.restore()
	awaitClient(t, theo, "resync_required after the listener's re-LISTEN", func() bool {
		return theo.Status().Resyncs > resyncs
	})

	after := l.mustCommit(t, k, cs.a, cs.ada, "live while the /sync is held")
	live = wid(after.ID)
	awaitClient(t, theo, "the live frame above the gap", func() bool { return theo.Holds(live) })

	close(held.release)
	awaitClient(t, theo, "catch-up after the held page", func() bool { return theo.Status().CaughtUp })
	return gap, live
}

func (k *killRig) replay(t *testing.T, l *ledger, clients map[string]*client.Client) int {
	t.Helper()
	head := k.head()
	n := 0
	for _, s := range l.sockets {
		c := clients[s.who]
		awaitClient(t, c, s.who+" connected for the replay", func() bool { return c.Status().Ready })
		ctx, cancel := context.WithTimeout(k.ctx, 10*time.Second)
		ack, err := c.Send(ctx, s.frame)
		cancel()
		if err != nil {
			t.Fatalf("replay of %s over %s's socket: %v", s.frame.ClientID, s.who, err)
		}
		if ack.Duplicate == nil || !*ack.Duplicate {
			t.Errorf("replayed ack for %s = %+v, want duplicate: true", s.frame.ClientID, ack)
		}
		if ack.MessageID != s.ack.MessageID || ack.Seq != s.ack.Seq || ack.LogSeq != s.ack.LogSeq {
			t.Errorf("replayed ack %+v is not the original %+v", ack, s.ack)
		}
		n++
	}
	for _, s := range l.stores {
		again, err := k.st.SendMessage(k.ctx, s.msg)
		if err != nil || !again.Duplicate || again.ID != s.sent.ID {
			t.Errorf("store replay of %s = %+v, %v; want the original %s as a duplicate", s.msg.ClientID, again, err, s.sent.ID)
		}
		n++
	}
	if after := k.head(); after != head {
		t.Errorf("head moved from %d to %d across %d replays", head, after, n)
	}
	return n
}

// Criterion 6.
func TestAForcedKillResumesWithZeroLossAndZeroDuplication(t *testing.T) {
	for _, mode := range []killMode{clientDies, serverStops} {
		t.Run(string(mode), func(t *testing.T) {
			res := runKillTest(t, mode, client.Faults{})
			for _, cp := range []struct {
				name string
				r    client.Report
			}{
				{"after the reconnect", res.afterReconnect},
				{"after the gap", res.afterGap},
				{"after the replay", res.final},
			} {
				if !cp.r.Clean() {
					t.Errorf("%s: %s\nlost %v\nduplicated %v\nphantom %v\nmismatched %v\nseq conflicts %v\nout of order %v",
						cp.name, cp.r, cp.r.Lost, cp.r.Duplicated, cp.r.Phantom, cp.r.Mismatched, cp.r.SeqConflicts, cp.r.OutOfOrder)
				}
			}
			if res.final.Counted != res.final.ServerMessages {
				t.Errorf("counted %d, server log %d: each message counted exactly once", res.final.Counted, res.final.ServerMessages)
			}
			t.Logf("%s — missing at restart %d · %s · %d replays, head unchanged", mode, res.missingAtRestart, res.final, res.replays)
		})
	}
}

// The instrument, watched failing. Each subtest runs the whole of criterion
// 6 with a client broken in one obligation, and requires the report the
// passing test requires to be clean to name exactly what the fault cost.
func TestTheKillTestCatchesABrokenClient(t *testing.T) {
	t.Run("a cursor moved by live frames loses the gap", func(t *testing.T) {
		res := runKillTest(t, clientDies, client.Faults{CursorOnLiveFrames: true})
		if len(res.gap) == 0 || !containsAll(res.final.Lost, res.gap) {
			t.Errorf("lost %v, want every gap message %v: %s", res.final.Lost, res.gap, res.final)
		}
	})
	t.Run("catch-up ended on a page requested before the resync loses the gap", func(t *testing.T) {
		res := runKillTest(t, clientDies, client.Faults{EndCatchUpEarly: true})
		if len(res.gap) == 0 || !containsAll(res.afterGap.Lost, res.gap) {
			t.Errorf("lost %v, want every gap message %v: %s", res.afterGap.Lost, res.gap, res.afterGap)
		}
	})
	t.Run("dedupe by log_seq counts the live frame twice", func(t *testing.T) {
		res := runKillTest(t, clientDies, client.Faults{DedupeByLogSeq: true})
		if !containsAll(res.afterGap.Duplicated, []wire.Uuid{res.live}) {
			t.Errorf("duplicated %v, want the live frame %s: %s", res.afterGap.Duplicated, res.live, res.afterGap)
		}
	})
}

// The client keeps its half of CANT-23's heartbeat: one session held across
// more than a whole server window. A client that did not ping would be severed
// with 4000 inside it (internal/api's
// TestASilentSessionIsSeveredAtTheHeartbeatWindowAndDetaches).
func TestTheGoClientHoldsOneSessionAcrossTheHeartbeatWindow(t *testing.T) {
	const interval, limit = 5, 1
	k := newKillRig(t, func(c *config.Config) {
		c.HeartbeatIntervalSec, c.MissedPongLimit = interval, limit
	})
	theo := mkUser(k.ctx, t, k.pool, "theo", "Theo")
	c := k.client(k.enroll(theo, "phone"), nil, client.Faults{})
	awaitClient(t, c, "ready", func() bool { return c.Status().Ready })

	window := time.Duration(interval*(limit+1)) * time.Second
	time.Sleep(window + 5*time.Second)

	s := c.Status()
	if !s.Ready || s.Readys != 1 || s.Dials != 1 {
		t.Errorf("after %v: ready %v, %d readys over %d dials — want one session, never re-established (last close %q)",
			window+5*time.Second, s.Ready, s.Readys, s.Dials, s.LastClose)
	}
	if s.HeartbeatInterval != interval*time.Second || s.MissedPongLimit != limit {
		t.Errorf("the client runs %v / %d, want the announced %ds / %d", s.HeartbeatInterval, s.MissedPongLimit, interval, limit)
	}
	if s.PingsSent < 2 || s.PongsReceived < 2 || s.HeartbeatSevers != 0 {
		t.Errorf("pings %d, pongs %d, severs %d — want at least two answered pings and no sever", s.PingsSent, s.PongsReceived, s.HeartbeatSevers)
	}
}
