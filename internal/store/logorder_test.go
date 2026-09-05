package store

// CANT-19 — the property CANT-14 exists to guarantee, and the only test in this
// package that can falsify it.
//
// The claim: for any two committed messages, ascending `log_seq` implies
// ascending COMMIT order, deployment-wide. What a client depends on is the
// consequence — a reader that has advanced its cursor past N is never
// afterwards shown a message below N, so no message is ever permanently
// skipped.
//
// Every other concurrency test in this package passes against a sequence.
// TestConcurrentDistinctSendsStayDenseAndOrdered races 25 inserters in ONE
// conversation, so `conversations.last_seq`'s row lock has already serialised
// both draws before `log_counter` is consulted; `nextval` would fire inside
// that same lock window and ordering would still hold. Falsifiability needs
// writers in DISTINCT conversations, where nothing but `log_counter` orders
// them at all.
//
// Two arms:
//
//   - Arm 1 is a FORCED SCHEDULE and it is the proof. Scripted handoffs, no
//     sleeps. Against a sequence draw it loses a message every time; against
//     the counter row the schedule cannot reach that state, because the second
//     writer blocks on the counter's row lock until the first commits.
//   - Arm 2 is a randomized race and it is the net, not the proof. Counter row
//     only, and it must be green: a step that is allowed to be red is a red
//     light everyone learns to ignore.
//
// NO TABLE IS CREATED HERE. Both arms insert into the real `messages` table and
// differ only in where `log_seq` comes from, which is the whole variable under
// test. A mirror of `messages` would rot against the real schema and would
// break `publicTableCount`'s four exact assertions the moment it leaked.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// The write path under test

// inserter is one complete send. Arm 1's positive half and Arm 2 both drive the
// REAL write path through this parameter rather than through a copy, because
// the regressions a schema guard cannot see are all code-side: drawing the
// counter in its own short transaction "to cut contention", or drawing it
// before something slow. Both leave the DDL identical and break the property.
type inserter func(ctx context.Context, pool *pgxpool.Pool, conv, author, clientID uuid.UUID, text string) (sent, error)

// referenceInserter is the ONE call site naming the write path. CANT-14
// re-points this single line at the shipped insert primitive and deletes
// schema_test.go's reference copy; nothing else in this file names it.
var referenceInserter inserter = func(ctx context.Context, pool *pgxpool.Pool, conv, author, clientID uuid.UUID, text string) (sent, error) {
	return sendMessage(ctx, pool, conv, author, nil, clientID, text)
}

// Where log_seq comes from — the only difference between the two arms.
const (
	drawFromCounterRow = `UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value`
	drawFromSequence   = `SELECT nextval('` + testSequenceName + `')`
)

// testSequenceName is dropped unconditionally before creation and again in a
// t.Cleanup rather than relying on file order: `go test -shuffle=on` or a
// rename would otherwise leak it into TestOrdinalsAreNotDrawnFromASequence,
// which would then report a schema regression that is really a test fixture.
const testSequenceName = "logorder_test_bigserial_seq"

func createTestSequence(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	dropTestSequence(ctx, t, pool)
	if _, err := pool.Exec(ctx, `CREATE SEQUENCE `+testSequenceName); err != nil {
		t.Fatalf("create %s: %v", testSequenceName, err)
	}
	t.Cleanup(func() { dropTestSequence(context.WithoutCancel(ctx), t, pool) })
}

