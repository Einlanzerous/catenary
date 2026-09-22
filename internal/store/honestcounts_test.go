package store

// CANT-137 criteria 1, 3 and 5 of CANT-135's plan (rev 2) — the counts are
// honest, the change arrives without anybody speaking, and one file owns the
// write.
//
// Criterion 4, the lock order and its races, is offboardorder_test.go: it needs
// a second pool per racer and a rig of its own. Criterion 6, invariant 1 with
// both directions interleaved among sends, is logorder_test.go, beside the
// property it extends.
//
// WHY A ROOM OF FOUR AND NOT A ROOM OF TWO. A two-member room cannot tell
// "member_count dropped by one" from "member_count is now the viewer alone", and
// it cannot hold a message that some members have read and others have not — so
// it cannot show the numerator and the denominator moving TOGETHER, which is the
// whole of ruling 1. Four is the smallest room in which a reader, a non-reader,
// an author and the person being offboarded are four different people.

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Criterion 1 — both counts, one predicate, together

// roomOfFour is the fixture: an offboardable person and three others in one
// room, with a message everybody but one has read.
type roomOfFour struct {
	Ada     offboarded // the one who will be deactivated
	Theo    uuid.UUID  // has read everything
	Nadia   uuid.UUID  // has read nothing
	Marek   uuid.UUID  // has read everything
	Conv    uuid.UUID
	FromAda Sent
}

func newRoomOfFour(ctx context.Context, t *testing.T, st *Store, pool *pgxpool.Pool) roomOfFour {
	t.Helper()
	r := roomOfFour{
		Ada:   newOffboarded(ctx, t, st),
		Theo:  mkUser(ctx, t, pool, "theo"),
		Nadia: mkUser(ctx, t, pool, "nadia"),
		Marek: mkUser(ctx, t, pool, "marek"),
	}
	r.Conv = mkGroup(ctx, t, pool, "room of four", r.Ada.UserID, r.Theo, r.Nadia, r.Marek)

	r.FromAda = send(ctx, t, st, r.Conv, r.Ada.UserID, "from the one being offboarded")
	// Theo and Marek have read it; Nadia has not. Ada has read it by identity,
	// because she wrote it.
	for _, u := range []uuid.UUID{r.Theo, r.Marek} {
		if _, err := st.MarkRead(ctx, r.Conv, u, r.FromAda.Seq); err != nil {
			t.Fatalf("mark read: %v", err)
		}
	}
	return r
}

// convAndReadBy serves the page one viewer sees and returns the two numbers this
// criterion is about, THROUGH Sync rather than by re-running the expressions —
// the same reason CANT-134's own view test goes through the path that computes
// them.
func convAndReadBy(ctx context.Context, t *testing.T, st *Store, viewer, conv, msg uuid.UUID) (int64, int64, *int64) {
	t.Helper()
	page, err := st.Sync(ctx, viewer, 0, 100)
	if err != nil {
		t.Fatalf("sync as %s: %v", viewer, err)
	}
	for _, c := range page.Conversations {
		if c.ID == conv {
			return c.MemberCount, page.ReadBy[msg], c.FirstUnreadSeq
		}
	}
	t.Fatalf("conversation %s is not on %s's page", conv, viewer)
	return 0, 0, nil
}

