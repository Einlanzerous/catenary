package main

// CANT-42 row d — the Dart cohort through the soak: the twins of
// tscohort_test.go's three lanes, with the Dart client's transport over its
// own driver (dart-client/bin/driver.dart) in place of the TypeScript one, and
// a test that drives every command of that driver through the SAME Go adapter
// the TypeScript driver uses.
//
// GATED as the TypeScript lanes are. CATENARY_TEST_DATABASE_URL, and
// CATENARY_DART_DRIVER, the built driver: unset skips, so the ordinary
// `go test ./...` sweep on a machine with no Dart stays green, but SET AND
// MISSING FAILS, and so does a driver that cannot be executed. verify.sh's
// Dart cohort step builds it and sets the variable.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/client"
	"github.com/magos/catenary/internal/wire"
)

// dartDriverExe is the built Dart driver, or a skip. A driver that is named
// and cannot be run is a failure here, before any lane hands it a client.
func dartDriverExe(t *testing.T) string {
	t.Helper()
	p := os.Getenv("CATENARY_DART_DRIVER")
	if p == "" {
		t.Skip("CATENARY_DART_DRIVER not set; skipping the Dart cohort (verify.sh builds the driver and sets it)")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("CATENARY_DART_DRIVER=%s: %v — the lane was asked to run the Dart driver and there is none; build it with `dart build cli -t bin/driver.dart -o build/driver` in dart-client/", p, err)
	}
	// `--probe` opens a SQLite database in memory through the journal's own
	// open and exits 0. A file that is there and will not execute, or a
	// bundle that cannot load its SQLite library, fails here — not later, as
	// N clients that never became ready.
	if out, err := exec.Command(p, "--probe").CombinedOutput(); err != nil {
		t.Fatalf("CATENARY_DART_DRIVER=%s cannot be executed, or cannot open SQLite: %v\n%s", p, err, out)
	}
	return p
}

func dartConfig(t *testing.T, cohort string) Config {
	t.Helper()
	exe := dartDriverExe(t)
	cfg := tinyConfig(soakDBFixture(t))
	cfg.Cohort, cfg.DartDriver = cohort, exe
	return cfg
}

// The control the fault below needs: the same lane, unbroken, passes clean,
// with every client compared and every client the Dart one.
func TestDartSoakBaselinePasses(t *testing.T) {
	res := runSoak(context.Background(), dartConfig(t, cohortDart))
	t.Log(reportString(res.Report))
	if res.Verdict != VerdictPass {
		t.Fatalf("verdict = %s, want pass", res.Verdict)
	}
	if res.Report.ComparisonsRun != res.Report.N {
		t.Errorf("comparisons run = %d, want %d", res.Report.ComparisonsRun, res.Report.N)
	}
	for _, c := range res.Report.Clients {
		if c.Cohort != cohortDart {
			t.Errorf("client %d ran as %q, want dart", c.Index, c.Cohort)
		}
	}
}

// The Go and Dart clients in one room, judged by one Compare.
func TestMixedDartSoakBaselinePasses(t *testing.T) {
	cfg := dartConfig(t, cohortMixedDart)
	cfg.N = 4
	res := runSoak(context.Background(), cfg)
	t.Log(reportString(res.Report))
	if res.Verdict != VerdictPass {
		t.Fatalf("verdict = %s, want pass", res.Verdict)
	}
	n := map[string]int{}
	for _, c := range res.Report.Clients {
		n[c.Cohort]++
	}
	if n[cohortGo] != 2 || n[cohortDart] != 2 {
		t.Errorf("cohorts %v, want two of each", n)
	}
}

