package api

// CANT-268 — POST /conversations over a stubbed store: registration, the 401,
// the 201/200 split on a replayed request_id, and the refusal statuses. The
// store's own tests hold the rows.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

func stubCreateGroup(created bool, err error) func(context.Context, uuid.UUID, string, []string, *uuid.UUID) (store.ConversationRow, bool, error) {
	return func(_ context.Context, _ uuid.UUID, name string, _ []string, _ *uuid.UUID) (store.ConversationRow, bool, error) {
		if err != nil {
			return store.ConversationRow{}, false, err
		}
		return store.ConversationRow{ID: uuid.New(), Kind: "group", Name: &name, MemberCount: 3}, created, nil
	}
}

var groupBody = map[string]any{"name": "The Room", "member_handles": []string{"theo", "mal"}}

func TestCreateGroupIsNotRegisteredWithoutEverySeam(t *testing.T) {
	for name, d := range map[string]Deps{
		"no create": {Logger: discardLogger(), Caller: someFullCaller},
		"no caller": {Logger: discardLogger(), CreateGroup: stubCreateGroup(true, nil)},
	} {
		if res, _ := post(t, NewRouter(d), "/conversations", groupBody); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, res.StatusCode)
		}
	}
}

func TestCreateGroupRefusesAnUnidentifiedCaller(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), CreateGroup: stubCreateGroup(true, nil), Caller: unauthCaller})
	if res, _ := post(t, h, "/conversations", groupBody); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

func TestCreateGroupAnswers201ForANewRoomAnd200ForAReplay(t *testing.T) {
	body := map[string]any{"name": "The Room", "member_handles": []string{"theo"}, "request_id": uuid.NewString()}
	for _, tc := range []struct {
		created bool
		want    int
	}{{true, http.StatusCreated}, {false, http.StatusOK}} {
		h := NewRouter(Deps{Logger: discardLogger(), CreateGroup: stubCreateGroup(tc.created, nil), Caller: someFullCaller})
		res, got := post(t, h, "/conversations", body)
		if res.StatusCode != tc.want {
			t.Errorf("created=%v: status = %d, want %d", tc.created, res.StatusCode, tc.want)
		}
		var c wire.Conversation
		if err := json.Unmarshal([]byte(got), &c); err != nil || c.Name != "The Room" || c.MemberCount != 3 {
			t.Errorf("created=%v: body %s does not decode as the room (%v)", tc.created, got, err)
		}
	}
}

func TestCreateGroupMapsItsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		body any
		want int
	}{
		{"an invalid request", fmt.Errorf("%w: duplicate handle", store.ErrInvalidGroup), groupBody, http.StatusBadRequest},
		{"an unknown handle", store.ErrTargetNotFound, groupBody, http.StatusNotFound},
		{"a deactivated handle", store.ErrTargetDeactivated, groupBody, http.StatusNotFound},
		{"a body that is not a CreateGroupRequest", nil, map[string]any{"name": "x"}, http.StatusBadRequest},
		{"a request_id that is not a uuid", nil, map[string]any{"name": "x", "member_handles": []string{"a"}, "request_id": "nope"}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewRouter(Deps{Logger: discardLogger(), CreateGroup: stubCreateGroup(true, tc.err), Caller: someFullCaller})
			res, got := post(t, h, "/conversations", tc.body)
			if res.StatusCode != tc.want {
				t.Errorf("status = %d, want %d (%s)", res.StatusCode, tc.want, got)
			}
		})
	}
}
