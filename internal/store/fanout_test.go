package store

// CANT-107 — the read behind a `message` frame, and the two small reads
// beside it.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Everything a per-viewer Message needs, in one call: the row, attachments in
// position order, the read_by count with the author counted, and every
// member with their mark.
func TestMessageForFanoutCarriesEverythingAViewerNeeds(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", ada, theo)

	sent := send(ctx, t, st, conv, ada, "with two pictures")
	// Planted OUT of position order, so the ORDER BY is what the test sees.
	for _, pos := range []int{1, 0} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO attachments (id, message_id, kind, storage_key, position, filename, width, height, bytes)
			VALUES ($1, $2, 'image', $3, $4, 'p.jpg', 10, 10, 100)`,
			uuid.New(), sent.ID, "k/"+string(rune('a'+pos)), pos); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	if _, err := st.MarkRead(ctx, conv, theo, 1); err != nil {
		t.Fatal(err)
	}

	f, err := st.MessageForFanout(ctx, conv, sent.Seq)
	if err != nil {
		t.Fatalf("MessageForFanout: %v", err)
	}
	if f.Message.ID != sent.ID || f.Message.LogSeq != sent.LogSeq || f.Message.AuthorID != ada {
		t.Errorf("message = %+v, want the row for %s", f.Message, sent.ID)
	}
	if len(f.Attachments) != 2 || f.Attachments[0].Position != 0 || f.Attachments[1].Position != 1 {
		t.Errorf("attachments = %+v, want two in position order", f.Attachments)
	}
	// The author counts by identity and Theo's receipt passed it: 2 of 2.
	if f.ReadBy != 2 {
		t.Errorf("read_by = %d, want 2 (author by identity + Theo's receipt)", f.ReadBy)
	}
	marks := map[uuid.UUID]int64{}
	for _, m := range f.Members {
		marks[m.UserID] = m.ReadSeq
	}
	if len(marks) != 2 || marks[ada] != 0 || marks[theo] != 1 {
		t.Errorf("members = %+v, want ada@0 and theo@1", f.Members)
	}
	if f.ReplySource != nil {
		t.Errorf("reply source = %+v on a message with no reply_to", f.ReplySource)
	}
}

// The reply source is the source AS IT IS NOW and only when it is in the same
// conversation. A reply_to planted across rooms — which the send path refuses
// and this read must not undo — yields no source.
func TestMessageForFanoutScopesTheReplySourceToTheConversation(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	room := mkGroup(ctx, t, pool, "room", ada)
	other := mkGroup(ctx, t, pool, "other", ada)

	source := send(ctx, t, st, room, ada, "the original")
	reply, err := st.SendMessage(ctx, NewMessage{
		ConversationID: room, AuthorID: ada, ClientID: uuid.New(), Text: ptr("the reply"), ReplyTo: &source.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.MessageForFanout(ctx, room, reply.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if f.ReplySource == nil || f.ReplySource.MessageID != source.ID || derefStr(f.ReplySource.Text) != "the original" {
		t.Errorf("reply source = %+v, want the original", f.ReplySource)
	}

	// Planted past the send path's refusal: a reply in `other` pointing at a
	// message in `room`.
	elsewhere := send(ctx, t, st, other, ada, "in the other room")
	if _, err := pool.Exec(ctx, `UPDATE messages SET reply_to = $1 WHERE id = $2`, source.ID, elsewhere.ID); err != nil {
		t.Fatal(err)
	}
	f, err = st.MessageForFanout(ctx, other, elsewhere.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if f.ReplySource != nil {
		t.Errorf("a cross-conversation reply_to yielded a source %+v; the scope is a membership guard", f.ReplySource)
	}
}

func TestMessageForFanoutReportsAMissingRowAsNotFound(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	conv := mkGroup(ctx, t, pool, "room", ada)

	_, err := st.MessageForFanout(ctx, conv, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown seq: err = %v, want ErrNotFound", err)
	}
	_, err = st.MessageForFanout(ctx, uuid.New(), 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown conversation: err = %v, want ErrNotFound", err)
	}
}

// Members answers for any conversation, member or not, and an unknown one is
// an empty list rather than an error: the hub asks on behalf of a caller who
// may be a non-member, and the store is not what tells them the room exists.
func TestMembersListsTheRoomAndAnUnknownRoomIsEmpty(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", ada, theo)

	got, err := st.Members(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	set := map[uuid.UUID]bool{}
	for _, id := range got {
		set[id] = true
	}
	if len(set) != 2 || !set[ada] || !set[theo] {
		t.Errorf("members = %v, want ada and theo", got)
	}
	got, err = st.Members(ctx, uuid.New())
	if err != nil || len(got) != 0 {
		t.Errorf("unknown room = %v, %v; want an empty list and no error", got, err)
	}
}

// Head moves with the counter and Hello reads the same number.
func TestHeadIsTheLogCounter(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	conv := mkGroup(ctx, t, pool, "room", ada)

	head, err := st.Head(ctx)
	if err != nil || head != 0 {
		t.Fatalf("head on an empty log = %d, %v; want 0", head, err)
	}
	sent := send(ctx, t, st, conv, ada, "one")
	head, err = st.Head(ctx)
	if err != nil || head != sent.LogSeq {
		t.Errorf("head after one send = %d, %v; want %d", head, err, sent.LogSeq)
	}
	res, err := st.Hello(ctx, HelloRequest{DeviceID: uuid.New(), SessionID: uuid.New()})
	if err != nil || res.Head != head {
		t.Errorf("hello head = %d, %v; want %d", res.Head, err, head)
	}
}

func TestIsTransientIsTheClassifier(t *testing.T) {
	if !IsTransient(context.Canceled) {
		t.Error("context.Canceled is classified transient by isTransient; the export must agree")
	}
	if IsTransient(ErrNotFound) {
		t.Error("ErrNotFound is not transient")
	}
}
