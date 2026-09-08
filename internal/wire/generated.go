// Code generated from schema/catenary.wire.v1.schema.json. DO NOT EDIT.
//
// Regenerate with `npm run gen` from web/. Wire version 1.
// Editing this file by hand makes it a hand-written file with extra steps,
// and `npm run gen:check` will fail in CI the moment you do.
// See IDEA-27 (R4).

package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// WireVersion is the schema version this package was generated from.
const WireVersion = 1

// DecodeError is a frame that does not match the schema. It carries the JSON
// path, so a failure names the field rather than the frame.
type DecodeError struct {
	Path string
	Msg  string
}

func (e *DecodeError) Error() string { return "wire: " + e.Path + ": " + e.Msg }

func badf(path, format string, args ...any) error {
	return &DecodeError{Path: path, Msg: fmt.Sprintf(format, args...)}
}

// decodeErr renders an encoding/json failure as a DecodeError with a path.
//
// A raw UnmarshalTypeError names the SHADOW struct, which is an anonymous type
// with every field and tag spelled out — fourteen of them on Message. The field
// and the expected type are the useful part, and they are what TS and Dart
// report. A DecodeError coming back from a nested decode already carries its
// own path and is passed through untouched.
func decodeErr(p string, err error) error {
	var de *DecodeError
	if errors.As(err, &de) {
		return de
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		if te.Field != "" {
			return badf(p+"."+te.Field, "expected %s, got %s", te.Type, te.Value)
		}
		return badf(p, "expected %s, got %s", te.Type, te.Value)
	}
	return badf(p, "%s", err)
}

// oneOf reports whether s is in allowed. Used by inline enums, which have no
// named type to hang a Valid method on.
func oneOf(s string, allowed []string) bool {
	for _, a := range allowed {
		if s == a {
			return true
		}
	}
	return false
}

// A per-conversation, server-assigned, strictly monotonic message ordinal, starting at
// 1. Dense within a conversation for every member: if you can see seq 4 and seq 6 you
// may assume seq 5 exists and you are missing it. This is the number the UI reasons
// about — ordering, `first_unread_seq`, the "N NEW" rule.
//
// UPPER BOUND IS DELIBERATE. The natural type is int64 and Postgres will hand out
// int64, but JSON numbers land in a JavaScript double, which stops being an exact
// integer past 2^53-1 (9007199254740991). A seq above that silently rounds in the web
// client and does not in Dart or Go — the same class of bug as the non-portable
// waveform seed already recorded on IDEA-23, and far more dangerous because it
// corrupts ordering rather than pixels. 2^53 messages in one conversation is not
// reachable, so the bound costs nothing and removes the failure mode. Anything that
// could genuinely exceed it must be a string, not a number.
type Seq = int64

// checkSeq enforces the schema's constraints on a Seq.
func checkSeq(v int64, p string) error {
	if v < 1 {
		return badf(p, "Seq must be >= 1, got %v", v)
	}
	if v > 9007199254740991 {
		return badf(p, "Seq must be <= 9007199254740991, got %v", v)
	}
	return nil
}

// A SERVER-GLOBAL, server-assigned, strictly monotonic cursor over the whole message
// log. Each account reads it as a cursor into the subset it is allowed to see. This is
// the ONLY thing `GET /sync?after=N` takes.
//
// WHY THIS EXISTS, since IDEA-23 named only one sequence: `seq` is scoped per
// conversation, so a single scalar `after=N` is not well-defined across conversations,
// and a client returning from six hours offline needs to catch up on all of them —
// including conversations it has never seen, which by definition are absent from any
// per-conversation cursor map it holds. A cursor vector solves the first problem and
// not the second, and it grows without bound. One global ordinal solves both and stays
// a single integer per device.
//
// ONE COUNTER, NOT ONE PER ACCOUNT. An earlier draft of this text said
// “account-global”, which reads as a per-account counter and contradicts the sparsity
// rule below — a per-account counter would be dense. Ratified as path A on IDEA-27,
// 2026-08-17.
//
// Gaps are normal and carry no information: an account observes only the rows of
// conversations it belongs to, so its observed log_seq values are sparse. Never treat
// a gap as a missing message. `seq` is dense, `log_seq` is not; that is the whole
// division of labour between them.
//
// NORMATIVE — WITHIN ONE CONVERSATION, ASCENDING `log_seq` IMPLIES ASCENDING `seq`. A
// client discovers messages by `log_seq` and orders them by `seq`; if the two ever
// disagree it applies a conversation out of order. This forbids the obvious
// implementation. A `bigserial` assigns at INSERT, not at COMMIT, so a transaction
// holding 100 can commit after one holding 101, and a reader whose cursor has already
// passed 101 never sees 100 again — silent, permanent loss of a delivered message.
// Assign both ordinals from a single-row counter with `UPDATE … RETURNING` inside the
// inserting transaction, so the row lock makes assignment order and commit order the
// same order.
//
// 0 means “I have nothing”, so a fresh device syncs with `after=0`.
type LogSeq = int64

// checkLogSeq enforces the schema's constraints on a LogSeq.
func checkLogSeq(v int64, p string) error {
	if v < 0 {
		return badf(p, "LogSeq must be >= 0, got %v", v)
	}
	if v > 9007199254740991 {
		return badf(p, "LogSeq must be <= 9007199254740991, got %v", v)
	}
	return nil
}

// Lowercase hyphenated UUID. Lowercase is normative: these are compared as strings in
// caches and idempotency keys, and a client that sends uppercase would defeat
// deduplication.
type Uuid = string

var UuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// checkUuid enforces the schema's constraints on a Uuid.
func checkUuid(v string, p string) error {
	if !UuidPattern.MatchString(v) {
		return badf(p, "Uuid must match %s, got %q", `^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, v)
	}
	return nil
}

// RFC 3339, UTC, exactly three fractional digits, literal Z. The pattern is enforced
// rather than advisory because `2026-08-17T04:22:01Z` and
// `2026-08-17T04:22:01.193000Z` both parse fine in all three languages and then sort
// differently as strings.
type Timestamp = string

var TimestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`)

// checkTimestamp enforces the schema's constraints on a Timestamp.
func checkTimestamp(v string, p string) error {
	if !TimestampPattern.MatchString(v) {
		return badf(p, "Timestamp must match %s, got %q", `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`, v)
	}
	return nil
}

// D4: everything is a conversation. `direct` is two members, `group` is any number;
// neither is a distinct entity. Adding a third member to a direct conversation
// promotes it to `group` and is a row insert plus this field changing, never a
// migration.
type ConversationKind string

const (
	ConversationKindDirect ConversationKind = "direct"
	ConversationKindGroup  ConversationKind = "group"
)

// Valid reports whether v is a value this schema version defines.
func (v ConversationKind) Valid() bool {
	switch v {
	case ConversationKindDirect, ConversationKindGroup:
		return true
	}
	return false
}

// checkConversationKind rejects a value this schema version does not define.
func checkConversationKind(v ConversationKind, p string) error {
	if !v.Valid() {
		return badf(p, "expected one of %s, got %q", "direct|group", string(v))
	}
	return nil
}

// The server-authoritative half of the message lifecycle from IDEA-23, and the ONLY
// states that ever cross the wire.
//
// The full ladder the UI renders is `sending → sent → delivered → read`, plus `queued`
// and `failed`. `sending`, `queued` and `failed` are client-local: they describe a
// message's relationship to its own outbox, the server has no opinion on them, and a
// client that put them in a frame would be reporting its own state as fact. Keeping
// them out of this enum is what stops that being expressible.
//
// Generated clients therefore get this enum for decoding and are expected to widen it
// locally — see the `ClientDeliveryState` note in the generated output.
type DeliveryState string

const (
	DeliveryStateSent      DeliveryState = "sent"
	DeliveryStateDelivered DeliveryState = "delivered"
	DeliveryStateRead      DeliveryState = "read"
)

// Valid reports whether v is a value this schema version defines.
func (v DeliveryState) Valid() bool {
	switch v {
	case DeliveryStateSent, DeliveryStateDelivered, DeliveryStateRead:
		return true
	}
	return false
}

// checkDeliveryState rejects a value this schema version does not define.
func checkDeliveryState(v DeliveryState, p string) error {
	if !v.Valid() {
		return badf(p, "expected one of %s, got %q", "sent|delivered|read", string(v))
	}
	return nil
}

// Lifecycle of the async Whisper job (R3/IDEA-26) that writes the finished transcript
// back onto the ATTACHMENT it belongs to.
//
// AN EARLIER DRAFT OF THIS TEXT SAID the job writes `transcript_text` back onto the
// MESSAGE row. That was wrong and it named the wrong write target. `Transcript` hangs
// off `VoiceAttachment`, and a message may carry more than one voice note —
// `Message.attachments` has no `maxItems` — so a per-message column cannot hold two
// transcripts, and the second job to finish would silently replace the first while
// `word_count` and `segments`, which are per attachment, went on describing different
// audio. Corrected on CANT-13, 2026-09-02, when the initial schema settled where each
// field lives: the authoritative copy is per attachment, and the server keeps a
// denormalised copy of the text on the message row for CANT-59's single full-text
// index, which nothing reads to serve a message.
type TranscriptState string

const (
	TranscriptStatePending TranscriptState = "pending"
	TranscriptStateReady   TranscriptState = "ready"
	TranscriptStateFailed  TranscriptState = "failed"
)

// Valid reports whether v is a value this schema version defines.
func (v TranscriptState) Valid() bool {
	switch v {
	case TranscriptStatePending, TranscriptStateReady, TranscriptStateFailed:
		return true
	}
	return false
}

// checkTranscriptState rejects a value this schema version does not define.
func checkTranscriptState(v TranscriptState, p string) error {
	if !v.Valid() {
		return badf(p, "expected one of %s, got %q", "pending|ready|failed", string(v))
	}
	return nil
}

// A duration or offset in whole milliseconds.
//
// NO FLOATING-POINT NUMBER APPEARS ANYWHERE IN THIS PROTOCOL, and this type is why.
// JSON has a single number type; Dart has two. A `number`-typed field holding `0`
// decodes to Dart `double 0.0` and re-encodes as `0.0`, which is not what TypeScript
// or Go emit for the same field — so a value that survived a round trip in two
// languages would not survive it in the third, and the conformance vectors would fail
// on a difference with no bug behind it. Worse, the workaround (compare numbers
// loosely) would have hidden real precision differences too.
//
// Milliseconds as integers are exact everywhere, sort correctly, and are what a media
// pipeline already works in. The cost is that clients format for display, which they
// were doing anyway — nothing renders a raw duration.
type DurationMs = int64

// checkDurationMs enforces the schema's constraints on a DurationMs.
func checkDurationMs(v int64, p string) error {
	if v < 0 {
		return badf(p, "DurationMs must be >= 0, got %v", v)
	}
	if v > 9007199254740991 {
		return badf(p, "DurationMs must be <= 9007199254740991, got %v", v)
	}
	return nil
}

// The four source types the canvas's replies section renders.
type ReplyRefKind string

const (
	ReplyRefKindText  ReplyRefKind = "text"
	ReplyRefKindVoice ReplyRefKind = "voice"
	ReplyRefKindImage ReplyRefKind = "image"
	ReplyRefKindLink  ReplyRefKind = "link"
)

// Valid reports whether v is a value this schema version defines.
func (v ReplyRefKind) Valid() bool {
	switch v {
	case ReplyRefKindText, ReplyRefKindVoice, ReplyRefKindImage, ReplyRefKindLink:
		return true
	}
	return false
}

// checkReplyRefKind rejects a value this schema version does not define.
func checkReplyRefKind(v ReplyRefKind, p string) error {
	if !v.Valid() {
		return badf(p, "expected one of %s, got %q", "text|voice|image|link", string(v))
	}
	return nil
}

type TypingState string

const (
	TypingStateStart TypingState = "start"
	TypingStateStop  TypingState = "stop"
)

// Valid reports whether v is a value this schema version defines.
func (v TypingState) Valid() bool {
	switch v {
	case TypingStateStart, TypingStateStop:
		return true
	}
	return false
}

// checkTypingState rejects a value this schema version does not define.
func checkTypingState(v TypingState, p string) error {
	if !v.Valid() {
		return badf(p, "expected one of %s, got %q", "start|stop", string(v))
	}
	return nil
}

type ErrorCode string

const (
	ErrorCodeUnauthorized           ErrorCode = "unauthorized"
	ErrorCodeWireVersionUnsupported ErrorCode = "wire_version_unsupported"
	ErrorCodeRateLimited            ErrorCode = "rate_limited"
	ErrorCodeNotAMember             ErrorCode = "not_a_member"
	ErrorCodeConversationNotFound   ErrorCode = "conversation_not_found"
	ErrorCodeMessageTooLarge        ErrorCode = "message_too_large"
	ErrorCodeUploadNotFound         ErrorCode = "upload_not_found"
	ErrorCodeInternal               ErrorCode = "internal"
)

// Valid reports whether v is a value this schema version defines.
func (v ErrorCode) Valid() bool {
	switch v {
	case ErrorCodeUnauthorized, ErrorCodeWireVersionUnsupported, ErrorCodeRateLimited, ErrorCodeNotAMember, ErrorCodeConversationNotFound, ErrorCodeMessageTooLarge, ErrorCodeUploadNotFound, ErrorCodeInternal:
		return true
	}
	return false
}

// checkErrorCode rejects a value this schema version does not define.
func checkErrorCode(v ErrorCode, p string) error {
	if !v.Valid() {
		return badf(p, "expected one of %s, got %q", "unauthorized|wire_version_unsupported|rate_limited|not_a_member|conversation_not_found|message_too_large|upload_not_found|internal", string(v))
	}
	return nil
}

// Tagged union on `kind`. Decoders MUST ignore an attachment whose `kind` they do not
// know rather than failing the whole message — a client that hard-errors on an unknown
// attachment type cannot be shipped ahead of a server that adds one.
type Attachment interface {
	isAttachment()
	// WireTag returns the discriminator value this member carries.
	WireTag() string
}

// AttachmentList is a JSON array of Attachment. It exists because encoding/json
// cannot unmarshal into a slice of interfaces: there is no concrete type to
// allocate. Unmarshalling SKIPS an element whose tag is unrecognised rather
// than failing, so a client or server built against an older schema loses the
// element and not the whole message.
type AttachmentList []Attachment

func (l *AttachmentList) UnmarshalJSON(b []byte) error {
	return l.decode(b, "AttachmentList")
}

// decode is UnmarshalJSON with the caller's JSON path.
func (l *AttachmentList) decode(b []byte, p string) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return decodeErr(p, err)
	}
	out := make(AttachmentList, 0, len(raw))
	for i, r := range raw {
		v, err := decodeAttachment(r, fmt.Sprintf("%s[%d]", p, i))
		if err != nil {
			return err // already carries its element path
		}
		if v == nil {
			continue
		}
		out = append(out, v)
	}
	*l = out
	return nil
}

// Anything a client may send over the socket. One JSON object per WebSocket text
// frame; never batched, never binary.
type ClientFrame interface {
	isClientFrame()
	// WireTag returns the discriminator value this member carries.
	WireTag() string
}

// ClientFrameList is a JSON array of ClientFrame. It exists because encoding/json
// cannot unmarshal into a slice of interfaces: there is no concrete type to
// allocate. Unmarshalling SKIPS an element whose tag is unrecognised rather
// than failing, so a client or server built against an older schema loses the
// element and not the whole message.
type ClientFrameList []ClientFrame

func (l *ClientFrameList) UnmarshalJSON(b []byte) error {
	return l.decode(b, "ClientFrameList")
}

// decode is UnmarshalJSON with the caller's JSON path.
func (l *ClientFrameList) decode(b []byte, p string) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return decodeErr(p, err)
	}
	out := make(ClientFrameList, 0, len(raw))
	for i, r := range raw {
		v, err := decodeClientFrame(r, fmt.Sprintf("%s[%d]", p, i))
		if err != nil {
			return err // already carries its element path
		}
		if v == nil {
			continue
		}
		out = append(out, v)
	}
	*l = out
	return nil
}

// Anything the server may send over the socket.
type ServerFrame interface {
	isServerFrame()
	// WireTag returns the discriminator value this member carries.
	WireTag() string
}

// ServerFrameList is a JSON array of ServerFrame. It exists because encoding/json
// cannot unmarshal into a slice of interfaces: there is no concrete type to
// allocate. Unmarshalling SKIPS an element whose tag is unrecognised rather
// than failing, so a client or server built against an older schema loses the
// element and not the whole message.
type ServerFrameList []ServerFrame

func (l *ServerFrameList) UnmarshalJSON(b []byte) error {
	return l.decode(b, "ServerFrameList")
}

// decode is UnmarshalJSON with the caller's JSON path.
func (l *ServerFrameList) decode(b []byte, p string) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return decodeErr(p, err)
	}
	out := make(ServerFrameList, 0, len(raw))
	for i, r := range raw {
		v, err := decodeServerFrame(r, fmt.Sprintf("%s[%d]", p, i))
		if err != nil {
			return err // already carries its element path
		}
		if v == nil {
			continue
		}
		out = append(out, v)
	}
	*l = out
	return nil
}

type User struct {
	ID Uuid `json:"id"`
	// Display name. The typing-indicator naming rule uses the first whitespace-separated
	// token of this as the first name.
	Name string `json:"name"`
	// Two letters for the avatar tile. Server-derived so web and Flutter cannot disagree
	// about how to abbreviate a name — deriving this client-side is a two-implementation
	// problem for zero benefit.
	Initials *string `json:"initials,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a User: required fields must be
// present, and every constrained value is checked against the schema.
func (v *User) UnmarshalJSON(b []byte) error {
	return v.decode(b, "User")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *User) decode(b []byte, p string) error {
	var s struct {
		ID       *Uuid   `json:"id"`
		Name     *string `json:"name"`
		Initials *string `json:"initials"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out User
	if s.ID == nil {
		return badf(p+".id", "required field is missing")
	}
	out.ID = *s.ID
	if s.Name == nil {
		return badf(p+".name", "required field is missing")
	}
	out.Name = *s.Name
	if s.Initials != nil {
		out.Initials = s.Initials
	}
	if err := checkUuid(out.ID, p+".id"); err != nil {
		return err
	}
	*v = out
	return nil
}

// One timed span of transcript, used by search's JUMP TO and by playback highlighting.
type TranscriptSegment struct {
	// Offset from the start of the clip.
	AtMs DurationMs `json:"at_ms"`
	Text string     `json:"text"`
}

