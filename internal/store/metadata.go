package store

// CANT-89/91 — the change marker, and the ONE place it is drawn.
//
// `/sync` serves a conversation or a user whose marker is above the caller's
// cursor, which is the half of SyncResponse.conversations' and .users' promise
// that loadConversations and loadUsers used to concede they did not keep.
//
// ONE ATOMIC CHANGE DRAWS ONE MARKER, AND LOCKS EVERY ROW IT TOUCHES BEFORE IT
// DRAWS. That shape is not tidiness; it is the only shape that keeps the
// counter last, and the first version of this file got it wrong.
//
// A per-row `lock → draw → write` helper is correct for ONE row and unsafe for
// two. After the first call the transaction holds `log_counter`, so the second
// call asks for its target's row lock WHILE HOLDING THE COUNTER — the counter is
// no longer at the bottom of the order. Against a concurrent SendMessage, which
// takes `conversations(X)` at position 8 and draws at position 10, that is a
// cycle: the bump holds the counter and wants `conversations(X)`, the send holds
// `conversations(X)` and wants the counter. Postgres resolves it by aborting
// somebody's send. A membership change is exactly this case — it writes
// `conversation_members` AND `conversations` — so it is the shape the first
// caller of this file will have.
//
// So a caller names every row first and applies once:
//
//	_, err := newMetadataBump().conversation(conv).member(conv, joiner).apply(ctx, tx)
//
// THE LOCK ORDER WITHIN A BUMP IS conversations → conversation_members → users
// → log_counter, and ascending id within each table. Within a table the id
// order matters for bump-against-bump: two membership changes touching {A, B}
// in opposite orders would deadlock on `conversations` alone, before the
// counter is ever consulted.
//
// EVERY LOCK HERE IS `FOR NO KEY UPDATE`, AND THAT IS THE SECOND HALF OF THE
// SAFETY ARGUMENT — the first version used FOR UPDATE and was wrong about
// `users` in a way that is worth spelling out, because the ordering argument
// above does NOT cover it.
//
// `conversations` is genuinely ordered: SendMessage takes it at position 8,
// before the counter at position 10, so a bump taking it first agrees. `users`
// is the inversion. SendMessage never locks a user row until position 11, when
// the insert takes KEY SHARE via `messages.author_id → users(id)` — AFTER the
// counter. So a rename bump holding `users(U)` FOR UPDATE and waiting on the
// counter, against a send holding the counter and waiting for KEY SHARE on
// `users(U)`, is a cycle, and there is no order this file could choose that
// removes it: the counter is genuinely before users for one writer and after
// for the other.
//
// FOR NO KEY UPDATE closes it instead of reordering around it. It does not
// conflict with KEY SHARE, so the FK's lock passes straight through; it still
// conflicts with itself, so two bumps of one row still serialise; and it is
// exactly the lock the marker write takes anyway, `metadata_log_seq` being a
// non-key column — so FOR UPDATE was strictly stronger than anything here
// needs. messages.go's own note relies on the same property for
// `users.deactivated_at`, and that note names this file now.
//
// `conversation_members` IS LOCKED THE SAME WAY FOR A WEAKER REASON, AND THE
// DIFFERENCE IS WORTH KEEPING STRAIGHT. Nothing in `migrations/` references
// that table, so no foreign key ever takes KEY SHARE on a member row and
// FOR UPDATE there would have been equally safe — markRead still uses it. NKU
// is chosen for consistency within this function, so that "every lock here is
// the weakest one that orders" is a rule with no exception to remember, and
// so that it stays true if something later does reference the table.
//
// (The KEY SHARE a membership insert takes is on `users(joiner)`, not on the
// member row. That is an argument for the `users` lock above, and an earlier
// draft of this comment filed it here.)
//
// AND NO DRAWN VALUE CROSSES A COMMIT BOUNDARY. That is Invariant 1 for this
// column: a marker drawn in one transaction and written in another is a
// bigserial by a different route, and what it loses is a rename that commits
// after a page has already advanced past its number. metadata_order_test.go's
// Arm 1 convicts that shape.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type memberKey struct{ conv, user uuid.UUID }

// metadataBump is one atomic metadata change: the rows it touches, and the one
// marker value all of them get.
//
// One value for one change is also the more truthful record. Two rows changed
// together by a membership write are not two events, and giving them two
// markers would say they were.
type metadataBump struct {
	convs   map[uuid.UUID]bool
	members map[memberKey]bool
	users   map[uuid.UUID]bool
}

