package store

// CANT-134 criteria 3, 6 (for the functions this row adds) and 7 — the
// offboard: one transaction, convergent on retry, out of reach of a bot, and
// leaving membership exactly where it found it.
//
// Criterion 5, the reversal, is reactivate_test.go. Criterion 4, severance
// through the real hub and the real router, is cmd/catenary/offboard_test.go —
// it needs a listener and a socket, which this package has neither of.
//
// THE NEGATIVE CONTROLS (TestWithout…) are proposal_test.go's and
// persons_test.go's shape: a fault flipped on, and a test that is EXPECTED to
// show the bad behaviour, so each guard's necessity is demonstrated rather than
// asserted. CANT-73's closing notes are why — two guards that cover for each
// other pass every test that removes only one.

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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Fixtures

// offboarded is the state an offboard has to converge from: one person with an
// email, TWO enrolled devices, and an unredeemed invitation on top. Two
// devices rather than one because "everywhere, at once" is the claim, and a
// one-device fixture would pass against a sweep scoped to a single device.
type offboarded struct {
	UserID uuid.UUID
	Email  string
	Phone  Enrollment
	Laptop Enrollment

	// Pending is an invitation that was issued and never redeemed — the row an
	// offboard has to supersede, or a deprovisioned person's mailbox still
	// holds a working way back in.
	Pending IssuedToken
}

func newOffboarded(ctx context.Context, t *testing.T, st *Store) offboarded {
	t.Helper()
	const email = "ada@example.com"

	first, err := st.EnsurePerson(ctx, email, "Ada Lovelace")
	if err != nil {
		t.Fatalf("fixture: ensure person: %v", err)
	}
	phone, err := st.RedeemEnrollment(ctx, first.Token.Plaintext, "Pixel 8 Pro")
	if err != nil {
		t.Fatalf("fixture: redeem phone: %v", err)
	}

	second, err := st.EnsurePerson(ctx, email, "Ada Lovelace")
	if err != nil {
		t.Fatalf("fixture: re-invite: %v", err)
	}
	laptop, err := st.RedeemEnrollment(ctx, second.Token.Plaintext, "Framework 13")
	if err != nil {
		t.Fatalf("fixture: redeem laptop: %v", err)
	}

	pending, err := st.IssueEnrollmentToken(ctx, first.Account.UserID)
	if err != nil {
		t.Fatalf("fixture: pending invitation: %v", err)
	}

	return offboarded{UserID: first.Account.UserID, Email: email, Phone: phone, Laptop: laptop, Pending: pending}
}

// accountState is what an account looks like after the fact, across every
// table an offboard writes.
type accountState struct {
	deactivated             bool
	devices, devicesRevoked int
	refresh, refreshRevoked int
	access, accessRevoked   int
	liveInvitations         int
}

func readAccountState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) accountState {
	t.Helper()
	var st accountState
	mustScan(t, pool.QueryRow(ctx, `
		SELECT (SELECT deactivated_at IS NOT NULL FROM users WHERE id = $1),
		       (SELECT count(*) FROM devices WHERE user_id = $1),
		       (SELECT count(*) FROM devices WHERE user_id = $1 AND revoked_at IS NOT NULL),
		       (SELECT count(*) FROM refresh_tokens r JOIN devices d ON d.id = r.device_id WHERE d.user_id = $1),
		       (SELECT count(*) FROM refresh_tokens r JOIN devices d ON d.id = r.device_id
		         WHERE d.user_id = $1 AND r.revoked_at IS NOT NULL),
		       (SELECT count(*) FROM access_tokens WHERE user_id = $1),
		       (SELECT count(*) FROM access_tokens WHERE user_id = $1 AND revoked_at IS NOT NULL),
		       (SELECT count(*) FROM enrollment_tokens
		         WHERE user_id = $1 AND redeemed_at IS NULL AND superseded_at IS NULL)`, userID),
		&st.deactivated, &st.devices, &st.devicesRevoked, &st.refresh, &st.refreshRevoked,
		&st.access, &st.accessRevoked, &st.liveInvitations)
	return st
}

// assertFullyRevoked is the whole of "everywhere, at once", asserted over rows
// rather than over what the call returned.
//
// FIVE INDEPENDENT CHECKS AND NOT A switch — found in review (#81). This is the
// assertion five tests and both race loops share, so it is the one place where
// seeing the whole shape of a failure at once is worth the most: a run of cases
// reports only the first, and "deactivated_at is not set" would hide that four
// other things were live too.
func assertFullyRevoked(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, what string) {
	t.Helper()
	got := readAccountState(ctx, t, pool, userID)
	if !got.deactivated {
		t.Errorf("%s: users.deactivated_at is not set", what)
	}
	if got.devicesRevoked != got.devices {
		t.Errorf("%s: %d of %d devices revoked, want all — Authenticate, DeadDevices and CANT-30's "+
			"gap re-check all read this column", what, got.devicesRevoked, got.devices)
	}
	if got.refreshRevoked != got.refresh {
		t.Errorf("%s: %d of %d refresh tokens revoked, want all — a live family is a way back in "+
			"that survives every access token expiring", what, got.refreshRevoked, got.refresh)
	}
	if got.accessRevoked != got.access {
		t.Errorf("%s: %d of %d access tokens revoked, want all", what, got.accessRevoked, got.access)
	}
	if got.liveInvitations != 0 {
		t.Errorf("%s: %d redeemable invitations survive, want 0 — a deprovisioned person's mailbox "+
			"must not still hold a working enrollment", what, got.liveInvitations)
	}
}

// poolWithLockTimeout is a second pool whose sessions REFUSE TO WAIT for a row
// lock, so "blocked" is an observable 55P03 rather than a hung test — the
// technique sendpath_test.go uses with SET LOCAL, moved onto the pool because
// the calls under test here open their own transactions and there is nowhere to
// put a SET LOCAL from outside.
func poolWithLockTimeout(ctx context.Context, t *testing.T, d time.Duration) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = fmt.Sprintf("%dms", d.Milliseconds())
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ---------------------------------------------------------------------------
// Criterion 3 — the offboard is all or nothing, converges on retry, and owes
// nobody a deadlock.

