// Package client is a Go Catenary client: the socket and its heartbeat, the
// /sync catch-up, and the five client obligations of CANT-24's decision record
// (docs/decisions/cant-24-resume.md), cited below by number.
//
// WHERE IT LIVES, AND WHY HERE (CANT-102). Three callers need one client and
// must not grow three: CANT-102's kill test in cmd/catenary, CANT-27's soak and
// chaos harness, and the deployed-path idle measurement CANT-23 still owes — an
// hour of idle through the real tunnel, pointed at an external URL with a real
// token. So it is not a _test file, the base URL and the credential are
// parameters, and it sits under the service module's internal/, which both
// cmd/catenary and the server/ module can import: Go applies the internal rule
// to the IMPORT PATH, and github.com/magos/catenary/server/... is under
// github.com/magos/catenary/ — the same reasoning that put internal/wire here
// (CANT-82).
//
// IT IS NOT THE PRODUCT'S CLIENT. CANT-35 (TypeScript) and CANT-42 (Dart) are,
// and CANT-46 checks them. This one exists to be measured against the server's
// own record of what it committed, so it renders nothing, holds everything in a
// Journal, and says exactly what it did.
//
// THE OBLIGATIONS.
//
//  1. Persist before render — Journal; a page lands under one lock.
//  2. The cursor moves on a /sync page's log_seq and on nothing else. Live
//     frames are applied by message id, a later record replaces an earlier
//     one, and every cursor move is monotonic — applyPage, applyLive.
//  3. Catch-up is re-entrant, and ends on a has_more:false page whose request
//     was issued after the most recent trigger. Triggers are a reconnect,
//     `ready`, `resync_required`, and CatchUp — catchUp.
//  4. `ready.log_seq` below the cursor means wipe and bootstrap from 0, and a
//     page requested before the wipe is dropped rather than applied over it —
//     onReady.
//  5. Conversations and users arrive on catch-up and never live. CatchUp is
//     the hook for a client's own policy of catching up sooner.
//
// And the two the record states outside the list: nothing is derived from
// `ready.log_seq` about streaming (every `ready` is treated as resumed: false,
// which is always correct), and a retried send reuses its client_id — Send takes
// the frame as the caller built it and never mints one.
//
// THE /sync A RECONNECT NEEDS IS ISSUED BESIDE THE UPGRADE, not after `ready`:
// Run pulls the reconnect trigger before it dials, so the catch-up goroutine
// requests its page while the socket is still being established.
//
// FAULTS. Each field of Faults deliberately breaks one obligation. They exist
// so that an assertion claiming zero loss or zero duplication can be shown to
// fail against a client that deserves it: a measurement nobody has seen say
// "not zero" is a claim about the instrument, not about the system.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/magos/catenary/internal/wire"
)

const (
	subprotocolV1          = "catenary.v1"
	tokenSubprotocolPrefix = "catenary.token."

	dialTimeout  = 30 * time.Second
	writeTimeout = 10 * time.Second
	syncTimeout  = 30 * time.Second

	// maxFrameBytes bounds one inbound frame. A `message` frame is one
	// message; nothing the server sends approaches this.
	maxFrameBytes = 8 << 20
	// maxSyncBody bounds one /sync response: MaxSyncLimit messages with their
	// conversations and users, with a wide margin.
	maxSyncBody = 64 << 20

	defaultClientInfo = "catenary-go-client"
	defaultBackoffMin = 250 * time.Millisecond
	defaultBackoffMax = 5 * time.Second
)

var (
	// ErrKilled is what Run returns after Kill.
	ErrKilled = errors.New("client: killed")
	// ErrNotConnected is Send without an open socket.
	ErrNotConnected = errors.New("client: no socket is open")
	// ErrSessionEnded is a Send whose socket closed before it was answered.
	// The outcome is unknown; retrying with the SAME client_id is safe.
	ErrSessionEnded = errors.New("client: the session ended before the send was answered")
	// ErrSendInFlight is a second Send with a client_id still awaiting its
	// answer on this client.
	ErrSendInFlight = errors.New("client: a send with this client_id is already awaiting its answer")
)

// SendError is a send the server refused with an `error` frame.
type SendError struct{ Frame wire.ServerError }

