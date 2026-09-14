package store

// CANT-85 — attachments linked inside the insert's own transaction.
//
// Every test here that touches the database goes through SendMessage and reads
// the result back with its own queries. The resolver is the only seam faked,
// and the one test about a CONSUMING resolver fakes it with a real row lock,
// because that is the only kind of resolver the store's race recovery is sound
// against.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/wire"
)

// realisticRows is what an ingest would have recorded for each requested
// attachment: every column the kind's CHECK requires, and every optional one
// set, so a column the insert forgot is a column a test can see missing.
//
// Position is set DELIBERATELY WRONG — reversed — because the store must
// overwrite it with the send's order, and a fake that happened to agree would
// prove nothing.
func realisticRows(want []NewAttachment) []AttachmentRow {
	out := make([]AttachmentRow, len(want))
	for i, w := range want {
		row := AttachmentRow{
			Kind:       w.Kind,
			Position:   len(want) - 1 - i,
			StorageKey: fmt.Sprintf("%s/%s", w.Kind, w.UploadID),
		}
		switch w.Kind {
		case "voice":
			row.DurationMs = ptr(int64(4200 + i))
			row.Peaks = []int64{0, 17, 100, int64(i % 101)}
			row.TranscriptState = ptr("ready")
			row.TranscriptJSON = []byte(fmt.Sprintf(
				`{"text":"voice note %d","word_count":3,"segments":[{"at_ms":0,"text":"voice note"}],"engine":"whisper.cpp","language":"en"}`, i))
		case "image":
			row.Filename = ptr(fmt.Sprintf("photo-%d.jpg", i))
			row.Width = ptr(int64(1600))
			row.Height = ptr(int64(1200))
			row.Bytes = ptr(int64(204800 + i))
			row.Placeholder = ptr("LEHV6nWB2yk8pyo0adR*.7kCMdnj")
		}
		out[i] = row
	}
	return out
}

// fakeResolver hands out realisticRows, or whatever rows overrides, and counts
// its calls. Safe for concurrent use, because the race tests share one.
type fakeResolver struct {
	rows  func(want []NewAttachment) []AttachmentRow
	calls atomic.Int64
}

func (f *fakeResolver) Resolve(_ context.Context, _ pgx.Tx, _ uuid.UUID, want []NewAttachment) ([]AttachmentRow, error) {
	f.calls.Add(1)
	if f.rows != nil {
		return f.rows(want), nil
	}
	return realisticRows(want), nil
}

// failIfCalled is a resolver whose only job is to prove it was never reached.
type failIfCalled struct{ t *testing.T }

func (f failIfCalled) Resolve(context.Context, pgx.Tx, uuid.UUID, []NewAttachment) ([]AttachmentRow, error) {
	f.t.Error("the resolver was called for a send carrying no attachments — " +
		"RefuseUploads would refuse every text message the service accepts")
	return nil, nil
}

func attachmentsFor(kinds ...string) []NewAttachment {
	out := make([]NewAttachment, len(kinds))
	for i, k := range kinds {
		out[i] = NewAttachment{Kind: k, UploadID: uuid.New()}
	}
	return out
}

