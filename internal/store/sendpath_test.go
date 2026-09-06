package store

// CANT-83 stage 3 — criteria 6, 8 and 17, and criterion 10's send-path half.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/wire"
)

func readSeq(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conv, user uuid.UUID) int64 {
	t.Helper()
	var n int64
	mustScan(t, pool.QueryRow(ctx,
		`SELECT read_seq FROM conversation_members WHERE conversation_id = $1 AND user_id = $2`,
		conv, user), &n)
	return n
}

func counters(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conv uuid.UUID) (lastSeq, logCounter int64) {
	t.Helper()
	mustScan(t, pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, conv), &lastSeq)
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &logCounter)
	return
}

// Criterion 6. You have read what you just sent — 0002_conversations says so,
// and CANT-26's first_unread_seq rests on it: with the author's own send
// advancing read_seq, first_unread_seq is arithmetic rather than a scan that
// has to skip your own messages. Nothing wrote it until now.
func TestAnAuthorsOwnSendAdvancesTheirReadSeq(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	other := mkUser(ctx, t, pool, "other")
	conv := mkGroup(ctx, t, pool, "room", author, other)

	if got := readSeq(ctx, t, pool, conv, author); got != 0 {
		t.Fatalf("read_seq starts at %d, want 0", got)
	}

	sent, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("mine"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := readSeq(ctx, t, pool, conv, author); got != sent.Seq {
		t.Errorf("the author's read_seq = %d after sending seq %d", got, sent.Seq)
	}

	// The OTHER member is untouched: you have read what you sent, not what
	// anyone else sent, and an unread count that moved for a message somebody
	// else wrote would be the drift Invariant 3 exists to prevent.
	if got := readSeq(ctx, t, pool, conv, other); got != 0 {
		t.Errorf("another member's read_seq moved to %d on somebody else's send", got)
	}
}

// GREATEST, not assignment. A read receipt for a LATER seq can commit between
// this send's seq draw and the advance; assigning would walk it backwards and
// resurrect messages the member has already read.
func TestTheReadSeqAdvanceIsAFloorAndNeverWalksBackwards(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)

	for i := 0; i < 3; i++ {
		if _, err := st.SendMessage(ctx, NewMessage{
			ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("x"),
		}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// A receipt lands ahead of anything this author has sent — the shape
	// CANT-26 will write.
	if _, err := pool.Exec(ctx,
		`UPDATE conversation_members SET read_seq = 99 WHERE conversation_id = $1 AND user_id = $2`,
		conv, author); err != nil {
		t.Fatalf("planting a later receipt: %v", err)
	}

	sent, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author, Text: ptr("after"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := readSeq(ctx, t, pool, conv, author); got != 99 {
		t.Errorf("read_seq = %d after a send at seq %d under a receipt at 99 — "+
			"the advance is an assignment, not a floor", got, sent.Seq)
	}
}

// A replay never reaches position 9: the idempotency check returns before any
// draw, so re-sending an acked message cannot drag a read_seq that has since
// moved on back to where it was.
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
	if _, err := pool.Exec(ctx,
		`UPDATE conversation_members SET read_seq = 50 WHERE conversation_id = $1 AND user_id = $2`,
		conv, author); err != nil {
		t.Fatal(err)
	}
	beforeLast, beforeLog := counters(ctx, t, pool, conv)

	if _, err := st.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Text: ptr("once"),
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if got := readSeq(ctx, t, pool, conv, author); got != 50 {
		t.Errorf("read_seq = %d after a replay, want 50 — the replay reached position 9", got)
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
			if msg, _ := lines[0]["msg"].(string); !contains(msg, tc.wantLog) {
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

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
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
			if indexOf(buf.String(), secret) >= 0 {
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
	if err := holder.QueryRow(ctx, replyToResolveQuery, source.ID).Scan(&got); err != nil {
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