func TestDeactivateUserRevokesEverythingAndPublishesExactlyOnce(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)

	// Precondition: everything works, so only the offboard can be doing the
	// work below. Without this the test would pass against a fixture that had
	// never enrolled anything.
	if _, err := st.Authenticate(ctx, f.Phone.Access.Plaintext); err != nil {
		t.Fatalf("precondition: the phone's access token should work: %v", err)
	}
	if _, err := st.Authenticate(ctx, f.Laptop.Access.Plaintext); err != nil {
		t.Fatalf("precondition: the laptop's access token should work: %v", err)
	}

	var out Offboard
	got := revocationsDuring(ctx, t, pool, func() {
		var err error
		if out, err = st.DeactivateUser(ctx, f.UserID); err != nil {
			t.Errorf("deactivate: %v", err)
		}
	})

	if len(got) != 1 {
		t.Fatalf("%d revocations published, want exactly 1 — without one, every live socket this "+
			"account holds keeps streaming until it happens to drop (CANT-28 ruling 2)", len(got))
	}
	if got[0].UserID == nil || *got[0].UserID != f.UserID {
		t.Errorf("the revocation names user %v, want %s", got[0].UserID, f.UserID)
	}
	if got[0].DeviceID != nil {
		t.Errorf("the revocation also names device %v; an offboard's subject is the account", got[0].DeviceID)
	}

	if !out.Deactivated || !out.Changed() {
		t.Errorf("reported %+v, want Deactivated and Changed on a live account", out)
	}
	if out.Revoked.Devices != 2 || out.Revoked.EnrollmentTokens != 1 {
		t.Errorf("reported %+v, want 2 devices and 1 invitation — the counts are what the log line "+
			"and CANT-131's answer are built from", out.Revoked)
	}
	if out.Revoked.RefreshTokens < 2 || out.Revoked.AccessTokens < 2 {
		t.Errorf("reported %+v, want at least one refresh and one access token per device", out.Revoked)
	}
	assertFullyRevoked(ctx, t, pool, f.UserID, "after one offboard")

	// And every door the account had is shut, which is the claim the row counts
	// are only evidence for.
	if _, err := st.Authenticate(ctx, f.Phone.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the phone still authenticates: %v", err)
	}
	if _, err := st.Authenticate(ctx, f.Laptop.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the laptop still authenticates: %v", err)
	}
	if _, err := st.RotateRefresh(ctx, f.Phone.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the phone's refresh token still rotates: %v", err)
	}
	if _, err := st.RedeemEnrollment(ctx, f.Pending.Plaintext, "a device that should never exist"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the pending invitation still redeems: %v", err)
	}
}

// THE WRITE ORDER, ASSERTED AGAINST THE SOURCE OF BOTH WRITERS. A row-level
// oracle cannot see the order of two UPDATEs inside one committed transaction,
// and the order is the whole reason an offboard and a replay-triggered
// invalidation cannot deadlock. So this reads the two functions and requires
// them to agree — a source scan on tokens_guard_test.go's own precedent, for
// the same reason: the tables are strings in SQL, so only their names can be
// watched.
func TestTheOffboardTakesTheCredentialTablesInInvalidateFamilysOrder(t *testing.T) {
	root := storeModuleRoot(t)
	want := []string{"refresh_tokens", "access_tokens", "devices"}

	for _, tc := range []struct{ file, fn string }{
		{"internal/store/offboard.go", "func (s *Store) revokeCredentialsTx("},
		{"internal/store/refresh.go", "func (s *Store) invalidateFamily("},
	} {
		src, err := os.ReadFile(root + "/" + tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		body := string(src)
		at := strings.Index(body, tc.fn)
		if at < 0 {
			t.Fatalf("%s: %q is gone; this guard now watches nothing", tc.file, tc.fn)
		}
		body = body[at:]

		var order []int
		for _, table := range want {
			i := strings.Index(body, "UPDATE "+table)
			if i < 0 {
				t.Fatalf("%s does not UPDATE %s", tc.fn, table)
			}
			order = append(order, i)
		}
		if !(order[0] < order[1] && order[1] < order[2]) {
			t.Errorf("%s writes %v out of order (offsets %v).\n"+
				"The order must be refresh_tokens -> access_tokens -> devices in BOTH functions: an "+
				"invalidation's rows are a subset of an offboard's, so opposite orders over them are a "+
				"deadlock cycle, and Postgres resolves one by aborting somebody's offboard or somebody's "+
				"refusal.", tc.fn, want, order)
		}
	}
}

func TestAFaultAfterTheFirstWriteLeavesNothingChangedAndNothingPublished(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)

	before := tablesSnapshot(ctx, t, pool)
	st.personGuardFault = personFaultOffboardFailsAfterFirstWrite

	got := revocationsDuring(ctx, t, pool, func() {
		if _, err := st.DeactivateUser(ctx, f.UserID); err == nil {
			t.Error("the injected fault did not fail the call")
		}
	})

	if len(got) != 0 {
		t.Errorf("%d revocations published by a failed offboard, want 0 — a notification without its "+
			"cause severs sockets for an account that is still active", len(got))
	}
	if after := tablesSnapshot(ctx, t, pool); after != before {
		t.Error("a failed offboard changed the database.\n" +
			"users.deactivated_at was written before the fault, so this is ruling 4's whole claim: one " +
			"transaction, all or none. Without it the account is DEACTIVATED with every credential it " +
			"names still live, which is R6's partial failure arriving from inside the service.")
	}
	// The account is still whole, which is stronger than "the tables look the
	// same": its credentials still work.
	if _, err := st.Authenticate(ctx, f.Phone.Access.Plaintext); err != nil {
		t.Errorf("the phone stopped authenticating after a rolled-back offboard: %v", err)
	}
}

