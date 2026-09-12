package main

// CANT-28 criteria 11, 12 and 13, end to end through the REAL router that
// setup() builds — not an injected CallerID, because the claim being made is
// specifically that a real process now registers GET /sync and refuses an
// unauthenticated request to it.
//
// CANT-20 left the seam unwired on purpose and said why: the route "turns on
// the moment authentication does — rather than shipping now behind a header
// that would quietly become the auth scheme". This file is the evidence that
// the moment arrived and that the header it turned on behind is a credential
// resolved by store.Authenticate.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/store"
)

func authFixture(t *testing.T) (context.Context, *pgxpool.Pool, *store.Store, http.Handler) {
	t.Helper()
	dsn := os.Getenv("CATENARY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CATENARY_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.MigrateDown(ctx, pool, 0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.New(pool, store.DefaultLimits(), slog.New(slog.DiscardHandler))

	cfg := config.Config{DatabaseURL: dsn, Addr: ":0", LogFormat: "json"}
	d := setup(cfg, slog.New(slog.DiscardHandler), st)
	return ctx, pool, st, d.router
}

func do(t *testing.T, h http.Handler, req *http.Request) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return rec.Code, body
}

func enrollRequest(t *testing.T, token, name string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]string{"enrollment_token": token, "device_name": name})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/enroll", bytes.NewReader(body))
}

// Criterion 12 — the route exists in a real process, and says no.
func TestSyncIsRegisteredAndRefusesAnUnauthenticatedRequest(t *testing.T) {
	_, _, _, h := authFixture(t)

	code, body := do(t, h, httptest.NewRequest(http.MethodGet, "/sync?after=0", nil))
	if code == http.StatusNotFound {
		t.Fatal("GET /sync is not registered — CallerID is still unwired, so this ticket's " +
			"central claim is false")
	}
	if code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /sync = %d, want 401", code)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %q", body)
	}
	if got["code"] != "unauthorized" {
		t.Errorf("refusal body = %v, want the wire code `unauthorized`", got)
	}

	// A credential that is well-formed and simply not ours gets the same answer.
	plaintext, _, err := store.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	if code, _ := do(t, h, req); code != http.StatusUnauthorized {
		t.Errorf("GET /sync with an unknown bearer = %d, want 401", code)
	}
}