func (e *SendError) Error() string {
	return fmt.Sprintf("client: send refused: %s: %s", e.Frame.Code, e.Frame.Message)
}

// Faults deliberately break one obligation each. The zero value is a correct
// client. Never set outside a test that is proving an assertion can fail.
type Faults struct {
	// CursorOnLiveFrames breaks obligation 2: a live `message` frame moves
	// the cursor to its log_seq. R1's client did this, before ruling 5.
	CursorOnLiveFrames bool

	// DedupeByLogSeq breaks obligation 2's dedupe rule: a record counts as
	// new exactly when its log_seq is above the cursor, whatever its id. Also
	// R1's shape. It counts a message twice when a page re-carries one that
	// arrived live, and drops a CANT-92 re-emission as already held.
	DedupeByLogSeq bool

	// EndCatchUpEarly breaks obligation 3: catch-up ends on the first
	// has_more:false page, even one requested before the latest trigger.
	EndCatchUpEarly bool

	// SkipWipe breaks obligation 4: on `ready.log_seq` below the cursor the
	// client re-syncs from 0 and keeps its store and its (monotonic) cursor.
	SkipWipe bool
}

// Config is everything a Client needs. BaseURL, AccessToken and DeviceID are
// required; everything else has a default.
type Config struct {
	// BaseURL is the server's origin, http:// or https://, e.g.
	// https://catenary.example.org. /ws and /sync are appended.
	BaseURL string
	// AccessToken rides on the upgrade as `catenary.token.<token>` and on
	// /sync as a bearer.
	AccessToken string
	// DeviceID is the device the token was minted for; the hello must name it.
	DeviceID wire.Uuid
	// ClientInfo goes on the hello for the server's log. Never parsed.
	ClientInfo string

	// Journal is the durable state. Nil means a fresh one. Pass the journal
	// of a killed client to restart it.
	Journal *Journal

	// HTTPClient carries both /sync and the upgrade. Nil means a plain
	// client; requests are bounded by their own contexts.
	HTTPClient *http.Client
	// Logger receives the client's own account of itself. Nil discards.
	Logger *slog.Logger

	// SyncLimit is the page size /sync is asked for. Zero is the server's.
	SyncLimit int

	// BackoffMin and BackoffMax bound the reconnect and catch-up retry
	// backoff, which doubles from the one to the other. Zero means 250 ms and
	// 5 s.
	BackoffMin, BackoffMax time.Duration

	Faults Faults
}

// Stats counts what the client did. Cumulative over the Client's life.
type Stats struct {
	Dials      int // connection attempts
	DialErrors int
	Readys     int // `ready` frames received — sessions established
	Resyncs    int // `resync_required` frames received
	Discards   int // `ready.log_seq` below the cursor (obligation 4)
	LiveFrames int // `message` frames applied
	Pages      int // /sync pages applied or dropped
	SyncErrors int
	// SyncsBeforeReady counts /sync requests issued while a connection
	// attempt had not yet received `ready`.
	SyncsBeforeReady int
	PingsSent        int
	PongsReceived    int
	HeartbeatSevers  int // sockets this client severed for unanswered pings
	Undecodable      int
	LastRTT          time.Duration
	LastClose        string
}

// Status is a point-in-time view of a Client.
type Status struct {
	Stats
	Connected         bool // a socket is open
	Ready             bool // and has received `ready`
	SessionID         wire.Uuid
	HeartbeatInterval time.Duration
	MissedPongLimit   int
	// CaughtUp is true when no trigger is outstanding: the last catch-up
	// ended on a page requested after the most recent trigger.
	CaughtUp  bool
	Cursor    int64
	HasCursor bool
	Messages  int
	Wipes     int
}

// Client is one device's connection. Safe for concurrent use.
type Client struct {
	cfg        Config
	j          *Journal
	log        *slog.Logger
	http       *http.Client
	httpBase   string
	wsURL      string
	backoffMin time.Duration
	backoffMax time.Duration

	// killed is written under j.mu (Kill), so a journal write either lands
	// before the death or not at all.
	killed atomic.Bool
	// wake nudges the catch-up goroutine; capacity one, sends never block.
	wake chan struct{}

	mu sync.Mutex
	// gen counts triggers; doneGen is the trigger the last completed
	// catch-up satisfied. Caught up is gen == doneGen.
	gen, doneGen uint64
	// from0 is SkipWipe's re-sync: the next catch-up starts at 0.
	from0       bool
	running     bool
	cancel      context.CancelFunc
	connecting  bool
	conn        *websocket.Conn
	ready       bool
	sessionID   wire.Uuid
	interval    time.Duration
	missedLimit int
	pingSeq     uint64
	outstanding map[string]time.Time
	waiters     map[wire.Uuid]chan sendResult
	stats       Stats

	nmu     sync.Mutex
	changed chan struct{}
}

