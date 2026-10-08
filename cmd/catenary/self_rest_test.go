package main

// CANT-257 · POST /conversations/self. No body; idempotent; returns the same
// Conversation /sync serves; and what the owner writes into it serves the
// server's own receipt, `sent` with read_by 1, from /sync.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

func TestSelfConversationOverRESTIsIdempotentAndServesTheServersReceipt(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	enrolled := r.enroll(ada, "laptop")
	token := string(enrolled.AccessToken)

	post := func() (int, wire.Conversation) {
		code, raw := do(t, r.d.router, authedRequest(t, "/conversations/self", token, nil))
		var c wire.Conversation
		_ = json.Unmarshal(raw, &c)
		if code != http.StatusOK {
			t.Logf("POST /conversations/self = %d: %s", code, raw)
		}
		return code, c
	}
	code1, first := post()
	if code1 != http.StatusOK {
		t.Fatalf("first call = %d", code1)
	}
	if first.Kind != wire.ConversationKindSelf {
		t.Errorf("kind = %q, want self", first.Kind)
	}
	if first.Name != "Notes" {
		t.Errorf("name = %q, want Notes", first.Name)
	}
	if first.MemberCount != 1 {
		t.Errorf("member_count = %d, want 1", first.MemberCount)
	}
	if first.OtherMemberID != nil {
		t.Errorf("other_member_id = %v, want absent", *first.OtherMemberID)
	}
	code2, second := post()
	if code2 != http.StatusOK || second.ID != first.ID {
		t.Fatalf("second call = %d id %s, want 200 and %s", code2, second.ID, first.ID)
	}

	// It is on the owner's /sync page, as the same record.
	page := r.syncPage(token)
	var onSync *wire.Conversation
	for i := range page.Conversations {
		if page.Conversations[i].ID == first.ID {
			onSync = &page.Conversations[i]
		}
	}
	if onSync == nil {
		t.Fatal("the self conversation is not on the owner's /sync page")
	}
	if onSync.Kind != wire.ConversationKindSelf || onSync.Name != "Notes" || onSync.MemberCount != 1 || onSync.OtherMemberID != nil {
		t.Errorf("/sync serves %+v, want the record POST /conversations/self returned", *onSync)
	}

	// A message the owner wrote reads `sent` with read_by 1.
	convID, err := uuid.Parse(string(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	r.commit(convID, ada, "remember the milk")
	page = r.syncPage(token)
	if len(page.Messages) != 1 {
		t.Fatalf("/sync served %d messages, want 1", len(page.Messages))
	}
	m := page.Messages[0]
	if m.State != wire.DeliveryStateSent {
		t.Errorf("state = %q, want sent", m.State)
	}
	if m.ReadBy == nil || *m.ReadBy != 1 {
		t.Errorf("read_by = %v, want 1", m.ReadBy)
	}

	var count int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM conversations WHERE kind = 'self'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d self conversations exist, want 1", count)
	}
}

func TestSelfConversationOverRESTRefusesAnUnauthenticatedCaller(t *testing.T) {
	r := newRig(t)
	code, _ := do(t, r.d.router, authedRequest(t, "/conversations/self", "not-a-token", nil))
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}
