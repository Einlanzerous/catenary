package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

func getRaw(t *testing.T, h http.Handler, url string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

// THE ROUTE DOES NOT EXIST while nothing can say who is asking. Not a 401, not
// an empty page — absent.
//
// A sync endpoint that cannot identify its caller would serve one member's log
// to another, and CANT-22's handshake and CANT-29's refresh rotation are both
// Mode C and both unbuilt. The safe absence beats a placeholder that looks
// wired, and this test is what keeps it from quietly becoming one.
func TestSyncIsNotRegisteredWithoutACallerIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps Deps
	}{
		{"neither wired", Deps{Logger: discardLogger()}},
		{"a reader but no caller", Deps{Logger: discardLogger(), Sync: okSync}},
		{"a caller but no reader", Deps{Logger: discardLogger(), CallerID: someCaller}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := getRaw(t, NewRouter(tc.deps), "/sync")
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("GET /sync = %d, want 404 — the route must not exist", res.StatusCode)
			}
		})
	}
}

func okSync(context.Context, uuid.UUID, int64, int) (wire.SyncResponse, error) {
	return wire.SyncResponse{
		Messages: []wire.Message{}, Conversations: []wire.Conversation{},
		Users: []wire.User{}, ServerTime: "2026-08-17T04:32:00.000Z",
	}, nil
}

func someCaller(*http.Request) (uuid.UUID, bool) { return uuid.New(), true }

func TestSyncRefusesAnUnidentifiedCaller(t *testing.T) {
	h := NewRouter(Deps{
		Logger: discardLogger(), Sync: okSync,
		CallerID: func(*http.Request) (uuid.UUID, bool) { return uuid.Nil, false },
	})
	res, _ := getRaw(t, h, "/sync")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.StatusCode)
	}
}

// A malformed cursor is a 400 rather than a silent rewind to 0. Rewinding would
// replay the entire log at a client whose only problem was a bug in its query
// string.
func TestAMalformedCursorIsRefusedRatherThanRewound(t *testing.T) {
	var sawAfter int64 = -1
	h := NewRouter(Deps{
		Logger:   discardLogger(),
		CallerID: someCaller,
		Sync: func(_ context.Context, _ uuid.UUID, after int64, _ int) (wire.SyncResponse, error) {
			sawAfter = after
			return okSync(context.Background(), uuid.Nil, 0, 0)
		},
	})

	for _, q := range []string{"?after=abc", "?after=-1", "?limit=nope", "?limit=-5"} {
		t.Run(q, func(t *testing.T) {
			res, body := getRaw(t, h, "/sync"+q)
			if res.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", res.StatusCode)
			}
			if sawAfter != -1 {
				t.Errorf("the store was called with after=%d despite a malformed query", sawAfter)
			}
			// Not a wire ServerError: the enum has no member for an
			// unparseable query string, and inventing one is a wire change.
			var probe map[string]any
			_ = json.Unmarshal([]byte(body), &probe)
			if _, isCode := probe["code"]; isCode {
				t.Errorf("the 400 body carries a `code`, which reads as a wire ErrorCode: %s", body)
			}
		})
	}
}

// The cursor and the limit reach the store as given, and an absent one is the
// default rather than an error.
func TestTheCursorAndLimitReachTheStore(t *testing.T) {
	var gotAfter int64
	var gotLimit int
	h := NewRouter(Deps{
		Logger:   discardLogger(),
		CallerID: someCaller,
		Sync: func(_ context.Context, _ uuid.UUID, after int64, limit int) (wire.SyncResponse, error) {
			gotAfter, gotLimit = after, limit
			return okSync(context.Background(), uuid.Nil, 0, 0)
		},
	})

	for _, tc := range []struct {
		url   string
		after int64
		limit int
	}{
		{"/sync", 0, 0},
		{"/sync?after=41251", 41251, 0},
		{"/sync?after=41251&limit=50", 41251, 50},
		{"/sync?limit=0", 0, 0},
	} {
		t.Run(tc.url, func(t *testing.T) {
			res, _ := getRaw(t, h, tc.url)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.StatusCode)
			}
			if gotAfter != tc.after || gotLimit != tc.limit {
				t.Errorf("store saw after=%d limit=%d, want %d/%d",
					gotAfter, gotLimit, tc.after, tc.limit)
			}
		})
	}
}

// The served body is a SyncResponse the decoder accepts. The handler encodes
// what the mapper built, so this is the transport's half of the same claim
// wireview's test makes.
func TestTheServedBodyValidates(t *testing.T) {
	h := NewRouter(Deps{Logger: discardLogger(), CallerID: someCaller, Sync: okSync})
	res, body := getRaw(t, h, "/sync")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if _, err := wire.DecodeNamed("SyncResponse", []byte(body)); err != nil {
		t.Fatalf("the served body does not validate: %v\n%s", err, body)
	}
}
