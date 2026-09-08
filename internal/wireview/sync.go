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

// SyncViewer is the reader. It is separate from Viewer because a page's
// per-message delivery state is derived here rather than supplied per message.
//
// IT NO LONGER CARRIES READ STATE, AND THAT IS THE POINT. It used to take a
// `ReadSeq map[uuid.UUID]int64` that the caller built, which meant the
// composition root reconstructed read_seq from first_unread_seq — a lossy
// inversion that reported every message a lone sender had ever written as
// read. CANT-20's review found that, and noted the map was redundant anyway:
// Sync already receives the page, and ConversationRow carries the column. So
// the seam is gone rather than covered, and there is no second place for the
// next transport to build the same map differently.
type SyncViewer struct {
	UserID   uuid.UUID
	MediaURL func(storageKey string) string
}

// Sync assembles the response. `messages` is ascending by log_seq because the
// store returned it that way and the wire calls that ordering normative — a
// client applies the page as a stream and may stop anywhere without leaving a
// hole behind its cursor, which is only true if the order is the log's.
// TimeLayout is the wire's timestamp format, exported so the composition root
// does not carry a second copy of a format the schema calls normative. The
// reasoning for the precision and the literal Z lives with it in message.go.
const TimeLayout = wireTimeLayout

func Sync(page store.SyncPage, v SyncViewer, serverTime string) wire.SyncResponse {
	// Read state comes off the page's own conversation rows. A conversation
	// with no row here contributes nothing, and deliveryState treats an absent
	// entry as "nothing read" — which is the safe direction: a message shows
	// as delivered rather than as read on a claim the page cannot support.
	readSeq := make(map[uuid.UUID]int64, len(page.Conversations))
	for _, c := range page.Conversations {
		readSeq[c.ID] = c.ReadSeq
	}

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
				if s.MessageID != uuid.Nil && sameConversation(m, s) {
					src = &s
				}
			}
		}
		// READ_BY IS SERVED FOR EVERY MESSAGE, ZERO INCLUDED. A sync page
		// always knows the answer — CANT-26's count ran over the whole page —
		// so a number here is always a fact, where omitting the field would
		// mean "not known" and is not one. It is taken by address per
		// iteration because the wire field is a pointer; a shared variable
		// would leave every message pointing at the last count computed.
		//
		// `0` DOES NOT MEAN "NOBODY ELSE HAS READ IT", which is what this
		// said until CANT-90. readByExpr counts the author by identity, so a
		// message whose author is still a member is at least 1 before anyone
		// has done anything. Zero needs the author GONE from
		// conversation_members — having left, or deleted by CANT-33's Purser
		// connector — and no current member's receipt to have passed it.
		//
		// It is also the value deliveryState reads below, which is what keeps
		// the word and the number from being two answers to one question.
		readBy := page.ReadBy[m.ID]
		out.Messages = append(out.Messages, Message(m, page.Attachments[m.ID], src, Viewer{
			UserID:   v.UserID,
			State:    deliveryState(m, v.UserID, readSeq, readBy),
			ReadBy:   &readBy,
			MediaURL: v.MediaURL,
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

// sameConversation keeps a reply ref inside its own thread, and it answers for
// EVERY source rather than only the ones that happen to be on the page.
//
// It used to look for the source among page.Messages and return true when it
// was not there — "the store scoped it, trust that". The store's scope is a
// MEMBERSHIP guard: it bounds sources to the conversations on this page, and a
// reader who is in two conversations has both, so a source in the other one
// passed. Any source older than the cursor was off the page entirely and took
// the default. So the case the check existed for was the case it could not see,
// and "belt and braces" described one belt.
//
// The store now returns the source's conversation_id, so this compares the
// thing itself. No page scan, no default, no second round trip.
func sameConversation(m store.MessageRow, src store.ReplySource) bool {
	return src.ConversationID == m.ConversationID
}

// deliveryState is what THIS reader sees, and the SUBJECT of the answer
// changes with authorship. CANT-90 settled that; the wire schema's
// DeliveryState description states the same rule for client authors.
//
// FOR A MESSAGE THE READER WROTE, `state` describes EVERYONE ELSE: `sent`
// until another member's receipt has passed it, `read` after, and never
// `delivered`. Two rungs, because that is how many the server can back — D1
// declined delivery receipts, so nothing is stored between "written" and
// "somebody has read it", and a third rung would be this service claiming
// something it cannot keep.
//
// THE THRESHOLD IS `readBy > 1` AND THE OFF-BY-ONE IS THE WHOLE POINT.
// readByExpr counts the author BY IDENTITY, so an own message sits at 1 from
// the instant the row exists. "At least one other member" is therefore
// readBy-1 >= 1, which is readBy > 1. `>= 1` holds for every message anyone
// has ever written, so it would report each of them read the moment it was
// sent — CANT-20's bug reached from the other side.
//
// It reads the COUNT, never the reader's own read_seq. That is what CANT-20
// removed and this does not put back: a reader's own mark answers "have I read
// what I wrote", which is not a question.
//
// It never reads member_count, so a JOIN moves the fraction without touching
// the word. A DEPARTURE moves both, and saying otherwise was wrong: leaving is
// a row DELETE from conversation_members — there is no left_at, the key is
// (conversation_id, user_id) — and readByExpr counts rows in that table, so the
// numerator drops with the denominator. A room where exactly one other member
// had read your message sits at 2 and serves `read`; that member leaves, the
// count is 1, and the next page serves `sent`. Message.read_by's own schema
// description says the same thing from the other side.
//
// FOR SOMEONE ELSE'S MESSAGE, `state` describes THIS READER: `read` once their
// read_seq has passed it, `delivered` otherwise — they are receiving it in this
// very response, which is what delivered means.
//
// readBy 0 is unreachable here for an own message: store.Sync joins
// conversation_members on the viewer, so wherever that branch runs the viewer
// is a member and their own +1 is in the count. The comparison is total over it
// anyway, and answers `sent`.
func deliveryState(m store.MessageRow, viewer uuid.UUID, readSeq map[uuid.UUID]int64, readBy int64) wire.DeliveryState {
	if m.AuthorID == viewer {
		if readBy > 1 {
			return wire.DeliveryStateRead
		}
		return wire.DeliveryStateSent
	}
	if seq, ok := readSeq[m.ConversationID]; ok && m.Seq <= seq {
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
	out := wire.User{
		ID:   wire.Uuid(u.ID.String()),
		Name: u.DisplayName,
	}
	// ABSENT rather than empty. User.initials is minLength: 1, and
	// users.display_name is TEXT NOT NULL with no CHECK — so a name of "!!!" or
	// a lone emoji is storable and derives to nothing. Neither the generated Go
	// decoder nor the TypeScript one enforces minLength, so `"initials":""`
	// would have shipped as an empty avatar tile rather than being refused.
	// Optional-and-absent is what the schema actually allows.
	if init := initials(u.DisplayName); init != "" {
		out.Initials = &init
	}
	return out
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
