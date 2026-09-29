package main

// CANT-153 (CANT-35 ruling 0 → A) — the Go half of the TypeScript cohort:
// cohortClient, the interface every rig drives a client through, and tsDriver,
// which satisfies it by running web/src/transport/driver/driver.ts as a child
// process (`node web/dist-transport-driver/driver.js`) and speaking its eight
// line-delimited JSON commands over stdio. The protocol is written down once,
// at the top of driver.ts; this file is its caller.
//
// ONE FILE, TWO PACKAGES. cmd/catenary/tsdriver_test.go is a symlink to this
// file, so the kill-test and restore-test rigs drive the TypeScript client
// through the SAME adapter the soak does, not a second copy grown beside it.
// Both are `package main`, and neither module can import the other, so the
// link is how "built from the same Go adapter code" stays true. It follows
// that nothing here may name anything only one of the two packages declares:
// the standard library, internal/client and internal/wire, and nothing else.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// cohortClient is exactly what the rigs call on a client: the soak's phases,
// and CatchUp for the kill test. *client.Client satisfies it unchanged; the
// Go-only journal a rig may also hold stays outside it.
//
// A RIG THAT COMPARES does not call Snapshot on a cohortClient directly: it
// reads through tsSnapshotter when the client has one (soakClient.snapshot in
// soakrig, snapshotOf in cmd/catenary), because a TypeScript journal is read
// over stdio and that read can fail. tsDriver.Snapshot panics on a failed read
// rather than hand Compare an empty journal.
type cohortClient interface {
	Run(ctx context.Context) error
	Await(ctx context.Context, pred func() bool) error
	Send(ctx context.Context, f wire.ClientSend) (wire.ServerAck, error)
	Sever()
	Status() client.Status
	Snapshot() client.Snapshot
	Kill()
	CatchUp()
}

var (
	_ cohortClient = (*client.Client)(nil)
	_ cohortClient = (*tsDriver)(nil)
)

// tsSnapshotter is the one thing a tsDriver can say that a *client.Client
// never needs to: that it could not READ its journal at all. A snapshot that
// failed must never be compared as an empty journal — that would report every
// committed message as lost, a server failure the server did not commit.
type tsSnapshotter interface {
	TrySnapshot() (client.Snapshot, error)
}

// tsDriverConfig is one TypeScript client.
type tsDriverConfig struct {
	// Node is the node binary; empty is "node" on PATH. Script is the built
	// driver bundle, driver.js.
	Node, Script string

	BaseURL     string
	UserID      wire.Uuid
	DeviceID    wire.Uuid
	AccessToken string

	Faults client.Faults
	// BackoffMin and BackoffMax are the transport's backoffMinMs/backoffMaxMs,
	// which exist for rigs only, as Go's Config.BackoffMin/BackoffMax do.
	BackoffMin, BackoffMax time.Duration

	// JournalFile, when set, launches the driver with --journal=<file>: the
	// durable IdbJournal over fake-indexeddb, persisted to that file after
	// every completed transaction (CANT-169). A driver launched again over the
	// same file resumes from what the last one committed, which is the
	// relaunch after a Kill. Empty is a journal in memory, which dies with the
	// process.
	JournalFile string

	// Log turns the transport's own structured log on, to Stderr.
	Log bool
	// Stderr receives the driver's stderr. Nil discards it; the last few KiB
	// are kept regardless, for the error a driver that died is reported with.
	Stderr io.Writer
}

// enrolled fills a config's credential from an enrollment. The
// driver presents it as held — heldCredential, Go's Refresh: false — which is
// what every rig runs.
func (c *tsDriverConfig) enrolled(e wire.EnrollResponse) {
	c.UserID, c.DeviceID, c.AccessToken = e.UserID, e.DeviceID, string(e.AccessToken)
}

// tsDriver is one TypeScript transport in a Node child process.
type tsDriver struct {
	cfg  tsDriverConfig
	cmd  *exec.Cmd
	tail *tsTail

	wmu   sync.Mutex // one request line at a time on stdin
	stdin io.WriteCloser

	mu      sync.Mutex
	next    int64
	pending map[int64]chan tsResponse
	last    client.Status
	stopRun chan struct{} // closed by Stop to end the Run in progress
	killed  bool

	exited  chan struct{}
	exitErr error
}

type tsResponse struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *tsDriverError  `json:"error"`
}

// tsDriverError is a refusal from the driver, by the kind driver.ts names.
type tsDriverError struct {
	Kind    string           `json:"kind"`
	Message string           `json:"message"`
	Frame   *json.RawMessage `json:"frame,omitempty"`
}