// COUNTER-PROOF — `dedupeByLogSeq`, through the soak, for the Dart client.
// Client 0 counts a record as new exactly when its log_seq is above the
// cursor, so a /sync page re-carrying a message it already held live — which
// the reconnect storm guarantees — counts it twice. Compare ran, and is
// dirty, and the duplicate is reported by the faulted client's own comparison.
func TestBrokenDartRunCountsAsServerFailure(t *testing.T) {
	cfg := dartConfig(t, cohortDart)
	cfg.debugFaults = map[int]client.Faults{0: {DedupeByLogSeq: true}}
	res := runSoak(context.Background(), cfg)
	t.Log(reportString(res.Report))
	if res.Verdict != VerdictServerFailure {
		t.Fatalf("verdict = %s, want server_failure", res.Verdict)
	}
	if res.Report.ComparisonsRun != res.Report.N {
		t.Errorf("comparisons run = %d, want %d — the fault must not stop the comparison from running", res.Report.ComparisonsRun, res.Report.N)
	}
	for _, c := range res.Report.Clients {
		switch {
		case c.Index == 0 && (!c.Compared || len(c.Compare.Duplicated) == 0):
			t.Errorf("client 0's own comparison reports no duplicate — this test did not exercise what it claims: %s", c.Compare)
		case c.Index != 0 && c.Compared && !c.Compare.Clean():
			t.Errorf("client %d, unbroken, is dirty: %s", c.Index, c.Compare)
		}
	}
}

