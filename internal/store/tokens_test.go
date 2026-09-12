package store

// CANT-28 — the credential model's oracle.
//
// The plan's criteria are the contract and each test below names the one it
// discharges. Two criteria are NOT here and are not silently dropped: the
// refresh exchange's concurrency and a deactivated account's third door are
// CANT-97's, the sub-task of CANT-29 that ruling 5 puts the /refresh handler
// in — they are carried onto it verbatim, because the code they test does not
// exist in this ticket.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Criterion 3 — one encoding, and it has to survive a subprotocol header

// base64urlToken is the wire schema's `Token` pattern, restated here so the Go
// side is held to the same rule the TypeScript and Dart decoders are.
var base64urlToken = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// rfc7230Token is the grammar a Sec-WebSocket-Protocol value must satisfy:
// RFC 6455 says each subprotocol name is an RFC 7230 `token`.
//
// NOTE WHAT IS IN IT. `+` IS a legal token character; `/` and `=` are not. So
// "avoid the base64 specials" is the wrong rule — it is wrong about `+`, and a
// blocklist written from it would wave through a token carrying `/`. The
// contract is the ALPHABET above, which is strictly narrower than this
// grammar; this expression is here to show that the alphabet is a safe subset
// rather than a coincidence.
var rfc7230Token = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)

func TestEveryCredentialIsLegalAsASubprotocolValue(t *testing.T) {
	// Enough draws that a rare character class would show up. base64url over 32
	// random bytes has 64 possible characters per position, so 500 tokens is
	// ~21,500 characters and every legal symbol appears many times over.
	seen := map[string]bool{}
	for range 500 {
		plaintext, hash, err := MintToken()
		if err != nil {
			t.Fatalf("MintToken: %v", err)
		}
		if !base64urlToken.MatchString(plaintext) {
			t.Fatalf("token %q is not 43 characters of the base64url alphabet, which is what "+
				"the wire schema's Token pattern requires of every generated decoder", plaintext)
		}
		if !rfc7230Token.MatchString(plaintext) {
			t.Fatalf("token %q is not an RFC 7230 token, so it cannot ride "+
				"Sec-WebSocket-Protocol and an intermediary may mangle rather than refuse it",
				plaintext)
		}
		if len(hash) != 32 {
			t.Fatalf("hash is %d bytes, want a 32-byte sha256 digest", len(hash))
		}
		if seen[plaintext] {
			t.Fatalf("MintToken returned a duplicate in 500 draws: %q", plaintext)
		}
		seen[plaintext] = true
	}

	// The probes, because a pattern that has only ever seen legal input is a
	// pattern nobody has watched work. Both are one identifier away from the
	// right answer in Go, TypeScript and Dart alike: StdEncoding adds padding,
	// RawStdEncoding uses the `+/` alphabet.
	for _, tc := range []struct {
		name, token string
		rfc7230OK   bool
	}{
		{"StdEncoding, padded", "padded_std_encoding_FIXTURE_not_a_secret___=", false},
		// `+` and no `/`, deliberately: `+` is the half that IS a legal RFC 7230
		// token character, so this is the counterexample that matters.
		{"RawStdEncoding, + alphabet", "std+alphabet+FIXTURE+not+a+real+secret_____", true},
	} {
		if base64urlToken.MatchString(tc.token) {
			t.Errorf("%s: %q passes the alphabet check, so the pattern is asserting nothing",
				tc.name, tc.token)
		}
		// And the reason the alphabet rather than the grammar is the contract:
		// the RawStd case is a perfectly legal RFC 7230 token that would still
		// have been the wrong encoding — a check written against the grammar
		// alone would have let it through.
		if got := rfc7230Token.MatchString(tc.token); got != tc.rfc7230OK {
			t.Errorf("%s: rfc7230Token.MatchString(%q) = %v, want %v", tc.name, tc.token, got, tc.rfc7230OK)
		}
	}
}