// UnmarshalJSON decodes and VALIDATES a TranscriptSegment: required fields must be
// present, and every constrained value is checked against the schema.
func (v *TranscriptSegment) UnmarshalJSON(b []byte) error {
	return v.decode(b, "TranscriptSegment")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *TranscriptSegment) decode(b []byte, p string) error {
	var s struct {
		AtMs *DurationMs `json:"at_ms"`
		Text *string     `json:"text"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out TranscriptSegment
	if s.AtMs == nil {
		return badf(p+".at_ms", "required field is missing")
	}
	out.AtMs = *s.AtMs
	if s.Text == nil {
		return badf(p+".text", "required field is missing")
	}
	out.Text = *s.Text
	if err := checkDurationMs(out.AtMs, p+".at_ms"); err != nil {
		return err
	}
	*v = out
	return nil
}

type Transcript struct {
	State TranscriptState `json:"state"`
	// Present when state is `ready`.
	Text *string `json:"text,omitempty"`
	// Server-computed. The web client currently derives this from the text on screen so
	// the count cannot lie; that stays true — a client SHOULD prefer its own count of the
	// text it is actually rendering and treat this as a hint for collapsed state.
	WordCount *int64              `json:"word_count,omitempty"`
	Segments  []TranscriptSegment `json:"segments,omitempty"`
	// e.g. `whisper.cpp/small.en`. Recorded because a transcript's quality is not
	// interpretable without knowing what produced it, and R3 expects the model to be a
	// tunable knob.
	Engine *string `json:"engine,omitempty"`
	// BCP 47.
	Language *string `json:"language,omitempty"`
	// Server's own estimate of remaining work, shown while `pending`. A client must not
	// invent one.
	ETASec *int64 `json:"eta_sec,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a Transcript: required fields must be
// present, and every constrained value is checked against the schema.
func (v *Transcript) UnmarshalJSON(b []byte) error {
	return v.decode(b, "Transcript")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *Transcript) decode(b []byte, p string) error {
	var s struct {
		State     *TranscriptState   `json:"state"`
		Text      *string            `json:"text"`
		WordCount *int64             `json:"word_count"`
		Segments  *[]json.RawMessage `json:"segments"`
		Engine    *string            `json:"engine"`
		Language  *string            `json:"language"`
		ETASec    *int64             `json:"eta_sec"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out Transcript
	if s.State == nil {
		return badf(p+".state", "required field is missing")
	}
	out.State = *s.State
	if s.Text != nil {
		out.Text = s.Text
	}
	if s.WordCount != nil {
		out.WordCount = s.WordCount
	}
	if s.Segments != nil {
		out.Segments = make([]TranscriptSegment, len(*s.Segments))
		for i, raw := range *s.Segments {
			if err := out.Segments[i].decode(raw, fmt.Sprintf("%s[%d]", p+".segments", i)); err != nil {
				return err
			}
		}
	}
	if s.Engine != nil {
		out.Engine = s.Engine
	}
	if s.Language != nil {
		out.Language = s.Language
	}
	if s.ETASec != nil {
		out.ETASec = s.ETASec
	}
	if err := checkTranscriptState(out.State, p+".state"); err != nil {
		return err
	}
	*v = out
	return nil
}

type VoiceAttachment struct {
	// Media URL. Opaque to the client; may be presigned and time-limited.
	URL        string     `json:"url"`
	DurationMs DurationMs `json:"duration_ms"`
	// Amplitude peaks 0–100, computed server-side ONCE and stored with the message.
	// Deliberate call 13 of the design canvas: the bar pattern must be identical in web
	// and Flutter, which is only true if both render the same stored array.
	//
	// This is not a style preference. The canvas's own seeded generator multiplies by
	// 1103515245, which for seeds near 2^31 exceeds 2^53 — so JavaScript rounds where
	// Dart's 64-bit int does not, and the identical seed yields different bars. Any
	// client-side generation reintroduces that. Clients render this array and never
	// synthesise one.
	Peaks      []int64    `json:"peaks"`
	Transcript Transcript `json:"transcript"`
}

// UnmarshalJSON decodes and VALIDATES a VoiceAttachment: required fields must be
// present, and every constrained value is checked against the schema.
func (v *VoiceAttachment) UnmarshalJSON(b []byte) error {
	return v.decode(b, "VoiceAttachment")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *VoiceAttachment) decode(b []byte, p string) error {
	var s struct {
		URL        *string          `json:"url"`
		DurationMs *DurationMs      `json:"duration_ms"`
		Peaks      *[]int64         `json:"peaks"`
		Transcript *json.RawMessage `json:"transcript"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out VoiceAttachment
	if s.URL == nil {
		return badf(p+".url", "required field is missing")
	}
	out.URL = *s.URL
	if s.DurationMs == nil {
		return badf(p+".duration_ms", "required field is missing")
	}
	out.DurationMs = *s.DurationMs
	if s.Peaks == nil {
		return badf(p+".peaks", "required field is missing")
	}
	out.Peaks = *s.Peaks
	if s.Transcript == nil {
		return badf(p+".transcript", "required field is missing")
	}
	if err := out.Transcript.decode(*s.Transcript, p+".transcript"); err != nil {
		return err
	}
	if err := checkDurationMs(out.DurationMs, p+".duration_ms"); err != nil {
		return err
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "voice".
func (VoiceAttachment) WireTag() string { return "voice" }

// MarshalJSON injects the constant "kind" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v VoiceAttachment) MarshalJSON() ([]byte, error) {
	type alias VoiceAttachment
	return json.Marshal(struct {
		T string `json:"kind"`
		alias
	}{T: "voice", alias: alias(v)})
}

func (VoiceAttachment) isAttachment() {}

type ImageAttachment struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	// Stored intrinsic width. The client caps the rendered box at 340x400 from this ratio,
	// so the row height is final before a byte of image data arrives — which is the
	// property the blurhash placeholder exists to protect.
	Width  int64 `json:"width"`
	Height int64 `json:"height"`
	Bytes  int64 `json:"bytes"`
	// Blurhash. Absent means render the plane flat; never means block on the image.
	Placeholder *string `json:"placeholder,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a ImageAttachment: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ImageAttachment) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ImageAttachment")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ImageAttachment) decode(b []byte, p string) error {
	var s struct {
		URL         *string `json:"url"`
		Filename    *string `json:"filename"`
		Width       *int64  `json:"width"`
		Height      *int64  `json:"height"`
		Bytes       *int64  `json:"bytes"`
		Placeholder *string `json:"placeholder"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ImageAttachment
	if s.URL == nil {
		return badf(p+".url", "required field is missing")
	}
	out.URL = *s.URL
	if s.Filename == nil {
		return badf(p+".filename", "required field is missing")
	}
	out.Filename = *s.Filename
	if s.Width == nil {
		return badf(p+".width", "required field is missing")
	}
	out.Width = *s.Width
	if s.Height == nil {
		return badf(p+".height", "required field is missing")
	}
	out.Height = *s.Height
	if s.Bytes == nil {
		return badf(p+".bytes", "required field is missing")
	}
	out.Bytes = *s.Bytes
	if s.Placeholder != nil {
		out.Placeholder = s.Placeholder
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "image".
func (ImageAttachment) WireTag() string { return "image" }

// MarshalJSON injects the constant "kind" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ImageAttachment) MarshalJSON() ([]byte, error) {
	type alias ImageAttachment
	return json.Marshal(struct {
		T string `json:"kind"`
		alias
	}{T: "image", alias: alias(v)})
}

func (ImageAttachment) isAttachment() {}

type ReplyRef struct {
	MessageID Uuid         `json:"message_id"`
	AuthorID  Uuid         `json:"author_id"`
	Kind      ReplyRefKind `json:"kind"`
	// One line, rendered from the LIVE source message server-side, not frozen at send
	// time. That is what lets a reply to a voice note back-fill its preview when the
	// transcript lands.
	Preview    string      `json:"preview"`
	DurationMs *DurationMs `json:"duration_ms,omitempty"`
	URL        *string     `json:"url,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a ReplyRef: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ReplyRef) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ReplyRef")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ReplyRef) decode(b []byte, p string) error {
	var s struct {
		MessageID  *Uuid         `json:"message_id"`
		AuthorID   *Uuid         `json:"author_id"`
		Kind       *ReplyRefKind `json:"kind"`
		Preview    *string       `json:"preview"`
		DurationMs *DurationMs   `json:"duration_ms"`
		URL        *string       `json:"url"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ReplyRef
	if s.MessageID == nil {
		return badf(p+".message_id", "required field is missing")
	}
	out.MessageID = *s.MessageID
	if s.AuthorID == nil {
		return badf(p+".author_id", "required field is missing")
	}
	out.AuthorID = *s.AuthorID
	if s.Kind == nil {
		return badf(p+".kind", "required field is missing")
	}
	out.Kind = *s.Kind
	if s.Preview == nil {
		return badf(p+".preview", "required field is missing")
	}
	out.Preview = *s.Preview
	if s.DurationMs != nil {
		out.DurationMs = s.DurationMs
	}
	if s.URL != nil {
		out.URL = s.URL
	}
	if err := checkUuid(out.MessageID, p+".message_id"); err != nil {
		return err
	}
	if err := checkUuid(out.AuthorID, p+".author_id"); err != nil {
		return err
	}
	if err := checkReplyRefKind(out.Kind, p+".kind"); err != nil {
		return err
	}
	if out.DurationMs != nil {
		if err := checkDurationMs(*out.DurationMs, p+".duration_ms"); err != nil {
			return err
		}
	}
	*v = out
	return nil
}

type Message struct {
	ID             Uuid           `json:"id"`
	Seq            Seq            `json:"seq"`
	LogSeq         LogSeq         `json:"log_seq"`
	ConversationID Uuid           `json:"conversation_id"`
	AuthorID       Uuid           `json:"author_id"`
	At             Timestamp      `json:"at"`
	Text           *string        `json:"text,omitempty"`
	Attachments    AttachmentList `json:"attachments,omitempty"`
	ReplyTo        *ReplyRef      `json:"reply_to,omitempty"`
	State          DeliveryState  `json:"state"`
	// How many members other than the author have read it. Rooms render the fraction
	// against `Conversation.member_count`: READ 5/7.
	ReadBy *int64 `json:"read_by,omitempty"`
	// Echoed back to the sender only, so a client can match a broadcast message against
	// its own outbox entry when the `ack` and the `message` frame race. Other members
	// never receive it.
	ClientID *Uuid      `json:"client_id,omitempty"`
	EditedAt *Timestamp `json:"edited_at,omitempty"`
	// A tombstone. The row keeps its seq — deleting a message must not renumber the
	// conversation, or every other client's unread arithmetic breaks.
	Deleted *bool `json:"deleted,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a Message: required fields must be
// present, and every constrained value is checked against the schema.
func (v *Message) UnmarshalJSON(b []byte) error {
	return v.decode(b, "Message")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *Message) decode(b []byte, p string) error {
	var s struct {
		ID             *Uuid            `json:"id"`
		Seq            *Seq             `json:"seq"`
		LogSeq         *LogSeq          `json:"log_seq"`
		ConversationID *Uuid            `json:"conversation_id"`
		AuthorID       *Uuid            `json:"author_id"`
		At             *Timestamp       `json:"at"`
		Text           *string          `json:"text"`
		Attachments    *json.RawMessage `json:"attachments"`
		ReplyTo        *json.RawMessage `json:"reply_to"`
		State          *DeliveryState   `json:"state"`
		ReadBy         *int64           `json:"read_by"`
		ClientID       *Uuid            `json:"client_id"`
		EditedAt       *Timestamp       `json:"edited_at"`
		Deleted        *bool            `json:"deleted"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out Message
	if s.ID == nil {
		return badf(p+".id", "required field is missing")
	}
	out.ID = *s.ID
	if s.Seq == nil {
		return badf(p+".seq", "required field is missing")
	}
	out.Seq = *s.Seq
	if s.LogSeq == nil {
		return badf(p+".log_seq", "required field is missing")
	}
	out.LogSeq = *s.LogSeq
	if s.ConversationID == nil {
		return badf(p+".conversation_id", "required field is missing")
	}
	out.ConversationID = *s.ConversationID
	if s.AuthorID == nil {
		return badf(p+".author_id", "required field is missing")
	}
	out.AuthorID = *s.AuthorID
	if s.At == nil {
		return badf(p+".at", "required field is missing")
	}
	out.At = *s.At
	if s.Text != nil {
		out.Text = s.Text
	}
	if s.Attachments != nil {
		if err := out.Attachments.decode(*s.Attachments, p+".attachments"); err != nil {
			return err
		}
	}
	if s.ReplyTo != nil {
		out.ReplyTo = new(ReplyRef)
		if err := out.ReplyTo.decode(*s.ReplyTo, p+".reply_to"); err != nil {
			return err
		}
	}
	if s.State == nil {
		return badf(p+".state", "required field is missing")
	}
	out.State = *s.State
	if s.ReadBy != nil {
		out.ReadBy = s.ReadBy
	}
	if s.ClientID != nil {
		out.ClientID = s.ClientID
	}
	if s.EditedAt != nil {
		out.EditedAt = s.EditedAt
	}
	if s.Deleted != nil {
		out.Deleted = s.Deleted
	}
	if err := checkUuid(out.ID, p+".id"); err != nil {
		return err
	}
	if err := checkSeq(out.Seq, p+".seq"); err != nil {
		return err
	}
	if err := checkLogSeq(out.LogSeq, p+".log_seq"); err != nil {
		return err
	}
	if err := checkUuid(out.ConversationID, p+".conversation_id"); err != nil {
		return err
	}
	if err := checkUuid(out.AuthorID, p+".author_id"); err != nil {
		return err
	}
	if err := checkTimestamp(out.At, p+".at"); err != nil {
		return err
	}
	if err := checkDeliveryState(out.State, p+".state"); err != nil {
		return err
	}
	if out.ClientID != nil {
		if err := checkUuid(*out.ClientID, p+".client_id"); err != nil {
			return err
		}
	}
	if out.EditedAt != nil {
		if err := checkTimestamp(*out.EditedAt, p+".edited_at"); err != nil {
			return err
		}
	}
	*v = out
	return nil
}

type Conversation struct {
	ID          Uuid             `json:"id"`
	Kind        ConversationKind `json:"kind"`
	Name        string           `json:"name"`
	MemberCount int64            `json:"member_count"`
	Muted       *bool            `json:"muted,omitempty"`
	// The first seq the reader has not seen; absent means fully read. Both the rail's
	// badge and the thread's "N NEW" rule derive from this. There is deliberately NO
	// stored unread count on the wire, because a count and a marker can disagree and this
	// one does: your own messages are never unread, so a count computed anywhere but from
	// this marker gets it wrong. Absent-means-read also keeps a resync that delivers out
	// of order from producing a wrong badge.
	FirstUnreadSeq *Seq `json:"first_unread_seq,omitempty"`
	// Newest seq the server holds for this conversation. What a resync counts towards, and
	// what the numeric resync progress is a fraction of.
	HeadSeq Seq `json:"head_seq"`
	// Per-conversation override of the global infinite retention. Absent means keep
	// forever.
	RetentionDays *int64 `json:"retention_days,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a Conversation: required fields must be
// present, and every constrained value is checked against the schema.
func (v *Conversation) UnmarshalJSON(b []byte) error {
	return v.decode(b, "Conversation")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *Conversation) decode(b []byte, p string) error {
	var s struct {
		ID             *Uuid             `json:"id"`
		Kind           *ConversationKind `json:"kind"`
		Name           *string           `json:"name"`
		MemberCount    *int64            `json:"member_count"`
		Muted          *bool             `json:"muted"`
		FirstUnreadSeq *Seq              `json:"first_unread_seq"`
		HeadSeq        *Seq              `json:"head_seq"`
		RetentionDays  *int64            `json:"retention_days"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out Conversation
	if s.ID == nil {
		return badf(p+".id", "required field is missing")
	}
	out.ID = *s.ID
	if s.Kind == nil {
		return badf(p+".kind", "required field is missing")
	}
	out.Kind = *s.Kind
	if s.Name == nil {
		return badf(p+".name", "required field is missing")
	}
	out.Name = *s.Name
	if s.MemberCount == nil {
		return badf(p+".member_count", "required field is missing")
	}
	out.MemberCount = *s.MemberCount
	if s.Muted != nil {
		out.Muted = s.Muted
	}
	if s.FirstUnreadSeq != nil {
		out.FirstUnreadSeq = s.FirstUnreadSeq
	}
	if s.HeadSeq == nil {
		return badf(p+".head_seq", "required field is missing")
	}
	out.HeadSeq = *s.HeadSeq
	if s.RetentionDays != nil {
		out.RetentionDays = s.RetentionDays
	}
	if err := checkUuid(out.ID, p+".id"); err != nil {
		return err
	}
	if err := checkConversationKind(out.Kind, p+".kind"); err != nil {
		return err
	}
	if out.FirstUnreadSeq != nil {
		if err := checkSeq(*out.FirstUnreadSeq, p+".first_unread_seq"); err != nil {
			return err
		}
	}
	if err := checkSeq(out.HeadSeq, p+".head_seq"); err != nil {
		return err
	}
	*v = out
	return nil
}

// First frame the client sends after the socket opens. The server answers with `ready`
// or `error`.
type ClientHello struct {
	// The version of THIS schema the client was generated from. The server refuses a
	// version it cannot speak with an `error` frame rather than failing halfway through a
	// session.
	WireVersion int64 `json:"wire_version"`
	// Stable per install. D2 makes the device the unit of credential and of revocation, so
	// it is the unit here too.
	DeviceID Uuid `json:"device_id"`
	// The client's cursor. Absent means "do not stream backlog, I will call /sync myself"
	// — which is the correct choice for a client with a large gap, since a backlog burst
	// over the socket has no progress indication.
	ResumeFromLogSeq *LogSeq `json:"resume_from_log_seq,omitempty"`
	// Free-form build identifier for logs, e.g. `catenary-web/0.3.1`. Never parsed.
	ClientInfo *string `json:"client_info,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a ClientHello: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ClientHello) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ClientHello")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ClientHello) decode(b []byte, p string) error {
	var s struct {
		WireVersion      *int64  `json:"wire_version"`
		DeviceID         *Uuid   `json:"device_id"`
		ResumeFromLogSeq *LogSeq `json:"resume_from_log_seq"`
		ClientInfo       *string `json:"client_info"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ClientHello
	if s.WireVersion == nil {
		return badf(p+".wire_version", "required field is missing")
	}
	out.WireVersion = *s.WireVersion
	if s.DeviceID == nil {
		return badf(p+".device_id", "required field is missing")
	}
	out.DeviceID = *s.DeviceID
	if s.ResumeFromLogSeq != nil {
		out.ResumeFromLogSeq = s.ResumeFromLogSeq
	}
	if s.ClientInfo != nil {
		out.ClientInfo = s.ClientInfo
	}
	if err := checkUuid(out.DeviceID, p+".device_id"); err != nil {
		return err
	}
	if out.ResumeFromLogSeq != nil {
		if err := checkLogSeq(*out.ResumeFromLogSeq, p+".resume_from_log_seq"); err != nil {
			return err
		}
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "hello".
func (ClientHello) WireTag() string { return "hello" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ClientHello) MarshalJSON() ([]byte, error) {
	type alias ClientHello
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "hello", alias: alias(v)})
}

func (ClientHello) isClientFrame() {}

type ServerReady struct {
	SessionID Uuid `json:"session_id"`
	// Lets a client with a skewed clock render correct relative timestamps by offset
	// rather than trusting its own clock.
	ServerTime Timestamp `json:"server_time"`
	// How often the client must send `ping`. SERVER-DRIVEN ON PURPOSE (R1/IDEA-24): the
	// 30–45 s window sits under Cloudflare's ~100 s idle timeout, and if that constant
	// lived in two clients it would be two constants that drift, with the Flutter one
	// discovered wrong only in the field. Here the number is deployed, not shipped.
	HeartbeatIntervalSec int64 `json:"heartbeat_interval_sec"`
	// How many unanswered pings before the client severs deliberately and reconnects. R1
	// sets this at 2. Waiting for the OS to notice a half-open socket is the failure this
	// exists to prevent.
	MissedPongLimit int64 `json:"missed_pong_limit"`
	// The server's current head. A client that asked to resume can compare this against
	// its own cursor to decide between streaming and a `/sync` call.
	LogSeq LogSeq `json:"log_seq"`
	// True when the server accepted `resume_from_log_seq` and will stream the gap. False
	// means the client must catch up over `/sync` before trusting anything it renders.
	Resumed bool `json:"resumed"`
}

// UnmarshalJSON decodes and VALIDATES a ServerReady: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ServerReady) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ServerReady")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ServerReady) decode(b []byte, p string) error {
	var s struct {
		SessionID            *Uuid      `json:"session_id"`
		ServerTime           *Timestamp `json:"server_time"`
		HeartbeatIntervalSec *int64     `json:"heartbeat_interval_sec"`
		MissedPongLimit      *int64     `json:"missed_pong_limit"`
		LogSeq               *LogSeq    `json:"log_seq"`
		Resumed              *bool      `json:"resumed"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ServerReady
	if s.SessionID == nil {
		return badf(p+".session_id", "required field is missing")
	}
	out.SessionID = *s.SessionID
	if s.ServerTime == nil {
		return badf(p+".server_time", "required field is missing")
	}
	out.ServerTime = *s.ServerTime
	if s.HeartbeatIntervalSec == nil {
		return badf(p+".heartbeat_interval_sec", "required field is missing")
	}
	out.HeartbeatIntervalSec = *s.HeartbeatIntervalSec
	if s.MissedPongLimit == nil {
		return badf(p+".missed_pong_limit", "required field is missing")
	}
	out.MissedPongLimit = *s.MissedPongLimit
	if s.LogSeq == nil {
		return badf(p+".log_seq", "required field is missing")
	}
	out.LogSeq = *s.LogSeq
	if s.Resumed == nil {
		return badf(p+".resumed", "required field is missing")
	}
	out.Resumed = *s.Resumed
	if err := checkUuid(out.SessionID, p+".session_id"); err != nil {
		return err
	}
	if err := checkTimestamp(out.ServerTime, p+".server_time"); err != nil {
		return err
	}
	if err := checkLogSeq(out.LogSeq, p+".log_seq"); err != nil {
		return err
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "ready".
func (ServerReady) WireTag() string { return "ready" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ServerReady) MarshalJSON() ([]byte, error) {
	type alias ServerReady
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "ready", alias: alias(v)})
}

func (ServerReady) isServerFrame() {}

// APPLICATION-level heartbeat, sent in both directions. Deliberately not the WebSocket
// protocol's own ping/pong control frames: those are handled inside the transport
// stack, are not observable from application code in a browser at all, and can be
// answered by an intermediary. R1 requires a heartbeat observable from both ends,
// which means it has to be a data frame.
type Ping struct {
	// Echoed verbatim in the matching `pong`. Matching by id rather than by arrival order
	// is what makes a missed pong countable — with unmatched pings you cannot tell a lost
	// pong from a slow one.
	ID string     `json:"id"`
	At *Timestamp `json:"at,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a Ping: required fields must be
// present, and every constrained value is checked against the schema.
func (v *Ping) UnmarshalJSON(b []byte) error {
	return v.decode(b, "Ping")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *Ping) decode(b []byte, p string) error {
	var s struct {
		ID *string    `json:"id"`
		At *Timestamp `json:"at"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out Ping
	if s.ID == nil {
		return badf(p+".id", "required field is missing")
	}
	out.ID = *s.ID
	if s.At != nil {
		out.At = s.At
	}
	if out.At != nil {
		if err := checkTimestamp(*out.At, p+".at"); err != nil {
			return err
		}
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "ping".
func (Ping) WireTag() string { return "ping" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v Ping) MarshalJSON() ([]byte, error) {
	type alias Ping
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "ping", alias: alias(v)})
}

func (Ping) isClientFrame() {}

func (Ping) isServerFrame() {}

type Pong struct {
	// The id of the ping being answered, verbatim.
	ID string     `json:"id"`
	At *Timestamp `json:"at,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a Pong: required fields must be
// present, and every constrained value is checked against the schema.
func (v *Pong) UnmarshalJSON(b []byte) error {
	return v.decode(b, "Pong")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *Pong) decode(b []byte, p string) error {
	var s struct {
		ID *string    `json:"id"`
		At *Timestamp `json:"at"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out Pong
	if s.ID == nil {
		return badf(p+".id", "required field is missing")
	}
	out.ID = *s.ID
	if s.At != nil {
		out.At = s.At
	}
	if out.At != nil {
		if err := checkTimestamp(*out.At, p+".at"); err != nil {
			return err
		}
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "pong".
func (Pong) WireTag() string { return "pong" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v Pong) MarshalJSON() ([]byte, error) {
	type alias Pong
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "pong", alias: alias(v)})
}

func (Pong) isClientFrame() {}

func (Pong) isServerFrame() {}

type OutboundAttachment struct {
	Kind string `json:"kind"`
	// Handle returned by the presigned-upload flow. The client sends this, never the
	// media: dimensions, duration, peaks, EXIF stripping and the blurhash are all computed
	// server-side on ingest, so the client cannot report them and cannot get them wrong.
	UploadID Uuid `json:"upload_id"`
}

// UnmarshalJSON decodes and VALIDATES a OutboundAttachment: required fields must be
// present, and every constrained value is checked against the schema.
func (v *OutboundAttachment) UnmarshalJSON(b []byte) error {
	return v.decode(b, "OutboundAttachment")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *OutboundAttachment) decode(b []byte, p string) error {
	var s struct {
		Kind     *string `json:"kind"`
		UploadID *Uuid   `json:"upload_id"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out OutboundAttachment
	if s.Kind == nil {
		return badf(p+".kind", "required field is missing")
	}
	out.Kind = *s.Kind
	if s.UploadID == nil {
		return badf(p+".upload_id", "required field is missing")
	}
	out.UploadID = *s.UploadID
	if !oneOf(out.Kind, []string{"voice", "image"}) {
		return badf(p+".kind", "expected one of %s, got %q", "voice|image", out.Kind)
	}
	if err := checkUuid(out.UploadID, p+".upload_id"); err != nil {
		return err
	}
	*v = out
	return nil
}

type ClientSend struct {
	// The idempotency key, generated by the client once per logical message and REUSED
	// verbatim on every retry. Same pattern as Switchyard. The server deduplicates on
	// (account, client_id) and answers a duplicate with the original `ack`, so a retry
	// after an ambiguous failure is free and safe — which is what makes an offline outbox
	// correct rather than hopeful.
	//
	// A client that mints a fresh key on retry has re-implemented double-sending.
	ClientID       Uuid                 `json:"client_id"`
	ConversationID Uuid                 `json:"conversation_id"`
	Text           *string              `json:"text,omitempty"`
	Attachments    []OutboundAttachment `json:"attachments,omitempty"`
	// Just the id — the server builds the whole ReplyRef from the live source message, so
	// the preview is never a stale copy taken at send time.
	ReplyToMessageID *Uuid `json:"reply_to_message_id,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a ClientSend: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ClientSend) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ClientSend")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ClientSend) decode(b []byte, p string) error {
	var s struct {
		ClientID         *Uuid              `json:"client_id"`
		ConversationID   *Uuid              `json:"conversation_id"`
		Text             *string            `json:"text"`
		Attachments      *[]json.RawMessage `json:"attachments"`
		ReplyToMessageID *Uuid              `json:"reply_to_message_id"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ClientSend
	if s.ClientID == nil {
		return badf(p+".client_id", "required field is missing")
	}
	out.ClientID = *s.ClientID
	if s.ConversationID == nil {
		return badf(p+".conversation_id", "required field is missing")
	}
	out.ConversationID = *s.ConversationID
	if s.Text != nil {
		out.Text = s.Text
	}
	if s.Attachments != nil {
		out.Attachments = make([]OutboundAttachment, len(*s.Attachments))
		for i, raw := range *s.Attachments {
			if err := out.Attachments[i].decode(raw, fmt.Sprintf("%s[%d]", p+".attachments", i)); err != nil {
				return err
			}
		}
	}
	if s.ReplyToMessageID != nil {
		out.ReplyToMessageID = s.ReplyToMessageID
	}
	if err := checkUuid(out.ClientID, p+".client_id"); err != nil {
		return err
	}
	if err := checkUuid(out.ConversationID, p+".conversation_id"); err != nil {
		return err
	}
	if out.ReplyToMessageID != nil {
		if err := checkUuid(*out.ReplyToMessageID, p+".reply_to_message_id"); err != nil {
			return err
		}
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "send".
func (ClientSend) WireTag() string { return "send" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ClientSend) MarshalJSON() ([]byte, error) {
	type alias ClientSend
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "send", alias: alias(v)})
}

func (ClientSend) isClientFrame() {}

// The other half of the send/ack pair. Receiving this moves the outbox entry from
// `sending` to `sent` and fixes its seq.
type ServerAck struct {
	// The key from the `send` this answers.
	ClientID       Uuid   `json:"client_id"`
	MessageID      Uuid   `json:"message_id"`
	ConversationID Uuid   `json:"conversation_id"`
	Seq            Seq    `json:"seq"`
	LogSeq         LogSeq `json:"log_seq"`
	// The server's timestamp for the message. Authoritative — the client's own send time
	// is never persisted, so two devices with different clocks cannot order a conversation
	// differently.
	At Timestamp `json:"at"`
	// True when this ack replays an earlier send with the same `client_id`. The client
	// treats it identically; it exists so the deduplication path is observable in logs and
	// in R1's zero-duplication test rather than being invisibly correct.
	Duplicate *bool `json:"duplicate,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a ServerAck: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ServerAck) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ServerAck")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ServerAck) decode(b []byte, p string) error {
	var s struct {
		ClientID       *Uuid      `json:"client_id"`
		MessageID      *Uuid      `json:"message_id"`
		ConversationID *Uuid      `json:"conversation_id"`
		Seq            *Seq       `json:"seq"`
		LogSeq         *LogSeq    `json:"log_seq"`
		At             *Timestamp `json:"at"`
		Duplicate      *bool      `json:"duplicate"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ServerAck
	if s.ClientID == nil {
		return badf(p+".client_id", "required field is missing")
	}
	out.ClientID = *s.ClientID
	if s.MessageID == nil {
		return badf(p+".message_id", "required field is missing")
	}
	out.MessageID = *s.MessageID
	if s.ConversationID == nil {
		return badf(p+".conversation_id", "required field is missing")
	}
	out.ConversationID = *s.ConversationID
	if s.Seq == nil {
		return badf(p+".seq", "required field is missing")
	}
	out.Seq = *s.Seq
	if s.LogSeq == nil {
		return badf(p+".log_seq", "required field is missing")
	}
	out.LogSeq = *s.LogSeq
	if s.At == nil {
		return badf(p+".at", "required field is missing")
	}
	out.At = *s.At
	if s.Duplicate != nil {
		out.Duplicate = s.Duplicate
	}
	if err := checkUuid(out.ClientID, p+".client_id"); err != nil {
		return err
	}
	if err := checkUuid(out.MessageID, p+".message_id"); err != nil {
		return err
	}
	if err := checkUuid(out.ConversationID, p+".conversation_id"); err != nil {
		return err
	}
	if err := checkSeq(out.Seq, p+".seq"); err != nil {
		return err
	}
	if err := checkLogSeq(out.LogSeq, p+".log_seq"); err != nil {
		return err
	}
	if err := checkTimestamp(out.At, p+".at"); err != nil {
		return err
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "ack".
func (ServerAck) WireTag() string { return "ack" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ServerAck) MarshalJSON() ([]byte, error) {
	type alias ServerAck
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "ack", alias: alias(v)})
}

func (ServerAck) isServerFrame() {}

// Carries the WHOLE message, deliberately. IDEA-23 is explicit that this must not
// degrade into a ping that makes the client go and fetch: that doubles latency on
// every message in the steady state. The internal Postgres NOTIFY payload between
// server instances is the thing that carries only (conversation_id, seq) under its
// 8000-byte cap — that is a different layer and never appears on this wire.
type ServerMessageFrame struct {
	Message Message `json:"message"`
}

// UnmarshalJSON decodes and VALIDATES a ServerMessageFrame: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ServerMessageFrame) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ServerMessageFrame")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ServerMessageFrame) decode(b []byte, p string) error {
	var s struct {
		Message *json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ServerMessageFrame
	if s.Message == nil {
		return badf(p+".message", "required field is missing")
	}
	if err := out.Message.decode(*s.Message, p+".message"); err != nil {
		return err
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "message".
func (ServerMessageFrame) WireTag() string { return "message" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ServerMessageFrame) MarshalJSON() ([]byte, error) {
	type alias ServerMessageFrame
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "message", alias: alias(v)})
}

func (ServerMessageFrame) isServerFrame() {}

type ServerReceipt struct {
	ConversationID Uuid `json:"conversation_id"`
	UserID         Uuid `json:"user_id"`
	// Read receipts are a high-water mark, never per-message. One frame settles an
	// arbitrary backlog and they cannot arrive out of order in a way that matters, since a
	// lower mark than one already held is simply discarded.
	UpToSeq Seq `json:"up_to_seq"`
}

// UnmarshalJSON decodes and VALIDATES a ServerReceipt: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ServerReceipt) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ServerReceipt")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ServerReceipt) decode(b []byte, p string) error {
	var s struct {
		ConversationID *Uuid `json:"conversation_id"`
		UserID         *Uuid `json:"user_id"`
		UpToSeq        *Seq  `json:"up_to_seq"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ServerReceipt
	if s.ConversationID == nil {
		return badf(p+".conversation_id", "required field is missing")
	}
	out.ConversationID = *s.ConversationID
	if s.UserID == nil {
		return badf(p+".user_id", "required field is missing")
	}
	out.UserID = *s.UserID
	if s.UpToSeq == nil {
		return badf(p+".up_to_seq", "required field is missing")
	}
	out.UpToSeq = *s.UpToSeq
	if err := checkUuid(out.ConversationID, p+".conversation_id"); err != nil {
		return err
	}
	if err := checkUuid(out.UserID, p+".user_id"); err != nil {
		return err
	}
	if err := checkSeq(out.UpToSeq, p+".up_to_seq"); err != nil {
		return err
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "receipt".
func (ServerReceipt) WireTag() string { return "receipt" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ServerReceipt) MarshalJSON() ([]byte, error) {
	type alias ServerReceipt
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "receipt", alias: alias(v)})
}

func (ServerReceipt) isServerFrame() {}

type ClientRead struct {
	ConversationID Uuid `json:"conversation_id"`
	UpToSeq        Seq  `json:"up_to_seq"`
}

// UnmarshalJSON decodes and VALIDATES a ClientRead: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ClientRead) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ClientRead")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ClientRead) decode(b []byte, p string) error {
	var s struct {
		ConversationID *Uuid `json:"conversation_id"`
		UpToSeq        *Seq  `json:"up_to_seq"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ClientRead
	if s.ConversationID == nil {
		return badf(p+".conversation_id", "required field is missing")
	}
	out.ConversationID = *s.ConversationID
	if s.UpToSeq == nil {
		return badf(p+".up_to_seq", "required field is missing")
	}
	out.UpToSeq = *s.UpToSeq
	if err := checkUuid(out.ConversationID, p+".conversation_id"); err != nil {
		return err
	}
	if err := checkSeq(out.UpToSeq, p+".up_to_seq"); err != nil {
		return err
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "read".
func (ClientRead) WireTag() string { return "read" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ClientRead) MarshalJSON() ([]byte, error) {
	type alias ClientRead
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "read", alias: alias(v)})
}

func (ClientRead) isClientFrame() {}

type ClientTyping struct {
	ConversationID Uuid        `json:"conversation_id"`
	State          TypingState `json:"state"`
}

// UnmarshalJSON decodes and VALIDATES a ClientTyping: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ClientTyping) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ClientTyping")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ClientTyping) decode(b []byte, p string) error {
	var s struct {
		ConversationID *Uuid        `json:"conversation_id"`
		State          *TypingState `json:"state"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ClientTyping
	if s.ConversationID == nil {
		return badf(p+".conversation_id", "required field is missing")
	}
	out.ConversationID = *s.ConversationID
	if s.State == nil {
		return badf(p+".state", "required field is missing")
	}
	out.State = *s.State
	if err := checkUuid(out.ConversationID, p+".conversation_id"); err != nil {
		return err
	}
	if err := checkTypingState(out.State, p+".state"); err != nil {
		return err
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "typing".
func (ClientTyping) WireTag() string { return "typing" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ClientTyping) MarshalJSON() ([]byte, error) {
	type alias ClientTyping
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "typing", alias: alias(v)})
}

func (ClientTyping) isClientFrame() {}

type ServerTyping struct {
	ConversationID Uuid `json:"conversation_id"`
	// Who is currently typing, IN THE ORDER THEY STARTED. The order is normative because
	// spec card F's naming rule depends on it: one person is a first name, two or three
	// are comma-separated in start order, four or more collapse to "Several people"
	// because past three the list churns faster than it can be read. Sorting this list in
	// a client silently changes the rendered string, which is exactly the kind of
	// divergence R4 exists to prevent — the rule is shared, so its input has to be too.
	UserIds []Uuid `json:"user_ids"`
}

// UnmarshalJSON decodes and VALIDATES a ServerTyping: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ServerTyping) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ServerTyping")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ServerTyping) decode(b []byte, p string) error {
	var s struct {
		ConversationID *Uuid   `json:"conversation_id"`
		UserIds        *[]Uuid `json:"user_ids"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ServerTyping
	if s.ConversationID == nil {
		return badf(p+".conversation_id", "required field is missing")
	}
	out.ConversationID = *s.ConversationID
	if s.UserIds == nil {
		return badf(p+".user_ids", "required field is missing")
	}
	out.UserIds = *s.UserIds
	if err := checkUuid(out.ConversationID, p+".conversation_id"); err != nil {
		return err
	}
	for i0, v0 := range out.UserIds {
		_ = i0
		if err := checkUuid(v0, fmt.Sprintf("%s[%d]", p+".user_ids", i0)); err != nil {
			return err
		}
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "typing".
func (ServerTyping) WireTag() string { return "typing" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ServerTyping) MarshalJSON() ([]byte, error) {
	type alias ServerTyping
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "typing", alias: alias(v)})
}

func (ServerTyping) isServerFrame() {}

type ServerError struct {
	Code ErrorCode `json:"code"`
	// Human-readable, for logs and for the inline error on a failed send. The canvas shows
	// send failures inline on the message, never as a toast.
	Message string `json:"message"`
	// Whether re-sending the identical frame could succeed. This is the server's
	// judgement, not the client's guess: `rate_limited` is retryable, `not_a_member` is
	// not, and a client that retried the latter forever would look exactly like a client
	// with no error handling.
	Retryable bool `json:"retryable"`
	// Present when the error is attributable to a specific `send`, so the right outbox
	// entry goes to `failed` instead of all of them.
	ClientID      *Uuid  `json:"client_id,omitempty"`
	RetryAfterSec *int64 `json:"retry_after_sec,omitempty"`
}

// UnmarshalJSON decodes and VALIDATES a ServerError: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ServerError) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ServerError")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ServerError) decode(b []byte, p string) error {
	var s struct {
		Code          *ErrorCode `json:"code"`
		Message       *string    `json:"message"`
		Retryable     *bool      `json:"retryable"`
		ClientID      *Uuid      `json:"client_id"`
		RetryAfterSec *int64     `json:"retry_after_sec"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ServerError
	if s.Code == nil {
		return badf(p+".code", "required field is missing")
	}
	out.Code = *s.Code
	if s.Message == nil {
		return badf(p+".message", "required field is missing")
	}
	out.Message = *s.Message
	if s.Retryable == nil {
		return badf(p+".retryable", "required field is missing")
	}
	out.Retryable = *s.Retryable
	if s.ClientID != nil {
		out.ClientID = s.ClientID
	}
	if s.RetryAfterSec != nil {
		out.RetryAfterSec = s.RetryAfterSec
	}
	if err := checkErrorCode(out.Code, p+".code"); err != nil {
		return err
	}
	if out.ClientID != nil {
		if err := checkUuid(*out.ClientID, p+".client_id"); err != nil {
			return err
		}
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "error".
func (ServerError) WireTag() string { return "error" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ServerError) MarshalJSON() ([]byte, error) {
	type alias ServerError
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "error", alias: alias(v)})
}

func (ServerError) isServerFrame() {}

// The server cannot stream the requested gap and the client must catch up over `/sync`
// instead. Naming this as its own frame rather than letting the client infer it from a
// short stream is deliberate: silent partial resume is how a client ends up
// confidently missing messages.
type ServerResyncRequired struct {
	Reason string `json:"reason"`
	// The server's current head, so the client knows what it is syncing towards.
	LogSeq LogSeq `json:"log_seq"`
}

// UnmarshalJSON decodes and VALIDATES a ServerResyncRequired: required fields must be
// present, and every constrained value is checked against the schema.
func (v *ServerResyncRequired) UnmarshalJSON(b []byte) error {
	return v.decode(b, "ServerResyncRequired")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *ServerResyncRequired) decode(b []byte, p string) error {
	var s struct {
		Reason *string `json:"reason"`
		LogSeq *LogSeq `json:"log_seq"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out ServerResyncRequired
	if s.Reason == nil {
		return badf(p+".reason", "required field is missing")
	}
	out.Reason = *s.Reason
	if s.LogSeq == nil {
		return badf(p+".log_seq", "required field is missing")
	}
	out.LogSeq = *s.LogSeq
	if !oneOf(out.Reason, []string{"cursor_too_old", "membership_changed", "retention_purge"}) {
		return badf(p+".reason", "expected one of %s, got %q", "cursor_too_old|membership_changed|retention_purge", out.Reason)
	}
	if err := checkLogSeq(out.LogSeq, p+".log_seq"); err != nil {
		return err
	}
	*v = out
	return nil
}

// WireTag returns the discriminator value "resync_required".
func (ServerResyncRequired) WireTag() string { return "resync_required" }

// MarshalJSON injects the constant "type" tag, so the tag cannot be
// forgotten at a call site or set to something the schema does not allow.
func (v ServerResyncRequired) MarshalJSON() ([]byte, error) {
	type alias ServerResyncRequired
	return json.Marshal(struct {
		T string `json:"type"`
		alias
	}{T: "resync_required", alias: alias(v)})
}

func (ServerResyncRequired) isServerFrame() {}

// Response to `GET /sync?after=<log_seq>&limit=<n>`. The RECONNECT AND CATCH-UP path,
// not the steady state — steady state is a `message` frame over the socket. Applying a
// page is idempotent: every message carries its own seq and id, so replaying an
// overlapping page cannot duplicate anything, which is what makes reconnection a query
// rather than a guess.
type SyncResponse struct {
	// The cursor to send on the NEXT call. Always the caller's new high-water mark — not
	// the server's head, which may be further along when `has_more` is true. Deriving this
	// client-side by maxing over `messages` is wrong the moment a page contains no
	// messages the client can see.
	LogSeq LogSeq `json:"log_seq"`
	// Ascending by log_seq. The ordering is normative so a client can apply the page as a
	// stream and stop anywhere without leaving a hole behind its cursor.
	Messages []Message `json:"messages"`
	// Every conversation touched by this page, plus any whose metadata changed — head_seq,
	// first_unread_seq, membership. A client learns about a brand-new conversation here,
	// which is why the sync cursor cannot be a per-conversation map.
	Conversations []Conversation `json:"conversations"`
	// Every user referenced by this page — as a message author, a reply's author, or a
	// member of a returned conversation.
	//
	// Without this there is NO path from `author_id` to a display name, and both clients
	// would have to invent one. Messages carry ids rather than embedded authors so a
	// rename lands everywhere at once instead of being frozen into every message ever
	// sent; this array is the other half of that decision, and omitting it would have made
	// the id-only design unusable rather than normalised.
	//
	// Send the full record whenever a user first appears on a page or their name or
	// initials changed. Clients cache by id and treat a later record as authoritative.
	Users []User `json:"users"`
	// Call again with the returned `log_seq`. The resync UI renders numeric progress
	// rather than a spinner, which is only possible because `Conversation.head_seq` says
	// what it is counting towards.
	HasMore    bool      `json:"has_more"`
	ServerTime Timestamp `json:"server_time"`
}

// UnmarshalJSON decodes and VALIDATES a SyncResponse: required fields must be
// present, and every constrained value is checked against the schema.
func (v *SyncResponse) UnmarshalJSON(b []byte) error {
	return v.decode(b, "SyncResponse")
}

// decode carries the JSON path, so a nested failure names the field it came
// from rather than the outermost type.
func (v *SyncResponse) decode(b []byte, p string) error {
	var s struct {
		LogSeq        *LogSeq            `json:"log_seq"`
		Messages      *[]json.RawMessage `json:"messages"`
		Conversations *[]json.RawMessage `json:"conversations"`
		Users         *[]json.RawMessage `json:"users"`
		HasMore       *bool              `json:"has_more"`
		ServerTime    *Timestamp         `json:"server_time"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return decodeErr(p, err)
	}
	var out SyncResponse
	if s.LogSeq == nil {
		return badf(p+".log_seq", "required field is missing")
	}
	out.LogSeq = *s.LogSeq
	if s.Messages == nil {
		return badf(p+".messages", "required field is missing")
	}
	out.Messages = make([]Message, len(*s.Messages))
	for i, raw := range *s.Messages {
		if err := out.Messages[i].decode(raw, fmt.Sprintf("%s[%d]", p+".messages", i)); err != nil {
			return err
		}
	}
	if s.Conversations == nil {
		return badf(p+".conversations", "required field is missing")
	}
	out.Conversations = make([]Conversation, len(*s.Conversations))
	for i, raw := range *s.Conversations {
		if err := out.Conversations[i].decode(raw, fmt.Sprintf("%s[%d]", p+".conversations", i)); err != nil {
			return err
		}
	}
	if s.Users == nil {
		return badf(p+".users", "required field is missing")
	}
	out.Users = make([]User, len(*s.Users))
	for i, raw := range *s.Users {
		if err := out.Users[i].decode(raw, fmt.Sprintf("%s[%d]", p+".users", i)); err != nil {
			return err
		}
	}
	if s.HasMore == nil {
		return badf(p+".has_more", "required field is missing")
	}
	out.HasMore = *s.HasMore
	if s.ServerTime == nil {
		return badf(p+".server_time", "required field is missing")
	}
	out.ServerTime = *s.ServerTime
	if err := checkLogSeq(out.LogSeq, p+".log_seq"); err != nil {
		return err
	}
	if err := checkTimestamp(out.ServerTime, p+".server_time"); err != nil {
		return err
	}
	*v = out
	return nil
}

// DecodeAttachment dispatches on "kind". A nil result with a nil error
// means an unrecognised tag, which callers MUST treat as "ignore and carry on".
func DecodeAttachment(b []byte) (Attachment, error) {
	return decodeAttachment(b, "Attachment")
}

// decodeAttachment is DecodeAttachment with the caller's JSON path, so a failure
// names the field the frame arrived in rather than the union type.
func decodeAttachment(b []byte, p string) (Attachment, error) {
	var probe struct {
		T string `json:"kind"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, decodeErr(p, err)
	}
	switch probe.T {
	case "voice":
		var v VoiceAttachment
		if err := v.decode(b, p+"[voice]"); err != nil {
			return nil, err
		}
		return v, nil
	case "image":
		var v ImageAttachment
		if err := v.decode(b, p+"[image]"); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, nil
}

// DecodeClientFrame dispatches on "type". A nil result with a nil error
// means an unrecognised tag, which callers MUST treat as "ignore and carry on".
func DecodeClientFrame(b []byte) (ClientFrame, error) {
	return decodeClientFrame(b, "ClientFrame")
}

// decodeClientFrame is DecodeClientFrame with the caller's JSON path, so a failure
// names the field the frame arrived in rather than the union type.
func decodeClientFrame(b []byte, p string) (ClientFrame, error) {
	var probe struct {
		T string `json:"type"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, decodeErr(p, err)
	}
	switch probe.T {
	case "hello":
		var v ClientHello
		if err := v.decode(b, p+"[hello]"); err != nil {
			return nil, err
		}
		return v, nil
	case "ping":
		var v Ping
		if err := v.decode(b, p+"[ping]"); err != nil {
			return nil, err
		}
		return v, nil
	case "pong":
		var v Pong
		if err := v.decode(b, p+"[pong]"); err != nil {
			return nil, err
		}
		return v, nil
	case "send":
		var v ClientSend
		if err := v.decode(b, p+"[send]"); err != nil {
			return nil, err
		}
		return v, nil
	case "read":
		var v ClientRead
		if err := v.decode(b, p+"[read]"); err != nil {
			return nil, err
		}
		return v, nil
	case "typing":
		var v ClientTyping
		if err := v.decode(b, p+"[typing]"); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, nil
}

// DecodeServerFrame dispatches on "type". A nil result with a nil error
// means an unrecognised tag, which callers MUST treat as "ignore and carry on".
func DecodeServerFrame(b []byte) (ServerFrame, error) {
	return decodeServerFrame(b, "ServerFrame")
}

// decodeServerFrame is DecodeServerFrame with the caller's JSON path, so a failure
// names the field the frame arrived in rather than the union type.
func decodeServerFrame(b []byte, p string) (ServerFrame, error) {
	var probe struct {
		T string `json:"type"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, decodeErr(p, err)
	}
	switch probe.T {
	case "ready":
		var v ServerReady
		if err := v.decode(b, p+"[ready]"); err != nil {
			return nil, err
		}
		return v, nil
	case "ping":
		var v Ping
		if err := v.decode(b, p+"[ping]"); err != nil {
			return nil, err
		}
		return v, nil
	case "pong":
		var v Pong
		if err := v.decode(b, p+"[pong]"); err != nil {
			return nil, err
		}
		return v, nil
	case "ack":
		var v ServerAck
		if err := v.decode(b, p+"[ack]"); err != nil {
			return nil, err
		}
		return v, nil
	case "message":
		var v ServerMessageFrame
		if err := v.decode(b, p+"[message]"); err != nil {
			return nil, err
		}
		return v, nil
	case "receipt":
		var v ServerReceipt
		if err := v.decode(b, p+"[receipt]"); err != nil {
			return nil, err
		}
		return v, nil
	case "typing":
		var v ServerTyping
		if err := v.decode(b, p+"[typing]"); err != nil {
			return nil, err
		}
		return v, nil
	case "error":
		var v ServerError
		if err := v.decode(b, p+"[error]"); err != nil {
			return nil, err
		}
		return v, nil
	case "resync_required":
		var v ServerResyncRequired
		if err := v.decode(b, p+"[resync_required]"); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, nil
}

// DecodeNamed decodes a named wire type. Unions return a nil value with a nil
// error for an unrecognised tag.
func DecodeNamed(name string, b []byte) (any, error) {
	switch name {
	case "User":
		var v User
		if err := v.decode(b, "User"); err != nil {
			return nil, err
		}
		return v, nil
	case "TranscriptSegment":
		var v TranscriptSegment
		if err := v.decode(b, "TranscriptSegment"); err != nil {
			return nil, err
		}
		return v, nil
	case "Transcript":
		var v Transcript
		if err := v.decode(b, "Transcript"); err != nil {
			return nil, err
		}
		return v, nil
	case "VoiceAttachment":
		var v VoiceAttachment
		if err := v.decode(b, "VoiceAttachment"); err != nil {
			return nil, err
		}
		return v, nil
	case "ImageAttachment":
		var v ImageAttachment
		if err := v.decode(b, "ImageAttachment"); err != nil {
			return nil, err
		}
		return v, nil
	case "Attachment":
		v, err := DecodeAttachment(b)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, nil
		}
		return v, nil
	case "ReplyRef":
		var v ReplyRef
		if err := v.decode(b, "ReplyRef"); err != nil {
			return nil, err
		}
		return v, nil
	case "Message":
		var v Message
		if err := v.decode(b, "Message"); err != nil {
			return nil, err
		}
		return v, nil
	case "Conversation":
		var v Conversation
		if err := v.decode(b, "Conversation"); err != nil {
			return nil, err
		}
		return v, nil
	case "ClientHello":
		var v ClientHello
		if err := v.decode(b, "ClientHello"); err != nil {
			return nil, err
		}
		return v, nil
	case "ServerReady":
		var v ServerReady
		if err := v.decode(b, "ServerReady"); err != nil {
			return nil, err
		}
		return v, nil
	case "Ping":
		var v Ping
		if err := v.decode(b, "Ping"); err != nil {
			return nil, err
		}
		return v, nil
	case "Pong":
		var v Pong
		if err := v.decode(b, "Pong"); err != nil {
			return nil, err
		}
		return v, nil
	case "OutboundAttachment":
		var v OutboundAttachment
		if err := v.decode(b, "OutboundAttachment"); err != nil {
			return nil, err
		}
		return v, nil
	case "ClientSend":
		var v ClientSend
		if err := v.decode(b, "ClientSend"); err != nil {
			return nil, err
		}
		return v, nil
	case "ServerAck":
		var v ServerAck
		if err := v.decode(b, "ServerAck"); err != nil {
			return nil, err
		}
		return v, nil
	case "ServerMessageFrame":
		var v ServerMessageFrame
		if err := v.decode(b, "ServerMessageFrame"); err != nil {
			return nil, err
		}
		return v, nil
	case "ServerReceipt":
		var v ServerReceipt
		if err := v.decode(b, "ServerReceipt"); err != nil {
			return nil, err
		}
		return v, nil
	case "ClientRead":
		var v ClientRead
		if err := v.decode(b, "ClientRead"); err != nil {
			return nil, err
		}
		return v, nil
	case "ClientTyping":
		var v ClientTyping
		if err := v.decode(b, "ClientTyping"); err != nil {
			return nil, err
		}
		return v, nil
	case "ServerTyping":
		var v ServerTyping
		if err := v.decode(b, "ServerTyping"); err != nil {
			return nil, err
		}
		return v, nil
	case "ServerError":
		var v ServerError
		if err := v.decode(b, "ServerError"); err != nil {
			return nil, err
		}
		return v, nil
	case "ServerResyncRequired":
		var v ServerResyncRequired
		if err := v.decode(b, "ServerResyncRequired"); err != nil {
			return nil, err
		}
		return v, nil
	case "ClientFrame":
		v, err := DecodeClientFrame(b)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, nil
		}
		return v, nil
	case "ServerFrame":
		v, err := DecodeServerFrame(b)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, nil
		}
		return v, nil
	case "SyncResponse":
		var v SyncResponse
		if err := v.decode(b, "SyncResponse"); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, fmt.Errorf("wire: unknown type %q", name)
}