// THE SWEEPS ARE UNCONDITIONAL, which is what makes a retry a REPAIR rather
// than a no-op. Rev 1 of the plan gated the whole transaction on the users-row
// transition; this is the state that would leave behind.
func TestASecondOffboardRepairsAnAccountWhoseColumnWasAlreadySetAndPublishes(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)

	// By hand, exactly as a half-converged account looks: the column is set and
	// every credential it names is still live. (A hand UPDATE, an earlier
	// partial state, or the stray device RedeemEnrollment documents all arrive
	// here.)
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, f.UserID); err != nil {
		t.Fatal(err)
	}

	var out Offboard
	got := revocationsDuring(ctx, t, pool, func() {
		var err error
		if out, err = st.DeactivateUser(ctx, f.UserID); err != nil {
			t.Errorf("deactivate: %v", err)
		}
	})

	if out.Deactivated {
		t.Error("reported Deactivated on an account whose column was already set")
	}
	if !out.Changed() {
		t.Error("reported no change, having just revoked two live devices")
	}
	if len(got) != 1 {
		t.Fatalf("%d revocations published while repairing a half-converged account, want 1 — the "+
			"notification is the ONLY thing that severs those devices' sockets, and the call that set "+
			"the column is the call that did not send it", len(got))
	}
	assertFullyRevoked(ctx, t, pool, f.UserID, "after the repairing retry")
}

func TestAConvergedOffboardChangesNothingAndPublishesNothing(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)

	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("first offboard: %v", err)
	}
	before := tablesSnapshot(ctx, t, pool)

	var out Offboard
	got := revocationsDuring(ctx, t, pool, func() {
		var err error
		if out, err = st.DeactivateUser(ctx, f.UserID); err != nil {
			t.Errorf("second offboard: %v", err)
		}
	})

	if out.Changed() {
		t.Errorf("a converged retry reported %+v, want nothing changed", out)
	}
	if len(got) != 0 {
		t.Errorf("%d revocations published by a converged retry, want 0 — R6 requires Deprovision to "+
			"be safe to retry, and a retry that re-notifies severs sockets that were severed the first "+
			"time (the bug invalidateFamily's own comment records finding)", len(got))
	}
	if after := tablesSnapshot(ctx, t, pool); after != before {
		t.Error("a converged retry changed the database")
	}
}

// Criterion 6's negative control for the IS NULL guards — which are not
// "removed guards against reaching a bot" but are guards, and are the only
// thing making the two tests above different from each other.
func TestWithoutTheIsNullGuardsAConvergedRetryRepublishesAndMovesRevokedAt(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)

	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("first offboard: %v", err)
	}
	var firstRevocation time.Time
	mustScan(t, pool.QueryRow(ctx,
		`SELECT max(revoked_at) FROM devices WHERE user_id = $1`, f.UserID), &firstRevocation)

	st.personGuardFault = personFaultSweepIgnoresIsNull
	got := revocationsDuring(ctx, t, pool, func() {
		if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
			t.Errorf("second offboard: %v", err)
		}
	})

	if len(got) == 0 {
		t.Fatal("with the IS NULL guards removed a converged retry published nothing — the control " +
			"proves nothing, and the guards may be dead code")
	}
	var second time.Time
	mustScan(t, pool.QueryRow(ctx,
		`SELECT max(revoked_at) FROM devices WHERE user_id = $1`, f.UserID), &second)
	if !second.After(firstRevocation) {
		t.Errorf("revoked_at did not move (%v -> %v); this control exists to show that it would", firstRevocation, second)
	}
}

// THE LOCK, BEHAVIOURALLY, AND ITS OWN CONTROL. FOR NO KEY UPDATE composes with
// the KEY SHARE a send takes on users(author_id) at position 11; FOR UPDATE
// would not, and the cycle that follows is the one metadata.go's comment
// describes. Both halves are here because the passing half alone would also
// pass with no lock at all.
func TestTheOffboardsUserLockDoesNotBlockThatPersonsSend(t *testing.T) {
	ctx, pool := freshDB(t)
	author := mkUser(ctx, t, pool, "ada")
	conv := mkGroup(ctx, t, pool, "room", author)

	// The sender's pool refuses to WAIT, so a lock it should not have to wait
	// for shows up as 55P03 instead of as a slow test.
	sender := New(poolWithLockTimeout(ctx, t, 500*time.Millisecond), DefaultLimits(), discardLogger())

	for _, tc := range []struct {
		name      string
		lock      string
		wantBlock bool
	}{
		// THE QUERY THE OFFBOARD RUNS, not a copy of it: strengthen
		// deactivateLockQuery and this case goes red.
		{"the offboard's own lock", deactivateLockQuery(personFaultNone), false},
		{"a key-level lock on the same row", `SELECT id FROM users WHERE id = $1 FOR UPDATE`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			var locked uuid.UUID
			if err := holder.QueryRow(ctx, tc.lock, author).Scan(&locked); err != nil {
				t.Fatalf("take the lock: %v", err)
			}

			_, sendErr := sender.SendMessage(ctx, NewMessage{
				ConversationID: conv, AuthorID: author, ClientID: uuid.New(), Text: ptr("hello"),
			})
			var pgErr *pgconn.PgError
			blocked := errors.As(sendErr, &pgErr) && pgErr.Code == "55P03"

			switch {
			case tc.wantBlock && !blocked:
				t.Fatalf("a send was NOT blocked by FOR UPDATE on its author's row (err = %v). "+
					"This control exists to prove the probe has teeth; without it the case above "+
					"would pass against a lock that was never taken.", sendErr)
			case !tc.wantBlock && blocked:
				t.Fatalf("a send was blocked while an offboard held the author's row: %v.\n"+
					"FOR NO KEY UPDATE must not conflict with the KEY SHARE the insert takes on "+
					"users(author_id) — a key-level lock there holds a user row while the send holds "+
					"log_counter, which is a cycle no ordering removes.", sendErr)
			case !tc.wantBlock && sendErr != nil:
				t.Fatalf("the send failed for some other reason: %v", sendErr)
			}
		})
	}
}