func TestARoomOfFourCountsOnlyItsActiveMembers(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	r := newRoomOfFour(ctx, t, st, pool)

	// A message NOBODY has read, written by somebody who stays. Its read_by is
	// its author alone, and it is the control for "the numerator moved because
	// the predicate moved" rather than "the numerator moved at all".
	fromTheo := send(ctx, t, st, r.Conv, r.Theo, "unread by everyone else")
	// And one from Ada that nobody else has read, which is the only way to reach
	// `read_by: 0` — its author is the whole of the count until they stop being
	// a member. EVERY MESSAGE THIS TEST NEEDS IS SENT BEFORE THE BASELINE, so
	// first_unread_seq can be compared across the offboard and across the
	// reversal against one snapshot rather than a moving one.
	orphan := send(ctx, t, st, r.Conv, r.Ada.UserID, "written before the offboard, read by nobody")

	// BEFORE. Four members; Ada's message read by Ada (author), Theo and Marek.
	for _, viewer := range []uuid.UUID{r.Theo, r.Nadia, r.Marek} {
		count, readBy, _ := convAndReadBy(ctx, t, st, viewer, r.Conv, r.FromAda.ID)
		if count != 4 || readBy != 3 {
			t.Fatalf("before the offboard, %s sees member_count=%d read_by=%d; want 4 and 3 — "+
				"the fixture is not what the rest of this test assumes", viewer, count, readBy)
		}
	}
	unreadBefore := map[uuid.UUID]*int64{}
	for _, viewer := range []uuid.UUID{r.Theo, r.Nadia, r.Marek} {
		_, _, fu := convAndReadBy(ctx, t, st, viewer, r.Conv, r.FromAda.ID)
		unreadBefore[viewer] = fu
	}

	if _, err := st.DeactivateUser(ctx, r.Ada.UserID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// AFTER. Three members, and BOTH HALVES moved by the same one.
	for _, viewer := range []uuid.UUID{r.Theo, r.Nadia, r.Marek} {
		count, readBy, fu := convAndReadBy(ctx, t, st, viewer, r.Conv, r.FromAda.ID)
		if count != 3 {
			t.Errorf("%s sees member_count=%d after the offboard, want 3 — the header must not claim "+
				"somebody who can no longer read anything", viewer, count)
		}
		if readBy != 2 {
			t.Errorf("%s sees read_by=%d for a message the deactivated person AUTHORED, want 2.\n"+
				"The author counted themselves by identity while they were a member; they are not one "+
				"now, so the numerator drops with the denominator and 3/3 stays reachable.", viewer, readBy)
		}
		// AND THE FRACTION IS STILL REACHABLE, which is the property the two
		// halves exist to keep: Nadia reads it and every active member has.
		if readBy > count {
			t.Errorf("%s sees read_by=%d over member_count=%d — a numerator above its denominator",
				viewer, readBy, count)
		}
		// first_unread_seq is unchanged for every one of them.
		switch b := unreadBefore[viewer]; {
		case (b == nil) != (fu == nil):
			t.Errorf("%s's first_unread_seq presence changed across the offboard: %v -> %v", viewer, b, fu)
		case b != nil && *b != *fu:
			t.Errorf("%s's first_unread_seq moved %d -> %d across an offboard", viewer, *b, *fu)
		}
	}

	// THE CONTROL THAT DID NOT MOVE. Theo's message is read by Theo alone, by
	// identity, and Theo was not offboarded — so it holds at 1, which is what
	// says the change above came from the predicate rather than from the count
	// breaking.
	if _, readBy, _ := convAndReadBy(ctx, t, st, r.Theo, r.Conv, fromTheo.ID); readBy != 1 {
		t.Errorf("read_by for a message only its (active) author has read is %d, want 1", readBy)
	}

	// AND ALL THE WAY DOWN TO ZERO, which is the third cause the schema now
	// names: the author is deactivated and no active member's receipt has passed
	// the message.
	if _, readBy, _ := convAndReadBy(ctx, t, st, r.Theo, r.Conv, orphan.ID); readBy != 0 {
		t.Errorf("read_by for a DEACTIVATED author's unread message is %d, want 0 — the third cause of "+
			"zero the schema now names", readBy)
	}

	// REACTIVATION RESTORES BOTH, with the receipts that were already there.
	if _, err := st.EnsurePerson(ctx, r.Ada.Email, "Ada Lovelace"); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	for _, viewer := range []uuid.UUID{r.Theo, r.Nadia, r.Marek} {
		count, readBy, fu := convAndReadBy(ctx, t, st, viewer, r.Conv, r.FromAda.ID)
		if count != 4 || readBy != 3 {
			t.Errorf("%s sees member_count=%d read_by=%d after a reactivation, want 4 and 3 — "+
				"the person is frozen in place, so their old read_seq is still there and both halves "+
				"return to exactly where they were", viewer, count, readBy)
		}
		switch b := unreadBefore[viewer]; {
		case (b == nil) != (fu == nil):
			t.Errorf("%s's first_unread_seq presence changed across the reversal", viewer)
		case b != nil && *b != *fu:
			t.Errorf("%s's first_unread_seq is %d after the reversal, want %d", viewer, *fu, *b)
		}
	}
}

// A BOT STILL COUNTS. `kind` is no part of the predicate: a bot is a member that
// can read, and the only thing that decides is whether the account can reach the
// room. CANT-76's `User.kind` is the enum-shaped question and is not this one.
func TestABotCountsAsAMember(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room with a bot", theo, bot)
	m := send(ctx, t, st, conv, theo, "morning")

	count, readBy, _ := convAndReadBy(ctx, t, st, theo, conv, m.ID)
	if count != 2 {
		t.Errorf("member_count=%d in a room of a person and a bot, want 2 — a bot is a member that can "+
			"read, and the active-member predicate tests deactivation, not kind", count)
	}
	if readBy != 1 {
		t.Errorf("read_by=%d, want 1 (the author); the bot has read nothing yet", readBy)
	}
	// And a receipt from the bot reaches the denominator it belongs to.
	if _, err := st.MarkRead(ctx, conv, bot, m.Seq); err != nil {
		t.Fatalf("bot marks read: %v", err)
	}
	if _, readBy, _ := convAndReadBy(ctx, t, st, theo, conv, m.ID); readBy != 2 {
		t.Errorf("read_by=%d after the bot's receipt, want 2 — 2/2 has to be reachable", readBy)
	}
}

// member_count IS NEVER BELOW 1 ON A SERVED PAGE, which is the wire's own
// `minimum: 1`. A page is served only to a member who is active, so the reader
// is always in their own count — even in a direct conversation whose other half
// has been deprovisioned, which is the narrowest case there is.
func TestAServedMemberCountIsNeverBelowOne(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	theo := mkUser(ctx, t, pool, "theo")
	pair := mkGroup(ctx, t, pool, "the two of them", f.UserID, theo)
	alone := mkGroup(ctx, t, pool, "theo by himself", theo)
	send(ctx, t, st, pair, theo, "anyone there")
	// A message in the one-member room too: mkGroup leaves metadata_log_seq at
	// the DEFAULT 0, which is below every cursor there is, so a room with no
	// traffic at all reaches no page (TestAConversationLeftAtTheDefaultIsInvisible).
	send(ctx, t, st, alone, theo, "note to self")

	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	page, err := st.Sync(ctx, theo, 0, 100)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	seen := 0
	for _, c := range page.Conversations {
		seen++
		if c.MemberCount < 1 {
			t.Errorf("conversation %s served member_count=%d to a member of it; the wire says minimum 1, "+
				"and a page reaches nobody who is not in its own count", c.ID, c.MemberCount)
		}
	}
	if seen < 2 {
		t.Fatalf("theo's page carries %d conversations, want both %s and %s", seen, pair, alone)
	}
}

// THE PREDICATE IS DEFINED ONCE AND USED AT EVERY COUNTING SITE, asserted
// against the source of the three of them.
//
// A ROW-LEVEL ORACLE CANNOT SEE THIS. Three hand-written counts that agree today
// pass every behavioural test in this file; what they cannot do is stay in
// agreement, and the numerator drifting from the denominator is the READ 6/7 bug
// `Message.read_by`'s description is mostly about. So this reads the source and
// requires every `count(*) FROM conversation_members` in the package to carry
// the predicate — a source scan on tokens_guard_test.go's own precedent, for the
// same reason: the population is a string in SQL, so only its shape can be
// watched.
func TestEveryMemberCountUsesTheActivePredicate(t *testing.T) {
	root := storeModuleRoot(t)
	// The identifier, not the SQL: the const is spliced into each statement with
	// Go's `+`, so the predicate's text is never inside the string literal that
	// contains the count. Scanning raw source is what sees the splice.
	const predicate = "activeMemberExpr"
	counting := regexp.MustCompile(`count\(\*\)\s*FROM\s+conversation_members`)

	files, err := os.ReadDir(root + "/internal/store")
	if err != nil {
		t.Fatalf("read internal/store: %v", err)
	}
	found := 0
	for _, e := range files {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(root + "/internal/store/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		body := string(src)
		for _, loc := range counting.FindAllStringIndex(body, -1) {
			found++
			span := balancedFrom(body, loc[0])
			if !strings.Contains(span, predicate) {
				t.Errorf("%s counts conversation_members without %s:\n\n%s\n\n"+
					"Every counting site has to apply the SAME active-member test, or the numerator and "+
					"the denominator of READ n/m stop being able to reach each other — which is the bug "+
					"readstate.go's header is about, and the one a passing behavioural test cannot see. "+
					"Use memberCountExpr, or splice activeMemberExpr into your own count and give its "+
					"conversation_members scan the alias `am`.", e.Name(), predicate, span)
			}
		}
	}
	// The guard has to be watching something. TWO written counts today, and that
	// is the point rather than an accident: `readByExpr` is one and
	// `memberCountExpr` is the other, and sync.go's two `member_count` columns
	// are the SAME const spliced twice. A third written count appearing here is
	// not a failure in itself — the loop above is what decides — but zero or one
	// means the shapes moved and this scan is looking at nothing.
	if found < 2 {
		t.Errorf("the scan found %d counting site(s) over conversation_members, want at least 2 — "+
			"either they were rewritten into a shape this regexp cannot see, in which case this guard "+
			"now watches nothing, or one of them is gone", found)
	}

	// AND THE TWO member_count COLUMNS REALLY ARE THAT CONST. The loop above
	// cannot see a site that does not exist: somebody replacing memberCountExpr
	// with an inline `count(*)` would be caught, but somebody serving the column
	// from a different expression altogether — a stored count, a join — would
	// not. sync.go has exactly two places that select it, loadConversations and
	// conversationRowPerViewer, and both have to name the const.
	sync, err := os.ReadFile(root + "/internal/store/sync.go")
	if err != nil {
		t.Fatalf("read sync.go: %v", err)
	}
	spliced := 0
	for _, line := range strings.Split(string(sync), "\n") {
		// Comments name the const too, and a comment is not a query. The guard
		// counts the places it is actually concatenated into SQL.
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, "memberCountExpr") {
			spliced++
		}
	}
	if spliced != 2 {
		t.Errorf("sync.go splices memberCountExpr into %d quer(ies), want 2 — loadConversations' page "+
			"query and conversationRowPerViewer, which is what the hub's fan-out and CANT-75's "+
			"find-or-create both read. A member_count served from anything else is a second answer to "+
			"the question readstate.go exists to have one answer to.", spliced)
	}
}

// balancedFrom returns the text of the parenthesised expression that encloses
// the match at `at`, walking back to the opening paren and forward to the one
// that closes it. Text rather than SQL parsing, because the span deliberately
// includes the Go `+ activeMemberExpr +` splice that makes the predicate one
// definition rather than three.
func balancedFrom(body string, at int) string {
	start := strings.LastIndex(body[:at], "(")
	if start < 0 {
		start = at
	}
	depth := 0
	for i := start; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return body[start : i+1]
			}
		}
	}
	return body[start:]
}