func TestHashingIsDeterministicAndTheComparisonIsConstantTime(t *testing.T) {
	a := HashToken("abc")
	if b := HashToken("abc"); !tokensEqual(a, b) {
		t.Error("the same plaintext hashed to two different digests")
	}
	if tokensEqual(a, HashToken("abd")) {
		t.Error("two different plaintexts hashed to the same digest")
	}
}

// ---------------------------------------------------------------------------
// Criterion 2 — no token is stored in the clear, anywhere

// TestNoTokenColumnHoldsPlaintext reads the live schema rather than the SQL,
// so a column added by any future migration is covered without anybody
// remembering this test exists.
//
// THE RULE IS SHAPE, NOT SPELLING: any column whose name mentions a credential
// must be a BYTEA hash. A TEXT column called `token` would be a working
// credential sitting in a database backup, and it is the sort of thing that
// arrives as a debugging convenience and stays.
func TestNoTokenColumnHoldsPlaintext(t *testing.T) {
	ctx, pool := freshDB(t)

	rows, err := pool.Query(ctx, `
		SELECT table_name, column_name, data_type FROM information_schema.columns
		 WHERE table_schema = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	var table, column, dataType string
	if _, err := pgx.ForEachRow(rows, []any{&table, &column, &dataType}, func() error {
		if offence := plaintextCredentialOffence(table, column, dataType); offence != "" {
			t.Error(offence)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The probe. A guard that has only run against a clean schema is a guard
	// nobody has seen bite.
	for _, planted := range []struct{ table, column, dataType string }{
		{"access_tokens", "token", "text"},
		{"refresh_tokens", "secret", "text"},
		{"enrollment_tokens", "token_hash", "text"},
	} {
		if plaintextCredentialOffence(planted.table, planted.column, planted.dataType) == "" {
			t.Errorf("a planted %s.%s %s did not trip the guard — it reads nothing",
				planted.table, planted.column, planted.dataType)
		}
	}
	// And it must not fire on something innocent, or it will be deleted the
	// first time it cries wolf.
	if o := plaintextCredentialOffence("messages", "text", "text"); o != "" {
		t.Errorf("the guard fired on messages.text: %s", o)
	}
}

func plaintextCredentialOffence(table, column, dataType string) string {
	lower := strings.ToLower(column)
	credentialish := strings.Contains(lower, "token") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "password") || strings.Contains(lower, "credential")
	if !credentialish {
		return ""
	}
	if !strings.HasSuffix(lower, "_hash") {
		return fmt.Sprintf("%s.%s names a credential and is not a _hash column — "+
			"a plaintext credential in a table is a working credential in every backup", table, column)
	}
	if dataType != "bytea" {
		return fmt.Sprintf("%s.%s is %s; a hash column is bytea, and a text one invites "+
			"somebody to put the token in it", table, column, dataType)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Criteria 4, 9 — redemption is single-use, concurrently, and auditable after

func TestRedemptionMintsOneDeviceAndOnePairAndIsAuditableAfter(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")

	issued, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	got, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel 8 Pro")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if got.UserID != user {
		t.Errorf("redeemed as %s, want %s", got.UserID, user)
	}
	if got.Access.Plaintext == "" || got.Refresh.Plaintext == "" {
		t.Fatal("redemption returned an empty credential")
	}
	if got.Access.Plaintext == got.Refresh.Plaintext {
		t.Error("the access and refresh tokens are the same string")
	}
	if !got.Access.ExpiresAt.After(ServerTime()) || !got.Refresh.ExpiresAt.After(got.Access.ExpiresAt) {
		t.Errorf("expiries are wrong: access %v, refresh %v", got.Access.ExpiresAt, got.Refresh.ExpiresAt)
	}

	if n := countRows(ctx, t, pool, `SELECT count(*) FROM devices WHERE user_id = $1`, user); n != 1 {
		t.Errorf("%d device rows, want exactly 1", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM refresh_tokens WHERE device_id = $1`, got.DeviceID); n != 1 {
		t.Errorf("%d refresh tokens, want exactly 1", n)
	}

	// Criterion 9: the row stays, and says what became of it.
	var redeemedAt *time.Time
	var redeemedBy *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT redeemed_at, redeemed_by_device FROM enrollment_tokens WHERE user_id = $1`, user).
		Scan(&redeemedAt, &redeemedBy); err != nil {
		t.Fatalf("the enrollment token row is gone — a redemption that leaves no row cannot be audited: %v", err)
	}
	if redeemedAt == nil {
		t.Error("redeemed_at is NULL on a redeemed token")
	}
	if redeemedBy == nil || *redeemedBy != got.DeviceID {
		t.Errorf("redeemed_by_device = %v, want %s", redeemedBy, got.DeviceID)
	}

	// The first token of a family is its own family, so CANT-29's invalidation
	// is one predicate rather than a walk.
	var id, family uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id, family_id FROM refresh_tokens WHERE device_id = $1`, got.DeviceID).
		Scan(&id, &family); err != nil {
		t.Fatal(err)
	}
	if id != family {
		t.Errorf("family_id = %s, want the row's own id %s", family, id)
	}
}

