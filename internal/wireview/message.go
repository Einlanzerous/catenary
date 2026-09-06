// Package wireview turns stored rows into the wire's Message, for one viewer.
//
// CANT-84. It is the ONLY thing in the service module that constructs a
// wire.Message, enforced by a guard test — because CANT-75's Done-when says the
// socket's Message and REST's must be indistinguishable, and that is true only
// if one function produces both. Two transports each assembling their own is
// Invariant 2's failure class arriving through the back door.
//
// PURE. Rows and a viewer in, a Message out: no queries, no clock, no
// database. That is what lets its oracle be the conformance vectors rather than
// a fixture, and what makes "does this reader see this field" answerable by
// reading one function.
package wireview

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// Viewer is everything about the READER that changes what they are shown.
//
// State and ReadBy are consumed, never computed: they are per-reader and the
// query behind them is CANT-26's read-state work. The send path passes
// state "sent" with no ReadBy — nobody has read a message that was just
// written, which is what the vector server_message_text says.
type Viewer struct {
	UserID uuid.UUID
	State  wire.DeliveryState
	ReadBy *int64

	// MediaURL derives a served URL from an opaque storage key. Injected
	// because whether that is a presigned R2 GET or a Traefik route is
	// CANT-47's decision; the same row under two derivers yields two URLs.
	MediaURL func(storageKey string) string
}

// Message builds the wire Message this viewer is entitled to see.
//
// src is the reply source as it is NOW, or nil. A stored ReplyTo with a nil src
// yields no reply_to on the wire, which is the same thing CANT-67's sweep
// produces when it deletes a source: messages.reply_to is ON DELETE SET NULL
// and Message.reply_to is optional, so "the source is gone" is a state the
// schema and the wire already agree on.
func Message(m store.MessageRow, atts []store.AttachmentRow, src *store.ReplySource, v Viewer) wire.Message {
	out := wire.Message{
		ID:             wire.Uuid(m.ID.String()),
		Seq:            wire.Seq(m.Seq),
		LogSeq:         wire.LogSeq(m.LogSeq),
		ConversationID: wire.Uuid(m.ConversationID.String()),
		AuthorID:       wire.Uuid(m.AuthorID.String()),
		At:             wire.Timestamp(timestamp(m)),
		Text:           m.Text,
		State:          v.State,
		ReadBy:         v.ReadBy,
	}

	// CLIENT_ID IS ECHOED TO ITS AUTHOR ONLY. The wire is explicit that other
	// members never receive it — it exists so a sender can match a broadcast
	// against its own outbox entry when the ack and the message frame race.
	// A per-viewer rule in one function is a per-viewer rule that cannot be got
	// right in one transport and wrong in the other.
	if m.ClientID != nil && m.AuthorID == v.UserID {
		id := wire.Uuid(m.ClientID.String())
		out.ClientID = &id
	}

	if m.EditedAt != nil {
		ts := wire.Timestamp(m.EditedAt.UTC().Format(wireTimeLayout))
		out.EditedAt = &ts
	}
	// A tombstone keeps its seq — deleting must not renumber a conversation, or
	// every other client's unread arithmetic breaks. Omitted rather than false
	// when it is not one, because the wire makes it optional and the vectors
	// carry it only on message_tombstone.
	if m.Deleted {
		d := true
		out.Deleted = &d
	}

	for i := range atts {
		if a := attachment(atts[i], v); a != nil {
			out.Attachments = append(out.Attachments, a)
		}
	}
	if src != nil {
		out.ReplyTo = replyRef(*src, v)
	}
	return out
}

// wireTimeLayout is the wire's timestamp: RFC 3339 with milliseconds and a
// literal Z. The schema promises these sort chronologically as strings, which
// is only true at a fixed precision in a fixed zone.
const wireTimeLayout = "2006-01-02T15:04:05.000Z"

func timestamp(m store.MessageRow) string { return m.At.UTC().Format(wireTimeLayout) }

// attachment maps one stored row. Returns nil for a kind this server does not
// serve, which the column's CHECK already makes unstorable — the branch exists
// so an unknown kind cannot reach a client as a half-built object.
func attachment(a store.AttachmentRow, v Viewer) wire.Attachment {
	switch a.Kind {
	case "voice":
		out := wire.VoiceAttachment{
			URL:        v.MediaURL(a.StorageKey),
			DurationMs: wire.DurationMs(deref(a.DurationMs)),
			// PASSTHROUGH, and there is no arithmetic on peaks anywhere in
			// this package. They are computed server-side once because the
			// seeded generator overflows 2^53 and produces different bars in
			// JavaScript than in Dart; recomputing here would reintroduce
			// exactly that.
			Peaks:      a.Peaks,
			Transcript: transcript(a),
		}
		return out
	case "image":
		out := wire.ImageAttachment{
			URL:         v.MediaURL(a.StorageKey),
			Filename:    derefString(a.Filename),
			Width:       deref(a.Width),
			Height:      deref(a.Height),
			Bytes:       deref(a.Bytes),
			Placeholder: a.Placeholder,
		}
		return out
	}
	return nil
}

