package store

// CANT-257 — FindOrCreateSelf (CANT-254 ruling 0, B): a person's conversation
// with only themselves. Modeled on direct_test.go's find-or-create tests.

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestFindOrCreateSelfTwiceReturnsTheSameConversation(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")

	first, err := st.FindOrCreateSelf(ctx, ada)
	if err != nil {
		t.Fatalf("first find-or-create: %v", err)
	}
	if first.Kind != "self" {
		t.Errorf("kind = %q, want self", first.Kind)
	}
	if first.Name == nil || *first.Name != "Notes" {
		t.Errorf("name = %v, want Notes", first.Name)
	}
	if first.MemberCount != 1 {
		t.Errorf("member_count = %d, want 1", first.MemberCount)
	}
	if first.OtherMemberID != nil || first.OtherMemberName != nil {
		t.Errorf("a self conversation has no other member, got id=%v name=%v", first.OtherMemberID, first.OtherMemberName)
	}
	if first.LastSeq != 0 {
		t.Errorf("head_seq = %d, want 0 — nothing has been sent into it", first.LastSeq)
	}

	second, err := st.FindOrCreateSelf(ctx, ada)
	if err != nil {
		t.Fatalf("second find-or-create: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second call returned %s, want %s", second.ID, first.ID)
	}
	var convs, members int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE kind = 'self'`), &convs)
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversation_members WHERE conversation_id = $1`, first.ID), &members)
	if convs != 1 || members != 1 {
		t.Errorf("%d self conversations and %d member rows, want 1 and 1", convs, members)
	}
}

func TestConcurrentFindOrCreateSelfMakesOneConversation(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")

	const n = 10
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			c, err := st.FindOrCreateSelf(ctx, ada)
			ids[i] = c.ID
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Errorf("caller %d found %s, want %s", i, ids[i], ids[0])
		}
	}
	var convs, members int
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE kind = 'self'`), &convs)
	mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM conversation_members`), &members)
	if convs != 1 {
		t.Errorf("%d concurrent calls made %d self conversations, want 1", n, convs)
	}
	if members != 1 {
		t.Errorf("%d conversation_members rows, want 1", members)
	}
}

func TestEachPersonHasTheirOwnSelfConversationAndOthersCannotSeeIt(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	ada := mkUser(ctx, t, pool, "ada")
	theo := mkUser(ctx, t, pool, "theo")

	a, err := st.FindOrCreateSelf(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.FindOrCreateSelf(ctx, theo)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two people share one self conversation")
	}
	page, err := st.Sync(ctx, theo, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Conversations {
		if c.ID == a.ID {
			t.Error("theo is served ada's self conversation")
		}
	}
}

// Discoverable on the FIRST /sync page, and what is sent into it reaches a
// second device through /sync with dense seq and increasing log_seq. The
// receipt is the server's own: read_by is 1 (the author counts), and the
// wire derives `sent` from that (wireview.DeliveryState).
func TestASelfConversationIsOnSyncAndCarriesMessagesToASecondDevice(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger()).WithUploadResolver(&fakeResolver{})
	ada := mkUser(ctx, t, pool, "ada")
	laptop := mkDevice(ctx, t, pool, ada, "laptop")

	c, err := st.FindOrCreateSelf(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}

	// Device 2 has never heard of the conversation: its first page carries it.
	first, err := st.Sync(ctx, ada, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cr := range first.Conversations {
		if cr.ID == c.ID {
			found = true
			if cr.Kind != "self" || cr.MemberCount != 1 {
				t.Errorf("served kind=%q member_count=%d, want self and 1", cr.Kind, cr.MemberCount)
			}
		}
	}
	if !found {
		t.Fatal("the self conversation is not on the owner's first /sync page")
	}

	// Device 1 writes a text and a voice note into it.
	text, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: c.ID, AuthorID: ada, SenderDeviceID: &laptop,
		Text: ptr("buy oat milk"),
	})
	if err != nil {
		t.Fatalf("send text: %v", err)
	}
	voice, err := st.SendMessage(ctx, NewMessage{
		ClientID: uuid.New(), ConversationID: c.ID, AuthorID: ada, SenderDeviceID: &laptop,
		Attachments: attachmentsFor("voice"),
	})
	if err != nil {
		t.Fatalf("send voice: %v", err)
	}

	// Device 2 catches up from its cursor.
	next, err := st.Sync(ctx, ada, first.HighWater, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Messages) != 2 {
		t.Fatalf("second device received %d messages, want 2", len(next.Messages))
	}
	if next.Messages[0].ID != text.ID || next.Messages[1].ID != voice.ID {
		t.Errorf("messages out of order or wrong: %v", next.Messages)
	}
	if next.Messages[0].Seq != 1 || next.Messages[1].Seq != 2 {
		t.Errorf("seq = %d, %d, want dense 1, 2", next.Messages[0].Seq, next.Messages[1].Seq)
	}
	if next.Messages[1].LogSeq <= next.Messages[0].LogSeq {
		t.Errorf("log_seq not increasing: %d then %d", next.Messages[0].LogSeq, next.Messages[1].LogSeq)
	}
	for _, m := range next.Messages {
		if got := next.ReadBy[m.ID]; got != 1 {
			t.Errorf("read_by = %d for a self message, want 1 (the author counts)", got)
		}
	}
	if len(next.Attachments[voice.ID]) != 1 {
		t.Errorf("the voice note's attachment did not arrive: %v", next.Attachments[voice.ID])
	}
}

// Only a person has a Notes, the same rule CreateGroup applies to its creator.
func TestABotCannotHaveASelfConversation(t *testing.T) {
	ctx, pool := freshDB(t)
	st := New(pool, DefaultLimits(), discardLogger())
	bot := mkUser(ctx, t, pool, "robot")
	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'bot' WHERE id = $1`, bot); err != nil {
		t.Fatal(err)
	}
	counter := head(ctx, t, pool)
	if _, err := st.FindOrCreateSelf(ctx, bot); !errors.Is(err, ErrBotCannotCreateSelf) {
		t.Fatalf("err = %v, want ErrBotCannotCreateSelf", err)
	}
	if n := countTableRows(ctx, t, pool, "conversations"); n != 0 {
		t.Errorf("%d conversations, want none", n)
	}
	if n := countTableRows(ctx, t, pool, "conversation_members"); n != 0 {
		t.Errorf("%d member rows, want none", n)
	}
	if got := head(ctx, t, pool); got != counter {
		t.Errorf("log_counter %d -> %d; a refusal drew a marker", counter, got)
	}
}