// TestTwoSimultaneousRedemptionsOfOneTokenProduceExactlyOneDevice is criterion
// 4's real half. Sequentially, `redeemed_at IS NOT NULL` refuses the second
// attempt on any implementation; the interesting case is two that read the row
// before either has written it, which is what FOR UPDATE is there for.
func TestTwoSimultaneousRedemptionsOfOneTokenProduceExactlyOneDevice(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")

	issued, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	const racers = 8
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	results := make([]error, racers)
	enrollments := make([]Enrollment, racers)
	for i := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			enrollments[i], results[i] = st.RedeemEnrollment(ctx, issued.Plaintext, "racer")
		}()
	}
	start.Done()
	done.Wait()

	var winners, refusals int
	for i, err := range results {
		switch {
		case err == nil:
			winners++
			if enrollments[i].DeviceID == uuid.Nil {
				t.Error("a winning redemption returned no device")
			}
		case errors.Is(err, ErrUnauthorized):
			refusals++
		default:
			t.Errorf("racer %d failed with an unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Errorf("%d of %d concurrent redemptions succeeded, want exactly 1 — "+
			"a second device on one enrollment token is an enrollment nobody authorised",
			winners, racers)
	}
	if refusals != racers-1 {
		t.Errorf("%d refusals, want %d", refusals, racers-1)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM devices WHERE user_id = $1`, user); n != 1 {
		t.Errorf("%d device rows after the race, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Criterion 5 — redemption takes no lock on users, and the race closes at
// authentication rather than at enrollment

func TestRedemptionDoesNotLockUsersAndTheRaceClosesAtAuthentication(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")
	issued, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A deactivation in flight, holding exactly the lock messages.go:66 says
	// it takes: FOR NO KEY UPDATE on the user row.
	deactivation, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deactivation.Rollback(ctx) }()
	var held uuid.UUID
	if err := deactivation.QueryRow(ctx,
		`SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, user).Scan(&held); err != nil {
		t.Fatalf("hold the user row: %v", err)
	}

	// Redemption must COMPLETE while that is held. If it took FOR UPDATE on
	// users — the lock messages.go's note says nothing here may take — it would
	// block until the deactivation committed, and this would time out.
	type result struct {
		e   Enrollment
		err error
	}
	ch := make(chan result, 1)
	go func() {
		e, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel 8 Pro")
		ch <- result{e, err}
	}()

	var got result
	select {
	case got = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("redemption blocked while a FOR NO KEY UPDATE was held on the user row — " +
			"it is taking a stronger lock on users than messages.go:66 allows, and the " +
			"deadlock that argument rules out is back")
	}
	if got.err != nil {
		t.Fatalf("redeem: %v", got.err)
	}

	// Now let the deactivation land. Both committed, which is the accepted
	// outcome: a stray device row and no access.
	if _, err := deactivation.Exec(ctx,
		`UPDATE users SET deactivated_at = now() WHERE id = $1`, user); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if err := deactivation.Commit(ctx); err != nil {
		t.Fatalf("commit deactivation: %v", err)
	}

	if n := countRows(ctx, t, pool, `SELECT count(*) FROM devices WHERE id = $1`, got.e.DeviceID); n != 1 {
		t.Fatalf("the enrolled device row is missing; the race was supposed to leave it behind")
	}
	if _, err := st.Authenticate(ctx, got.e.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a device enrolled into a now-deactivated account authenticated: err = %v. "+
			"The race is closed at authentication, not at enrollment — that is R6's own argument "+
			"and it only holds if this refuses", err)
	}
}

