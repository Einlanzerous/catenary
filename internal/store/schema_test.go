package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// The reference insert
//
// This is NOT the production write path. CANT-14 owns the ordinal draw and
// CANT-18 owns the send, both under their own review modes; what lives here is
// the smallest insert that exercises the ORDERING this migration settled, so
// the schema's claims are proved by the ticket that makes them rather than
// inherited on trust by the ticket that comes next.
//
// Ruling 2, in code:
//
//  1. Check idempotency BEFORE either ordinal is drawn. The tempting idiom —
//     INSERT ... ON CONFLICT DO NOTHING, then SELECT — commits either way,
//     including the counter bumps, so a replay draws both ordinals, inserts
//     nothing, and leaves a permanent hole in that conversation's seq. The
//     wire tells every client a seq gap means a message it is missing.
//
//  2. Draw the conversation's ordinal first and the global counter LAST. Both
//     are row locks held until commit; taking log_counter last keeps the
//     deployment-wide serialised section down to draw-insert-commit instead of
//     the whole transaction. Taking them in a consistent order is also what
//     makes deadlock between two conversations impossible.
//
//  3. On the residual race — two concurrent sends under one key both pass the
//     check — catch the unique violation, roll back, and re-select by key.
//     The rollback UN-DRAWS both ordinals, because they came from rows rather
//     than from a sequence. That is the difference between a row counter and a
//     bigserial at the exact moment it matters, and it is why the race leaves
//     no hole in either ordinal.
type sent struct {
	id        uuid.UUID
	seq       int64
	logSeq    int64
	duplicate bool
}

func sendMessage(ctx context.Context, pool *pgxpool.Pool, convID, authorID uuid.UUID, deviceID *uuid.UUID, clientID uuid.UUID, text string) (sent, error) {
	s, err := trySend(ctx, pool, convID, authorID, deviceID, clientID, text)
	if err == nil {
		return s, nil
	}
	if !isUniqueViolation(err, "messages_author_id_client_id_key") {
		return sent{}, err
	}
	// Somebody else committed this key while we were drawing. Our ordinals
	// went back with the rollback; re-read theirs.
	return selectByKey(ctx, pool, authorID, clientID)
}

func trySend(ctx context.Context, pool *pgxpool.Pool, convID, authorID uuid.UUID, deviceID *uuid.UUID, clientID uuid.UUID, text string) (sent, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return sent{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1 — check, before anything is drawn.
	var existing sent
	err = tx.QueryRow(ctx,
		`SELECT id, seq, log_seq FROM messages WHERE author_id = $1 AND client_id = $2`,
		authorID, clientID).Scan(&existing.id, &existing.seq, &existing.logSeq)
	switch {
	case err == nil:
		existing.duplicate = true
		return existing, nil // rolled back by the defer: nothing was drawn
	case !errors.Is(err, pgx.ErrNoRows):
		return sent{}, err
	}

	// 2 — draw, conversation first.
	var out sent
	if err := tx.QueryRow(ctx,
		`UPDATE conversations SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq`,
		convID).Scan(&out.seq); err != nil {
		return sent{}, fmt.Errorf("draw seq: %w", err)
	}
	if err := tx.QueryRow(ctx,
		`UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value`).Scan(&out.logSeq); err != nil {
		return sent{}, fmt.Errorf("draw log_seq: %w", err)
	}

	// 3 — insert. `at` and `updated_log_seq` are the schema's job, not the
	// caller's: the default is server-assigned and updated_log_seq starts equal
	// to log_seq.
	out.id = uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, seq, log_seq, updated_log_seq,
		                      text, client_id, sender_device_id)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8)`,
		out.id, convID, authorID, out.seq, out.logSeq, text, clientID, deviceID); err != nil {
		return sent{}, err
	}
	return out, tx.Commit(ctx)
}

func selectByKey(ctx context.Context, pool *pgxpool.Pool, authorID, clientID uuid.UUID) (sent, error) {
	var s sent
	err := pool.QueryRow(ctx,
		`SELECT id, seq, log_seq FROM messages WHERE author_id = $1 AND client_id = $2`,
		authorID, clientID).Scan(&s.id, &s.seq, &s.logSeq)
	s.duplicate = true
	return s, err
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(constraint == "" || pgErr.ConstraintName == constraint)
}

// ---------------------------------------------------------------------------
// fixtures

func mkUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, handle string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, handle, display_name) VALUES ($1, $2, $2)`, id, handle); err != nil {
		t.Fatalf("mkUser(%s): %v", handle, err)
	}
	return id
}

