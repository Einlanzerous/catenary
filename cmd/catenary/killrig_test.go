package main

// CANT-102 — the kill rig: the process setup() builds, run so that it can be
// killed, and so that the faults the kill test needs are planted rather than
// raced. Nothing here hooks into the code under test; every fault is applied
// from outside, at a boundary a real deployment also has.
//
//	pgProxy     sits between the NOTIFY listener and Postgres. A sever drops the
//	            listener's connection AND HOLDS IT DROPPED while messages commit,
//	            so they are missed by construction. pg_terminate_backend — the
//	            plain rig's tool — cannot hold it: the listener is back within
//	            250 ms and a commit may or may not land in the window.
//	switchboard owns the listening address across process lifetimes. A process
//	            is killed by closing every connection it ever accepted, hijacked
//	            sockets included (http.Server does not track those), with no
//	            close frame, no hub.Shutdown and no listener drain. A restart is
//	            setup() again over the same database, on the same address.
//	gates       hold one /sync response after the real handler has served it,
//	            and one upgrade until the same client's /sync has arrived. The
//	            first plants a listener gap while a /sync is in flight; the
//	            second makes "/sync issued concurrently with the upgrade"
//	            falsifiable — a client that waits for `ready` deadlocks it.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// --- the listener's proxy ----------------------------------------------------

type pgProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	down  bool
	conns map[net.Conn]struct{}
}

func newPGProxy(t *testing.T, target string) *pgProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pgProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		p.sever()
	})
	return p
}

func (p *pgProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			_ = c.Close()
			continue
		}
		go p.pipe(c)
	}
}

// track registers a connection, or refuses it while the proxy is severed.
func (p *pgProxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *pgProxy) pipe(c net.Conn) {
	up, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	if !p.track(up) {
		_ = up.Close()
		_ = c.Close()
		return
	}
	go func() {
		_, _ = io.Copy(up, c)
		_ = up.Close()
		_ = c.Close()
	}()
	_, _ = io.Copy(c, up)
	_ = c.Close()
	_ = up.Close()
}

// sever drops every proxied connection and refuses new ones until restore.
// When it returns, nothing the database sends can reach the listener.
func (p *pgProxy) sever() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

func (p *pgProxy) restore() {
	p.mu.Lock()
	p.down = false
	p.mu.Unlock()
}

// --- the switchboard -----------------------------------------------------------

type switchboard struct {
	ln  net.Listener
	mu  sync.Mutex
	cur *procListener
}

func newSwitchboard(t *testing.T) *switchboard {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &switchboard{ln: ln}
	go b.accept()
	t.Cleanup(func() { _ = ln.Close() })
	return b
}

func (b *switchboard) base() string { return "http://" + b.ln.Addr().String() }

// accept hands each connection to the live process, or closes it: a dial
// while no process is up fails the way a dial to a stopped server does.
func (b *switchboard) accept() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		p := b.cur
		b.mu.Unlock()
		if p == nil || !p.offer(c) {
			_ = c.Close()
		}
	}
}

func (b *switchboard) open() *procListener {
	p := &procListener{addr: b.ln.Addr(), ch: make(chan net.Conn), closed: make(chan struct{})}
	b.mu.Lock()
	b.cur = p
	b.mu.Unlock()
	return p
}

func (b *switchboard) refuse(p *procListener) {
	b.mu.Lock()
	if b.cur == p {
		b.cur = nil
	}
	b.mu.Unlock()
}

// procListener is one process's view of the address.
type procListener struct {
	addr   net.Addr
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once

	mu    sync.Mutex
	dead  bool
	conns []net.Conn
}

func (p *procListener) offer(c net.Conn) bool {
	select {
	case p.ch <- c:
		return true
	case <-p.closed:
		return false
	}
}

func (p *procListener) Accept() (net.Conn, error) {
	select {
	case c := <-p.ch:
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.dead {
			_ = c.Close()
			return nil, net.ErrClosed
		}
		p.conns = append(p.conns, c)
		return c, nil
	case <-p.closed:
		return nil, net.ErrClosed
	}
}

func (p *procListener) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func (p *procListener) Addr() net.Addr { return p.addr }

// kill closes the listener and every connection it accepted, with no close
// handshake on any of them.
func (p *procListener) kill() {
	_ = p.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = true
	for _, c := range p.conns {
		_ = c.Close()
	}
}

