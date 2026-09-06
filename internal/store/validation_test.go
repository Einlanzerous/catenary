package store

// CANT-83 stage 2 — criteria 1, 3, 4 and 11: what the send path refuses, where
// it refuses it, and the one refusal a replay does NOT win over.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/wire"
)

// closedPool is the idiom TestTheMissingKeyGuardFiresBeforeTheDatabaseIsTouched
// established: pgxpool.New parses the DSN and dials nothing, so closing it makes
// any Begin fail immediately and locally. A guard that still refuses over this
// pool is a guard that ran before the database was touched.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/unreachable")
	if err != nil {
		t.Fatalf("constructing the pool should not need a server: %v", err)
	}
	pool.Close()
	return pool
}

// Criterion 3. Both bounds are pure arithmetic over the frame, so they must
// fail without a transaction and without a round trip. Over a closed pool, a
// bound that has drifted below Begin returns a connection error instead.
func TestTheSizeBoundsFireBeforeTheDatabaseIsTouched(t *testing.T) {
	st := New(closedPool(t), Limits{MaxMessageBytes: 8, MaxAttachments: 2})
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		msg  NewMessage
		want error
		code wire.ErrorCode
	}{
		{
			name: "a body over the byte bound",
			msg:  NewMessage{ClientID: uuid.New(), ConversationID: uuid.New(), AuthorID: uuid.New(), Text: ptr("nine char")},
			want: ErrMessageTooLarge,
			code: wire.ErrorCodeMessageTooLarge,
		},
		{
			name: "more attachments than the bound",
			msg: NewMessage{ClientID: uuid.New(), ConversationID: uuid.New(), AuthorID: uuid.New(),
				Attachments: []NewAttachment{{Kind: "image"}, {Kind: "image"}, {Kind: "image"}}},
			want: ErrTooManyAttachments,
			code: wire.ErrorCodeMessageTooLarge,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.SendMessage(ctx, tc.msg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("over a closed pool the send failed with %v, want %v — the bound reached the database, so it is no longer ahead of Begin", err, tc.want)
			}
			var se *SendError
			if !errors.As(err, &se) {
				t.Fatalf("the refusal is not a *SendError: %T", err)
			}
			if se.Code != tc.code {
				t.Errorf("code = %q, want %q", se.Code, tc.code)
			}
			if se.Retryable {
				t.Error("a size refusal is not retryable — the identical frame fails identically")
			}
		})
	}
}

// The bound counts BYTES, not runes. The column and the wire both count bytes,
// and a rune bound would refuse a different set of messages than the database
// would accept.
func TestTheBodyBoundCountsBytesNotRunes(t *testing.T) {
	st := New(closedPool(t), Limits{MaxMessageBytes: 8, MaxAttachments: 16})
	ctx := context.Background()

	// Six runes, twelve bytes.
	_, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: uuid.New(), AuthorID: uuid.New(),
		Text: ptr(strings.Repeat("é", 6)),
	})
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("6 runes of 2 bytes each passed an 8-byte bound: %v", err)
	}
}

// The size is reportable; the BODY never is. D1 declines end-to-end encryption
// and names its mitigation as honesty about what the server can see — worth
// less if a size refusal copies the message into a log with a different
// retention story than the message itself.
func TestASizeRefusalDoesNotCarryTheBody(t *testing.T) {
	st := New(closedPool(t), Limits{MaxMessageBytes: 4, MaxAttachments: 16})
	secret := "hunter2-and-the-rest-of-the-note"

	_, err := st.SendMessage(context.Background(), NewMessage{
		ClientID: uuid.New(), ConversationID: uuid.New(), AuthorID: uuid.New(), Text: &secret,
	})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal quotes the body: %v", err)
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("the refusal should name the size it measured: %v", err)
	}
}

// Criterion 1, both halves. The two codes are distinguishable on purpose, and
// the existence leak is the accepted cost — a non-member learns that an id
// resolves.
func TestMembershipAndExistenceAreDecidedInTheStore(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits())
	author := mkUser(ctx, t, pool, "author")
	stranger := mkUser(ctx, t, pool, "stranger")
	conv := mkGroup(ctx, t, pool, "room", author)

	t.Run("a non-member send is not_a_member", func(t *testing.T) {
		_, err := st.SendMessage(ctx, NewMessage{
			ClientID: uuid.New(), ConversationID: conv, AuthorID: stranger, Text: ptr("let me in"),
		})
		assertCode(t, err, wire.ErrorCodeNotAMember, ErrNotAMember)
	})

	t.Run("a conversation that does not resolve is conversation_not_found", func(t *testing.T) {
		_, err := st.SendMessage(ctx, NewMessage{
			ClientID: uuid.New(), ConversationID: uuid.New(), AuthorID: author, Text: ptr("into the void"),
		})
		assertCode(t, err, wire.ErrorCodeConversationNotFound, ErrConversationNotFound)
	})
}