func mkDevice(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO devices (id, user_id, name) VALUES ($1, $2, $3)`, id, user, name); err != nil {
		t.Fatalf("mkDevice: %v", err)
	}
	return id
}

func mkGroup(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', $2)`, id, name); err != nil {
		t.Fatalf("mkGroup: %v", err)
	}
	return id
}

func seqs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conv uuid.UUID) []int64 {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT seq FROM messages WHERE conversation_id = $1 ORDER BY seq`, conv)
	if err != nil {
		t.Fatalf("seqs: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan seq: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// assertDense is the property the wire promises every client: if you can see
// seq 4 and seq 6 you may assume seq 5 exists and you are missing it.
func assertDense(t *testing.T, got []int64) {
	t.Helper()
	for i, s := range got {
		if want := int64(i + 1); s != want {
			t.Fatalf("seq is not dense: got %v, first gap at position %d (have %d, want %d)", got, i, s, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 0 — one counter for the whole deployment

func TestLogCounterIsOneRowDeploymentWide(t *testing.T) {
	ctx, pool := freshDB(t)

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM log_counter`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("log_counter holds %d rows, want exactly 1 for the whole deployment", n)
	}

	// Enforced by a constraint rather than by convention. A second row is what
	// a per-account counter would look like on its first day, and a per-account
	// counter would be DENSE — which inverts the entire seq/log_seq division of
	// labour, silently, because a dense counter passes every test a sparse one
	// does.
	_, err := pool.Exec(ctx, `INSERT INTO log_counter (id, value) VALUES (2, 0)`)
	if err == nil {
		t.Fatal("a second log_counter row was accepted; CHECK (id = 1) is not doing its job")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("second row rejected by %v, want a check-constraint violation", err)
	}
}

// ---------------------------------------------------------------------------
// Criteria 1-3 — idempotency

// Criterion 1. The wire is normative: "The server deduplicates on
// (account, client_id)". Rev 2 of the plan said (sender_device_id, client_id),
// and sender_device_id is nullable for bots — Postgres treats NULLs as
// distinct in a unique constraint, so that scope would have given every
// device-less sender NO deduplication at all while the schema promised the
// opposite. A retried bot post after an ambiguous failure would be a double
// post, which is exactly what client_id exists to prevent.
func TestDedupScopeIsAuthorNotDevice(t *testing.T) {
	ctx, pool := freshDB(t)
	bot := mkUser(ctx, t, pool, "bot")
	conv := mkGroup(ctx, t, pool, "room")
	key := uuid.New()

	first, err := sendMessage(ctx, pool, conv, bot, nil, key, "hello")
	if err != nil {
		t.Fatalf("first send from a device-less sender: %v", err)
	}
	if first.duplicate {
		t.Fatal("the first send reported itself a duplicate")
	}

	again, err := sendMessage(ctx, pool, conv, bot, nil, key, "hello")
	if err != nil {
		t.Fatalf("replay from a device-less sender: %v", err)
	}
	if !again.duplicate || again.id != first.id {
		t.Fatalf("a bot replay created a second message: %v then %v", first, again)
	}

	// And the same key from two devices of one account is ONE message, which
	// is the other half of what (author_id, client_id) means.
	human := mkUser(ctx, t, pool, "human")
	phone := mkDevice(ctx, t, pool, human, "phone")
	laptop := mkDevice(ctx, t, pool, human, "laptop")
	shared := uuid.New()

	a, err := sendMessage(ctx, pool, conv, human, &phone, shared, "hi")
	if err != nil {
		t.Fatalf("send from phone: %v", err)
	}
	b, err := sendMessage(ctx, pool, conv, human, &laptop, shared, "hi")
	if err != nil {
		t.Fatalf("send from laptop: %v", err)
	}
	if b.id != a.id {
		t.Errorf("one key from two devices of one account made two messages: %v, %v", a, b)
	}
}