type sendResult struct {
	ack wire.ServerAck
	err error
}

// New builds a client. It does nothing until Run.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" || cfg.AccessToken == "" || cfg.DeviceID == "" {
		return nil, errors.New("client: BaseURL, AccessToken and DeviceID are required")
	}
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("client: BaseURL: %w", err)
	}
	ws := *u
	switch u.Scheme {
	case "http":
		ws.Scheme = "ws"
	case "https":
		ws.Scheme = "wss"
	default:
		return nil, fmt.Errorf("client: BaseURL scheme %q, want http or https", u.Scheme)
	}
	ws.Path += "/ws"

	c := &Client{
		cfg:        cfg,
		j:          cfg.Journal,
		log:        cfg.Logger,
		http:       cfg.HTTPClient,
		httpBase:   u.String(),
		wsURL:      ws.String(),
		backoffMin: cfg.BackoffMin,
		backoffMax: cfg.BackoffMax,
		wake:       make(chan struct{}, 1),
		waiters:    map[wire.Uuid]chan sendResult{},
		changed:    make(chan struct{}),
	}
	if c.j == nil {
		c.j = NewJournal()
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.backoffMin <= 0 {
		c.backoffMin = defaultBackoffMin
	}
	if c.backoffMax < c.backoffMin {
		c.backoffMax = max(defaultBackoffMax, c.backoffMin)
	}
	if c.cfg.ClientInfo == "" {
		c.cfg.ClientInfo = defaultClientInfo
	}
	return c, nil
}

// Journal is the client's durable state, for a restart after Kill.
func (c *Client) Journal() *Journal { return c.j }

// Run connects, keeps the socket alive with the announced heartbeat,
// reconnects with backoff when it drops, and runs catch-up on every trigger,
// until ctx is cancelled or Kill is called. It returns ErrKilled after Kill and
// ctx's error otherwise.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	switch {
	case c.running:
		c.mu.Unlock()
		return errors.New("client: Run is already running")
	case c.killed.Load():
		c.mu.Unlock()
		return ErrKilled
	}
	ctx, cancel := context.WithCancel(ctx)
	c.running, c.cancel = true, cancel
	c.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.catchUpLoop(ctx)
	}()

	backoff := c.backoffMin
	for ctx.Err() == nil {
		// A RECONNECT IS A TRIGGER, PULLED BEFORE THE DIAL. The catch-up
		// goroutine issues its /sync now, beside the upgrade, rather than a
		// round trip later on `ready` — which is still a trigger of its own,
		// so the page that ends catch-up is one requested after it.
		c.mu.Lock()
		c.stats.Dials++
		c.connecting = true
		c.gen++
		c.mu.Unlock()
		c.wakeCatchUp()
		c.notify()

		readied, err := c.session(ctx)
		c.mu.Lock()
		if err != nil {
			c.stats.LastClose = err.Error()
		}
		c.mu.Unlock()
		c.notify()
		if ctx.Err() != nil {
			break
		}
		if readied {
			backoff = c.backoffMin
		}
		c.log.Info("session ended; reconnecting", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if !readied {
			backoff = min(backoff*2, c.backoffMax)
		}
	}
	cancel()
	wg.Wait()
	c.mu.Lock()
	c.running, c.connecting = false, false
	c.mu.Unlock()
	c.notify()
	if c.killed.Load() {
		return ErrKilled
	}
	return ctx.Err()
}

// Kill is `kill -9`: the client stops at once, with no close handshake, and
// nothing it had in flight — a page, a live frame — reaches the Journal after
// this returns. Restart with a new Client over Journal().
func (c *Client) Kill() {
	c.j.mu.Lock()
	c.killed.Store(true)
	c.j.mu.Unlock()
	c.mu.Lock()
	cancel, conn := c.cancel, c.conn
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.CloseNow()
	}
	c.notify()
}

