package store

// CANT-84 — the rows a served Message is built from.
//
// These are the store's shape, not the wire's, and they are deliberately dumb:
// columns as they come out of Postgres, with no derivation and no viewer in
// sight. internal/wireview turns them into a wire.Message, and it is the only
// thing that does.
//
// They live here rather than in wireview because CLAUDE.md puts types beside
// the queries that return them. Nothing populates them yet — CANT-20's /sync,
// CANT-75's REST send and CANT-85's attachment insert are the three tickets
// that will, and the mapper is written against the shape rather than against
// whichever of them lands first.

import (
	"time"

	"github.com/google/uuid"
)

// MessageRow is one row of `messages`, as stored.
//
// Everything a reader is told that is NOT here is derived: `state` and
// `read_by` are per-reader (CANT-26), `client_id` is echoed only to its author,
// and attachments and the reply ref are their own rows.
type MessageRow struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	AuthorID       uuid.UUID
	Seq            int64
	LogSeq         int64
	At             time.Time
	Text           *string

	// ClientID is on the row for every send, and reaches the wire only when
	// the viewer is the author. The mapper enforces that; nothing else should.
	ClientID *uuid.UUID

	// ReplyTo is the stored id. The served ReplyRef is built from the SOURCE
	// message live, never from a copy taken at send time, which is what lets a
	// reply to a voice note back-fill its preview when the transcript lands.
	ReplyTo *uuid.UUID

	EditedAt *time.Time
	Deleted  bool
}

// AttachmentRow is one row of `attachments`, as stored, in `position` order.
//
// One type for both kinds, matching the table: 0003_messages uses per-kind
// CHECK constraints rather than two tables, because the wire has one
// `attachments` array. Which fields are populated follows `kind`.
type AttachmentRow struct {
	Kind     string // voice | image, per the column's CHECK
	Position int

	// StorageKey is OPAQUE and never reaches the wire. The served `url` is
	// derived from it at serve time, because whether that is a presigned R2 GET
	// or a Traefik route is CANT-47's decision and a stored URL would be wrong
	// the moment that lands.
	StorageKey string

	// voice
	DurationMs      *int64
	Peaks           []int64
	TranscriptState *string
	TranscriptJSON  []byte

	// image
	Filename    *string
	Width       *int64
	Height      *int64
	Bytes       *int64
	Placeholder *string
}

// ReplySource is what the served ReplyRef is built from: the source message as
// it is NOW, not as it was when the reply was sent.
//
// FirstAttachment is the source's attachment at position 0, or nil. It is what
// decides ReplyRef.kind and carries the duration and storage key a voice ref
// needs — the wire's ReplyRef has no room for more than one.
type ReplySource struct {
	MessageID       uuid.UUID
	AuthorID        uuid.UUID
	Text            *string
	FirstAttachment *AttachmentRow
}
