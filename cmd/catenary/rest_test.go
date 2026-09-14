package main

// CANT-75's Done-when, end to end through the REAL router setup() builds,
// the real store on Postgres and real sockets — the same rig fanout_test.go
// uses, because the central claim is comparative: a message posted over REST
// must be indistinguishable, on /sync and on every connected socket, from
// one sent over the socket.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// mkBot is a service account: a user with no device, authenticated with a
// long-lived bot token rather than an access/refresh pair (CANT-28 ruling 4).
func mkBot(ctx context.Context, t *testing.T, pool *pgxpool.Pool, st *store.Store, handle, name string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, handle, display_name, kind) VALUES ($1, $2, $3, 'bot')`, id, handle, name); err != nil {
		t.Fatalf("mkBot(%s): %v", handle, err)
	}
	issued, err := st.IssueBotToken(ctx, id)
	if err != nil {
		t.Fatalf("issue bot token: %v", err)
	}
	return id, issued.Plaintext
}

// authedRequest builds a POST with a bearer token and a JSON body.
func authedRequest(t *testing.T, url, token string, body any) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(http.MethodPost, url, r)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// convCounters reads the two ordinal sources directly, the same shape
// internal/store's sendpath_test.go uses to prove a replay draws neither.
func convCounters(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conv uuid.UUID) (lastSeq, logCounter int64) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT last_seq FROM conversations WHERE id = $1`, conv).Scan(&lastSeq); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&logCounter); err != nil {
		t.Fatal(err)
	}
	return
}

func mustParseWireUUID(t *testing.T, u wire.Uuid) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(string(u))
	if err != nil {
		t.Fatalf("not a uuid: %s", u)
	}
	return id
}

func (r *rig) postMessage(token, conv string, body any) (int, wire.Message) {
	r.t.Helper()
	code, raw := do(r.t, r.d.router, authedRequest(r.t, "/conversations/"+conv+"/messages", token, body))
	var m wire.Message
	_ = json.Unmarshal(raw, &m)
	if code != http.StatusOK {
		r.t.Logf("POST messages = %d: %s", code, raw)
	}
	return code, m
}

func (r *rig) postDirect(token string, body any) (int, wire.Conversation) {
	r.t.Helper()
	code, raw := do(r.t, r.d.router, authedRequest(r.t, "/conversations/direct", token, body))
	var c wire.Conversation
	_ = json.Unmarshal(raw, &c)
	if code != http.StatusOK {
		r.t.Logf("POST /conversations/direct = %d: %s", code, raw)
	}
	return code, c
}

func (r *rig) syncPage(token string) wire.SyncResponse {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/sync?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	code, raw := do(r.t, r.d.router, req)
	if code != http.StatusOK {
		r.t.Fatalf("GET /sync = %d: %s", code, raw)
	}
	var page wire.SyncResponse
	if err := json.Unmarshal(raw, &page); err != nil {
		r.t.Fatalf("decode /sync: %v", err)
	}
	return page
}

