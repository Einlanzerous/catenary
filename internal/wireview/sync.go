package wireview

// CANT-20 — the SyncResponse a reader gets, built from one store page.
//
// Same rule as message.go: rows and a viewer in, wire types out, no queries.
// Every Message on the page goes through Message(), which is what keeps the
// socket's Message and REST's indistinguishable.

import (
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// SyncViewer is the reader, plus the read state needed to say what they have
// seen. It is separate from Viewer because a page's per-message delivery state
// is derived here rather than supplied per message.
type SyncViewer struct {
	UserID uuid.UUID

	// ReadSeq is the reader's read_seq per conversation, from the same page.
	ReadSeq map[uuid.UUID]int64

	MediaURL func(storageKey string) string
}

// Sync assembles the response. `messages` is ascending by log_seq because the
// store returned it that way and the wire calls that ordering normative — a
// client applies the page as a stream and may stop anywhere without leaving a
// hole behind its cursor, which is only true if the order is the log's.
func Sync(page store.SyncPage, v SyncViewer, serverTime string) wire.SyncResponse {
	out := wire.SyncResponse{
		LogSeq:     wire.LogSeq(page.HighWater),
		HasMore:    page.HasMore,
		ServerTime: wire.Timestamp(serverTime),
		// Non-nil so an empty page encodes as [] rather than null. The three
		// arrays are required on the wire, and `null` is not an empty array to
		// a decoder that validates — which all three now do.
		Messages:      []wire.Message{},
		Conversations: []wire.Conversation{},
		Users:         []wire.User{},
	}

	for _, m := range page.Messages {
		var src *store.ReplySource
		if m.ReplyTo != nil {
			if s, ok := page.ReplySources[*m.ReplyTo]; ok {
				// Scoped to this conversation, matching what the send path
				// stores: a source elsewhere is no ref at all.
				if s.MessageID != uuid.Nil && sameConversation(page, m, s) {
					src = &s
				}
			}
		}
		out.Messages = append(out.Messages, Message(m, page.Attachments[m.ID], src, Viewer{
			UserID:   v.UserID,
			State:    deliveryState(m, v),
			MediaURL: v.MediaURL,
			// ReadBy is CANT-26's: it counts how many OTHER members have read
			// the message, which needs a query over every member's read_seq.
			// Optional on the wire, so omitting it is honest rather than a
			// placeholder that would be wrong.
		}))
	}

	for _, c := range page.Conversations {
		out.Conversations = append(out.Conversations, conversation(c))
	}
	for _, u := range page.Users {
		out.Users = append(out.Users, user(u))
	}
	return out
}

// sameConversation keeps a reply ref inside its own thread. The store already
// scopes the lookup, so this is the second half of a belt-and-braces pair
// rather than the only check.
func sameConversation(page store.SyncPage, m store.MessageRow, src store.ReplySource) bool {
	for _, other := range page.Messages {
		if other.ID == src.MessageID {
			return other.ConversationID == m.ConversationID
		}
	}
	// The source is not on this page, so its conversation is not knowable from
	// here. The store's query scoped it; trusting that is the alternative to a
	// second round trip per reply.
	return true
}

// deliveryState is what THIS reader sees, derived from their read_seq.
//
// `read` once the reader's own read_seq has passed the message, `delivered`
// otherwise — they are receiving it in this very response, which is what
// delivered means. `sent` is the state the author's own send returns and is not
// reachable here.
//
// CANT-26 owns read state proper. This is the minimum honest thing /sync can
// say using only the column that already exists, and it is stated here rather
// than left as an unexplained constant.
func deliveryState(m store.MessageRow, v SyncViewer) wire.DeliveryState {
	if seq, ok := v.ReadSeq[m.ConversationID]; ok && m.Seq <= seq {
		return wire.DeliveryStateRead
	}
	return wire.DeliveryStateDelivered
}

func conversation(c store.ConversationRow) wire.Conversation {
	out := wire.Conversation{
		ID:          wire.Uuid(c.ID.String()),
		Kind:        wire.ConversationKind(c.Kind),
		MemberCount: c.MemberCount,
		// head_seq is SERVED from conversations.last_seq and is not a second
		// column: with the row lock the bump and the insert commit together, so
		// the two would be equal at every instant — one number stored twice.
		HeadSeq: wire.Seq(c.LastSeq),
		Muted:   &c.Muted,
	}

	// `name` is REQUIRED on the wire and NULLABLE in the database, and that is
	// not a mismatch — it is 0002 declining to store something that is per
	// reader. A direct shows the OTHER member, which cannot live in one column,
	// so the store resolves it for this reader and it arrives here already
	// chosen.
	switch {
	case c.Name != nil:
		out.Name = *c.Name
	case c.OtherMemberName != nil:
		out.Name = *c.OtherMemberName
	}

	if c.FirstUnreadSeq != nil {
		s := wire.Seq(*c.FirstUnreadSeq)
		out.FirstUnreadSeq = &s
	}
	if c.RetentionDays != nil {
		d := int64(*c.RetentionDays)
		out.RetentionDays = &d
	}
	return out
}

func user(u store.UserRow) wire.User {
	init := initials(u.DisplayName)
	return wire.User{
		ID:       wire.Uuid(u.ID.String()),
		Name:     u.DisplayName,
		Initials: &init,
	}
}

// initials is the avatar tile's two letters, DERIVED server-side so web and
// Flutter cannot disagree — the same reason peaks are computed once.
//
// The rule is fixed by the sync_response vector, which is the only place it is
// written down: "Nadia Ruiz" is NR and "Theo" is TH. So two or more words give
// the first letter of the first two; one word gives its first two letters.
// Uppercased, and letters only, so punctuation in a display name cannot reach a
// tile that has room for exactly two glyphs.
func initials(name string) string {
	fields := strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	switch {
	case len(fields) == 0:
		return ""
	case len(fields) == 1:
		return strings.ToUpper(firstN(fields[0], 2))
	default:
		return strings.ToUpper(firstN(fields[0], 1) + firstN(fields[1], 1))
	}
}

func firstN(s string, n int) string {
	out := []rune(s)
	if len(out) > n {
		out = out[:n]
	}
	return string(out)
}
