package store

// CANT-26 — read state. Where a receipt is written, what it is allowed to
// claim, and the one derivation everything unread is read off.
//
// There is NO STORED UNREAD COUNT, here or anywhere. The rail badge and the
// thread's "N NEW" divider both come from first_unread_seq, so they cannot
// drift apart, and neither counts the reader's own messages. That is Invariant
// 3 stated as a query rather than as a convention.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// firstUnreadSeqExpr IS THE DERIVATION, and it is one string because two
// copies of it are two answers to "how many unread".
//
// The first seq above this member's read_seq that this member did not write.
// `min` over `WHERE seq > read_seq` is an index scan forward on UNIQUE
// (conversation_id, seq) that stops at the first qualifying row — typically
// one row, worst case a run the reader authored themselves. 0002 called this a
// table scan and built arithmetic to avoid it; 0005 records that the premise
// was the mistake.
//
// THE AUTHOR FILTER IS THE WHOLE POINT. Without it a reader's own send counts
// toward their own unread, which CLAUDE.md forbids outright. CANT-83's removed
// criterion 6 tried to get there by advancing the author's own read_seq on
// send, which marked their unread backlog read whenever they replied without
// opening the thread. A scalar read_seq cannot say "1-5 unread, 6 is mine";
// this can.
//
// It correlates against `cm` rather than naming a conversation or a viewer, so
// both call sites are the same text over their own join.
const firstUnreadSeqExpr = `(SELECT min(m.seq) FROM messages m
	         WHERE m.conversation_id = cm.conversation_id
	           AND m.seq > cm.read_seq
	           AND m.author_id <> cm.user_id)`

// readByExpr counts how many members have read a message. `READ 5/7` in a room
// is this over Conversation.member_count.
//
// IT COUNTS THE SAME POPULATION member_count DOES, and that is the whole of the
// design here. The first version excluded the author from the numerator while
// the denominator counted everybody, so in a seven-member room where all seven
// had read a message the label said READ 6/7 — telling the author one person
// had not seen it when everybody had, permanently, because 7/7 was unreachable.
// The canvas's own reference data draws READ 7/7 and READ 9/9.
//
// THE AUTHOR COUNTS FROM THE MOMENT THE MESSAGE EXISTS, by derivation rather
// than by a receipt: `cm.user_id = m.author_id OR …`. You have read what you
// wrote, and their read_seq cannot say so — advancing it on send was built and
// removed, because it swallowed the sender's own unread backlog (0005). This is
// the same fact, in the one place where a derivation can express it without a
// scalar column having to.
const readByExpr = `(SELECT count(*) FROM conversation_members cm
	         WHERE cm.conversation_id = m.conversation_id
	           AND (cm.user_id = m.author_id OR cm.read_seq >= m.seq))`

// ErrNotAMember is returned when the reader is not in the conversation. It
// carries wire.ErrorCodeNotAMember through sendErrorTable, so a transport does
// not decide the code — see senderror.go.
//
// ErrSeqOutOfRange is NOT a wire code, deliberately. The ErrorCode enum has no
// member for "your input is unparseable", the closest (`internal`) would be a
// lie about whose fault it is, and adding one is a wire change under CANT-74's
// unresolved compatibility policy. syncHandler already answers that shape with
// a plain 400, and a receipt transport should do the same. Seq's minimum is 1
// on the wire, so a well-formed frame never carries this.
var ErrSeqOutOfRange = errors.New("store: up_to_seq must be at least 1")

// ReadReceipt is what a receipt DID, which is not always what it claimed.
type ReadReceipt struct {
	ConversationID uuid.UUID
	UserID         uuid.UUID

	// UpToSeq is the mark AFTER the write — the value to broadcast, not the
	// value that was asked for. A claim below the mark leaves it where it was
	// and a claim above the head is capped, so echoing the request back would
	// tell every other member something untrue about this one.
	UpToSeq int64

	// Advanced is false when the mark did not move: a duplicate receipt, one
	// that arrived out of order, or one already covered. CANT-21's fanout
	// should stay quiet on those rather than waking every device in the room
	// to tell it nothing changed.
	Advanced bool
}

