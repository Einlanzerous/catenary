package store

// CANT-21 — fanout across instances, the payload's contents, and the cap.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FANOUT WORKS ACROSS INSTANCES, which is what two connections to one Postgres
// are: an instance is a process with its own pool, and nothing about the
// delivery cares which process opened the socket.
//
// Two listeners, because the interesting property is not that one receives it
// but that BOTH do — a fanout that delivered to whichever listener asked first
// would pass a single-listener test and lose every message for every member not
// connected to that instance.
func TestANotificationReachesEveryListeningInstance(t *testing.T) {
	ctx, pool := freshDB(t)
	dsn := testDSN(t)

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	got := make(chan NotifyPayload, 4)
	for i := 0; i < 2; i++ {
		l := &Listener{
			DSN: dsn, Channel: NotifyChannel, Logger: discardLogger(),
			OnNotify: func(_ context.Context, p NotifyPayload) { got <- p },
		}
		go func() { _ = l.Run(runCtx) }()
	}
	// LISTEN has to be registered before the NOTIFY is raised — Postgres does
	// not deliver to a session that was not listening at the time, which is the
	// same property OnGap exists for. Waited for rather than slept past.
	waitForListeners(ctx, t, pool, 2)

	conv := uuid.New()
	notify(ctx, t, pool, NotifyPayload{ConversationID: conv, Seq: 41})

	for i := 0; i < 2; i++ {
		select {
		case p := <-got:
			if p.ConversationID != conv || p.Seq != 41 {
				t.Errorf("listener %d received %+v, want conversation %s seq 41", i, p, conv)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("listener %d never received the notification", i)
		}
	}
}

// A NOTIFICATION DOES NOT EXIST UNTIL ITS TRANSACTION COMMITS, and that is the
// property CANT-18's ruling 2 rests on — pg_notify goes inside SendMessage's
// own transaction precisely because Postgres holds delivery until commit, so a
// notification cannot exist without its message or the reverse.
//
// Asserted here rather than trusted, because the whole placement of that call
// is justified by it. The rollback half is the one that would be quietly wrong:
// a notification delivered for a message that never committed sends every
// instance to read a row that is not there.
func TestANotificationIsHeldUntilCommitAndDroppedOnRollback(t *testing.T) {
	ctx, pool := freshDB(t)
	dsn := testDSN(t)

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	got := make(chan NotifyPayload, 4)
	l := &Listener{
		DSN: dsn, Channel: NotifyChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p NotifyPayload) { got <- p },
	}
	go func() { _ = l.Run(runCtx) }()
	waitForListeners(ctx, t, pool, 1)

	rolled := uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	payload, err := NotifyPayload{ConversationID: rolled, Seq: 1}.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, payload); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}
	select {
	case p := <-got:
		t.Fatalf("a notification arrived before its transaction committed: %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// A committed one behind it, so the assertion cannot pass by the listener
	// being broken: if this arrives and the rolled-back one never does, the
	// ordering proves the drop rather than a timeout doing it.
	committed := uuid.New()
	notify(ctx, t, pool, NotifyPayload{ConversationID: committed, Seq: 2})

	select {
	case p := <-got:
		if p.ConversationID == rolled {
			t.Error("a rolled-back notification was delivered")
		}
		if p.ConversationID != committed {
			t.Errorf("received %+v, want the committed one", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the committed notification never arrived")
	}
}

// A payload from somewhere else does not take fanout down. Any process may
// NOTIFY on a channel name, and a listener that died on the first unparseable
// string would be a denial of service anyone with a database connection could
// trigger.
func TestAMalformedPayloadIsSkippedRatherThanFatal(t *testing.T) {
	ctx, pool := freshDB(t)
	dsn := testDSN(t)

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	got := make(chan NotifyPayload, 4)
	l := &Listener{
		DSN: dsn, Channel: NotifyChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p NotifyPayload) { got <- p },
	}
	go func() { _ = l.Run(runCtx) }()
	waitForListeners(ctx, t, pool, 1)

	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, "not json at all"); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}
	conv := uuid.New()
	notify(ctx, t, pool, NotifyPayload{ConversationID: conv, Seq: 7})

	select {
	case p := <-got:
		if p.ConversationID != conv {
			t.Errorf("received %+v, want the well-formed one", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the listener stopped after a malformed payload")
	}
}

// A RECONNECT IS A HOLE, THE LISTENER SAYS SO, AND IT SAYS SO ONLY ONCE IT IS
// LISTENING AGAIN.
//
// Postgres does not queue notifications for a disconnected listener, so
// everything raised while the connection was down is gone. The layer above
// answers a gap by resyncing from its cursor — which means the ORDER of the two
// is the correctness of the whole callback. Announced before the subscription
// is back, the sequence is: resync reads to head N, a message commits with
// nobody listening, the subscription registers. That message is on neither
// path and no second gap is raised to say so.
//
// So the assertion that matters is not "a gap happened", it is "a gap happened
// while a subscription was live" — checked from inside OnGap itself, which is
// the only moment the ordering is observable.
//
// WHAT THIS TEST DELIBERATELY DOES NOT ASSERT is that the notification raised
// during the outage is lost. An earlier version claimed to, and could not: it
// read through a filter that discarded everything but the payload it was
// waiting for, so the check was unreachable. It is also not a property to
// assert — pg_terminate_backend signals the backend and returns without waiting
// for it to exit, so a notification raised immediately after it can genuinely
// reach the still-dying session. Losing it is the ordinary case, not a
// guarantee, and the gap exists precisely because the loss cannot be predicted.
func TestAReconnectReportsAGapOnlyOnceItIsListeningAgain(t *testing.T) {
	ctx, pool := freshDB(t)
	dsn := testDSN(t)

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	got := make(chan NotifyPayload, 8)

	type gap struct{ listening int }
	gaps := make(chan gap, 8)
	l := &Listener{
		DSN: dsn, Channel: NotifyChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p NotifyPayload) { got <- p },
		OnGap: func(context.Context) {
			// Counted from another connection, at the instant the gap is
			// announced. Zero here means the resync this triggers has a window
			// after it in which nothing is subscribed.
			var n int
			mustScan(t, pool.QueryRow(ctx, `
				SELECT count(*) FROM pg_stat_activity
				 WHERE datname = current_database() AND query ILIKE 'LISTEN %'`), &n)
			gaps <- gap{listening: n}
		},
	}
	go func() { _ = l.Run(runCtx) }()
	waitForListeners(ctx, t, pool, 1)

	before := uuid.New()
	notify(ctx, t, pool, NotifyPayload{ConversationID: before, Seq: 1})
	awaitNotify(t, got, before, "the notification before the outage")

	// NO GAP ON THE FIRST SUBSCRIPTION: there was nothing before it to have
	// missed, and a signal that fires on every boot is one the layer above
	// learns to ignore.
	//
	// Checked HERE rather than right after waitForListeners, and the ordering
	// is not cosmetic: OnGap is raised from inside the ready callback, so a
	// spurious one would still be in flight while pg_stat_activity already
	// showed the LISTEN. A delivered notification is proof the listener is past
	// that callback and into its wait loop, which makes the empty channel mean
	// something.
	select {
	case <-gaps:
		t.Fatal("a gap was reported before anything had been missed")
	default:
	}

	if _, err := pool.Exec(ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = current_database() AND query ILIKE 'LISTEN %'`); err != nil {
		t.Fatalf("terminate the listener's backend: %v", err)
	}

	select {
	case g := <-gaps:
		if g.listening == 0 {
			t.Error("the gap was announced with nothing subscribed — a resync " +
				"triggered by it has a window after it that is on neither path")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the listener reconnected without reporting a gap")
	}

	// And it is really listening: delivery resumes.
	after := uuid.New()
	notify(ctx, t, pool, NotifyPayload{ConversationID: after, Seq: 3})
	awaitNotify(t, got, after, "the notification after the reconnect")
}

// awaitNotify reads until the wanted payload arrives. It DISCARDS anything
// else, which is fine here and was not fine when a caller tried to conclude
// something from what it returned — see the comment above.
func awaitNotify(t *testing.T, ch <-chan NotifyPayload, want uuid.UUID, what string) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case p := <-ch:
			if p.ConversationID == want {
				return
			}
		case <-deadline:
			t.Fatalf("%s never arrived", what)
		}
	}
}

// THE CAP IS A TEST RATHER THAN A COMMENT, which is the ticket's own wording,
// and the number it is measured against is the real payload rather than a
// guess: the widest conversation id there is, and the largest seq the wire
// admits.
func TestThePayloadIsNowhereNearTheCap(t *testing.T) {
	// Seq's schema maximum, 2^53-1 — the largest value a valid frame can carry,
	// so the widest this payload can legitimately get.
	const seqMax = 9007199254740991
	widest := NotifyPayload{ConversationID: uuid.Max, Seq: seqMax}

	encoded, err := widest.Encode()
	if err != nil {
		t.Fatalf("the widest legal payload does not encode: %v", err)
	}
	if len(encoded) >= NotifyPayloadMax {
		t.Fatalf("the widest payload is %d bytes, at or over the %d limit", len(encoded), NotifyPayloadMax)
	}
	// Stated as a ratio because the margin is the actual claim: this is bounded
	// by construction, not by being careful.
	if len(encoded) > NotifyPayloadMax/10 {
		t.Errorf("the payload is %d bytes, more than a tenth of the cap — it is "+
			"supposed to be ids only", len(encoded))
	}
	t.Logf("widest payload: %d bytes of %d (%s)", len(encoded), NotifyPayloadMax, encoded)
}

// And the cap is where Postgres says it is, which a Go constant cannot assert
// on its own. The encoder's own refusal is unreachable through NotifyPayload —
// two fixed-width fields cannot reach 8,000 bytes — so the half that can be
// proved today is that the number in notify.go describes a real limit. The
// struct guard below is what keeps the other half unreachable.
func TestPostgresRefusesAPayloadOverTheDocumentedLimit(t *testing.T) {
	ctx, pool := freshDB(t)

	// One byte under, and one byte over, so this pins the boundary rather than
	// showing that something very large fails.
	under := strings.Repeat("x", NotifyPayloadMax-1)
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, under); err != nil {
		t.Errorf("Postgres refused %d bytes, which the constant says is legal: %v",
			len(under), err)
	}
	over := strings.Repeat("x", NotifyPayloadMax)
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, over); err == nil {
		t.Errorf("Postgres accepted %d bytes; NotifyPayloadMax is describing a "+
			"limit that is not there", len(over))
	}

	// And the encoder refuses at the same boundary, before Postgres has to —
	// which is what keeps the failure out of SendMessage's transaction, where
	// it would refuse a member's message over a broadcast detail. Checked
	// through withinNotifyCap, because Encode cannot reach its own refusal
	// today and an unreachable branch is an unchecked one.
	if err := withinNotifyCap([]byte(under)); err != nil {
		t.Errorf("the encoder refused %d bytes, one under the limit: %v", len(under), err)
	}
	err := withinNotifyCap([]byte(over))
	if !errors.Is(err, ErrNotifyTooLarge) {
		t.Errorf("withinNotifyCap(%d bytes) = %v, want ErrNotifyTooLarge", len(over), err)
	}
	if _, err := (NotifyPayload{ConversationID: uuid.Max, Seq: 1}).Encode(); err != nil {
		t.Fatalf("a legal payload was refused: %v", err)
	}
}

// THE PAYLOAD CARRIES IDS ONLY, and this is the guard that keeps it that way.
//
// A byte-size test cannot express this: adding a `text` field would still
// encode to well under 8,000 bytes and every size assertion would stay green
// while message bodies started travelling through a channel with no retention
// story, landing in pg_stat_activity and in any log that records a notify. That
// is the D1 honesty problem arriving through the back door, so the check is on
// the STRUCT'S SHAPE rather than on its output.
//
// The probe at the end is what proves the guard bites: a planted third field
// has to make it fail, or it is asserting nothing.
func TestTheNotifyPayloadCannotGrowAContentField(t *testing.T) {
	const want = "ConversationID uuid.UUID, Seq int64"

	got, err := notifyPayloadFields("notify.go")
	if err != nil {
		t.Fatalf("read notify.go: %v", err)
	}
	if got != want {
		t.Errorf("NotifyPayload is now {%s}, and it is supposed to be {%s}.\n"+
			"This struct is server-internal and unversioned, so nothing else "+
			"will stop a message body from reaching a NOTIFY. If a new id is "+
			"genuinely needed, change the line above deliberately.", got, want)
	}

	// The probe: the same check over a planted source with a body field.
	planted := `package store
type NotifyPayload struct {
	ConversationID uuid.UUID ` + "`json:\"conversation_id\"`" + `
	Seq            int64     ` + "`json:\"seq\"`" + `
	Text           string    ` + "`json:\"text\"`" + `
}`
	if got, err := notifyPayloadFieldsIn("planted.go", planted); err != nil {
		t.Fatalf("parse the planted source: %v", err)
	} else if got == want {
		t.Error("a planted Text field did not change the answer — the guard reads nothing")
	}

	// The second probe is the one that caught a real blind spot: an EMBEDDED
	// struct has no field name, so a guard that only walked Names read this as
	// unchanged while json.Marshal inlined whatever the embedded type carries.
	embedded := `package store
type NotifyPayload struct {
	ConversationID uuid.UUID ` + "`json:\"conversation_id\"`" + `
	Seq            int64     ` + "`json:\"seq\"`" + `
	MessageBody
}`
	if got, err := notifyPayloadFieldsIn("embedded.go", embedded); err != nil {
		t.Fatalf("parse the embedded source: %v", err)
	} else if got == want {
		t.Error("an embedded struct did not change the answer — the guard is " +
			"blind to exactly the shape that inlines a body into the payload")
	}
}

func notifyPayloadFields(path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return notifyPayloadFieldsIn(path, string(src))
}

func notifyPayloadFieldsIn(name, src string) (string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		return "", err
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "NotifyPayload" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, field := range st.Fields.List {
			// AN EMBEDDED FIELD HAS NO NAME, and skipping it is how this guard
			// would have missed the thing it exists for: embedding a struct
			// with a Text field leaves the named fields identical while
			// json.Marshal inlines the body straight into the payload. Recorded
			// by its type, which is the only name it has.
			if len(field.Names) == 0 {
				out = append(out, types(field.Type))
				continue
			}
			for _, id := range field.Names {
				out = append(out, id.Name+" "+types(field.Type))
			}
		}
		return false
	})
	return strings.Join(out, ", "), nil
}

// types renders a field's type as written, which is all this guard needs.
func types(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return types(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + types(t.X)
	default:
		return "?"
	}
}

// --------------------------------------------------------------------------
// fixtures

func notify(ctx context.Context, t *testing.T, pool *pgxpool.Pool, p NotifyPayload) {
	t.Helper()
	payload, err := p.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, payload); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}
}