func (e *tsDriverError) Error() string { return "ts driver: " + e.Kind + ": " + e.Message }

// errTSDriverGone is every request made after the process has ended.
var errTSDriverGone = errors.New("ts driver: the process has exited")

// newTSDriver starts the process. The transport is not started until Run.
func newTSDriver(cfg tsDriverConfig) (*tsDriver, error) {
	if cfg.Script == "" {
		return nil, errors.New("ts driver: no driver script")
	}
	if _, err := os.Stat(cfg.Script); err != nil {
		return nil, fmt.Errorf("ts driver: the driver bundle: %w — build it with `npm run build:driver` in web/", err)
	}
	node := cfg.Node
	if node == "" {
		node = "node"
	}
	d := &tsDriver{cfg: cfg, tail: &tsTail{max: 16 << 10}, pending: map[int64]chan tsResponse{}, exited: make(chan struct{})}
	args := []string{cfg.Script}
	if cfg.JournalFile != "" {
		args = append(args, "--journal="+cfg.JournalFile)
	}
	d.cmd = exec.Command(node, args...)
	if cfg.Stderr != nil {
		d.cmd.Stderr = io.MultiWriter(d.tail, cfg.Stderr)
	} else {
		d.cmd.Stderr = d.tail
	}
	stdin, err := d.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := d.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	d.stdin = stdin
	if err := d.cmd.Start(); err != nil {
		return nil, fmt.Errorf("ts driver: start %s: %w", node, err)
	}
	// THE READER OWNS WAIT. os/exec's StdoutPipe may not be read after Wait has
	// begun, so Wait is called only once the reader has seen EOF.
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 64<<20) // a snapshot of a soak's journal is one line
		for sc.Scan() {
			var r tsResponse
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				continue
			}
			d.mu.Lock()
			ch := d.pending[r.ID]
			delete(d.pending, r.ID)
			d.mu.Unlock()
			if ch != nil {
				ch <- r
			}
		}
		err := d.cmd.Wait()
		d.mu.Lock()
		d.exitErr = err
		pending := d.pending
		d.pending = map[int64]chan tsResponse{}
		d.mu.Unlock()
		close(d.exited)
		for _, ch := range pending {
			close(ch)
		}
	}()
	return d, nil
}

// call sends one request and waits for its answer. out, if non-nil, receives
// the result.
func (d *tsDriver) call(ctx context.Context, cmd string, args any, out any) error {
	ch := make(chan tsResponse, 1)
	d.mu.Lock()
	select {
	case <-d.exited:
		d.mu.Unlock()
		return d.goneErr()
	default:
	}
	d.next++
	id := d.next
	d.pending[id] = ch
	d.mu.Unlock()

	line, err := json.Marshal(map[string]any{"id": id, "cmd": cmd, "args": args})
	if err != nil {
		d.forget(id)
		return err
	}
	d.wmu.Lock()
	_, err = d.stdin.Write(append(line, '\n'))
	d.wmu.Unlock()
	if err != nil {
		d.forget(id)
		return fmt.Errorf("ts driver: write %s: %w", cmd, err)
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return d.goneErr()
		}
		if !r.OK {
			if r.Error == nil {
				return fmt.Errorf("ts driver: %s refused with no error", cmd)
			}
			return r.Error
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-ctx.Done():
		d.forget(id)
		return ctx.Err()
	}
}

func (d *tsDriver) forget(id int64) {
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}

func (d *tsDriver) goneErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.killed {
		return client.ErrKilled
	}
	return fmt.Errorf("%w (%v); stderr:\n%s", errTSDriverGone, d.exitErr, d.tail.String())
}

