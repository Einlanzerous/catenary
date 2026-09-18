package main

// CANT-73 — the operator surface, through the real router setup() builds.
//
// The store's own tests are the oracle for the credential semantics. What this
// file adds is the half that only exists at the CLI: that the token reaches
// stdout ALONE, so `catenary bot create argosy > token` is a safe thing to
// type, and that the credential it prints is one the real server accepts.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// capturedStdout runs fn with os.Stdout replaced by a pipe and returns what it
// wrote there. Stderr is deliberately left alone: the point of the split is
// that everything advisory goes there, and this proves it by what it does NOT
// find on stdout.
func capturedStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	runErr := fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done, runErr
}

// getWithToken is a bearer-authenticated GET. Named for this file rather than
// shared, because cmd/catenary has no common request helper for GETs yet.
func getWithToken(t *testing.T, path, token string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// THE CLAIM THE STDERR SPLIT EXISTS FOR. A credential a human has to select out
// of prose is a credential that ends up in a scrollback buffer, so stdout must
// carry the token and nothing else.
func TestBotCreatePrintsTheTokenAloneOnStdoutAndItAuthenticates(t *testing.T) {
	ctx, _, st, h := authFixture(t)

	out, err := capturedStdout(t, func() error {
		return botCreate(ctx, st, []string{"argosy", "Argosy"})
	})
	if err != nil {
		t.Fatalf("bot create: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout carried %d lines, want exactly 1 — a redirect must capture the "+
			"credential and nothing else:\n%s", len(lines), out)
	}
	token := lines[0]
	if token == "" {
		t.Fatal("stdout carried no token")
	}
	if strings.Contains(strings.ToLower(out), "argosy") {
		t.Error("the handle appears on stdout; advisory text belongs on stderr")
	}

	code, body := do(t, h, getWithToken(t, "/sync?after=0", token))
	if code != http.StatusOK {
		t.Fatalf("the minted bot token was refused by GET /sync: %d %s", code, body)
	}
}

// THE WIRING TEST, and it exists because the one above structurally cannot see
// the defect it is named for.
//
// The test above calls botCreate with the store authFixture builds, and that
// store carries slog.DiscardHandler — which is exactly the line a stdout-logger
// bug lives on. So its two assertions, right as they are, are pointed one layer
// too low: they pass whether runBot logs to stdout or stderr. This one drives
// runBot itself, which loads config and builds its own logger, and therefore
// sees what an operator sees.
//
// It is deliberately NOT given a tuned logger. Everything about the wiring is
// the subject.
func TestRunBotKeepsLogOutputOffStdout(t *testing.T) {
	dsn := os.Getenv("CATENARY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATENARY_TEST_DATABASE_URL not set; skipping database test")
	}
	// Reset the schema the way every other cmd test does; the fixture's store
	// and router are unused here because runBot builds its own.
	authFixture(t)
	t.Setenv("CATENARY_DATABASE_URL", dsn)

	out, err := capturedStdout(t, func() error {
		return runBot([]string{"create", "argosy", "Argosy"})
	})
	if err != nil {
		t.Fatalf("runBot create: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout carried %d lines, want exactly 1. A log line here means "+
			"`catenary bot create argosy > token` writes JSON into the file ahead of the "+
			"credential, and what gets piped into Signet is not a credential:\n%s", len(lines), out)
	}
	// A slog JSON record starts with '{'; a token never does.
	if strings.HasPrefix(lines[0], "{") {
		t.Fatalf("stdout carried a log record rather than the token: %q", lines[0])
	}
	if lines[0] == "" {
		t.Fatal("stdout carried no token")
	}
}