func newMetadataBump() *metadataBump {
	return &metadataBump{
		convs:   map[uuid.UUID]bool{},
		members: map[memberKey]bool{},
		users:   map[uuid.UUID]bool{},
	}
}

// conversation marks a conversation's SHARED metadata as changed: name, kind,
// membership, retention.
func (b *metadataBump) conversation(id uuid.UUID) *metadataBump {
	b.convs[id] = true
	return b
}

// member marks ONE member's per-member state as changed: first_unread_seq and
// muted. Per member rather than per conversation, which is ruling 1's whole
// argument — a receipt changes what one person's devices should see, and a
// conversation-level marker would wake all seven for it.
func (b *metadataBump) member(conv, user uuid.UUID) *metadataBump {
	b.members[memberKey{conv, user}] = true
	return b
}

// user marks a display name as changed. Whether it REACHES a given reader is
// ruling 3's reachability guard, which lives in loadUsers' query — it is a fact
// about the reader, and this type does not have one.
func (b *metadataBump) user(id uuid.UUID) *metadataBump {
	b.users[id] = true
	return b
}

func sortedIDs(set map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i][:], out[j][:]) < 0 })
	return out
}

func sortedMembers(set map[memberKey]bool) []memberKey {
	out := make([]memberKey, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if c := bytes.Compare(out[i].conv[:], out[j].conv[:]); c != 0 {
			return c < 0
		}
		return bytes.Compare(out[i].user[:], out[j].user[:]) < 0
	})
	return out
}

// apply locks every named row, draws once, and writes the marker to all of
// them, on the CALLER'S transaction. It returns the drawn value.
//
// Locking one row per statement rather than `WHERE id = ANY(...) FOR UPDATE`
// is deliberate: Postgres does not promise the order in which a multi-row lock
// takes its rows, and the order is the property this function exists to have.
func (b *metadataBump) apply(ctx context.Context, tx pgx.Tx) (int64, error) {
	convs, members, users := sortedIDs(b.convs), sortedMembers(b.members), sortedIDs(b.users)
	if len(convs)+len(members)+len(users) == 0 {
		return 0, fmt.Errorf("store: metadata bump names no rows")
	}

	// 1 — every target row, in the documented order. All of it before the draw.
	for _, id := range convs {
		var got uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&got)
		if err == pgx.ErrNoRows {
			return 0, fmt.Errorf("store: metadata bump: no conversation %s: %w", id, ErrNotAMember)
		}
		if err != nil {
			return 0, fmt.Errorf("store: metadata bump: lock conversation: %w", err)
		}
	}
	for _, k := range members {
		var got uuid.UUID
		err := tx.QueryRow(ctx, `SELECT user_id FROM conversation_members
			 WHERE conversation_id = $1 AND user_id = $2 FOR NO KEY UPDATE`, k.conv, k.user).Scan(&got)
		if err == pgx.ErrNoRows {
			return 0, fmt.Errorf("store: metadata bump: not a member: %w", ErrNotAMember)
		}
		if err != nil {
			return 0, fmt.Errorf("store: metadata bump: lock member: %w", err)
		}
	}
	for _, id := range users {
		var got uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&got)
		if err == pgx.ErrNoRows {
			return 0, fmt.Errorf("store: metadata bump: no user %s", id)
		}
		if err != nil {
			return 0, fmt.Errorf("store: metadata bump: lock user: %w", err)
		}
	}

	// 2 — THE COUNTER, LAST, and exactly once for the whole change.
	var v int64
	if err := tx.QueryRow(ctx,
		`UPDATE log_counter SET value = value + 1 WHERE id = 1 RETURNING value`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: draw metadata_log_seq: %w", err)
	}

	// 3 — the writes. Every row this change touched already locked above, so
	// nothing here can block on a row the transaction has not got.
	if len(convs) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE conversations SET metadata_log_seq = $2 WHERE id = ANY($1)`, convs, v); err != nil {
			return 0, fmt.Errorf("store: metadata bump: write conversations: %w", err)
		}
	}
	for _, k := range members {
		if _, err := tx.Exec(ctx,
			`UPDATE conversation_members SET metadata_log_seq = $3
			  WHERE conversation_id = $1 AND user_id = $2`, k.conv, k.user, v); err != nil {
			return 0, fmt.Errorf("store: metadata bump: write member: %w", err)
		}
	}
	if len(users) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE users SET metadata_log_seq = $2 WHERE id = ANY($1)`, users, v); err != nil {
			return 0, fmt.Errorf("store: metadata bump: write users: %w", err)
		}
	}
	return v, nil
}