// Criterion 2. The whole reason the ordering is a ruling: ON CONFLICT commits
// either way, so a replay under the tempting idiom draws both ordinals, inserts
// nothing, and burns a seq — a permanent hole the client reads as a lost
// message, and one no single-threaded test notices.
func TestReplayDrawsNoOrdinalsAndLeavesSeqDense(t *testing.T) {
	ctx, pool := freshDB(t)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")

	keys := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, k := range keys {
		if _, err := sendMessage(ctx, pool, conv, u, nil, k, fmt.Sprintf("m%d", i)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	var lastSeq, counter int64
	pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, conv).Scan(&lastSeq)
	pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&counter)

	// Replay every key, twice over.
	for range 2 {
		for i, k := range keys {
			got, err := sendMessage(ctx, pool, conv, u, nil, k, fmt.Sprintf("m%d", i))
			if err != nil {
				t.Fatalf("replay %d: %v", i, err)
			}
			if !got.duplicate {
				t.Errorf("replay of key %d was not reported as a duplicate", i)
			}
		}
	}

	var lastSeqAfter, counterAfter int64
	pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, conv).Scan(&lastSeqAfter)
	pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&counterAfter)

	if lastSeqAfter != lastSeq {
		t.Errorf("a replay burned a seq: last_seq %d -> %d", lastSeq, lastSeqAfter)
	}
	if counterAfter != counter {
		t.Errorf("a replay burned a log_seq: log_counter %d -> %d", counter, counterAfter)
	}
	assertDense(t, seqs(ctx, t, pool, conv))
}

// Criterion 3. N concurrent sends under one key produce one message and NO
// error: the unique violation is caught, rolled back and re-read, never
// surfaced. CANT-18's Done-when requires exactly this.
func TestConcurrentSendsUnderOneKey(t *testing.T) {
	ctx, pool := freshDB(t)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")
	key := uuid.New()

	const n = 12
	var wg sync.WaitGroup
	results := make([]sent, n)
	errs := make([]error, n)
	start := make(chan struct{})

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = sendMessage(ctx, pool, conv, u, nil, key, "once")
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("send %d returned an error: %v", i, err)
		}
	}
	for i, r := range results {
		if r.id != results[0].id {
			t.Errorf("send %d got a different message id: %v vs %v", i, r.id, results[0].id)
		}
	}

	var count int64
	pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE author_id = $1 AND client_id = $2`, u, key).Scan(&count)
	if count != 1 {
		t.Errorf("%d concurrent sends under one key produced %d messages, want 1", n, count)
	}

	// The losers' rollbacks un-drew their ordinals, because both counters are
	// rows. A bigserial would have left n-1 holes here.
	assertDense(t, seqs(ctx, t, pool, conv))

	var counter int64
	pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&counter)
	if counter != 1 {
		t.Errorf("log_counter = %d after %d racing sends of one message, want 1", counter, n)
	}
}

// Within one conversation, ascending log_seq implies ascending seq, under real
// contention and with every send landing.
//
// This is the WEAKER half of Invariant 1. The strong claim — ascending log_seq
// implies ascending COMMIT order, deployment-wide — is what a bigserial
// violates, and it is not falsifiable from inside a single conversation whose
// row lock already orders both draws. CANT-19 owns that property test and is
// written so it fails against a bigserial; this one guards the ordering a
// client actually applies, which is per conversation.
func TestConcurrentDistinctSendsStayDenseAndOrdered(t *testing.T) {
	ctx, pool := freshDB(t)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")

	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = sendMessage(ctx, pool, conv, u, nil, uuid.New(), fmt.Sprintf("m%d", i))
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	assertDense(t, seqs(ctx, t, pool, conv))

	// Within one conversation, log_seq order and seq order agree. A client
	// discovers by log_seq and orders by seq; if the two disagree it applies a
	// conversation out of order.
	rows, err := pool.Query(ctx,
		`SELECT seq, log_seq FROM messages WHERE conversation_id = $1 ORDER BY log_seq`, conv)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var prev int64
	for rows.Next() {
		var s, l int64
		if err := rows.Scan(&s, &l); err != nil {
			t.Fatal(err)
		}
		if s <= prev {
			t.Fatalf("ascending log_seq did not imply ascending seq: saw seq %d after %d", s, prev)
		}
		prev = s
	}
}

// ---------------------------------------------------------------------------
// Criterion 4 — head_seq is served, not stored

func TestHeadSeqIsNotAColumn(t *testing.T) {
	ctx, pool := freshDB(t)
	for _, table := range []string{"conversations", "messages"} {
		var exists bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'head_seq')`,
			table).Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("%s.head_seq exists; head_seq and last_seq are one number stored twice", table)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 7 — the retention sweep can actually run

