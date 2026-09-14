package store

// CANT-75 — FindOrCreateDirect's own Done-when clauses: find-or-create twice
// returns the same conversation, concurrent find-or-create for one pair
// never creates a second direct conversation, and the three refusals
// (unknown handle, a deactivated target, and the caller's own handle) all
// carry conversation_not_found — one code for three causes a caller cannot
// retry past, on the same reasoning ErrTooManyAttachments already shares
// message_too_large with ErrMessageTooLarge.

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

func TestFindOrCreateDirectTwiceReturnsTheSameConversation(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")

	first, err := st.FindOrCreateDirect(ctx, ada, "theo")
	if err != nil {
		t.Fatalf("first find-or-create: %v", err)
	}
	if first.Kind != "direct" {
		t.Errorf("kind = %q, want direct", first.Kind)
	}
	if first.MemberCount != 2 {
		t.Errorf("member_count = %d, want 2", first.MemberCount)
	}
	if first.OtherMemberName == nil || *first.OtherMemberName != "theo" {
		t.Errorf("other_member_name = %v, want theo", first.OtherMemberName)
	}

	second, err := st.FindOrCreateDirect(ctx, ada, "theo")
	if err != nil {
		t.Fatalf("second find-or-create: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second call returned %s, want the same conversation %s", second.ID, first.ID)
	}

	var count int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE kind = 'direct'`), &count)
	if count != 1 {
		t.Errorf("%d direct conversations exist, want 1", count)
	}
	var members int
	mustScan(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM conversation_members WHERE conversation_id = $1`, first.ID), &members)
	if members != 2 {
		t.Errorf("%d member rows, want exactly 2 — D4's pair, not a race that inserted a third", members)
	}

	// theo's own call for the same pair, from the OTHER side, finds the
	// identical conversation and sees the identical id.
	fromTheo, err := st.FindOrCreateDirect(ctx, theo, "ada")
	if err != nil {
		t.Fatalf("find-or-create from the other side: %v", err)
	}
	if fromTheo.ID != first.ID {
		t.Errorf("theo's call found %s, want %s", fromTheo.ID, first.ID)
	}
	if fromTheo.OtherMemberName == nil || *fromTheo.OtherMemberName != "ada" {
		t.Errorf("theo's other_member_name = %v, want ada", fromTheo.OtherMemberName)
	}
}

// The database-level property (TestConcurrentFindOrCreateDirectMakesOneConversation
// in schema_test.go) already proves the constraint holds; this proves the
// STORE METHOD built on top of it does too — every caller succeeds, every
// caller sees the same conversation, and exactly one direct conversation and
// one pair of member rows exist afterward.
func TestConcurrentFindOrCreateDirectViaTheStoreMakesOneConversation(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	_ = mkUser(ctx, t, pool, "theo")

	const n = 10
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			c, err := st.FindOrCreateDirect(ctx, ada, "theo")
			ids[i] = c.ID
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	first := ids[0]
	for i, id := range ids {
		if id != first {
			t.Errorf("caller %d found %s, want %s", i, id, first)
		}
	}

	var convCount, memberCount int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE kind = 'direct'`), &convCount)
	mustScan(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM conversation_members WHERE conversation_id = $1`, first), &memberCount)
	if convCount != 1 {
		t.Errorf("%d concurrent calls made %d direct conversations, want 1", n, convCount)
	}
	if memberCount != 2 {
		t.Errorf("%d member rows for the winning conversation, want 2", memberCount)
	}
}

// A conversation created by find-or-create is discoverable on the FIRST
// /sync page either member issues — the metadata bump's whole job, and the
// same claim TestAnEmptyConversationIsDiscoverableOnlyIfItsCreationDrew makes
// about a raw insert, now made about this call.
func TestAFoundOrCreatedDirectIsDiscoverableOnSync(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")

	c, err := st.FindOrCreateDirect(ctx, ada, "theo")
	if err != nil {
		t.Fatalf("find-or-create: %v", err)
	}

	for _, who := range []uuid.UUID{ada, theo} {
		page, err := st.Sync(ctx, who, 0, 0)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		found := false
		for _, cr := range page.Conversations {
			if cr.ID == c.ID {
				found = true
			}
		}
		if !found {
			t.Errorf("viewer %s does not see the new direct conversation on its first /sync page", who)
		}
	}
}

func TestFindOrCreateDirectRefusesAnUnknownHandle(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")

	_, err := st.FindOrCreateDirect(ctx, ada, "nobody-by-this-handle")
	assertConversationNotFound(t, err, "an unknown handle")
}

func TestFindOrCreateDirectRefusesADeactivatedTarget(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, theo); err != nil {
		t.Fatal(err)
	}

	_, err := st.FindOrCreateDirect(ctx, ada, "theo")
	assertConversationNotFound(t, err, "a deactivated target")

	var count int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE kind = 'direct'`), &count)
	if count != 0 {
		t.Errorf("a refused find-or-create created %d conversations, want 0", count)
	}
}

func TestFindOrCreateDirectRefusesTheCallersOwnHandle(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")

	_, err := st.FindOrCreateDirect(ctx, ada, "ada")
	assertConversationNotFound(t, err, "the caller's own handle")

	var count int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversations`), &count)
	if count != 0 {
		t.Errorf("a self-direct refusal created %d conversations, want 0", count)
	}
}

func assertConversationNotFound(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", reason)
	}
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("%s: %v is not a *SendError", reason, err)
	}
	if se.Code != wire.ErrorCodeConversationNotFound {
		t.Errorf("%s: code = %q, want conversation_not_found", reason, se.Code)
	}
	if se.HTTPStatus() != 404 {
		t.Errorf("%s: http status = %d, want 404", reason, se.HTTPStatus())
	}
	if se.Retryable {
		t.Errorf("%s: retryable = true, want false — retrying the identical request changes nothing", reason)
	}
}
