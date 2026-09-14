package api

// CANT-75 — the two REST routes, unit-tested against fake Deps the way
// sync_test.go tests GET /sync: route registration is conditional on every
// seam it needs, a malformed request is a 400 and not a wire code, an
// authentication failure is the one shared 401 body, and a store refusal
// answers with the code AND the status senderror.go's row carries — never one
// decided here.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

func post(t *testing.T, h http.Handler, url string, body any) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, url, r))
	res := rec.Result()
	got, _ := io.ReadAll(res.Body)
	return res, string(got)
}

func someFullCaller(*http.Request) (store.Caller, bool) {
	return store.Caller{UserID: uuid.New(), DeviceID: uuid.New()}, true
}

func noMediaURL(storageKey string) string { return storageKey }

func okSend(context.Context, store.NewMessage) (store.Sent, error) {
	return store.Sent{ID: uuid.New(), ConversationID: uuid.New(), Seq: 1, LogSeq: 1, At: time.Now()}, nil
}

func okFanout(context.Context, uuid.UUID, int64) (store.FanoutMessage, error) {
	return store.FanoutMessage{
		Message: store.MessageRow{
			ID: uuid.New(), ConversationID: uuid.New(), AuthorID: uuid.New(),
			Seq: 1, LogSeq: 1, At: time.Now(),
		},
		ReadBy: 1,
	}, nil
}

func okFindOrCreateDirect(context.Context, uuid.UUID, string) (store.ConversationRow, error) {
	return store.ConversationRow{ID: uuid.New(), Kind: "direct", MemberCount: 2, LastSeq: 0}, nil
}

// --- registration ------------------------------------------------------------

func TestRESTSendIsNotRegisteredWithoutEverySeam(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps Deps
	}{
		{"nothing wired", Deps{Logger: discardLogger()}},
		{"send but no caller", Deps{Logger: discardLogger(), Send: okSend, MessageForFanout: okFanout}},
		{"send and caller but no fanout", Deps{Logger: discardLogger(), Send: okSend, Caller: someFullCaller}},
		{"caller and fanout but no send", Deps{Logger: discardLogger(), Caller: someFullCaller, MessageForFanout: okFanout}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := post(t, NewRouter(tc.deps), "/conversations/"+uuid.NewString()+"/messages", map[string]any{"client_id": uuid.NewString()})
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404 — the route must not exist without every seam", res.StatusCode)
			}
		})
	}
}

func TestFindOrCreateDirectIsNotRegisteredWithoutEverySeam(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps Deps
	}{
		{"nothing wired", Deps{Logger: discardLogger()}},
		{"find-or-create but no caller", Deps{Logger: discardLogger(), FindOrCreateDirect: okFindOrCreateDirect}},
		{"caller but no find-or-create", Deps{Logger: discardLogger(), Caller: someFullCaller}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := post(t, NewRouter(tc.deps), "/conversations/direct", map[string]any{"handle": "theo"})
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404 — the route must not exist without every seam", res.StatusCode)
			}
		})
	}
}

// --- authentication ------------------------------------------------------------

func unauthCaller(*http.Request) (store.Caller, bool) { return store.Caller{}, false }

func TestRESTSendRefusesAnUnidentifiedCaller(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), Send: okSend, MessageForFanout: okFanout, Caller: unauthCaller})
	res, body := post(t, h, "/conversations/"+uuid.NewString()+"/messages", map[string]any{"client_id": uuid.NewString()})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil || got["code"] != "unauthorized" {
		t.Errorf("body = %s, want the shared unauthorized body", body)
	}
}