// The other half of the same invariant, in one line, because a strengthened
// lock is a one-word edit and the behavioural test above needs a live Postgres
// to notice.
func TestTheOffboardsLockIsNeverKeyLevel(t *testing.T) {
	q := deactivateLockQuery(personFaultNone)
	if !strings.Contains(q, "FOR NO KEY UPDATE") {
		t.Errorf("the offboard's lock is %q, want FOR NO KEY UPDATE", q)
	}
	if strings.Contains(q, "FOR UPDATE") {
		t.Errorf("the offboard's lock is %q — FOR UPDATE on a user row cycles against the send path", q)
	}
	if !strings.Contains(q, "kind = 'person'") {
		t.Errorf("the offboard's lock is %q, want the kind predicate that keeps a bot out of reach", q)
	}
}

// AN OFFBOARD DRAWS EXACTLY ONE log_seq, HOWEVER MANY ROOMS, AND NO seq AT ALL.
//
// THIS TEST ASSERTED THE OPPOSITE UNTIL CANT-137, and the sentence it asserted —
// "the offboard must never enter the deployment-wide serialised section" — is the
// one CANT-135 ruling 3 overturned on purpose: without a draw the change reached
// no co-member's cursor until something else happened to touch the room. What
// survives unweakened is the half that was always the real hazard, and it is
// asserted more strictly than before: the draw is ONE, not one per room, and
// `conversations.last_seq` — the per-conversation ordinal, which is dense and
// whose gaps a client is entitled to treat as missing messages — must not move at
// all. Three rooms rather than one, because one room cannot tell a single draw
// from a draw per room.
func TestAnOffboardDrawsExactlyOneOrdinalForEveryRoomAndNoConversationSeq(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	rooms := []uuid.UUID{
		mkGroup(ctx, t, pool, "room-a", f.UserID),
		mkGroup(ctx, t, pool, "room-b", f.UserID),
		mkGroup(ctx, t, pool, "room-c", f.UserID),
	}
	if _, err := st.SendMessage(ctx, NewMessage{
		ConversationID: rooms[0], AuthorID: f.UserID, ClientID: uuid.New(), Text: ptr("before"),
	}); err != nil {
		t.Fatal(err)
	}

	lastSeqBefore := map[uuid.UUID]int64{}
	var before int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &before)
	for _, r := range rooms {
		var n int64
		mustScan(t, pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, r), &n)
		lastSeqBefore[r] = n
	}

	out, err := st.DeactivateUser(ctx, f.UserID)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	var after int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &after)
	if after != before+1 {
		t.Errorf("log_counter moved %d -> %d across an offboard of somebody in %d rooms; want exactly one "+
			"draw.\nOne atomic change draws ONE marker (metadata.go): an offboard is one event, not one per "+
			"room, and a second apply() in the same transaction would reach for its target row while "+
			"holding the counter — the cycle against SendMessage that metadata.go's header describes.",
			before, after, len(rooms))
	}
	if out.MarkerLogSeq != after {
		t.Errorf("the call reported MarkerLogSeq %d and the counter stands at %d; the reported value is "+
			"what an operator correlates against a client's cursor, so it has to be the value drawn",
			out.MarkerLogSeq, after)
	}
	// AND EVERY ROOM CARRIES THAT ONE VALUE, which is what makes the single draw
	// a delivery rather than an economy: a room left behind at its old marker is a
	// room whose member_count never arrives.
	for _, r := range rooms {
		var marker int64
		mustScan(t, pool.QueryRow(ctx, `SELECT metadata_log_seq FROM conversations WHERE id = $1`, r), &marker)
		if marker != after {
			t.Errorf("room %s carries marker %d, want the change's own %d", r, marker, after)
		}
		var lastSeqAfter int64
		mustScan(t, pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, r), &lastSeqAfter)
		if lastSeqAfter != lastSeqBefore[r] {
			t.Errorf("conversations.last_seq for %s moved %d -> %d across an offboard.\n"+
				"`seq` is DENSE: a client that can see 4 and 6 may assume 5 exists and it is missing it. "+
				"Nothing here writes a message, so nothing here may draw one.", r, lastSeqBefore[r], lastSeqAfter)
		}
	}
	var userMarker int64
	mustScan(t, pool.QueryRow(ctx, `SELECT metadata_log_seq FROM users WHERE id = $1`, f.UserID), &userMarker)
	if userMarker != after {
		t.Errorf("the person's own marker is %d, want %d — without it their `User` record, and so "+
			"`deactivated: true`, rides no page at all", userMarker, after)
	}
}

// A CONVERGED RETRY DRAWS NOTHING, on the same terms as it publishes nothing.
// Entering the deployment-wide serialised section to record that nothing happened
// is the cost MarkRead's own bump refuses for a duplicate receipt, and a
// drawn-but-silent value is a gap in log_seq no /sync can explain.
func TestAConvergedOffboardDrawsNoMarker(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	mkGroup(ctx, t, pool, "room", f.UserID)

	first, err := st.DeactivateUser(ctx, f.UserID)
	if err != nil {
		t.Fatalf("first offboard: %v", err)
	}
	if first.MarkerLogSeq == 0 {
		t.Fatal("the first offboard drew no marker, so this test's second half proves nothing")
	}
	var before int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &before)

	second, err := st.DeactivateUser(ctx, f.UserID)
	if err != nil {
		t.Fatalf("second offboard: %v", err)
	}
	if second.MarkerLogSeq != 0 {
		t.Errorf("a converged retry reported MarkerLogSeq %d, want 0", second.MarkerLogSeq)
	}
	var after int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &after)
	if after != before {
		t.Errorf("log_counter moved %d -> %d on a converged retry; want no draw at all", before, after)
	}
}

