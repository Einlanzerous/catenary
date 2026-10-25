package store

// CANT-85 — the seam between a send and the uploads it names, and the one
// statement that writes what they resolve to.
//
// CANT-18 Ruling 1: the lookup lives behind an interface, and the send path
// knows nothing about what an upload IS. CANT-309 mints the handles under
// CANT-47's storage decision and supplies the real resolver (PostgresUploads,
// below); a store nobody wired with it refuses every attachment with
// upload_not_found, which is not a placeholder pretending to work — no client
// can hold a real handle, and that is the wire's own name for exactly this.
//
// What the store DOES decide, whatever resolver is wired, is decided here and in
// messages.go rather than left to each implementation: one row per requested
// attachment, the kind the sender asked for, and a position that is the send's
// order rather than the upload's. A refusal decided in the store is decided
// once — CANT-83's argument, one level down.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UploadResolver turns the upload handles a send names into the attachment rows
// it stores. PostgresUploads is the real one.
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

// RefuseUploads is the resolver New wires by default (CANT-85 Ruling 2), and the
// one a process runs when it was given no CATENARY_MEDIA_DIR.
//
// It touches neither tx nor the database. The cause names how many handles were
// asked for and not the handles themselves.
type RefuseUploads struct{}

// Resolve refuses everything.
func (RefuseUploads) Resolve(_ context.Context, _ pgx.Tx, _ uuid.UUID, want []NewAttachment) ([]AttachmentRow, error) {
	return nil, fmt.Errorf("store: %d upload(s) named and no media store is configured (CATENARY_MEDIA_DIR): %w",
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
// It is the columns Sync's loadAttachments reads, plus id and message_id;
// cmd/catenary/servedattachments_test.go is what proves the two lists agree, by
// serving what this writes.
var attachmentColumns = []string{
	"id", "message_id", "kind", "storage_key", "position",
	"duration_ms", "peaks", "transcript_state", "transcript_json",
	"filename", "width", "height", "bytes", "placeholder",
	"link_url", "link_title", "link_description", "link_site_name",
}

func attachmentValues(id, messageID uuid.UUID, a AttachmentRow) []any {
	return []any{
		id, messageID, a.Kind, storageKeyValue(a.Kind, a.StorageKey), a.Position,
		a.DurationMs, a.Peaks, a.TranscriptState, a.TranscriptJSON,
		a.Filename, a.Width, a.Height, a.Bytes, a.Placeholder,
		a.LinkURL, a.LinkTitle, a.LinkDescription, a.LinkSiteName,
	}
}

// storageKeyValue is what the storage_key column is given: NULL for a link card
// with no image, the key as it is for everything else. The kind test is the
// point. An empty key on a voice or image row is written as the empty string it
// has always been, and the two CHECKs (uploads_storage_key_present,
// attachments_storage_key_present) stay the only thing that decides which kinds
// may omit a blob.
func storageKeyValue(kind, key string) any {
	if kind == "link" && key == "" {
		return nil
	}
	return key
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

// NewUpload is one stored blob waiting for a send to take it.
type NewUpload struct {
	Uploader   uuid.UUID
	Kind       string // voice | image | link
	StorageKey string // the media store's key for the stored bytes; empty for a link with no image
	Bytes      int64  // 0 for a link with no image

	// Link only (CANT-308). LinkURL is required for kind link; the rest are nil
	// when the page did not provide them. Width, Height and Placeholder below
	// describe the card's image when it has one, with Width and Height together.
	LinkURL         *string
	LinkTitle       *string
	LinkDescription *string
	LinkSiteName    *string

	// Image only (CANT-310); nil leaves the column NULL.
	ThumbKey    *string
	Width       *int
	Height      *int
	Placeholder *string
	Filename    *string
}

// CreateUpload records an upload and returns its handle: the single-use id a
// send names in `upload_id`.
//
// The handle is a random UUID the server chose, so it is not derivable from the
// bytes, and the row binds it to its uploader (contract line 4 below).
func (s *Store) CreateUpload(ctx context.Context, u NewUpload) (uuid.UUID, error) {
	id := uuid.New()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO uploads (id, uploader, kind, storage_key, bytes, thumb_key, width, height, placeholder, filename,
		                      link_url, link_title, link_description, link_site_name)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		id, u.Uploader, u.Kind, storageKeyValue(u.Kind, u.StorageKey), u.Bytes, u.ThumbKey, u.Width, u.Height, u.Placeholder, u.Filename,
		u.LinkURL, u.LinkTitle, u.LinkDescription, u.LinkSiteName); err != nil {
		return uuid.Nil, fmt.Errorf("store: create upload: %w", err)
	}
	return id, nil
}

// PostgresUploads is the real UploadResolver (CANT-309): it consumes the
// uploads rows a send names, in the send's own transaction.
//
// THE CONTRACT LINES, AS THIS IMPLEMENTS THEM:
//
//	1  rows come back in want's order, one per handle.
//	2  a handle that is unknown, already consumed, listed twice or not ready is
//	   ErrUploadNotFound.
//	3  ONE statement, `UPDATE … WHERE id IN (SELECT … FOR UPDATE) RETURNING`, so
//	   the row lock and the consumed flag are the same step. A concurrent send
//	   naming the same handle blocks on the lock, and when the winner commits
//	   the loser's re-check of consumed_at IS NULL (READ COMMITTED re-evaluates
//	   the subquery's WHERE after waiting) finds nothing — which is the
//	   ErrUploadNotFound line 2 says is safe to report.
//	4  uploader = $2 is in the predicate: a handle minted by somebody else is
//	   indistinguishable from an unknown one, so this is not an oracle for which
//	   ids exist.
//	6  only the uploads table is touched. The subquery orders by id so two
//	   sends naming overlapping handles lock them in one order and cannot
//	   deadlock on each other.
//
// READY IS PART OF THE PREDICATE, NOT A CHECK AFTER IT. attachments requires
// peaks and duration_ms on a voice row and filename, width and height on an
// image row. A row missing them cannot become an attachment, and consuming it
// first would burn a handle for a send that can only fail; leaving it
// unconsumed means it resolves the moment the field is filled (peaks, CANT-56).
//
// A LINK is ready when it has its url (CANT-308); the CHECKs on uploads already
// make its image dimensions come as a pair. A card with no image has a NULL
// storage_key, so it is scanned into a nullable and an empty key means no blob.
type PostgresUploads struct{}

// Resolve implements UploadResolver.
func (PostgresUploads) Resolve(ctx context.Context, tx pgx.Tx, uploader uuid.UUID, want []NewAttachment) ([]AttachmentRow, error) {
	ids := make([]uuid.UUID, len(want))
	for i, w := range want {
		ids[i] = w.UploadID
	}

	rows, err := tx.Query(ctx, `
		UPDATE uploads SET consumed_at = now()
		 WHERE id IN (
		       SELECT id FROM uploads
		        WHERE id = ANY($1)
		          AND uploader = $2
		          AND consumed_at IS NULL
		          AND (   (kind = 'voice' AND peaks IS NOT NULL AND duration_ms IS NOT NULL)
		               OR (kind = 'image' AND width IS NOT NULL AND height IS NOT NULL AND filename IS NOT NULL)
		               OR (kind = 'link' AND link_url IS NOT NULL))
		        ORDER BY id
		          FOR UPDATE)
		RETURNING id, kind, storage_key, bytes, width, height, placeholder, filename, duration_ms, peaks,
		          link_url, link_title, link_description, link_site_name`,
		ids, uploader)
	if err != nil {
		return nil, fmt.Errorf("store: consume uploads: %w", err)
	}
	defer rows.Close()

	byID := make(map[uuid.UUID]AttachmentRow, len(want))
	for rows.Next() {
		var (
			id         uuid.UUID
			row        AttachmentRow
			storageKey *string
			bytes      int64
			width      *int32
			height     *int32
			durationMs *int32
			peaks      []int16
		)
		if err := rows.Scan(&id, &row.Kind, &storageKey, &bytes, &width, &height,
			&row.Placeholder, &row.Filename, &durationMs, &peaks,
			&row.LinkURL, &row.LinkTitle, &row.LinkDescription, &row.LinkSiteName); err != nil {
			return nil, fmt.Errorf("store: scan upload: %w", err)
		}
		row.StorageKey = derefStr(storageKey)
		if row.Kind != "link" {
			// A link card's attachment row has no byte count: it is not a wire
			// field, and the blob's size stays on the kept uploads row.
			row.Bytes = &bytes
		}
		if width != nil {
			w := int64(*width)
			row.Width = &w
		}
		if height != nil {
			h := int64(*height)
			row.Height = &h
		}
		if row.Kind == "voice" {
			d := int64(*durationMs)
			row.DurationMs = &d
			row.Peaks = make([]int64, len(peaks))
			for i, p := range peaks {
				row.Peaks[i] = int64(p)
			}
			// Transcription is a client of the estate ASR service (invariant 4),
			// and nothing has submitted this audio: it is pending, and says so.
			pending := "pending"
			row.TranscriptState = &pending
		}
		byID[id] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: consume uploads: %w", err)
	}

	// Count against the REQUEST, so a handle listed twice (one row consumed,
	// two wanted) and a handle that did not match both fall out here.
	out := make([]AttachmentRow, len(want))
	seen := make(map[uuid.UUID]bool, len(want))
	for i, w := range want {
		row, ok := byID[w.UploadID]
		if !ok || seen[w.UploadID] {
			return nil, fmt.Errorf("store: attachment %d: %w", i, ErrUploadNotFound)
		}
		seen[w.UploadID] = true
		out[i] = row
	}
	return out, nil
}

// UploadByStorageKey is what GET /media/{key} needs from uploads (CANT-311): the
// row's kind, for the sniffed content type of a voice blob, and its thumbnail's
// key. Found is false when no row names the key. Two rows can share a key (the
// store is content-addressed, so the same bytes uploaded twice are one blob), and
// any of them answers; a thumbnail is taken from whichever row has one.
func (s *Store) UploadByStorageKey(ctx context.Context, storageKey string) (kind, thumbKey string, found bool, err error) {
	var thumb *string
	err = s.pool.QueryRow(ctx,
		`SELECT kind, thumb_key FROM uploads WHERE storage_key = $1
		  ORDER BY (thumb_key IS NULL), created_at LIMIT 1`, storageKey).Scan(&kind, &thumb)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: upload by storage key: %w", err)
	}
	if thumb != nil {
		thumbKey = *thumb
	}
	return kind, thumbKey, true, nil
}