// Close stops the client politely: a normal close handshake, then Run returns.
func (c *Client) Close() {
	c.mu.Lock()
	cancel, conn := c.cancel, c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
	}
	if cancel != nil {
		cancel()
	}
}

// CatchUp is a trigger (obligation 5's "own policy"): catch-up runs from the
// cursor, and CaughtUp is false until it ends on a page requested after this.
func (c *Client) CatchUp() {
	c.mu.Lock()
	c.gen++
	c.mu.Unlock()
	c.wakeCatchUp()
	c.notify()
}

// Send writes a `send` frame on the open socket and waits for its `ack`, or
// for the `error` frame naming its client_id (a *SendError). The frame goes as
// the caller built it: a retry is the same frame, client_id included.
func (c *Client) Send(ctx context.Context, f wire.ClientSend) (wire.ServerAck, error) {
	ch := make(chan sendResult, 1)
	c.mu.Lock()
	conn := c.conn
	switch {
	case conn == nil:
		c.mu.Unlock()
		return wire.ServerAck{}, ErrNotConnected
	case c.waiters[f.ClientID] != nil:
		c.mu.Unlock()
		return wire.ServerAck{}, ErrSendInFlight
	}
	c.waiters[f.ClientID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.waiters[f.ClientID] == ch {
			delete(c.waiters, f.ClientID)
		}
		c.mu.Unlock()
	}()

	if err := c.write(ctx, conn, f); err != nil {
		return wire.ServerAck{}, fmt.Errorf("client: send: %w", err)
	}
	select {
	case r := <-ch:
		return r.ack, r.err
	case <-ctx.Done():
		return wire.ServerAck{}, ctx.Err()
	}
}

// Status is a point-in-time view.
func (c *Client) Status() Status {
	c.mu.Lock()
	s := Status{
		Stats:             c.stats,
		Connected:         c.conn != nil,
		Ready:             c.ready,
		SessionID:         c.sessionID,
		HeartbeatInterval: c.interval,
		MissedPongLimit:   c.missedLimit,
		CaughtUp:          c.gen == c.doneGen,
	}
	c.mu.Unlock()
	c.j.mu.Lock()
	s.Cursor, s.HasCursor, s.Messages, s.Wipes = c.j.cursor, c.j.hasCursor, len(c.j.messages), c.j.wipes
	c.j.mu.Unlock()
	return s
}

// Holds reports whether the journal holds a message.
func (c *Client) Holds(id wire.Uuid) bool {
	c.j.mu.Lock()
	defer c.j.mu.Unlock()
	_, ok := c.j.messages[id]
	return ok
}

// Message returns the held record for an id.
func (c *Client) Message(id wire.Uuid) (wire.Message, bool) {
	c.j.mu.Lock()
	defer c.j.mu.Unlock()
	m, ok := c.j.messages[id]
	return m, ok
}

// Snapshot copies the journal.
func (c *Client) Snapshot() Snapshot { return c.j.Snapshot() }