// ---------------------------------------------------------------------------
// Criterion 3 — it arrives, without anybody speaking

// syncFrom serves one page from a cursor and reports what it carries about a
// conversation and a user, which is what "it arrived" means: a co-member's NEXT
// /sync, from the cursor they already hold, returns the room and the record.
func syncFrom(ctx context.Context, t *testing.T, st *Store, viewer uuid.UUID, after int64) SyncPage {
	t.Helper()
	page, err := st.Sync(ctx, viewer, after, 100)
	if err != nil {
		t.Fatalf("sync as %s from %d: %v", viewer, after, err)
	}
	return page
}

func hasConversation(page SyncPage, id uuid.UUID) *ConversationRow {
	for i := range page.Conversations {
		if page.Conversations[i].ID == id {
			return &page.Conversations[i]
		}
	}
	return nil
}

func hasUser(page SyncPage, id uuid.UUID) *UserRow {
	for i := range page.Users {
		if page.Users[i].ID == id {
			return &page.Users[i]
		}
	}
	return nil
}

func TestAnOffboardAndItsReversalArriveWithNobodySpeaking(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	theo := mkUser(ctx, t, pool, "theo")
	nadia := mkUser(ctx, t, pool, "nadia")
	// TWO shared rooms, so "every room they share" is a claim with a plural in
	// it, and a stranger who shares none.
	first := mkGroup(ctx, t, pool, "first", f.UserID, theo)
	second := mkGroup(ctx, t, pool, "second", f.UserID, theo)
	elsewhere := mkGroup(ctx, t, pool, "elsewhere", nadia)
	send(ctx, t, st, first, theo, "a message so the room has a head")
	send(ctx, t, st, elsewhere, nadia, "nothing to do with any of this")

	// Both readers catch up completely, so anything on the next page arrived
	// because of the offboard and not because it was already owed to them.
	theoCursor := syncFrom(ctx, t, st, theo, 0).HighWater
	nadiaCursor := syncFrom(ctx, t, st, nadia, 0).HighWater
	if n := len(syncFrom(ctx, t, st, theo, theoCursor).Conversations); n != 0 {
		t.Fatalf("theo is not caught up: %d conversations still owed", n)
	}

	// THE OFFBOARD. Nobody sends anything.
	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	page := syncFrom(ctx, t, st, theo, theoCursor)
	if len(page.Messages) != 0 {
		t.Errorf("the page carries %d messages; nobody sent one", len(page.Messages))
	}
	for _, id := range []uuid.UUID{first, second} {
		c := hasConversation(page, id)
		if c == nil {
			t.Errorf("room %s is not on theo's next page.\n"+
				"Every room the person is in has to ride the offboard's own marker, or a quiet room "+
				"serves the old `N MEMBERS` until something else happens to touch it — which is the "+
				"failure CANT-135 ruling 3 exists to remove.", id)
			continue
		}
		if c.MemberCount != 1 {
			t.Errorf("room %s arrives with member_count=%d, want 1", id, c.MemberCount)
		}
	}
	u := hasUser(page, f.UserID)
	if u == nil {
		t.Fatal("the deactivated person's User record is not on theo's next page; without it no client " +
			"can tell WHICH member is the one who left, and CANT-135 ruling 2's field never ships")
	}
	if !u.Deactivated {
		t.Error("the record arrived with Deactivated false — wireview.User emits the wire field only " +
			"when this is true, so a false here is an absent field and the client learns nothing")
	}

	// THE NON-CO-MEMBER GETS NEITHER. loadUsers' reachability EXISTS is what
	// bounds it: a deactivation reaches the people who could see the count
	// change, and telling anybody else would be a widening of what /sync reveals
	// decided in a query clause.
	stranger := syncFrom(ctx, t, st, nadia, nadiaCursor)
	for _, id := range []uuid.UUID{first, second} {
		if hasConversation(stranger, id) != nil {
			t.Errorf("nadia was served room %s, which she is not a member of", id)
		}
	}
	if hasUser(stranger, f.UserID) != nil {
		t.Error("nadia was served the deactivated person's User record; she shares no room with them, " +
			"so this would tell her that somebody she has never heard of was deprovisioned, and when")
	}
	if hasConversation(stranger, elsewhere) != nil {
		t.Error("nadia's own room was re-served by somebody else's offboard")
	}

	// THE REVERSAL, on the same terms.
	theoCursor = page.HighWater
	if _, err := st.EnsurePerson(ctx, f.Email, "Ada Lovelace"); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	back := syncFrom(ctx, t, st, theo, theoCursor)
	if len(back.Messages) != 0 {
		t.Errorf("the reversal's page carries %d messages; nobody sent one", len(back.Messages))
	}
	for _, id := range []uuid.UUID{first, second} {
		c := hasConversation(back, id)
		if c == nil {
			t.Errorf("room %s is not on theo's page after the reversal — a reactivation that reaches "+
				"nobody leaves every client showing a room one member short, indefinitely", id)
			continue
		}
		if c.MemberCount != 2 {
			t.Errorf("room %s arrives with member_count=%d after the reversal, want 2", id, c.MemberCount)
		}
	}
	switch u := hasUser(back, f.UserID); {
	case u == nil:
		t.Error("the reactivated person's User record is not on theo's page; the client is still " +
			"holding one that says deactivated")
	case u.Deactivated:
		t.Error("the record arrived still saying deactivated after a reversal")
	}
}

