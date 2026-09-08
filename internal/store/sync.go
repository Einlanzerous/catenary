package store

// CANT-20 — the reconnect and catch-up read.
//
// Deliberately NOT the steady state: a connected client gets full frames over
// the socket rather than a ping that sends it back to fetch. Degrading the
// socket into a notification doubles latency for every message.
//
// Everything here is a READ. Nothing in this file draws an ordinal, writes a
// row or takes a lock beyond the snapshot its transaction already has.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SyncPage is one page of the log as one reader may see it.
type SyncPage struct {
	// Messages ascending by log_seq. The ordering is normative: a client
	// applies the page as a stream and may stop anywhere without leaving a
	// hole behind its cursor.
	Messages []MessageRow

	// Attachments and ReplySources are keyed by the message they belong to, so
	// the mapper can be handed one message's rows without a second query.
	Attachments  map[uuid.UUID][]AttachmentRow
	ReplySources map[uuid.UUID]ReplySource

	Conversations []ConversationRow
	Users         []UserRow

	// HighWater is the cursor the client sends next, and it is NOT the highest
	// log_seq in Messages.
	//
	// The wire is explicit: "always the caller's new high-water mark — not the
	// server's head, which may be further along when has_more is true", and
	// "deriving this client-side by maxing over messages is wrong the moment a
	// page contains no messages the client can see". Both halves matter and
	// they pull opposite ways:
	//
	//   has_more  → the last message actually returned. Nothing beyond it was
	//               scanned, so claiming more would skip whatever sits between.
	//   caught up → the server's head at read time. Everything up to it WAS
	//               scanned, including rows this reader cannot see, and a
	//               cursor that stopped at the last visible message would
	//               re-scan those gaps on every call forever.
	HighWater int64

	// HasMore reports whether the page hit its limit with visible messages
	// still behind it, bounded by the head this page was read against.
	HasMore bool
}

// ConversationRow is a conversation as one reader sees it: the stored columns
// plus the three things that are per-reader or counted rather than stored.
type ConversationRow struct {
	ID            uuid.UUID
	Kind          string
	Name          *string
	LastSeq       int64
	RetentionDays *int32
	Muted         bool
	MemberCount   int64

	// FirstUnreadSeq is nil when the reader is fully caught up.
	//
	// 0005_read_seq_derivation: the first seq above this member's read_seq that
	// the VIEWER DID NOT AUTHOR. The author filter is what keeps your own
	// messages out of your own unread count, and it is why nothing writes
	// read_seq on a send.
	FirstUnreadSeq *int64

	// OtherMemberName is the display name a DIRECT conversation shows this
	// reader, because a direct has no stored name — 0002 declines to store one
	// precisely because the rail shows the other member, which is per reader
	// and cannot live in one column.
	OtherMemberName *string
}

// UserRow is a user as served. `initials` is derived at serve time and is not
// here, because storing it would be a second source of truth that goes stale on
// a rename.
type UserRow struct {
	ID          uuid.UUID
	DisplayName string
}

// DefaultSyncLimit is the page size when the caller names none.
const DefaultSyncLimit = 200

// MaxSyncLimit bounds what a caller may ask for. A page is assembled in memory
// and fans out to attachments, conversations and users, so an unbounded limit
// is an unbounded read on a shared Postgres.
const MaxSyncLimit = 500

