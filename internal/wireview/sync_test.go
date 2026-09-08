package wireview

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// The strongest form of CANT-20's "the captured response still validates":
// round-trip OUR OWN output through the decoder, which since CANT-25 enforces
// every constraint the schema states rather than merely parsing.
//
// A response that encodes but would be refused by our own clients is the exact
// failure the three-runner conformance suite exists to prevent, reached from
// the producing side.
func TestAnAssembledSyncResponseValidatesAgainstTheDecoder(t *testing.T) {
	author := mustUUID(t, nadiaID)
	reader := mustUUID(t, theoID)
	conv := mustUUID(t, convID)
	at := mustTime(t, "2026-08-17T04:22:03.117Z")

	page := store.SyncPage{
		Messages: []store.MessageRow{{
			ID:             mustUUID(t, "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"),
			ConversationID: conv, AuthorID: author, Seq: 1905, LogSeq: 41251, At: at,
			Text: ptr("the wire is up"),
		}},
		Attachments:  map[uuid.UUID][]store.AttachmentRow{},
		ReplySources: map[uuid.UUID]store.ReplySource{},
		Conversations: []store.ConversationRow{{
			ID: conv, Kind: "group", Name: ptr("Sunday Dinner"),
			LastSeq: 1908, MemberCount: 7, ReadSeq: 1905, FirstUnreadSeq: ptr(int64(1906)),
		}},
		Users: []store.UserRow{
			{ID: author, DisplayName: "Nadia Ruiz"},
			{ID: reader, DisplayName: "Theo"},
		},
		HighWater: 41254,
		HasMore:   true,
	}

	got := Sync(page, SyncViewer{UserID: reader, MediaURL: noMedia},
		"2026-08-17T04:32:00.000Z")

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := wire.DecodeNamed("SyncResponse", raw); err != nil {
		t.Fatalf("our own SyncResponse does not validate: %v\n%s", err, raw)
	}
}

// The three arrays are REQUIRED on the wire, and `null` is not an empty array
// to a decoder that validates — which all three now do. An empty page is the
// commonest response a caught-up client gets, so it is the one most likely to
// be shipped broken.
func TestAnEmptyPageEncodesAsEmptyArraysAndStillValidates(t *testing.T) {
	got := Sync(store.SyncPage{HighWater: 7}, SyncViewer{
		UserID: mustUUID(t, theoID), MediaURL: noMedia,
	}, "2026-08-17T04:32:00.000Z")

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, field := range []string{`"messages":[]`, `"conversations":[]`, `"users":[]`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("empty page is missing %s: %s", field, raw)
		}
	}
	if _, err := wire.DecodeNamed("SyncResponse", raw); err != nil {
		t.Fatalf("an empty page does not validate: %v\n%s", err, raw)
	}
}

