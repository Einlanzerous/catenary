package main

// CANT-117 — the device surface through the REAL router setup() builds.
//
// The store's own tests are the oracle for the scoping. What this file adds is
// the half that cannot be tested below the transport: that the routes are
// registered in a real process, that the body is the generated wire type every
// client will decode, and that revoking through the surface reaches CANT-30's
// severance over a real socket.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/magos/catenary/internal/hub"
	"github.com/magos/catenary/internal/wire"
)

func authed(t *testing.T, method, path, token string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestTheDeviceListIsRegisteredAndServesOnlyTheCallersOwn(t *testing.T) {
	ctx, pool, st, h := authFixture(t)
	ada := mkUser(ctx, t, pool, "ada", "Ada Lovelace")
	theo := mkUser(ctx, t, pool, "theo", "Theo")

	issued, err := st.IssueEnrollmentToken(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	code, body := do(t, h, enrollRequest(t, issued.Plaintext, "Pixel 8 Pro"))
	if code != http.StatusOK {
		t.Fatalf("enroll = %d: %s", code, body)
	}
	var mine wire.EnrollResponse
	if err := json.Unmarshal(body, &mine); err != nil {
		t.Fatal(err)
	}

	// Somebody else's device, which must never appear.
	theoIssued, err := st.IssueEnrollmentToken(ctx, theo)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, h, enrollRequest(t, theoIssued.Plaintext, "Theo's phone")); code != http.StatusOK {
		t.Fatalf("theo enroll = %d: %s", code, body)
	}

	code, body = do(t, h, authed(t, http.MethodGet, "/devices", string(mine.AccessToken)))
	if code == http.StatusNotFound {
		t.Fatal("GET /devices is not registered — the route is this ticket's central claim")
	}
	if code != http.StatusOK {
		t.Fatalf("GET /devices = %d: %s", code, body)
	}

	// THE GENERATED DECODER, not a hand-rolled struct: if the body is not a
	// valid DeviceListResponse, every client refuses it and so must this test.
	if _, err := wire.DecodeNamed("DeviceListResponse", body); err != nil {
		t.Fatalf("the served page is not a DeviceListResponse: %v\n%s", err, body)
	}
	var page wire.DeviceListResponse
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Devices) != 1 {
		t.Fatalf("%d devices, want exactly 1 — another account's device is on this page", len(page.Devices))
	}
	if page.Devices[0].ID != mine.DeviceID {
		t.Errorf("device id = %s, want the caller's own %s", page.Devices[0].ID, mine.DeviceID)
	}
	if page.Devices[0].Name != "Pixel 8 Pro" {
		t.Errorf("name = %q, want the name the install enrolled with", page.Devices[0].Name)
	}
	if page.Devices[0].RevokedAt != nil {
		t.Errorf("a live device carries revoked_at %v; absent means live", *page.Devices[0].RevokedAt)
	}
}

// A bot has no device and never enrols one, so its list is empty — and an empty
// page must encode as `[]`, which a nil slice in Go would break.
func TestABotsDeviceListIsAnEmptyArrayAndNotARefusal(t *testing.T) {
	ctx, pool, st, h := authFixture(t)
	bot := mkUser(ctx, t, pool, "argosy-bot", "Argosy")
	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'bot' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}
	token, err := st.IssueBotToken(ctx, bot)
	if err != nil {
		t.Fatalf("issue bot token: %v", err)
	}

	code, body := do(t, h, authed(t, http.MethodGet, "/devices", token.Plaintext))
	if code != http.StatusOK {
		t.Fatalf("a bot's GET /devices = %d: %s — a caller with nothing to list is not a caller "+
			"who may not look", code, body)
	}
	if _, err := wire.DecodeNamed("DeviceListResponse", body); err != nil {
		t.Fatalf("a bot's page is not a DeviceListResponse: %v\n%s", err, body)
	}
	// The encoded shape, not the decoded one: `null` would decode into a nil
	// slice and read as empty here while every generated client refused it.
	var raw struct {
		Devices *[]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Devices == nil {
		t.Fatal("`devices` is absent or null for a bot; it is required and an empty page must " +
			"encode as []")
	}
	if len(*raw.Devices) != 0 {
		t.Errorf("%d devices for a bot, want 0", len(*raw.Devices))
	}
}