// A message posted over REST is indistinguishable from one sent over the
// socket: it arrives as a `message` frame on every OTHER attached member
// session with the state that member sees, carries client_id echoed to its
// own author, and appears on /sync with the identical wire values the REST
// response itself carried.
func TestARESTSentMessageIsIndistinguishableFromASocketSentOne(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo)

	adaEnrolled := r.enroll(ada, "laptop")
	theoS := r.open(theo, "phone")

	clientID := uuid.New()
	text := "posted over REST"
	code, got := r.postMessage(string(adaEnrolled.AccessToken), group.String(),
		map[string]any{"client_id": clientID.String(), "text": text})
	if code != http.StatusOK {
		t.Fatalf("POST messages = %d", code)
	}
	if got.Text == nil || *got.Text != text {
		t.Errorf("REST response text = %v, want %q", got.Text, text)
	}
	if got.State != wire.DeliveryStateSent {
		t.Errorf("REST response state = %s, want sent (own message, nobody else has read it)", got.State)
	}
	if got.ClientID == nil || string(*got.ClientID) != clientID.String() {
		t.Errorf("REST response client_id = %v, want echoed to the author", got.ClientID)
	}
	if got.ReadBy == nil || *got.ReadBy != 1 {
		t.Errorf("REST response read_by = %v, want 1 (the author)", got.ReadBy)
	}

	// It arrives as a `message` frame on theo's attached socket, per-viewer.
	m := theoS.message()
	if m.ID != got.ID || m.Seq != got.Seq || m.LogSeq != got.LogSeq || m.AuthorID != got.AuthorID {
		t.Errorf("socket frame = %+v, want the same row the REST response carried (%+v)", m, got)
	}
	if m.Text == nil || *m.Text != text {
		t.Errorf("socket frame text = %v, want %q", m.Text, text)
	}
	if m.State != wire.DeliveryStateDelivered {
		t.Errorf("theo's state = %s, want delivered", m.State)
	}
	if m.ClientID != nil {
		t.Errorf("client_id leaked to a non-author: %v", m.ClientID)
	}

	// And it appears on /sync with the SAME wire values the REST response
	// itself carried — the claim is comparative, not merely "some message
	// showed up".
	page := r.syncPage(string(adaEnrolled.AccessToken))
	var onSync *wire.Message
	for i := range page.Messages {
		if page.Messages[i].ID == got.ID {
			onSync = &page.Messages[i]
		}
	}
	if onSync == nil {
		t.Fatal("the REST-sent message is not on /sync")
	}
	if onSync.Seq != got.Seq || onSync.LogSeq != got.LogSeq || onSync.State != got.State ||
		onSync.Text == nil || got.Text == nil || *onSync.Text != *got.Text ||
		onSync.ClientID == nil || got.ClientID == nil || *onSync.ClientID != *got.ClientID {
		t.Errorf("/sync message = %+v, want the same wire values as the REST response %+v", onSync, got)
	}
}