// MarkRead advances a member's read mark, and it is the ONLY thing that writes
// read_seq. No other path touches the column, so its meaning is exactly "the
// highest seq this member has read" with no second author to reason about —
// which is what 0005 settled and what makes the derivation above trustworthy.
//
// IT TAKES TWO ROW LOCKS, IN THIS ORDER: conversation_members (the FOR UPDATE
// below) and then, only when the mark advances, log_counter through the
// metadata bump at the bottom of this function (CANT-91). This note used to
// claim a single lock and a disjoint lock set from the send path, and both
// clauses stopped being true when that bump landed — a comment that no
// longer matches the code is worse than none, because the next author plans
// against it.
//
// SendMessage locks conversations → messages → log_counter and, since CANT-83
// dropped the author's read_seq advance, never LOCKS this table: it reads it,
// at its membership EXISTS, and a read takes no row lock. So log_counter is
// the ONLY lock the two paths share, and both take it LAST. That is what rules
// out a cycle in either direction — a stronger reason than the disjoint lock
// sets this note used to claim, and it is the same sentence messages.go
// states outward. It holds for as long as the counter stays at the bottom of
// every writer's order, which is the rule that file exists to keep.
//
// "LAST" IS EXACT HERE AND ONLY "LAST AMONG SHARED LOCKS" THERE. This path
// locks nothing new after the draw: the bump writes the member row it already
// holds and commits. The send path is not so tidy — its insert takes KEY SHARE
// on `users` and `devices` AFTER the counter, and messages.go says so forty
// lines above the sentence it states outward. That is why every lock in
// metadata.go is FOR NO KEY UPDATE, which KEY SHARE passes through; anyone
// adding a lock to this path after the draw has to make the same argument, or
// take it before.
//
// Three rules, and the second is the one with teeth:
//
//   - MONOTONIC. A mark below the one held is discarded, which is what the wire
//     says a receipt is: a high-water mark that cannot arrive out of order in a
//     way that matters.
//
//   - CLAMPED TO THE HEAD. up_to_seq is client-supplied, and an unclamped claim
//     of 2^53-1 would mark every message that ever arrives afterwards as read,
//     permanently — and the member could not undo it, because a lower mark is
//     discarded by the rule above. One buggy client and that person never sees
//     an unread badge in that room again. The clamp makes the claim mean what
//     it says: I have read up to a message that exists.
//
//   - MEMBERS ONLY. A non-member gets not_a_member rather than a silent no-op,
//     because the two are different facts and the caller can tell them apart.
func (s *Store) MarkRead(ctx context.Context, conv, user uuid.UUID, upToSeq int64) (ReadReceipt, error) {
	r, err := s.markRead(ctx, conv, user, upToSeq)
	if err == nil {
		return r, nil
	}

	// ONE EXIT, ONE LOG LINE, the same shape SendMessage uses — the level and
	// the code-derived fields are senderror.go's decision and the ids are this
	// file's. A receipt carries no body, so there is nothing here that D1 would
	// keep out of a log; the rule holds for free rather than by care.
	//
	// ErrSeqOutOfRange is NOT classified: it is the caller's malformed input,
	// the enum has no member that says so, and `internal` would be a lie about
	// whose fault it is. It is returned raw so a transport can answer 400 the
	// way syncHandler already does. A transport that hands it to SendErrorFor
	// without checking will get `internal`, so CHECK IT FIRST.
	if errors.Is(err, ErrSeqOutOfRange) {
		return ReadReceipt{}, err
	}
	se := sendErrorFor(err)
	s.logger.Log(ctx, se.Level(), "read receipt refused", append([]any{
		"conversation_id", conv,
		"user_id", user,
		"up_to_seq", upToSeq,
	}, se.LogAttrs()...)...)
	return ReadReceipt{}, se
}

