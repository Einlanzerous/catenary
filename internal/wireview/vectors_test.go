package wireview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// Oracle one: the conformance vectors. CANT-84 criterion 13.
//
// Seven of the eight vectors carrying a Message are things the SERVER could
// legitimately produce. The eighth, unknown_attachment_kind_is_dropped, is a
// DECODER property and is excluded here with its reason rather than left as a
// gap in a count: attachments.kind has a CHECK, so an unknown kind is
// unstorable and this mapper cannot produce one.
//
// No database. Rows in, Message out, compared against the vector.

const vectorsPath = "../../schema/vectors/vectors.json"

func loadVector(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(vectorsPath))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var doc struct {
		Cases []struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	for _, c := range doc.Cases {
		if c.Name != name {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(c.JSON, &m); err != nil {
			t.Fatalf("parse case %s: %v", name, err)
		}
		// A ServerFrame wraps its message; a bare Message vector does not.
		if inner, ok := m["message"].(map[string]any); ok {
			return inner
		}
		if msgs, ok := m["messages"].([]any); ok && len(msgs) > 0 {
			return msgs[0].(map[string]any)
		}
		return m
	}
	t.Fatalf("vector %q not found", name)
	return nil
}

// canonical re-encodes through map[string]any, which sorts keys, so the
// comparison is about VALUES and not about field order.
func canonical(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(round)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid %q: %v", s, err)
	}
	return u
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(wireTimeLayout, s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return ts
}

func ptr[T any](v T) *T { return &v }

// The ids the vectors share.
const (
	convID    = "aa11bb22-cc33-4d44-9e55-ff66aa77bb88"
	nadiaID   = "8d9e0f1a-2b3c-4d4e-9f5a-6b7c8d9e0f1a"
	theoID    = "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e"
	voiceMsg  = "9e0f1a2b-3c4d-4e5f-8a6b-7c8d9e0f1a2b"
	imageURL  = "https://media.catenary.invalid/i/1a2b3c4d.jpg"
	voiceURL  = "https://media.catenary.invalid/v/9e0f1a2b.opus"
	voiceURL2 = "https://media.catenary.invalid/v/0f1a2b3c.opus"
)

// fixedURL stands in for CANT-47's deriver. The mapper never builds a URL
// itself, so a test can hand it any rule at all — which is criterion 15's
// point, and TestTheSameRowUnderTwoDeriversYieldsTwoURLs is the direct proof.
func fixedURL(u string) func(string) string {
	return func(string) string { return u }
}

func TestTheProducibleVectorsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		build  func(t *testing.T) wire.Message
		vector string
	}{
		{
			name:   "server_message_text",
			vector: "server_message_text",
			build: func(t *testing.T) wire.Message {
				author := mustUUID(t, nadiaID)
				return Message(store.MessageRow{
					ID:             mustUUID(t, "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"),
					ConversationID: mustUUID(t, convID),
					AuthorID:       author,
					Seq:            1905,
					LogSeq:         41251,
					At:             mustTime(t, "2026-08-17T04:22:03.117Z"),
					Text:           ptr("the wire is up"),
					ClientID:       ptr(mustUUID(t, "1c2d3e4f-5a6b-4c7d-9e8f-0a1b2c3d4e5f")),
				}, nil, nil, Viewer{UserID: author, State: wire.DeliveryStateSent})
			},
		},
		{
			name:   "sync_response's message — the non-author view",
			vector: "sync_response",
			build: func(t *testing.T) wire.Message {
				// The SAME row as server_message_text, seen by somebody else:
				// state read, and NO client_id. This is the vector that proves
				// the echo rule in both directions.
				return Message(store.MessageRow{
					ID:             mustUUID(t, "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"),
					ConversationID: mustUUID(t, convID),
					AuthorID:       mustUUID(t, nadiaID),
					Seq:            1905,
					LogSeq:         41251,
					At:             mustTime(t, "2026-08-17T04:22:03.117Z"),
					Text:           ptr("the wire is up"),
					ClientID:       ptr(mustUUID(t, "1c2d3e4f-5a6b-4c7d-9e8f-0a1b2c3d4e5f")),
				}, nil, nil, Viewer{UserID: mustUUID(t, theoID), State: wire.DeliveryStateRead})
			},
		},
		{
			name:   "message_at_max_safe_seq",
			vector: "message_at_max_safe_seq",
			build: func(t *testing.T) wire.Message {
				author := mustUUID(t, nadiaID)
				return Message(store.MessageRow{
					ID:             mustUUID(t, "4d5e6f7a-8b9c-4d0e-8f1a-3b4c5d6e7f8a"),
					ConversationID: mustUUID(t, convID),
					AuthorID:       author,
					Seq:            9007199254740991,
					LogSeq:         9007199254740991,
					At:             mustTime(t, "2026-08-17T04:22:03.117Z"),
				}, nil, nil, Viewer{UserID: author, State: wire.DeliveryStateSent})
			},
		},
		{
			name:   "message_tombstone",
			vector: "message_tombstone",
			build: func(t *testing.T) wire.Message {
				return Message(store.MessageRow{
					ID:             mustUUID(t, "5e6f7a8b-9c0d-4e1f-9a2b-4c5d6e7f8a9b"),
					ConversationID: mustUUID(t, convID),
					AuthorID:       mustUUID(t, nadiaID),
					Seq:            1899,
					LogSeq:         41200,
					At:             mustTime(t, "2026-08-16T22:10:00.000Z"),
					Deleted:        true,
				}, nil, nil, Viewer{UserID: mustUUID(t, theoID), State: wire.DeliveryStateRead})
			},
		},
		{
			name:   "server_message_voice_transcript_ready",
			vector: "server_message_voice_transcript_ready",
			build: func(t *testing.T) wire.Message {
				author := mustUUID(t, nadiaID)
				return Message(store.MessageRow{
					ID:             mustUUID(t, voiceMsg),
					ConversationID: mustUUID(t, convID),
					AuthorID:       author,
					Seq:            1906,
					LogSeq:         41252,
					At:             mustTime(t, "2026-08-17T04:24:10.900Z"),
				}, []store.AttachmentRow{{
					Kind:            "voice",
					StorageKey:      "v/9e0f1a2b",
					DurationMs:      ptr(int64(62500)),
					Peaks:           []int64{3, 18, 44, 71, 88, 64, 30, 12, 5},
					TranscriptState: ptr("ready"),
					TranscriptJSON: []byte(`{"text":"picking up the trailer at eight, should be back before the rain",
					  "word_count":12,
					  "segments":[{"at_ms":0,"text":"picking up the trailer at eight,"},
					              {"at_ms":2800,"text":"should be back before the rain"}],
					  "engine":"whisper.cpp/small.en","language":"en"}`),
				}}, nil, Viewer{
					UserID: mustUUID(t, theoID), State: wire.DeliveryStateDelivered,
					ReadBy: ptr(int64(5)), MediaURL: fixedURL(voiceURL),
				})
			},
		},
		{
			name:   "server_message_voice_transcript_pending",
			vector: "server_message_voice_transcript_pending",
			build: func(t *testing.T) wire.Message {
				author := mustUUID(t, nadiaID)
				return Message(store.MessageRow{
					ID:             mustUUID(t, "0f1a2b3c-4d5e-4f6a-9b7c-8d9e0f1a2b3c"),
					ConversationID: mustUUID(t, convID),
					AuthorID:       author,
					Seq:            1907,
					LogSeq:         41253,
					At:             mustTime(t, "2026-08-17T04:25:00.000Z"),
				}, []store.AttachmentRow{{
					Kind:            "voice",
					StorageKey:      "v/0f1a2b3c",
					DurationMs:      ptr(int64(9250)),
					Peaks:           []int64{10, 40, 62, 22},
					TranscriptState: ptr("pending"),
					TranscriptJSON:  []byte(`{"eta_sec":4}`),
				}}, nil, Viewer{
					UserID: author, State: wire.DeliveryStateSent, MediaURL: fixedURL(voiceURL2),
				})
			},
		},
		{
			name:   "server_message_image_with_reply",
			vector: "server_message_image_with_reply",
			build: func(t *testing.T) wire.Message {
				// The reply source is the voice message above, and its preview
				// is derived from the transcript rather than stored — which is
				// what lets a reply back-fill when the transcript lands.
				src := &store.ReplySource{
					MessageID: mustUUID(t, voiceMsg),
					AuthorID:  mustUUID(t, nadiaID),
					FirstAttachment: &store.AttachmentRow{
						Kind:       "voice",
						StorageKey: "v/9e0f1a2b",
						DurationMs: ptr(int64(62500)),
						TranscriptJSON: []byte(`{"text":"picking up the trailer at eight, ` +
							`should be back before the rain"}`),
					},
				}
				return Message(store.MessageRow{
					ID:             mustUUID(t, "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"),
					ConversationID: mustUUID(t, convID),
					AuthorID:       mustUUID(t, theoID),
					Seq:            1908,
					LogSeq:         41254,
					At:             mustTime(t, "2026-08-17T04:31:44.250Z"),
					ReplyTo:        ptr(mustUUID(t, voiceMsg)),
				}, []store.AttachmentRow{{
					Kind:        "image",
					StorageKey:  "i/1a2b3c4d",
					Filename:    ptr("trailer.jpg"),
					Width:       ptr(int64(3024)),
					Height:      ptr(int64(2016)),
					Bytes:       ptr(int64(2874113)),
					Placeholder: ptr("LKO2?U%2Tw=w]~RBVZRi};RPxuwH"),
				}}, src, Viewer{
					UserID: mustUUID(t, nadiaID), State: wire.DeliveryStateRead,
					ReadBy: ptr(int64(7)), MediaURL: fixedURL(imageURL),
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := canonical(t, loadVector(t, tc.vector))
			got := canonical(t, tc.build(t))
			if got != want {
				t.Errorf("the mapped Message does not re-encode to the vector\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// The eighth vector, excluded with its reason rather than silently.
func TestTheDecoderOnlyVectorIsExcludedDeliberately(t *testing.T) {
	m := loadVector(t, "unknown_attachment_kind_is_dropped")
	if m == nil {
		t.Fatal("the vector this test explains is gone; the exclusion above needs revisiting")
	}
	// attachments.kind carries a CHECK (voice | image), so an unknown kind is
	// unstorable and this mapper cannot produce one. It is a property of the
	// generated DECODER, which is CANT-25's, not of this package.
	if a := attachment(store.AttachmentRow{Kind: "hologram"}, Viewer{}); a != nil {
		t.Errorf("an unknown kind produced %#v; it must be dropped, never half-built", a)
	}
}