// A REACTIVATION DRAWS EXACTLY ONE MARKER TOO, and an ordinary re-invite of an
// ACTIVE person draws none — which is the gate, and the thing that keeps every
// Purser bundle re-run out of the deployment-wide serialised section.
func TestAReactivationDrawsOneMarkerAndAPlainReInviteDrawsNone(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	mkGroup(ctx, t, pool, "one", f.UserID)
	mkGroup(ctx, t, pool, "two", f.UserID)

	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("offboard: %v", err)
	}

	before := head(ctx, t, pool)
	back, err := st.EnsurePerson(ctx, f.Email, "Ada Lovelace")
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if back.Outcome != PersonReactivated {
		t.Fatalf("outcome = %q, want reactivated", back.Outcome)
	}
	after := head(ctx, t, pool)
	if after != before+1 {
		t.Errorf("a reactivation of somebody in two rooms moved log_counter %d -> %d; want exactly one "+
			"draw", before, after)
	}

	// The re-invite: same call, active person, nothing to announce.
	if _, err := st.EnsurePerson(ctx, f.Email, "Ada Lovelace"); err != nil {
		t.Fatalf("re-invite: %v", err)
	}
	if again := head(ctx, t, pool); again != after {
		t.Errorf("an ordinary re-invite moved log_counter %d -> %d.\n"+
			"EnsurePerson is what Purser calls on every bundle run; a draw here would put the whole "+
			"estate's provisioning into the serialised section to record that nothing changed.", after, again)
	}
}