// ---------------------------------------------------------------------------
// Criterion 6 — expiry is enforced on every shape that has one

func TestAnExpiredCredentialIsRefused(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")

	t.Run("enrollment token", func(t *testing.T) {
		issued, err := st.IssueEnrollmentToken(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE enrollment_tokens SET expires_at = now() - interval '1 second' WHERE user_id = $1`,
			user); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel"); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("an expired enrollment token was redeemed: err = %v. expires_at is in the "+
				"migration, and a column nothing checks reads as protection it does not provide", err)
		}
	})

	t.Run("access token", func(t *testing.T) {
		issued, err := st.IssueEnrollmentToken(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		e, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Authenticate(ctx, e.Access.Plaintext); err != nil {
			t.Fatalf("a fresh access token was refused: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE access_tokens SET expires_at = now() - interval '1 second' WHERE device_id = $1`,
			e.DeviceID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Authenticate(ctx, e.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("an expired access token authenticated: err = %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Criterion 7 — a deactivated user is refused at every door this ticket builds
//
// THE THIRD DOOR IS NOT HERE AND IS NOT DROPPED. "Their device cannot exchange
// a refresh token" is CANT-97's, because ruling 5 put the /refresh handler
// there; the criterion is carried onto that ticket verbatim. What follows is
// the two doors this ticket owns, and each is arranged so that ONLY
// users.deactivated_at can be doing the work: the credential is live, the
// device is un-revoked, nothing has expired.

func TestADeactivatedUserIsRefusedWithEverythingElseInOrder(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	t.Run("their access token stops working", func(t *testing.T) {
		user := mkUser(ctx, t, pool, "grace")
		issued, err := st.IssueEnrollmentToken(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		e, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Authenticate(ctx, e.Access.Plaintext); err != nil {
			t.Fatalf("precondition: the token should work before deactivation: %v", err)
		}

		if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, user); err != nil {
			t.Fatal(err)
		}
		// Everything else is deliberately still in order.
		assertLive(ctx, t, pool, e)

		if _, err := st.Authenticate(ctx, e.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("a deactivated account authenticated: err = %v", err)
		}
	})

	t.Run("their enrollment token cannot be redeemed", func(t *testing.T) {
		user := mkUser(ctx, t, pool, "hopper")
		issued, err := st.IssueEnrollmentToken(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, user); err != nil {
			t.Fatal(err)
		}
		// The token itself is live: unredeemed, unsuperseded, unexpired.
		var redeemed, superseded *time.Time
		var expires time.Time
		if err := pool.QueryRow(ctx,
			`SELECT redeemed_at, superseded_at, expires_at FROM enrollment_tokens WHERE user_id = $1`, user).
			Scan(&redeemed, &superseded, &expires); err != nil {
			t.Fatal(err)
		}
		if redeemed != nil || superseded != nil || !expires.After(ServerTime()) {
			t.Fatal("precondition: the enrollment token must be live, or this proves nothing")
		}

		if _, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel"); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("a deactivated account enrolled a device: err = %v.\n"+
				"R6 chose disable-then-revoke on the strength of this exact sentence — "+
				"\"a disabled account cannot refresh a token or enroll a device\" — so the "+
				"offboard fails open without it", err)
		}
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM devices WHERE user_id = $1`, user); n != 0 {
			t.Errorf("%d device rows for a deactivated account, want 0", n)
		}
	})
}

// assertLive fails unless every reason to refuse EXCEPT the one under test is
// absent. Without it a passing test proves only that something said no.
func assertLive(ctx context.Context, t *testing.T, pool *pgxpool.Pool, e Enrollment) {
	t.Helper()
	var tokenRevoked, deviceRevoked *time.Time
	var expires *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT a.revoked_at, a.expires_at, d.revoked_at
		  FROM access_tokens a JOIN devices d ON d.id = a.device_id
		 WHERE a.device_id = $1`, e.DeviceID).Scan(&tokenRevoked, &expires, &deviceRevoked); err != nil {
		t.Fatal(err)
	}
	switch {
	case tokenRevoked != nil:
		t.Fatal("precondition: the access token is revoked, so this test proves nothing")
	case deviceRevoked != nil:
		t.Fatal("precondition: the device is revoked, so this test proves nothing")
	case expires == nil || !expires.After(ServerTime()):
		t.Fatal("precondition: the access token has expired, so this test proves nothing")
	}
}

// ---------------------------------------------------------------------------
// Criterion 10 — R6's re-invite case

func TestReIssuingSupersedesAndLeavesExactlyOneRedeemable(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")

	first, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatalf("re-issue: %v — Provision must be able to re-invite, which R6 classes "+
			"as a rotation Provision may perform", err)
	}

	if n := countRows(ctx, t, pool, `
		SELECT count(*) FROM enrollment_tokens
		 WHERE user_id = $1 AND redeemed_at IS NULL AND superseded_at IS NULL`, user); n != 1 {
		t.Errorf("%d redeemable tokens after a re-invite, want exactly 1 — "+
			"this is the case Lyceum's connector gets wrong, and it gets it wrong by "+
			"leaving the old one live", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM enrollment_tokens WHERE user_id = $1`, user); n != 2 {
		t.Errorf("%d rows total, want 2 — the superseded token stays for the audit trail", n)
	}

	if _, err := st.RedeemEnrollment(ctx, first.Plaintext, "old"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("the superseded token still redeems: err = %v", err)
	}
	if _, err := st.RedeemEnrollment(ctx, second.Plaintext, "new"); err != nil {
		t.Errorf("the current token does not redeem: %v", err)
	}
}

// TestThePartialIndexRefusesASecondLiveToken proves the belt as well as the
// braces: IssueEnrollmentToken supersedes, and the database would refuse it if
// a future caller forgot to.
func TestThePartialIndexRefusesASecondLiveToken(t *testing.T) {
	ctx, pool := freshDB(t)
	user := mkUser(ctx, t, pool, "ada")

	insert := func() error {
		_, hash, err := MintToken()
		if err != nil {
			return err
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO enrollment_tokens (id, user_id, token_hash, expires_at)
			VALUES ($1, $2, $3, now() + interval '7 days')`, uuid.New(), user, hash)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Error("a second live enrollment token was accepted — the partial unique index is not " +
			"doing its job, and forgetting to supersede would be invisible until two devices enrolled")
	}
}

// ---------------------------------------------------------------------------
// Criterion 11 — every refusal is logged, granularly, and never with the token

func TestEveryRedemptionRefusalLogsItsReasonAndNeverTheCredential(t *testing.T) {
	ctx, pool := freshDB(t)
	user := mkUser(ctx, t, pool, "ada")

	// One store per case so the buffer holds only that case's lines.
	newCase := func() (*Store, func() []map[string]any) {
		logger, buf := captureLogger()
		st := New(pool, DefaultLimits(), logger)
		return st, func() []map[string]any { return logLines(t, buf) }
	}

	type probe struct {
		name       string
		wantReason string
		arrange    func(t *testing.T, st *Store) string // returns the token to present
	}
	probes := []probe{
		{"unknown", "unknown token", func(t *testing.T, st *Store) string {
			plaintext, _, err := MintToken()
			if err != nil {
				t.Fatal(err)
			}
			return plaintext
		}},
		{"expired", "expired", func(t *testing.T, st *Store) string {
			issued, err := st.IssueEnrollmentToken(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx,
				`UPDATE enrollment_tokens SET expires_at = now() - interval '1 second'
				  WHERE user_id = $1 AND redeemed_at IS NULL AND superseded_at IS NULL`, user); err != nil {
				t.Fatal(err)
			}
			return issued.Plaintext
		}},
		{"already redeemed", "already redeemed", func(t *testing.T, st *Store) string {
			issued, err := st.IssueEnrollmentToken(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.RedeemEnrollment(ctx, issued.Plaintext, "first"); err != nil {
				t.Fatal(err)
			}
			return issued.Plaintext
		}},
		{"superseded", "superseded", func(t *testing.T, st *Store) string {
			issued, err := st.IssueEnrollmentToken(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.IssueEnrollmentToken(ctx, user); err != nil {
				t.Fatal(err)
			}
			return issued.Plaintext
		}},
		{"deactivated", "account deactivated", func(t *testing.T, st *Store) string {
			victim := mkUser(ctx, t, pool, "victim-"+uuid.NewString()[:8])
			issued, err := st.IssueEnrollmentToken(ctx, victim)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx,
				`UPDATE users SET deactivated_at = now() WHERE id = $1`, victim); err != nil {
				t.Fatal(err)
			}
			return issued.Plaintext
		}},
	}

	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			st, lines := newCase()
			token := p.arrange(t, st)

			// The arrangement above may log; only what happens next is asserted,
			// so take a mark.
			before := len(lines())

			if _, err := st.RedeemEnrollment(ctx, token, "Pixel"); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("refusal = %v, want ErrUnauthorized", err)
			}

			after := lines()[before:]
			var reasons []string
			for _, l := range after {
				if r, ok := l["reason"].(string); ok {
					reasons = append(reasons, r)
				}
				// THE TOKEN IS NEVER IN A LOG LINE, at any level, in any field.
				// This is what makes "visibility substitutes for the rate
				// limiter" a defensible trade instead of a leak.
				for k, v := range l {
					if s, ok := v.(string); ok && strings.Contains(s, token) {
						t.Errorf("the log line put the credential in field %q", k)
					}
				}
			}
			if len(reasons) != 1 || reasons[0] != p.wantReason {
				t.Errorf("logged reasons = %v, want exactly [%q]. The response says one thing "+
					"for all five; the log is where they are told apart", reasons, p.wantReason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Criterion 13 — revocation takes effect on the next request, for both shapes

func TestARevokedDeviceIsRefusedOnItsNextRequest(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")
	issued, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Authenticate(ctx, e.Access.Plaintext); err != nil {
		t.Fatalf("precondition: %v", err)
	}

	revoked, err := st.RevokeDevice(ctx, e.DeviceID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !revoked {
		t.Fatal("RevokeDevice reported nothing to do on a live device")
	}
	if _, err := st.Authenticate(ctx, e.Access.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a revoked device authenticated: err = %v. Ruling 0 buys immediate revocation "+
			"precisely by reading the device row on every request", err)
	}

	// Idempotent: R6's Deprovision fans out and may half-succeed, so the retry
	// that mops up must not report work it did not do.
	again, err := st.RevokeDevice(ctx, e.DeviceID)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if again {
		t.Error("a second revoke reported a change; a retry finding its work done is not a change")
	}
	if unknown, err := st.RevokeDevice(ctx, uuid.New()); err != nil || unknown {
		t.Errorf("revoking an unknown device = (%v, %v), want (false, nil)", unknown, err)
	}
}

func TestARevokedBotTokenIsRefusedRatherThanSilentlyDropped(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	bot := mkUser(ctx, t, pool, "argosy-bot")
	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'bot' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}
	token, err := st.IssueBotToken(ctx, bot)
	if err != nil {
		t.Fatalf("issue bot token: %v", err)
	}

	caller, err := st.Authenticate(ctx, token.Plaintext)
	if err != nil {
		t.Fatalf("a fresh bot token was refused: %v", err)
	}
	if !caller.IsBot() {
		t.Errorf("caller.Kind = %q, want bot", caller.Kind)
	}
	if caller.DeviceID != uuid.Nil {
		t.Errorf("a bot resolved to device %s; a bot has no device and must not pretend to", caller.DeviceID)
	}
	// Long-lived means no expiry, and 0007's CHECK is what keeps that shape
	// from reaching a person.
	if !token.ExpiresAt.IsZero() {
		t.Errorf("a bot token expires at %v; it is non-rotating and long-lived because a cron "+
			"job restarted from a snapshot cannot be expected to have rotated", token.ExpiresAt)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE access_tokens SET revoked_at = now() WHERE user_id = $1`, bot); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Authenticate(ctx, token.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a revoked bot token authenticated: err = %v. The recorded ruling asks for "+
			"`unauthorized` rather than a 200 that quietly drops the message", err)
	}
}

func TestAPersonCannotBeIssuedANonExpiringToken(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	person := mkUser(ctx, t, pool, "ada")

	if _, err := st.IssueBotToken(ctx, person); err == nil {
		t.Error("a person was issued a bot token. 0007's CHECK cannot see users.kind, so this " +
			"half of the shape is a store invariant — and without it a person holds a credential " +
			"that never expires")
	}

	// And the database refuses the shape directly, so the store is not the only
	// thing standing between a person and a permanent credential.
	device := mkDevice(ctx, t, pool, person, "Pixel")
	_, hash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO access_tokens (id, token_hash, user_id, device_id, expires_at)
		VALUES ($1, $2, $3, $4, NULL)`, uuid.New(), hash, person, device); err == nil {
		t.Error("a device-scoped access token with no expiry was accepted; 0007's CHECK ties " +
			"the two together precisely so that fifteen minutes cannot become forever")
	}
}

// ---------------------------------------------------------------------------
// Criterion 15 — the revocation publish, and the channel it is not on

func TestRevocationPublishesInsideItsOwnTransaction(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")
	device := mkDevice(ctx, t, pool, user, "Pixel")

	got := make(chan RevocationPayload, 4)
	l := &Listener[RevocationPayload]{
		DSN: testDSN(t), Channel: RevocationChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p RevocationPayload) { got <- p },
	}
	l.OnGap = func(context.Context) {}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = l.Run(runCtx) }()
	// The listener has to be subscribed before the revocation commits, or this
	// is testing nothing — Postgres queues nothing for a listener that was not
	// there at the time, which is the same property OnGap exists for.
	sentinel, isSentinel := revocationSentinel()
	awaitSubscribed(ctx, t, pool, RevocationChannel, got, sentinel, isSentinel)

	if _, err := st.RevokeDevice(ctx, device); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	select {
	case p := <-got:
		if p.DeviceID == nil || *p.DeviceID != device {
			t.Errorf("payload device = %v, want %s", p.DeviceID, device)
		}
		if p.UserID != nil {
			t.Errorf("payload named a user subject on a device revocation: %v", p.UserID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification for a committed revocation — CANT-30 would sever nothing")
	}
}

func TestARolledBackRevocationPublishesNothing(t *testing.T) {
	ctx, pool := freshDB(t)
	user := mkUser(ctx, t, pool, "ada")
	device := mkDevice(ctx, t, pool, user, "Pixel")

	got := make(chan RevocationPayload, 4)
	l := &Listener[RevocationPayload]{
		DSN: testDSN(t), Channel: RevocationChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p RevocationPayload) { got <- p },
		OnGap:    func(context.Context) {},
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = l.Run(runCtx) }()
	sentinel, isSentinel := revocationSentinel()
	awaitSubscribed(ctx, t, pool, RevocationChannel, got, sentinel, isSentinel)

	// The same shape RevokeDevice has, abandoned. Postgres delivers a NOTIFY at
	// COMMIT, so the rollback is what must silence it — this is CANT-18's
	// ruling 2 applied to a second event, and the property only holds because
	// the pg_notify is inside the transaction rather than after it.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET revoked_at = now() WHERE id = $1`, device); err != nil {
		t.Fatal(err)
	}
	payload, err := RevocationPayload{DeviceID: &device}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, RevocationChannel, payload); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// A positive control, so "nothing arrived" is not just a slow listener.
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, RevocationChannel,
		`{"device_id":"00000000-0000-4000-8000-000000000001"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if p.DeviceID == nil || p.DeviceID.String() != "00000000-0000-4000-8000-000000000001" {
			t.Errorf("the rolled-back revocation was delivered: %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the control notification never arrived; this test proved nothing")
	}

	var revoked *time.Time
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM devices WHERE id = $1`, device).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != nil {
		t.Error("the rolled-back revocation is in the table")
	}
}

// TestAMessageListenerNeverSeesARevocation is the reason ruling 7 chose a
// second channel over a second shape on the first.
//
// The listener decodes with a plain json.Unmarshal, which ignores unknown
// fields and zeroes absent ones. On ONE channel, a revocation would not reach
// the "did not parse" branch at all: it would decode to NotifyPayload{Nil, 0}
// and be handed to OnNotify as a message notification for the nil conversation
// at seq 0. Nothing would log, and nothing would fail.
func TestAMessageListenerNeverSeesARevocation(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	user := mkUser(ctx, t, pool, "ada")
	device := mkDevice(ctx, t, pool, user, "Pixel")

	onMessageChannel := make(chan NotifyPayload, 4)
	l := &Listener[NotifyPayload]{
		DSN: testDSN(t), Channel: NotifyChannel, Logger: discardLogger(),
		OnNotify: func(_ context.Context, p NotifyPayload) { onMessageChannel <- p },
		OnGap:    func(context.Context) {},
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = l.Run(runCtx) }()
	awaitSubscribed(ctx, t, pool, NotifyChannel, onMessageChannel,
		`{"conversation_id":"`+sentinelDevice+`","seq":99}`,
		func(p NotifyPayload) bool { return p.ConversationID.String() == sentinelDevice && p.Seq == 99 })

	if _, err := st.RevokeDevice(ctx, device); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// A real message notification afterwards, as the control: if THIS arrives
	// and the revocation did not, the separation holds.
	conv := uuid.New()
	payload, err := NotifyPayload{ConversationID: conv, Seq: 7}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, payload); err != nil {
		t.Fatal(err)
	}

	select {
	case p := <-onMessageChannel:
		switch {
		case p.ConversationID == uuid.Nil && p.Seq == 0:
			t.Fatal("the message listener received a zero-valued payload — a revocation " +
				"decoded as a message notification for the nil conversation at seq 0, which " +
				"is the silent failure ruling 7 exists to avoid")
		case p.ConversationID != conv || p.Seq != 7:
			t.Fatalf("unexpected payload on the message channel: %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the control message notification never arrived; this test proved nothing")
	}
}

// awaitSubscribed proves that the listener feeding ch is actually subscribed,
// by round-tripping a sentinel through Postgres until one comes back.
//
// COUNTING LISTENERS IS NOT ENOUGH, and the difference is a false pass rather
// than a flake. waitForListeners counts sessions running LISTEN across the
// whole database, so a listener left over from an earlier test in the same
// package satisfies it instantly — and a test that then publishes into the
// window before its OWN listener subscribed sees nothing and calls that the
// result. That is exactly the shape TestAMessageListenerNeverSeesARevocation
// is asserting, so it would have passed for the wrong reason.
func awaitSubscribed[P any](ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	channel string, ch <-chan P, sentinel string, isSentinel func(P) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, sentinel); err != nil {
			t.Fatalf("sentinel notify on %s: %v", channel, err)
		}
		select {
		case p := <-ch:
			if isSentinel(p) {
				// Drain anything else that was already queued behind it, so the
				// test starts from a known-empty channel.
				for {
					select {
					case <-ch:
					default:
						return
					}
				}
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("no listener picked up a sentinel on %s within the deadline", channel)
}

const sentinelDevice = "00000000-0000-4000-8000-0000000000ff"

func revocationSentinel() (string, func(RevocationPayload) bool) {
	return `{"device_id":"` + sentinelDevice + `"}`,
		func(p RevocationPayload) bool { return p.DeviceID != nil && p.DeviceID.String() == sentinelDevice }
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
