package store

// CANT-85 — the seam between a send and the uploads it names, and the one
// statement that writes what they resolve to.
//
// CANT-18 Ruling 1: the lookup lives behind an interface, and nothing here
// knows what an upload IS. CANT-48 mints the handles under CANT-47's storage
// decision and supplies the real resolver; until it lands the store refuses
// every attachment with upload_not_found, which is not a placeholder pretending
// to work — no client can hold a real handle, and that is the wire's own name
// for exactly this.
//
// What the store DOES decide, whatever resolver is wired, is decided here and in
// messages.go rather than left to each implementation: one row per requested
// attachment, the kind the sender asked for, and a position that is the send's
// order rather than the upload's. A refusal decided in the store is decided
// once — CANT-83's argument, one level down.

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UploadResolver turns the upload handles a send names into the attachment rows
// it stores. CANT-48 supplies the real one.
//
// THE CONTRACT, and SendMessage relies on every line of it:
//
//  1. Return exactly len(want) rows, in want's order, or an error. The store
//     checks the count and refuses a mismatch as `internal` — a resolver that
//     answers a different question is our bug, not the sender's.
//  2. A handle that does not resolve is ErrUploadNotFound, wrapped — INCLUDING
//     a handle another send has already consumed. That is safe to report as
//     not-found because the store re-reads the send's idempotency key before it
//     refuses (see send in messages.go), so the loser of a race under one key
//     gets the original message rather than the refusal.
//  3. LOCK WHAT YOU CONSUME, in tx. Line 2 is sound only because of this one. A
//     resolver that reads a consumed flag without taking the row lock can report
//     not-found while the winner of that race is still uncommitted; the store's
//     re-read then finds nothing, and a message that is about to exist is
//     refused as failed.
//  4. uploader is the sender's author_id, so a resolver can refuse a handle
//     minted by somebody else.
//  5. Position on a returned row is ignored. The store assigns it.
//  6. Lock only upload-side rows — never `conversations`, `messages` or
//     `log_counter`. Resolve runs at position 7, ABOVE the conversation draw,
//     so whatever it locks is first in this transaction's order; the lock-order
//     note in messages.go states the rule that follows for everybody else.
type UploadResolver interface {
	Resolve(ctx context.Context, tx pgx.Tx, uploader uuid.UUID, want []NewAttachment) ([]AttachmentRow, error)
}

// RefuseUploads is the resolver the service runs until CANT-48 lands, and the
// one New wires by default (CANT-85 Ruling 2).
//
// It touches neither tx nor the database. The cause names how many handles were
// asked for and not the handles themselves.
type RefuseUploads struct{}

// Resolve refuses everything.
func (RefuseUploads) Resolve(_ context.Context, _ pgx.Tx, _ uuid.UUID, want []NewAttachment) ([]AttachmentRow, error) {
	return nil, fmt.Errorf("store: %d upload(s) named and no upload flow exists yet (CANT-48): %w",
		len(want), ErrUploadNotFound)
}

// WithUploadResolver returns a store that resolves uploads through r, sharing
// this store's pool, bounds and logger.
//
// A COPY rather than a setter, so a store already handed to a transport cannot
// have its resolver swapped underneath a send in flight. The composition root
// calls this once; tests call it per case.
//
// Nil panics, for the same reason New's logger does: a nil resolver panics at
// the first photo rather than at boot.
func (s *Store) WithUploadResolver(r UploadResolver) *Store {
	if r == nil {
		panic("store.WithUploadResolver: resolver must not be nil (the default is RefuseUploads; leave it rather than clearing it)")
	}
	cp := *s
	cp.uploads = r
	return &cp
}