// CANT-67 deletes messages on a timer and is Mode C for that reason. Proving
// the FKs let it run belongs HERE, before that ticket is written: RESTRICT on
// reply_to would make the sweep fail every time it reached a source with a
// reply, which is always, because a reply is newer than its source by
// definition.
func TestSweepCanDeleteAMessageWithRepliesAndAttachments(t *testing.T) {
	ctx, pool := freshDB(t)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")

	source, err := sendMessage(ctx, pool, conv, u, nil, uuid.New(), "the original")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO attachments (id, message_id, kind, storage_key, position, duration_ms, peaks, transcript_state)
		VALUES ($1, $2, 'voice', 'k/1', 0, 1200, ARRAY[0,50,100]::smallint[], 'ready')`,
		uuid.New(), source.id); err != nil {
		t.Fatalf("attach: %v", err)
	}

	reply, err := sendMessage(ctx, pool, conv, u, nil, uuid.New(), "the reply")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET reply_to = $1 WHERE id = $2`, source.id, reply.id); err != nil {
		t.Fatalf("set reply_to: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, source.id); err != nil {
		t.Fatalf("the retention sweep cannot delete a message with a reply and an attachment: %v", err)
	}

	// The reply survives with a null ref — the wire builds ReplyRef live and
	// Message.reply_to is optional, so a swept source is already "no ref".
	var replyTo *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT reply_to FROM messages WHERE id = $1`, reply.id).Scan(&replyTo); err != nil {
		t.Fatalf("reply vanished with its source: %v", err)
	}
	if replyTo != nil {
		t.Errorf("reply_to = %v after the source was swept, want NULL", replyTo)
	}

	// The attachment row went with the message. The BYTES are CANT-47's and
	// CANT-67's problem and are not claimed here.
	var attachments int
	pool.QueryRow(ctx, `SELECT count(*) FROM attachments WHERE message_id = $1`, source.id).Scan(&attachments)
	if attachments != 0 {
		t.Errorf("%d attachment rows outlived their message; message_id is not CASCADE", attachments)
	}
}

// The other half of Ruling 4: an author who has written anything cannot be
// deleted. That is the single fact that keeps CANT-33 off the Mode C list —
// a Purser offboard deactivates, and cannot destroy authored messages.
func TestAnAuthorWithMessagesCannotBeDeleted(t *testing.T) {
	ctx, pool := freshDB(t)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")
	if _, err := sendMessage(ctx, pool, conv, u, nil, uuid.New(), "hello"); err != nil {
		t.Fatal(err)
	}

	_, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u)
	if err == nil {
		t.Fatal("an author with messages was deleted; CANT-33's Mode A exemption rests on this being impossible")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("delete refused by %v, want a foreign-key violation", err)
	}

	// The offboard path that does work.
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, u); err != nil {
		t.Errorf("deactivation, the path Purser is supposed to use, failed: %v", err)
	}
}

// A revoked device is still referenced by everything it ever sent.
func TestADeviceWithMessagesCannotBeDeleted(t *testing.T) {
	ctx, pool := freshDB(t)
	u := mkUser(ctx, t, pool, "u")
	d := mkDevice(ctx, t, pool, u, "phone")
	conv := mkGroup(ctx, t, pool, "room")
	if _, err := sendMessage(ctx, pool, conv, u, &d, uuid.New(), "hello"); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM devices WHERE id = $1`, d); err == nil {
		t.Fatal("a device with messages was deleted; CANT-30 revocation must be a column, not a delete")
	}
	if _, err := pool.Exec(ctx, `UPDATE devices SET revoked_at = now() WHERE id = $1`, d); err != nil {
		t.Errorf("revocation failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 11 — a direct conversation is unique per pair

func TestConcurrentFindOrCreateDirectMakesOneConversation(t *testing.T) {
	ctx, pool := freshDB(t)
	a := mkUser(ctx, t, pool, "alice")
	b := mkUser(ctx, t, pool, "bob")

	// The two ids sorted and joined — the same key from either side.
	key := directKey(a, b)
	if key != directKey(b, a) {
		t.Fatal("direct_key is not order-independent")
	}

	const n = 10
	var wg sync.WaitGroup
	created := make([]int, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tag, err := pool.Exec(ctx, `
				INSERT INTO conversations (id, kind, direct_key) VALUES ($1, 'direct', $2)
				ON CONFLICT DO NOTHING`, uuid.New(), key)
			if err == nil {
				created[i] = int(tag.RowsAffected())
			}
		}()
	}
	close(start)
	wg.Wait()

	var count int
	pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE direct_key = $1`, key).Scan(&count)
	if count != 1 {
		t.Errorf("%d concurrent find-or-create calls made %d direct conversations for one pair, want 1", n, count)
	}

	// Promotion to a group nulls direct_key and supplies a name, which frees
	// the pair to have a direct again.
	if _, err := pool.Exec(ctx, `
		UPDATE conversations SET kind = 'group', name = 'the three of us', direct_key = NULL
		WHERE direct_key = $1`, key); err != nil {
		t.Fatalf("promotion to group: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, direct_key) VALUES ($1, 'direct', $2)`, uuid.New(), key); err != nil {
		t.Errorf("after promotion the pair could not start a new direct: %v", err)
	}
}