func assertCode(t *testing.T, err error, want wire.ErrorCode, cause error) {
	t.Helper()
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("refused with %T (%v), want a *SendError", err, err)
	}
	if se.Code != want {
		t.Errorf("code = %q, want %q", se.Code, want)
	}
	if se.Retryable {
		t.Error("retryable = true; the identical frame will fail identically")
	}
	if !errors.Is(err, cause) {
		t.Errorf("the cause is not reachable through errors.Is (want %v)", cause)
	}
}

// Criterion 4, first half. The idempotency check sits ABOVE every refusal that
// depends on server state, so a replay returns the original message even after
// the sender has been removed from the conversation.
//
// The alternative — validate first — means an author whose ack was lost retries
// and is told not_a_member about a message sitting in the log with their name
// on it, while everyone else can see it.
func TestAReplayWinsOverAStateDependentRefusal(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits())
	author := mkUser(ctx, t, pool, "leaver")
	conv := mkGroup(ctx, t, pool, "room", author)
	key := uuid.New()

	first, err := st.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Text: ptr("still mine"),
	})
	if err != nil {
		t.Fatalf("first send: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`DELETE FROM conversation_members WHERE conversation_id = $1 AND user_id = $2`,
		conv, author); err != nil {
		t.Fatalf("removing the author from the conversation: %v", err)
	}

	replay, err := st.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Text: ptr("still mine"),
	})
	if err != nil {
		t.Fatalf("the replay of an acked message was refused after the author left: %v", err)
	}
	if !replay.Duplicate {
		t.Error("the replay did not report itself a duplicate")
	}
	if replay.ID != first.ID || replay.Seq != first.Seq || replay.LogSeq != first.LogSeq {
		t.Errorf("the replay returned a different message: %+v vs %+v", replay, first)
	}
}

// Criterion 4, second half — the NAMED cost. The bounds sit at position 2,
// above the idempotency check, because a body over the bound should not open a
// transaction. So an operator who lowers CATENARY_MAX_MESSAGE_BYTES after a
// message was acked makes that message's replay report message_too_large.
//
// Accepted rather than designed away: moving the bounds below the check buys
// consistency for a rare, deliberate operator action at the price of a
// transaction per oversized frame. Pinned here so it stays a decision.
func TestAReplayDoesNotWinOverALoweredSizeBound(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "author")
	conv := mkGroup(ctx, t, pool, "room", author)
	key := uuid.New()
	body := ptr("a body of some length")

	generous := New(pool, DefaultLimits())
	if _, err := generous.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Text: body,
	}); err != nil {
		t.Fatalf("first send: %v", err)
	}

	// The operator lowers the bound under the acked message.
	strict := New(pool, Limits{MaxMessageBytes: 4, MaxAttachments: 16})
	_, err := strict.SendMessage(ctx, NewMessage{
		ClientID: key, ConversationID: conv, AuthorID: author, Text: body,
	})
	assertCode(t, err, wire.ErrorCodeMessageTooLarge, ErrMessageTooLarge)
}

// Criterion 11. A send with neither text nor attachments is STORED. Both fields
// are optional on ClientSend so it is a legal frame, and no ErrorCode describes
// it: refusing means either widening a closed enum — a wire change under
// CANT-74's unresolved compatibility policy — or reporting a client bug as
// `internal`, which is a lie about whose fault it is.
func TestAnEmptySendIsStored(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits())
	author := mkUser(ctx, t, pool, "quiet")
	conv := mkGroup(ctx, t, pool, "room", author)

	sent, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: conv, AuthorID: author,
	})
	if err != nil {
		t.Fatalf("an empty send was refused: %v", err)
	}
	if sent.Seq != 1 {
		t.Errorf("seq = %d, want 1 — an empty send costs a seq like any other", sent.Seq)
	}

	var text *string
	mustScan(t, pool.QueryRow(ctx, `SELECT text FROM messages WHERE id = $1`, sent.ID), &text)
	if text != nil {
		t.Errorf("text = %q, want NULL", *text)
	}
}

