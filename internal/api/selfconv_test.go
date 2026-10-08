package api

// CANT-257 — POST /conversations/self maps a bot's refusal to 403, as
// POST /conversations does for CreateGroup.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
)

func TestSelfConversationMapsItsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a bot caller", store.ErrBotCannotCreateSelf, http.StatusForbidden},
		{"a person", nil, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "Notes"
			h := NewRouter(Deps{
				Logger: discardLogger(),
				Caller: someFullCaller,
				FindOrCreateSelf: func(context.Context, uuid.UUID) (store.ConversationRow, error) {
					if tc.err != nil {
						return store.ConversationRow{}, tc.err
					}
					return store.ConversationRow{ID: uuid.New(), Kind: "self", Name: &name, MemberCount: 1}, nil
				},
			})
			res, got := post(t, h, "/conversations/self", nil)
			if res.StatusCode != tc.want {
				t.Errorf("status = %d, want %d (%s)", res.StatusCode, tc.want, got)
			}
		})
	}
}