func directKey(a, b uuid.UUID) string {
	x, y := a.String(), b.String()
	if x > y {
		x, y = y, x
	}
	return x + "|" + y
}

// A group with no name, and a direct with one, are both refused.
func TestConversationNameRules(t *testing.T) {
	ctx, pool := freshDB(t)

	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind) VALUES ($1, 'group')`, uuid.New()); err == nil {
		t.Error("a group with no name was accepted")
	}
	// A DM has no name: the rail shows the OTHER member, which is per reader.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind) VALUES ($1, 'direct')`, uuid.New()); err != nil {
		t.Errorf("a direct with no name was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criteria 8-10 — the small ones with sharp edges

// Ruling 5. CANT-67 is Mode C precisely because reading this column wrongly
// deletes everything: NULL means inherit the global infinite default, not
// "zero days".
func TestRetentionDaysNullMeansInheritInfinite(t *testing.T) {
	ctx, pool := freshDB(t)

	// Zero is not expressible, here or on the wire (minimum: 1).
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name, retention_days) VALUES ($1, 'group', 'g', 0)`,
		uuid.New()); err == nil {
		t.Error("retention_days = 0 was accepted; the wire says minimum 1 and the sweep would read it as delete-everything")
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name, retention_days) VALUES ($1, 'group', 'g1', NULL)`,
		uuid.New()); err != nil {
		t.Errorf("retention_days = NULL was refused: %v", err)
	}

	// The meaning travels with the column, so CANT-67's test can be written
	// against something other than this conversation.
	var comment string
	err := pool.QueryRow(ctx, `
		SELECT col_description('conversations'::regclass, ordinal_position)
		FROM information_schema.columns
		WHERE table_name = 'conversations' AND column_name = 'retention_days'`).Scan(&comment)
	if err != nil {
		t.Fatalf("read column comment: %v", err)
	}
	for _, want := range []string{"NULL", "infinite"} {
		if !strings.Contains(comment, want) {
			t.Errorf("conversations.retention_days comment does not say what NULL means (missing %q): %q", want, comment)
		}
	}
}

