package store

// CANT-73 — the credential that is not a device.
//
// THE CLAIMS WITH NO ORACLE BEFORE THIS FILE are the two refusals. CANT-28
// built the bot shape and tokens_test.go proves a revoked bot token is refused,
// but nothing anywhere asserted that a bot cannot enroll a device or refresh —
// both of which are `Done when` clauses, and both of which are one forgotten
// predicate away from being false.
//
// The other half is the new write: RevokeBotTokens is the first path in this
// service that ends a credential WITHOUT a device to name, so the test that
// matters most is that it cannot reach a person's.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreateBotMakesAServiceAccountAndDefaultsItsName(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	id, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM users WHERE id = $1 AND kind = 'bot'`, id); n != 1 {
		t.Error("the row was not created as kind = 'bot'; IssueBotToken reads that column and " +
			"would refuse to mint")
	}

	// An operator who gives no name gets the handle rather than a refusal.
	if _, err := st.CreateBot(ctx, "sre-agent", ""); err != nil {
		t.Fatalf("create bot with no display name: %v", err)
	}
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT display_name FROM users WHERE handle = 'sre-agent'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "sre-agent" {
		t.Errorf("display_name = %q, want the handle — the column is NOT NULL and renders beside "+
			"the bot's messages", name)
	}
}

// THE ONE THAT MATTERS ON THIS PATH. `kind = 'bot'` is what lets IssueBotToken
// mint a credential that never expires, so a create that upserted would be a
// route from "typed a handle somebody already has" to "handed a person's
// account a non-expiring token".
func TestCreateBotRefusesATakenHandleAndLeavesThatAccountAlone(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")

	id, err := st.CreateBot(ctx, "ada", "Definitely A Bot")
	if !errors.Is(err, ErrHandleTaken) {
		t.Fatalf("create bot on a taken handle = (%v, %v), want ErrHandleTaken", id, err)
	}

	var kind, display string
	if err := pool.QueryRow(ctx,
		`SELECT kind, display_name FROM users WHERE id = $1`, ada).Scan(&kind, &display); err != nil {
		t.Fatal(err)
	}
	if kind != "person" {
		t.Fatal("A PERSON WAS CONVERTED INTO A BOT. The conflict arm must do nothing rather than " +
			"update kind: a bot may hold a token that never expires, and a person must never be " +
			"handed one by an operator retyping a handle")
	}
	if display == "Definitely A Bot" {
		t.Error("the existing account's display_name was overwritten by a refused create")
	}
}

func TestBotByHandleFindsBotsAndNotPeople(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	mkUser(ctx, t, pool, "ada")
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.BotByHandle(ctx, "argosy")
	if err != nil {
		t.Fatalf("by handle: %v", err)
	}
	if got != bot {
		t.Errorf("by handle = %s, want %s", got, bot)
	}
	// A stray space is an operator's typing, not a different bot.
	if got, err := st.BotByHandle(ctx, "  argosy  "); err != nil || got != bot {
		t.Errorf("a padded handle = (%v, %v), want the bot", got, err)
	}
	if _, err := st.BotByHandle(ctx, "ada"); !errors.Is(err, ErrNoSuchBot) {
		t.Errorf("a person's handle = %v, want ErrNoSuchBot — the kind predicate is in the WHERE "+
			"clause so a person never comes back as a revocable subject", err)
	}
	if _, err := st.BotByHandle(ctx, "nobody"); !errors.Is(err, ErrNoSuchBot) {
		t.Errorf("an unknown handle = %v, want ErrNoSuchBot", err)
	}
}

// The `Done when` clause, end to end: revocation takes effect on the NEXT
// request, because Authenticate reads access_tokens.revoked_at every time.
func TestRevokingABotsTokensStopsItOnItsNextRequest(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.IssueBotToken(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Authenticate(ctx, token.Plaintext); err != nil {
		t.Fatalf("precondition: a fresh bot token was refused: %v", err)
	}

	n, err := st.RevokeBotTokens(ctx, bot)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 1 {
		t.Errorf("revoked %d tokens, want 1", n)
	}
	if _, err := st.Authenticate(ctx, token.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a revoked bot token still authenticates: %v", err)
	}

	// Idempotent: a retry finding its work done is success, not a failure.
	again, err := st.RevokeBotTokens(ctx, bot)
	if err != nil {
		t.Errorf("second revoke errored: %v", err)
	}
	if again != 0 {
		t.Errorf("second revoke reported %d, want 0 — a no-op must not re-date the first", again)
	}
}

// Re-minting is how a bot token rotates, so "revoke the bot" must mean every
// live credential rather than the newest one.
func TestRevokingABotEndsEveryLiveTokenItHolds(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}

	var tokens []string
	for i := 0; i < 3; i++ {
		issued, err := st.IssueBotToken(ctx, bot)
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		tokens = append(tokens, issued.Plaintext)
	}

	n, err := st.RevokeBotTokens(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("revoked %d tokens, want 3 — a rotation leaves the old one live on purpose, and "+
			"revoking must not stop at the newest", n)
	}
	for i, tok := range tokens {
		if _, err := st.Authenticate(ctx, tok); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("token %d survived the revoke: %v", i, err)
		}
	}
}