// waitForListeners blocks until n sessions are listening on the channel.
//
// pg_listening_channels() reports the CALLING session only, so this reads
// pg_stat_activity's wait state instead: a session parked in
// WaitForNotification is idle, and counting those is the only way one
// connection can see another's LISTEN. Polled rather than slept, because a
// sleep long enough to be reliable on a loaded CI runner is a sleep every run
// pays for.
func waitForListeners(ctx context.Context, t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var count int
		mustScan(t, pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event = 'ClientRead'
			   AND query ILIKE 'LISTEN %'`), &count)
		if count >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d listeners registered within the deadline", count, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A SUBSCRIPTION THAT NEVER SUCCEEDS RAISES NO GAP, however many times it
// retries.
//
// This is the boot case: the shared Postgres is still starting — which
// ConnectWithRetry exists because it happens — so the first attempt fails and
// the second is still the FIRST subscription. Announcing a gap there tells a
// consumer that has never received anything that it has missed something, and a
// signal that fires on every boot is one the layer above learns to ignore.
//
// It holds by CONSTRUCTION rather than by the `connected` flag, and that is
// worth being precise about: OnGap is raised from inside the callback that runs
// after LISTEN succeeds, so a failed attempt has no path to it at all. Flipping
// `connected` to true does not make this test fail — what it protects against
// is the earlier shape, where the gap was raised at the top of the loop and a
// failed attempt did reach it. `connected` still carries the other half, and
// TestAReconnectReportsAGapOnlyOnceItIsListeningAgain is what holds that one.
//
// Port 1 refuses immediately, so this runs several attempts inside the window
// rather than waiting on a timeout.
func TestAFailedFirstConnectionIsNotAGap(t *testing.T) {
	testDSN(t) // consistent skip with the rest of the database tests

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	gaps := make(chan struct{}, 8)
	l := &Listener{
		DSN:     "postgres://postgres@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1",
		Channel: NotifyChannel, Logger: discardLogger(),
		OnGap: func(context.Context) { gaps <- struct{}{} },
	}
	go func() { _ = l.Run(ctx) }()

	// Long enough for the 250ms backoff to have produced several attempts.
	time.Sleep(1500 * time.Millisecond)
	select {
	case <-gaps:
		t.Error("a gap was announced while the FIRST subscription was still failing; " +
			"nothing had been received, so nothing could have been missed")
	default:
	}
}

// A Listener built without a logger does not panic on its first disconnect.
//
// Logger is exported with no constructor to default it, so the nil is reachable
// — and the only place it was dereferenced is the reconnect path, which is the
// least-exercised code in this file and the worst place to find a nil pointer.
func TestAListenerWithNoLoggerStillLogs(t *testing.T) {
	if (&Listener{}).logger() == nil {
		t.Error("logger() returned nil; the reconnect path dereferences it")
	}
	custom := discardLogger()
	if (&Listener{Logger: custom}).logger() != custom {
		t.Error("logger() did not return the one it was given")
	}
}