func dropTestSequence(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DROP SEQUENCE IF EXISTS `+testSequenceName); err != nil {
		t.Fatalf("drop %s: %v", testSequenceName, err)
	}
}

// ---------------------------------------------------------------------------
// Connections
//
// The pool must not be the serialisation point. `Connect` leaves MaxConns at
// max(4, NumCPU), so past that Arm 2's writers queue in the CLIENT and never
// contend at the database — the race would be far narrower than N suggests.
// Worse, Arm 1 holds a transaction open by design, so writers can take every
// connection and the reader's poll blocks on Acquire until the context expires:
// a timeout that says nothing about the invariant.

// arm2PoolMaxConns is set explicitly, and the writer count is stated relative
// to it rather than chosen independently.
const arm2PoolMaxConns = 16

// arm2Writers leaves headroom so that every writer holds a connection at once
// and the arm measures log_counter rather than pgxpool's queue.
const arm2Writers = arm2PoolMaxConns / 2

func writerPool(ctx context.Context, t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.RuntimeParams["application_name"] = "cant19-writer"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("writer pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// standaloneConn gives the reader and the schedule's holder their own
// connections, outside any pool the writers can exhaust.
func standaloneConn(ctx context.Context, t *testing.T, appName string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.RuntimeParams["application_name"] = appName
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect %s: %v", appName, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(ctx)) })
	return conn
}

// ---------------------------------------------------------------------------
// The oracle — a sync client, in the smallest form that can be wrong

// skipErr is the property violation, stated in the terms the invariant is
// written in rather than as an abstract ordering complaint.
type skipErr struct{ got, want int64 }

func (e skipErr) Error() string {
	return fmt.Sprintf(
		"a sync client was handed log_seq %d having never been handed %d: the message holding log_seq %d is invisible to it for good",
		e.got, e.want, e.want)
}

// syncReader is /sync?after= reduced to what the property needs: a cursor, and
// a memory of everything it has ever been delivered.
type syncReader struct {
	conn      *pgx.Conn
	cursor    int64
	nextWant  int64
	delivered []uuid.UUID
}

func newSyncReader(conn *pgx.Conn) *syncReader {
	return &syncReader{conn: conn, nextWant: 1}
}

// poll fetches everything above the cursor and checks CONTIGUITY as it goes.
//
// This is the assertion that fires AT the moment of the skip. Under the counter
// row, commit order and log_seq order are identical and visibility is monotone,
// so a poll can never hand back N without N-1 having already been delivered:
// the delivered stream is exactly 1..N. The closing set comparison in
// assertDeliveredEverything is the second assertion, not the only one — it can
// only speak after every writer has committed, so on its own it cannot say
// WHEN a message went missing.
//
// The draft's other assertion — "no row is ever returned below an already
// advanced cursor" — is deliberately absent: the query says
// `WHERE log_seq > $1`, so Postgres cannot return such a row and it could only
// fail if this harness were miswritten.
func (r *syncReader) poll(ctx context.Context) error {
	rows, err := r.conn.Query(ctx,
		`SELECT id, log_seq FROM messages WHERE log_seq > $1 ORDER BY log_seq`, r.cursor)
	if err != nil {
		return fmt.Errorf("poll: %w", err)
	}
	defer rows.Close()

	var firstSkip error
	for rows.Next() {
		var id uuid.UUID
		var logSeq int64
		if err := rows.Scan(&id, &logSeq); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if logSeq != r.nextWant && firstSkip == nil {
			firstSkip = skipErr{got: logSeq, want: r.nextWant}
		}
		// The cursor advances to what it was handed even across a gap, and
		// that is not a shortcut — it is the mechanism. A real client cannot
		// know the row below is missing, so it never asks for it again, and
		// the skip becomes permanent at exactly this line. A reader that
		// refused to advance here would quietly repair the bug it exists to
		// detect.
		r.nextWant = logSeq + 1
		r.cursor = logSeq
		r.delivered = append(r.delivered, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return firstSkip
}

// pollUntil polls until the reader has been handed n messages or the bound
// expires. It returns the first skip rather than retrying past it.
func (r *syncReader) pollUntil(ctx context.Context, n int, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		if err := r.poll(ctx); err != nil {
			return err
		}
		if len(r.delivered) >= n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("reader saw %d of %d messages within %s", len(r.delivered), n, within)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// assertDeliveredEverything is the closing comparison: the set of ids a sync
// client was handed equals the set of ids in the table. It states the failure
// as "N messages were never delivered to a sync client", which is what the
// invariant is actually about.
func (r *syncReader) assertDeliveredEverything(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	got := make(map[uuid.UUID]bool, len(r.delivered))
	for _, id := range r.delivered {
		got[id] = true
	}

	rows, err := pool.Query(ctx, `SELECT id, log_seq FROM messages ORDER BY log_seq`)
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	defer rows.Close()

	var missing []string
	var total int
	for rows.Next() {
		var id uuid.UUID
		var logSeq int64
		if err := rows.Scan(&id, &logSeq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		total++
		if !got[id] {
			missing = append(missing, fmt.Sprintf("log_seq %d (%s)", logSeq, id))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d of %d messages were never delivered to a sync client: %v", len(missing), total, missing)
	}
}

// ---------------------------------------------------------------------------
// A transaction held open mid-draw
//
// heldDraw exists so an arm can stop a writer between drawing its ordinals and
// committing. No shipped inserter would ever expose that, which is why this is
// scripted here rather than driven through the inserter parameter.

type heldDraw struct {
	tx     pgx.Tx
	id     uuid.UUID
	seq    int64
	logSeq int64
	conv   uuid.UUID
	author uuid.UUID
}

// beginDraw opens a transaction, draws the conversation ordinal FIRST and
// log_seq LAST — Ruling 2's order, which is a deadlock rule as much as a
// latency one — and returns with the transaction open and both locks held.
func beginDraw(ctx context.Context, t *testing.T, conn *pgx.Conn, conv, author uuid.UUID, draw string) *heldDraw {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	h := &heldDraw{tx: tx, id: uuid.New(), conv: conv, author: author}
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })

	if err := tx.QueryRow(ctx,
		`UPDATE conversations SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq`,
		conv).Scan(&h.seq); err != nil {
		t.Fatalf("draw seq: %v", err)
	}
	if err := tx.QueryRow(ctx, draw).Scan(&h.logSeq); err != nil {
		t.Fatalf("draw log_seq: %v", err)
	}
	return h
}

// commitInsert writes the row and commits, releasing both locks.
func (h *heldDraw) commitInsert(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := h.tx.Exec(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, seq, log_seq, updated_log_seq, text, client_id)
		VALUES ($1, $2, $3, $4, $5, $5, 'held', $6)`,
		h.id, h.conv, h.author, h.seq, h.logSeq, uuid.New()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := h.tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Arm 1 · the forced schedule
//
// Both halves run the SAME schedule. Only the draw differs, which is the point:
// the schedule is reachable with a sequence and unreachable with the counter
// row, and that is the whole claim.

// The negative half. It must fail, every time — a guard that is asserted to
// bite rather than proved to bite is the shape CANT-13's planted-line probe and
// CANT-15's staleness proof both exist to avoid.
func TestForcedScheduleLosesAMessageWhenLogSeqComesFromASequence(t *testing.T) {
	ctx, pool := freshDB(t)
	createTestSequence(ctx, t, pool)

	author := mkUser(ctx, t, pool, "holder")
	convA := mkGroup(ctx, t, pool, "room-a")
	convB := mkGroup(ctx, t, pool, "room-b")

	connA := standaloneConn(ctx, t, "cant19-a")
	connB := standaloneConn(ctx, t, "cant19-b")
	reader := newSyncReader(standaloneConn(ctx, t, "cant19-reader"))

	// A draws first and holds. A sequence takes no lock, so B is free to
	// overtake it — that is the bug, in three lines.
	a := beginDraw(ctx, t, connA, convA, author, drawFromSequence)
	b := beginDraw(ctx, t, connB, convB, author, drawFromSequence)
	if a.logSeq >= b.logSeq {
		t.Fatalf("schedule is wrong: A drew log_seq %d and B drew %d; A must draw first", a.logSeq, b.logSeq)
	}

	// B commits while A is still open, and the reader advances past it.
	b.commitInsert(ctx, t)

	err := reader.poll(ctx)
	if err == nil {
		t.Fatal("the reader was handed nothing out of order — the schedule did not reproduce the skip, so this arm proves nothing")
	}
	var skip skipErr
	if !errors.As(err, &skip) {
		t.Fatalf("poll failed for the wrong reason: %v", err)
	}
	if skip.got != b.logSeq || skip.want != a.logSeq {
		t.Fatalf("skip named the wrong rows: got %+v, want got=%d want=%d", skip, b.logSeq, a.logSeq)
	}
	t.Logf("the property broke exactly where it should: %v", skip)

	// A commits last. Its row is below the cursor for good.
	a.commitInsert(ctx, t)
	if err := reader.poll(ctx); err != nil {
		t.Logf("and stays broken on the next poll: %v", err)
	}

	// The closing comparison agrees, in the invariant's own words.
	var delivered bool
	for _, id := range reader.delivered {
		if id == a.id {
			delivered = true
		}
	}
	if delivered {
		t.Fatal("A's message was delivered after all; the arm did not reproduce a permanent skip")
	}
}

// The positive half, against the REAL write path. B is the production inserter,
// and it blocks — which is the mechanism, not a coincidence of timing.
func TestForcedScheduleCannotLoseAMessageWithTheCounterRow(t *testing.T) {
	ctx, pool := freshDB(t)
	writers := writerPool(ctx, t, 4)

	author := mkUser(ctx, t, pool, "holder")
	convA := mkGroup(ctx, t, pool, "room-a")
	convB := mkGroup(ctx, t, pool, "room-b")

	connA := standaloneConn(ctx, t, "cant19-a")
	reader := newSyncReader(standaloneConn(ctx, t, "cant19-reader"))

	// A draws from the counter row and holds, taking the row lock the whole
	// deployment shares.
	a := beginDraw(ctx, t, connA, convA, author, drawFromCounterRow)

	// B is the shipped path. It must not get past the counter draw.
	type result struct {
		s   sent
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := referenceInserter(ctx, writers, convB, author, uuid.New(), "b")
		done <- result{s, err}
	}()

	// The block is asserted as a FACT ABOUT THE DATABASE, not guessed from
	// scheduling. The assertion is on the wait_event_type — the event itself
	// ("transactionid", "tuple") is an implementation detail of how Postgres
	// happens to queue this particular contention, and pinning it would make
	// the arm brittle for no gain.
	waitFor(ctx, t, pool, `
		SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database()
		  AND application_name = 'cant19-writer'
		  AND wait_event_type = 'Lock'`,
		5*time.Second,
		"B never blocked on a lock while A held the counter row: the shipped inserter is not taking the counter's row lock, so nothing orders it against A")

	select {
	case r := <-done:
		t.Fatalf("B completed while A still held the counter row (log_seq %d, err %v): the draws are not serialised", r.s.logSeq, r.err)
	default:
	}

	// Releasing A lets B through, and only then.
	a.commitInsert(ctx, t)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("B failed after A committed: %v", r.err)
		}
		if r.s.logSeq <= a.logSeq {
			t.Fatalf("B drew log_seq %d, not above A's %d: draw order and commit order disagree", r.s.logSeq, a.logSeq)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never completed after A committed")
	}

	if err := reader.pollUntil(ctx, 2, 5*time.Second); err != nil {
		t.Fatalf("the schedule that loses a message with a sequence lost one with the counter row too: %v", err)
	}
	reader.assertDeliveredEverything(ctx, t, pool)
}