// CANT-85's seam, pinned as a gap rather than left to a comment.
//
// NewMessage.Attachments is COUNTED by checkBounds and never written: the
// INSERT lists no attachment columns and no upload id is resolved. The field
// comment says so, but a comment does not stop CANT-22 or CANT-75 wiring
// ClientSend.Attachments through and acking a send whose attachments silently
// vanished. This test makes the gap visible and gives CANT-85 something to
// invert — when it lands, this assertion flips from 0 to 2 and the skip goes.
func TestAttachmentsAreCountedButNotYetStored(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits())
	u := mkUser(ctx, t, pool, "att")
	conv := mkGroup(ctx, t, pool, "room", u)

	sent, err := st.SendMessage(ctx, NewMessage{
		ConversationID: conv,
		AuthorID:       u,
		ClientID:       uuid.New(),
		Text:           ptr("two attachments, neither stored"),
		Attachments: []NewAttachment{
			{Kind: "image", UploadID: uuid.New()},
			{Kind: "voice", UploadID: uuid.New()},
		},
	})
	if err != nil {
		t.Fatalf("a send within CATENARY_MAX_ATTACHMENTS was refused: %v", err)
	}

	var rows int
	mustScan(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM attachments WHERE message_id = $1`, sent.ID), &rows)
	if rows != 0 {
		t.Fatalf("%d attachment rows exist — CANT-85 has landed, so this test should now assert "+
			"that both rows are present and committed with the message", rows)
	}
}

// CANT-83's half of the cross-conversation replay. The transaction half —
// membership, existence — is above; this is about what the ACK may say.
//
// Deduplication is scoped (author_id, client_id), not per conversation, so one
// key reused across two conversations returns the FIRST row. That is correct
// dedup and criterion 4 requires the check to sit above membership, so the
// behaviour stays. What must not happen is a transport acking the REQUEST's
// conversation with this row's seq: the client would then believe a seq exists
// in a thread it does not, and a dense seq it cannot see is a message it
// believes it is missing forever.
func TestAReplayUnderOneKeyReportsTheConversationTheRowIsActuallyIn(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits())
	u := mkUser(ctx, t, pool, "bot")
	first := mkGroup(ctx, t, pool, "first", u)
	second := mkGroup(ctx, t, pool, "second", u)

	key := uuid.New()
	original, err := st.SendMessage(ctx, NewMessage{
		ConversationID: first, AuthorID: u, ClientID: key, Text: ptr("hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if original.ConversationID != first {
		t.Fatalf("a fresh send reported conversation %s, want %s", original.ConversationID, first)
	}

	// The same key, a different conversation — the mechanical bot with a
	// deterministic key that CANT-75 describes.
	replay, err := st.SendMessage(ctx, NewMessage{
		ConversationID: second, AuthorID: u, ClientID: key, Text: ptr("hello"),
	})
	if err != nil {
		t.Fatalf("the replay was refused: %v", err)
	}
	if !replay.Duplicate {
		t.Error("the replay was not reported as a duplicate")
	}
	if replay.ConversationID != first {
		t.Errorf("the replay reported conversation %s, want %s — Sent must name the conversation the "+
			"row IS IN, or a transport building the ack from the request will place this seq in the "+
			"wrong thread's dense sequence", replay.ConversationID, first)
	}
	if replay.Seq != original.Seq || replay.LogSeq != original.LogSeq {
		t.Errorf("the replay returned seq %d/log_seq %d, want the original's %d/%d",
			replay.Seq, replay.LogSeq, original.Seq, original.LogSeq)
	}

	// And nothing was written into the second conversation.
	var inSecond int
	mustScan(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE conversation_id = $1`, second), &inSecond)
	if inSecond != 0 {
		t.Errorf("%d messages landed in the second conversation; a replay must draw nothing", inSecond)
	}
	var lastSeq int64
	mustScan(t, pool.QueryRow(ctx,
		`SELECT last_seq FROM conversations WHERE id = $1`, second), &lastSeq)
	if lastSeq != 0 {
		t.Errorf("the second conversation's last_seq advanced to %d; a replay must draw no ordinal", lastSeq)
	}
}

// The zero Limits value is legal Go and inverts both bounds — a store that
// refuses every message carrying any text, and every send carrying any
// attachment, silently. New's doc argues an optional bound fails quietly by
// not refusing; this is the same failure in the other and worse direction, so
// it is loud instead.
func TestNewRefusesANonPositiveBound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits Limits
	}{
		{"the zero value", Limits{}},
		{"no byte bound", Limits{MaxAttachments: 16}},
		{"no attachment bound", Limits{MaxMessageBytes: 16384}},
		{"a negative bound", Limits{MaxMessageBytes: -1, MaxAttachments: 16}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("New(pool, %+v) returned a store; it would refuse every send "+
						"with any text as message_too_large and say nothing about why", tc.limits)
				}
			}()
			_ = New(closedPool(t), tc.limits)
		})
	}

	// And the shape a caller who does not care is meant to use still works.
	if st := New(closedPool(t), DefaultLimits()); st.Limits() != DefaultLimits() {
		t.Error("DefaultLimits() did not survive New")
	}
}

// A closed pool is the LEAST ambiguous "nothing reached the server" there is,
// and it was classified permanent until CANT-83's review caught it: puddle
// returns a plain errors.New, so no PgError, no SafeToRetry, no io.EOF and no
// net.Error — every branch of isTransient declined.
//
// It matters as soon as CANT-22 lands. http.Server.Shutdown does not wait for
// hijacked connections, so `defer pool.Close()` fires while socket handlers are
// still inside SendMessage: every send in flight during an ordinary deploy
// comes back not-retryable, and a client outbox reading that flag marks each
// one failed forever.
func TestAClosedPoolIsTransient(t *testing.T) {
	st := New(closedPool(t), DefaultLimits())

	_, err := st.SendMessage(context.Background(), NewMessage{
		ClientID: uuid.New(), ConversationID: uuid.New(), AuthorID: uuid.New(), Text: ptr("in flight"),
	})
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("refused with %T (%v), want a *SendError", err, err)
	}
	if se.Code != wire.ErrorCodeInternal {
		t.Errorf("code = %q, want %q", se.Code, wire.ErrorCodeInternal)
	}
	if !se.Retryable {
		t.Error("a closed pool was reported permanent — nothing was sent, so nothing can have committed, " +
			"and an ordinary deploy would park every in-flight send at failed")
	}
}
