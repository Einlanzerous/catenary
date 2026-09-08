package wireview

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// The strongest form of CANT-20's "the captured response still validates":
// round-trip OUR OWN output through the decoder, which since CANT-25 enforces
// every constraint the schema states rather than merely parsing.
//
// A response that encodes but would be refused by our own clients is the exact
// failure the three-runner conformance suite exists to prevent, reached from
// the producing side.
func TestAnAssembledSyncResponseValidatesAgainstTheDecoder(t *testing.T) {
	author := mustUUID(t, nadiaID)
	reader := mustUUID(t, theoID)
	conv := mustUUID(t, convID)
	at := mustTime(t, "2026-08-17T04:22:03.117Z")

	page := store.SyncPage{
		Messages: []store.MessageRow{{
			ID:             mustUUID(t, "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"),
			ConversationID: conv, AuthorID: author, Seq: 1905, LogSeq: 41251, At: at,
			Text: ptr("the wire is up"),
		}},
		Attachments:  map[uuid.UUID][]store.AttachmentRow{},
		ReplySources: map[uuid.UUID]store.ReplySource{},
		Conversations: []store.ConversationRow{{
			ID: conv, Kind: "group", Name: ptr("Sunday Dinner"),
			LastSeq: 1908, MemberCount: 7, FirstUnreadSeq: ptr(int64(1906)),
		}},
		Users: []store.UserRow{
			{ID: author, DisplayName: "Nadia Ruiz"},
			{ID: reader, DisplayName: "Theo"},
		},
		HighWater: 41254,
		HasMore:   true,
	}

	got := Sync(page, SyncViewer{
		UserID: reader, ReadSeq: map[uuid.UUID]int64{conv: 1905}, MediaURL: noMedia,
	}, "2026-08-17T04:32:00.000Z")

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := wire.DecodeNamed("SyncResponse", raw); err != nil {
		t.Fatalf("our own SyncResponse does not validate: %v\n%s", err, raw)
	}
}

// The three arrays are REQUIRED on the wire, and `null` is not an empty array
// to a decoder that validates — which all three now do. An empty page is the
// commonest response a caught-up client gets, so it is the one most likely to
// be shipped broken.
func TestAnEmptyPageEncodesAsEmptyArraysAndStillValidates(t *testing.T) {
	got := Sync(store.SyncPage{HighWater: 7}, SyncViewer{
		UserID: mustUUID(t, theoID), MediaURL: noMedia,
	}, "2026-08-17T04:32:00.000Z")

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, field := range []string{`"messages":[]`, `"conversations":[]`, `"users":[]`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("empty page is missing %s: %s", field, raw)
		}
	}
	if _, err := wire.DecodeNamed("SyncResponse", raw); err != nil {
		t.Fatalf("an empty page does not validate: %v\n%s", err, raw)
	}
}

// `initials` is server-derived so web and Flutter cannot disagree — the same
// reason peaks are computed once. The rule is fixed by the sync_response
// vector, which is the only place it is written down.
func TestInitialsFollowTheVectorsRule(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"Nadia Ruiz", "NR"}, // the vector
		{"Theo", "TH"},       // the vector: one word gives two letters
		{"ada lovelace king", "AL"},
		{"cher", "CH"},
		{"J", "J"},
		{"", ""},
		{"  ", ""},
		{"Jean-Luc Picard", "JL"}, // punctuation is a separator, not a glyph
		{"陳 大文", "陳大"},
	} {
		if got := initials(tc.name); got != tc.want {
			t.Errorf("initials(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A direct conversation has no stored name — 0002 declines to store one because
// the rail shows the OTHER member, which is per reader. The wire requires
// `name`, so the store resolves it and the mapper uses it.
func TestADirectConversationIsNamedForTheOtherMember(t *testing.T) {
	got := conversation(store.ConversationRow{
		ID: mustUUID(t, convID), Kind: "direct", Name: nil,
		OtherMemberName: ptr("Nadia Ruiz"), LastSeq: 3, MemberCount: 2,
	})
	if got.Name != "Nadia Ruiz" {
		t.Errorf("name = %q, want the other member's — a direct has no stored name", got.Name)
	}
}