// Criterion 9 — the wire bounds peaks' ELEMENTS as well as its length.
func TestPeaksBoundsElementsAndLength(t *testing.T) {
	ctx, pool := freshDB(t)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")
	m, err := sendMessage(ctx, pool, conv, u, nil, uuid.New(), "voice")
	if err != nil {
		t.Fatal(err)
	}

	insert := func(peaks string) error {
		_, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO attachments (id, message_id, kind, storage_key, position, duration_ms, peaks, transcript_state)
			VALUES ($1, $2, 'voice', 'k', $3, 100, %s, 'pending')`, peaks),
			uuid.New(), m.id, nextPos())
		return err
	}

	if err := insert(`ARRAY[0,50,100]::smallint[]`); err != nil {
		t.Errorf("a valid peaks array was refused: %v", err)
	}
	if err := insert(`ARRAY[0,50,101]::smallint[]`); err == nil {
		t.Error("peaks element 101 was accepted; the wire's maximum is 100")
	}
	if err := insert(`ARRAY[-1]::smallint[]`); err == nil {
		t.Error("peaks element -1 was accepted; the wire's minimum is 0")
	}
	if err := insert(`(SELECT array_agg(50)::smallint[] FROM generate_series(1,513))`); err == nil {
		t.Error("513 peaks were accepted; the wire's maxItems is 512")
	}
	if err := insert(`(SELECT array_agg(50)::smallint[] FROM generate_series(1,512))`); err != nil {
		t.Errorf("512 peaks were refused: %v", err)
	}
}

var posCounter int

func nextPos() int { posCounter++; return posCounter }

// Ruling 6 — a voice note without a transcript state cannot be stored, because
// Transcript is a REQUIRED field of VoiceAttachment: without it the server
// cannot serve a voice note at all, not even a pending one.
func TestPerKindRequiredFields(t *testing.T) {
	ctx, pool := freshDB(t)
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")
	m, err := sendMessage(ctx, pool, conv, u, nil, uuid.New(), "attachments")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		cols    string
		vals    string
		wantErr bool
	}{
		{"voice with everything", "duration_ms, peaks, transcript_state", "100, ARRAY[1]::smallint[], 'pending'", false},
		{"voice with no transcript_state", "duration_ms, peaks", "100, ARRAY[1]::smallint[]", true},
		{"voice with no peaks", "duration_ms, transcript_state", "100, 'pending'", true},
		{"voice with no duration", "peaks, transcript_state", "ARRAY[1]::smallint[], 'pending'", true},
		{"unknown transcript state", "duration_ms, peaks, transcript_state", "100, ARRAY[1]::smallint[], 'queued'", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, fmt.Sprintf(`
				INSERT INTO attachments (id, message_id, kind, storage_key, position, %s)
				VALUES ($1, $2, 'voice', 'k', $3, %s)`, tc.cols, tc.vals),
				uuid.New(), m.id, nextPos())
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}

	imageCases := []struct {
		name    string
		cols    string
		vals    string
		wantErr bool
	}{
		{"image with everything", "filename, width, height, bytes", "'a.png', 10, 10, 900", false},
		{"image with no dimensions", "filename, bytes", "'a.png', 900", true},
		{"image with no filename", "width, height, bytes", "10, 10, 900", true},
		{"zero width", "filename, width, height, bytes", "'a.png', 0, 10, 900", true},
	}
	for _, tc := range imageCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, fmt.Sprintf(`
				INSERT INTO attachments (id, message_id, kind, storage_key, position, %s)
				VALUES ($1, $2, 'image', 'k', $3, %s)`, tc.cols, tc.vals),
				uuid.New(), m.id, nextPos())
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// Criterion 10 — messages.at is server-assigned. Client clocks would put `at`
// order in disagreement with seq order, and the wire promises timestamps sort
// chronologically as strings.
func TestMessageAtIsServerAssigned(t *testing.T) {
	ctx, pool := freshDB(t)

	var def *string
	err := pool.QueryRow(ctx, `
		SELECT column_default FROM information_schema.columns
		WHERE table_name = 'messages' AND column_name = 'at'`).Scan(&def)
	if err != nil {
		t.Fatal(err)
	}
	if def == nil || !strings.Contains(*def, "clock_timestamp") {
		t.Fatalf("messages.at default = %v, want clock_timestamp() — now() is the transaction's start time and can order two concurrent inserts opposite to the row lock that ordered their seqs", def)
	}

	// And `at` order agrees with seq order for sends that actually raced.
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room")
	const n = 15
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sendMessage(ctx, pool, conv, u, nil, uuid.New(), "m")
		}()
	}
	close(start)
	wg.Wait()

	var disagreements int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT seq,
			       lag(at) OVER (ORDER BY seq) AS prev_at,
			       at
			FROM messages WHERE conversation_id = $1
		) t WHERE prev_at IS NOT NULL AND at < prev_at`, conv).Scan(&disagreements); err != nil {
		t.Fatal(err)
	}
	if disagreements != 0 {
		t.Errorf("%d messages have an `at` earlier than the message before them by seq", disagreements)
	}
}