// waitFor polls a scalar count query until it is non-zero, and fails with msg
// if the bound expires. One-sided: it never asserts a non-event by sleeping.
func waitFor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, within time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var n int
		if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
			t.Fatalf("waitFor: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (waited %s)", msg, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Arm 2 · the randomized race
//
// Counter row only, and it must be green. Running it against the sequence draw
// would put a step in CI whose failure is expected sometimes, which is the red
// light verify.sh's own header argues against.

func TestConcurrentWritersInDistinctConversationsAreNeverSkipped(t *testing.T) {
	ctx, pool := freshDB(t)
	writers := writerPool(ctx, t, arm2PoolMaxConns)

	author := mkUser(ctx, t, pool, "racer")
	convs := make([]uuid.UUID, arm2Writers)
	for i := range convs {
		convs[i] = mkGroup(ctx, t, pool, fmt.Sprintf("room-%d", i))
	}

	const perWriter = 15
	const total = arm2Writers * perWriter

	reader := newSyncReader(standaloneConn(ctx, t, "cant19-reader"))
	readerDone := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		for {
			if err := reader.poll(ctx); err != nil {
				readerDone <- err
				return
			}
			select {
			case <-stop:
				readerDone <- reader.poll(ctx) // one last sweep
				return
			default:
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make([]error, arm2Writers)
	start := make(chan struct{})
	for w := range arm2Writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range perWriter {
				if _, err := referenceInserter(ctx, writers, convs[w], author, uuid.New(), "m"); err != nil {
					errs[w] = err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", w, err)
		}
	}

	close(stop)
	if err := <-readerDone; err != nil {
		t.Fatalf("a sync client reading alongside %d concurrent writers in %d distinct conversations was skipped: %v",
			arm2Writers, arm2Writers, err)
	}
	if err := reader.pollUntil(ctx, total, 10*time.Second); err != nil {
		t.Fatalf("after every writer committed: %v", err)
	}
	reader.assertDeliveredEverything(ctx, t, pool)
}

// ---------------------------------------------------------------------------
// The structural guard
//
// The cheap half of the permanent net, and the half that cannot rot. Arm 1's
// positive half catches the code-side regressions; this catches the schema-side
// one — someone collapsing log_counter into a sequence, which is the
// plausible-looking simplification this project would otherwise never notice.
//
// TestSchemaMatchesThePlanColumnForColumn checks type and nullability only, so
// none of this is redundant.

func TestOrdinalsAreNotDrawnFromASequence(t *testing.T) {
	ctx, pool := freshDB(t)

	// Zero sequences in public. Asserting the COUNT alone would report a
	// leaked test fixture as a schema regression, so the names come with it.
	rows, err := pool.Query(ctx, `SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' ORDER BY sequencename`)
	if err != nil {
		t.Fatalf("list sequences: %v", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found = append(found, name)
	}
	if len(found) > 0 {
		t.Errorf("schema public holds %d sequence(s), want 0: %v — "+
			"if one of these is %s a test leaked its fixture; otherwise an ordinal is being drawn outside its transaction",
			len(found), found, testSequenceName)
	}

	// And neither ordinal draws from one implicitly. A hand-created sequence
	// read from Go would be caught above; this catches the DDL saying it.
	for _, col := range []string{"seq", "log_seq"} {
		var def *string
		var identity string
		err := pool.QueryRow(ctx, `
			SELECT column_default, is_identity FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'messages' AND column_name = $1`,
			col).Scan(&def, &identity)
		if err != nil {
			t.Fatalf("describe messages.%s: %v", col, err)
		}
		if def != nil {
			t.Errorf("messages.%s has a column default (%s); both ordinals are drawn in the inserting transaction, never by the column", col, *def)
		}
		if identity != "NO" {
			t.Errorf("messages.%s is an identity column (is_identity=%s); an identity hands out its number outside the transaction, exactly as a bigserial does", col, identity)
		}
	}
}