// ---------------------------------------------------------------------------
// Criterion 5 — the guard owns the write
//
// TestOnlyOneFileMovesAMetadataMarker and TestTheMetadataGuardBites are in
// metadata_guard_test.go and now carry `users.deactivated_at`. What is here is
// the other half of criterion 5: the write really is in metadata.go and really
// is nowhere else, and the guard really does bite when a copy is planted
// somewhere it should not be.

// BOTH DIRECTIONS ARE IN metadata.go, AND NEITHER IS ANYWHERE ELSE.
//
// It runs the SAME scanner the guard runs — offencesIn, over parsed Go so a
// statement in a string literal is told from the same words in a comment — in
// the two directions the guard itself cannot cover on its own.
// TestOnlyOneFileMovesAMetadataMarker skips metadata.go by design, so nothing
// else in the tree asserts that the write actually landed there rather than
// simply vanishing; and it reports every watched column at once, so nothing
// else says that `deactivated_at` in particular is clean.
func TestBothDeactivatedAtWritesAreInMetadataGoAndNowhereElse(t *testing.T) {
	root := storeModuleRoot(t)

	src, err := os.ReadFile(root + "/" + metadataFile)
	if err != nil {
		t.Fatalf("read %s: %v", metadataFile, err)
	}
	inMetadata := 0
	for _, o := range offencesInFile(t, metadataFile, src) {
		if strings.Contains(o, "deactivated_at") {
			inMetadata++
		}
	}
	if inMetadata != 2 {
		t.Errorf("%s holds %d statement(s) writing users.deactivated_at, want 2 — setDeactivatedAt and "+
			"clearDeactivatedAt. Both directions belong in one file, or the one that drifts is the "+
			"reversal, which is the path a reader exercises least and the one where a missed marker "+
			"means every client keeps showing somebody as deactivated after they are back.",
			metadataFile, inMetadata)
	}

	var elsewhere []string
	for _, o := range scanForMetadataWrites(t, moduleRoot(t)) {
		if strings.Contains(o, "deactivated_at") {
			elsewhere = append(elsewhere, o)
		}
	}
	if len(elsewhere) > 0 {
		t.Errorf("users.deactivated_at is written outside %s:\n\n%s\n\n"+
			"Since CANT-137 that column is watched: changing it changes what /sync serves for every room "+
			"the person is in, so the write and the marker have to be in one transaction and one file.",
			metadataFile, strings.Join(elsewhere, "\n"))
	}
}