// storedAttachments reads a message's rows back with the columns Sync reads,
// in position order.
func storedAttachments(ctx context.Context, t *testing.T, pool *pgxpool.Pool, messageID uuid.UUID) []AttachmentRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT kind, position, storage_key, duration_ms, peaks,
		       transcript_state, transcript_json, filename, width, height, bytes, placeholder
		  FROM attachments WHERE message_id = $1 ORDER BY position`, messageID)
	if err != nil {
		t.Fatalf("read attachments: %v", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AttachmentRow, error) {
		var a AttachmentRow
		err := r.Scan(&a.Kind, &a.Position, &a.StorageKey, &a.DurationMs, &a.Peaks,
			&a.TranscriptState, &a.TranscriptJSON, &a.Filename, &a.Width, &a.Height, &a.Bytes, &a.Placeholder)
		return a, err
	})
	if err != nil {
		t.Fatalf("collect attachments: %v", err)
	}
	return out
}

// canonical re-encodes transcript_json, because JSONB normalizes whitespace and
// key order on the way in and a byte comparison would fail on a faithful write.
func canonical(t *testing.T, rows []AttachmentRow) []AttachmentRow {
	t.Helper()
	out := append([]AttachmentRow(nil), rows...)
	for i := range out {
		if out[i].TranscriptJSON == nil {
			continue
		}
		var v any
		if err := json.Unmarshal(out[i].TranscriptJSON, &v); err != nil {
			t.Fatalf("transcript_json is not JSON: %v", err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("re-encode transcript_json: %v", err)
		}
		out[i].TranscriptJSON = b
	}
	return out
}

// positioned is what the store is expected to have written for a set of
// resolver rows: the same rows, with position the send's order.
func positioned(rows []AttachmentRow) []AttachmentRow {
	out := append([]AttachmentRow(nil), rows...)
	for i := range out {
		out[i].Position = i
	}
	return out
}

func countWhere(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	mustScan(t, pool.QueryRow(ctx, query, args...), &n)
	return n
}

// ---------------------------------------------------------------------------
// criterion 13 — the ceiling, without a database

// The insert writes every row in one statement, so the ceiling is also a
// statement-size claim. A column added to attachmentColumns later must not
// quietly carry a maximal send past the extended protocol's parameter limit,
// and attachmentValues must stay aligned with the column list it fills.
func TestTheAttachmentCeilingFitsInOneStatement(t *testing.T) {
	if got := len(attachmentValues(uuid.New(), uuid.New(), AttachmentRow{})); got != len(attachmentColumns) {
		t.Fatalf("attachmentValues produces %d values for %d columns", got, len(attachmentColumns))
	}
	if params := MaxAttachmentsCeiling * len(attachmentColumns); params > math.MaxUint16 {
		t.Errorf("a maximal send binds %d parameters (%d rows × %d columns); the extended protocol carries %d",
			params, MaxAttachmentsCeiling, len(attachmentColumns), math.MaxUint16)
	}
}

// ---------------------------------------------------------------------------
// criterion 1 — a message and its attachments commit as one

func TestAMessageAndItsAttachmentsCommitTogether(t *testing.T) {
	ctx, pool := freshDB(t)
	fake := &fakeResolver{}
	st := New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(fake)
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	// Voice, image, voice: an order a sort by kind would not reproduce.
	want := attachmentsFor("voice", "image", "voice")
	sent, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author,
		Text: ptr("three things"), Attachments: want,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM messages WHERE id = $1`, sent.ID); n != 1 {
		t.Fatalf("%d messages rows for the send, want 1", n)
	}
	got := canonical(t, storedAttachments(ctx, t, pool, sent.ID))
	expect := canonical(t, positioned(realisticRows(want)))
	if !reflect.DeepEqual(got, expect) {
		t.Errorf("stored attachments differ from what the resolver returned, positioned by the send:\n got  %+v\n want %+v", got, expect)
	}
	for i, a := range got {
		if a.Position != i || a.Kind != want[i].Kind {
			t.Errorf("row %d is position %d kind %q, want position %d kind %q — the send's order, not the resolver's",
				i, a.Position, a.Kind, i, want[i].Kind)
		}
	}
	if c := fake.calls.Load(); c != 1 {
		t.Errorf("the resolver was called %d times for one send, want 1", c)
	}
}

// The ceiling goes through the real statement, on a store configured AT the
// ceiling, so the parameter arithmetic is proved under the bound rather than
// under the default.
func TestAMaximalSendGoesThroughOneStatement(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, Limits{MaxMessageBytes: 16384, MaxAttachments: MaxAttachmentsCeiling}, discardLogger()).
		WithUploadResolver(&fakeResolver{})
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	kinds := make([]string, MaxAttachmentsCeiling)
	for i := range kinds {
		kinds[i] = []string{"voice", "image"}[i%2]
	}
	want := attachmentsFor(kinds...)
	sent, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Attachments: want,
	})
	if err != nil {
		t.Fatalf("a send at the ceiling was refused: %v", err)
	}
	got := canonical(t, storedAttachments(ctx, t, pool, sent.ID))
	if !reflect.DeepEqual(got, canonical(t, positioned(realisticRows(want)))) {
		t.Errorf("a maximal send stored %d rows that differ from what was resolved", len(got))
	}
}

// ---------------------------------------------------------------------------
// criterion 2 — a failure at 11b takes the message with it

