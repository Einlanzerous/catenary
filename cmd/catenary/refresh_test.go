package main

// CANT-97 — POST /refresh through the REAL router that setup() builds.
//
// The store's own tests are the oracle for the write. What this file adds is
// the half that cannot be tested below the transport: that the route is
// registered in a real process, that its body is the shape every generated
// decoder will accept, and that its refusal is byte-for-byte the one the rest
// of this service gives — which is a property OF THE SERVICE rather than of
// any one handler, and so has to be asserted where all three routes exist
// together.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
)

func refreshRequest(t *testing.T, token string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]string{"refresh_token": token})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/refresh", bytes.NewReader(body))
}

type pair struct {
	AccessToken      string `json:"access_token"`
	AccessExpiresAt  string `json:"access_expires_at"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresAt string `json:"refresh_expires_at"`
}

// enrollOne runs a full POST /enroll and hands back the pair it minted.
func enrollOne(t *testing.T, h http.Handler, enrollmentToken string) pair {
	t.Helper()
	code, body := do(t, h, enrollRequest(t, enrollmentToken, "Pixel 8 Pro"))
	if code != http.StatusOK {
		t.Fatalf("POST /enroll = %d: %s", code, body)
	}
	var got struct {
		pair
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("enroll response is not JSON: %q", body)
	}
	return got.pair
}

func TestRefreshExchangesAPairThroughTheRealRouter(t *testing.T) {
	ctx, pool, st, h := authFixture(t)
	user := mkUser(ctx, t, pool, "ada", "Ada Lovelace")
	issued, err := st.IssueEnrollmentToken(ctx, user)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	first := enrollOne(t, h, issued.Plaintext)

	code, body := do(t, h, refreshRequest(t, first.RefreshToken))
	if code == http.StatusNotFound {
		t.Fatal("POST /refresh is not registered — the route is this ticket's central claim")
	}
	if code != http.StatusOK {
		t.Fatalf("POST /refresh = %d: %s", code, body)
	}
	var next pair
	if err := json.Unmarshal(body, &next); err != nil {
		t.Fatalf("refresh response is not JSON: %q", body)
	}

	if next.RefreshToken == first.RefreshToken {
		t.Error("POST /refresh returned the same refresh token; this exchange rotates")
	}
	if next.AccessToken == first.AccessToken {
		t.Error("POST /refresh returned the same access token")
	}
	// The timestamps have to be the wire's own format, or a generated decoder
	// refuses the one response a client cannot recover from refusing.
	for _, ts := range []string{next.AccessExpiresAt, next.RefreshExpiresAt} {
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err != nil {
			t.Errorf("%q is not the wire timestamp format: %v", ts, err)
		}
	}

	// The new access token is a working credential on a real authenticated
	// route, which is the only thing the client actually wanted.
	authed := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	authed.Header.Set("Authorization", "Bearer "+next.AccessToken)
	if code, body := do(t, h, authed); code != http.StatusOK {
		t.Fatalf("GET /sync with a rotated access token = %d: %s", code, body)
	}

	// And the presented one is spent.
	if code, _ := do(t, h, refreshRequest(t, first.RefreshToken)); code != http.StatusUnauthorized {
		t.Errorf("re-presenting a rotated refresh token = %d, want 401", code)
	}
}

// One refusal shape, byte for byte, across four causes AND across three routes.
//
// THE REASONING IS CANT-28'S AND IT REACHES THIS ROUTE UNCHANGED. /refresh
// takes a credential from an unauthenticated caller and has no rate limiter in
// front of it, so a response that told an unknown token from a spent one would
// tell a prober which of its guesses was once real, and a distinct answer for a
// deactivated account would confirm that the account exists. The log
// distinguishes all of them; the response must not.
func TestEveryRefreshRefusalIsByteForByteIdenticalAndMatchesTheOtherRoutes(t *testing.T) {
	ctx, pool, st, h := authFixture(t)

	tokenFor := map[string]string{}

	// 1. Unknown: well-formed, never issued.
	unknown, _, err := store.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	tokenFor["unknown"] = unknown

	// 2. Already rotated.
	spentUser := mkUser(ctx, t, pool, "spent", "Spent")
	spentIssued, err := st.IssueEnrollmentToken(ctx, spentUser)
	if err != nil {
		t.Fatal(err)
	}
	spent := enrollOne(t, h, spentIssued.Plaintext)
	if code, body := do(t, h, refreshRequest(t, spent.RefreshToken)); code != http.StatusOK {
		t.Fatalf("the first rotation should have worked: %d %s", code, body)
	}
	tokenFor["already rotated"] = spent.RefreshToken

	// 3. Revoked device, with a live unrotated token.
	revokedUser := mkUser(ctx, t, pool, "revoked", "Revoked")
	revokedIssued, err := st.IssueEnrollmentToken(ctx, revokedUser)
	if err != nil {
		t.Fatal(err)
	}
	code, body := do(t, h, enrollRequest(t, revokedIssued.Plaintext, "Pixel 8 Pro"))
	if code != http.StatusOK {
		t.Fatalf("enroll: %d %s", code, body)
	}
	var revokedResp struct {
		pair
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(body, &revokedResp); err != nil {
		t.Fatal(err)
	}
	device, err := uuid.Parse(revokedResp.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeDevice(ctx, device); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	tokenFor["revoked device"] = revokedResp.RefreshToken

	// 4. Deactivated account, with a live unrotated token and a live device.
	goneUser := mkUser(ctx, t, pool, "gone", "Gone")
	goneIssued, err := st.IssueEnrollmentToken(ctx, goneUser)
	if err != nil {
		t.Fatal(err)
	}
	gone := enrollOne(t, h, goneIssued.Plaintext)
	if _, err := pool.Exec(ctx,
		`UPDATE users SET deactivated_at = now() WHERE id = $1`, goneUser); err != nil {
		t.Fatal(err)
	}
	tokenFor["deactivated"] = gone.RefreshToken

	type answer struct {
		code int
		body string
	}
	answers := map[string]answer{}
	for cause, token := range tokenFor {
		code, body := do(t, h, refreshRequest(t, token))
		answers[cause] = answer{code, string(body)}
		if code != http.StatusUnauthorized {
			t.Errorf("%s: POST /refresh = %d, want 401: %s", cause, code, body)
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
				"All four must be indistinguishable. The log tells them apart; the response must "+
				"not, or a credential endpoint with no limiter in front of it becomes an oracle "+
				"for which tokens once existed and which accounts do.",
				cause, firstCause, cause, a.code, a.body, firstCause, first.code, first.body)
		}
	}

	// And it is the same body the other two credential routes write, because
	// there is ONE way for this service to refuse a credential.
	syncCode, syncBody := do(t, h, httptest.NewRequest(http.MethodGet, "/sync?after=0", nil))
	if syncCode != first.code || string(syncBody) != first.body {
		t.Errorf("GET /sync refuses with %d %s and POST /refresh with %d %s; one service, one refusal",
			syncCode, syncBody, first.code, first.body)
	}
	enrollCode, enrollBody := do(t, h, enrollRequest(t, unknown, "Pixel 8 Pro"))
	if enrollCode != first.code || string(enrollBody) != first.body {
		t.Errorf("POST /enroll refuses with %d %s and POST /refresh with %d %s; one service, one refusal",
			enrollCode, enrollBody, first.code, first.body)
	}
}

// A malformed request is NOT part of that rule: it says nothing about whether a
// credential exists, and collapsing it into the 401 would tell a client with a
// typo that its token was rejected.
func TestAMalformedRefreshRequestIsABadRequest(t *testing.T) {
	_, _, _, h := authFixture(t)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"missing refresh_token", `{}`},
		{"padded token", `{"refresh_token":"padded_std_encoding_FIXTURE_not_a_secret___="}`},
		{"empty token", `{"refresh_token":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/refresh", bytes.NewReader([]byte(tc.body)))
			if code, body := do(t, h, req); code != http.StatusBadRequest {
				t.Errorf("POST /refresh = %d, want 400: %s", code, body)
			}
		})
	}
}