// Run starts the transport and blocks until ctx ends, Stop or Kill is called,
// the process dies, or the transport goes terminal. It returns what
// client.Client.Run returns for each: ctx's error, nil after Stop (a Go client
// has no Stop; Close is its nearest), ErrKilled, an error naming the dead
// process, or a *client.TerminalError.
func (d *tsDriver) Run(ctx context.Context) error {
	stop := make(chan struct{})
	d.mu.Lock()
	d.stopRun = stop
	d.mu.Unlock()

	faults, err := tsFaults(d.cfg.Faults)
	if err != nil {
		return err
	}
	if err := d.call(ctx, "start", map[string]any{
		"baseUrl": d.cfg.BaseURL,
		"credential": map[string]any{
			"userId": d.cfg.UserID, "deviceId": d.cfg.DeviceID, "accessToken": d.cfg.AccessToken,
		},
		"faults":        faults,
		"backoffMinMs":  d.cfg.BackoffMin.Milliseconds(),
		"backoffMaxMs":  d.cfg.BackoffMax.Milliseconds(),
		"clientVersion": "soakrig-driver",
		"log":           d.cfg.Log,
	}, nil); err != nil {
		return err
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = d.call(sctx, "stop", nil, nil)
			cancel()
			return ctx.Err()
		case <-stop:
			return nil
		case <-d.exited:
			return d.goneErr()
		case <-tick.C:
			if s := d.Status(); s.Terminal.Kind != client.NotTerminal {
				return &client.TerminalError{Terminal: s.Terminal}
			}
		}
	}
}

// Stop stops the transport with a clean close and keeps the journal, and ends
// the Run in progress. A later Run is a NEW transport over the SAME journal,
// with its stats at zero: Go's "restart with a new Client over Journal()",
// which is how a rig restarts a TypeScript client whose journal lives only in
// this process (CANT-35 ruling 2 → B).
func (d *tsDriver) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := d.call(ctx, "stop", nil, nil)
	d.mu.Lock()
	// The stopped transport's status is not the next one's: a rig awaiting the
	// next session's `ready` must not read this session's.
	d.last = client.Status{}
	if d.stopRun != nil {
		close(d.stopRun)
		d.stopRun = nil
	}
	d.mu.Unlock()
	return err
}

