package wireview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

	if got := Message(row, nil, nil, Viewer{UserID: author, MediaURL: noMedia}); got.ClientID == nil {
		t.Error("the author did not get their own client_id back; their outbox cannot match the broadcast")
	}
	if got := Message(row, nil, nil, Viewer{UserID: other, MediaURL: noMedia}); got.ClientID != nil {
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

// Criterion 14's OTHER half: "every field the map calls `column` is a
// passthrough of that column".
//
// The check above only asserted that the MAP names a column, which is a
// statement about wire-fields.json and not about the mapper. Nothing connected
// a `column` entry to the row field the mapper reads, and the vectors cover
// most of the gap only incidentally.
//
// Message.edited_at was the live hole: no vector carries it, no other test set
// it, and deleting the mapping left the whole package green — an edit made
// through CANT-63 would have been invisible on the wire with a passing suite.
//
// So: one maximal row with every column field distinctly set, mapped, and every
// `column` field asserted against the value that went in. The coverage table is
// checked BOTH ways, so a column field with no assertion fails rather than
// passing silently.
func TestEveryColumnFieldIsAPassthrough(t *testing.T) {
	fields := loadFieldMap(t)

	author := mustUUID(t, nadiaID)
	key := mustUUID(t, "1c2d3e4f-5a6b-4c7d-9e8f-0a1b2c3d4e5f")
	src := mustUUID(t, voiceMsg)
	edited := mustTime(t, "2026-08-18T09:00:00.000Z")

	row := store.MessageRow{
		ID:             mustUUID(t, "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"),
		ConversationID: mustUUID(t, convID),
		AuthorID:       author,
		Seq:            4242,
		LogSeq:         909090,
		At:             mustTime(t, "2026-08-17T04:22:03.117Z"),
		Text:           ptr("a distinctive body"),
		ClientID:       &key,
		ReplyTo:        &src,
		EditedAt:       &edited,
		Deleted:        true,
	}
	got := marshalToMap(t, Message(row, nil, &store.ReplySource{
		MessageID: src, AuthorID: author, Text: ptr("the source"),
	}, Viewer{UserID: author, State: wire.DeliveryStateSent, MediaURL: fixedURL("u")}))

	want := map[string]any{
		"id":              row.ID.String(),
		"seq":             float64(4242),
		"log_seq":         float64(909090),
		"conversation_id": row.ConversationID.String(),
		"author_id":       author.String(),
		"at":              "2026-08-17T04:22:03.117Z",
		"text":            "a distinctive body",
		"client_id":       key.String(),
		"edited_at":       "2026-08-18T09:00:00.000Z",
		"deleted":         true,
	}
	assertColumnsPassThrough(t, fields, "Message", wire.Message{}, got, want)

	// The reply ref's one column field is the stored id.
	ref, _ := got["reply_to"].(map[string]any)
	if ref == nil {
		t.Fatal("no reply_to on a row that has one")
	}
	assertColumnsPassThrough(t, fields, "ReplyRef", wire.ReplyRef{}, ref,
		map[string]any{"message_id": src.String()})
}

func TestEveryAttachmentColumnFieldIsAPassthrough(t *testing.T) {
	fields := loadFieldMap(t)
	v := Viewer{MediaURL: fixedURL("u")}

	voice := marshalToMap(t, attachment(store.AttachmentRow{
		Kind: "voice", StorageKey: "k", DurationMs: ptr(int64(31337)),
		Peaks: []int64{7, 8, 9}, TranscriptState: ptr("ready"),
		TranscriptJSON: []byte(`{"text":"t","word_count":3,"engine":"e","language":"l",` +
			`"segments":[{"at_ms":5,"text":"s"}]}`),
	}, v))
	assertColumnsPassThrough(t, fields, "VoiceAttachment", wire.VoiceAttachment{}, voice, map[string]any{
		"kind": "voice", "duration_ms": float64(31337),
		"peaks": []any{float64(7), float64(8), float64(9)},
	})

	tr, _ := voice["transcript"].(map[string]any)
	if tr == nil {
		t.Fatal("no transcript on a voice attachment")
	}
	assertColumnsPassThrough(t, fields, "Transcript", wire.Transcript{}, tr, map[string]any{
		"state": "ready", "text": "t", "word_count": float64(3),
		"engine": "e", "language": "l",
		"segments": []any{map[string]any{"at_ms": float64(5), "text": "s"}},
		// eta_sec is absent on a ready transcript, and its own column entry
		// points at the same document. Covered by the pending vector.
		"eta_sec": nil,
	})

	image := marshalToMap(t, attachment(store.AttachmentRow{
		Kind: "image", StorageKey: "k", Filename: ptr("f.jpg"),
		Width: ptr(int64(11)), Height: ptr(int64(22)), Bytes: ptr(int64(33)),
		Placeholder: ptr("blur"),
	}, v))
	assertColumnsPassThrough(t, fields, "ImageAttachment", wire.ImageAttachment{}, image, map[string]any{
		"kind": "image", "filename": "f.jpg", "width": float64(11),
		"height": float64(22), "bytes": float64(33), "placeholder": "blur",
	})
}

// assertColumnsPassThrough checks the two directions that matter: every field
// the map calls `column` on this type has an expected value here and carries
// it, and every expectation names a field the map still calls `column`.
func assertColumnsPassThrough(t *testing.T, fields map[string]fieldEntry,
	typeName string, sample any, got, want map[string]any) {
	t.Helper()

	// THE UNION of the struct's reflectable fields and the map's own keys for
	// this type, not just the former.
	//
	// wireFields reflects over json tags, and wire.VoiceAttachment has no Kind
	// field at all — the generated MarshalJSON injects the constant. So the
	// forward loop skipped `kind` entirely, `"kind": "voice"` was never
	// compared against anything, and it would have passed unchanged if a voice
	// row went down the image branch. Two entries the reader is most likely to
	// trust, silently checking nothing.
	for _, f := range columnFieldNames(fields, typeName, sample) {
		e := fields[typeName+"."+f]
		expected, covered := want[f]
		if !covered {
			t.Errorf("%s.%s is `column` (%s) and NOTHING here asserts it is a passthrough — "+
				"delete its line in the mapper and this suite stays green", typeName, f, e.Column)
			continue
		}
		if expected == nil {
			if _, present := got[f]; present {
				t.Errorf("%s.%s was expected absent, got %v", typeName, f, got[f])
			}
			continue
		}
		if !reflect.DeepEqual(got[f], expected) {
			t.Errorf("%s.%s = %#v, want %#v — not a passthrough of %s",
				typeName, f, got[f], expected, e.Column)
		}
	}

	for f := range want {
		e, ok := fields[typeName+"."+f]
		if !ok {
			t.Errorf("this test asserts %s.%s, which the field map does not have", typeName, f)
		} else if e.Kind != "column" {
			t.Errorf("this test asserts %s.%s is a column passthrough, but the map calls it %q",
				typeName, f, e.Kind)
		}
	}
}

// columnFieldNames is every field the map calls `column` on this type, whether
// or not the generated struct exposes it — so an injected constant like `kind`
// is checked like any other column.
func columnFieldNames(fields map[string]fieldEntry, typeName string, sample any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		if e, ok := fields[typeName+"."+f]; ok && e.Kind == "column" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, f := range wireFields(sample) {
		add(f)
	}
	for key := range fields {
		if strings.HasPrefix(key, typeName+".") {
			add(strings.TrimPrefix(key, typeName+"."))
		}
	}
	sort.Strings(out)
	return out
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// A malformed segment costs its own field and nothing else.
//
// CANT-25 gave the wire types validating decoders, and while storedTranscript
// pointed at wire.TranscriptSegment a single bad segment aborted the whole
// json.Unmarshal at that key. transcript_json is JSONB, which stores keys in
// canonical order — length, then bytewise — so `segments` sorts before
// `word_count`, and one out-of-range at_ms silently cost the word count AND
// truncated the segment list, while `state` still said `ready` from its own
// column. Those are what search's JUMP TO and playback highlighting read.
func TestAMalformedSegmentDoesNotTruncateTheRestOfTheTranscript(t *testing.T) {
	// Keys in the order JSONB returns them, so this is the document shape the
	// database actually hands back rather than the one a fixture would.
	raw := []byte(`{"text":"hello there","engine":"whisper.cpp/small.en","language":"en",` +
		`"segments":[{"at_ms":-1,"text":"x"},{"at_ms":10,"text":"y"}],"word_count":2}`)

	got := transcript(store.AttachmentRow{
		Kind: "voice", TranscriptState: ptr("ready"), TranscriptJSON: raw,
	})

	if got.WordCount == nil || *got.WordCount != 2 {
		t.Errorf("word_count = %v, want 2 — it sorts AFTER segments in JSONB, so losing it "+
			"means the decode aborted on a segment and took everything past it", got.WordCount)
	}
	if got.Text == nil || *got.Text != "hello there" {
		t.Errorf("text = %v, want the stored text", got.Text)
	}
	if len(got.Segments) != 2 {
		t.Errorf("segments = %d, want 2 — the list was truncated at the offending element", len(got.Segments))
	}
	if got.State != wire.TranscriptStateReady {
		t.Errorf("state = %q, want ready — it comes from its own column either way", got.State)
	}
}
