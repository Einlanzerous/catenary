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
	"sort"
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
// state "sent" with no ReadBy, which is what the vector server_message_text
// says — nobody OTHER THAN THE AUTHOR has read a message that was just
// written. The author themselves counts from the moment the row exists, so the
// count behind a served message is 1 rather than 0, and CANT-90's own-message
// ladder turns on exactly that: `read` is readBy > 1, not >= 1.
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
	// A NIL DERIVER IS A WIRING MISTAKE, and it is caught here rather than
	// three frames down on the first row that happens to carry media.
	//
	// It used to be both things at once: attachment() called it unguarded and
	// panicked, replyRef() guarded it and silently dropped the ref's url. One
	// missing dependency, two behaviours, in a package whose whole point is
	// that "what does this reader see" is answerable by reading one function.
	//
	// Required rather than optional because a caller cannot know in advance
	// whether a row carries an attachment, so it must always supply one. Same
	// reasoning as store.New's logger, and the same shape of failure if it were
	// optional: a Viewer built without one serves text fine and breaks the
	// first time somebody sends a photo.
	if v.MediaURL == nil {
		panic("wireview: Viewer.MediaURL is required — a viewer cannot serve a message carrying media " +
			"without a deriver over storage_key; pass one even if this row has no attachments")
	}

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

	// ORDERED BY position HERE, not by whichever query produced the rows.
	//
	// wire-fields.json says Message.attachments is "ordered by position", and
	// that guarantee was being delegated to three queries that do not exist yet
	// — CANT-20, CANT-75 and CANT-85 — each of which would have to remember an
	// ORDER BY, and a missing one produces a reordered array no test in this
	// package could see. The field was already in hand and unread. Sorting a
	// COPY, so a caller's slice is not reordered under it.
	ordered := append([]store.AttachmentRow(nil), atts...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Position < ordered[j].Position })
	for i := range ordered {
		if a := attachment(ordered[i], v); a != nil {
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

// storedTranscript is the transcript_json document, which is deliberately NOT a
// whole wire.Transcript: `state` lives in its own CHECKed column so it is
// queryable, and wire-fields.json says exactly that — Transcript.state ←
// transcript_state, everything else ← transcript_json.
//
// It needs its own type BECAUSE the wire type validates. CANT-25 gave
// wire.Transcript an UnmarshalJSON that requires `state`, and decoding the
// stored document straight into it therefore fails — which it silently did,
// dropping text, segments, engine, language and word_count on the floor, until
// the field-map oracle caught it. Decoding a partial document into a type that
// demands a whole one is the bug; a type that matches what is stored is the
// fix.
type storedTranscript struct {
	Text      *string         `json:"text,omitempty"`
	WordCount *int64          `json:"word_count,omitempty"`
	Segments  []storedSegment `json:"segments,omitempty"`
	Engine    *string         `json:"engine,omitempty"`
	Language  *string         `json:"language,omitempty"`
	ETASec    *int64          `json:"eta_sec,omitempty"`
}

// storedSegment is the same decoupling ONE LEVEL DOWN, and it is not
// decoration. wire.TranscriptSegment validates too — at_ms is a DurationMs
// bounded >= 0 and text is required — so pointing storedTranscript at it left
// the stored document coupled to the wire contract after all.
//
// The cost was worse than a rejected segment. encoding/json aborts the whole
// object at the offending key, and the swallowed error means what survives is
// whatever it had already reached. transcript_json is JSONB, which stores keys
// in canonical order — length, then bytewise — so `segments` sorts before
// `word_count`: one bad segment silently cost the word count AND truncated the
// segment list, which are what search's JUMP TO and playback highlighting read,
// while `state` still said `ready` from its own column.
//
// A stored-document type has to mirror what is stored all the way down, or the
// decoupling is only at the root.
type storedSegment struct {
	AtMs int64  `json:"at_ms"`
	Text string `json:"text"`
}

// segments lifts the stored segments onto the wire — a copy rather than a cast,
// because the two types are deliberately separate.
func segments(stored []storedSegment) []wire.TranscriptSegment {
	if len(stored) == 0 {
		return nil
	}
	out := make([]wire.TranscriptSegment, len(stored))
	for i, s := range stored {
		out[i] = wire.TranscriptSegment{AtMs: wire.DurationMs(s.AtMs), Text: s.Text}
	}
	return out
}

func decodeStoredTranscript(raw []byte) storedTranscript {
	var st storedTranscript
	if len(raw) > 0 {
		// A malformed document yields whatever parsed rather than a panic; the
		// state column still describes it truthfully. "Whatever parsed" is the
		// honest description — it used to say "the zero value", which held only
		// while storedTranscript carried no validating types. This is the one place
		// the error is dropped, and it is dropped because the column is the
		// authority on state and a half-read document is still servable.
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

// transcript takes `state` from its own column and everything else from the
// stored JSON.
func transcript(a store.AttachmentRow) wire.Transcript {
	st := decodeStoredTranscript(a.TranscriptJSON)
	t := wire.Transcript{
		Text:      st.Text,
		WordCount: st.WordCount,
		Segments:  segments(st.Segments),
		Engine:    st.Engine,
		Language:  st.Language,
		ETASec:    st.ETASec,
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
		if a.Kind == "image" {
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
		// The stored document, not a wire.Transcript — see storedTranscript.
		if st := decodeStoredTranscript(a.TranscriptJSON); st.Text != nil {
			return *st.Text
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