// Sync reads one page of the log for viewer, after the given cursor.
//
// ONE TRANSACTION, and the head is read FIRST. The page is then bounded by that
// head rather than by "now", so HighWater cannot claim a position the page did
// not actually scan — a message committing between the two reads belongs to the
// next page, not to this one's cursor.
func (s *Store) Sync(ctx context.Context, viewer uuid.UUID, after int64, limit int) (SyncPage, error) {
	if limit <= 0 {
		limit = DefaultSyncLimit
	}
	if limit > MaxSyncLimit {
		limit = MaxSyncLimit
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SyncPage{}, fmt.Errorf("store: sync: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var page SyncPage
	var head int64
	if err := tx.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&head); err != nil {
		return SyncPage{}, fmt.Errorf("store: sync: read head: %w", err)
	}

	// limit+1 so the boundary is observed rather than guessed: the extra row is
	// the difference between "the page is full" and "there is more behind it".
	rows, err := tx.Query(ctx, `
		SELECT m.id, m.conversation_id, m.author_id, m.seq, m.log_seq, m.at,
		       m.text, m.client_id, m.reply_to, m.edited_at, m.deleted
		  FROM messages m
		  JOIN conversation_members cm
		    ON cm.conversation_id = m.conversation_id AND cm.user_id = $1
		 WHERE m.log_seq > $2 AND m.log_seq <= $3
		 ORDER BY m.log_seq
		 LIMIT $4`, viewer, after, head, limit+1)
	if err != nil {
		return SyncPage{}, fmt.Errorf("store: sync: messages: %w", err)
	}
	msgs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MessageRow, error) {
		var m MessageRow
		err := r.Scan(&m.ID, &m.ConversationID, &m.AuthorID, &m.Seq, &m.LogSeq, &m.At,
			&m.Text, &m.ClientID, &m.ReplyTo, &m.EditedAt, &m.Deleted)
		return m, err
	})
	if err != nil {
		return SyncPage{}, fmt.Errorf("store: sync: collect messages: %w", err)
	}

	page.HasMore = len(msgs) > limit
	if page.HasMore {
		msgs = msgs[:limit]
		page.HighWater = msgs[len(msgs)-1].LogSeq
	} else {
		page.HighWater = head
	}
	page.Messages = msgs

	if err := s.loadAttachments(ctx, tx, &page); err != nil {
		return SyncPage{}, err
	}
	if err := s.loadReplySources(ctx, tx, &page); err != nil {
		return SyncPage{}, err
	}
	if err := s.loadConversations(ctx, tx, viewer, &page); err != nil {
		return SyncPage{}, err
	}
	if err := s.loadUsers(ctx, tx, &page); err != nil {
		return SyncPage{}, err
	}
	return page, nil
}

func messageIDs(page *SyncPage) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(page.Messages))
	for _, m := range page.Messages {
		out = append(out, m.ID)
	}
	return out
}