// transcript takes `state` from its own column and everything else from the
// stored JSON, which is what wire-fields.json says: transcript_state is a
// CHECKed column so the state is queryable, and the rest is the authoritative
// Transcript object as one document.
func transcript(a store.AttachmentRow) wire.Transcript {
	var t wire.Transcript
	if len(a.TranscriptJSON) > 0 {
		// A malformed document yields the zero value rather than a panic; the
		// state column below still describes it truthfully.
		_ = json.Unmarshal(a.TranscriptJSON, &t)
	}
	if a.TranscriptState != nil {
		t.State = wire.TranscriptState(*a.TranscriptState)
	}
	return t
}

// replyRef builds the ref from the LIVE source, never from a copy frozen at
// send time. That is what lets a reply to a voice note back-fill its preview
// when the transcript lands.
func replyRef(src store.ReplySource, v Viewer) *wire.ReplyRef {
	ref := wire.ReplyRef{
		MessageID: wire.Uuid(src.MessageID.String()),
		AuthorID:  wire.Uuid(src.AuthorID.String()),
		Kind:      wire.ReplyRefKindText,
		Preview:   preview(sourceText(src)),
	}

	// KIND comes from the source's first attachment, else text.
	//
	// `link` is the fourth value of the enum and the SERVER NEVER EMITS IT.
	// Nothing derives it: there is no link-preview feature, no column behind it
	// and no ticket that owns one. The enum keeps it for the clients that
	// render one, and this comment exists so a reader looking for the missing
	// branch finds the answer rather than assuming a bug.
	if a := src.FirstAttachment; a != nil {
		switch a.Kind {
		case "voice":
			ref.Kind = wire.ReplyRefKindVoice
			if a.DurationMs != nil {
				d := wire.DurationMs(*a.DurationMs)
				ref.DurationMs = &d
			}
		case "image":
			ref.Kind = wire.ReplyRefKindImage
		}
		// URL ON AN IMAGE REF ONLY, and this is the one rule the vectors do
		// not fix — flagged rather than smoothed over.
		//
		// server_message_image_with_reply is the only vector carrying a
		// ReplyRef, its source is a VOICE note, and it has no `url`. So a voice
		// ref demonstrably does not get one: it carries duration_ms, and the
		// client renders a duration and an affordance that opens the source
		// message. An image ref has no other way to show anything, so it gets
		// the thumbnail. Nothing in the schema says so — `url` is optional with
		// no description — and no vector exercises an image ref, so this is the
		// reading that satisfies the evidence rather than a rule the wire
		// states. If it is wrong, it is one branch.
		if a.Kind == "image" && v.MediaURL != nil {
			u := v.MediaURL(a.StorageKey)
			ref.URL = &u
		}
	}
	return &ref
}

// sourceText is what a preview is cut from: the message's own text, or the
// transcript of its first voice attachment when it has no text. A voice note
// whose transcript has not landed previews as empty rather than as a lie.
func sourceText(src store.ReplySource) string {
	if src.Text != nil && *src.Text != "" {
		return *src.Text
	}
	if a := src.FirstAttachment; a != nil && a.Kind == "voice" {
		var t wire.Transcript
		if len(a.TranscriptJSON) > 0 {
			_ = json.Unmarshal(a.TranscriptJSON, &t)
		}
		if t.Text != nil {
			return *t.Text
		}
	}
	return ""
}

// previewRunes is the whole preview INCLUDING the ellipsis.
//
// The rule is derived from server_message_image_with_reply, which is the only
// place it was written down: a 63-rune transcript previews as 48 runes ending
// in an ellipsis, cut after "back" and not after "before". 47 runes of text
// plus the ellipsis is the longest word boundary that fits, and 47 + " before"
// would be 54. Written here rather than left implicit in a vector, because a
// single sample does not define a rule.
const previewRunes = 48

// preview cuts s to at most previewRunes, on a word boundary, appending an
// ellipsis when anything was removed. Runes, not bytes: the bound is what a
// reader sees on one line, and a byte bound would cut a multi-byte character in
// half and truncate differently per language.
func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= previewRunes {
		return s
	}

	// One rune of the budget belongs to the ellipsis.
	//
	// The scan runs to n > budget rather than n == budget, because a space AT
	// rune index budget is a legal cut: slicing there yields exactly `budget`
	// runes. Stopping one earlier drops the last word that fits — against the
	// vector it cut after "should be" instead of after "back", which is the
	// whole difference between a rule and nearly a rule.
	budget := previewRunes - 1
	cut, n := 0, 0
	for i, r := range s {
		if n > budget {
			break
		}
		if r == ' ' {
			cut = i
		}
		n++
	}
	if cut == 0 {
		// A single word longer than the budget: cut mid-word rather than
		// return the whole thing, since the bound is what protects the layout.
		cut = runeIndex(s, budget)
	}
	return s[:cut] + "…"
}

func runeIndex(s string, n int) int {
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