func (s *Store) markRead(ctx context.Context, conv, user uuid.UUID, upToSeq int64) (ReadReceipt, error) {
	if upToSeq < 1 {
		return ReadReceipt{}, fmt.Errorf("%w (got %d)", ErrSeqOutOfRange, upToSeq)
	}

	// TWO STATEMENTS IN ONE TRANSACTION, and the reason is Advanced rather than
	// the write. As one statement with a CTE, the "before" value came from the
	// statement's snapshot while the SET read the row again after the lock was
	// granted — so two of the same member's devices racing both saw before = 1,
	// and the loser reported Advanced for a mark it had not moved. UpToSeq was
	// still right; what was wrong was exactly the redundant broadcast the field
	// exists to suppress. FOR UPDATE reads the mark under the same lock the
	// write takes, so "did this call move it" is answerable.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReadReceipt{}, fmt.Errorf("store: mark read: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var before, after int64
	err = tx.QueryRow(ctx, `
		SELECT read_seq FROM conversation_members
		 WHERE conversation_id = $1 AND user_id = $2
		   FOR UPDATE`, conv, user).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row means no membership. `conversations` is joined rather than
		// checked separately, so a conversation that does not exist reaches
		// here too — and not_a_member is the true answer for that as well,
		// without telling a caller whether a room they cannot see exists.
		return ReadReceipt{}, fmt.Errorf("store: mark read: %w", ErrNotAMember)
	}
	if err != nil {
		return ReadReceipt{}, fmt.Errorf("store: mark read: %w", err)
	}

	if err := tx.QueryRow(ctx, `
		UPDATE conversation_members cm
		   SET read_seq = LEAST(GREATEST(cm.read_seq, $3), c.last_seq)
		  FROM conversations c
		 WHERE c.id = cm.conversation_id
		   AND cm.conversation_id = $1 AND cm.user_id = $2
		RETURNING cm.read_seq`, conv, user, upToSeq).Scan(&after); err != nil {
		return ReadReceipt{}, fmt.Errorf("store: mark read: %w", err)
	}
	// CANT-89 RULING 1 — the member's own devices need this page back.
	//
	// ONLY WHEN THE MARK MOVED. `Advanced` already exists to keep a duplicate
	// receipt from waking every device in the room to tell it nothing changed,
	// and the same reasoning applies with more force to a counter draw: a
	// no-op receipt would enter the deployment-wide serialised section to
	// record that nothing happened.
	//
	// The member row is already locked, from the FOR UPDATE above, so this
	// takes conversation_members before log_counter — the order messages.go
	// states outward, counter last. The send path never touches this table, so
	// the only lock the two paths share is the counter and both take it last:
	// no cycle exists in either direction.
	if after > before {
		if _, err := newMetadataBump().member(conv, user).apply(ctx, tx); err != nil {
			return ReadReceipt{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ReadReceipt{}, fmt.Errorf("store: mark read: commit: %w", err)
	}

	return ReadReceipt{
		ConversationID: conv,
		UserID:         user,
		UpToSeq:        after,
		Advanced:       after > before,
	}, nil
}

// FirstUnreadSeq is the derivation for ONE conversation, for callers that are
// not serving a whole sync page. nil means nothing is unread.
//
// Sync computes the same thing inline over its own join — the same const, so
// there is one derivation and not two that agree today.
func (s *Store) FirstUnreadSeq(ctx context.Context, conv, viewer uuid.UUID) (*int64, error) {
	var seq *int64
	err := s.pool.QueryRow(ctx, `
		SELECT `+firstUnreadSeqExpr+`
		  FROM conversation_members cm
		 WHERE cm.conversation_id = $1 AND cm.user_id = $2`,
		conv, viewer).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, sendErrorFor(fmt.Errorf("store: first unread: %w", ErrNotAMember))
	}
	if err != nil {
		return nil, sendErrorFor(fmt.Errorf("store: first unread: %w", err))
	}
	return seq, nil
}

// loadReadBy fills in read_by for the page's messages.
//
// ONE ROUND TRIP for the page, not one aggregate: readByExpr is a correlated
// subquery and Postgres evaluates it once per row of the outer scan, over
// conversation_members' own index. Said precisely because the comments in this
// file are load-bearing and somebody will reason from this one — "one
// aggregate" would have invited a rewrite to a GROUP BY join on the strength of
// a claim that was never true.
//
// Every message on the page gets a row, because the author always counts, so
// nothing here is ever absent. The mapper still treats an absent key as zero,
// which is what makes a hand-built page in a test behave.
func (s *Store) loadReadBy(ctx context.Context, tx pgx.Tx, page *SyncPage) error {
	page.ReadBy = map[uuid.UUID]int64{}
	if len(page.Messages) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(page.Messages))
	for _, m := range page.Messages {
		ids = append(ids, m.ID)
	}
	rows, err := tx.Query(ctx, `
		SELECT m.id, `+readByExpr+`
		  FROM messages m
		 WHERE m.id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("store: sync: read_by: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return fmt.Errorf("store: sync: scan read_by: %w", err)
		}
		page.ReadBy[id] = n
	}
	return rows.Err()
}
