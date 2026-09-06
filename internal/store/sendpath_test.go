package store

// CANT-83 stage 3 — criteria 6, 8 and 17, and criterion 10's send-path half.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/wire"
)

func counters(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conv uuid.UUID) (lastSeq, logCounter int64) {
	t.Helper()
	mustScan(t, pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, conv), &lastSeq)
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &logCounter)
	return
}

// A replay draws nothing: the idempotency check returns before either ordinal
// is drawn, so re-sending an acked message leaves both counters where they
// were.
func TestAReplayAdvancesNothing(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	key := uuid.New()

	if _, err := st.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Text: ptr("once"),
	}); err != nil {
		t.Fatalf("first send: %v", err)
	}
	beforeLast, beforeLog := counters(ctx, t, pool, conv)

	if _, err := st.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Text: ptr("once"),
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	afterLast, afterLog := counters(ctx, t, pool, conv)
	if afterLast != beforeLast || afterLog != beforeLog {
		t.Errorf("a replay moved the counters: last_seq %d→%d, log_counter %d→%d",
			beforeLast, afterLast, beforeLog, afterLog)
	}
}

// Criterion 8. Everything fallible happens before the log_counter draw, so a
// refusal consumes NO ordinals — neither the conversation's dense seq nor the
// deployment-wide counter. A hole in either is a message a client believes it
// is missing.
func TestARefusalConsumesNoOrdinals(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	stranger := mkUser(ctx, t, pool, "stranger")
	conv := mkGroup(ctx, t, pool, "room", author)
	st := New(pool, DefaultLimits(), discardLogger())

	if _, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("real"),
	}); err != nil {
		t.Fatalf("seeding a real send: %v", err)
	}
	beforeLast, beforeLog := counters(ctx, t, pool, conv)

	tiny := New(pool, Limits{MaxMessageBytes: 2, MaxAttachments: 1}, discardLogger())
	for _, tc := range []struct {
		name string
		st   *Store
		msg  NewMessage
	}{
		{"not a member", st, NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: stranger, Text: ptr("no")}},
		{"no such conversation", st, NewMessage{ClientID: uuid.New(), ConversationID: uuid.New(), AuthorID: author, Text: ptr("no")}},
		{"too large", tiny, NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("far too long")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.st.SendMessage(ctx, tc.msg); err == nil {
				t.Fatal("expected a refusal")
			}
			last, log := counters(ctx, t, pool, conv)
			if last != beforeLast {
				t.Errorf("last_seq %d→%d: the refusal burned a dense seq", beforeLast, last)
			}
			if log != beforeLog {
				t.Errorf("log_counter %d→%d: the refusal burned a global ordinal", beforeLog, log)
			}
		})
	}
}

// Criterion 17, the store's half. Ruling 4: a reply_to that does not resolve,
// or resolves into another conversation, is stored as NULL and the send
// SUCCEEDS. The wire builds ReplyRef live with a one-line preview of the
// source, so a cross-conversation reply_to would render another conversation's
// text to every member of this one.
func TestAnUnresolvableReplyToIsStoredAsNullAndLogged(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	here := mkGroup(ctx, t, pool, "here", author)
	elsewhere := mkGroup(ctx, t, pool, "elsewhere", author)

	seed := New(pool, DefaultLimits(), discardLogger())
	other, err := seed.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: elsewhere, AuthorID: author, Text: ptr("private to elsewhere"),
	})
	if err != nil {
		t.Fatalf("seeding the other conversation: %v", err)
	}
	mine, err := seed.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: here, AuthorID: author, Text: ptr("a real source"),
	})
	if err != nil {
		t.Fatalf("seeding this conversation: %v", err)
	}
	missing := uuid.New()

	for _, tc := range []struct {
		name    string
		replyTo uuid.UUID
		wantNil bool
		wantLog string
	}{
		{"a source in another conversation", other.ID, true, "another conversation"},
		{"a source that does not exist", missing, true, "does not resolve"},
		{"a source in this conversation", mine.ID, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger()
			st := New(pool, DefaultLimits(), logger)

			ref := tc.replyTo
			sent, err := st.SendMessage(ctx, NewMessage{
				ClientID: uuid.New(), ConversationID: here, AuthorID: author,
				Text: ptr("a reply"), ReplyTo: &ref,
			})
			if err != nil {
				t.Fatalf("the send was refused rather than having its ref dropped: %v", err)
			}

			var stored *uuid.UUID
			mustScan(t, pool.QueryRow(ctx, `SELECT reply_to FROM messages WHERE id = $1`, sent.ID), &stored)
			if tc.wantNil && stored != nil {
				t.Errorf("reply_to = %v, want NULL", *stored)
			}
			if !tc.wantNil && (stored == nil || *stored != tc.replyTo) {
				t.Errorf("reply_to = %v, want %v", stored, tc.replyTo)
			}

			lines := logLines(t, buf)
			if tc.wantLog == "" {
				if len(lines) != 0 {
					t.Errorf("a valid reply_to logged %d line(s): %v", len(lines), lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("want exactly one line about the dropped ref, got %d: %v", len(lines), lines)
			}
			if msg, _ := lines[0]["msg"].(string); !strings.Contains(msg, tc.wantLog) {
				t.Errorf("log message %q does not say why the ref was dropped", msg)
			}
			// Both ids, because someone will eventually ask why the reply
			// arrow vanished and the answer has to be findable.
			if lines[0]["reply_to_message_id"] != tc.replyTo.String() {
				t.Errorf("the line does not name the source: %v", lines[0])
			}
		})
	}
}