// The injected failure is a real constraint, not a hook: a voice row with no
// peaks violates attachments_voice_fields, so the attachment statement itself
// fails after the message insert and both draws have run.
func TestAFailedAttachmentInsertLeavesNoMessage(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	if _, err := New(pool, DefaultLimits(), discardLogger()).SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("before"),
	}); err != nil {
		t.Fatalf("seeding a real send: %v", err)
	}
	beforeLast, beforeLog := counters(ctx, t, pool, conv)

	const secret = "the transcript nobody should find in a log file"
	broken := &fakeResolver{rows: func(want []NewAttachment) []AttachmentRow {
		rows := realisticRows(want)
		rows[1].Peaks = nil // voice, and the CHECK requires peaks
		rows[1].TranscriptJSON = []byte(`{"text":"` + secret + `"}`)
		return rows
	}}
	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger).WithUploadResolver(broken)

	key := uuid.New()
	_, err := st.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author,
		Text: ptr("doomed"), Attachments: attachmentsFor("image", "voice"),
	})

	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("refused with %T (%v), want a *SendError", err, err)
	}
	if se.Code != wire.ErrorCodeInternal || se.Retryable {
		t.Errorf("code %q retryable %t, want internal and not retryable — a CHECK is a decision, not a hiccup", se.Code, se.Retryable)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "attachments_voice_fields" {
		t.Fatalf("the failure is %v, want 23514 on attachments_voice_fields — the test did not reach 11b", err)
	}

	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM messages WHERE author_id = $1 AND client_id = $2`, author, key); n != 0 {
		t.Errorf("%d messages rows survived a failed attachment insert; a message and its attachments commit together or not at all", n)
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM attachments`); n != 0 {
		t.Errorf("%d attachment rows exist after the only attachment send failed", n)
	}
	afterLast, afterLog := counters(ctx, t, pool, conv)
	if afterLast != beforeLast || afterLog != beforeLog {
		t.Errorf("the failed send consumed ordinals: last_seq %d→%d, log_counter %d→%d",
			beforeLast, afterLast, beforeLog, afterLog)
	}

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want exactly one refusal line, got %d: %v", len(lines), lines)
	}
	if lines[0]["level"] != "WARN" || lines[0]["sqlstate"] != "23514" || lines[0]["constraint"] != "attachments_voice_fields" {
		t.Errorf("the refusal line does not carry level, SQLSTATE and constraint: %v", lines[0])
	}
	if strings.Contains(buf.String(), secret) {
		t.Error("the refusal log contains the attachment's transcript — Detail's failing row is the message")
	}
}

// ---------------------------------------------------------------------------
// criterion 3 — the service refuses attachments until an upload flow exists

// Constructed exactly as runServe constructs it: New, and nothing else.
func TestTheServiceRefusesAttachmentsUntilAnUploadFlowExists(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	beforeLast, beforeLog := counters(ctx, t, pool, conv)

	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)
	key := uuid.New()
	_, err := st.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author,
		Text: ptr("see attached"), Attachments: attachmentsFor("image", "voice"),
	})
	assertCode(t, err, wire.ErrorCodeUploadNotFound, ErrUploadNotFound)

	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM messages WHERE author_id = $1 AND client_id = $2`, author, key); n != 0 {
		t.Errorf("%d messages rows for a refused send", n)
	}
	afterLast, afterLog := counters(ctx, t, pool, conv)
	if afterLast != beforeLast || afterLog != beforeLog {
		t.Errorf("the refusal consumed ordinals: last_seq %d→%d, log_counter %d→%d",
			beforeLast, afterLast, beforeLog, afterLog)
	}
	lines := logLines(t, buf)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["code"] != "upload_not_found" {
		t.Errorf("want one info line with code upload_not_found, got %v", lines)
	}
}

// ---------------------------------------------------------------------------
// criterion 4 — a send without attachments never resolves

func TestASendWithoutAttachmentsNeverResolves(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(failIfCalled{t})
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	for name, msg := range map[string]NewMessage{
		"text only": {ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("just words")},
		"empty":     {ClientID: uuid.New(), ConversationID: conv, AuthorID: author},
	} {
		if _, err := st.SendMessage(ctx, msg); err != nil {
			t.Errorf("%s send was refused: %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// criterion 5 — resolution runs below the idempotency check

// Once CANT-48 consumes uploads, the replay of an acked attachment send names
// handles its own first attempt used up.
//
// TWO REPLAYS, because the first one alone cannot see the ordering. Against the
// service's own refusing store, a replay gets the original either way: if it
// reached the resolver, the not-found would send it through the race-loser
// re-read in send, which finds the original by key. The probe that moved
// resolution above position 4 stayed green against that assertion for exactly
// this reason. So the ORDERING is pinned by a replay to a resolver that fails
// the test if it is called at all, and the refusing store is kept for the
// outcome the service actually gives.
func TestAReplayNeverResolves(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	key := uuid.New()
	want := attachmentsFor("voice", "image")

	first, err := New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(&fakeResolver{}).SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Attachments: want,
	})
	if err != nil {
		t.Fatalf("first send: %v", err)
	}

	for name, st := range map[string]*Store{
		"a resolver that must not be reached": New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(failIfCalled{t}),
		"the service's refusing store":        New(pool, DefaultLimits(), discardLogger()),
	} {
		replay, err := st.SendMessage(ctx, NewMessage{
			ClientID: key, ConversationID: conv, AuthorID: author, Attachments: want,
		})
		if err != nil {
			t.Fatalf("%s: the replay of an acked attachment send was refused: %v", name, err)
		}
		if !replay.Duplicate || replay.ID != first.ID {
			t.Errorf("%s: the replay returned %+v, want the original %s as a duplicate", name, replay, first.ID)
		}
	}
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM attachments WHERE message_id = $1`, first.ID); n != 2 {
		t.Errorf("%d attachment rows after a replay, want the original 2", n)
	}
}