// CANT-75 — find-or-create the direct conversation between two people.
//
// D4: "create" is a conversations row PLUS TWO conversation_members rows,
// atomically, and "find" is the lookup of the pair that already exists. Both
// live here, in the one file the guard above lets write INSERT INTO
// conversations and INSERT INTO conversation_members, because creating one IS
// a metadata change — a row left at the DEFAULT metadata_log_seq is invisible
// to every cursor there is (see TestOnlyOneFileMovesAMetadataMarker's own
// case for exactly this statement).
//
// directKey (0002) is the two member ids, sorted so either side of the pair
// computes the identical string, then joined. `conversations_direct_key_idx`
// is a PARTIAL UNIQUE index over it, so two callers racing to find-or-create
// the SAME pair collide on the database's own constraint rather than on
// anything this file has to coordinate — the loser's INSERT reports zero rows
// affected rather than an error (ON CONFLICT DO NOTHING is the house idiom
// TestConcurrentFindOrCreateDirectMakesOneConversation already proves safe
// under ten concurrent callers), and it falls through to read the winner's
// row back inside the SAME transaction, never a second one.
//
// directKey is unexported and lives here rather than as a test helper,
// because it is now something production code computes and not only
// something a test asserts about the database's own uniqueness.
func directKey(a, b uuid.UUID) string {
	x, y := a.String(), b.String()
	if x > y {
		x, y = y, x
	}
	return x + "|" + y
}

// FindOrCreateDirect finds or creates the direct conversation between viewer
// and the user targetHandle names, and returns it exactly as viewer would see
// it on /sync.
//
// THREE REFUSALS, ONE CODE. targetHandle resolving to nobody, resolving to a
// deactivated account, and resolving to viewer's own handle are three
// different causes and senderror.go gives all three conversation_not_found —
// the direct conversation this request names does not exist and this call
// will never make one, for three different reasons a caller cannot retry
// past. RedeemEnrollment already collapses "unknown token" and "deactivated
// account" into one answer for the identical reason: among a small trusted
// group with authenticated senders, a finer distinction than "no such
// conversation" is not one this surface owes.
//
// A SELF-DIRECT IS REFUSED RATHER THAN BUILT AS A ONE-MEMBER "DIRECT". D4's
// "two conversation_members rows" is not a detail to relax when the two ids
// happen to be equal — conversation_members' primary key is (conversation_id,
// user_id), so a self-pair could hold at most one row, and every reader
// downstream of this table (member_count, the OtherMemberName join /sync
// serves a direct's name from) assumes two. Refusing here is cheaper than
// auditing every one of them for a member_count of 1 they were never
// designed to see.
func (s *Store) FindOrCreateDirect(ctx context.Context, viewer uuid.UUID, targetHandle string) (ConversationRow, error) {
	c, err := s.findOrCreateDirect(ctx, viewer, targetHandle)
	if err == nil {
		return c, nil
	}
	// ONE EXIT, ONE LOG LINE — the same shape SendMessage and MarkRead use.
	// The handle is logged: unlike a message body, D1's honesty argument does
	// not cover it, and it is the one piece of information that makes this
	// line useful to an operator diagnosing a bot's misbehaviour.
	se := sendErrorFor(err)
	s.logger.Log(ctx, se.Level(), "find-or-create direct refused", append([]any{
		"viewer_id", viewer, "target_handle", targetHandle,
	}, se.LogAttrs()...)...)
	return ConversationRow{}, se
}