// Await polls status every 50 ms until pred holds. pred reads through Status,
// as it does over a *client.Client.
func (d *tsDriver) Await(ctx context.Context, pred func() bool) error {
	for {
		if pred() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Send writes a `send` frame and waits for its ack, mapping each refusal onto
// the error a *client.Client returns for it — so sendWasRefused reads both
// cohorts the same way.
func (d *tsDriver) Send(ctx context.Context, f wire.ClientSend) (wire.ServerAck, error) {
	var out struct {
		Ack wire.ServerAck `json:"ack"`
	}
	err := d.call(ctx, "send", map[string]any{"frame": f}, &out)
	var de *tsDriverError
	if errors.As(err, &de) {
		switch de.Kind {
		case "SendRefused":
			var frame wire.ServerError
			if de.Frame == nil || json.Unmarshal(*de.Frame, &frame) != nil {
				return wire.ServerAck{}, fmt.Errorf("ts driver: a refusal with no readable error frame: %w", err)
			}
			return wire.ServerAck{}, &client.SendError{Frame: frame}
		case "NotConnected":
			return wire.ServerAck{}, client.ErrNotConnected
		case "SessionEnded":
			return wire.ServerAck{}, client.ErrSessionEnded
		case "SendInFlight":
			return wire.ServerAck{}, client.ErrSendInFlight
		}
	}
	if err != nil {
		return wire.ServerAck{}, err
	}
	return out.Ack, nil
}

// Sever drops the socket through the driver's proxy: no close frame, the same
// as a network drop, and Run carries on.
func (d *tsDriver) Sever() { _ = d.quick("sever") }

// Blackhole silences the socket through the proxy and leaves it open: a
// half-dead socket, which only the heartbeat finds. No Go counterpart.
func (d *tsDriver) Blackhole() { _ = d.quick("blackhole") }

// CatchUp is a trigger.
func (d *tsDriver) CatchUp() { _ = d.quick("catchup") }

func (d *tsDriver) quick(cmd string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return d.call(ctx, cmd, nil, nil)
}

// Status is the transport's status decoded into client.Status, field by
// field. A driver that cannot answer reports the last status it did.
func (d *tsDriver) Status() client.Status {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out struct {
		Status       tsStatus `json:"status"`
		JournalWipes int      `json:"journalWipes"`
	}
	if err := d.call(ctx, "status", nil, &out); err != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.last
	}
	s := out.Status.client(out.JournalWipes)
	d.mu.Lock()
	d.last = s
	d.mu.Unlock()
	return s
}

// Snapshot is TrySnapshot for the interface, and PANICS on a failed read. An
// empty client.Snapshot is the one wrong answer here: Compare would report
// every committed message as lost, a server failure nobody committed. Rigs that
// compare call TrySnapshot and name the failure; a caller that reaches for
// Snapshot instead fails loudly rather than falsely.
func (d *tsDriver) Snapshot() client.Snapshot {
	s, err := d.TrySnapshot()
	if err != nil {
		panic("ts driver: the journal could not be read, and an empty one would read as every message lost: " + err.Error())
	}
	return s
}

// TrySnapshot reads the journal, wire JSON decoded into client.Snapshot for the
// unchanged client.Compare.
func (d *tsDriver) TrySnapshot() (client.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out struct {
		Cursor        *int64              `json:"cursor"`
		Messages      []wire.Message      `json:"messages"`
		Conversations []wire.Conversation `json:"conversations"`
		Users         []wire.User         `json:"users"`
		Counted       []wire.Uuid         `json:"counted"`
		Wipes         int                 `json:"wipes"`
	}
	if err := d.call(ctx, "snapshot", nil, &out); err != nil {
		return client.Snapshot{}, err
	}
	s := client.Snapshot{
		HasCursor: out.Cursor != nil, Wipes: out.Wipes,
		Messages: out.Messages, Conversations: out.Conversations, Users: out.Users, Counted: out.Counted,
	}
	if out.Cursor != nil {
		s.Cursor = *out.Cursor
	}
	return s, nil
}

// ReadPersisted is what a driver launched with a JournalFile left on disk —
// what its relaunch will resume from — read by launching a second driver over
// the same file, reading its journal without starting a transport, and ending
// it. It is how a rig reads a TypeScript client that is dead, as it reads a dead
// Go client through the Journal it kept.
func (d *tsDriver) ReadPersisted() (client.Snapshot, error) {
	if d.cfg.JournalFile == "" {
		return client.Snapshot{}, errors.New("ts driver: no journal file — a journal in memory died with its process")
	}
	r, err := newTSDriver(d.cfg)
	if err != nil {
		return client.Snapshot{}, err
	}
	defer r.Kill()
	return r.TrySnapshot()
}

// Kill is `kill -9` on the driver: SIGKILL, and nothing it had in flight
// reaches anything after. A journal in memory dies with it, so a relaunch
// starts from nothing; one in a JournalFile (CANT-169) is what the dead driver
// had committed, and a relaunch over the file resumes from it.
func (d *tsDriver) Kill() {
	d.mu.Lock()
	d.killed = true
	d.mu.Unlock()
	if d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
	_ = d.stdin.Close()
	<-d.exited
}

// tsFaults is client.Faults in the driver's camelCase. HelloInstead and
// AfterReady are not mirrored (faults.ts), so asking for them is an error
// rather than a run that silently lacks them.
func tsFaults(f client.Faults) (map[string]bool, error) {
	if f.HelloInstead != nil || f.AfterReady != nil {
		return nil, errors.New("ts driver: HelloInstead and AfterReady have no TypeScript counterpart")
	}
	return map[string]bool{
		"cursorOnLiveFrames":       f.CursorOnLiveFrames,
		"dedupeByLogSeq":           f.DedupeByLogSeq,
		"endCatchUpEarly":          f.EndCatchUpEarly,
		"skipWipe":                 f.SkipWipe,
		"refreshUnlocked":          f.RefreshUnlocked,
		"noChain":                  f.NoChain,
		"proposeAfresh":            f.ProposeAfresh,
		"unbounded":                f.Unbounded,
		"presentRefusedToken":      f.PresentRefusedToken,
		"neverPresentRefusedToken": f.NeverPresentRefusedToken,
		"neverTerminal":            f.NeverTerminal,
		"alwaysTerminal":           f.AlwaysTerminal,
	}, nil
}

// tsStatus is TransportStatus (web/src/transport/status.ts) as JSON.
type tsStatus struct {
	Terminal struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"terminal"`
	RefreshHold          string   `json:"refreshHold"`
	TokenRefused         bool     `json:"tokenRefused"`
	NextRefreshAt        *float64 `json:"nextRefreshAt"`
	Connected            bool     `json:"connected"`
	Ready                bool     `json:"ready"`
	SessionID            *string  `json:"sessionId"`
	HeartbeatIntervalSec *float64 `json:"heartbeatIntervalSec"`
	MissedPongLimit      *int     `json:"missedPongLimit"`
	CaughtUp             bool     `json:"caughtUp"`
	Cursor               *int64   `json:"cursor"`
	Messages             int      `json:"messages"`
	Stats                tsStats  `json:"stats"`
}

// tsStats is Stats (status.ts): Go's names, camelCased.
type tsStats struct {
	Dials                    int         `json:"dials"`
	DialErrors               int         `json:"dialErrors"`
	Readys                   int         `json:"readys"`
	Resyncs                  int         `json:"resyncs"`
	Discards                 int         `json:"discards"`
	LiveFrames               int         `json:"liveFrames"`
	Pages                    int         `json:"pages"`
	SyncErrors               int         `json:"syncErrors"`
	SyncsBeforeReady         int         `json:"syncsBeforeReady"`
	PingsSent                int         `json:"pingsSent"`
	PongsReceived            int         `json:"pongsReceived"`
	HeartbeatSevers          int         `json:"heartbeatSevers"`
	Refreshes                int         `json:"refreshes"`
	RefreshesSkipped         int         `json:"refreshesSkipped"`
	RefreshErrors            int         `json:"refreshErrors"`
	RefreshWalkBacks         int         `json:"refreshWalkBacks"`
	ChainLength              int         `json:"chainLength"`
	RefreshesHeldUnreachable int         `json:"refreshesHeldUnreachable"`
	RefreshesHeldBackoff     int         `json:"refreshesHeldBackoff"`
	DialsWithheld            int         `json:"dialsWithheld"`
	SyncsWithheld            int         `json:"syncsWithheld"`
	Undecodable              int         `json:"undecodable"`
	LastRttMs                float64     `json:"lastRttMs"`
	LastClose                string      `json:"lastClose"`
	CloseStatuses            map[int]int `json:"closeStatuses"`
}

// client maps the status onto client.Status. Wipes is the JOURNAL's count, as
// Go's is; TransportStatus.wipes is one transport's, and a restarted driver
// has had more than one.
func (t tsStatus) client(journalWipes int) client.Status {
	s := client.Status{
		Stats: client.Stats{
			Dials: t.Stats.Dials, DialErrors: t.Stats.DialErrors, Readys: t.Stats.Readys,
			Resyncs: t.Stats.Resyncs, Discards: t.Stats.Discards, LiveFrames: t.Stats.LiveFrames,
			Pages: t.Stats.Pages, SyncErrors: t.Stats.SyncErrors, SyncsBeforeReady: t.Stats.SyncsBeforeReady,
			PingsSent: t.Stats.PingsSent, PongsReceived: t.Stats.PongsReceived, HeartbeatSevers: t.Stats.HeartbeatSevers,
			Refreshes: t.Stats.Refreshes, RefreshesSkipped: t.Stats.RefreshesSkipped, RefreshErrors: t.Stats.RefreshErrors,
			RefreshWalkBacks: t.Stats.RefreshWalkBacks, ChainLength: t.Stats.ChainLength,
			RefreshesHeldUnreachable: t.Stats.RefreshesHeldUnreachable, RefreshesHeldBackoff: t.Stats.RefreshesHeldBackoff,
			DialsWithheld: t.Stats.DialsWithheld, SyncsWithheld: t.Stats.SyncsWithheld, Undecodable: t.Stats.Undecodable,
			LastRTT:       time.Duration(t.Stats.LastRttMs * float64(time.Millisecond)),
			LastClose:     t.Stats.LastClose,
			CloseStatuses: t.Stats.CloseStatuses,
		},
		TokenRefused: t.TokenRefused,
		Connected:    t.Connected,
		Ready:        t.Ready,
		CaughtUp:     t.CaughtUp,
		Messages:     t.Messages,
		Wipes:        journalWipes,
	}
	switch t.Terminal.Kind {
	case "credential":
		s.Terminal = client.Terminal{Kind: client.TerminalCredential, Reason: t.Terminal.Reason}
	case "protocol":
		s.Terminal = client.Terminal{Kind: client.TerminalProtocol, Reason: t.Terminal.Reason}
	}
	switch t.RefreshHold {
	case "unreachable":
		s.RefreshHold = client.RefreshHeldUnreachable
	case "backoff":
		s.RefreshHold = client.RefreshHeldBackoff
	}
	if t.NextRefreshAt != nil {
		s.NextRefreshAt = time.UnixMilli(int64(*t.NextRefreshAt))
	}
	if t.SessionID != nil {
		s.SessionID = wire.Uuid(*t.SessionID)
	}
	if t.HeartbeatIntervalSec != nil {
		s.HeartbeatInterval = time.Duration(*t.HeartbeatIntervalSec * float64(time.Second))
	}
	if t.MissedPongLimit != nil {
		s.MissedPongLimit = *t.MissedPongLimit
	}
	if t.Cursor != nil {
		s.Cursor, s.HasCursor = *t.Cursor, true
	}
	return s
}

// tsTail keeps the last max bytes written to it.
type tsTail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tsTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tsTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