// Beside TestConcurrentSendsUnderOneKey rather than an edit to it: that test is
// cited by name as a regression guarantee and keeps meaning what it meant.
func TestConcurrentSendsWithAttachmentsUnderOneKey(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(&fakeResolver{})
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	key := uuid.New()
	want := attachmentsFor("voice", "image", "voice")

	const n = 12
	var wg sync.WaitGroup
	results := make([]Sent, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = st.SendMessage(ctx, NewMessage{
				ClientID: key, ConversationID: conv, AuthorID: author, Attachments: want,
			})
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("send %d returned an error: %v", i, err)
		}
		if results[i].ID != results[0].ID {
			t.Errorf("send %d got message %s, want %s", i, results[i].ID, results[0].ID)
		}
	}
	if c := countWhere(ctx, t, pool, `SELECT count(*) FROM messages WHERE author_id = $1 AND client_id = $2`, author, key); c != 1 {
		t.Errorf("%d racing sends under one key produced %d messages, want 1", n, c)
	}
	if c := countWhere(ctx, t, pool, `SELECT count(*) FROM attachments`); c != 3 {
		t.Errorf("%d attachment rows, want 3 — a loser's rows must roll back with its message", c)
	}
	if c := countWhere(ctx, t, pool, `SELECT value FROM log_counter WHERE id = 1`); c != 1 {
		t.Errorf("log_counter = %d, want 1", c)
	}
}

// ---------------------------------------------------------------------------
// criterion 12 — the race loser under a consuming resolver

// consumingResolver is what CANT-48's resolver has to be for the store's
// recovery to be sound: it consumes handles with a ROW-LOCKED update in the
// send's transaction. A Go set would not do — it cannot block the loser until
// the winner commits, and it stays consumed if the winner rolls back, so it
// would test a resolver nobody should write.
//
// The first call pauses while holding its row locks, so the test can put the
// second send behind them.
type consumingResolver struct {
	first   atomic.Bool
	holding chan struct{}
	release chan struct{}
}

func (r *consumingResolver) Resolve(ctx context.Context, tx pgx.Tx, _ uuid.UUID, want []NewAttachment) ([]AttachmentRow, error) {
	ids := make([]uuid.UUID, len(want))
	for i, w := range want {
		ids[i] = w.UploadID
	}
	rows, err := tx.Query(ctx,
		`UPDATE cant85_uploads SET consumed = true WHERE id = ANY($1) AND NOT consumed RETURNING id`, ids)
	if err != nil {
		return nil, err
	}
	consumed, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	if r.first.CompareAndSwap(false, true) {
		r.holding <- struct{}{}
		<-r.release
	}
	if len(consumed) != len(want) {
		return nil, fmt.Errorf("consumed %d of %d handles: %w", len(consumed), len(want), ErrUploadNotFound)
	}
	return realisticRows(want), nil
}