// Await blocks until pred returns true or ctx ends. pred is re-evaluated after
// every change the client makes, so it should read through Status, Holds and
// Message rather than cache.
func (c *Client) Await(ctx context.Context, pred func() bool) error {
	for {
		// The channel is taken BEFORE pred runs, so a change that lands
		// between the two still wakes this loop.
		c.nmu.Lock()
		ch := c.changed
		c.nmu.Unlock()
		if pred() {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) notify() {
	c.nmu.Lock()
	close(c.changed)
	c.changed = make(chan struct{})
	c.nmu.Unlock()
}

func (c *Client) wakeCatchUp() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// --- the socket ------------------------------------------------------------

// session runs one socket from dial to close. readied reports whether it got
// as far as `ready`.
func (c *Client) session(ctx context.Context) (readied bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, resp, err := websocket.Dial(dctx, c.wsURL, &websocket.DialOptions{
		HTTPClient:   c.http,
		Subprotocols: []string{subprotocolV1, tokenSubprotocolPrefix + c.cfg.AccessToken},
	})
	cancel()
	if err != nil {
		c.mu.Lock()
		c.stats.DialErrors++
		c.mu.Unlock()
		if resp != nil {
			return false, fmt.Errorf("client: dial: %s: %w", resp.Status, err)
		}
		return false, fmt.Errorf("client: dial: %w", err)
	}
	conn.SetReadLimit(maxFrameBytes)

	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	if c.killed.Load() {
		_ = conn.CloseNow()
		return false, ErrKilled
	}

	// THE HELLO IS WRITTEN BEFORE THE SOCKET IS PUBLISHED. The server closes
	// a session whose first known frame is not a hello with 1008, and Send
	// writes to whatever c.conn holds — so until the hello is on the wire
	// c.conn stays nil, and a Send racing a reconnect gets ErrNotConnected
	// instead of severing the session it raced (PR #51's review;
	// TestNoFrameCanPrecedeTheHello). Frames after the hello and before
	// `ready` still go out, as the wire allows.
	c.j.mu.Lock()
	var resume *wire.LogSeq
	if c.j.hasCursor {
		v := c.j.cursor
		resume = &v
	}
	c.j.mu.Unlock()
	info := c.cfg.ClientInfo
	hello := wire.ClientHello{
		WireVersion: wire.WireVersion, DeviceID: c.cfg.DeviceID,
		ResumeFromLogSeq: resume, ClientInfo: &info,
	}
	if err := c.write(sctx, conn, hello); err != nil {
		_ = conn.CloseNow()
		return false, fmt.Errorf("client: hello: %w", err)
	}

	c.mu.Lock()
	c.conn, c.ready, c.outstanding = conn, false, map[string]time.Time{}
	c.mu.Unlock()
	defer c.endSession(conn)
	// Kill reads c.conn after setting killed, so one of the two sees the
	// other: a kill racing the dial or the hello never leaves a socket open
	// behind it. One that lands before this point has also cancelled ctx,
	// which ends a hello write still in flight.
	if c.killed.Load() {
		return false, ErrKilled
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	for {
		_, data, err := conn.Read(sctx)
		if err != nil {
			return readied, fmt.Errorf("client: read: %w", err)
		}
		f, err := wire.DecodeServerFrame(data)
		if err != nil {
			c.mu.Lock()
			c.stats.Undecodable++
			c.mu.Unlock()
			c.log.Warn("server frame the generated decoder refuses", "error", err)
			continue
		}
		if f == nil {
			continue // an unknown tag: ignored, as every decoder is told to
		}
		switch v := f.(type) {
		case wire.ServerReady:
			readied = true
			c.onReady(v)
			go c.heartbeat(sctx, conn, time.Duration(v.HeartbeatIntervalSec)*time.Second, int(v.MissedPongLimit))
		case wire.Ping:
			_ = c.write(sctx, conn, wire.Pong{ID: v.ID})
		case wire.Pong:
			c.mu.Lock()
			if at, ok := c.outstanding[v.ID]; ok {
				c.stats.LastRTT = time.Since(at)
				delete(c.outstanding, v.ID)
			}
			c.stats.PongsReceived++
			c.mu.Unlock()
		case wire.ServerMessageFrame:
			c.applyLive(v.Message)
		case wire.ServerResyncRequired:
			// A trigger (obligation 3). Nothing else: the cursor is the last
			// page's high water, so a catch-up from it covers the gap by
			// construction.
			c.mu.Lock()
			c.stats.Resyncs++
			c.gen++
			c.mu.Unlock()
			c.wakeCatchUp()
			c.notify()
		case wire.ServerAck:
			c.answer(v.ClientID, sendResult{ack: v})
		case wire.ServerError:
			if v.ClientID != nil {
				c.answer(*v.ClientID, sendResult{err: &SendError{Frame: v}})
			} else {
				c.log.Warn("server error", "code", v.Code, "message", v.Message)
			}
		}
	}
}

func (c *Client) endSession(conn *websocket.Conn) {
	_ = conn.CloseNow()
	c.mu.Lock()
	if c.conn == conn {
		c.conn, c.ready = nil, false
	}
	waiters := c.waiters
	c.waiters = map[wire.Uuid]chan sendResult{}
	c.mu.Unlock()
	for _, ch := range waiters {
		ch <- sendResult{err: ErrSessionEnded}
	}
	c.notify()
}

func (c *Client) answer(id wire.Uuid, r sendResult) {
	c.mu.Lock()
	ch := c.waiters[id]
	delete(c.waiters, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// onReady is the trigger `ready` is, after obligation 4's check.
func (c *Client) onReady(r wire.ServerReady) {
	// OBLIGATION 4. The server's log is behind the cursor: a restore, or the
	// wrong server. Wiped under the journal lock, and the epoch moves, so a
	// page already requested from the old cursor is dropped rather than
	// landing on the empty store — which would set the cursor to that page's
	// high water with none of the messages below it.
	c.j.mu.Lock()
	discard := c.j.hasCursor && r.LogSeq < c.j.cursor
	if discard && !c.cfg.Faults.SkipWipe && !c.killed.Load() {
		c.j.resetLocked()
		c.j.wipes++
	}
	c.j.mu.Unlock()

	c.mu.Lock()
	c.ready, c.connecting = true, false
	c.sessionID = r.SessionID
	c.interval = time.Duration(r.HeartbeatIntervalSec) * time.Second
	c.missedLimit = int(r.MissedPongLimit)
	c.stats.Readys++
	if discard {
		c.stats.Discards++
		if c.cfg.Faults.SkipWipe {
			c.from0 = true
		}
	}
	// `ready{resumed: false}` is a trigger, and in wire version 1 every
	// `ready` is one: `resumed: true` is treated as false, which is always
	// correct. Counted and triggered under one lock, so no observer sees a
	// `ready` without its trigger.
	c.gen++
	c.mu.Unlock()
	c.wakeCatchUp()
	c.notify()
	c.log.Info("ready", "session_id", r.SessionID, "head", r.LogSeq, "discard", discard,
		"heartbeat_interval_sec", r.HeartbeatIntervalSec, "missed_pong_limit", r.MissedPongLimit)
}

// heartbeat pings every announced interval and severs the socket itself once
// missed_pong_limit pings are outstanding, rather than waiting for the OS to
// notice a half-open connection (R1 §3).
func (c *Client) heartbeat(ctx context.Context, conn *websocket.Conn, interval time.Duration, limit int) {
	if interval <= 0 {
		return
	}
	if limit < 1 {
		limit = 1
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		if c.conn != conn {
			c.mu.Unlock()
			return
		}
		if n := len(c.outstanding); n >= limit {
			c.stats.HeartbeatSevers++
			c.mu.Unlock()
			c.log.Warn("severing: pings unanswered", "outstanding", n, "missed_pong_limit", limit)
			_ = conn.CloseNow()
			return
		}
		c.pingSeq++
		id := "hb-" + strconv.FormatUint(c.pingSeq, 10)
		c.outstanding[id] = time.Now()
		c.stats.PingsSent++
		c.mu.Unlock()
		if err := c.write(ctx, conn, wire.Ping{ID: id}); err != nil {
			return
		}
	}
}

func (c *Client) write(ctx context.Context, conn *websocket.Conn, f any) error {
	if c.killed.Load() {
		return ErrKilled
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

// --- the journal writes ------------------------------------------------------

// applyLive is a `message` frame: applied and rendered by id (obligation 2),
// and the cursor does not move.
func (c *Client) applyLive(m wire.Message) {
	c.j.mu.Lock()
	if c.killed.Load() {
		c.j.mu.Unlock()
		return
	}
	c.recordLocked(m)
	if c.cfg.Faults.CursorOnLiveFrames && (!c.j.hasCursor || m.LogSeq > c.j.cursor) {
		c.j.cursor, c.j.hasCursor = m.LogSeq, true
	}
	c.j.mu.Unlock()
	c.mu.Lock()
	c.stats.LiveFrames++
	c.mu.Unlock()
	c.notify()
}

// applyPage lands one page atomically: messages, conversations and users,
// then the cursor, which only ever moves forward (obligations 1 and 2). It
// returns false when the page was requested before a wipe, or after a kill.
func (c *Client) applyPage(p wire.SyncResponse, epoch int) bool {
	c.j.mu.Lock()
	defer c.j.mu.Unlock()
	if c.killed.Load() || c.j.wipes != epoch {
		return false
	}
	for _, m := range p.Messages {
		c.recordLocked(m)
	}
	for _, cv := range p.Conversations {
		c.j.conversations[cv.ID] = cv
	}
	for _, u := range p.Users {
		c.j.users[u.ID] = u
	}
	if !c.j.hasCursor || p.LogSeq > c.j.cursor {
		c.j.cursor = p.LogSeq
	}
	c.j.hasCursor = true
	return true
}

// recordLocked holds a message. THE DEDUPE KEY IS THE ID: an id not yet held
// is counted, and a later record for a held id replaces it without a count —
// a CANT-92 re-emission is exactly that.
func (c *Client) recordLocked(m wire.Message) {
	if c.cfg.Faults.DedupeByLogSeq {
		if c.j.hasCursor && m.LogSeq <= c.j.cursor {
			return
		}
		c.j.messages[m.ID] = m
		c.j.counted = append(c.j.counted, m.ID)
		return
	}
	if _, held := c.j.messages[m.ID]; !held {
		c.j.counted = append(c.j.counted, m.ID)
	}
	c.j.messages[m.ID] = m
}

// --- catch-up ----------------------------------------------------------------

func (c *Client) pending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen != c.doneGen
}

// catchUpLoop runs catch-up whenever a trigger is outstanding, retrying a
// failed one with backoff — or at once, on a new trigger.
func (c *Client) catchUpLoop(ctx context.Context) {
	backoff := c.backoffMin
	for {
		if !c.pending() {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			continue
		}
		err := c.catchUp(ctx)
		if err == nil {
			backoff = c.backoffMin
			continue
		}
		if ctx.Err() != nil || c.killed.Load() {
			return
		}
		c.mu.Lock()
		c.stats.SyncErrors++
		c.mu.Unlock()
		c.log.Debug("catch-up failed; retrying", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			// A new trigger retries at once: a reconnect's /sync goes out
			// beside its upgrade, not a backoff later.
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, c.backoffMax)
	}
}

// catchUp pages until obligation 3 says it is done: a has_more:false page
// whose request was issued after the most recent trigger.
func (c *Client) catchUp(ctx context.Context) error {
	next := int64(-1) // -1: start from the cursor
	for {
		c.mu.Lock()
		issued := c.gen
		from0 := c.from0 && next < 0
		if from0 {
			c.from0 = false
		}
		c.mu.Unlock()

		c.j.mu.Lock()
		epoch := c.j.wipes
		after := next
		if after < 0 {
			after = c.j.cursor
		}
		c.j.mu.Unlock()
		if from0 {
			after = 0
		}

		page, err := c.fetch(ctx, after)
		if err != nil {
			return err
		}
		applied := c.applyPage(page, epoch)

		c.mu.Lock()
		c.stats.Pages++
		done := false
		switch {
		case !applied:
			next = -1 // a wipe intervened: start again from the new cursor
		case page.HasMore:
			next = page.LogSeq
		case issued == c.gen || c.cfg.Faults.EndCatchUpEarly:
			c.doneGen = c.gen
			done = true
		default:
			next = -1 // a trigger arrived after this request: ask again
		}
		c.mu.Unlock()
		c.notify()
		if done || c.killed.Load() {
			return nil
		}
	}
}

// fetch is one GET /sync, decoded by the generated validating decoder.
func (c *Client) fetch(ctx context.Context, after int64) (wire.SyncResponse, error) {
	c.mu.Lock()
	if c.connecting {
		c.stats.SyncsBeforeReady++
	}
	c.mu.Unlock()

	q := url.Values{"after": {strconv.FormatInt(after, 10)}}
	if c.cfg.SyncLimit > 0 {
		q.Set("limit", strconv.Itoa(c.cfg.SyncLimit))
	}
	rctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.httpBase+"/sync?"+q.Encode(), nil)
	if err != nil {
		return wire.SyncResponse{}, fmt.Errorf("client: sync: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return wire.SyncResponse{}, fmt.Errorf("client: sync: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSyncBody))
	if err != nil {
		return wire.SyncResponse{}, fmt.Errorf("client: sync: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if len(body) > 256 {
			body = body[:256]
		}
		return wire.SyncResponse{}, fmt.Errorf("client: sync: %s: %s", resp.Status, body)
	}
	var page wire.SyncResponse
	if err := json.Unmarshal(body, &page); err != nil {
		return wire.SyncResponse{}, fmt.Errorf("client: sync: decode: %w", err)
	}
	return page, nil
}