func (s *Store) loadAttachments(ctx context.Context, tx pgx.Tx, page *SyncPage) error {
	page.Attachments = map[uuid.UUID][]AttachmentRow{}
	ids := messageIDs(page)
	if len(ids) == 0 {
		return nil
	}
	// ORDER BY position is here as well as in the mapper. Belt and braces on
	// purpose: the mapper sorts because it owns the wire's guarantee, and this
	// orders because a query that returns rows in an arbitrary order is one
	// somebody eventually reads without sorting.
	rows, err := tx.Query(ctx, `
		SELECT message_id, kind, position, storage_key, duration_ms, peaks,
		       transcript_state, transcript_json, filename, width, height, bytes, placeholder
		  FROM attachments
		 WHERE message_id = ANY($1)
		 ORDER BY message_id, position`, ids)
	if err != nil {
		return fmt.Errorf("store: sync: attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mid uuid.UUID
		var a AttachmentRow
		if err := rows.Scan(&mid, &a.Kind, &a.Position, &a.StorageKey, &a.DurationMs, &a.Peaks,
			&a.TranscriptState, &a.TranscriptJSON, &a.Filename, &a.Width, &a.Height,
			&a.Bytes, &a.Placeholder); err != nil {
			return fmt.Errorf("store: sync: scan attachment: %w", err)
		}
		page.Attachments[mid] = append(page.Attachments[mid], a)
	}
	return rows.Err()
}

// loadReplySources reads each referenced source AS IT IS NOW, which is what
// lets a reply to a voice note back-fill its preview when the transcript lands.
//
// Scoped to the same conversation as the reply, so a source that has since been
// swept or that never belonged here resolves to nothing and the served Message
// carries no reply_to — the same outcome CANT-18's send path stores as NULL.
func (s *Store) loadReplySources(ctx context.Context, tx pgx.Tx, page *SyncPage) error {
	page.ReplySources = map[uuid.UUID]ReplySource{}
	var ids []uuid.UUID
	for _, m := range page.Messages {
		if m.ReplyTo != nil {
			ids = append(ids, *m.ReplyTo)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT src.id, src.author_id, src.text,
		       a.kind, a.storage_key, a.duration_ms, a.transcript_json
		  FROM messages src
		  LEFT JOIN LATERAL (
		       SELECT kind, storage_key, duration_ms, transcript_json
		         FROM attachments WHERE message_id = src.id
		        ORDER BY position LIMIT 1
		  ) a ON TRUE
		 WHERE src.id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("store: sync: reply sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src ReplySource
		var kind, storageKey *string
		var durationMs *int64
		var transcript []byte
		if err := rows.Scan(&src.MessageID, &src.AuthorID, &src.Text,
			&kind, &storageKey, &durationMs, &transcript); err != nil {
			return fmt.Errorf("store: sync: scan reply source: %w", err)
		}
		if kind != nil {
			src.FirstAttachment = &AttachmentRow{
				Kind: *kind, StorageKey: derefStr(storageKey),
				DurationMs: durationMs, TranscriptJSON: transcript,
			}
		}
		page.ReplySources[src.MessageID] = src
	}
	return rows.Err()
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// loadConversations returns every conversation this page touches.
//
// NOT YET the other half of the wire's promise: "plus any whose metadata
// changed — head_seq, first_unread_seq, membership". That needs a
// per-conversation change marker, no column carries one, and no ticket owns it.
// A client learns about a conversation here the moment a message in it reaches
// the page, which covers the case the description leads with; a conversation
// renamed with no new message will not appear until one does. Stated rather
// than left for a client author to discover.
func (s *Store) loadConversations(ctx context.Context, tx pgx.Tx, viewer uuid.UUID, page *SyncPage) error {
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	for _, m := range page.Messages {
		if !seen[m.ConversationID] {
			seen[m.ConversationID] = true
			ids = append(ids, m.ConversationID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.kind, c.name, c.last_seq, c.retention_days, cm.muted,
		       (SELECT count(*) FROM conversation_members x WHERE x.conversation_id = c.id),
		       (SELECT min(m.seq) FROM messages m
		         WHERE m.conversation_id = c.id AND m.seq > cm.read_seq AND m.author_id <> $2),
		       (SELECT u.display_name FROM conversation_members o
		          JOIN users u ON u.id = o.user_id
		         WHERE o.conversation_id = c.id AND o.user_id <> $2
		         ORDER BY u.display_name LIMIT 1)
		  FROM conversations c
		  JOIN conversation_members cm
		    ON cm.conversation_id = c.id AND cm.user_id = $2
		 WHERE c.id = ANY($1)
		 ORDER BY c.id`, ids, viewer)
	if err != nil {
		return fmt.Errorf("store: sync: conversations: %w", err)
	}
	page.Conversations, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ConversationRow, error) {
		var c ConversationRow
		err := r.Scan(&c.ID, &c.Kind, &c.Name, &c.LastSeq, &c.RetentionDays, &c.Muted,
			&c.MemberCount, &c.FirstUnreadSeq, &c.OtherMemberName)
		return c, err
	})
	if err != nil {
		return fmt.Errorf("store: sync: collect conversations: %w", err)
	}
	return nil
}

// loadUsers returns every user this page references: a message author, a
// reply's author, or a member of a returned conversation.
//
// Without this there is no path from author_id to a display name. Messages
// carry ids rather than embedded authors so a rename lands everywhere at once,
// and this array is the other half of that decision.
func (s *Store) loadUsers(ctx context.Context, tx pgx.Tx, page *SyncPage) error {
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	add := func(id uuid.UUID) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, m := range page.Messages {
		add(m.AuthorID)
	}
	for _, src := range page.ReplySources {
		add(src.AuthorID)
	}
	var convIDs []uuid.UUID
	for _, c := range page.Conversations {
		convIDs = append(convIDs, c.ID)
	}
	if len(convIDs) > 0 {
		rows, err := tx.Query(ctx,
			`SELECT DISTINCT user_id FROM conversation_members WHERE conversation_id = ANY($1)`, convIDs)
		if err != nil {
			return fmt.Errorf("store: sync: members: %w", err)
		}
		members, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return fmt.Errorf("store: sync: collect members: %w", err)
		}
		for _, id := range members {
			add(id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx,
		`SELECT id, display_name FROM users WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return fmt.Errorf("store: sync: users: %w", err)
	}
	page.Users, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (UserRow, error) {
		var u UserRow
		err := r.Scan(&u.ID, &u.DisplayName)
		return u, err
	})
	if err != nil {
		return fmt.Errorf("store: sync: collect users: %w", err)
	}
	return nil
}

// ServerTime is the timestamp a SyncResponse carries, read at serve time.
func ServerTime() time.Time { return time.Now().UTC() }
