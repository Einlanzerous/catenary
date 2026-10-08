package store

// CANT-268 — CreateGroup's own Done-when clauses, one test group per plan
// criterion of CANT-253:
//
//   - `[ruling 0 → option 0] POST /conversations with a name` — the room, its
//     three member rows, the trimmed name, and every member's /sync serving it.
//   - `...is atomic and refuses` — each refusal leaves both tables untouched.
//   - `...Creating a group moves the metadata marker` — one counter draw, no
//     message ordinal, and a caught-up cursor still sees the room.
//   - `[ruling 3 → option 0] Creating a room, by whichever path exists` — no
//     notification is raised by the create, and the first message afterwards
//     still introduces the room.
//   - `[ruling 2 → option 1] A POST /conversations replayed` — one room per
//     (creator, request id), also under ten concurrent callers.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func countTableRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+table), &n)
	return n
}

func TestCreateGroupMakesOneGroupWithEveryMemberAndEveryMemberSyncsIt(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	mal := mkUser(ctx, t, pool, "mal")

	c, created, err := st.CreateGroup(ctx, ada, "  The Room \n", []string{"theo", "mal"}, nil)
	if err != nil || !created {
		t.Fatalf("CreateGroup = %v created=%v, want a created room", err, created)
	}
	if c.Kind != "group" || c.MemberCount != 3 || c.LastSeq != 0 || c.Name == nil || *c.Name != "The Room" {
		t.Errorf("row = %+v, want a group named %q with 3 members at seq 0", c, "The Room")
	}
	var groups, members int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE kind = 'group'`), &groups)
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversation_members WHERE conversation_id = $1`, c.ID), &members)
	if groups != 1 || members != 3 {
		t.Errorf("%d group row(s) and %d member row(s), want 1 and 3", groups, members)
	}

	for who, id := range map[string]uuid.UUID{"ada": ada, "theo": theo, "mal": mal} {
		page, err := st.Sync(ctx, id, 0, DefaultSyncLimit)
		if err != nil {
			t.Fatalf("%s sync: %v", who, err)
		}
		served := false
		for _, r := range page.Conversations {
			if r.ID == c.ID && r.MemberCount == 3 {
				served = true
			}
		}
		if !served {
			t.Errorf("%s: /sync at cursor 0 did not serve the new room: %+v", who, page.Conversations)
		}
	}
}

func TestCreateGroupRefusesAtomically(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	mkUser(ctx, t, pool, "theo")
	gone := mkUser(ctx, t, pool, "gone")
	bot := mkUser(ctx, t, pool, "robot")
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'bot' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}
	many := make([]string, 50)
	for i := range many {
		many[i] = "h" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}

	for _, tc := range []struct {
		name    string
		room    string
		handles []string
		want    error
	}{
		{"unknown handle", "r", []string{"theo", "nobody"}, ErrTargetNotFound},
		{"deactivated handle", "r", []string{"theo", "gone"}, ErrTargetDeactivated},
		{"bot handle", "r", []string{"theo", "robot"}, ErrInvalidGroup},
		{"duplicate handle", "r", []string{"theo", "theo"}, ErrInvalidGroup},
		{"the creator's own handle", "r", []string{"theo", "ada"}, ErrInvalidGroup},
		{"empty trimmed name", " \t ", []string{"theo"}, ErrInvalidGroup},
		{"over-long name", string(make([]rune, 81)) + "x", []string{"theo"}, ErrInvalidGroup},
		{"no members", "r", nil, ErrInvalidGroup},
		{"too many members", "r", many, ErrInvalidGroup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			convs, mems := countTableRows(ctx, t, pool, "conversations"), countTableRows(ctx, t, pool, "conversation_members")
			counter := head(ctx, t, pool)
			_, _, err := st.CreateGroup(ctx, ada, tc.room, tc.handles, nil)
			if err == nil || !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := countTableRows(ctx, t, pool, "conversations"); got != convs {
				t.Errorf("conversations rows %d -> %d; a refusal created something", convs, got)
			}
			if got := countTableRows(ctx, t, pool, "conversation_members"); got != mems {
				t.Errorf("conversation_members rows %d -> %d; a refusal created something", mems, got)
			}
			if got := head(ctx, t, pool); got != counter {
				t.Errorf("log_counter %d -> %d; a refusal drew a marker", counter, got)
			}
		})
	}
}

func TestABotCannotCreateAGroup(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot := mkUser(ctx, t, pool, "robot")
	mkUser(ctx, t, pool, "theo")
	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'bot' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}
	counter := head(ctx, t, pool)
	if _, _, err := st.CreateGroup(ctx, bot, "x", []string{"theo"}, nil); !errors.Is(err, ErrBotCannotCreateGroup) {
		t.Fatalf("err = %v, want ErrBotCannotCreateGroup", err)
	}
	if n := countTableRows(ctx, t, pool, "conversations"); n != 0 {
		t.Errorf("%d conversations, want none", n)
	}
	if got := head(ctx, t, pool); got != counter {
		t.Errorf("log_counter %d -> %d", counter, got)
	}
}