// A CREDENTIAL-REPAIRING RETRY DRAWS NOTHING EITHER, and the gate is
// `Deactivated` rather than `Changed()` for exactly this case. Revoking a stray
// device changes no value on the wire, so there is nothing to tell a co-member —
// while the publish, which severs that device's socket, still has to happen.
func TestARepairingOffboardPublishesButDrawsNoMarker(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	mkGroup(ctx, t, pool, "room", f.UserID)

	// The half-converged account TestASecondOffboardRepairs… builds: the column
	// set by something outside this service, every credential it names still live.
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, f.UserID); err != nil {
		t.Fatal(err)
	}
	var before int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &before)

	var out Offboard
	got := revocationsDuring(ctx, t, pool, func() {
		var err error
		if out, err = st.DeactivateUser(ctx, f.UserID); err != nil {
			t.Errorf("deactivate: %v", err)
		}
	})

	if len(got) != 1 {
		t.Fatalf("%d revocations published while repairing, want 1", len(got))
	}
	if out.Deactivated {
		t.Error("reported Deactivated on an account whose column was already set")
	}
	if out.MarkerLogSeq != 0 {
		t.Errorf("MarkerLogSeq %d on a retry that moved no column; want 0 — the marker announces a change "+
			"to what /sync serves, and revoking a device is not one", out.MarkerLogSeq)
	}
	var after int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &after)
	if after != before {
		t.Errorf("log_counter moved %d -> %d while only credentials were repaired", before, after)
	}
}