// --- gates ---------------------------------------------------------------------

type heldSync struct {
	read    chan struct{} // closed once the real handler has served the page
	release chan struct{} // close to write it to the client
}

type syncMark struct {
	ch   chan struct{}
	once sync.Once
}

type gates struct {
	mu       sync.Mutex
	holds    map[string]*heldSync
	marks    map[string]*syncMark // bearer → closed by that bearer's next /sync
	upgrades map[string]*syncMark // bearer → its next upgrade waits on this
	held     int
	failure  string
}

func newGates() *gates {
	return &gates{holds: map[string]*heldSync{}, marks: map[string]*syncMark{}, upgrades: map[string]*syncMark{}}
}

// holdNextSync holds the response to the next /sync this token issues, after
// the real handler has read the page.
func (g *gates) holdNextSync(token string) *heldSync {
	h := &heldSync{read: make(chan struct{}), release: make(chan struct{})}
	g.mu.Lock()
	g.holds[token] = h
	g.mu.Unlock()
	return h
}

// upgradeAfterSync holds this token's next upgrade until a /sync from it has
// arrived — one arriving after this call.
func (g *gates) upgradeAfterSync(token string) {
	m := &syncMark{ch: make(chan struct{})}
	g.mu.Lock()
	g.marks[token], g.upgrades[token] = m, m
	g.mu.Unlock()
}

func (g *gates) upgradesHeld() (int, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held, g.failure
}

func (g *gates) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync":
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			g.mu.Lock()
			if m := g.marks[tok]; m != nil {
				m.once.Do(func() { close(m.ch) })
				delete(g.marks, tok)
			}
			h := g.holds[tok]
			delete(g.holds, tok)
			g.mu.Unlock()
			if h == nil {
				next.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			close(h.read)
			select {
			case <-h.release:
			case <-r.Context().Done():
				return
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
		case "/ws":
			tok := upgradeToken(r)
			g.mu.Lock()
			m := g.upgrades[tok]
			delete(g.upgrades, tok)
			g.mu.Unlock()
			if m != nil {
				select {
				case <-m.ch:
					g.mu.Lock()
					g.held++
					g.mu.Unlock()
				case <-time.After(10 * time.Second):
					// Recorded rather than t.Errorf'd: this is a handler
					// goroutine, and the test reads it at its next checkpoint.
					g.mu.Lock()
					g.failure = "an upgrade waited 10 s for a /sync from the same client that never came: the client's /sync is not issued beside its upgrade"
					g.mu.Unlock()
					http.Error(w, "held", http.StatusServiceUnavailable)
					return
				}
			}
			next.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func upgradeToken(r *http.Request) string {
	for _, line := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, v := range strings.Split(line, ",") {
			if v = strings.TrimSpace(v); strings.HasPrefix(v, "catenary.token.") {
				return strings.TrimPrefix(v, "catenary.token.")
			}
		}
	}
	return ""
}

// --- the rig -------------------------------------------------------------------

type process struct {
	d         deps
	board     *switchboard
	ln        *procListener
	stopL     context.CancelFunc
	lDone     chan struct{}
	serveDone chan struct{}
	once      sync.Once
}

// kill stops the process the way a crash does: no drain, no close frames, no
// hub.Shutdown. The database is untouched.
func (p *process) kill() {
	p.once.Do(func() {
		p.board.refuse(p.ln)
		p.ln.kill()
		p.stopL()
		<-p.lDone
		<-p.serveDone
	})
}

type killRig struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	st    *store.Store
	cfg   config.Config
	log   *recorder
	proxy *pgProxy
	board *switchboard
	gates *gates
	proc  *process
}

// newKillRig builds a fresh database, a proxy for the listener, and the first
// process. tune adjusts the config every process is built from.
func newKillRig(t *testing.T, tune func(*config.Config)) *killRig {
	t.Helper()
	log := &recorder{}
	var cfg config.Config
	var proxy *pgProxy
	ctx, pool, st, _ := processFixtureWith(t, slog.New(log), func(c *config.Config) {
		u, err := url.Parse(c.DatabaseURL)
		if err != nil {
			t.Fatalf("CATENARY_TEST_DATABASE_URL: %v", err)
		}
		proxy = newPGProxy(t, u.Host)
		u.Host = proxy.ln.Addr().String()
		c.DatabaseURL = u.String()
		if tune != nil {
			tune(c)
		}
		cfg = *c
	})
	k := &killRig{t: t, ctx: ctx, pool: pool, st: st, cfg: cfg, log: log, proxy: proxy,
		board: newSwitchboard(t), gates: newGates()}
	k.start()
	return k
}