// THE GUARD, WATCHED FAILING. A copy of CANT-134's original statement, fed to the
// scanner exactly as it would be read out of offboard.go. Without this the
// column's entry in metadataColumns could be a word somebody typed rather than a
// guard, which is the shape CANT-73's closing notes are about.
func TestTheMetadataGuardBitesOnADeactivatedAtWritePlantedInOffboardGo(t *testing.T) {
	planted := []byte(`package store

func plantedOffboard() string {
	return ` + "`" + `
		UPDATE users SET deactivated_at = now()
		 WHERE id = $1 AND deactivated_at IS NULL` + "`" + `
}
`)
	got := offencesInFile(t, "internal/store/offboard.go", planted)
	if len(got) != 1 {
		t.Fatalf("the guard reported %d offence(s) for an offboard's own write planted back in "+
			"offboard.go, want 1: %v.\n"+
			"Without this, `deactivated_at` in metadataColumns is a word somebody typed and not a rule "+
			"anything enforces — and the write could drift back out of metadata.go with the marker left "+
			"behind.", len(got), got)
	}
	if !strings.Contains(got[0], "deactivated_at") {
		t.Errorf("the offence is %q and does not name the column", got[0])
	}

	// And the reversal's statement, which is the one that would be forgotten.
	plantedClear := []byte(`package store

func plantedReversal() string {
	return ` + "`" + `
		UPDATE users SET deactivated_at = NULL
		 WHERE id = $1 AND deactivated_at IS NOT NULL` + "`" + `
}
`)
	if got := offencesInFile(t, "internal/store/offboard.go", plantedClear); len(got) != 1 {
		t.Errorf("the guard reported %d offence(s) for the REVERSAL's write, want 1: %v", len(got), got)
	}
}
