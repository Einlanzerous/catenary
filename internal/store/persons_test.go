package store

// CANT-130 criteria 1, 2 and 6 (this row's own predicates) — EnsurePerson,
// PersonByEmail, SetEmail and the handle function.
//
// The negative controls below (TestWithout…) are the deliberately-broken
// counterparts proposal_test.go's TestWithoutItsBranchesACollisionFailsTheWay-
// EachBranchPrevents already established the shape for: a fault flipped on,
// and a test that is EXPECTED to show the bad behaviour, so the corresponding
// guard's necessity is not merely asserted but demonstrated.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// tablesSnapshot — criterion 2's "no row touched" oracle, shared by C1's
// deactivated-refusal test.

// tablesSnapshot dumps every row of the credential-adjacent tables a call
// promises not to touch, as one comparable string. fmt's own map formatting
// sorts keys, so two snapshots of the same rows compare equal regardless of
// scan order.
func tablesSnapshot(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var b strings.Builder
	for _, table := range []string{"users", "enrollment_tokens", "devices", "refresh_tokens", "access_tokens"} {
		rows, err := pool.Query(ctx, `SELECT * FROM `+table+` ORDER BY id`)
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		vals, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			t.Fatalf("snapshot %s: collect: %v", table, err)
		}
		b.WriteString(table)
		b.WriteByte(':')
		for _, v := range vals {
			// %v on a map is key-sorted since Go 1.12, so this is stable
			// across two snapshots of identical rows.
			fmt.Fprintf(&b, "%v;", v)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// dropBotHasNoEmailCheck removes 0010's users_bot_has_no_email CHECK, the
// only way to ever put a bot in the state its own code-level guards are
// written to survive — see the negative controls below for why this is
// necessary rather than gratuitous.
//
// RESTORED ON CLEANUP, BEFORE freshDB's OWN pool.Close AND cancel — testing.T
// runs Cleanup funcs last-added-first, and this is added after freshDB's, so
// it unwinds first, on a still-open pool and a still-live context. Without
// this, the NEXT test's freshDB reset (MigrateDown to empty) fails trying to
// run 0010's own down migration against a constraint this test already
// removed by hand, and every test after this one in the package goes down
// with it.
func dropBotHasNoEmailCheck(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `ALTER TABLE users DROP CONSTRAINT `+usersBotHasNoEmailConstraint); err != nil {
		t.Fatalf("drop %s: %v", usersBotHasNoEmailConstraint, err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `UPDATE users SET email = NULL WHERE kind = 'bot'`); err != nil {
			t.Fatalf("cleanup: clear bot emails: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`ALTER TABLE users ADD CONSTRAINT `+usersBotHasNoEmailConstraint+` CHECK (kind <> 'bot' OR email IS NULL)`); err != nil {
			t.Fatalf("cleanup: restore %s: %v", usersBotHasNoEmailConstraint, err)
		}
	})
}

// ---------------------------------------------------------------------------
// Criterion 1 — a person can be created, once, with a handle that collides
// with nobody.

func TestEnsurePersonCreatesOnePersonWithExactlyOneRedeemableToken(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	ep, err := st.EnsurePerson(ctx, "Ada@Example.com", "Ada Lovelace")
	if err != nil {
		t.Fatalf("ensure person: %v", err)
	}
	if ep.Outcome != PersonCreated {
		t.Errorf("outcome = %q, want %q", ep.Outcome, PersonCreated)
	}
	if ep.Account.Email != "ada@example.com" {
		t.Errorf("email = %q, want the normalized lowercase address", ep.Account.Email)
	}
	if ep.Account.Handle != "ada" {
		t.Errorf("handle = %q, want ada", ep.Account.Handle)
	}
	if ep.Account.DisplayName != "Ada Lovelace" {
		t.Errorf("display name = %q, want the one passed in", ep.Account.DisplayName)
	}
	if ep.Account.Status != PersonStatusActive {
		t.Errorf("status = %q, want active", ep.Account.Status)
	}
	if ep.Note != "" {
		t.Errorf("note = %q, want empty — nothing was adopted or skipped", ep.Note)
	}

	if n := countRows(ctx, t, pool, `SELECT count(*) FROM users WHERE kind = 'person'`); n != 1 {
		t.Fatalf("%d person rows, want 1", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM enrollment_tokens WHERE user_id = $1`, ep.Account.UserID); n != 1 {
		t.Fatalf("%d enrollment tokens, want exactly 1", n)
	}

	enrolled, err := st.RedeemEnrollment(ctx, ep.Token.Plaintext, "Pixel 8 Pro")
	if err != nil {
		t.Fatalf("the issued token did not redeem: %v", err)
	}
	if enrolled.UserID != ep.Account.UserID {
		t.Errorf("redeemed into %s, want %s", enrolled.UserID, ep.Account.UserID)
	}
}

func TestEnsurePersonAgainReturnsTheSameAccountAndSupersedesTheFirstToken(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	first, err := st.EnsurePerson(ctx, "ada@example.com", "Ada")
	if err != nil {
		t.Fatal(err)
	}

	second, err := st.EnsurePerson(ctx, "ADA@EXAMPLE.COM", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if second.Outcome != PersonExisting {
		t.Errorf("outcome = %q, want %q", second.Outcome, PersonExisting)
	}
	if second.Account.UserID != first.Account.UserID {
		t.Fatal("a second ensure of the same (case-folded) email produced a different account")
	}
	if second.Token.Plaintext == first.Token.Plaintext {
		t.Fatal("the second token is identical to the first")
	}

	if _, err := st.RedeemEnrollment(ctx, first.Token.Plaintext, "old phone"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the first token still redeems after a second ensure: %v", err)
	}
	if _, err := st.RedeemEnrollment(ctx, second.Token.Plaintext, "new phone"); err != nil {
		t.Errorf("the second (current) token does not redeem: %v", err)
	}
}

// The approving review's own addition to criterion 1.
func TestEnsurePersonAgainWithADifferentNameLeavesTheNameAndTheCounterUntouched(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	first, err := st.EnsurePerson(ctx, "ada@example.com", "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}

	var before int64
	if err := pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	second, err := st.EnsurePerson(ctx, "ada@example.com", "Someone Else Entirely")
	if err != nil {
		t.Fatal(err)
	}
	if second.Account.DisplayName != "Ada Lovelace" {
		t.Errorf("returned display name = %q, want the original — ensure never renames", second.Account.DisplayName)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1`, first.Account.UserID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "Ada Lovelace" {
		t.Errorf("stored display_name = %q, want unchanged", stored)
	}

	var after int64
	if err := pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("log_counter moved %d -> %d — ensure drew the deployment-wide counter for a rename it never made", before, after)
	}
}

func TestEightConcurrentEnsurePersonsForOneNewEmailProduceOneUser(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	const n = 8
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ep, err := st.EnsurePerson(ctx, "concurrent@example.com", "Name")
			ids[i], errs[i] = ep.Account.UserID, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	first := ids[0]
	for i, id := range ids {
		if id != first {
			t.Errorf("racer %d resolved to %s, want %s", i, id, first)
		}
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM users WHERE kind = 'person'`); n != 1 {
		t.Errorf("%d person rows after 8 concurrent ensures of one email, want 1", n)
	}
}

func TestConcurrentEnsurePersonsWithTheSameLocalPartGetSuffixedHandles(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	emails := []string{"alice@a.example", "alice@b.example"}
	results := make([]EnsuredPerson, len(emails))
	errs := make([]error, len(emails))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, email := range emails {
		wg.Add(1)
		go func(i int, email string) {
			defer wg.Done()
			<-start
			results[i], errs[i] = st.EnsurePerson(ctx, email, "Alice")
		}(i, email)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	if results[0].Account.UserID == results[1].Account.UserID {
		t.Fatal("two different emails produced one account")
	}
	handles := map[string]bool{results[0].Account.Handle: true, results[1].Account.Handle: true}
	if !handles["alice"] || !handles["alice-2"] {
		t.Fatalf("got handles %v, want {alice, alice-2}", handles)
	}
}

func TestDeriveHandle(t *testing.T) {
	for _, tc := range []struct {
		name, email, want string
	}{
		{"a +tag is dropped", "alice+work@example.com", "alice"},
		{"dots survive", "a.b.c@example.com", "a.b.c"},
		{"mixed case folds to lower", "AlicE@Example.com", "alice"},
		{"unicode is dropped rather than transliterated", "héllo@example.com", "hllo"},
		{"empty after stripping falls back to user", "+++@example.com", "user"},
		{"punctuation-only local part falls back to user", "...@example.com", "user"},
		{"a 64-octet local part truncates to 32", strings.Repeat("a", 64) + "@example.com", strings.Repeat("a", 32)},
		{"leading and trailing punctuation is trimmed", "-.alice.-@example.com", "alice"},
		// Found in review (#79): a stem whose 33rd character was the ONLY
		// thing keeping an internal dot from being the last one. Truncating
		// to 32 without re-trimming would return "...29 a's....b." — ending
		// in a dot the doc comment says cannot happen.
		{
			"truncation cannot reintroduce trailing punctuation",
			strings.Repeat("a", 29) + ".b.c@x.example",
			strings.Repeat("a", 29) + ".b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveHandle(tc.email); got != tc.want {
				t.Errorf("deriveHandle(%q) = %q, want %q", tc.email, got, tc.want)
			}
		})
	}
}

func TestHandleCandidateTruncatesToMakeRoomForItsSuffix(t *testing.T) {
	stem := strings.Repeat("a", maxHandleLength)
	if got := handleCandidate(stem, 1); got != stem {
		t.Errorf("candidate(1) = %q, want the bare, untruncated stem", got)
	}
	for _, n := range []int{2, 3, 10} {
		suffix := fmt.Sprintf("-%d", n)
		got := handleCandidate(stem, n)
		if len(got) > maxHandleLength {
			t.Errorf("candidate(%d) = %q is %d bytes, want <= %d", n, got, len(got), maxHandleLength)
		}
		if !strings.HasSuffix(got, suffix) {
			t.Errorf("candidate(%d) = %q, want it to carry its suffix %q", n, got, suffix)
		}
	}
}

// Found in review (#79): handleCandidate's OWN truncation, cutting an
// already-clean stem further to make room for a suffix, can land on an
// internal dot exactly as easily as deriveHandle's own truncation can.
func TestHandleCandidateDoesNotReintroduceTrailingPunctuation(t *testing.T) {
	stem := strings.Repeat("a", 29) + ".b" // 31 bytes, deriveHandle's own clean output
	got := handleCandidate(stem, 2)
	if !strings.HasSuffix(got, "-2") {
		t.Fatalf("candidate = %q, want it to carry -2", got)
	}
	before := strings.TrimSuffix(got, "-2")
	if strings.HasSuffix(before, ".") || strings.HasSuffix(before, "-") || strings.HasSuffix(before, "_") {
		t.Fatalf("candidate = %q, the truncated stem ends in punctuation right before its suffix", got)
	}
	if len(got) > maxHandleLength {
		t.Fatalf("candidate = %q is %d bytes, want <= %d", got, len(got), maxHandleLength)
	}
}

func TestEnsurePersonTruncatesAFullLengthHandleToMakeRoomForItsSuffix(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	stem := strings.Repeat("a", maxHandleLength)

	first, err := st.EnsurePerson(ctx, stem+"@example.com", "First")
	if err != nil {
		t.Fatal(err)
	}
	if first.Account.Handle != stem {
		t.Fatalf("first handle = %q, want the untruncated %d-char stem", first.Account.Handle, maxHandleLength)
	}

	second, err := st.EnsurePerson(ctx, stem+"@other.example", "Second")
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Account.Handle) > maxHandleLength {
		t.Fatalf("second handle %q is %d bytes, want <= %d", second.Account.Handle, len(second.Account.Handle), maxHandleLength)
	}
	if !strings.HasSuffix(second.Account.Handle, "-2") {
		t.Fatalf("second handle %q does not carry its de-dup suffix", second.Account.Handle)
	}
}

func TestEnsurePersonFoldsCaseAgainstABotsHandle(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	if _, err := st.CreateBot(ctx, "Argosy", "Argosy"); err != nil {
		t.Fatal(err)
	}

	ep, err := st.EnsurePerson(ctx, "argosy@x.example", "Someone")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Account.Handle != "argosy-2" {
		t.Errorf("handle = %q, want argosy-2 — a bot's mixed-case handle must still collide case-insensitively", ep.Account.Handle)
	}
}

func TestEnsurePersonNeverAdoptsAnEmaillessPersonsHandle(t *testing.T) {
	ctx, pool := freshDB(t)
	legacy := mkUser(ctx, t, pool, "alice")

	logger, buf := captureLogger()
	st := New(pool, DefaultLimits(), logger)

	ep, err := st.EnsurePerson(ctx, "alice@example.com", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Account.Handle != "alice-2" {
		t.Fatalf("handle = %q, want alice-2 — an email-less person must never be adopted by handle match", ep.Account.Handle)
	}
	if ep.Account.UserID == legacy {
		t.Fatal("the legacy email-less account was adopted rather than a new one created")
	}
	if !strings.Contains(ep.Note, "alice-2") || !strings.Contains(ep.Note, "alice") {
		t.Errorf("note = %q, want the operator-facing sentence naming both handles", ep.Note)
	}
	if strings.Contains(ep.Note, "@") {
		t.Error("the note names an email address")
	}

	logged := buf.String()
	if !strings.Contains(logged, "alice-2") {
		t.Errorf("the WARN log does not name the assigned handle:\n%s", logged)
	}
	if strings.Contains(strings.ToLower(logged), "@example.com") {
		t.Errorf("the WARN log leaks the email address:\n%s", logged)
	}

	var email *string
	if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, legacy).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != nil {
		t.Error("the legacy account was given an email by a create it never asked for")
	}
}

// CANT-130's TestEnsurePersonOnADeactivatedPersonRefusesAndChangesNothing HAS
// BECOME reactivate_test.go's TestEnsurePersonOnADeactivatedPersonReactivates-
// ThemAfterRevokingEverything. That test asserted the placeholder ruling 5
// always meant to replace — ErrPersonDeactivated, and nothing changed — so
// CANT-134 could not leave it standing and could not keep its name either: the
// behaviour it pinned is now the behaviour that would be a bug.

func TestEnsurePersonRefusesAMalformedEmailBeforeTouchingThePool(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	pool.Close() // any real query now fails; validation has to run first

	for _, bad := range []string{"", "   ", "not-an-email", "two@@at.example", "trailing@", "@leading.example", "no-dot@example"} {
		if _, err := st.EnsurePerson(ctx, bad, "Name"); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("EnsurePerson(%q) = %v, want ErrInvalidEmail with no pool access", bad, err)
		}
	}
}

func TestTheHandleAndEmailConstraintsArePinned(t *testing.T) {
	ctx, pool := freshDB(t)

	var handleConstraint string
	if err := pool.QueryRow(ctx, `
		SELECT c.conname
		  FROM pg_constraint c
		  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		 WHERE c.conrelid = 'users'::regclass AND c.contype = 'u' AND a.attname = 'handle'`).
		Scan(&handleConstraint); err != nil {
		t.Fatalf("users has no UNIQUE constraint on handle: %v", err)
	}
	if handleConstraint != usersHandleKeyConstraint {
		t.Errorf("the handle constraint is %q; isHandleCollision matches %q and would route nothing",
			handleConstraint, usersHandleKeyConstraint)
	}

	var emailIndex string
	if err := pool.QueryRow(ctx, `
		SELECT indexname FROM pg_indexes WHERE tablename = 'users' AND indexdef ILIKE '%lower(email)%'`).
		Scan(&emailIndex); err != nil {
		t.Fatalf("users has no case-insensitive email index: %v", err)
	}
	if emailIndex != usersEmailLowerIndexConstraint {
		t.Errorf("the email index is %q; isEmailCollision matches %q and would route nothing",
			emailIndex, usersEmailLowerIndexConstraint)
	}

	var checkConstraint string
	if err := pool.QueryRow(ctx, `
		SELECT conname FROM pg_constraint
		 WHERE conrelid = 'users'::regclass AND contype = 'c' AND conname = $1`, usersBotHasNoEmailConstraint).
		Scan(&checkConstraint); err != nil {
		t.Fatalf("users has no %s CHECK: %v", usersBotHasNoEmailConstraint, err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 2 — look-up never changes anything.

func TestPersonByEmailReportsAnActivePersonAndTheirLiveDeviceCount(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ep, err := st.EnsurePerson(ctx, "ada@example.com", "Ada")
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.PersonByEmail(ctx, "ADA@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("case-insensitive lookup: %v", err)
	}
	if got.UserID != ep.Account.UserID {
		t.Error("resolved to the wrong account")
	}
	if got.Status != PersonStatusActive {
		t.Errorf("status = %q, want active", got.Status)
	}
	if got.LiveDevices != 0 {
		t.Errorf("live devices = %d, want 0 before any redemption", got.LiveDevices)
	}

	if _, err := st.RedeemEnrollment(ctx, ep.Token.Plaintext, "phone"); err != nil {
		t.Fatal(err)
	}
	got2, err := st.PersonByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got2.LiveDevices != 1 {
		t.Errorf("live devices = %d, want 1 after one redemption", got2.LiveDevices)
	}
}

func TestPersonByEmailReportsADeactivatedPerson(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ep, err := st.EnsurePerson(ctx, "ada@example.com", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, ep.Account.UserID); err != nil {
		t.Fatal(err)
	}

	got, err := st.PersonByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != PersonStatusDeactivated {
		t.Errorf("status = %q, want deactivated", got.Status)
	}
}

func TestPersonByEmailReportsNotFoundForNobody(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	if _, err := st.PersonByEmail(ctx, "nobody@example.com"); !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("err = %v, want ErrPersonNotFound", err)
	}
}

func TestPersonByEmailChangesNothing(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ep, err := st.EnsurePerson(ctx, "ada@example.com", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RedeemEnrollment(ctx, ep.Token.Plaintext, "phone"); err != nil {
		t.Fatal(err)
	}

	before := tablesSnapshot(ctx, t, pool)
	if _, err := st.PersonByEmail(ctx, "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PersonByEmail(ctx, "nobody@example.com"); !errors.Is(err, ErrPersonNotFound) {
		t.Fatal(err)
	}
	after := tablesSnapshot(ctx, t, pool)
	if before != after {
		t.Error("PersonByEmail changed the database")
	}
}

// ---------------------------------------------------------------------------
// Criterion 6 — a bot is out of reach, and the predicate is not redundant.
// Each guarding predicate this row adds is removed in turn (personGuardFault)
// and a test is watched failing, so no two of them cover for each other.

func TestPersonByEmailIgnoresABotEvenIfTheCheckConstraintEverFailed(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	dropBotHasNoEmailCheck(ctx, t, pool)
	if _, err := pool.Exec(ctx, `UPDATE users SET email = 'argosy@example.com' WHERE id = $1`, bot); err != nil {
		t.Fatalf("arrange (only possible because the CHECK was just dropped): %v", err)
	}

	if _, err := st.PersonByEmail(ctx, "argosy@example.com"); !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("a bot given an email surfaced as a person: %v", err)
	}
}

// Criterion 6's negative control for personLookupQuery's kind filter, shared
// by EnsurePerson and PersonByEmail. Provably redundant with the CHECK in
// ordinary operation (see personFaultLookupIgnoresKind's own comment), so this
// is the only way to show it is not dead code: drop the CHECK first, the one
// state its removal is written to survive.
func TestWithoutTheKindFilterPersonByEmailWouldSurfaceABot(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	dropBotHasNoEmailCheck(ctx, t, pool)
	if _, err := pool.Exec(ctx, `UPDATE users SET email = 'argosy@example.com' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}

	st.personGuardFault = personFaultLookupIgnoresKind
	got, err := st.PersonByEmail(ctx, "argosy@example.com")
	if err != nil {
		t.Fatalf("with the kind filter removed (and the CHECK already gone) the bot should have surfaced: %v", err)
	}
	if got.UserID != bot {
		t.Fatalf("resolved to %s, want the bot %s", got.UserID, bot)
	}
}

// WITH THE KIND FILTER INTACT (fault=none, the shipped default), EnsurePerson
// for an email a bot holds — reachable only via the same broken-CHECK
// arrangement — must never succeed by adopting the bot. It is allowed to fail
// outright: the bot's row already holds that email, users_email_lower_idx
// applies across every kind, and no retry can manufacture a second row
// holding the same address. What matters is which of the two failure modes
// happens, and it is never "the bot got a token".
func TestEnsurePersonNeverReturnsOrReissuesABot(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	dropBotHasNoEmailCheck(ctx, t, pool)
	if _, err := pool.Exec(ctx, `UPDATE users SET email = 'argosy@example.com' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}

	ep, err := st.EnsurePerson(ctx, "argosy@example.com", "Someone")
	if err == nil {
		if ep.Account.UserID == bot {
			t.Fatal("EnsurePerson returned the bot's own account as an existing person")
		}
		if ep.Outcome == PersonExisting {
			t.Fatal("EnsurePerson treated the bot's email as an existing person's re-invite")
		}
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM enrollment_tokens WHERE user_id = $1`, bot); n != 0 {
		t.Errorf("the bot has %d enrollment tokens, want 0 — EnsurePerson must never mint one for a bot", n)
	}
}

// Criterion 6's negative control for personLookupQuery's kind filter, from
// EnsurePerson's own call site. WHAT IT ACTUALLY SHOWS, RECORDED BECAUSE A
// FIRST DRAFT OF THIS TEST EXPECTED MORE: removing this ONE predicate does
// NOT let the bot's token redeem into a device — RedeemEnrollment (tokens.go)
// has its own, entirely independent "kind != person" refusal, predating this
// ticket, and it still catches it; two layers, and this fault removes only
// one. What the fault alone costs is EnsurePerson's OWN answer: it reports
// PersonExisting for a bot's account and mints it a real, if useless,
// enrollment token — which is still a defect worth a red test, just a
// smaller one than "a bot can enroll a device".
func TestWithoutTheKindFilterEnsurePersonWouldMisreportABotAsAnExistingPerson(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	dropBotHasNoEmailCheck(ctx, t, pool)
	if _, err := pool.Exec(ctx, `UPDATE users SET email = 'argosy@example.com' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}

	st.personGuardFault = personFaultLookupIgnoresKind
	ep, err := st.EnsurePerson(ctx, "argosy@example.com", "Someone")
	if err != nil {
		t.Fatalf("with the filter removed the bot should have been treated as an existing person: %v", err)
	}
	if ep.Account.UserID != bot || ep.Outcome != PersonExisting {
		t.Fatalf("got %+v, want the bot misreported as an existing person — the defect this fault exists to demonstrate", ep)
	}
	if _, err := st.RedeemEnrollment(ctx, ep.Token.Plaintext, "a device for a cron job"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a bot's mis-issued token redeemed into a device: %v — RedeemEnrollment's own kind check should still refuse it", err)
	}
}

func TestWithoutBotsInTheDedupComparisonACaseCollisionSlipsThrough(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	st.personGuardFault = personFaultDedupIgnoresBots
	if _, err := st.CreateBot(ctx, "Argosy", "Argosy"); err != nil {
		t.Fatal(err)
	}

	ep, err := st.EnsurePerson(ctx, "argosy@x.example", "Someone")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Account.Handle != "argosy" {
		t.Fatalf("with bots excluded from the dedup check, handle = %q, want the UNSUFFIXED argosy — "+
			"this is the collision the fault exists to demonstrate: argosy and Argosy now name different accounts",
			ep.Account.Handle)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM users WHERE handle = 'argosy'`); n != 1 {
		t.Fatalf("%d rows named exactly 'argosy', want 1 — the case-sensitive UNIQUE(handle) index does not catch this either", n)
	}
}

// ---------------------------------------------------------------------------
// catenary user set-email — the store half

// Found in review (#79): SetEmail's own lookup now locks the row it decides
// on, so two racing calls on one handle cannot both read "no email yet" and
// both report success — the TOCTOU that would otherwise silently re-key the
// account to whichever call committed second.
func TestConcurrentSetEmailsOnOneHandleRefuseTheLoser(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	mkUser(ctx, t, pool, "alice")

	emails := []string{"a@x.example", "b@y.example"}
	errs := make([]error, len(emails))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, email := range emails {
		wg.Add(1)
		go func(i int, email string) {
			defer wg.Done()
			<-start
			_, errs[i] = st.SetEmail(ctx, "alice", email)
		}(i, email)
	}
	close(start)
	wg.Wait()

	var succeeded, refused int
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrPersonAlreadyHasEmail):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("succeeded=%d refused=%d, want exactly one of each — a race must not let both report success", succeeded, refused)
	}
}