// start runs a new process — setup() over the same store — and returns once
// its listener is registered.
func (k *killRig) start() {
	k.t.Helper()
	since := dbNow(k.ctx, k.t, k.pool)
	d := setup(k.cfg, slog.New(k.log), k.st)
	p := &process{d: d, board: k.board, ln: k.board.open(), lDone: make(chan struct{}), serveDone: make(chan struct{})}
	srv := &http.Server{Handler: k.gates.wrap(d.router), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		defer close(p.serveDone)
		_ = srv.Serve(p.ln)
	}()
	lctx, stop := context.WithCancel(k.ctx)
	p.stopL = stop
	go func() {
		defer close(p.lDone)
		_ = d.listener.Run(lctx)
	}()
	k.proc = p
	k.t.Cleanup(p.kill)
	(&rig{t: k.t, ctx: k.ctx, pool: k.pool, since: since}).waitForListen()
}

func (k *killRig) base() string { return k.board.base() }

func (k *killRig) enroll(user uuid.UUID, name string) wire.EnrollResponse {
	k.t.Helper()
	return (&rig{t: k.t, ctx: k.ctx, st: k.st, d: k.proc.d}).enroll(user, name)
}

func (k *killRig) head() int64 {
	k.t.Helper()
	h, err := k.st.Head(k.ctx)
	if err != nil {
		k.t.Fatal(err)
	}
	return h
}

func (k *killRig) client(dev wire.EnrollResponse, j *client.Journal, faults client.Faults) *client.Client {
	k.t.Helper()
	return runClient(k.t, k.ctx, k.base(), dev, j, faults)
}

// --- shared by every CANT-102 test ------------------------------------------------

// runClient builds a Go client for an enrolled device and runs it until the
// test ends, or until the test kills it.
func runClient(t *testing.T, ctx context.Context, base string, dev wire.EnrollResponse, j *client.Journal, faults client.Faults) *client.Client {
	t.Helper()
	// A FIRST RUN ENROLLS THE JOURNAL; A RESTART DOES NOT (CANT-121). Every
	// restart in these tests passes the same `dev` it started with, and on a
	// journal that already holds a credential that argument is ignored — the
	// journal's pair may be a rotation ahead of it.
	if j == nil {
		j = client.NewJournal()
	}
	if _, held := j.Credential(); !held {
		cred, err := client.CredentialFromEnroll(dev)
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Enroll(cred); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(client.Config{
		BaseURL:    base,
		ClientInfo: "cant-102-test", Journal: j, Faults: faults,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	t.Cleanup(func() {
		c.Kill()
		<-done
	})
	return c
}

// awaitClient waits, with a bound, for a client to reach a state.
func awaitClient(t *testing.T, c *client.Client, what string, pred func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Await(ctx, pred); err != nil {
		t.Fatalf("waiting for %s: %v\nstatus: %+v", what, err, c.Status())
	}
}

// serverLog is the store's own record of what was committed that a viewer may
// see: every message in every conversation they are a member of.
func serverLog(ctx context.Context, t *testing.T, pool *pgxpool.Pool, viewer uuid.UUID) []client.LogEntry {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT m.id, m.conversation_id, m.seq, m.log_seq
		  FROM messages m
		  JOIN conversation_members cm
		    ON cm.conversation_id = m.conversation_id AND cm.user_id = $1
		 ORDER BY m.log_seq`, viewer)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []client.LogEntry
	for rows.Next() {
		var id, conv uuid.UUID
		var e client.LogEntry
		if err := rows.Scan(&id, &conv, &e.Seq, &e.LogSeq); err != nil {
			t.Fatal(err)
		}
		e.ID, e.ConversationID = wid(id), wid(conv)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func wid(id uuid.UUID) wire.Uuid { return wire.Uuid(id.String()) }

func containsAll(set, want []wire.Uuid) bool {
	in := map[wire.Uuid]bool{}
	for _, id := range set {
		in[id] = true
	}
	for _, id := range want {
		if !in[id] {
			return false
		}
	}
	return true
}