func TestFindOrCreateDirectRefusesAnUnidentifiedCaller(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), FindOrCreateDirect: okFindOrCreateDirect, Caller: unauthCaller})
	res, body := post(t, h, "/conversations/direct", map[string]any{"handle": "theo"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil || got["code"] != "unauthorized" {
		t.Errorf("body = %s, want the shared unauthorized body", body)
	}
}

// --- malformed requests --------------------------------------------------------

func TestAMalformedConversationIDIsABadRequest(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), Send: okSend, MessageForFanout: okFanout, Caller: someFullCaller})
	res, _ := post(t, h, "/conversations/not-a-uuid/messages", map[string]any{"client_id": uuid.NewString()})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestAMalformedSendBodyIsABadRequest(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), Send: okSend, MessageForFanout: okFanout, Caller: someFullCaller})
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"missing client_id", `{"text":"hi"}`},
		{"client_id is not a uuid", `{"client_id":"nope"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/conversations/"+uuid.NewString()+"/messages", bytes.NewReader([]byte(tc.body))))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestAMalformedDirectConversationBodyIsABadRequest(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), FindOrCreateDirect: okFindOrCreateDirect, Caller: someFullCaller})
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"missing handle", `{}`},
		{"empty handle", `{"handle":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/conversations/direct", bytes.NewReader([]byte(tc.body))))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// --- the REST body bound follows Deps.MaxRESTBodyBytes ------------------------

// A review finding on this PR: a FIXED REST body constant, independent of
// the configured message bound, let an operator raise
// CATENARY_MAX_MESSAGE_BYTES and have a message the store would have
// accepted refused by the TRANSPORT instead. Deps.MaxRESTBodyBytes exists so
// the two move together; this proves the handler actually reads it rather
// than a package constant.
func TestRESTBodyLimitFollowsMaxRESTBodyBytes(t *testing.T) {
	// A handle long enough to clear a tiny configured bound but well under
	// the default (256 KiB) — so the DEFAULT would accept this request and
	// only a configured, smaller bound refuses it.
	handle := string(bytes.Repeat([]byte("a"), 200))
	reqBody := map[string]any{"handle": handle}

	tooSmall := NewRouter(Deps{
		Logger: discardLogger(), FindOrCreateDirect: okFindOrCreateDirect, Caller: someFullCaller,
		MaxRESTBodyBytes: 100, // smaller than the body this request carries
	})
	res, body := post(t, tooSmall, "/conversations/direct", reqBody)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("with MaxRESTBodyBytes=100, status = %d, want 400: %s", res.StatusCode, body)
	}

	roomy := NewRouter(Deps{
		Logger: discardLogger(), FindOrCreateDirect: okFindOrCreateDirect, Caller: someFullCaller,
		MaxRESTBodyBytes: 1 << 20, // comfortably above this request's body
	})
	res, body = post(t, roomy, "/conversations/direct", reqBody)
	if res.StatusCode != http.StatusOK {
		t.Errorf("with MaxRESTBodyBytes=1MiB, status = %d, want 200: %s", res.StatusCode, body)
	}

	// Zero (unset) falls back to the default, which is comfortably above
	// this 200-byte-ish request too — the ordinary case for a store built
	// outside the composition root, e.g. these very tests.
	defaulted := NewRouter(Deps{Logger: discardLogger(), FindOrCreateDirect: okFindOrCreateDirect, Caller: someFullCaller})
	res, body = post(t, defaulted, "/conversations/direct", reqBody)
	if res.StatusCode != http.StatusOK {
		t.Errorf("with the default bound, status = %d, want 200: %s", res.StatusCode, body)
	}
}

// --- success shape ---------------------------------------------------------

// The response is a wire.Message the generated decoder accepts — the
// transport's half of CANT-75's "indistinguishable from the socket" claim —
// built through wireview.Message from Deps.MessageForFanout's own read,
// never from the request.
func TestARESTSendResponseIsAValidatingMessage(t *testing.T) {
	h := NewRouter(Deps{
		Logger: discardLogger(), Send: okSend, MessageForFanout: okFanout,
		Caller: someFullCaller, MediaURL: noMediaURL,
	})
	res, body := post(t, h, "/conversations/"+uuid.NewString()+"/messages", map[string]any{"client_id": uuid.NewString(), "text": "hi"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	if _, err := wire.DecodeNamed("Message", []byte(body)); err != nil {
		t.Fatalf("the served body does not validate as Message: %v\n%s", err, body)
	}
}

func TestADirectConversationResponseIsAValidatingConversation(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), FindOrCreateDirect: okFindOrCreateDirect, Caller: someFullCaller})
	res, body := post(t, h, "/conversations/direct", map[string]any{"handle": "theo"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	if _, err := wire.DecodeNamed("Conversation", []byte(body)); err != nil {
		t.Fatalf("the served body does not validate as Conversation: %v\n%s", err, body)
	}
}

// --- a bot has no device --------------------------------------------------------

// A bot's Caller carries DeviceID uuid.Nil (store.Caller.IsBot's own shape),
// and the handler must not turn that into a pointer to the zero UUID.
func TestARESTSendFromABotCarriesNoSenderDeviceID(t *testing.T) {
	var got store.NewMessage
	send := func(_ context.Context, m store.NewMessage) (store.Sent, error) {
		got = m
		return store.Sent{ID: uuid.New(), ConversationID: m.ConversationID, Seq: 1, LogSeq: 1, At: time.Now()}, nil
	}
	botCaller := func(*http.Request) (store.Caller, bool) {
		return store.Caller{UserID: uuid.New(), DeviceID: uuid.Nil, Kind: "bot"}, true
	}
	h := NewRouter(Deps{
		Logger: discardLogger(), Send: send, MessageForFanout: okFanout,
		Caller: botCaller, MediaURL: noMediaURL,
	})
	res, body := post(t, h, "/conversations/"+uuid.NewString()+"/messages", map[string]any{"client_id": uuid.NewString(), "text": "new media available"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	if got.SenderDeviceID != nil {
		t.Errorf("SenderDeviceID = %v, want nil for a bot", *got.SenderDeviceID)
	}
}

// --- refusals carry the table's code AND status ------------------------------

func TestARESTSendRefusalCarriesTheTablesCodeAndStatus(t *testing.T) {
	send := func(context.Context, store.NewMessage) (store.Sent, error) {
		return store.Sent{}, store.ErrNotAMember
	}
	h := NewRouter(Deps{
		Logger: discardLogger(), Send: send, MessageForFanout: okFanout,
		Caller: someFullCaller, MediaURL: noMediaURL,
	})
	cid := uuid.NewString()
	res, body := post(t, h, "/conversations/"+uuid.NewString()+"/messages", map[string]any{"client_id": cid})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (not_a_member's row)", res.StatusCode)
	}
	var got wire.ServerError
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not a ServerError: %v\n%s", err, body)
	}
	if got.Code != wire.ErrorCodeNotAMember {
		t.Errorf("code = %q, want not_a_member", got.Code)
	}
	if got.ClientID == nil || string(*got.ClientID) != cid {
		t.Errorf("client_id = %v, want %s — a refused send names which outbox entry failed", got.ClientID, cid)
	}
}

func TestFindOrCreateDirectRefusalCarriesTheTablesCodeAndStatus(t *testing.T) {
	find := func(context.Context, uuid.UUID, string) (store.ConversationRow, error) {
		return store.ConversationRow{}, store.ErrSelfDirect
	}
	h := NewRouter(Deps{Logger: discardLogger(), FindOrCreateDirect: find, Caller: someFullCaller})
	res, body := post(t, h, "/conversations/direct", map[string]any{"handle": "self"})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (conversation_not_found's row)", res.StatusCode)
	}
	var got wire.ServerError
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not a ServerError: %v\n%s", err, body)
	}
	if got.Code != wire.ErrorCodeConversationNotFound {
		t.Errorf("code = %q, want conversation_not_found", got.Code)
	}
}
