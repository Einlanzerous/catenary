package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

func okRoster(context.Context, uuid.UUID) ([]store.RosterRow, error) {
	return []store.RosterRow{{ID: uuid.New(), DisplayName: "Theo Park", Handle: "theo"}}, nil
}

func rosterGet(t *testing.T, h http.Handler, url string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestRosterIsNotRegisteredWithoutEverySeam(t *testing.T) {
	for name, d := range map[string]Deps{
		"no roster": {Logger: discardLogger(), Caller: someFullCaller},
		"no caller": {Logger: discardLogger(), Roster: okRoster},
	} {
		if res, _ := rosterGet(t, NewRouter(d), "/users"); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, res.StatusCode)
		}
	}
}

func TestRosterRefusesAnUnidentifiedCaller(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), Roster: okRoster, Caller: unauthCaller})
	if res, _ := rosterGet(t, h, "/users"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

func TestRosterServesTheWireShape(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), Roster: okRoster, Caller: someFullCaller})
	res, body := rosterGet(t, h, "/users")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var got wire.RosterResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body does not decode as RosterResponse: %v\n%s", err, body)
	}
	if len(got.Users) != 1 || got.Users[0].Handle != "theo" || got.Users[0].Initials == nil || *got.Users[0].Initials != "TP" {
		t.Errorf("users = %+v, want theo with initials TP", got.Users)
	}
}