// Criterion 10's send-path half, and the refusal-logging contract. Every
// refusal logs exactly ONCE, at warn for internal and info for the rest, with
// the ids and the code-derived fields — and never the body.
func TestEveryRefusalLogsOnceWithItsCodeAndNeverTheBody(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	stranger := mkUser(ctx, t, pool, "stranger")
	conv := mkGroup(ctx, t, pool, "room", author)
	secret := "the note nobody should find in a log file"

	for _, tc := range []struct {
		name      string
		limits    Limits
		msg       NewMessage
		wantCode  wire.ErrorCode
		wantLevel string
	}{
		{
			name: "not_a_member logs at info", limits: DefaultLimits(),
			msg:      NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: stranger, Text: &secret},
			wantCode: wire.ErrorCodeNotAMember, wantLevel: "INFO",
		},
		{
			name: "message_too_large logs at info and not the body", limits: Limits{MaxMessageBytes: 4, MaxAttachments: 16},
			msg:      NewMessage{ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: &secret},
			wantCode: wire.ErrorCodeMessageTooLarge, wantLevel: "INFO",
		},
		{
			name: "a missing key is internal and logs at warn", limits: DefaultLimits(),
			msg:      NewMessage{ConversationID: conv, AuthorID: author, Text: &secret},
			wantCode: wire.ErrorCodeInternal, wantLevel: "WARN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger()
			if _, err := New(pool, tc.limits, logger).SendMessage(ctx, tc.msg); err == nil {
				t.Fatal("expected a refusal")
			}

			lines := logLines(t, buf)
			if len(lines) != 1 {
				t.Fatalf("want exactly one line per refusal, got %d: %v", len(lines), lines)
			}
			got := lines[0]
			if got["level"] != tc.wantLevel {
				t.Errorf("level = %v, want %s", got["level"], tc.wantLevel)
			}
			if got["code"] != string(tc.wantCode) {
				t.Errorf("code = %v, want %q", got["code"], tc.wantCode)
			}
			for _, field := range []string{"conversation_id", "author_id", "client_id", "retryable"} {
				if _, ok := got[field]; !ok {
					t.Errorf("the line has no %s: %v", field, got)
				}
			}
			if strings.Contains(buf.String(), secret) {
				t.Error("the refusal log contains the message body")
			}
		})
	}
}

