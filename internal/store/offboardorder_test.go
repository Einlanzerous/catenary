package store

// CANT-137 criterion 4 — the lock order is what the plan says, for BOTH
// directions, and nothing deadlocks.
//
// TWO KINDS OF ORACLE, AND NEITHER IS ENOUGH ALONE. A row-level oracle cannot
// see the order of two locks inside one committed transaction, so the order
// itself is asserted against the SOURCE of both directions — CANT-134's own
// TestTheOffboardTakesTheCredentialTablesInInvalidateFamilysOrder, extended from
// three tables to all eight positions. And a source scan cannot see a cycle,
// because a cycle needs two transactions; so every pairing the new positions
// create is raced, with a server-side `lock_timeout` on every pool so that a
// cycle FAILS rather than hangs and a test that takes its whole deadline cannot
// pass by waiting.
//
// WHAT THE NEW POSITIONS ARE. Until CANT-137 both directions took
// `users → refresh_tokens → access_tokens → devices → enrollment_tokens` and
// drew nothing. They now take the person's rooms FIRST and the counter LAST:
//
//	conversations (ascending id) → conversation_members → users →
//	refresh_tokens → access_tokens → devices → enrollment_tokens → log_counter
//
// so the pairings that did not exist before are: against a send (which takes
// `conversations(X)` at position 8 and draws at 10), against a metadata bump
// (same tables, same order), and against the other direction of itself.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// The source scan

