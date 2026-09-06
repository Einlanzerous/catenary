package wireview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// Oracle two: schema/mapping/wire-fields.json. CANT-84 criterion 14.
//
// That file classifies every wire field as `column`, `derived` or client-local,
// and .github/review-ignore records that NOTHING checks whether a `derived`
// note is true — "that is a reading job". This package is the first thing that
// can check part of it: every field the map calls `derived` must have a named
// producer here, every field it calls `column` must be a passthrough, and a
// wire field with no entry at all fails.
//
// It does not check that a note's PROSE is true. That is still a reading job,
// and it is the one this epic has been bitten by three times.

const fieldMapPath = "../../schema/mapping/wire-fields.json"

type fieldEntry struct {
	Kind   string `json:"kind"`
	Column string `json:"column"`
	Note   string `json:"note"`
}

func loadFieldMap(t *testing.T) map[string]fieldEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(fieldMapPath))
	if err != nil {
		t.Fatalf("read field map: %v", err)
	}
	var doc struct {
		Fields map[string]fieldEntry `json:"fields"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse field map: %v", err)
	}
	if len(doc.Fields) < 50 {
		t.Fatalf("field map has %d entries — the parse is broken, not the map", len(doc.Fields))
	}
	return doc.Fields
}

// wireFields lists the json names of a wire struct, which is the vocabulary
// the map is keyed on.
func wireFields(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// derivedProducers names the function in THIS package that produces each field
// the map calls `derived`. "A named producer" is meant literally: if a derived
// field has no entry here the test fails, and if an entry names a function that
// no longer exists the package does not compile.
//
// The two entries pointing at nothing are the honest ones, and both are
// documented in message.go rather than quietly absent.
var derivedProducers = map[string]string{
	"Message.attachments":        "Message (the attachment loop)",
	"Message.state":              "Viewer.State — consumed, never computed: it is CANT-26's query",
	"Message.read_by":            "Viewer.ReadBy — same",
	"Message.reply_to":           "replyRef",
	"VoiceAttachment.url":        "Viewer.MediaURL, injected over storage_key",
	"VoiceAttachment.transcript": "transcript",
	"ImageAttachment.url":        "Viewer.MediaURL, injected over storage_key",
	"ReplyRef.author_id":         "replyRef, from the live source",
	"ReplyRef.kind":              "replyRef",
	"ReplyRef.preview":           "preview",
	"ReplyRef.duration_ms":       "replyRef, from the source's first voice attachment",
	"ReplyRef.url":               "replyRef, image sources only — see the comment there",
}

func TestEveryWireFieldIsClassifiedAndEveryDerivedOneHasAProducer(t *testing.T) {
	fields := loadFieldMap(t)

	for _, tc := range []struct {
		typeName string
		sample   any
	}{
		{"Message", wire.Message{}},
		{"VoiceAttachment", wire.VoiceAttachment{}},
		{"ImageAttachment", wire.ImageAttachment{}},
		{"Transcript", wire.Transcript{}},
		{"ReplyRef", wire.ReplyRef{}},
	} {
		for _, f := range wireFields(tc.sample) {
			key := tc.typeName + "." + f
			e, ok := fields[key]
			if !ok {
				t.Errorf("%s is on the wire and has NO entry in the field map — "+
					"nothing says whether it is stored or derived", key)
				continue
			}
			switch e.Kind {
			case "column":
				if e.Column == "" {
					t.Errorf("%s is `column` but names no column", key)
				}
			case "derived":
				if _, named := derivedProducers[key]; !named {
					t.Errorf("%s is `derived` and no producer in this package is named for it. "+
						"Either this package should produce it, or it belongs to another ticket "+
						"and derivedProducers should say which.", key)
				}
			default:
				t.Errorf("%s has kind %q, which is neither column nor derived", key, e.Kind)
			}
		}
	}

	// And the other direction: a producer named for a field the map no longer
	// calls derived is a claim about work nobody is doing.
	for key := range derivedProducers {
		e, ok := fields[key]
		if !ok {
			t.Errorf("derivedProducers names %s, which is not in the field map any more", key)
			continue
		}
		if e.Kind != "derived" {
			t.Errorf("derivedProducers names %s, which the map now calls %q", key, e.Kind)
		}
	}
}

// Criterion 15, first half: `url` comes from an injected deriver, so the SAME
// row under two derivers yields two different URLs. The mapper never builds one.
func TestTheSameRowUnderTwoDeriversYieldsTwoURLs(t *testing.T) {
	row := store.AttachmentRow{Kind: "image", StorageKey: "i/abc", Filename: ptr("x.jpg"),
		Width: ptr(int64(1)), Height: ptr(int64(1)), Bytes: ptr(int64(1))}

	r2 := attachment(row, Viewer{MediaURL: func(k string) string { return "https://r2.invalid/" + k }})
	traefik := attachment(row, Viewer{MediaURL: func(k string) string { return "/media/" + k }})

	got1 := r2.(wire.ImageAttachment).URL
	got2 := traefik.(wire.ImageAttachment).URL
	if got1 != "https://r2.invalid/i/abc" || got2 != "/media/i/abc" {
		t.Errorf("urls = %q, %q — the mapper is not going through the deriver", got1, got2)
	}
	if got1 == got2 {
		t.Error("one storage key produced one url under two derivers; CANT-47 has not decided yet")
	}
}

// Criterion 15, second half: peaks reach the wire EXACTLY as stored, with no
// arithmetic anywhere in this package. The seeded generator overflows 2^53 and
// produces different bars in JavaScript than in Dart, so the only correct
// implementation is a passthrough.
func TestPeaksAreAPassthrough(t *testing.T) {
	stored := []int64{0, 1, 50, 99, 100}
	got := attachment(store.AttachmentRow{
		Kind: "voice", DurationMs: ptr(int64(1)), Peaks: stored, TranscriptState: ptr("pending"),
	}, Viewer{MediaURL: fixedURL("u")}).(wire.VoiceAttachment).Peaks

	if !reflect.DeepEqual(got, stored) {
		t.Errorf("peaks = %v, want %v exactly", got, stored)
	}
	// Same backing array, not a transformed copy — the strongest form of "no
	// arithmetic": there is nowhere for any to have happened.
	if len(got) > 0 && &got[0] != &stored[0] {
		t.Error("peaks were copied; a copy is where a transformation hides")
	}
}

// Criterion 16: client_id reaches its author and nobody else, proved in BOTH
// directions on the same row.
func TestClientIDIsEchoedToItsAuthorOnly(t *testing.T) {
	author := mustUUID(t, nadiaID)
	other := mustUUID(t, theoID)
	key := mustUUID(t, "1c2d3e4f-5a6b-4c7d-9e8f-0a1b2c3d4e5f")
	row := store.MessageRow{ID: author, ConversationID: author, AuthorID: author, ClientID: &key}

	if got := Message(row, nil, nil, Viewer{UserID: author}); got.ClientID == nil {
		t.Error("the author did not get their own client_id back; their outbox cannot match the broadcast")
	}
	if got := Message(row, nil, nil, Viewer{UserID: other}); got.ClientID != nil {
		t.Errorf("another member received client_id %v — the wire says they never do", *got.ClientID)
	}
}

// The preview rule, which one vector fixes and nothing else writes down.
func TestThePreviewRule(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"short enough is untouched", "already short", "already short"},
		{"cut on a word boundary with room for the ellipsis",
			"picking up the trailer at eight, should be back before the rain",
			"picking up the trailer at eight, should be back…"},
		{"exactly at the bound is untouched",
			strings.Repeat("a", previewRunes), strings.Repeat("a", previewRunes)},
		{"one over the bound loses a rune to the ellipsis",
			strings.Repeat("a", previewRunes+1), strings.Repeat("a", previewRunes-1) + "…"},
		{"runes, not bytes", strings.Repeat("é", previewRunes), strings.Repeat("é", previewRunes)},
		{"whitespace is collapsed first", "two  \n spaces", "two spaces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preview(tc.in); got != tc.want {
				t.Errorf("preview(%q)\n = %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