// A FAULT AFTER THE MARKER WRITES LEAVES NOTHING CHANGED, PUBLISHES NOTHING, AND
// CONSUMES NO VISIBLE log_seq. The counter is a ROW, not a sequence, so its draw
// rolls back with everything else — which is Invariant 1's whole argument, and
// this is the one place a test can watch it hold for a marker rather than for a
// message.
func TestAFaultAfterTheMarkerLeavesNothingChangedAndNoGap(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	mkGroup(ctx, t, pool, "room", f.UserID)

	before := tablesSnapshot(ctx, t, pool)
	beforeMembership := membershipSnapshot(ctx, t, pool)
	var headBefore int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &headBefore)

	st.personGuardFault = personFaultOffboardFailsAfterTheMarker
	got := revocationsDuring(ctx, t, pool, func() {
		if _, err := st.DeactivateUser(ctx, f.UserID); err == nil {
			t.Error("the injected fault did not fail the call")
		}
	})

	if len(got) != 0 {
		t.Errorf("%d revocations published by an offboard that failed after its marker, want 0", len(got))
	}
	if after := tablesSnapshot(ctx, t, pool); after != before {
		t.Error("an offboard that failed after its marker changed the database")
	}
	if after := membershipSnapshot(ctx, t, pool); after != beforeMembership {
		t.Error("an offboard that failed after its marker left a marker behind.\n" +
			"The bump writes conversations.metadata_log_seq, which membershipSnapshot excludes — so " +
			"seeing this fire means something else in that table moved and survived.")
	}
	var headAfter int64
	mustScan(t, pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`), &headAfter)
	if headAfter != headBefore {
		t.Errorf("log_counter stands at %d having been %d before a FAILED offboard.\n"+
			"A drawn value that survives its own rollback is a gap no /sync can explain — and it is "+
			"precisely what a bigserial would have left here.", headAfter, headBefore)
	}
	// And the marker the bump wrote is gone with it, so no room claims to have
	// been told anything.
	var marker int64
	mustScan(t, pool.QueryRow(ctx,
		`SELECT coalesce(max(metadata_log_seq), 0) FROM conversations`), &marker)
	if marker > headAfter {
		t.Errorf("a conversation carries marker %d above the counter's own %d", marker, headAfter)
	}
}

// ---------------------------------------------------------------------------
// Criterion 3's races. Each runs enough iterations to mean something, each
// racer's pool refuses to wait, and every goroutine carries a deadline — so a
// deadlock or a wedge FAILS rather than hangs.

// raceRounds is high enough that an ordering bug shows up rather than hides,
// and low enough that the whole file still runs in seconds. Every round is a
// different person in one database, so no round can be explained by the
// previous one's rows.
const raceRounds = 20

// racePerson is one person with one enrolled device, ready to be raced.
func racePerson(ctx context.Context, t *testing.T, st *Store, n int) (uuid.UUID, Enrollment, string) {
	t.Helper()
	email := fmt.Sprintf("racer%d@example.com", n)
	ep, err := st.EnsurePerson(ctx, email, "Racer")
	if err != nil {
		t.Fatalf("round %d: ensure: %v", n, err)
	}
	e, err := st.RedeemEnrollment(ctx, ep.Token.Plaintext, "phone")
	if err != nil {
		t.Fatalf("round %d: redeem: %v", n, err)
	}
	return ep.Account.UserID, e, email
}

// AN OFFBOARD RACING A REPLAY-TRIGGERED INVALIDATION. Both write the same three
// credential tables over overlapping rows; the reverse order is the deadlock
// cycle the write order exists to remove. invalidateFamily LOGS rather than
// returns, so the log is part of the oracle here.
func TestAnOffboardRacingAReplayTriggeredInvalidationNeitherAbortsNorDeadlocks(t *testing.T) {
	ctx, pool := freshDB(t)
	logger, buf := captureLogger()
	// Neither racer may WAIT for a lock: with a timeout this short, a cycle
	// surfaces as 55P03 (or 40P01) on a real assertion rather than as a test
	// that takes its full deadline and passes.
	st := New(poolWithLockTimeout(ctx, t, 2*time.Second), DefaultLimits(), logger)

	for n := range raceRounds {
		user, e, _ := racePerson(ctx, t, st, n)
		// A rotation, backdated past the grace window, so presenting the
		// original token is a REPLAY and invalidateFamily actually runs.
		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
			t.Fatalf("round %d: rotate: %v", n, err)
		}
		backdateRotation(ctx, t, pool, e.DeviceID, ReuseGraceWindow+time.Minute)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var offboardErr, replayErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, offboardErr = st.DeactivateUser(ctx, user)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, replayErr = st.RotateRefresh(ctx, e.Refresh.Plaintext)
		}()
		close(start)
		wg.Wait()

		if offboardErr != nil {
			t.Fatalf("round %d: the offboard failed while a replay was being detected: %v.\n"+
				"This is the one call that must not flake: Purser's Deprovision sees a 500.", n, offboardErr)
		}
		if !errors.Is(replayErr, ErrUnauthorized) {
			t.Fatalf("round %d: the replay was not refused: %v", n, replayErr)
		}
		assertFullyRevoked(ctx, t, pool, user, fmt.Sprintf("round %d", n))
	}

	for _, l := range logLines(t, buf) {
		if l["msg"] == "family invalidation failed" {
			t.Errorf("an invalidation failed while racing an offboard: %v.\n"+
				"invalidateFamily logs instead of returning, so a deadlock or a lock timeout there is "+
				"silent to its caller — an attacker keeps a live family and the refusal looks ordinary.", l)
		}
		if s, ok := l["error"].(string); ok && strings.Contains(s, "40P01") {
			t.Errorf("a deadlock (40P01) was detected: %v", l)
		}
	}
}

// ENSURE RACING DEACTIVATE, BOTH LAUNCH ORDERS.
//
// WHAT THIS CAN AND CANNOT ASSERT, AND THE DIFFERENCE IS RULING 5. The two
// calls serialize on the user row — FOR NO KEY UPDATE conflicts with itself —
// so the outcome is always one of the two serial orders, and they are NOT
// symmetrical:
//
//   - ensure, then deactivate: a token is issued for an active person and the
//     offboard then supersedes it. Every token either call returned is refused
//     afterwards, and the account is deactivated.
//   - deactivate, then ensure: ensure finds a deactivated person and
//     REACTIVATES them (ruling 5, picked by a person). The account is active
//     afterwards and the token ensure returned is deliberately live — that is
//     the ruling, and a test asserting otherwise would be asserting against it.
//
// So what is asserted in both orders is the property that covers both: THERE IS
// NEVER A LIVE CREDENTIAL ON A DEACTIVATED ACCOUNT, and never a token from
// before the race that still works. The ticket's own `Done when` states the
// first bullet's wording ("redeeming every token either call ever returned is
// refused") for both orders; it holds verbatim in that order and cannot in the
// other without contradicting ruling 5, so this test proves it where it is true
// and proves the coherence property everywhere. Named in the PR body.
func TestEnsurePersonRacingDeactivateUserCommitsOneOfTheTwoSerialOrders(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(poolWithLockTimeout(ctx, t, 2*time.Second), DefaultLimits(), discardLogger())

	var reactivated, offboarded int
	for n := range raceRounds {
		user, e, email := racePerson(ctx, t, st, n)
		// An invitation issued BEFORE the race. Whatever order wins, this one
		// must never redeem afterwards: the offboard supersedes it, and so does
		// the reactivation's own fresh issue.
		stale, err := st.IssueEnrollmentToken(ctx, user)
		if err != nil {
			t.Fatalf("round %d: stale invitation: %v", n, err)
		}

		var ensured EnsuredPerson
		var ensureErr, offboardErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		// BOTH LAUNCH ORDERS, alternating by round. Launch order does not
		// decide the serial order — the lock does — so alternating is how the
		// test covers both without pretending to control which wins.
		first := func() { ensured, ensureErr = st.EnsurePerson(ctx, email, "Racer") }
		second := func() { _, offboardErr = st.DeactivateUser(ctx, user) }
		if n%2 == 1 {
			first, second = second, first
		}
		go func() { defer wg.Done(); <-start; first() }()
		go func() { defer wg.Done(); <-start; second() }()
		close(start)
		wg.Wait()

		if offboardErr != nil {
			t.Fatalf("round %d: the offboard failed: %v", n, offboardErr)
		}
		if ensureErr != nil {
			t.Fatalf("round %d: ensure failed: %v", n, ensureErr)
		}

		// Nothing from before the race survives, in EITHER order.
		if _, err := st.RedeemEnrollment(ctx, stale.Plaintext, "stale"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("round %d: an invitation issued before the race still redeems: %v", n, err)
		}
		if _, err := st.Authenticate(ctx, e.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("round %d: the device enrolled before the race still authenticates: %v", n, err)
		}
		if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("round %d: the device enrolled before the race still rotates: %v", n, err)
		}

		state := readAccountState(ctx, t, pool, user)
		switch ensured.Outcome {
		case PersonExisting:
			// ensure went first. The offboard then superseded its token, so
			// EVERY token either call returned is refused — the ticket's own
			// wording, in the order where it holds.
			offboarded++
			if !state.deactivated {
				t.Fatalf("round %d: ensure won the row and the offboard did not deactivate the account", n)
			}
			if _, err := st.RedeemEnrollment(ctx, ensured.Token.Plaintext, "after the race"); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("round %d: the token ensure returned still redeems on a DEACTIVATED account: %v.\n"+
					"An offboard that leaves a redeemable invitation behind is a deprovisioned person "+
					"with a working way back in.", n, err)
			}
			assertFullyRevoked(ctx, t, pool, user, fmt.Sprintf("round %d (ensure then deactivate)", n))
		case PersonReactivated:
			// The offboard went first and ensure reversed it — ruling 5. The
			// account is active and exactly the reversal's own token works.
			reactivated++
			if state.deactivated {
				t.Fatalf("round %d: a reactivation committed and left deactivated_at set", n)
			}
			if state.devicesRevoked != state.devices {
				t.Fatalf("round %d: a reactivation left %d of %d devices live — the reversal revokes "+
					"for itself before it clears the column", n, state.devices-state.devicesRevoked, state.devices)
			}
			if _, err := st.RedeemEnrollment(ctx, ensured.Token.Plaintext, "the only way back in"); err != nil {
				t.Fatalf("round %d: the reactivation's own token does not redeem: %v — ruling 5 says the "+
					"person enrolls again from nothing, which needs exactly one working invitation", n, err)
			}
		default:
			t.Fatalf("round %d: outcome = %q, want existing or reactivated", n, ensured.Outcome)
		}
	}

	// Both orders were actually exercised. Without this the test could pass
	// having only ever seen one of them — the failure mode a race test is most
	// prone to.
	if reactivated == 0 || offboarded == 0 {
		t.Errorf("only one serial order occurred in %d rounds (reactivated=%d, offboarded=%d); "+
			"this test proves nothing about the other", raceRounds, reactivated, offboarded)
	}
}

// A SEND RACING AN OFFBOARD. The send takes KEY SHARE on users(author_id) and
// on devices(sender_device_id) at position 11; the offboard holds FOR NO KEY
// UPDATE on the first and writes the second. Neither conflicts, so neither
// waits — and the send either commits or is refused for a reason of its own,
// never because two writers took locks in opposite orders.
func TestASendRacingAnOffboardNeitherDeadlocks(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(poolWithLockTimeout(ctx, t, 2*time.Second), DefaultLimits(), discardLogger())

	for n := range raceRounds {
		user, e, _ := racePerson(ctx, t, st, n)
		conv := mkGroup(ctx, t, pool, fmt.Sprintf("room%d", n), user)

		var sendErr, offboardErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, sendErr = st.SendMessage(ctx, NewMessage{
				ConversationID: conv, AuthorID: user, SenderDeviceID: &e.DeviceID,
				ClientID: uuid.New(), Text: ptr("racing an offboard"),
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, offboardErr = st.DeactivateUser(ctx, user)
		}()
		close(start)
		wg.Wait()

		if offboardErr != nil {
			t.Fatalf("round %d: the offboard failed while a send was in flight: %v", n, offboardErr)
		}
		// The send may succeed or fail on its own merits; what it must not be
		// is blocked or aborted by the offboard's locks.
		var pgErr *pgconn.PgError
		if errors.As(sendErr, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "40P01") {
			t.Fatalf("round %d: the send was blocked or deadlocked by the offboard (%s): %v.\n"+
				"An offboard must be invisible to the send path — a person's last message must not "+
				"fail because somebody deprovisioned them a millisecond later.", n, pgErr.Code, sendErr)
		}
		if sendErr != nil && !errors.Is(sendErr, ErrUnauthorized) {
			// A refusal from the send path's own validation is fine; anything
			// else is worth seeing.
			t.Logf("round %d: the send was refused: %v (acceptable — it raced its own author's offboard)", n, sendErr)
		}
	}
}

// ---------------------------------------------------------------------------
// Criterion 6 — a bot is out of reach, and the predicate is not redundant.

func TestDeactivateUserOnABotsIdIsNotFoundAndLeavesEveryBotTokenWorking(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	botToken, err := st.IssueBotToken(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	f := newOffboarded(ctx, t, st)

	if _, err := st.DeactivateUser(ctx, bot); !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("deactivating a bot = %v, want ErrPersonNotFound — a bot must not be distinguishable "+
			"from nobody, or this surface is an oracle for which ids name service accounts", err)
	}
	// And an UNKNOWN id answers identically, which is the other half of that
	// claim.
	if _, err := st.DeactivateUser(ctx, uuid.New()); !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("deactivating an unknown id = %v, want the same ErrPersonNotFound", err)
	}
	var deactivated *time.Time
	mustScan(t, pool.QueryRow(ctx, `SELECT deactivated_at FROM users WHERE id = $1`, bot), &deactivated)
	if deactivated != nil {
		t.Error("the bot was deactivated by a call that reported not-found")
	}

	// A person's offboard leaves every bot token working — the sweeps are
	// scoped by account, and a bot is another account.
	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("deactivate the person: %v", err)
	}
	if _, err := st.Authenticate(ctx, botToken.Plaintext); err != nil {
		t.Errorf("a bot's token stopped working when a PERSON was deprovisioned: %v", err)
	}
}

// Criterion 6's negative control for DeactivateUser's own kind predicate. No
// broken CHECK is needed to reach it, unlike personFaultLookupIgnoresKind's:
// this call takes an ID, so nothing about the bot's row stands between Purser
// and a service account except this one predicate.
func TestWithoutItsKindFilterDeactivateUserWouldRevokeABotsCredentials(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	botToken, err := st.IssueBotToken(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}

	st.personGuardFault = personFaultDeactivateIgnoresKind
	out, err := st.DeactivateUser(ctx, bot)
	if err != nil {
		t.Fatalf("with the kind filter removed the bot should have been deactivated: %v", err)
	}
	if !out.Deactivated || out.Revoked.AccessTokens == 0 {
		t.Fatalf("got %+v, want the bot deactivated and its token revoked — the defect this fault "+
			"exists to demonstrate", out)
	}
	if _, err := st.Authenticate(ctx, botToken.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the bot's token still works, so this control shows nothing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 7 — membership does what ruling 7 picked: it stays.

// membershipSnapshot dumps every row of the tables an offboard must not touch,
// as one comparable string — tablesSnapshot's shape, pointed at the other side
// of the model. `messages` is here too: an offboard must not edit, delete or
// re-attribute a single one.
//
// `conversations.metadata_log_seq` IS THE ONE EXCLUSION, AND CANT-137 IS WHY.
// That column is not membership and not content: it is the marker that carries a
// changed `member_count` to the room's other members, and an offboard now moves
// it in exactly the rooms the person is a member of. Excluding it by name — rather
// than dropping `conversations` from this snapshot — is what keeps the assertion
// SHARPER than it was: every other column of that table, `name`, `kind`,
// `last_seq`, `retention_days` and `direct_key`, must still be identical, so the
// offboard is held to writing precisely one column of it and nothing else.
//
// `conversation_members` IS NOT EXCLUDED AT ALL, including its own
// `metadata_log_seq`. A deactivation changes nobody's first_unread_seq and
// nobody's muted, so the per-member marker must not move — and this is where that
// is asserted (metadata.go's own note says the same thing from the other side).
//
// The columns come from to_jsonb rather than from a hand-written list, so a
// column added to any of the three tables is in the snapshot the day it is
// added and this helper cannot silently stop watching one.
func membershipSnapshot(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []struct{ name, sql string }{
		{"conversation_members", `SELECT to_jsonb(x)::text FROM conversation_members x
			 ORDER BY conversation_id, user_id`},
		{"conversations", `SELECT (to_jsonb(c) - 'metadata_log_seq')::text FROM conversations c ORDER BY id`},
		{"messages", `SELECT to_jsonb(m)::text FROM messages m ORDER BY log_seq`},
	} {
		rows, err := pool.Query(ctx, q.sql)
		if err != nil {
			t.Fatalf("snapshot %s: %v", q.name, err)
		}
		vals, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("snapshot %s: collect: %v", q.name, err)
		}
		b.WriteString(q.name)
		b.WriteByte(':')
		for _, v := range vals {
			fmt.Fprintf(&b, "%s;", v)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func TestAnOffboardLeavesMembershipReceiptsAndMessagesByteForByte(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", f.UserID, theo)

	// Traffic in both directions and a receipt, so there is something to
	// preserve: read_seq, metadata_log_seq and two authors' messages.
	sent := send(ctx, t, st, conv, f.UserID, "from the person being offboarded")
	send(ctx, t, st, conv, theo, "from somebody staying")
	if _, err := st.MarkRead(ctx, conv, theo, sent.Seq); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	before := membershipSnapshot(ctx, t, pool)
	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if after := membershipSnapshot(ctx, t, pool); after != before {
		t.Error("an offboard changed conversation_members, conversations or messages.\n" +
			"Ruling 7: a membership row is not a credential. The person is frozen in place — their " +
			"messages stay attributed, their receipts stop moving, and a reactivation finds their rooms " +
			"as they left them. Removing them instead would draw log_seq per room and reach every " +
			"other member's /sync, which is a different ticket and a different decision.")
	}

	// Still a member, by the one query that decides it.
	var stillAMember bool
	mustScan(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM conversation_members WHERE conversation_id = $1 AND user_id = $2)`,
		conv, f.UserID), &stillAMember)
	if !stillAMember {
		t.Error("the deactivated person is no longer a member")
	}
}