func TestSetEmailGivesAnEmaillessPersonOne(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	id := mkUser(ctx, t, pool, "alice")

	got, err := st.SetEmail(ctx, "alice", "Alice@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Errorf("returned %s, want %s", got, id)
	}

	lookup, err := st.PersonByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("the new email does not resolve: %v", err)
	}
	if lookup.UserID != id {
		t.Error("resolved to the wrong account")
	}
}

func TestSetEmailRefusesAnUnknownHandle(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	if _, err := st.SetEmail(ctx, "nobody", "x@example.com"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("err = %v, want ErrNoSuchUser", err)
	}
}

func TestSetEmailIsExactMatchOnHandle(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	mkUser(ctx, t, pool, "Alice")
	if _, err := st.SetEmail(ctx, "alice", "alice@example.com"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("a case-folded handle matched: %v, want ErrNoSuchUser — exact match only, like BotByHandle", err)
	}
}

func TestSetEmailRefusesABot(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	if _, err := st.CreateBot(ctx, "argosy", "Argosy"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.SetEmail(ctx, "argosy", "argosy@example.com"); !errors.Is(err, ErrCannotEmailBot) {
		t.Errorf("set-email on a bot = %v, want ErrCannotEmailBot", err)
	}
	var email *string
	if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE handle = 'argosy'`).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != nil {
		t.Error("the bot was given an email")
	}
}

// Criterion 6's negative control for SetEmail's own bot check. The CHECK
// constraint still stops the write — this is defence in depth, not the last
// line — so what removing the code-level guard costs is the clean refusal.
func TestWithoutItsOwnCheckSetEmailStillCannotEmailABotButStopsSayingSoCleanly(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	if _, err := st.CreateBot(ctx, "argosy", "Argosy"); err != nil {
		t.Fatal(err)
	}

	st.personGuardFault = personFaultSetEmailIgnoresBotCheck
	_, err := st.SetEmail(ctx, "argosy", "argosy@example.com")
	if err == nil {
		t.Fatal("a bot was given an email")
	}
	if errors.Is(err, ErrCannotEmailBot) {
		t.Fatalf("got the clean refusal even with the check removed — the control proves nothing: %v", err)
	}
	var email *string
	if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE handle = 'argosy'`).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != nil {
		t.Error("THE CHECK CONSTRAINT DID NOT HOLD — a bot was given an email with only the code-level refusal removed")
	}
}