// THE SECURITY-CRITICAL ONE FOR THIS WRITE. RevokeBotTokens takes a user id,
// and a person's id is the same shape. Two predicates keep it away from them.
func TestRevokingBotTokensCannotReachAPersonsCredentials(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	e := enrolled(ctx, t, st, pool, "ada")

	n, err := st.RevokeBotTokens(ctx, e.UserID)
	if err != nil {
		t.Fatalf("revoke against a person: %v", err)
	}
	if n != 0 {
		t.Errorf("revoked %d of a person's tokens, want 0", n)
	}
	if _, err := st.Authenticate(ctx, e.Access.Plaintext); err != nil {
		t.Fatal("A PERSON'S ACCESS TOKEN WAS REVOKED by the bot path. `u.kind = 'bot'` and " +
			"`device_id IS NULL` are both in the UPDATE's WHERE clause precisely so a user id " +
			"alone cannot end a person's session")
	}
	if _, err := st.RotateRefresh(ctx, e.Refresh.Plaintext); err != nil {
		t.Errorf("the person's refresh token was affected too: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The two refusals — `Done when` clauses that had no oracle before this file.

// A bot has no device and must not acquire one. Purser never issues a bot an
// enrollment token, so this is the belt to that braces.
func TestABotCannotEnrollADevice(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}

	// IssueEnrollmentToken does not read kind — the refusal is at redemption,
	// which is the moment a device row would be written.
	issued, err := st.IssueEnrollmentToken(ctx, bot)
	if err != nil {
		t.Fatalf("issue enrollment token to a bot: %v", err)
	}
	if _, err := st.RedeemEnrollment(ctx, issued.Plaintext, "Pixel 8 Pro"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a bot redeemed an enrollment token: %v. A bot has no device and must not "+
			"pretend to — a device would give it a refresh token, and reuse detection would "+
			"then revoke a cron job's family the first time it restarted from a snapshot", err)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM devices WHERE user_id = $1`, bot); n != 0 {
		t.Errorf("%d device rows exist for a bot, want 0", n)
	}
}

// A bot token is long-lived and non-rotating. Presenting it to the rotation
// path must be refused rather than mint it a refresh token.
func TestABotTokenCannotBeRotated(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.IssueBotToken(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.RotateRefresh(ctx, token.Plaintext); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a bot token was accepted by the rotation path: %v. It is not a refresh token, "+
			"and a bot that could rotate would be a bot that could be locked out by reuse "+
			"detection on a restart", err)
	}
	// And it still works afterwards: a refused rotation must not spend it.
	if _, err := st.Authenticate(ctx, token.Plaintext); err != nil {
		t.Errorf("the bot token stopped working after a refused rotation: %v", err)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM refresh_tokens`); n != 0 {
		t.Errorf("%d refresh tokens exist after a bot tried to rotate, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Listing

func TestBotsListsServiceAccountsWithTheirLiveTokenCount(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	mkUser(ctx, t, pool, "ada") // a person, which must not appear

	argosy, err := st.CreateBot(ctx, "argosy", "Argosy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBot(ctx, "sre-agent", "SRE"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.IssueBotToken(ctx, argosy); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.Bots(ctx)
	if err != nil {
		t.Fatalf("bots: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d bots listed, want 2 — a person is not a service account", len(got))
	}
	byHandle := map[string]BotRow{}
	for _, b := range got {
		byHandle[b.Handle] = b
		if b.Handle == "ada" {
			t.Fatal("a person is on the service-account list")
		}
	}
	if n := byHandle["argosy"].LiveTokens; n != 2 {
		t.Errorf("argosy has %d live tokens, want 2", n)
	}
	if n := byHandle["sre-agent"].LiveTokens; n != 0 {
		t.Errorf("a bot with no token minted has %d live tokens, want 0", n)
	}

	// The count is derived, so revoking moves it with no second write.
	if _, err := st.RevokeBotTokens(ctx, argosy); err != nil {
		t.Fatal(err)
	}
	got, err = st.Bots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got {
		if b.Handle == "argosy" && b.LiveTokens != 0 {
			t.Errorf("argosy still shows %d live tokens after a revoke — the count is a COUNT, "+
				"not a stored number that can disagree with the rows", b.LiveTokens)
		}
	}
}

// A bot id that names nothing is zero rather than an error, on RevokeDevice's
// own contract: the caller is told what changed by the number, not by a failure.
func TestRevokingAnUnknownBotIsZeroRatherThanAnError(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())

	n, err := st.RevokeBotTokens(ctx, uuid.New())
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("revoked %d, want 0", n)
	}
}
