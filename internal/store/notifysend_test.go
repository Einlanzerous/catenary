package store

// CANT-86 — criterion 18: a committed send delivers exactly one notification
// to a second connection holding LISTEN, and a send that rolls back delivers
// none.
//
// EVERY ASSERTION HERE IS BY ORDERING, NOT BY TIMEOUT. "Nothing arrived" is
// proved by queueing a committed send behind the case under test and checking
// that ITS payload is the next thing received — the idiom CANT-21's rollback
// test established. A timeout can only ever say "not yet"; the next arrival
// says "never".
//
// What these tests cannot see, and the read certifies: a pg_notify raised on
// the POOL after a successful commit passes all four, because when the commit
// fails nothing is raised and when it succeeds the notification arrives. The
// failure that placement admits — a commit that lands and a process that dies
// before the notify, so a message exists and never arrives — has no observer
// short of a fault-injection hook inside the transaction, which this ticket
// declined to add. That is why the line at position 12 is Mode C.
//
// NO t.Parallel IN THIS PACKAGE, and the rollback test depends on it: it
// installs a trigger on `messages` that fails every insert at commit for as
// long as it stands, which would fail any concurrent send in another test.
// freshDB already assumes the same thing — it drops and re-applies every
// migration — so this is a constraint the package holds, not a new one.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// listenForMessages starts one Listener on the message channel and returns
// the channel it delivers to, registered before the caller sends anything.
func listenForMessages(ctx context.Context, t *testing.T, pool *pgxpool.Pool) <-chan NotifyPayload {
	t.Helper()
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	got := make(chan NotifyPayload, 16)
	l := &Listener[NotifyPayload]{
		DSN: testDSN(t), Channel: NotifyChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p NotifyPayload) { got <- p },
	}
	go func() { _ = l.Run(runCtx) }()
	waitForListeners(ctx, t, pool, 1)
	return got
}

// next receives one payload or fails; the caller compares.
func next(t *testing.T, got <-chan NotifyPayload, what string) NotifyPayload {
	t.Helper()
	select {
	case p := <-got:
		return p
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no notification arrived", what)
		return NotifyPayload{}
	}
}

// A COMMITTED SEND DELIVERS EXACTLY ONE NOTIFICATION, AND IT IDENTIFIES THE
// ROW. Three sends; the listener receives three payloads in send order, each
// (conversation_id, seq) of the row that committed. A duplicate among the
// first two would put the wrong payload in the next slot, so "exactly one"
// is proved for every send but the last without a timeout.
func TestACommittedSendDeliversExactlyOneNotificationForItsRow(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room", u)
	got := listenForMessages(ctx, t, pool)

	for i, text := range []string{"one", "two", "three"} {
		sent, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: uuid.New(), Text: ptr(text)})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		p := next(t, got, "after send "+text)
		if p != (NotifyPayload{ConversationID: conv, Seq: sent.Seq}) {
			t.Fatalf("send %d (seq %d) was followed by payload %+v — a duplicate or a stray, not this send's", i, sent.Seq, p)
		}
		// The payload resolves to the row, and to no other: ids only is the
		// contract, and this is what makes ids enough.
		var id uuid.UUID
		mustScan(t, pool.QueryRow(ctx, `SELECT id FROM messages WHERE conversation_id = $1 AND seq = $2`, p.ConversationID, p.Seq), &id)
		if id != sent.ID {
			t.Errorf("payload %+v resolves to message %s, want %s", p, id, sent.ID)
		}
	}
}