// THE OTHER MEMBER'S VIEW, THROUGH THE PATH THAT COMPUTES IT rather than
// through the columns it reads. first_unread_seq, member_count and read_by are
// all derived (invariant 3), so "the rows did not change" is not the same claim
// as "what the other member sees did not change" — and the second is the one
// ruling 7's cost was stated in terms of.
//
// TWO OF THE THREE CHANGED THEIR ANSWER IN CANT-137, AND THAT IS THE POINT OF
// THE TICKET RATHER THAN A WEAKENING OF THIS TEST. Its old body required
// `member_count` and `read_by` to be unchanged across an offboard, and said in
// so many words that CANT-135 was where that would be answered and it must not
// be answered here by accident. It is answered there now, deliberately, and this
// test carries the corrected claim: the two counts FALL, by one predicate,
// together; `first_unread_seq` — which reads only messages and the viewer's own
// read_seq — does not move at all. The room-of-four scenario with a reactivation
// is honestcounts_test.go's TestARoomOfFourCountsOnlyItsActiveMembers; this one
// stays here because it is the CANT-134 evidence being corrected.
func TestAnOffboardLowersTheOtherMembersCountsAndLeavesFirstUnreadSeqAlone(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	f := newOffboarded(ctx, t, st)
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", f.UserID, theo)

	send(ctx, t, st, conv, f.UserID, "one")
	send(ctx, t, st, conv, f.UserID, "two")

	before, err := st.Sync(ctx, theo, 0, 50)
	if err != nil {
		t.Fatalf("sync before: %v", err)
	}
	if _, err := st.DeactivateUser(ctx, f.UserID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	after, err := st.Sync(ctx, theo, 0, 50)
	if err != nil {
		t.Fatalf("sync after: %v", err)
	}

	if len(before.Conversations) != 1 || len(after.Conversations) != 1 {
		t.Fatalf("conversations before=%d after=%d, want 1 each", len(before.Conversations), len(after.Conversations))
	}
	b, a := before.Conversations[0], after.Conversations[0]
	if b.MemberCount != 2 || a.MemberCount != 1 {
		t.Errorf("member count %d -> %d, want 2 -> 1. `N MEMBERS` must stop counting somebody who can "+
			"no longer read anything — the claim invariant 3 says this service must not make, and the "+
			"cost ruling 7 of CANT-33 accepted until CANT-135 ruling 1 removed it", b.MemberCount, a.MemberCount)
	}
	switch {
	case (b.FirstUnreadSeq == nil) != (a.FirstUnreadSeq == nil):
		t.Errorf("first_unread_seq presence changed: %v -> %v.\n"+
			"It depends only on messages and this viewer's own read_seq, so an offboard must not reach it "+
			"— and it is the one of the three counts CANT-137 deliberately did not touch.",
			b.FirstUnreadSeq, a.FirstUnreadSeq)
	case b.FirstUnreadSeq != nil && *b.FirstUnreadSeq != *a.FirstUnreadSeq:
		t.Errorf("first_unread_seq %d -> %d for a member who was not offboarded", *b.FirstUnreadSeq, *a.FirstUnreadSeq)
	}
	if len(before.ReadBy) != len(after.ReadBy) {
		t.Fatalf("read_by covers %d messages before and %d after", len(before.ReadBy), len(after.ReadBy))
	}
	// Both messages were written by the person now offboarded and theo has read
	// neither, so the author's own +1 is the whole of the count before and there
	// is nobody left to count after. This is `read_by: 0`'s THIRD cause, which
	// the schema's description now names.
	for id, n := range before.ReadBy {
		if n != 1 {
			t.Errorf("read_by for message %s was %d before the offboard, want 1 (the author, by "+
				"identity); the case below is not the one this test means to exercise", id, n)
		}
		if after.ReadBy[id] != 0 {
			t.Errorf("read_by for message %s: %d -> %d, want 0.\n"+
				"The author is deactivated, so they no longer count themselves and no active member's "+
				"receipt has passed the message — the third cause of 0 that Message.read_by's schema "+
				"description now states.", id, n, after.ReadBy[id])
		}
	}
}
