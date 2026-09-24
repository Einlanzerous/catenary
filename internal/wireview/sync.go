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
	// with no row here contributes nothing, and the map's zero value for an
	// absent entry is "nothing read" — which is the safe direction: a message
	// shows as delivered rather than as read on a claim the page cannot
	// support. The mark is passed to DeliveryState as that value, so the
	// absent case is expressed HERE, by the caller that knows it is absent.
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
		// message whose author is an ACTIVE member is at least 1 before
		// anyone has done anything. Zero needs the author out of the
		// COUNTED population, and since CANT-135 there are two ways in:
		// they left conversation_members, or they were DEACTIVATED.
		//
		// AN OFFBOARD IS NOW ONE OF THEM, and this comment used to say the
		// opposite. CANT-33 ruling 7 still keeps membership — DeactivateUser
		// writes nothing to that table — but the count no longer runs over
		// rows alone: readstate.go's activeMemberExpr joins `users` and drops
		// anyone whose `deactivated_at` is set, from BOTH halves of the
		// fraction together. So the cost ruling 7 accepted is the cost
		// CANT-135 removed, and `read_by` and `N MEMBERS` stopped counting
		// somebody who can no longer read anything.
		//
		// It is also the value DeliveryState reads below, which is what keeps
		// the word and the number from being two answers to one question.
		readBy := page.ReadBy[m.ID]
		out.Messages = append(out.Messages, Message(m, page.Attachments[m.ID], src, Viewer{
			UserID:   v.UserID,
			State:    DeliveryState(m, v.UserID, readSeq[m.ConversationID], readBy),
			ReadBy:   &readBy,
			MediaURL: v.MediaURL,
		}))
	}

	for _, c := range page.Conversations {
		out.Conversations = append(out.Conversations, Conversation(c))
	}
	for _, u := range page.Users {
		out.Users = append(out.Users, User(u))
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

// DeliveryState is what THIS reader sees, and the SUBJECT of the answer
// changes with authorship. CANT-90 settled that; the wire schema's
// DeliveryState description states the same rule for client authors.
//
// EXPORTED FOR THE HUB (CANT-107), AND FOR NOTHING ELSE. Sync serves a page
// and the hub serves a live frame, and both hand the word `state` to
// wireview.Message; if each derived it, the `readBy > 1` rule below would
// exist twice and the two transports would answer the same row differently
// the day one copy moved. The same argument the CANT-84 guard makes for
// Message, one function down. viewerReadSeq is the reader's own mark in this
// conversation — 0 when they have read nothing, which is also what a caller
// passes when it does not know.
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
// the word. TWO THINGS MOVE BOTH, and only one of them can happen.
//
// A DEPARTURE would: leaving is a row DELETE from conversation_members — there
// is no left_at, the key is (conversation_id, user_id) — and readByExpr counts
// rows in that table, so the numerator drops with the denominator. NOTHING IN
// THE SERVICE REMOVES A MEMBERSHIP ROW, so that has never been reachable, and
// this paragraph described it as the cause for want of another one.
//
// A DEACTIVATION DOES, AND SINCE CANT-137 IT IS THE REACHABLE CAUSE.
// activeMemberExpr drops a deactivated member from readByExpr, so a room where
// exactly one other member had read your message sits at 2 and serves `read`;
// that member is deprovisioned, the count is 1, and the next page serves `sent`.
// THAT IS THIS LADDER GOING BACKWARDS, and it is the first thing in the service
// that can lower the count at all — MarkRead's LEAST/GREATEST makes every
// receipt monotone, and nothing deletes a member row.
//
// AND IT IS RE-EMITTED FOR, THE WAY A RECEIPT IS (CANT-140 ruling 1, built as
// CANT-143). An offboard and its reversal raise the receipt's own notify over
// `(0, read_seq]` per room, and the hub re-emits each affected message to its
// live author through this same function with the count recomputed — so a
// connected author's `read` drops to `sent` when the one other reader is
// deprovisioned, and comes back on the reversal. What is NOT closed is the
// receipt's own offline window: a client detached for the event holds the
// old rung until it bootstraps, and here the old rung is one the server no
// longer backs. DeliveryState's schema description says all of this — the
// ladder is not monotone on your own message, the latest serve wins, the
// three events that re-emit, the window a bootstrap closes — and it is the
// rule both clients render by. This comment exists so the next reader does
// not conclude from the paragraph above that only a departure can lower the
// count, and does not conclude from this one that nothing can be stale.
//
// FOR SOMEONE ELSE'S MESSAGE, `state` describes THIS READER: `read` once their
// read_seq has passed it, `delivered` otherwise — they are receiving it in this
// very response, which is what delivered means.
//
// readBy 0 is unreachable here for an own message: store.Sync joins
// conversation_members on the viewer, so wherever that branch runs the viewer
// is a member and their own +1 is in the count. The comparison is total over it
// anyway, and answers `sent`.
func DeliveryState(m store.MessageRow, viewer uuid.UUID, viewerReadSeq int64, readBy int64) wire.DeliveryState {
	if m.AuthorID == viewer {
		if readBy > 1 {
			return wire.DeliveryStateRead
		}
		return wire.DeliveryStateSent
	}
	if m.Seq <= viewerReadSeq {
		return wire.DeliveryStateRead
	}
	return wire.DeliveryStateDelivered
}

// Conversation maps one stored row to the wire type, for one reader. EXPORTED
// (CANT-75) so a REST handler returning "the same Conversation either way"
// from POST /conversations/direct builds it here rather than a second time —
// there is no CANT-84-style guard over wire.Conversation the way there is over
// wire.Message, but the reason to have one function is the same reason: two
// assemblers of the same row are two places they can start answering
// differently for it.
func Conversation(c store.ConversationRow) wire.Conversation {
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

// User maps one stored user row to the wire type. EXPORTED (CANT-114) on the
// same argument Conversation is: the hub's `user` frame introduces the same
// record /sync serves, and two assemblers of one row are two places they can
// start answering differently — here, over `initials`, which is derived rather
// than stored precisely so web and Flutter cannot disagree about it.
func User(u store.UserRow) wire.User {
	out := wire.User{
		ID:   wire.Uuid(u.ID.String()),
		Name: u.DisplayName,
	}
	// ABSENT rather than empty. User.initials is minLength: 1, and
	// users.display_name is TEXT NOT NULL with no CHECK — so a name of "!!!" or
	// a lone emoji is storable and derives to nothing. CANT-106 closed the gap
	// where no generated decoder enforced a property-level minLength, so
	// `"initials":""` would decode fine as an empty avatar tile rather than
	// being refused; the mapper still sends absent rather than empty because
	// that is what the schema actually allows, not because refusal was optional.
	if init := initials(u.DisplayName); init != "" {
		out.Initials = &init
	}

	// TRUE OR ABSENT, NEVER FALSE (CANT-135 ruling 2, and `User.deactivated`'s
	// own schema description). The generated field is a `*bool` with
	// `omitempty`, so nil is what omits it — a pointer to `false` would encode
	// `"deactivated": false`, which is a thing said rather than a thing not
	// said, and it would change the bytes of every `User` on every page for
	// every account that is perfectly fine. This branch is the whole mechanism,
	// which is why it is here and not in the generator: absent-unless-true is
	// the wire's rule for THIS field, not a property of optional booleans.
	if u.Deactivated {
		t := true
		out.Deactivated = &t
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