func TestTheDeviceSurfaceRefusesAnUnauthenticatedCaller(t *testing.T) {
	_, _, _, h := authFixture(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/devices"},
		{http.MethodPost, "/devices/" + "d1e2f3a4-b5c6-4d7e-8f9a-0b1c2d3e4f5a" + "/revoke"},
	} {
		code, body := do(t, h, httptest.NewRequest(tc.method, tc.path, nil))
		if code == http.StatusNotFound {
			t.Fatalf("%s %s is not registered", tc.method, tc.path)
		}
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated = %d, want 401: %s", tc.method, tc.path, code, body)
		}
	}
}

func TestAMalformedDeviceIdIsABadRequest(t *testing.T) {
	ctx, pool, st, h := authFixture(t)
	ada := mkUser(ctx, t, pool, "ada", "Ada Lovelace")
	issued, err := st.IssueEnrollmentToken(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	_, body := do(t, h, enrollRequest(t, issued.Plaintext, "Pixel 8 Pro"))
	var mine wire.EnrollResponse
	if err := json.Unmarshal(body, &mine); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, h, authed(t, http.MethodPost, "/devices/not-a-uuid/revoke", string(mine.AccessToken)))
	if code != http.StatusBadRequest {
		t.Errorf("a malformed device id = %d, want 400: %s", code, body)
	}
}

// Revoking through the surface takes effect on the next request, and revoking
// somebody else's is the same 204 that changes nothing — so the route is not an
// oracle for which device ids exist elsewhere.
func TestRevokingThroughTheSurfaceTakesEffectAndCannotReachAnotherAccount(t *testing.T) {
	ctx, pool, st, h := authFixture(t)
	ada := mkUser(ctx, t, pool, "ada", "Ada Lovelace")
	theo := mkUser(ctx, t, pool, "theo", "Theo")

	adaIssued, err := st.IssueEnrollmentToken(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	_, body := do(t, h, enrollRequest(t, adaIssued.Plaintext, "Pixel 8 Pro"))
	var mine wire.EnrollResponse
	if err := json.Unmarshal(body, &mine); err != nil {
		t.Fatal(err)
	}

	theoIssued, err := st.IssueEnrollmentToken(ctx, theo)
	if err != nil {
		t.Fatal(err)
	}
	_, body = do(t, h, enrollRequest(t, theoIssued.Plaintext, "Theo's phone"))
	var theirs wire.EnrollResponse
	if err := json.Unmarshal(body, &theirs); err != nil {
		t.Fatal(err)
	}

	// Ada tries theo's device: the same 204, and nothing changes.
	if code, _ := do(t, h, authed(t, http.MethodPost,
		"/devices/"+string(theirs.DeviceID)+"/revoke", string(mine.AccessToken))); code != http.StatusNoContent {
		t.Errorf("revoking another account's device = %d, want 204 — telling it apart would make "+
			"this an oracle for which device ids exist", code)
	}
	if code, body := do(t, h, authed(t, http.MethodGet, "/sync?after=0", string(theirs.AccessToken))); code != http.StatusOK {
		t.Fatalf("theo's device stopped working after ada tried to revoke it: %d %s", code, body)
	}

	// Ada's own device: 204, and it stops working on the next request.
	if code, _ := do(t, h, authed(t, http.MethodPost,
		"/devices/"+string(mine.DeviceID)+"/revoke", string(mine.AccessToken))); code != http.StatusNoContent {
		t.Fatal("revoking my own device did not answer 204")
	}
	if code, _ := do(t, h, authed(t, http.MethodGet, "/sync?after=0", string(mine.AccessToken))); code != http.StatusUnauthorized {
		t.Errorf("a device revoked through the surface still authenticates: %d", code)
	}
}

// The severance half, end to end: revoking through the REST surface closes the
// device's live socket, which is CANT-30's machinery reached from CANT-117's
// route.
func TestRevokingThroughTheSurfaceSeversTheLiveSocket(t *testing.T) {
	r := newRig(t)
	runRevocations(r)

	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada Lovelace")
	cred := r.enroll(ada, "Pixel 8 Pro")
	sess := openAt(t, r.ctx, r.base, cred, ada)
	sess.pingPong()

	code, body := do(t, r.d.router, authed(t, http.MethodPost,
		"/devices/"+string(cred.DeviceID)+"/revoke", string(cred.AccessToken)))
	if code != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", code, body)
	}

	if got := sess.expectClose(); got != hub.StatusRevoked {
		t.Errorf("the socket closed with %d, want %d (StatusRevoked). The route writes "+
			"devices.revoked_at and publishes inside the same transaction; if the socket "+
			"survives, the surface is revoking on paper only", got, hub.StatusRevoked)
	}
}