func (s *Store) findOrCreateDirect(ctx context.Context, viewer uuid.UUID, targetHandle string) (ConversationRow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConversationRow{}, fmt.Errorf("store: find-or-create direct: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolved first, and not locked — the same argument RedeemEnrollment
	// makes for reading users.deactivated_at unlocked: the race that leaves
	// open (a deactivation committing a moment later) is closed at
	// Authenticate, on the target's own next request, not here.
	var target uuid.UUID
	var deactivated *time.Time
	err = tx.QueryRow(ctx, `SELECT id, deactivated_at FROM users WHERE handle = $1`, targetHandle).
		Scan(&target, &deactivated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ConversationRow{}, ErrTargetNotFound
	case err != nil:
		return ConversationRow{}, fmt.Errorf("store: find-or-create direct: resolve handle: %w", err)
	}
	if deactivated != nil {
		return ConversationRow{}, ErrTargetDeactivated
	}
	if target == viewer {
		return ConversationRow{}, ErrSelfDirect
	}

	key := directKey(viewer, target)

	// THE HOUSE FIND-OR-CREATE IDIOM: attempt the insert, ON CONFLICT DO
	// NOTHING rather than a targeted arbiter, because DO NOTHING with no
	// target suppresses ANY unique violation on the table without having to
	// restate the partial index's predicate. A row comes back only when this
	// call created it.
	id := uuid.New()
	created := false
	err = tx.QueryRow(ctx, `
		INSERT INTO conversations (id, kind, direct_key) VALUES ($1, 'direct', $2)
		ON CONFLICT DO NOTHING
		RETURNING id`, id, key).Scan(&id)
	switch {
	case err == nil:
		created = true
	case errors.Is(err, pgx.ErrNoRows):
		// Somebody else's row won the race. It is visible to this statement
		// under READ COMMITTED the instant it commits — ON CONFLICT DO
		// NOTHING waits on the conflicting row's inserter rather than racing
		// past it — so this read cannot miss it.
		//
		// AND kind = 'direct' (CANT-75's review): conversations_direct_key_idx
		// is PARTIAL — ON conversations (direct_key) WHERE kind = 'direct' —
		// and a bare `WHERE direct_key = $1` does not imply that predicate, so
		// the planner cannot prove the partial index applies and falls back
		// to a sequential scan. Restating the predicate here is what lets this
		// read use the very index the find-or-create idiom leans on for
		// uniqueness.
		if err := tx.QueryRow(ctx,
			`SELECT id FROM conversations WHERE direct_key = $1 AND kind = 'direct'`, key).Scan(&id); err != nil {
			return ConversationRow{}, fmt.Errorf("store: find-or-create direct: find existing: %w", err)
		}
	default:
		return ConversationRow{}, fmt.Errorf("store: find-or-create direct: insert: %w", err)
	}

	if created {
		if _, err := tx.Exec(ctx, `
			INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`,
			id, viewer, target); err != nil {
			return ConversationRow{}, fmt.Errorf("store: find-or-create direct: insert members: %w", err)
		}
	}

	// Read back on tx, so a caller that just created the row sees it without
	// waiting for the commit below — the same reason conversationRowOne runs
	// on the caller's own transaction rather than the pool.
	//
	// BEFORE THE BUMP, DELIBERATELY (CANT-75's review). messages.go's own
	// lock-order note puts the counter draw LAST so the deployment-wide
	// serialised section is draw-insert-commit rather than the whole
	// transaction; a read sitting between the bump and Commit widens that
	// section by however long the read takes, and every send anywhere in the
	// deployment queues behind it. Nothing conversationRowOne selects — id,
	// kind, name, last_seq, retention_days, muted, read_seq, member_count,
	// first_unread_seq, other_member_name — is written by the bump below, so
	// moving the read above it costs nothing the caller can observe and
	// restores draw-then-commit.
	out, err := s.conversationRowOne(ctx, tx, id, viewer)
	if err != nil {
		return ConversationRow{}, err
	}

	if created {
		// ONE BUMP FOR ALL THREE ROWS THIS CREATE TOUCHED — the conversation
		// and both fresh member rows — on the same argument the type's own
		// doc gives: a membership change is one event, not three, and only
		// one draw should say so. Without the conversation's marker moving,
		// the row sits at DEFAULT 0 and is invisible to every cursor there
		// is; that is what makes this call, and not only the insert above,
		// load-bearing. LAST, right before Commit, which is what keeps the
		// counter at the bottom of this transaction's own order.
		if _, err := newMetadataBump().conversation(id).member(id, viewer).member(id, target).
			apply(ctx, tx); err != nil {
			return ConversationRow{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ConversationRow{}, fmt.Errorf("store: find-or-create direct: commit: %w", err)
	}
	return out, nil
}