func TestSetEmailRefusesAnEmailAlreadyHeldByAnyone(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	if _, err := st.EnsurePerson(ctx, "ada@example.com", "Ada"); err != nil {
		t.Fatal(err)
	}
	mkUser(ctx, t, pool, "bob")

	if _, err := st.SetEmail(ctx, "bob", "ADA@EXAMPLE.COM"); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("err = %v, want ErrEmailTaken", err)
	}
}

func TestSetEmailRefusesAPersonWhoAlreadyHasOne(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ep, err := st.EnsurePerson(ctx, "ada@example.com", "Ada")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.SetEmail(ctx, ep.Account.Handle, "someone-else@example.com"); !errors.Is(err, ErrPersonAlreadyHasEmail) {
		t.Errorf("err = %v, want ErrPersonAlreadyHasEmail", err)
	}
	got, err := st.PersonByEmail(ctx, "ada@example.com")
	if err != nil || got.UserID != ep.Account.UserID {
		t.Error("the original email no longer resolves — a refused set-email must not have touched it")
	}
}

func TestSetEmailRefusesAMalformedEmail(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	mkUser(ctx, t, pool, "alice")
	if _, err := st.SetEmail(ctx, "alice", "not-an-email"); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("err = %v, want ErrInvalidEmail", err)
	}
}