// ---------------------------------------------------------------------------
// Criterion 5 — the shape matches the plan, column for column
//
// The plan is the decision of record and its tables are the acceptance
// contract. This is the test that makes them one thing rather than two: a
// column that drifts from the approved shape fails here, named.

type col struct {
	name     string
	dataType string
	nullable bool
}

func TestSchemaMatchesThePlanColumnForColumn(t *testing.T) {
	ctx, pool := freshDB(t)

	want := map[string][]col{
		"users": {
			{"id", "uuid", false},
			{"handle", "text", false},
			{"display_name", "text", false},
			{"created_at", "timestamp with time zone", false},
			{"deactivated_at", "timestamp with time zone", true},
		},
		"devices": {
			{"id", "uuid", false},
			{"user_id", "uuid", false},
			{"name", "text", false},
			{"created_at", "timestamp with time zone", false},
			{"last_seen_at", "timestamp with time zone", true},
			{"revoked_at", "timestamp with time zone", true},
		},
		"conversations": {
			{"id", "uuid", false},
			{"kind", "text", false},
			{"name", "text", true},
			{"direct_key", "text", true},
			{"last_seq", "bigint", false},
			{"retention_days", "integer", true},
			{"created_at", "timestamp with time zone", false},
		},
		"conversation_members": {
			{"conversation_id", "uuid", false},
			{"user_id", "uuid", false},
			{"joined_at", "timestamp with time zone", false},
			{"read_seq", "bigint", false},
			{"muted", "boolean", false},
		},
		"messages": {
			{"id", "uuid", false},
			{"conversation_id", "uuid", false},
			{"author_id", "uuid", false},
			{"seq", "bigint", false},
			{"log_seq", "bigint", false},
			{"updated_log_seq", "bigint", false},
			{"at", "timestamp with time zone", false},
			{"text", "text", true},
			{"reply_to", "uuid", true},
			{"client_id", "uuid", true},
			{"sender_device_id", "uuid", true},
			{"edited_at", "timestamp with time zone", true},
			{"deleted", "boolean", false},
			{"transcript_text", "text", true},
		},
		"attachments": {
			{"id", "uuid", false},
			{"message_id", "uuid", false},
			{"kind", "text", false},
			{"storage_key", "text", false},
			{"position", "integer", false},
			{"duration_ms", "integer", true},
			{"peaks", "ARRAY", true},
			{"transcript_state", "text", true},
			{"transcript_json", "jsonb", true},
			{"filename", "text", true},
			{"width", "integer", true},
			{"height", "integer", true},
			{"bytes", "bigint", true},
			{"placeholder", "text", true},
		},
		"log_counter": {
			{"id", "integer", false},
			{"value", "bigint", false},
		},
	}

	for table, cols := range want {
		got := describeTable(ctx, t, pool, table)
		byName := map[string]col{}
		for _, c := range got {
			byName[c.name] = c
		}
		for _, w := range cols {
			g, ok := byName[w.name]
			if !ok {
				t.Errorf("%s.%s is missing", table, w.name)
				continue
			}
			if g.dataType != w.dataType {
				t.Errorf("%s.%s type = %s, plan says %s", table, w.name, g.dataType, w.dataType)
			}
			if g.nullable != w.nullable {
				t.Errorf("%s.%s nullable = %v, plan says %v", table, w.name, g.nullable, w.nullable)
			}
			delete(byName, w.name)
		}
		for extra := range byName {
			t.Errorf("%s.%s exists and is not in the plan", table, extra)
		}
	}

	// And nothing outside the plan's seven.
	rows, err := pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if _, ok := want[n]; !ok {
			t.Errorf("table %q exists and is not in the plan", n)
		}
	}
}