// THE DART DRIVER ANSWERS EVERY COMMAND in the driver.ts header — `compose`
// and `outbox` are TestADartComposedTextSurvivesTheDriversDeath's, below; S5
// with a Dart device is CANT-188's lane and not here — through the adapter the TypeScript driver is run by:
// newTSDriver, with a different command line. Each command is called and its
// effect observed against a real server, in the order a rig uses them.
func TestTheDartDriverAnswersEveryCommand(t *testing.T) {
	exe := dartDriverExe(t)
	h := startLocalServer(t, soakDBFixture(t))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	room, readerID, authorID := uuid.New(), uuid.New(), uuid.New()
	if _, err := h.pool.Exec(ctx, `INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', 'the dart driver')`, room); err != nil {
		t.Fatal(err)
	}
	for id, handle := range map[uuid.UUID]string{readerID: "dart-reader", authorID: "dart-author"} {
		if err := insertUser(ctx, h.pool, id, handle, handle); err != nil {
			t.Fatal(err)
		}
		if _, err := h.pool.Exec(ctx, `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`, room, id); err != nil {
			t.Fatal(err)
		}
	}
	driver := func(who string, userID uuid.UUID) *tsDriver {
		dev, err := h.enroll(ctx, 0, userID, who)
		if err != nil {
			t.Fatal(err)
		}
		cfg := tsDriverConfig{Script: exe, Native: true, BaseURL: h.baseURL, BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax}
		cfg.enrolled(dev)
		d, err := newTSDriver(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Kill)
		return d
	}
	reader, author := driver("reader", readerID), driver("author", authorID)
	ready := func(d *tsDriver, what string) {
		t.Helper()
		if err := d.Await(ctx, func() bool { s := d.Status(); return s.Ready && s.CaughtUp }); err != nil {
			t.Fatalf("%s: never ready and caught up: %v; status %+v", what, err, d.Status())
		}
	}

	// `read` before `start`: refused, as one NotConnected.
	read := wire.ClientRead{ConversationID: wid(room), UpToSeq: 1}
	if err := reader.Read(ctx, read); !errors.Is(err, client.ErrNotConnected) {
		t.Fatalf("read before start = %v, want ErrNotConnected", err)
	}
	// A command the driver does not have is answered, and named; and the
	// outbox's two are refused before `start`, when there is no outbox.
	var de *tsDriverError
	if err := reader.call(ctx, "nope", nil, nil); !errors.As(err, &de) || de.Kind != "UnknownCommand" {
		t.Fatalf("an unknown command = %v, want UnknownCommand", err)
	}
	if _, err := reader.Compose(ctx, wid(room), "x"); !errors.As(err, &de) || de.Kind != "NotStarted" {
		t.Fatalf("compose before start = %v, want NotStarted", err)
	}

	// `start` (Run), and `status`.
	for _, d := range []*tsDriver{reader, author} {
		go func() { _ = d.Run(ctx) }()
	}
	ready(reader, "the reader")
	ready(author, "the author")

	// `send`, and its ack; a second send under the same client_id is the same
	// message.
	text := "from the Dart driver"
	frame := wire.ClientSend{ClientID: wid(uuid.New()), ConversationID: wid(room), Text: &text}
	ack, err := author.Send(ctx, frame)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	again, err := author.Send(ctx, frame)
	if err != nil || again.MessageID != ack.MessageID {
		t.Fatalf("the same frame again = %+v, %v; want the first ack's message %s", again, err, ack.MessageID)
	}

	// `snapshot`: the journal in wire JSON, with the evidence log. The reader
	// met the room on the catch-up its first live frame pulled.
	holds := func(d *tsDriver) bool {
		s, err := d.TrySnapshot()
		return err == nil && holdsMessage(s, ack.MessageID) && holdsConversation(s, wid(room))
	}
	for deadline := time.Now().Add(20 * time.Second); !holds(reader); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the reader never held the message and its room")
		}
	}
	snap, err := reader.TrySnapshot()
	if err != nil || len(snap.Counted) != 1 || snap.Counted[0] != ack.MessageID || !snap.HasCursor {
		t.Fatalf("snapshot = %+v (err %v), want the one message counted once and a cursor", snap, err)
	}

	// `read`, on a ready session: the server's marker for the reader moves.
	if err := reader.Read(ctx, read); err != nil {
		t.Fatalf("read: %v", err)
	}
	dev, err := h.enroll(ctx, 1, readerID, "the test's own view")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		c, _, err := servedConversation(ctx, h.baseURL, dev.AccessToken, wid(room))
		if err == nil && c.FirstUnreadSeq == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after reading the only message the server still serves the reader %+v (err %v)", c, err)
		}
	}

	// `catchup`: a trigger, and a page.
	pages := reader.Status().Pages
	reader.CatchUp()
	if err := reader.Await(ctx, func() bool { s := reader.Status(); return s.Pages > pages && s.CaughtUp }); err != nil {
		t.Fatalf("catchup pulled no page: %v", err)
	}

	// `sever`: no close frame, and the transport redials.
	before := reader.Status()
	reader.Sever()
	if err := reader.Await(ctx, func() bool { s := reader.Status(); return s.Readys > before.Readys && s.Ready }); err != nil {
		t.Fatalf("after sever the driver never came back: %v; status %+v", err, reader.Status())
	}
	if s := reader.Status(); s.CloseStatuses[-1] == 0 && s.CloseStatuses[1006] == 0 {
		t.Errorf("after sever the close statuses are %v, want a session that ended with no close frame", s.CloseStatuses)
	}

	// `blackhole`: answered with how many sockets it silenced, and the
	// session stays up — only the heartbeat finds it, which is the kill-test
	// lane's to show.
	var holed struct {
		Holed int `json:"holed"`
	}
	if err := reader.call(ctx, "blackhole", nil, &holed); err != nil || holed.Holed != 1 {
		t.Fatalf("blackhole = %+v, %v; want one socket silenced", holed, err)
	}
	if s := reader.Status(); !s.Connected {
		t.Error("a black hole closed the socket; it must only silence it")
	}

	// `stop`: a clean close, the journal kept, and a later Run is a new
	// transport over it.
	if err := author.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	kept, err := author.TrySnapshot()
	if err != nil || !holdsMessage(kept, ack.MessageID) {
		t.Fatalf("after stop the author's journal = %+v (err %v), want it kept", kept, err)
	}
	if err := author.call(ctx, "status", nil, nil); !errors.As(err, &de) || de.Kind != "NotStarted" {
		t.Errorf("status after stop = %v, want NotStarted", err)
	}
	go func() { _ = author.Run(ctx) }()
	ready(author, "the author, started again")
	if s := author.Status(); s.Readys != 1 {
		t.Errorf("a second start reports %d readys, want a new transport's 1", s.Readys)
	}
	t.Log("the Dart driver answered start, send, read, sever, blackhole, catchup, status, snapshot and stop")
}