func TestARaceLoserUnderAConsumingResolverGetsTheOriginal(t *testing.T) {
	ctx, pool := freshDB(t)
	// A fixture table the migrations do not know about, so it is dropped both
	// before (a crashed run) and after (every other test counts public tables).
	dropFixture := func() {
		if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS cant85_uploads`); err != nil {
			t.Errorf("drop fixture table: %v", err)
		}
	}
	dropFixture()
	t.Cleanup(dropFixture)
	if _, err := pool.Exec(ctx,
		`CREATE TABLE cant85_uploads (id UUID PRIMARY KEY, consumed BOOLEAN NOT NULL DEFAULT false)`); err != nil {
		t.Fatalf("create fixture table: %v", err)
	}

	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	want := attachmentsFor("voice", "image")
	for _, w := range want {
		if _, err := pool.Exec(ctx, `INSERT INTO cant85_uploads (id) VALUES ($1)`, w.UploadID); err != nil {
			t.Fatalf("seed upload: %v", err)
		}
	}

	resolver := &consumingResolver{holding: make(chan struct{}, 1), release: make(chan struct{})}
	// Released exactly once, and on cleanup as well as on the happy path. If
	// the test fails while the winner is paused on its row locks, the fixture
	// table's DROP would otherwise wait behind the winner's open transaction
	// until go test's own timeout. Registered after dropFixture, so it runs
	// before it.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resolver.release) }) }
	t.Cleanup(release)
	st := New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(resolver)
	msg := NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Attachments: want}

	type outcome struct {
		sent Sent
		err  error
	}
	winner, loser := make(chan outcome, 1), make(chan outcome, 1)
	go func() { s, err := st.SendMessage(ctx, msg); winner <- outcome{s, err} }()

	select {
	case <-resolver.holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the first send never reached its resolver")
	}
	// The second attempt under the same key: past position 4, because nothing
	// has committed, and into the UPDATE, where it waits on the winner's rows.
	go func() { s, err := st.SendMessage(ctx, msg); loser <- outcome{s, err} }()
	waitForLockWaiter(ctx, t, pool)
	release()

	var w, l outcome
	for _, c := range []struct {
		name string
		ch   chan outcome
		into *outcome
	}{{"winner", winner, &w}, {"loser", loser, &l}} {
		select {
		case *c.into = <-c.ch:
		case <-time.After(15 * time.Second):
			t.Fatalf("the %s never returned", c.name)
		}
	}

	if w.err != nil || w.sent.Duplicate {
		t.Fatalf("winner = %+v, %v; want a fresh send", w.sent, w.err)
	}
	if l.err != nil {
		t.Fatalf("the race loser was refused (%v) for a message that committed — the store must re-read the key "+
			"before a consumed handle becomes upload_not_found", l.err)
	}
	if !l.sent.Duplicate || l.sent.ID != w.sent.ID {
		t.Errorf("loser = %+v, want the winner's message %s as a duplicate", l.sent, w.sent.ID)
	}
	if c := countWhere(ctx, t, pool, `SELECT count(*) FROM messages WHERE author_id = $1 AND client_id = $2`, author, msg.ClientID); c != 1 {
		t.Errorf("%d messages, want 1", c)
	}
	if c := countWhere(ctx, t, pool, `SELECT count(*) FROM attachments WHERE message_id = $1`, w.sent.ID); c != int64(len(want)) {
		t.Errorf("%d attachment rows, want %d", c, len(want))
	}
}

// ---------------------------------------------------------------------------
// criteria 6 and 7 — the store checks the resolver's answer

// Ruling 1: the kind the sender asked for, or no attachment.
func TestAKindThatDisagreesWithItsUploadIsUploadNotFound(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	liar := &fakeResolver{rows: func(want []NewAttachment) []AttachmentRow {
		// Asked for a voice note, and the upload is an image.
		return realisticRows([]NewAttachment{{Kind: "image", UploadID: want[0].UploadID}})
	}}
	st := New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(liar)
	key := uuid.New()
	_, err := st.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Attachments: attachmentsFor("voice"),
	})
	assertCode(t, err, wire.ErrorCodeUploadNotFound, ErrUploadNotFound)
	if n := countWhere(ctx, t, pool, `SELECT count(*) FROM messages WHERE author_id = $1 AND client_id = $2`, author, key); n != 0 {
		t.Errorf("%d messages rows for a refused send", n)
	}
}

func TestAResolverThatAnswersTheWrongQuestionIsInternal(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	for name, reshape := range map[string]func([]AttachmentRow) []AttachmentRow{
		"one row short": func(r []AttachmentRow) []AttachmentRow { return r[:len(r)-1] },
		"one row over":  func(r []AttachmentRow) []AttachmentRow { return append(r, r[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			wrong := &fakeResolver{rows: func(want []NewAttachment) []AttachmentRow {
				return reshape(realisticRows(want))
			}}
			st := New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(wrong)
			key := uuid.New()
			_, err := st.SendMessage(ctx, NewMessage{
				ClientID: key, ConversationID: conv, AuthorID: author, Attachments: attachmentsFor("voice", "image"),
			})
			var se *SendError
			if !errors.As(err, &se) || se.Code != wire.ErrorCodeInternal || se.Retryable {
				t.Fatalf("refused with %v, want internal and not retryable — a broken resolver is our bug", err)
			}
			if !errors.Is(err, ErrUploadResolverContract) {
				t.Errorf("the cause is not ErrUploadResolverContract: %v", err)
			}
			if n := countWhere(ctx, t, pool, `SELECT count(*) FROM messages WHERE author_id = $1 AND client_id = $2`, author, key); n != 0 {
				t.Errorf("%d messages rows for a refused send", n)
			}
		})
	}
}