// The whole path, once: Purser issues, a device redeems, the credential works,
// revocation ends it.
func TestADeviceEnrollsAuthenticatesAndIsRevoked(t *testing.T) {
	ctx, pool, st, h := authFixture(t)
	user := mkUser(ctx, t, pool, "ada", "Ada Lovelace")

	issued, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	code, body := do(t, h, enrollRequest(t, issued.Plaintext, "Pixel 8 Pro"))
	if code != http.StatusOK {
		t.Fatalf("POST /enroll = %d: %s", code, body)
	}
	var resp struct {
		UserID           string `json:"user_id"`
		DeviceID         string `json:"device_id"`
		AccessToken      string `json:"access_token"`
		AccessExpiresAt  string `json:"access_expires_at"`
		RefreshToken     string `json:"refresh_token"`
		RefreshExpiresAt string `json:"refresh_expires_at"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("enroll response is not JSON: %q", body)
	}
	if resp.UserID != user.String() {
		t.Errorf("user_id = %s, want %s", resp.UserID, user)
	}
	// The timestamps have to be the wire's own format, or a generated decoder
	// refuses the one response a client cannot recover from refusing.
	for _, ts := range []string{resp.AccessExpiresAt, resp.RefreshExpiresAt} {
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err != nil {
			t.Errorf("%q is not the wire timestamp format: %v", ts, err)
		}
	}

	authed := func(token string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
	if code, body := do(t, h, authed(resp.AccessToken)); code != http.StatusOK {
		t.Fatalf("GET /sync with a fresh access token = %d: %s", code, body)
	}

	device, err := uuid.Parse(resp.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeDevice(ctx, device); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code, _ := do(t, h, authed(resp.AccessToken)); code != http.StatusUnauthorized {
		t.Errorf("GET /sync after revocation = %d, want 401. Ruling 0 buys immediate "+
			"revocation by reading the device row on every request; if this is 200, it did not",
			code)
	}
}

// Criterion 11 — one refusal shape, byte for byte, for four different causes.
//
// THIS IS THE HALF THAT CANNOT BE TESTED IN THE HANDLER ALONE. A handler with
// an injected Enroll that returns one error will trivially produce one body.
// What matters is that four genuinely different states in the database
// converge on that error before the handler ever sees them — because /enroll is
// unauthenticated, has no rate limiter in front of it by decision, and a
// response that told them apart would tell a prober which of its guesses was
// once a real token, and whether an account exists.
func TestEveryEnrollmentRefusalIsByteForByteIdentical(t *testing.T) {
	ctx, pool, st, h := authFixture(t)

	tokenFor := map[string]string{}

	// 1. Unknown: well-formed, never issued.
	unknown, _, err := store.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	tokenFor["unknown"] = unknown

	// 2. Expired.
	expiredUser := mkUser(ctx, t, pool, "expired", "Expired")
	expired, err := st.IssueEnrollmentToken(ctx, expiredUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE enrollment_tokens SET expires_at = now() - interval '1 second' WHERE user_id = $1`,
		expiredUser); err != nil {
		t.Fatal(err)
	}
	tokenFor["expired"] = expired.Plaintext

	// 3. Already redeemed.
	redeemedUser := mkUser(ctx, t, pool, "redeemed", "Redeemed")
	redeemed, err := st.IssueEnrollmentToken(ctx, redeemedUser)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, h, enrollRequest(t, redeemed.Plaintext, "first")); code != http.StatusOK {
		t.Fatalf("the first redemption should have worked: %d %s", code, body)
	}
	tokenFor["already redeemed"] = redeemed.Plaintext

	// 4. Deactivated, with a live unredeemed token.
	goneUser := mkUser(ctx, t, pool, "gone", "Gone")
	gone, err := st.IssueEnrollmentToken(ctx, goneUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, goneUser); err != nil {
		t.Fatal(err)
	}
	tokenFor["deactivated"] = gone.Plaintext

	type answer struct {
		code int
		body string
	}
	answers := map[string]answer{}
	for cause, token := range tokenFor {
		code, body := do(t, h, enrollRequest(t, token, "Pixel 8 Pro"))
		answers[cause] = answer{code, string(body)}
		if code != http.StatusUnauthorized {
			t.Errorf("%s: POST /enroll = %d, want 401: %s", cause, code, body)
		}
	}

	var first answer
	var firstCause string
	for cause, a := range answers {
		if firstCause == "" {
			first, firstCause = a, cause
			continue
		}
		if a != first {
			t.Errorf("the refusal for %q differs from the one for %q:\n  %s: %d %s\n  %s: %d %s\n"+
				"All four must be indistinguishable. The log tells them apart; the response "+
				"must not, or an unauthenticated endpoint with no limiter in front of it "+
				"becomes an oracle for which tokens once existed and which accounts do.",
				cause, firstCause, cause, a.code, a.body, firstCause, first.code, first.body)
		}
	}

	// And it is the same body GET /sync writes, because there is one way for
	// this service to refuse a credential.
	syncCode, syncBody := do(t, h, httptest.NewRequest(http.MethodGet, "/sync?after=0", nil))
	if syncCode != first.code || string(syncBody) != first.body {
		t.Errorf("GET /sync refuses with %d %s and POST /enroll with %d %s; one service, one refusal",
			syncCode, syncBody, first.code, first.body)
	}
}

// A malformed request is NOT part of that rule, and the distinction is worth a
// test: it says nothing about whether a credential exists, and collapsing it
// into the 401 would tell a client with a typo that its token was rejected.
func TestAMalformedEnrollmentRequestIsABadRequest(t *testing.T) {
	_, _, _, h := authFixture(t)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"missing device_name", `{"enrollment_token":"enrollment_token_FIXTURE_not_a_real_secret_"}`},
		{"padded token", `{"enrollment_token":"padded_std_encoding_FIXTURE_not_a_secret___=","device_name":"x"}`},
		{"empty device_name", `{"enrollment_token":"enrollment_token_FIXTURE_not_a_real_secret_","device_name":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/enroll", bytes.NewReader([]byte(tc.body)))
			if code, body := do(t, h, req); code != http.StatusBadRequest {
				t.Errorf("POST /enroll = %d, want 400: %s", code, body)
			}
		})
	}
}