// CANT-42 row f — `compose` and `outbox` on the Dart driver, hosting the real
// Dart outbox over the real Dart transport, with the outbox persisted beside
// the journal file: a driver killed after `compose` and relaunched lists the
// entry as unsettled and sends it under the same clientId. The Dart twin of
// TestAComposedTextSurvivesTheDriversDeath, through the same adapter.
func TestADartComposedTextSurvivesTheDriversDeath(t *testing.T) {
	exe := dartDriverExe(t)
	h := startLocalServer(t, soakDBFixture(t))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	room, author := uuid.New(), uuid.New()
	if _, err := h.pool.Exec(ctx, `INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', 'the dart outbox')`, room); err != nil {
		t.Fatal(err)
	}
	if err := insertUser(ctx, h.pool, author, "dart-composer", "dart-composer"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`, room, author); err != nil {
		t.Fatal(err)
	}
	dev, err := h.enroll(ctx, 0, author, "phone")
	if err != nil {
		t.Fatal(err)
	}
	// The device's network is a proxy the test can take away, and its journal
	// a file a second process can reopen.
	proxy := newSeveringProxy(t, targetHost(h.baseURL))
	cfg := tsDriverConfig{
		Script: exe, Native: true, BaseURL: proxy.base(), JournalFile: filepath.Join(t.TempDir(), "journal.sqlite"),
		BackoffMin: soakBackoffMin, BackoffMax: soakBackoffMax,
	}
	cfg.enrolled(dev)
	launch := func() *tsDriver {
		t.Helper()
		d, err := newTSDriver(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Kill)
		go func() { _ = d.Run(ctx) }()
		return d
	}
	committed := func(id wire.Uuid) int {
		t.Helper()
		var n int
		if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE author_id = $1 AND client_id::text = $2`, author, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	d := launch()
	if err := d.Await(ctx, func() bool { return d.Status().Ready }); err != nil {
		t.Fatalf("the driver never became ready: %v", err)
	}
	proxy.hold()
	if err := d.Await(ctx, func() bool { return !d.Status().Ready }); err != nil {
		t.Fatal(err)
	}

	// `compose` with no session: persisted, answered with its clientId, and
	// not sent. `send` in the same position is refused — that is the
	// difference between the two.
	id, err := d.Compose(ctx, wid(room), "composed, then the process died")
	if err != nil {
		t.Fatalf("compose behind a held proxy: %v", err)
	}
	text := "sent, not composed"
	if _, err := d.Send(ctx, wire.ClientSend{ClientID: wid(uuid.New()), ConversationID: wid(room), Text: &text}); !errors.Is(err, client.ErrNotConnected) {
		t.Errorf("send behind a held proxy = %v, want ErrNotConnected", err)
	}
	listed, err := d.Outbox(ctx)
	if err != nil || len(listed) != 1 || listed[0].ClientID != id || listed[0].State != "queued" {
		t.Fatalf("the outbox after compose = %+v (err %v), want the one entry %s, queued", listed, err, id)
	}

	// SIGKILL, and a new process over the same file.
	d.Kill()
	d = launch()
	var left []outboxEntry
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		// `outbox` is refused before `start`, which Run sends.
		if left, err = d.Outbox(ctx); err == nil {
			break
		}
	}
	if err != nil || len(left) != 1 || left[0].ClientID != id || left[0].State != "queued" {
		t.Fatalf("the relaunched driver's outbox = %+v (err %v), want the one entry %s, queued", left, err, id)
	}
	if n := committed(id); n != 0 {
		t.Fatalf("%d rows committed for a text composed behind a held proxy", n)
	}

	proxy.heal()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		left, lerr := d.Outbox(ctx)
		if n := committed(id); lerr == nil && n == 1 && len(left) == 0 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("after the heal: %d rows committed, outbox %+v (err %v); want exactly one row and an empty outbox", n, left, lerr)
		}
	}
}