// The fix for the reply_to TOCTOU is one clause, so the test is about that
// clause. Position 8b reads the source FOR KEY SHARE, which blocks a concurrent
// DELETE until the send commits — without it, a source deleted between the read
// and the insert makes position 11's FK check raise 23503, and class 23 is not
// transient, so the send fails PERMANENTLY over a reply_to the sender cannot
// fix. That is the outcome Ruling 4 exists to prevent, arriving at the insert.
//
// It runs the query the send path runs, not a copy: drop FOR KEY SHARE from
// replyToResolveQuery and this goes red.
func TestTheReplyToResolveHoldsItsSourceAgainstAConcurrentDelete(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	source, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("the source"),
	})
	if err != nil {
		t.Fatalf("seeding the source: %v", err)
	}

	// A sender holding the lock, exactly as position 8b does.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var got uuid.UUID
	if err := holder.QueryRow(ctx, replyToResolveQuery, source.ID, conv).Scan(&got); err != nil {
		t.Fatalf("the resolve query failed: %v", err)
	}

	// A sweep or an edit trying to delete it. lock_timeout turns "blocked
	// forever" into an observable error rather than a hung test.
	deleter, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin deleter: %v", err)
	}
	if _, err := deleter.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	_, delErr := deleter.Exec(ctx, `DELETE FROM messages WHERE id = $1`, source.ID)
	_ = deleter.Rollback(ctx)

	if delErr == nil {
		t.Fatal("the source was deleted while a send held it — FOR KEY SHARE is not being taken, " +
			"so a concurrent delete can still make the insert raise 23503")
	}
	var pgErr *pgconn.PgError
	if !errors.As(delErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("delete failed with %v, want 55P03 (lock_not_available) — it should be BLOCKED, "+
			"not refused for some other reason", delErr)
	}

	// And the lock is released with the send: once the holder is done, the
	// sweep gets its row. A lock that outlived the transaction would wedge
	// CANT-67 rather than protect this send.
	_ = holder.Rollback(ctx)
	if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE id = $1`, source.ID); err != nil {
		t.Errorf("the source could not be deleted after the holder finished: %v", err)
	}
}

// firstUnreadSeq is CANT-26's derivation, written here rather than imported
// because CANT-26 has not been built. It is the query this ticket's decision
// rests on, so it is exercised rather than asserted: the first seq above
// read_seq that the VIEWER did not write.
//
// Not a scan — UNIQUE (conversation_id, seq) indexes it, and it stops at the
// first row it finds.
func firstUnreadSeq(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conv, viewer uuid.UUID) *int64 {
	t.Helper()
	var seq *int64
	mustScan(t, pool.QueryRow(ctx, `
		SELECT MIN(seq) FROM messages
		 WHERE conversation_id = $1
		   AND author_id <> $2
		   AND seq > (SELECT read_seq FROM conversation_members
		               WHERE conversation_id = $1 AND user_id = $2)`,
		conv, viewer), &seq)
	return seq
}

// CANT-83's replacement for criterion 6, and the reason that criterion was
// dropped rather than delivered.
//
// The removed version advanced the author's own read_seq to the seq they just
// drew, so that first_unread_seq could be read_seq + 1. That is wrong whenever
// the author was not already caught up: replying without opening the thread
// swallowed everything below the new message. A scalar read_seq cannot say
// "1-5 unread, 6 is mine"; the author filter can, and it is one indexed lookup.
func TestReplyingDoesNotMarkAnUnreadBacklogRead(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bob := mkUser(ctx, t, pool, "bob")
	alice := mkUser(ctx, t, pool, "alice")
	conv := mkGroup(ctx, t, pool, "room", bob, alice)

	for i := 0; i < 5; i++ {
		if _, err := st.SendMessage(ctx, NewMessage{
			ClientID: uuid.New(), ConversationID: conv, AuthorID: bob, Text: ptr("unread"),
		}); err != nil {
			t.Fatalf("bob's send %d: %v", i+1, err)
		}
	}

	// Alice replies from a notification without opening the thread.
	sent, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: alice, Text: ptr("on it"),
	})
	if err != nil {
		t.Fatalf("alice's reply: %v", err)
	}
	if sent.Seq != 6 {
		t.Fatalf("alice's reply drew seq %d, want 6", sent.Seq)
	}

	// THE SEND TOUCHED NOTHING. This is the assertion that stops position 9
	// coming back: read_seq is CANT-26's column and a send has no business in
	// it.
	var aliceRead int64
	mustScan(t, pool.QueryRow(ctx,
		`SELECT read_seq FROM conversation_members WHERE conversation_id = $1 AND user_id = $2`,
		conv, alice), &aliceRead)
	if aliceRead != 0 {
		t.Errorf("alice's read_seq = %d after replying; a send must not advance it — "+
			"the old behaviour set it to 6 and silently marked bob's five messages read", aliceRead)
	}

	// And the derivation still gives her the right answer: bob's five are
	// unread, her own reply is not.
	got := firstUnreadSeq(ctx, t, pool, conv, alice)
	if got == nil || *got != 1 {
		t.Errorf("alice's first_unread_seq = %v, want 1 — her backlog was cleared by her own reply", got)
	}

	// The other direction, which is the half read_seq + 1 got right and must
	// not be lost: your own message is never your own unread.
	got = firstUnreadSeq(ctx, t, pool, conv, bob)
	if got == nil || *got != 6 {
		t.Errorf("bob's first_unread_seq = %v, want 6 — his own five must not count as his unread", got)
	}
}

// The other half of the lock's contract, and the half the first version of this
// change got wrong: a source in ANOTHER conversation must not be locked at all.
//
// `FOR KEY SHARE` locks only the rows a query returns, so scoping the read to
// this conversation is what decides which row is touched. Unscoped, a send in
// conversation A took and held a lock on a message in conversation B that it
// was about to discard — a lock nothing in that transaction had any business
// holding, and one the table-level ordering rule cannot express: two writers
// can each obey "conversations then messages" and still deadlock when their
// two locks are in different conversations.
func TestACrossConversationReplyToSourceIsNeverLocked(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	here := mkGroup(ctx, t, pool, "here", author)
	elsewhere := mkGroup(ctx, t, pool, "elsewhere", author)

	other, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: elsewhere, AuthorID: author, Text: ptr("not yours"),
	})
	if err != nil {
		t.Fatalf("seeding the other conversation: %v", err)
	}

	// A sender in `here` resolving a source that lives in `elsewhere`, exactly
	// as position 8b does.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var got uuid.UUID
	if err := holder.QueryRow(ctx, replyToResolveQuery, other.ID, here).Scan(&got); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the scoped resolve returned %v/%v for a source in another conversation, want no rows", got, err)
	}

	// The sweep must not be blocked by a send that is not going to store this.
	deleter, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin deleter: %v", err)
	}
	defer func() { _ = deleter.Rollback(ctx) }()
	if _, err := deleter.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	if _, err := deleter.Exec(ctx, `DELETE FROM messages WHERE id = $1`, other.ID); err != nil {
		t.Fatalf("a send in another conversation is holding this row: %v — the resolve is "+
			"locking a source it will discard, which is a lock the ordering rule cannot account for", err)
	}
}
