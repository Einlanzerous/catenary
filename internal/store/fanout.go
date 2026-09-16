package store

// CANT-107 — what the hub reads between a NOTIFY and a `message` frame.
//
// A notification carries ids only (notify.go), so the receiving instance has
// to read the row it is being told about — and it has to read it PER VIEWER,
// because `state`, `read_by` and the echoed `client_id` are the reader's and
// not the row's (wireview). Everything the hub needs for every member is
// therefore one read, here, and the hub never touches the pool itself.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Member is one row of conversation_members as the fan-out sees it: who, and
// how far they have read. ReadSeq is what decides `delivered` versus `read`
// for a message they did not write.
type Member struct {
	UserID  uuid.UUID
	ReadSeq int64
}

// FanoutMessage is one message and everything a per-viewer Message is built
// from, plus the list of viewers it may be built for.
type FanoutMessage struct {
	Message     MessageRow
	Attachments []AttachmentRow
	// ReplySource is the source as it is NOW, scoped to this conversation, or
	// nil. wireview applies the same-thread rule on top; see sync.go's
	// loadReplySources for why the scope here is a membership guard and not
	// that rule.
	ReplySource *ReplySource
	// ReadBy counts the same population member_count does, the author
	// included — readstate.go owns the reasoning.
	ReadBy int64
	// Members is EVERY current member, read in the same snapshot as the
	// message. The hub intersects it with the sessions it holds.
	Members []Member
}

// MessageForFanout reads one message by (conversation_id, seq) — the UNIQUE
// the thread ordering hangs off, and exactly what a NotifyPayload carries —
// with its attachments, its reply source, its read_by count and the
// conversation's members.
//
// ONE TRANSACTION, REPEATABLE READ, READ ONLY. The default is READ COMMITTED,
// where every statement takes its own snapshot (loadConversations in sync.go
// says so about Sync), and under that default the member list could see a
// join or a departure the message read did not. REPEATABLE READ pins one
// snapshot for all five statements, so "the members of this conversation at
// the moment this message was read" is a sentence that is actually true. A
// read-only transaction cannot raise a serialization failure, so the stricter
// level costs nothing.
//
// The three page loaders are reused over a one-message SyncPage rather than
// re-written for one row: the SQL for attachments, reply sources and read_by
// exists once, in sync.go, and a second copy is a second thing to keep right.
//
// A missing row is ErrNotFound — the row was purged, or something else
// NOTIFYed on the channel — and the hub skips it. Every other error is
// returned raw for the hub to classify with IsTransient.
func (s *Store) MessageForFanout(ctx context.Context, conv uuid.UUID, seq int64) (FanoutMessage, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return FanoutMessage{}, fmt.Errorf("store: fanout: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var m MessageRow
	err = tx.QueryRow(ctx, `
		SELECT m.id, m.conversation_id, m.author_id, m.seq, m.log_seq, m.at,
		       m.text, m.client_id, m.reply_to, m.edited_at, m.deleted
		  FROM messages m
		 WHERE m.conversation_id = $1 AND m.seq = $2`, conv, seq).
		Scan(&m.ID, &m.ConversationID, &m.AuthorID, &m.Seq, &m.LogSeq, &m.At,
			&m.Text, &m.ClientID, &m.ReplyTo, &m.EditedAt, &m.Deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return FanoutMessage{}, fmt.Errorf("store: fanout: message %s/%d: %w", conv, seq, ErrNotFound)
	}
	if err != nil {
		return FanoutMessage{}, fmt.Errorf("store: fanout: message: %w", err)
	}

	page := SyncPage{Messages: []MessageRow{m}}
	if err := s.loadAttachments(ctx, tx, &page); err != nil {
		return FanoutMessage{}, err
	}
	if err := s.loadReplySources(ctx, tx, &page); err != nil {
		return FanoutMessage{}, err
	}
	if err := s.loadReadBy(ctx, tx, &page); err != nil {
		return FanoutMessage{}, err
	}

	rows, err := tx.Query(ctx, `
		SELECT user_id, read_seq FROM conversation_members
		 WHERE conversation_id = $1
		 ORDER BY user_id`, conv)
	if err != nil {
		return FanoutMessage{}, fmt.Errorf("store: fanout: members: %w", err)
	}
	members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var mem Member
		err := r.Scan(&mem.UserID, &mem.ReadSeq)
		return mem, err
	})
	if err != nil {
		return FanoutMessage{}, fmt.Errorf("store: fanout: collect members: %w", err)
	}

	out := FanoutMessage{
		Message:     m,
		Attachments: page.Attachments[m.ID],
		ReadBy:      page.ReadBy[m.ID],
		Members:     members,
	}
	if m.ReplyTo != nil {
		if src, ok := page.ReplySources[*m.ReplyTo]; ok {
			out.ReplySource = &src
		}
	}
	return out, nil
}

