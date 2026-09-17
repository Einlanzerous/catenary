package store

// CANT-107 — the read behind a `message` frame, and the two small reads
// beside it.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// --- CANT-114: the introduction ------------------------------------------------

// A conversation's FIRST message carries the conversation as EACH member sees
// it, plus every member's user record; a later message carries neither.
func TestMessageForFanoutIntroducesAConversationOnItsFirstMessage(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", ada, theo)

	first := send(ctx, t, st, conv, ada, "the first")
	if first.Seq != FirstMessageSeq {
		t.Fatalf("the first message in a fresh conversation is seq %d, want %d", first.Seq, FirstMessageSeq)
	}
	f, err := st.MessageForFanout(ctx, conv, first.Seq)
	if err != nil {
		t.Fatalf("MessageForFanout: %v", err)
	}
	adaRow, okA := f.Conversations[ada]
	theoRow, okT := f.Conversations[theo]
	if len(f.Conversations) != 2 || !okA || !okT {
		t.Fatalf("conversation rows = %+v, want one keyed by each member", f.Conversations)
	}
	if adaRow.ID != conv || adaRow.MemberCount != 2 || adaRow.LastSeq != first.Seq {
		t.Errorf("ada's row = %+v, want %s at seq %d with 2 members", adaRow, conv, first.Seq)
	}
	// PER VIEWER RATHER THAN ONE ROW FOR EVERYBODY: Ada wrote the only message,
	// so she has nothing unread; Theo has it waiting at seq 1. A statement that
	// answered for one viewer and handed the row to both would say the same
	// thing twice here.
	if adaRow.FirstUnreadSeq != nil {
		t.Errorf("ada's first_unread_seq = %d; she wrote the only message there is", *adaRow.FirstUnreadSeq)
	}
	if theoRow.FirstUnreadSeq == nil || *theoRow.FirstUnreadSeq != first.Seq {
		t.Errorf("theo's first_unread_seq = %v, want %d", theoRow.FirstUnreadSeq, first.Seq)
	}
	names := map[uuid.UUID]string{}
	for _, u := range f.Users {
		names[u.ID] = u.DisplayName
	}
	if len(names) != 2 || names[ada] != "ada" || names[theo] != "theo" {
		t.Errorf("users = %+v, want both members' records", f.Users)
	}

	second := send(ctx, t, st, conv, ada, "the second")
	f2, err := st.MessageForFanout(ctx, conv, second.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(f2.Conversations) != 0 || len(f2.Users) != 0 {
		t.Errorf("seq %d carried an introduction (%d row(s), %d user(s)); the gate is the ordinal",
			second.Seq, len(f2.Conversations), len(f2.Users))
	}
}

// CRITERION 12 (the plan's numbering; 13 in the ticket's `Done when`) — the
// batched statement correlates BOTH viewer binds.
//
// A direct conversation has NO STORED NAME (0002): each member sees it named
// for the OTHER one, which is per reader and cannot live in a column. A
// statement that bound one viewer for the other-member subquery would name the
// conversation for both members from that one side — so Theo would be
// introduced to a conversation named Theo. That is the failure this produces
// before the fix and not after.
func TestTheIntroductionNamesADirectFromEachMembersOwnSide(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")

	direct, err := st.FindOrCreateDirect(ctx, ada, "theo")
	if err != nil {
		t.Fatalf("find-or-create direct: %v", err)
	}
	first := send(ctx, t, st, direct.ID, ada, "hello")
	f, err := st.MessageForFanout(ctx, direct.ID, first.Seq)
	if err != nil {
		t.Fatal(err)
	}
	adaRow, theoRow := f.Conversations[ada], f.Conversations[theo]
	if got := derefStr(adaRow.OtherMemberName); got != "theo" {
		t.Errorf("ada's direct is named %q, want theo", got)
	}
	if got := derefStr(theoRow.OtherMemberName); got != "ada" {
		t.Errorf("theo's direct is named %q, want ada — the statement named it for both members from one side", got)
	}
	if adaRow.Kind != "direct" || adaRow.Name != nil {
		t.Errorf("ada's row = %+v, want a direct with no stored name", adaRow)
	}
}

// statementCounter counts every statement a pool issues, so criterion 14 is
// measured rather than reasoned about.
type statementCounter struct{ n atomic.Int64 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// CRITERION 14 (the plan's numbering; 15 in the ticket's `Done when`) — the
// introduction is two more statements inside the EXISTING
// transaction, and a message that is not a first pays for neither.
//
// One connection, so the count is this read's statements and not a second
// connection's setup, and a warm-up call before the measurements for the same
// reason.
func TestTheIntroductionCostsNothingOnAMessageThatIsNotAFirst(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")
	conv := mkGroup(ctx, t, pool, "room", ada, theo)
	first := send(ctx, t, st, conv, ada, "the first")
	second := send(ctx, t, st, conv, ada, "the second")

	counter := &statementCounter{}
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = counter
	counted, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer counted.Close()
	cst := New(counted, DefaultLimits(), discardLogger())

	measure := func(seq int64) int64 {
		t.Helper()
		counter.n.Store(0)
		if _, err := cst.MessageForFanout(ctx, conv, seq); err != nil {
			t.Fatalf("MessageForFanout(%d): %v", seq, err)
		}
		return counter.n.Load()
	}
	measure(second.Seq)
	withIntro, without := measure(first.Seq), measure(second.Seq)
	if withIntro-without != 2 {
		t.Errorf("a first message took %d statements and a later one %d; want exactly two more — the two introduction reads",
			withIntro, without)
	}
}