// A SEND THAT ROLLS BACK DELIVERS NOTHING, and it is SendMessage's own notify
// under test rather than a hand-raised one — CANT-21 proved the Postgres
// property; this proves position 12 is subject to it.
//
// THE ROLLBACK IS FORCED BY A DEFERRED CONSTRAINT TRIGGER, not by a hook. The
// insert succeeds, pg_notify runs, and the COMMIT fails: CommitTransaction
// fires deferred triggers (AfterTriggerFireDeferred) BEFORE PreCommit_Notify
// queues anything, so the raise aborts the transaction while the notification
// is still in the backend-local pending list, and AtAbort_Notify clears it.
// Nothing is discarded from the queue because nothing ever reaches it.
//
// This is the test that catches a notify raised on the POOL before commit —
// its own autocommit transaction — which would deliver a payload for a row
// that never existed and send every instance to fetch it.
func TestARolledBackSendDeliversNoNotification(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room", u)
	got := listenForMessages(ctx, t, pool)

	// OR REPLACE and IF EXISTS throughout: freshDB drops the table, which
	// takes the trigger with it, but not a function it did not create — so a
	// crashed run must not be able to fail the next one. CASCADE on the
	// cleanup, because a t.Fatalf before the DROP TRIGGER below leaves the
	// trigger depending on the function, and a plain DROP FUNCTION would then
	// fail — silently, behind the discarded error — and do nothing.
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION cant86_abort_at_commit() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'cant86: abort at commit'; END $$`); err != nil {
		t.Fatalf("create function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), `DROP FUNCTION IF EXISTS cant86_abort_at_commit() CASCADE`)
	})
	if _, err := pool.Exec(ctx, `
		CREATE CONSTRAINT TRIGGER cant86_abort_at_commit AFTER INSERT ON messages
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cant86_abort_at_commit()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	beforeLast, beforeLog := counters(ctx, t, pool, conv)
	_, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: uuid.New(), Text: ptr("never")})
	if err == nil {
		t.Fatal("the send committed under a trigger that raises at commit")
	}
	// P0001 is the trigger's raise, reached through *SendError's Unwrap on
	// the error SendMessage returned — the failure was the commit's, not
	// something earlier in the path. The retryable flag is NOT asserted: it
	// is this test's artefact (P0001 is not a transient class) and a real
	// commit failure classifies differently.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("send failed with %v, want the deferred trigger's P0001 at commit", err)
	}
	var rows int64
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE conversation_id = $1`, conv), &rows)
	if rows != 0 {
		t.Errorf("%d rows committed under a trigger that raises at commit", rows)
	}
	if last, log := counters(ctx, t, pool, conv); last != beforeLast || log != beforeLog {
		t.Errorf("the aborted send consumed ordinals: last_seq %d→%d, log_counter %d→%d", beforeLast, last, beforeLog, log)
	}

	if _, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS cant86_abort_at_commit ON messages`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	// A committed send behind it. If this is the first thing received, the
	// aborted one delivered nothing — proved by ordering, not by waiting.
	//
	// IN A SECOND CONVERSATION, AND THAT IS NOT A DETAIL. The aborted send
	// un-drew its ordinals — CANT-14's guarantee, asserted just above — so a
	// send in the SAME conversation would draw the same seq and carry the
	// same (conversation_id, seq), which is the entire payload. A stray
	// notification from the aborted send and the real one from this send
	// would then be byte-identical, and this assertion would pass against a
	// notify raised on the pool. It did, when that probe was planted; this
	// is the fix. A different conversation gives the trailer a payload the
	// aborted send could not have produced.
	other := mkGroup(ctx, t, pool, "elsewhere", u)
	sent, err := st.SendMessage(ctx, NewMessage{ConversationID: other, AuthorID: u, ClientID: uuid.New(), Text: ptr("after")})
	if err != nil {
		t.Fatalf("send after the trigger was dropped: %v", err)
	}
	if p := next(t, got, "after the committed send"); p != (NotifyPayload{ConversationID: other, Seq: sent.Seq}) {
		t.Fatalf("received %+v first, want the committed send's {%s %d} — the rolled-back send was delivered", p, other, sent.Seq)
	}
}

// A REPLAY DELIVERS NOTHING. The message already notified once, when it
// committed; a second notification for it would make every instance re-fetch
// and re-broadcast an old message — a duplicate on every socket, which is
// Invariant 2's failure class arriving through the fanout. The replay path
// returns at position 4, before anything is drawn, and never reaches 12.
func TestAReplayDeliversNoNotification(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room", u)
	got := listenForMessages(ctx, t, pool)

	key := uuid.New()
	first, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: key, Text: ptr("once")})
	if err != nil {
		t.Fatal(err)
	}
	if p := next(t, got, "after the first send"); p != (NotifyPayload{ConversationID: conv, Seq: first.Seq}) {
		t.Fatalf("received %+v, want the first send's", p)
	}

	replay, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: key, Text: ptr("once")})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Duplicate || replay.ID != first.ID {
		t.Fatalf("the replay did not return the original: %+v", replay)
	}

	fresh, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: uuid.New(), Text: ptr("new")})
	if err != nil {
		t.Fatal(err)
	}
	if p := next(t, got, "after the fresh send"); p != (NotifyPayload{ConversationID: conv, Seq: fresh.Seq}) {
		t.Fatalf("received %+v after the replay, want the fresh send's (seq %d) — the replay notified", p, fresh.Seq)
	}
}

// THE STORE HAS TWO REPLAY PATHS, AND BOTH RAISE NOTHING. The test above
// covers the position-4 check; the race loser is the other one — it blocks on
// the unique index at 11, takes 23505 after the winner commits, rolls back,
// and returns Duplicate through sentByKey. A later edit that notified on the
// way out of send() for a Duplicate would fail THIS test and not that one.
//
// What this does NOT pin, said so it is not read as doing it: the order of
// positions 11 and 12. A loser that notified before inserting would still
// roll back, and the abort clears its pending notification — so that order
// is certified by the read, like the after-commit case in the file header.
func TestRacingSendsUnderOneKeyNotifyOnce(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	u := mkUser(ctx, t, pool, "u")
	conv := mkGroup(ctx, t, pool, "room", u)
	got := listenForMessages(ctx, t, pool)
	key := uuid.New()

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
			results[i], errs[i] = st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: key, Text: ptr("once")})
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	winner := results[0]

	trailer, err := st.SendMessage(ctx, NewMessage{ConversationID: conv, AuthorID: u, ClientID: uuid.New(), Text: ptr("after")})
	if err != nil {
		t.Fatal(err)
	}
	if p := next(t, got, "after the race"); p != (NotifyPayload{ConversationID: conv, Seq: winner.Seq}) {
		t.Fatalf("received %+v first, want the winner's (seq %d)", p, winner.Seq)
	}
	if p := next(t, got, "after the trailer"); p != (NotifyPayload{ConversationID: conv, Seq: trailer.Seq}) {
		t.Fatalf("received %+v second, want the trailer's (seq %d) — a race loser notified", p, trailer.Seq)
	}
}