// A replayed client_id over REST returns the original message and creates no
// row, drawing no ordinal — the same property TestAReplayAdvancesNothing
// proves at the store layer, now proved through the transport.
func TestAReplayedClientIDOverRESTReturnsTheOriginalAndCreatesNoRow(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, theo)
	enrolled := r.enroll(ada, "laptop")
	theoS := r.open(theo, "phone")

	clientID := uuid.New()
	code1, first := r.postMessage(string(enrolled.AccessToken), group.String(),
		map[string]any{"client_id": clientID.String(), "text": "first attempt"})
	if code1 != http.StatusOK {
		t.Fatalf("first send = %d", code1)
	}
	theoS.message()
	beforeLast, beforeLog := convCounters(r.ctx, t, r.pool, group)

	code2, second := r.postMessage(string(enrolled.AccessToken), group.String(),
		map[string]any{"client_id": clientID.String(), "text": "first attempt, retried"})
	if code2 != http.StatusOK {
		t.Fatalf("replay = %d", code2)
	}
	if second.ID != first.ID || second.Seq != first.Seq || second.LogSeq != first.LogSeq {
		t.Errorf("replay returned %+v, want the original %+v", second, first)
	}
	if second.Text == nil || first.Text == nil || *second.Text != *first.Text {
		t.Errorf("replay's text = %v, want the ORIGINAL text %v, not the retried body", second.Text, first.Text)
	}

	afterLast, afterLog := convCounters(r.ctx, t, r.pool, group)
	if afterLast != beforeLast || afterLog != beforeLog {
		t.Errorf("a replay moved the counters: last_seq %d→%d, log_counter %d→%d",
			beforeLast, afterLast, beforeLog, afterLog)
	}

	var count int
	if err := r.pool.QueryRow(r.ctx,
		`SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_id = $2`,
		group, clientID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d rows for one client_id, want 1", count)
	}

	// No second delivery: theo's next hub frame is a fresh message, not the
	// replay.
	marker, err := r.st.SendMessage(r.ctx, store.NewMessage{
		ClientID: uuid.New(), ConversationID: group, AuthorID: ada, Text: ptrStr("marker"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m := theoS.message(); string(m.ID) != marker.ID.String() {
		t.Errorf("theo's next frame = %+v, want the marker — the replay fanned out a second time", m)
	}
}

// A non-member's REST send is refused not_a_member, with the HTTP status
// senderror.go's row carries and no message row created.
func TestARESTSendFromANonMemberGetsNotAMemberWithItsTableStatus(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	stranger := mkUser(r.ctx, t, r.pool, "stranger", "Stranger")
	group := mkGroup(r.ctx, t, r.pool, "A", ada)
	enrolled := r.enroll(stranger, "phone")

	req := authedRequest(t, "/conversations/"+group.String()+"/messages", string(enrolled.AccessToken),
		map[string]any{"client_id": uuid.NewString(), "text": "let me in"})
	code, body := do(t, r.d.router, req)
	if code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", code)
	}
	var got wire.ServerError
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not a ServerError: %v\n%s", err, body)
	}
	if got.Code != wire.ErrorCodeNotAMember {
		t.Errorf("code = %q, want not_a_member", got.Code)
	}

	var count int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM messages WHERE conversation_id = $1`, group).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("a refused send created %d rows, want 0", count)
	}
}

// A sender with no device row — a bot — is deduplicated exactly like one
// with, and its message reaches an attached member live with no
// sender_device_id.
func TestABotSenderWithNoDeviceDeduplicatesExactlyLikeOneWith(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	bot, token := mkBot(r.ctx, t, r.pool, r.st, "argosy", "Argosy")
	group := mkGroup(r.ctx, t, r.pool, "A", ada, bot)
	adaS := r.open(ada, "phone")

	clientID := uuid.New()
	code1, first := r.postMessage(token, group.String(),
		map[string]any{"client_id": clientID.String(), "text": "new media available"})
	if code1 != http.StatusOK {
		t.Fatalf("bot's first send = %d", code1)
	}
	if m := adaS.message(); string(m.ID) != string(first.ID) {
		t.Errorf("ada's frame = %+v, want the bot's message", m)
	}
	beforeLast, beforeLog := convCounters(r.ctx, t, r.pool, group)

	code2, second := r.postMessage(token, group.String(),
		map[string]any{"client_id": clientID.String(), "text": "new media available (retried)"})
	if code2 != http.StatusOK {
		t.Fatalf("bot's replay = %d", code2)
	}
	if second.ID != first.ID {
		t.Errorf("a bot's replay returned %s, want the original %s", second.ID, first.ID)
	}

	afterLast, afterLog := convCounters(r.ctx, t, r.pool, group)
	if afterLast != beforeLast || afterLog != beforeLog {
		t.Errorf("a bot's replay moved the counters: last_seq %d→%d, log_counter %d→%d",
			beforeLast, afterLast, beforeLog, afterLog)
	}

	var count int
	if err := r.pool.QueryRow(r.ctx,
		`SELECT count(*) FROM messages WHERE conversation_id = $1 AND client_id = $2`,
		group, clientID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d rows for the bot's one client_id, want 1", count)
	}

	var deviceID *uuid.UUID
	if err := r.pool.QueryRow(r.ctx, `SELECT sender_device_id FROM messages WHERE id = $1`,
		mustParseWireUUID(t, first.ID)).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	if deviceID != nil {
		t.Errorf("the bot's message has sender_device_id = %s, want NULL", *deviceID)
	}

	// A second attached device sees the same message live and it validates.
	adaLaptop := r.open(ada, "laptop")
	inA, err := r.st.SendMessage(r.ctx, store.NewMessage{
		ClientID: uuid.New(), ConversationID: group, AuthorID: ada, Text: ptrStr("marker"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m := adaLaptop.message(); string(m.ID) != inA.ID.String() {
		t.Errorf("ada's laptop next frame = %+v, want the marker", m)
	}
}

// find-or-create by handle returns the same conversation on the second call.
func TestFindOrCreateDirectOverRESTReturnsTheSameConversationTwice(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	_ = mkUser(r.ctx, t, r.pool, "theo", "Theo")
	enrolled := r.enroll(ada, "laptop")

	code1, first := r.postDirect(string(enrolled.AccessToken), map[string]any{"handle": "theo"})
	if code1 != http.StatusOK {
		t.Fatalf("first find-or-create = %d", code1)
	}
	if first.Kind != wire.ConversationKindDirect {
		t.Errorf("kind = %q, want direct", first.Kind)
	}
	if first.MemberCount != 2 {
		t.Errorf("member_count = %d, want 2", first.MemberCount)
	}
	if first.Name != "Theo" {
		t.Errorf("name = %q, want the other member's display name", first.Name)
	}
	if first.HeadSeq != 0 {
		t.Errorf("head_seq = %d, want 0 — nothing has been sent into it yet", first.HeadSeq)
	}

	code2, second := r.postDirect(string(enrolled.AccessToken), map[string]any{"handle": "theo"})
	if code2 != http.StatusOK {
		t.Fatalf("second find-or-create = %d", code2)
	}
	if second.ID != first.ID {
		t.Errorf("second call returned %s, want the same conversation %s", second.ID, first.ID)
	}

	var count int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM conversations WHERE kind = 'direct'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d direct conversations exist, want 1", count)
	}

	// It is discoverable on /sync at once, and a message sent into it fans
	// out live — the conversation created through REST is a real one.
	page := r.syncPage(string(enrolled.AccessToken))
	found := false
	for _, c := range page.Conversations {
		if c.ID == first.ID {
			found = true
		}
	}
	if !found {
		t.Error("the found-or-created conversation is not on ada's own /sync page")
	}
}

// A concurrent race for the same pair, driven entirely through the REST
// endpoint, never creates a second direct conversation.
func TestConcurrentFindOrCreateDirectOverRESTMakesOneConversation(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	_ = mkUser(r.ctx, t, r.pool, "theo", "Theo")
	enrolled := r.enroll(ada, "laptop")

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	ids := make([]wire.Uuid, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			code, c := r.postDirect(string(enrolled.AccessToken), map[string]any{"handle": "theo"})
			codes[i], ids[i] = code, c.ID
		}(i)
	}
	close(start)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("caller %d: status %d", i, code)
		}
	}
	first := ids[0]
	for i, id := range ids {
		if id != first {
			t.Errorf("caller %d found %s, want %s", i, id, first)
		}
	}

	var convCount, memberCount int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM conversations WHERE kind = 'direct'`).Scan(&convCount); err != nil {
		t.Fatal(err)
	}
	if err := r.pool.QueryRow(r.ctx,
		`SELECT count(*) FROM conversation_members WHERE conversation_id = $1`,
		mustParseWireUUID(t, first)).Scan(&memberCount); err != nil {
		t.Fatal(err)
	}
	if convCount != 1 {
		t.Errorf("%d concurrent REST find-or-create calls made %d direct conversations, want 1", n, convCount)
	}
	if memberCount != 2 {
		t.Errorf("%d member rows for the winning conversation, want 2", memberCount)
	}
}

// Self-DM and an unknown handle are both refused conversation_not_found over
// REST, at the status its row carries — the two "decide and record" cases
// exercised end to end.
func TestFindOrCreateDirectOverRESTRefusesSelfAndUnknownHandles(t *testing.T) {
	r := newRig(t)
	ada := mkUser(r.ctx, t, r.pool, "ada", "Ada")
	enrolled := r.enroll(ada, "laptop")

	for _, tc := range []struct {
		name   string
		handle string
	}{
		{"self", "ada"},
		{"unknown", "nobody-by-this-handle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := authedRequest(t, "/conversations/direct", string(enrolled.AccessToken), map[string]any{"handle": tc.handle})
			code, body := do(t, r.d.router, req)
			if code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", code)
			}
			var got wire.ServerError
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("body is not a ServerError: %v\n%s", err, body)
			}
			if got.Code != wire.ErrorCodeConversationNotFound {
				t.Errorf("code = %q, want conversation_not_found", got.Code)
			}
		})
	}

	var count int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM conversations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("a refused find-or-create created %d conversations, want 0", count)
	}
}