// resolveUploads is position 7.
//
// SKIPPED ENTIRELY for a send carrying no attachments. That is not an
// optimisation: RefuseUploads refuses whatever it is asked, so calling it with
// an empty list would refuse every text message the service accepts.
//
// The rows come back as the store will write them — checked, copied, and
// positioned — so position 11b inserts exactly what this returns and nothing
// between the two can reinterpret it.
func (s *Store) resolveUploads(ctx context.Context, tx pgx.Tx, m NewMessage) ([]AttachmentRow, error) {
	if len(m.Attachments) == 0 {
		return nil, nil
	}
	rows, err := s.uploads.Resolve(ctx, tx, m.AuthorID, m.Attachments)
	if err != nil {
		return nil, fmt.Errorf("store: resolve uploads: %w", err)
	}
	if len(rows) != len(m.Attachments) {
		return nil, fmt.Errorf("store: the resolver returned %d row(s) for %d attachment(s): %w",
			len(rows), len(m.Attachments), ErrUploadResolverContract)
	}

	out := make([]AttachmentRow, len(rows))
	for i, row := range rows {
		// RULING 1: THE KIND THE SENDER ASKED FOR, or no attachment at all.
		// Writing the upload's kind instead would serve an image where the
		// sender's client rendered a voice note, with no signal to anybody —
		// the client showing something the server did not keep. There is no
		// voice upload with that id, and upload_not_found already says so.
		if row.Kind != m.Attachments[i].Kind {
			return nil, fmt.Errorf("store: attachment %d was sent as %q and its upload is %q: %w",
				i, m.Attachments[i].Kind, row.Kind, ErrUploadNotFound)
		}
		// The SEND's order, overwriting whatever the resolver set. Order is a
		// fact about the message, not about the upload, and the wire promises
		// Message.attachments in position order.
		row.Position = i
		out[i] = row
	}
	return out, nil
}

// attachmentColumns is the column list position 11b writes, in the order
// attachmentValues produces them. The pure test in attachments_test.go pins the
// two to the same length and pins MaxAttachmentsCeiling × len(attachmentColumns)
// under the extended protocol's 65,535 parameters, so a column added here
// cannot quietly cross it.
//
// It is the twelve columns Sync's loadAttachments reads, plus id and
// message_id; cmd/catenary/servedattachments_test.go is what proves the two
// lists agree, by serving what this writes.
var attachmentColumns = []string{
	"id", "message_id", "kind", "storage_key", "position",
	"duration_ms", "peaks", "transcript_state", "transcript_json",
	"filename", "width", "height", "bytes", "placeholder",
}

func attachmentValues(id, messageID uuid.UUID, a AttachmentRow) []any {
	return []any{
		id, messageID, a.Kind, a.StorageKey, a.Position,
		a.DurationMs, a.Peaks, a.TranscriptState, a.TranscriptJSON,
		a.Filename, a.Width, a.Height, a.Bytes, a.Placeholder,
	}
}

// insertAttachments is position 11b: every row in ONE statement, on tx.
//
// One statement because the plan approved one, and because a message and its
// attachments must commit together or not at all — a failure here aborts the
// transaction and the deferred rollback takes the message and both ordinals
// with it, exactly as every failure below position 10 does. No savepoint and no
// retry.
//
// A VALUES list rather than unnest, because peaks is a per-row SMALLINT[] and
// unnest over a two-dimensional array flattens it. Rather than CopyFrom, because
// COPY is not an INSERT and its failures surface on a different path from every
// other statement in this transaction. Placeholders only: nothing a sender
// supplied is ever part of the statement text, and the row count is bounded by
// MaxAttachments, which Limits.Validate caps.
func insertAttachments(ctx context.Context, tx pgx.Tx, messageID uuid.UUID, rows []AttachmentRow) error {
	if len(rows) == 0 {
		return nil
	}
	var sql strings.Builder
	sql.WriteString("INSERT INTO attachments (")
	sql.WriteString(strings.Join(attachmentColumns, ", "))
	sql.WriteString(") VALUES ")

	args := make([]any, 0, len(rows)*len(attachmentColumns))
	for i, a := range rows {
		if i > 0 {
			sql.WriteString(", ")
		}
		sql.WriteByte('(')
		for j := range attachmentColumns {
			if j > 0 {
				sql.WriteString(", ")
			}
			fmt.Fprintf(&sql, "$%d", len(args)+j+1)
		}
		sql.WriteByte(')')
		// The id is the server's, like the message's. Nothing a resolver returns
		// names a row.
		args = append(args, attachmentValues(uuid.New(), messageID, a)...)
	}

	if _, err := tx.Exec(ctx, sql.String(), args...); err != nil {
		// Wrapped, so the PgError — SQLSTATE and constraint, never Detail — is
		// still what the refusal log reads.
		return fmt.Errorf("store: insert attachments: %w", err)
	}
	return nil
}