// funcBody is one function's CODE: from its signature to the next top-level
// `func `, with every `//` line removed.
//
// BOTH HALVES ARE LOAD-BEARING AND BOTH WERE FOUND BY THIS GUARD FAILING ON
// ITSELF. Without the bound, a needle in the NEXT function satisfies this one —
// the failure mode a bare strings.Index over the whole file has, and the one
// that would let this pass while the order was wrong. Without the comment
// stripping, the prose in this package matches its own rules: metadata.go's own
// note says "FOR NO KEY UPDATE, never FOR UPDATE", and a scan that reads
// comments convicts the file for describing the thing it does correctly.
func funcBody(t *testing.T, root, file, signature string) string {
	t.Helper()
	src, err := os.ReadFile(root + "/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	body := string(src)
	at := strings.Index(body, signature)
	if at < 0 {
		t.Fatalf("%s: %q is gone; this guard now watches nothing", file, signature)
	}
	body = body[at+len(signature):]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	var code []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// inOrder requires each needle to appear after the one before it, inside the
// code of one function.
func inOrder(t *testing.T, root, file, signature string, needles []struct{ what, find string }) {
	t.Helper()
	body := funcBody(t, root, file, signature)

	prev := -1
	for _, n := range needles {
		i := strings.Index(body, n.find)
		if i < 0 {
			t.Fatalf("%s in %s does not %s (looked for %q).\n"+
				"Every position of the order has to be taken, or the order describes something the code "+
				"does not do.", signature, file, n.what, n.find)
		}
		if i < prev {
			t.Errorf("%s in %s takes %s out of order.\n"+
				"The order is conversations (ascending) → conversation_members → users → refresh_tokens "+
				"→ access_tokens → devices → enrollment_tokens → log_counter LAST, for BOTH directions. "+
				"Taking `users` before `conversations` closes a cycle against a metadata bump, which "+
				"holds conversations and waits on users; drawing the counter before the rooms are locked "+
				"closes one against a send, which holds a conversation and waits on the counter.",
				signature, file, n.what)
		}
		prev = i
	}
}

func TestBothDirectionsTakeTheLocksInThePlansOrder(t *testing.T) {
	root := storeModuleRoot(t)

	// THE OFFBOARD. Positions 1-2 and 8 are calls into metadata.go; 3 is its own
	// lock query; 4-7 are inside the sweep, which the CANT-134 scan below still
	// owns table by table.
	inOrder(t, root, "internal/store/offboard.go", "func (s *Store) DeactivateUser(",
		[]struct{ what, find string }{
			{"lock the person's rooms first", "lockRoomsOfPerson(ctx, tx,"},
			{"then lock the user row", "deactivateLockQuery("},
			{"then write deactivated_at", "setDeactivatedAt(ctx, tx,"},
			{"then sweep the credential tables", "s.revokeCredentialsTx(ctx, tx,"},
			{"and draw the counter last", "bumpDeactivationMarkers(ctx, tx,"},
		})

	// THE REVERSAL, whose two ends live with EnsurePerson rather than with
	// reactivateTx — the room locks have to be above the lookup that chooses the
	// branch, and the draw has to be below the enrollment token.
	inOrder(t, root, "internal/store/persons.go", "func (s *Store) ensurePersonOnce(",
		[]struct{ what, find string }{
			{"lock the person's rooms first", "lockRoomsOfPerson(ctx, tx,"},
			{"then lock the user row", "personLookupQuery(true,"},
			{"then sweep and clear", "s.reactivateTx(ctx, tx,"},
			{"then issue the enrollment token", "issueEnrollmentTokenTx(ctx, tx,"},
			{"and draw the counter last", "bumpDeactivationMarkers(ctx, tx,"},
		})

	// POSITIONS 1 AND 2, INSIDE THE HELPER BOTH DIRECTIONS SHARE.
	// The first `FROM conversation_members` in this function is the UNLOCKED read
	// that discovers the room set, above everything; the needle is the LOCK,
	// which is why it names the select list that tells the two apart.
	inOrder(t, root, "internal/store/metadata.go", "func lockRoomsOfPerson(",
		[]struct{ what, find string }{
			{"lock conversations", "FROM conversations WHERE id = $1 FOR NO KEY UPDATE"},
			{"then conversation_members", "SELECT user_id FROM conversation_members"},
		})

	// AND THE TAIL, inside the bump itself: conversations → conversation_members
	// → users → the counter. Named here as well as in metadata.go's own tests
	// because a reader of THIS file is entitled to see the whole order in one
	// place rather than two.
	inOrder(t, root, "internal/store/metadata.go", "func (b *metadataBump) apply(",
		[]struct{ what, find string }{
			{"lock conversations", "FROM conversations WHERE id = $1 FOR NO KEY UPDATE"},
			{"then conversation_members", "SELECT user_id FROM conversation_members"},
			{"then users", "FROM users WHERE id = $1 FOR NO KEY UPDATE"},
			{"then draw the counter", "UPDATE log_counter SET value = value + 1"},
		})
}

// EVERY ROW LOCK ON BOTH PATHS IS FOR NO KEY UPDATE, NEVER FOR UPDATE.
//
// It is not a style rule and it is the half of the safety argument the ordering
// does NOT cover. A send takes KEY SHARE on `users(author_id)` and on
// `devices(sender_device_id)` at position 11, AFTER the counter — so the counter
// is genuinely before `users` for a bump and after it for a send, and no order
// removes that. FOR NO KEY UPDATE composes with KEY SHARE; FOR UPDATE does not.
func TestEveryLockOnBothDirectionsIsNeverKeyLevel(t *testing.T) {
	root := storeModuleRoot(t)
	for _, tc := range []struct{ file, fn string }{
		{"internal/store/metadata.go", "func lockRoomsOfPerson("},
		{"internal/store/metadata.go", "func (b *metadataBump) apply("},
		{"internal/store/offboard.go", "func deactivateLockQuery("},
		{"internal/store/persons.go", "func personLookupQuery("},
	} {
		// Strip the weaker spelling and see whether anything is left. `FOR NO KEY
		// UPDATE` contains `UPDATE` but not `FOR UPDATE`, so the naive check is
		// already exact — this makes it exact in a way a reader can see.
		body := funcBody(t, root, tc.file, tc.fn)
		if strings.Contains(strings.ReplaceAll(body, "FOR NO KEY UPDATE", ""), "FOR UPDATE") {
			t.Errorf("%s in %s takes a FOR UPDATE.\n"+
				"A key-level lock on a user or device row holds it against a send that is holding "+
				"log_counter and waiting for KEY SHARE on the same row at position 11 — a cycle no "+
				"ordering removes, which is why every lock on these paths is the weakest one that orders.",
				tc.fn, tc.file)
		}
	}
}

// ---------------------------------------------------------------------------
// The races
//
// Each runs raceRounds (20) rounds against a fresh person, each racer's pool
// refuses to WAIT, and a deadlock (40P01) or a lock timeout (55P03) is a real
// assertion rather than a slow test that passes.

// raceLockTimeout is long enough that ordinary contention resolves and short
// enough that a cycle surfaces well inside `go test`'s own bound. CANT-134's
// races use the same value, so a change here changes both.
const raceLockTimeout = 2 * time.Second

// isLockFailure reports a deadlock or a lock timeout, which are the two shapes a
// broken order takes. Postgres picks between them by which racer noticed first,
// so a test that checked for only one would pass half the time it should fail.
func isLockFailure(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "55P03") {
		return pgErr.Code, true
	}
	return "", false
}

// bumpRoom is the second racer for the metadata-bump cases: an ordinary
// conversation-metadata change, which is what a rename or a membership write
// does. It takes `conversations(room)` and then the counter — the same two locks
// an offboard now takes, in the same order.
func bumpRoom(ctx context.Context, pool *pgxpool.Pool, rooms ...uuid.UUID) error {
	return bumpRoomAndMaybeUser(ctx, pool, uuid.Nil, rooms...)
}

// bumpRoomAndMaybeUser is the stronger shape, and the ONE that can convict an
// inverted lock order on behaviour rather than on source.
//
// WHY THE ROOM ALONE IS NOT ENOUGH, MEASURED. With the offboard's room locks
// moved BELOW its `users` lock — the inversion the source scan catches — a bump
// naming only `conversations` still passes twenty rounds: the two transactions
// share exactly one lock and one shared lock cannot cycle. The cycle needs a bump
// that wants BOTH, which is what a membership change and what CANT-137's own
// marker bump are: conversations(C) then users(U), while the inverted offboard
// holds users(U) and waits for conversations(C). Measured against that inversion,
// this shape fails and the room-only shape does not — so the room-only case is
// kept for the ordinary contention it covers, and this one is the guard.
func bumpRoomAndMaybeUser(ctx context.Context, pool *pgxpool.Pool, user uuid.UUID, rooms ...uuid.UUID) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := newMetadataBump()
	for _, r := range rooms {
		b.conversation(r)
	}
	if user != uuid.Nil {
		b.user(user)
	}
	if _, err := b.apply(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// race runs two functions concurrently, once per round. The two goroutines start
// together on a closed channel rather than on a sleep, which is what makes the
// overlap real rather than hoped for.
//
// ANY ERROR FROM EITHER RACER IS FATAL, AND IT TOOK TWO ROUNDS OF REVIEW TO GET
// THERE (#84). Round 1 of this helper reported nothing but a deadlock and a lock
// timeout and logged everything else — so six cases whose own comments claimed
// both racers commit asserted only "no deadlock", and `go test` prints a `t.Logf`
// only under `-v`, which is the silent-green shape this repo's guards exist to
// catch. Round 3 added a `bothMustSucceed` flag with two SEND cases exempted,
// justified by "a send may be refused on its own merits — its author's own
// offboard can land first". Round 4 found that reason unavailable in either test:
// the sender is `theo`, an active member of the room in every round, and he is
// never the subject of a deprovision anywhere in the package. The case the
// exemption described is CANT-134's TestASendRacingAnOffboardNeitherDeadlocks,
// where the author IS the person being offboarded — that test does its own error
// classification and does not use this helper.
//
// Worse, the flag was per CALL rather than per RACER, so `false` exempted the
// offboard and the reversal too — the two calls this helper's own message says
// must not flake, because Purser's Deprovision sees a 500. So the flag is gone
// rather than split: every pairing here is two calls that must both commit while
// the other runs, and there is no case left to tolerate.
//
// A LOCK FAILURE IS REPORTED SEPARATELY, because it means something different: a
// 40P01 or a 55P03 says the order in messages.go's list is not the order the code
// takes, while any other error says one of these calls simply broke. Both are
// fatal; a reader of the failure should not have to work out which happened.
func race(t *testing.T, rounds int, setUp func(n int), left, right func(n int) error) {
	t.Helper()
	for n := range rounds {
		setUp(n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i, f := range []func(int) error{left, right} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = f(n)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err == nil {
				continue
			}
			if code, bad := isLockFailure(err); bad {
				t.Fatalf("round %d: racer %d failed with %s: %v.\n"+
					"A deadlock or a lock timeout between these two means the order in messages.go's "+
					"list is not the order the code takes — or that the two writers disagree about it.",
					n, i, code, err)
			}
			t.Fatalf("round %d: racer %d failed with something other than a lock: %v.\n"+
				"Both of these have to commit while the other runs. The offboard in particular is "+
				"the one call that must not flake — Purser's Deprovision sees a 500 — and a "+
				"reversal or a metadata bump aborting for any reason is the same defect wearing a "+
				"different error. A send that may legitimately be refused does not belong in this "+
				"helper; see TestASendRacingAnOffboardNeitherDeadlocks, which classifies its own.",
				n, i, err)
		}
	}
}

func TestAnOffboardRacingACoMembersSendIntoTheSharedRoom(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(poolWithLockTimeout(ctx, t, raceLockTimeout), DefaultLimits(), discardLogger())
	theo := mkUser(ctx, t, pool, "theo")

	var user uuid.UUID
	var room uuid.UUID
	// BOTH RACERS MUST COMMIT, AND theo IS WHY THAT IS THE HONEST VALUE (#84 round
	// 4). The sender is an active member of the room in every round and is never
	// the subject of a deprovision anywhere in this package, so there is no reason
	// his send can be refused — the "a send may legitimately fail" case is
	// CANT-134's TestASendRacingAnOffboardNeitherDeadlocks, where the author IS the
	// person being offboarded.
	race(t, raceRounds,
		func(n int) {
			user, _, _ = racePerson(ctx, t, st, n)
			room = mkGroup(ctx, t, pool, fmt.Sprintf("shared%d", n), user, theo)
		},
		func(n int) error { _, err := st.DeactivateUser(ctx, user); return err },
		func(n int) error {
			_, err := st.SendMessage(ctx, NewMessage{
				ConversationID: room, AuthorID: theo, ClientID: uuid.New(),
				Text: ptr("sent while somebody else was being offboarded"),
			})
			return err
		})

	// AND AFTERWARDS, INTO A ROOM WHOSE OTHER MEMBER IS ALREADY GONE. The race
	// above covers the concurrent case; this covers the settled one, which is the
	// state every room the person was in is left in. It OFFBOARDS FIRST — round 4
	// of the review found this loop asserting its message against a room whose
	// other member was never deprovioned at all, which made it a test of an
	// ordinary send.
	//
	// A co-member has nothing to do with somebody else's offboard, and their
	// message failing because of it would look like an ordinary 500 to them. Note
	// what the send has to do here: `member_count` for this room is now 1, and the
	// send path reads `conversation_members` at its membership EXISTS without
	// caring, which is the property this confirms rather than assumes.
	for n := range 3 {
		user, _, _ = racePerson(ctx, t, st, 100+n)
		room = mkGroup(ctx, t, pool, fmt.Sprintf("after%d", n), user, theo)
		if _, err := st.DeactivateUser(ctx, user); err != nil {
			t.Fatalf("offboard before the settled send: %v", err)
		}
		if _, err := st.SendMessage(ctx, NewMessage{
			ConversationID: room, AuthorID: theo, ClientID: uuid.New(), Text: ptr("still works"),
		}); err != nil {
			t.Fatalf("a send into a room whose other member was offboarded failed: %v", err)
		}
	}
}

func TestAnOffboardRacingAMetadataBumpOfTheSameRoom(t *testing.T) {
	ctx, pool := freshDB(t)
	racer := poolWithLockTimeout(ctx, t, raceLockTimeout)
	st := New(racer, DefaultLimits(), discardLogger())

	var user, room uuid.UUID
	race(t, raceRounds,
		func(n int) {
			user, _, _ = racePerson(ctx, t, st, n)
			room = mkGroup(ctx, t, pool, fmt.Sprintf("bumped%d", n), user)
		},
		func(n int) error { _, err := st.DeactivateUser(ctx, user); return err },
		func(n int) error { return bumpRoom(ctx, racer, room) })
}

// AND THE SAME AGAINST A BUMP THAT WANTS THE ROOM *AND* THE PERSON — a
// membership change's shape, and CANT-137's own marker bump's shape. See
// bumpRoomAndMaybeUser: this is the race that fails when the offboard's room
// locks drop below its `users` lock, and the one above is not.
func TestAnOffboardRacingABumpOfBothTheRoomAndThePerson(t *testing.T) {
	ctx, pool := freshDB(t)
	racer := poolWithLockTimeout(ctx, t, raceLockTimeout)
	st := New(racer, DefaultLimits(), discardLogger())

	var user, room uuid.UUID
	race(t, raceRounds,
		func(n int) {
			user, _, _ = racePerson(ctx, t, st, n)
			room = mkGroup(ctx, t, pool, fmt.Sprintf("bumped%d", n), user)
		},
		func(n int) error { _, err := st.DeactivateUser(ctx, user); return err },
		func(n int) error { return bumpRoomAndMaybeUser(ctx, racer, user, room) })
}

// THE SAME FOR THE REVERSAL, which is new to holding a conversation at all.
func TestAReactivationRacingABumpOfBothTheRoomAndThePerson(t *testing.T) {
	ctx, pool := freshDB(t)
	racer := poolWithLockTimeout(ctx, t, raceLockTimeout)
	st := New(racer, DefaultLimits(), discardLogger())

	var user, room uuid.UUID
	var email string
	race(t, raceRounds,
		func(n int) {
			user, _, email = racePerson(ctx, t, st, n)
			room = mkGroup(ctx, t, pool, fmt.Sprintf("bumped%d", n), user)
			if _, err := st.DeactivateUser(ctx, user); err != nil {
				t.Fatalf("round %d: offboard before the reversal: %v", n, err)
			}
		},
		func(n int) error { _, err := st.EnsurePerson(ctx, email, "Racer"); return err },
		func(n int) error { return bumpRoomAndMaybeUser(ctx, racer, user, room) })
}

// TWO OFFBOARDS OF TWO PEOPLE WHO SHARE TWO ROOMS. This is the case the
// ascending-id rule exists for and the only one that can fail on it alone: the
// two transactions want the same two `conversations` rows, and taking them in
// opposite orders is a cycle before the counter is ever consulted.
func TestTwoOffboardsOfTwoPeopleWhoShareTwoRooms(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(poolWithLockTimeout(ctx, t, raceLockTimeout), DefaultLimits(), discardLogger())

	var first, second uuid.UUID
	race(t, raceRounds,
		func(n int) {
			first, _, _ = racePerson(ctx, t, st, 2*n)
			second, _, _ = racePerson(ctx, t, st, 2*n+1)
			mkGroup(ctx, t, pool, fmt.Sprintf("a%d", n), first, second)
			mkGroup(ctx, t, pool, fmt.Sprintf("b%d", n), first, second)
		},
		func(n int) error { _, err := st.DeactivateUser(ctx, first); return err },
		func(n int) error { _, err := st.DeactivateUser(ctx, second); return err })
}

// THE REVERSAL IS NEW TO ALL OF THIS. CANT-134's reversal held no counter and
// locked no conversation, so none of its shipped races covers a reactivation
// against a send or a bump. Symmetry with the offboard is the argument; these
// two are the assertion.
func TestAReactivationRacingACoMembersSendIntoTheSharedRoom(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(poolWithLockTimeout(ctx, t, raceLockTimeout), DefaultLimits(), discardLogger())
	theo := mkUser(ctx, t, pool, "theo")

	var email string
	var room uuid.UUID
	// BOTH RACERS MUST COMMIT, on the same argument as the offboard's version of
	// this race: theo is an active member throughout and is never deprovisioned,
	// so his send has nothing to fail for, and a reversal that aborts for any
	// reason is a defect.
	race(t, raceRounds,
		func(n int) {
			var user uuid.UUID
			user, _, email = racePerson(ctx, t, st, n)
			room = mkGroup(ctx, t, pool, fmt.Sprintf("shared%d", n), user, theo)
			if _, err := st.DeactivateUser(ctx, user); err != nil {
				t.Fatalf("round %d: offboard before the reversal: %v", n, err)
			}
		},
		func(n int) error { _, err := st.EnsurePerson(ctx, email, "Racer"); return err },
		func(n int) error {
			_, err := st.SendMessage(ctx, NewMessage{
				ConversationID: room, AuthorID: theo, ClientID: uuid.New(),
				Text: ptr("sent while somebody else was being reinstated"),
			})
			return err
		})
}

func TestAReactivationRacingAMetadataBumpOfTheSameRoom(t *testing.T) {
	ctx, pool := freshDB(t)
	racer := poolWithLockTimeout(ctx, t, raceLockTimeout)
	st := New(racer, DefaultLimits(), discardLogger())

	var email string
	var room uuid.UUID
	race(t, raceRounds,
		func(n int) {
			var user uuid.UUID
			user, _, email = racePerson(ctx, t, st, n)
			room = mkGroup(ctx, t, pool, fmt.Sprintf("bumped%d", n), user)
			if _, err := st.DeactivateUser(ctx, user); err != nil {
				t.Fatalf("round %d: offboard before the reversal: %v", n, err)
			}
		},
		func(n int) error { _, err := st.EnsurePerson(ctx, email, "Racer"); return err },
		func(n int) error { return bumpRoom(ctx, racer, room) })
}

// ONE PERSON GOING EACH WAY, IN A ROOM THEY SHARE. The two directions take the
// same eight positions over an overlapping row set, so if either had them in a
// different order this is where it shows. Both must also SUCCEED: the outcome is
// whichever serial order the conversation row happens to impose, and neither
// racer is allowed to be the one Postgres aborts.
func TestAnOffboardRacingAReactivationOfSomebodyWhoSharesTheirRoom(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(poolWithLockTimeout(ctx, t, raceLockTimeout), DefaultLimits(), discardLogger())

	var goingOut uuid.UUID
	var comingBackEmail string
	race(t, raceRounds,
		func(n int) {
			var comingBack uuid.UUID
			goingOut, _, _ = racePerson(ctx, t, st, 2*n)
			comingBack, _, comingBackEmail = racePerson(ctx, t, st, 2*n+1)
			mkGroup(ctx, t, pool, fmt.Sprintf("shared%d", n), goingOut, comingBack)
			if _, err := st.DeactivateUser(ctx, comingBack); err != nil {
				t.Fatalf("round %d: offboard before the reversal: %v", n, err)
			}
		},
		func(n int) error { _, err := st.DeactivateUser(ctx, goingOut); return err },
		func(n int) error { _, err := st.EnsurePerson(ctx, comingBackEmail, "Racer"); return err })
}

// AND THE PEOPLE'S OWN ROOMS ARE LEFT CONSISTENT BY ALL OF IT — the property no
// deadlock check can state. A room whose count is stale after a race is a room
// whose marker was drawn but not written, or written under a lock somebody else
// held; both would be invisible to the assertions above.
func TestAfterTheRacesEveryRoomsCountMatchesItsActiveMembers(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(poolWithLockTimeout(ctx, t, raceLockTimeout), DefaultLimits(), discardLogger())

	var a, b uuid.UUID
	var rooms []uuid.UUID
	for n := range raceRounds {
		a, _, _ = racePerson(ctx, t, st, 2*n)
		b, _, _ = racePerson(ctx, t, st, 2*n+1)
		rooms = append(rooms, mkGroup(ctx, t, pool, fmt.Sprintf("r%d", n), a, b))

		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() { defer wg.Done(); <-start; _, _ = st.DeactivateUser(ctx, a) }()
		go func() { defer wg.Done(); <-start; _, _ = st.DeactivateUser(ctx, b) }()
		close(start)
		wg.Wait()
	}

	for _, room := range rooms {
		var served, actual int64
		mustScan(t, pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM conversation_members am
			         WHERE am.conversation_id = $1
			           AND EXISTS (SELECT 1 FROM users amu
			                        WHERE amu.id = am.user_id AND amu.deactivated_at IS NULL)),
			       (SELECT count(*) FROM conversation_members x WHERE x.conversation_id = $1)`, room),
			&served, &actual)
		if served != 0 {
			t.Errorf("room %s still counts %d active member(s) after both of its members were "+
				"offboarded concurrently", room, served)
		}
		if actual != 2 {
			t.Errorf("room %s holds %d membership rows, want 2 — ruling 7 keeps them, and a race must "+
				"not be what removes one", room, actual)
		}
		// And its marker moved: the second offboard's draw is above the first's,
		// so the room carries the later of the two.
		var marker int64
		mustScan(t, pool.QueryRow(ctx,
			`SELECT metadata_log_seq FROM conversations WHERE id = $1`, room), &marker)
		if marker == 0 {
			t.Errorf("room %s carries no marker after two offboards; nobody was ever told", room)
		}
	}
}