// `initials` is server-derived so web and Flutter cannot disagree — the same
// reason peaks are computed once. The rule is fixed by the sync_response
// vector, which is the only place it is written down.
func TestInitialsFollowTheVectorsRule(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"Nadia Ruiz", "NR"}, // the vector
		{"Theo", "TH"},       // the vector: one word gives two letters
		{"ada lovelace king", "AL"},
		{"cher", "CH"},
		{"J", "J"},
		// "" IS THE DERIVATION'S ANSWER, NOT THE WIRE'S. initials() reports what
		// it found; whether an empty result may be sent is user()'s decision,
		// and TestAnEmptyDerivationLeavesInitialsAbsent is where that is fixed.
		// Reading these two rows as "the wire allows empty initials" is the
		// mistake — User.initials is minLength: 1.
		{"", ""},
		{"  ", ""},
		{"!!!", ""},               // storable: display_name is TEXT NOT NULL with no CHECK
		{"Jean-Luc Picard", "JL"}, // punctuation is a separator, not a glyph
		{"陳 大文", "陳大"},
	} {
		if got := initials(tc.name); got != tc.want {
			t.Errorf("initials(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A direct conversation has no stored name — 0002 declines to store one because
// the rail shows the OTHER member, which is per reader. The wire requires
// `name`, so the store resolves it and the mapper uses it.
func TestADirectConversationIsNamedForTheOtherMember(t *testing.T) {
	got := conversation(store.ConversationRow{
		ID: mustUUID(t, convID), Kind: "direct", Name: nil,
		OtherMemberName: ptr("Nadia Ruiz"), LastSeq: 3, MemberCount: 2,
	})
	if got.Name != "Nadia Ruiz" {
		t.Errorf("name = %q, want the other member's — a direct has no stored name", got.Name)
	}
}

// THE SUBJECT OF `state` CHANGES WITH AUTHORSHIP, which is CANT-90's ruling and
// the thing this table exists to hold still.
//
// On a message the reader WROTE it describes everyone else — `sent` until
// another member's receipt has passed it, `read` after, never `delivered` —
// and the threshold is readBy > 1 because readByExpr counts the author by
// identity, so an own message is at 1 before anyone has done anything. On
// SOMEONE ELSE'S message it describes the reader: `read` once their own
// read_seq has passed it, `delivered` otherwise.
//
// Two derivations this must keep out, both of which have shipped:
//
//   - The reader's own read_seq deciding their own message. It answers "have I
//     read what I wrote", and in a conversation where the reader is the only
//     sender it reported every message they had ever written as read on every
//     reconnect (CANT-20). Row 2 is that case: seq 1905 <= read_seq 1905, and
//     the answer is still `sent` because nobody else has read it.
//   - readBy >= 1 as the threshold. True for every message that can exist, so
//     it is the same failure from the other side. Row 1 is that case.
func TestOwnMessageStateIsWhatOTHERSHaveRead(t *testing.T) {
	reader := mustUUID(t, theoID)
	other := mustUUID(t, nadiaID)
	conv := mustUUID(t, convID)
	readSeq := map[uuid.UUID]int64{conv: 1905}

	// memberCount stands in for a seven-member room: the largest readBy a real
	// page could carry, and the value the canvas draws as READ 7/7.
	const memberCount = 7

	for _, tc := range []struct {
		name   string
		author uuid.UUID
		seq    int64
		readBy int64
		want   wire.DeliveryState
	}{
		// Mine. Only readBy moves the answer; seq and read_seq do not.
		{"mine, only I have it", reader, 1906, 1, wire.DeliveryStateSent},
		{"mine, behind my own read_seq, still only I have it", reader, 1905, 1, wire.DeliveryStateSent},
		{"mine, one other member has read it", reader, 1906, 2, wire.DeliveryStateRead},
		{"mine, the whole room has read it", reader, 1906, memberCount, wire.DeliveryStateRead},
		// UNREACHABLE FROM Sync, kept because the function should be total.
		// store.Sync joins conversation_members on the viewer, so wherever this
		// branch runs the viewer is a member and their own +1 is in the count;
		// 0 needs the author gone from the room, and then it is not their page.
		// It is here so nobody reads `0` as a real own-message state.
		{"mine, author no longer a member — not reachable from Sync", reader, 1906, 0, wire.DeliveryStateSent},
		// Theirs. readBy does not enter this branch at all: these carry a
		// count high enough to be `read` if it did.
		{"theirs, behind my read_seq", other, 1905, memberCount, wire.DeliveryStateRead},
		{"theirs, ahead of my read_seq", other, 1906, memberCount, wire.DeliveryStateDelivered},
	} {
		got := deliveryState(store.MessageRow{
			ConversationID: conv, AuthorID: tc.author, Seq: tc.seq,
		}, reader, readSeq, tc.readBy)
		if got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// `delivered` IS NEVER THE ANSWER FOR YOUR OWN MESSAGE, asserted over the whole
// range rather than at the two points the table above happens to sample.
//
// D1 declined delivery receipts, so there is nothing stored between "written"
// and "somebody read it" — a `delivered` here would be the one claim Invariant
// 3 forbids. Separate from the table because the table proves the values it
// lists and this proves the absence of one across every count a page can carry.
func TestYourOwnMessageIsNeverDelivered(t *testing.T) {
	reader := mustUUID(t, theoID)
	conv := mustUUID(t, convID)
	readSeq := map[uuid.UUID]int64{conv: 1905}

	for readBy := int64(0); readBy <= 32; readBy++ {
		for _, seq := range []int64{1904, 1905, 1906} {
			got := deliveryState(store.MessageRow{
				ConversationID: conv, AuthorID: reader, Seq: seq,
			}, reader, readSeq, readBy)
			if got == wire.DeliveryStateDelivered {
				t.Fatalf("readBy %d, seq %d: state = %q on my own message; "+
					"the server stores nothing between written and read", readBy, seq, got)
			}
		}
	}
}

// A name that derives to nothing produces NO `initials` key rather than an
// empty one. User.initials is minLength: 1, and neither generated decoder
// enforces minLength today — so `"initials":""` would have shipped as a blank
// avatar tile in both clients instead of being refused by either.
//
// The `omitempty` on a *string means absent is reachable at all; this asserts
// the mapper actually takes it.
func TestAnEmptyDerivationLeavesInitialsAbsent(t *testing.T) {
	got := user(store.UserRow{ID: mustUUID(t, nadiaID), DisplayName: "!!!"})
	if got.Initials != nil {
		t.Fatalf("initials = %q, want absent — minLength: 1 has no empty member", *got.Initials)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(raw), `"initials"`) {
		t.Errorf("an empty derivation still emitted the key: %s", raw)
	}
	if _, err := wire.DecodeNamed("User", raw); err != nil {
		t.Fatalf("a user with no derivable initials does not validate: %v\n%s", err, raw)
	}

	// And the ordinary case still carries it, so the guard above cannot pass by
	// dropping initials for everyone.
	with := user(store.UserRow{ID: mustUUID(t, theoID), DisplayName: "Nadia Ruiz"})
	if with.Initials == nil || *with.Initials != "NR" {
		t.Errorf("initials = %v, want NR", with.Initials)
	}
}

// A REPLY WHOSE SOURCE IS IN ANOTHER CONVERSATION CARRIES NO REF — INCLUDING
// WHEN THE SOURCE IS NOT ON THE PAGE.
//
// The off-page case is the one that was broken and the one that is normal: a
// reply arrives on this page, the message it answers is older than the cursor,
// so it is not among page.Messages. The old check searched the page for the
// source and returned true when it was absent — "the store scoped it" — but the
// store's scope is a membership guard over the page's conversations, and a
// reader in two of them has both. So the default carried every off-page source,
// which is nearly all of them.
//
// ReplySource now carries conversation_id and the check compares it, so being
// off the page changes nothing.
func TestACrossConversationReplyRefIsDroppedEvenWhenTheSourceIsOffThePage(t *testing.T) {
	reader := mustUUID(t, theoID)
	author := mustUUID(t, nadiaID)
	here := mustUUID(t, convID)
	elsewhere := mustUUID(t, "3a1b2c3d-4e5f-4a6b-8c7d-8e9f0a1b2c3d")
	srcID := mustUUID(t, "9f8e7d6c-5b4a-4392-8180-7f6e5d4c3b2a")
	at := mustTime(t, "2026-08-17T04:22:03.117Z")

	page := func(srcConv uuid.UUID) store.SyncPage {
		return store.SyncPage{
			// One message only: the source is deliberately NOT on the page.
			Messages: []store.MessageRow{{
				ID:             mustUUID(t, "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"),
				ConversationID: here, AuthorID: author, Seq: 2, LogSeq: 9, At: at,
				Text: ptr("a reply"), ReplyTo: &srcID,
			}},
			Attachments: map[uuid.UUID][]store.AttachmentRow{},
			ReplySources: map[uuid.UUID]store.ReplySource{srcID: {
				MessageID: srcID, ConversationID: srcConv, AuthorID: author,
				Text: ptr("the source"),
			}},
			Conversations: []store.ConversationRow{{
				ID: here, Kind: "group", Name: ptr("Sunday Dinner"),
				LastSeq: 2, MemberCount: 7,
			}},
			Users:     []store.UserRow{{ID: author, DisplayName: "Nadia Ruiz"}},
			HighWater: 9,
		}
	}
	v := SyncViewer{UserID: reader, MediaURL: noMedia}

	got := Sync(page(elsewhere), v, "2026-08-17T04:32:00.000Z")
	if ref := got.Messages[0].ReplyTo; ref != nil {
		t.Errorf("an off-page source in another conversation was served as a ref: %+v", ref)
	}

	// Same source, same off-page position, its own conversation: the ref stands.
	// Without this the test would pass by dropping every reply ref.
	got = Sync(page(here), v, "2026-08-17T04:32:00.000Z")
	ref := got.Messages[0].ReplyTo
	if ref == nil {
		t.Fatalf("an in-conversation source off the page lost its ref")
	}
	if ref.Preview != "the source" {
		t.Errorf("preview = %q, want the source's text", ref.Preview)
	}
}

// read_by IS SERVED FOR EVERY MESSAGE, ZERO INCLUDED — and it is `0` rather
// than absent, because a sync page always knows the answer. CANT-26's
// aggregate ran over the whole page, so "nobody else has read it" is a fact the
// server holds; omitting the field would say "not known", which is a different
// and untrue thing.
//
// The second half is the one a shared variable would break: each message needs
// its own pointer, or every message ends up reporting the last count computed.
func TestReadByIsServedPerMessageIncludingZero(t *testing.T) {
	reader := mustUUID(t, theoID)
	author := mustUUID(t, nadiaID)
	conv := mustUUID(t, convID)
	at := mustTime(t, "2026-08-17T04:22:03.117Z")
	seen := mustUUID(t, "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f")
	unseen := mustUUID(t, "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d")

	got := Sync(store.SyncPage{
		Messages: []store.MessageRow{
			{ID: seen, ConversationID: conv, AuthorID: author, Seq: 1, LogSeq: 1, At: at, Text: ptr("read by five")},
			{ID: unseen, ConversationID: conv, AuthorID: author, Seq: 2, LogSeq: 2, At: at, Text: ptr("read by nobody")},
		},
		ReadBy: map[uuid.UUID]int64{seen: 5},
		Conversations: []store.ConversationRow{{
			ID: conv, Kind: "group", Name: ptr("Sunday Dinner"), LastSeq: 2, MemberCount: 7,
		}},
		Users:     []store.UserRow{{ID: author, DisplayName: "Nadia Ruiz"}},
		HighWater: 2,
	}, SyncViewer{UserID: reader, MediaURL: noMedia}, "2026-08-17T04:32:00.000Z")

	for i, want := range []int64{5, 0} {
		rb := got.Messages[i].ReadBy
		if rb == nil {
			t.Errorf("message %d has no read_by; a page always knows the count", i)
			continue
		}
		if *rb != want {
			t.Errorf("message %d read_by = %d, want %d", i, *rb, want)
		}
	}
	// Distinct pointers, not one variable shared across the loop.
	if got.Messages[0].ReadBy == got.Messages[1].ReadBy {
		t.Error("both messages point at the same read_by")
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := wire.DecodeNamed("SyncResponse", raw); err != nil {
		t.Fatalf("a page carrying read_by does not validate: %v\n%s", err, raw)
	}
}

// The read state that drives `read` vs `delivered` comes off the PAGE now, not
// off a map the caller built. This is the seam CANT-20's review flagged as
// redundant, deleted rather than covered: a caller can no longer supply a
// read_seq the page does not support, which is how the lossy inversion lived at
// the composition root.
func TestReadStateComesOffThePagesOwnConversationRow(t *testing.T) {
	reader := mustUUID(t, theoID)
	author := mustUUID(t, nadiaID)
	conv := mustUUID(t, convID)
	at := mustTime(t, "2026-08-17T04:22:03.117Z")

	page := func(readSeq int64) store.SyncPage {
		return store.SyncPage{
			Messages: []store.MessageRow{
				{ID: mustUUID(t, "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"), ConversationID: conv,
					AuthorID: author, Seq: 1, LogSeq: 1, At: at, Text: ptr("one")},
				{ID: mustUUID(t, "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"), ConversationID: conv,
					AuthorID: author, Seq: 2, LogSeq: 2, At: at, Text: ptr("two")},
			},
			Conversations: []store.ConversationRow{{
				ID: conv, Kind: "group", Name: ptr("Sunday Dinner"),
				LastSeq: 2, MemberCount: 7, ReadSeq: readSeq,
			}},
			Users:     []store.UserRow{{ID: author, DisplayName: "Nadia Ruiz"}},
			HighWater: 2,
		}
	}
	v := SyncViewer{UserID: reader, MediaURL: noMedia}

	got := Sync(page(1), v, "2026-08-17T04:32:00.000Z")
	if got.Messages[0].State != wire.DeliveryStateRead {
		t.Errorf("seq 1 with read_seq 1 = %q, want read", got.Messages[0].State)
	}
	if got.Messages[1].State != wire.DeliveryStateDelivered {
		t.Errorf("seq 2 with read_seq 1 = %q, want delivered", got.Messages[1].State)
	}

	// And the row is what moves it — nothing else can.
	got = Sync(page(2), v, "2026-08-17T04:32:00.000Z")
	if got.Messages[1].State != wire.DeliveryStateRead {
		t.Errorf("seq 2 with read_seq 2 = %q, want read", got.Messages[1].State)
	}

	// A conversation absent from the page contributes nothing, and the safe
	// direction is `delivered`: a message is never claimed read on a page that
	// cannot support the claim.
	orphan := page(2)
	orphan.Conversations = nil
	got = Sync(orphan, v, "2026-08-17T04:32:00.000Z")
	if got.Messages[0].State != wire.DeliveryStateDelivered {
		t.Errorf("with no conversation row, state = %q, want delivered", got.Messages[0].State)
	}
}
