package store

// CANT-92 — MarkRead's own receipt notify, and MessagesForReadNotify, the
// read behind the hub's live re-emission.

import (
	"testing"

	"github.com/google/uuid"
)

// THE NOTIFY IS GATED ON THE SAME Advanced AS THE METADATA BUMP. A receipt
// that moves the mark raises the widened NotifyPayload, in its receipt shape;
// one that does not is a duplicate or an out-of-order claim and raises
// nothing on this channel at all — MarkRead.Advanced's own contract, restated
// here for the notify rather than only for the caller-visible field.
func TestMarkReadNotifiesOnlyWhenTheMarkAdvances(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", ada, theo)
	send(ctx, t, st, conv, ada, "one")
	send(ctx, t, st, conv, ada, "two")

	got := listenForMessages(ctx, t, pool)

	r, err := st.MarkRead(ctx, conv, theo, 2)
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if !r.Advanced {
		t.Fatalf("the first mark did not report advanced: %+v", r)
	}
	p := next(t, got, "after the advancing mark")
	if !p.IsReceipt() || p.ConversationID != conv || p.UserID == nil || *p.UserID != theo || p.Before != 0 || p.After != 2 {
		t.Fatalf("payload = %+v, want a receipt{%s, theo, before 0, after 2}", p, conv)
	}

	// A repeat of the same claim: Advanced is false. Proved by ORDERING, on
	// CANT-21's own idiom — a fresh send behind it is the next thing the
	// listener receives, and it is a MESSAGE payload, not a stray receipt.
	r, err = st.MarkRead(ctx, conv, theo, 2)
	if err != nil {
		t.Fatalf("repeat mark read: %v", err)
	}
	if r.Advanced {
		t.Fatalf("the repeat mark reported advanced: %+v", r)
	}
	trailer := send(ctx, t, st, conv, ada, "three")
	p = next(t, got, "after the repeat mark")
	if p.IsReceipt() || p.ConversationID != conv || p.Seq != trailer.Seq {
		t.Fatalf("payload after the non-advancing mark = %+v, want the trailer's message payload {%s, %d} — "+
			"a non-advancing receipt notified", p, conv, trailer.Seq)
	}
}

// THE SPAN EXCLUDES THE READER'S OWN MESSAGES. readByExpr counts an author by
// identity regardless of read_seq (readstate.go), so a reader's own read_seq
// passing their own message changes nothing about it — there is nothing to
// re-emit, and MessagesForReadNotify leaves it out rather than sending a
// no-op frame that would also spend a slot in some author's cap on nothing.
//
// THE CAP IS PER AUTHOR: two other authors each get their own newest N,
// independently, ordered newest first — the messages a client is actually
// looking at, per CANT-92's decision.
func TestMessagesForReadNotifyExcludesTheReaderCapsPerAuthorNewestFirst(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	mallory := mkUser(ctx, t, pool, "mallory")
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", ada, mallory, theo)

	var adaSeqs, malSeqs []int64
	for i := 0; i < 5; i++ {
		adaSeqs = append(adaSeqs, send(ctx, t, st, conv, ada, "a").Seq)
		malSeqs = append(malSeqs, send(ctx, t, st, conv, mallory, "m").Seq)
	}
	// Theo's own message sits in the same span and must never come back:
	// Theo is the reader, and reading it changes nothing about it.
	last := send(ctx, t, st, conv, theo, "t").Seq

	if _, err := st.MarkRead(ctx, conv, theo, last); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	rows, err := st.MessagesForReadNotify(ctx, conv, theo, 0, last, 3)
	if err != nil {
		t.Fatalf("MessagesForReadNotify: %v", err)
	}

	byAuthor := map[uuid.UUID][]int64{}
	for _, r := range rows {
		if r.Message.AuthorID == theo {
			t.Fatalf("the reader's own message (seq %d) was returned", r.Message.Seq)
		}
		byAuthor[r.Message.AuthorID] = append(byAuthor[r.Message.AuthorID], r.Message.Seq)
		// Ada, Mallory and Theo are all members; Theo's own mark now covers
		// every message in the span, so each of Ada's and Mallory's messages
		// is read by its own author plus Theo: 2.
		if r.ReadBy != 2 {
			t.Errorf("message seq %d: read_by = %d, want 2 (author + theo)", r.Message.Seq, r.ReadBy)
		}
	}
	if len(rows) != 6 {
		t.Fatalf("%d rows, want 6 — 3 per author, 2 authors", len(rows))
	}
	wantAda := []int64{adaSeqs[4], adaSeqs[3], adaSeqs[2]}
	wantMal := []int64{malSeqs[4], malSeqs[3], malSeqs[2]}
	if !equalSeqs(byAuthor[ada], wantAda) {
		t.Errorf("ada's rows = %v, want the newest 3 descending %v", byAuthor[ada], wantAda)
	}
	if !equalSeqs(byAuthor[mallory], wantMal) {
		t.Errorf("mallory's rows = %v, want the newest 3 descending %v", byAuthor[mallory], wantMal)
	}
}

func equalSeqs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Attachments in position order and the reply source as it is now travel
// through the same loaders MessageForFanout and Sync use — this is a second
// caller of them, not a second copy of the SQL.
func TestMessagesForReadNotifyCarriesAttachmentsAndTheReplySource(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", ada, theo)

	source := send(ctx, t, st, conv, ada, "the original")
	reply, err := st.SendMessage(ctx, NewMessage{
		ConversationID: conv, AuthorID: ada, ClientID: uuid.New(), Text: ptr("the reply"), ReplyTo: &source.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO attachments (id, message_id, kind, storage_key, position, filename, width, height, bytes)
		VALUES ($1, $2, 'image', 'k/a', 0, 'p.jpg', 10, 10, 100)`, uuid.New(), reply.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}

	if _, err := st.MarkRead(ctx, conv, theo, reply.Seq); err != nil {
		t.Fatal(err)
	}
	rows, err := st.MessagesForReadNotify(ctx, conv, theo, 0, reply.Seq, 64)
	if err != nil {
		t.Fatal(err)
	}
	var got *ReadNotifyMessage
	for i := range rows {
		if rows[i].Message.ID == reply.ID {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("the reply (seq %d) was not in the span %+v", reply.Seq, rows)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].StorageKey != "k/a" {
		t.Errorf("attachments = %+v, want one at k/a", got.Attachments)
	}
	if got.ReplySource == nil || got.ReplySource.MessageID != source.ID || derefStr(got.ReplySource.Text) != "the original" {
		t.Errorf("reply source = %+v, want the original", got.ReplySource)
	}
	if got.ReadBy != 2 {
		t.Errorf("read_by = %d, want 2 (author + theo)", got.ReadBy)
	}
}