// ReadNotifyMessage is one message CANT-92's receipt re-emission refreshes,
// from its AUTHOR's own point of view — the only viewer CANT-90 ruling 2 lets
// see it. Lighter than FanoutMessage: there is exactly one viewer per row
// here (the row's own author), so there is no member list to intersect
// against sessions the way OnNotify's per-conversation fan-out needs one.
type ReadNotifyMessage struct {
	Message     MessageRow
	Attachments []AttachmentRow
	ReplySource *ReplySource
	// ReadBy is what changed: the count behind this receipt's new mark,
	// counting the same population member_count does, the author included —
	// readstate.go owns the reasoning.
	ReadBy int64
}

// MessagesForReadNotify reads the messages a receipt's span changed the
// read_by of, for CANT-92's live re-emission: seq IN (before, after] in conv,
// excluding the reader's own messages and any author who is no longer a
// current member, newest `capPerAuthor` per remaining author.
//
// THE READER'S OWN MESSAGES ARE EXCLUDED HERE, NOT AT THE HUB, and it is not
// an optimisation — it is what makes the ticket's own claim true. readByExpr
// counts the author by identity regardless of read_seq (readstate.go), so a
// member's read_seq passing their OWN message never changes that message's
// read_by: there is nothing to re-emit, and a query that included it would
// put a no-op frame on the wire and waste a slot in the cap on it.
//
// A DEPARTED AUTHOR IS EXCLUDED TOO, on hub.go's own header: "MEMBERSHIP IS
// READ, NEVER CACHED … a cached index would keep serving a departed member."
// messages.author_id is ON DELETE RESTRICT, so the row — and its author_id —
// outlives a member leaving; without this guard a departed author who still
// holds a live socket on this instance would get a `message` frame for a room
// they are no longer in, carrying read_by and a reply_to preview computed
// over CURRENT membership. Nothing removes a conversation_members row today
// (CANT-75 will), so this is a latent seam rather than a live one — closed
// now because it is one EXISTS in a query already inside this transaction's
// snapshot, and much cheaper here than after CANT-75 ships.
//
// THE CAP IS PER AUTHOR, NOT PER RECEIPT, on the same reasoning CANT-92
// settled: a wide span can carry many authors, each going to a DIFFERENT
// session, so only ONE author's own outbox is ever at risk from this
// receipt — the number is passed in rather than owned here because the
// relationship to outboxBound is internal/hub's to state (readNotifyCap,
// beside outboxBound in hub.go).
//
// NEWEST FIRST, PER AUTHOR: `row_number() OVER (PARTITION BY author_id ORDER
// BY seq DESC)`, so a span far larger than the cap keeps the messages a
// client is actually looking at and lets the tail catch up at that author's
// next full bootstrap (CANT-89: a page never re-serves a message it already
// carried, so nothing short of one does).
//
// ONE TRANSACTION, REPEATABLE READ, READ ONLY — MessageForFanout's own
// reasoning: the read_by read must see the same membership snapshot the
// message read did, or "the read_by this receipt produced" stops being a
// sentence that is true. The membership check above runs in the SAME
// snapshot for the same reason.
func (s *Store) MessagesForReadNotify(ctx context.Context, conv, reader uuid.UUID, before, after int64, capPerAuthor int) ([]ReadNotifyMessage, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("store: read notify: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT ranked.id, ranked.conversation_id, ranked.author_id, ranked.seq, ranked.log_seq,
		       ranked.at, ranked.text, ranked.client_id, ranked.reply_to, ranked.edited_at, ranked.deleted
		  FROM (
			SELECT m.id, m.conversation_id, m.author_id, m.seq, m.log_seq, m.at, m.text,
			       m.client_id, m.reply_to, m.edited_at, m.deleted,
			       row_number() OVER (PARTITION BY m.author_id ORDER BY m.seq DESC) AS rn
			  FROM messages m
			 WHERE m.conversation_id = $1 AND m.seq > $2 AND m.seq <= $3 AND m.author_id <> $4
			   AND EXISTS (SELECT 1 FROM conversation_members cm
			                WHERE cm.conversation_id = m.conversation_id AND cm.user_id = m.author_id)
		  ) ranked
		 WHERE ranked.rn <= $5
		 ORDER BY ranked.author_id, ranked.seq DESC`,
		conv, before, after, reader, capPerAuthor)
	if err != nil {
		return nil, fmt.Errorf("store: read notify: query: %w", err)
	}
	messages, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MessageRow, error) {
		var m MessageRow
		err := r.Scan(&m.ID, &m.ConversationID, &m.AuthorID, &m.Seq, &m.LogSeq, &m.At,
			&m.Text, &m.ClientID, &m.ReplyTo, &m.EditedAt, &m.Deleted)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: read notify: collect: %w", err)
	}
	if len(messages) == 0 {
		return nil, nil
	}

	// The three page loaders are reused over this span rather than re-written
	// for it, on MessageForFanout's own argument: the SQL for attachments,
	// reply sources and read_by exists once, in sync.go, and a second copy is
	// a second thing to keep right.
	page := SyncPage{Messages: messages}
	if err := s.loadAttachments(ctx, tx, &page); err != nil {
		return nil, err
	}
	if err := s.loadReplySources(ctx, tx, &page); err != nil {
		return nil, err
	}
	if err := s.loadReadBy(ctx, tx, &page); err != nil {
		return nil, err
	}

	out := make([]ReadNotifyMessage, 0, len(page.Messages))
	for _, m := range page.Messages {
		rm := ReadNotifyMessage{
			Message:     m,
			Attachments: page.Attachments[m.ID],
			ReadBy:      page.ReadBy[m.ID],
		}
		if m.ReplyTo != nil {
			if src, ok := page.ReplySources[*m.ReplyTo]; ok {
				rm.ReplySource = &src
			}
		}
		out = append(out, rm)
	}
	return out, nil
}

// Members lists a conversation's member ids, for the receipt and typing
// fan-outs, which need to know who is in the room and nothing about any
// message.
//
// AN UNKNOWN CONVERSATION IS AN EMPTY LIST, not an error. The hub asks this
// on behalf of a caller who may not be a member, and an error here would let
// it tell a non-member whether a room they cannot see exists — the same
// reasoning MarkRead gives for answering not_a_member in both cases.
// Authorization is the caller's: the hub checks the sender against the list.
func (s *Store) Members(ctx context.Context, conv uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id FROM conversation_members
		 WHERE conversation_id = $1
		 ORDER BY user_id`, conv)
	if err != nil {
		return nil, fmt.Errorf("store: members: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("store: members: collect: %w", err)
	}
	return ids, nil
}

// Head is the server's current log_seq head: one SELECT on the pool, no
// transaction, no lock, nothing drawn. The number is a snapshot the moment it
// is read; what it is good for is telling a client what its catch-up is
// counting towards (`ready.log_seq`, `resync_required.log_seq`).
//
// Hello reads head through this. Sync does NOT — its head read has to be
// inside its own transaction so the page is bounded by the head it scanned,
// and sync.go says so.
func (s *Store) Head(ctx context.Context) (int64, error) {
	var head int64
	if err := s.pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&head); err != nil {
		return 0, fmt.Errorf("store: read head: %w", err)
	}
	return head, nil
}