// Minting is not revoking: a rotation needs a window in which both credentials
// work, or it is an outage.
func TestBotTokenMintsASecondCredentialWithoutEndingTheFirst(t *testing.T) {
	ctx, _, st, h := authFixture(t)

	first, err := capturedStdout(t, func() error { return botCreate(ctx, st, []string{"argosy", "Argosy"}) })
	if err != nil {
		t.Fatal(err)
	}
	second, err := capturedStdout(t, func() error { return botToken(ctx, st, []string{"argosy"}) })
	if err != nil {
		t.Fatalf("bot token: %v", err)
	}

	a, b := strings.TrimSpace(first), strings.TrimSpace(second)
	if a == b {
		t.Fatal("the second mint returned the same credential; each is generated once and never recovered")
	}
	for name, tok := range map[string]string{"the first": a, "the second": b} {
		if code, body := do(t, h, getWithToken(t, "/sync?after=0", tok)); code != http.StatusOK {
			t.Errorf("%s token was refused during rotation: %d %s — revoking the old one is a "+
				"separate, deliberate step", name, code, body)
		}
	}
}

// Revoking ends every live token, and takes effect on the next request.
func TestBotRevokeStopsEveryTokenOnTheNextRequest(t *testing.T) {
	ctx, _, st, h := authFixture(t)

	first, err := capturedStdout(t, func() error { return botCreate(ctx, st, []string{"argosy", "Argosy"}) })
	if err != nil {
		t.Fatal(err)
	}
	second, err := capturedStdout(t, func() error { return botToken(ctx, st, []string{"argosy"}) })
	if err != nil {
		t.Fatal(err)
	}

	if err := botRevoke(ctx, st, []string{"argosy"}); err != nil {
		t.Fatalf("bot revoke: %v", err)
	}
	for name, tok := range map[string]string{"the first": first, "the second": second} {
		code, _ := do(t, h, getWithToken(t, "/sync?after=0", strings.TrimSpace(tok)))
		if code != http.StatusUnauthorized {
			t.Errorf("%s token still authenticates after a revoke: %d", name, code)
		}
	}

	// A second revoke is a no-op rather than a failure: an operator running it
	// twice during an incident must not be told something went wrong.
	if err := botRevoke(ctx, st, []string{"argosy"}); err != nil {
		t.Errorf("a second revoke errored: %v", err)
	}
}

func TestBotSubcommandsRefuseAHandleThatIsNotABot(t *testing.T) {
	ctx, pool, st, _ := authFixture(t)
	mkUser(ctx, t, pool, "ada", "Ada Lovelace")

	if err := botRevoke(ctx, st, []string{"ada"}); err == nil {
		t.Error("revoking a person's handle succeeded; a person is not a revocable service account")
	}
	if err := botToken(ctx, st, []string{"ada"}); err == nil {
		t.Error("minting a bot token against a person's handle succeeded")
	}
	if err := botRevoke(ctx, st, []string{"nobody"}); err == nil {
		t.Error("revoking an unknown handle succeeded")
	}

	// And the person is untouched by any of it.
	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM users WHERE handle = 'ada'`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "person" {
		t.Fatalf("ada is now %q", kind)
	}
}

func TestBotCreateRefusesATakenHandle(t *testing.T) {
	ctx, pool, st, _ := authFixture(t)
	mkUser(ctx, t, pool, "ada", "Ada Lovelace")

	_, err := capturedStdout(t, func() error { return botCreate(ctx, st, []string{"ada"}) })
	if err == nil {
		t.Fatal("creating a bot on a person's handle succeeded")
	}
	if !strings.Contains(err.Error(), "ada") {
		t.Errorf("the error does not name the handle: %v", err)
	}
	if _, err := st.BotByHandle(ctx, "ada"); err == nil {
		t.Error("ada is now resolvable as a bot")
	}
}

// The subcommand dispatch itself: an unknown action is an error rather than a
// silent success, and every known one is reachable.
func TestBotRejectsAnUnknownAction(t *testing.T) {
	if err := runBot([]string{"destroy"}); err == nil {
		t.Error("an unknown bot action succeeded")
	} else if !strings.Contains(err.Error(), "destroy") {
		t.Errorf("the error does not name the action: %v", err)
	}
	if err := runBot(nil); err == nil {
		t.Error("`catenary bot` with no action succeeded")
	}
}