func TestCreatingAGroupMovesTheMetadataMarkerAndDrawsNoMessageOrdinal(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	mal := mkUser(ctx, t, pool, "mal")

	// Both caught up to a head that already moved: a conversation among others.
	other := mkGroup(ctx, t, pool, "other", ada, theo, mal)
	send(ctx, t, st, other, ada, "hello")
	cursors := map[uuid.UUID]int64{}
	for _, u := range []uuid.UUID{ada, theo, mal} {
		page, err := st.Sync(ctx, u, 0, DefaultSyncLimit)
		if err != nil {
			t.Fatal(err)
		}
		cursors[u] = page.HighWater
	}

	before := head(ctx, t, pool)
	c, _, err := st.CreateGroup(ctx, ada, "late", []string{"theo", "mal"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after := head(ctx, t, pool); after != before+1 {
		t.Errorf("log_counter %d -> %d, want exactly one draw", before, after)
	}
	var lastSeq int64
	mustScan(t, pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, c.ID), &lastSeq)
	if lastSeq != 0 {
		t.Errorf("last_seq = %d, want 0: creating a room draws no message ordinal", lastSeq)
	}
	for who, u := range map[string]uuid.UUID{"ada": ada, "theo": theo, "mal": mal} {
		page, err := st.Sync(ctx, u, cursors[u], DefaultSyncLimit)
		if err != nil {
			t.Fatal(err)
		}
		served := false
		for _, r := range page.Conversations {
			served = served || r.ID == c.ID
		}
		if !served {
			t.Errorf("%s: a caught-up cursor (%d) did not see the new room", who, cursors[u])
		}
	}
}

// ruling 3 → option 0: creating a room raises no notification, which is what
// every attached session's frames hang off; and the first message afterwards
// still introduces the room (a conversation record and the user records) to a
// member who never heard of it.
func TestCreatingAGroupRaisesNoNotificationAndItsFirstMessageStillIntroducesIt(t *testing.T) {
	ctx, pool := freshDB(t)
	dsn := testDSN(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	mkUser(ctx, t, pool, "mal")

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	got := make(chan NotifyPayload, 8)
	l := &Listener[NotifyPayload]{
		DSN: dsn, Channel: NotifyChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p NotifyPayload) { got <- p },
	}
	go func() { _ = l.Run(runCtx) }()
	waitForListeners(ctx, t, pool, 1)

	c, _, err := st.CreateGroup(ctx, ada, "quiet", []string{"theo", "mal"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		t.Fatalf("creating the room raised a notification %+v; an empty room is introduced to no one", p)
	case <-time.After(1500 * time.Millisecond):
	}

	first := send(ctx, t, st, c.ID, ada, "first")
	select {
	case p := <-got:
		if p.ConversationID != c.ID || p.Seq != first.Seq {
			t.Errorf("notification %+v, want the first message of %s", p, c.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first message raised no notification")
	}
	f, err := st.MessageForFanout(ctx, c.ID, first.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Conversations[theo]; !ok || len(f.Users) != 3 {
		t.Errorf("first message carried %d conversation row(s) and %d user(s); want the room for theo and all 3 users",
			len(f.Conversations), len(f.Users))
	}
}

func TestAReplayedRequestIDReturnsTheFirstRoom(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	mkUser(ctx, t, pool, "mal")
	rid := uuid.New()

	first, created, err := st.CreateGroup(ctx, ada, "r", []string{"theo"}, &rid)
	if err != nil || !created {
		t.Fatalf("first = %v created=%v", err, created)
	}
	before := head(ctx, t, pool)
	again, created, err := st.CreateGroup(ctx, ada, "r", []string{"theo"}, &rid)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("replay = %v created=%v id %s, want the first room %s uncreated", err, created, again.ID, first.ID)
	}
	if got := head(ctx, t, pool); got != before {
		t.Errorf("a replay drew a marker: %d -> %d", before, got)
	}
	// A different body under the same key is still the first room: the key,
	// not the body, is the identity.
	other, created, err := st.CreateGroup(ctx, ada, "different", []string{"mal"}, &rid)
	if err != nil || created || other.ID != first.ID {
		t.Fatalf("replay with a different body = %v created=%v id %s", err, created, other.ID)
	}
	// Even a body that would now be refused: the key is the identity.
	invalid, created, err := st.CreateGroup(ctx, ada, "  ", nil, &rid)
	if err != nil || created || invalid.ID != first.ID {
		t.Fatalf("replay with an invalid body = %v created=%v id %s, want the first room", err, created, invalid.ID)
	}
	if n := countTableRows(ctx, t, pool, "conversations"); n != 1 {
		t.Errorf("%d conversations after replays, want 1", n)
	}

	// The same request id from a different creator is a separate room.
	sep, created, err := st.CreateGroup(ctx, theo, "r", []string{"ada"}, &rid)
	if err != nil || !created || sep.ID == first.ID {
		t.Fatalf("other creator = %v created=%v id %s, want a separate room", err, created, sep.ID)
	}
	if n := countTableRows(ctx, t, pool, "conversations"); n != 2 {
		t.Errorf("%d conversations, want 2", n)
	}
}

func TestTenConcurrentCreatesWithOneRequestIDMakeOneRoom(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	mkUser(ctx, t, pool, "theo")
	rid := uuid.New()

	var wg sync.WaitGroup
	ids := make([]uuid.UUID, 10)
	createdN := make([]bool, 10)
	errs := make([]error, 10)
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, created, err := st.CreateGroup(ctx, ada, "race", []string{"theo"}, &rid)
			ids[i], createdN[i], errs[i] = c.ID, created, err
		}()
	}
	close(start)
	wg.Wait()

	wins := 0
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("caller %d got room %s, caller 0 got %s", i, ids[i], ids[0])
		}
		if createdN[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("%d callers reported creating the room, want exactly 1", wins)
	}
	if n := countTableRows(ctx, t, pool, "conversations"); n != 1 {
		t.Errorf("%d conversations, want exactly 1", n)
	}
	if n := countTableRows(ctx, t, pool, "conversation_members"); n != 2 {
		t.Errorf("%d member rows, want 2", n)
	}
}