func describeTable(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) []col {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT column_name, data_type, is_nullable = 'YES'
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatalf("describe %s: %v", table, err)
	}
	defer rows.Close()

	var out []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.name, &c.dataType, &c.nullable); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		t.Fatalf("table %s does not exist", table)
	}
	return out
}

// ---------------------------------------------------------------------------
// Criterion 6 — every foreign key names its ON DELETE
//
// Ruling 4 named one FK of the set, for the right reason. CANT-67 deletes
// MESSAGES on a timer, so the two that point at messages decide whether the
// sweep can run at all — and an unstated ON DELETE is a default nobody chose.
func TestEveryForeignKeyNamesItsOnDelete(t *testing.T) {
	ctx, pool := freshDB(t)

	// The plan's table, verbatim. It calls this set "all seven"; there are in
	// fact eight, because messages carries three FKs rather than two. The
	// extra one is messages.sender_device_id, and it is RESTRICT for the same
	// reason as the rest.
	want := map[string]string{
		"messages.author_id -> users":                           "RESTRICT",
		"messages.reply_to -> messages":                         "SET NULL",
		"attachments.message_id -> messages":                    "CASCADE",
		"devices.user_id -> users":                              "RESTRICT",
		"conversation_members.conversation_id -> conversations": "RESTRICT",
		"conversation_members.user_id -> users":                 "RESTRICT",
		"messages.conversation_id -> conversations":             "RESTRICT",
		"messages.sender_device_id -> devices":                  "RESTRICT",
	}

	rows, err := pool.Query(ctx, `
		SELECT src.relname, att.attname, tgt.relname,
		       CASE c.confdeltype
		           WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT'
		           WHEN 'c' THEN 'CASCADE'   WHEN 'n' THEN 'SET NULL'
		           WHEN 'd' THEN 'SET DEFAULT'
		       END
		FROM pg_constraint c
		JOIN pg_class src ON src.oid = c.conrelid
		JOIN pg_class tgt ON tgt.oid = c.confrelid
		JOIN pg_attribute att ON att.attrelid = c.conrelid AND att.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND src.relnamespace = 'public'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var srcTable, srcCol, tgtTable, action string
		if err := rows.Scan(&srcTable, &srcCol, &tgtTable, &action); err != nil {
			t.Fatal(err)
		}
		got[fmt.Sprintf("%s.%s -> %s", srcTable, srcCol, tgtTable)] = action
	}

	for fk, wantAction := range want {
		gotAction, ok := got[fk]
		if !ok {
			t.Errorf("foreign key %s is missing", fk)
			continue
		}
		if gotAction != wantAction {
			t.Errorf("%s is ON DELETE %s, plan says %s", fk, gotAction, wantAction)
		}
		delete(got, fk)
	}
	for fk, action := range got {
		t.Errorf("foreign key %s (ON DELETE %s) exists and is not in the plan", fk, action)
	}
}

// ---------------------------------------------------------------------------
// Criterion 1, structural half — the dedup constraint is where the plan says

func TestDedupConstraintIsOnAuthorAndClientID(t *testing.T) {
	ctx, pool := freshDB(t)

	var cols []string
	err := pool.QueryRow(ctx, `
		SELECT array_agg(a.attname ORDER BY k.ord)
		FROM pg_constraint c
		CROSS JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conrelid = 'messages'::regclass AND c.contype = 'u'
		  AND EXISTS (
		      SELECT 1 FROM pg_attribute x
		      WHERE x.attrelid = c.conrelid AND x.attnum = ANY (c.conkey) AND x.attname = 'client_id')
		GROUP BY c.oid`).Scan(&cols)
	if err != nil {
		t.Fatalf("no unique constraint covers client_id: %v", err)
	}
	if len(cols) != 2 || cols[0] != "author_id" || cols[1] != "client_id" {
		t.Errorf("dedup constraint is on %v, want (author_id, client_id) per ClientSend.client_id", cols)
	}
}
