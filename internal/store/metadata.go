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
// → log_counter, and ascending id within each table. Across tables it agrees
// with messages.go, which takes `conversations` before anything else and the
// counter last. Within a table it matters for bump-against-bump: two membership
// changes touching {A, B} in opposite orders would deadlock on `conversations`
// alone, before the counter is ever consulted.
//
// AND NO DRAWN VALUE CROSSES A COMMIT BOUNDARY. That is Invariant 1 for this
// column: a marker drawn in one transaction and written in another is a
// bigserial by a different route, and what it loses is a rename that commits
// after a page has already advanced past its number. metadata_order_test.go's
// Arm 1 convicts that shape.

import (
	"bytes"
	"context"
	"fmt"
	"sort"

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
		err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id = $1 FOR UPDATE`, id).Scan(&got)
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
			 WHERE conversation_id = $1 AND user_id = $2 FOR UPDATE`, k.conv, k.user).Scan(&got)
		if err == pgx.ErrNoRows {
			return 0, fmt.Errorf("store: metadata bump: not a member: %w", ErrNotAMember)
		}
		if err != nil {
			return 0, fmt.Errorf("store: metadata bump: lock member: %w", err)
		}
	}
	for _, id := range users {
		var got uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&got)
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
