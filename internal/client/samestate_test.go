package client

// CANT-46 — SameState, the convergence rig's pairwise comparison. Each kind of
// difference is planted and watched being named; the three serve-time fields
// are planted and watched being ignored; and every other field of the three
// generated wire structs is walked by reflection, so a field the schema gains
// later is held to "compared" without this file being edited.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/magos/catenary/internal/wire"
)

func seqPtr(n int64) *wire.Seq { return &n }

// aDevice is one person's device after a settle: a group room, a direct
// conversation, three messages and two users.
func aDevice() Snapshot {
	other := wire.Uuid("u2")
	return Snapshot{
		Cursor: 9, HasCursor: true,
		Messages: []wire.Message{
			{ID: "m1", ConversationID: "A", Seq: 1, LogSeq: 1, AuthorID: "u2", State: wire.DeliveryStateDelivered},
			{ID: "m2", ConversationID: "A", Seq: 2, LogSeq: 4, AuthorID: "u2", State: wire.DeliveryStateDelivered},
			{ID: "m3", ConversationID: "D", Seq: 1, LogSeq: 6, AuthorID: "u2", State: wire.DeliveryStateDelivered},
		},
		Conversations: []wire.Conversation{
			{ID: "A", Kind: wire.ConversationKindGroup, Name: "the room", MemberCount: 2, HeadSeq: 2, FirstUnreadSeq: seqPtr(2)},
			{ID: "D", Kind: wire.ConversationKindDirect, Name: "Ada", OtherMemberID: &other, MemberCount: 2, HeadSeq: 1, FirstUnreadSeq: seqPtr(1)},
		},
		Users: []wire.User{{ID: "u1", Name: "Theo"}, {ID: "u2", Name: "Ada"}},
	}
}

// clone copies the three record slices, so a test can change one side.
func clone(s Snapshot) Snapshot {
	s.Messages = append([]wire.Message(nil), s.Messages...)
	s.Conversations = append([]wire.Conversation(nil), s.Conversations...)
	s.Users = append([]wire.User(nil), s.Users...)
	return s
}

func TestSameStateIsCleanForIdenticalSnapshotsAndIgnoresTheCursor(t *testing.T) {
	x := aDevice()
	if d := SameState(x, x); len(d) != 0 {
		t.Fatalf("a snapshot differs from itself: %v", d)
	}
	y := clone(x)
	y.Cursor, y.Wipes, y.Counted = 4, 1, []wire.Uuid{"m1"}
	if d := SameState(x, y); len(d) != 0 {
		t.Errorf("two snapshots differing only in cursor, wipes and counted: %v, want clean — a cursor is a position, not state", d)
	}
	if d := SameView(x, y); len(d) == 0 {
		t.Error("SameView no longer reports a cursor difference")
	}
}

func TestSameStateNamesEveryDifference(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(s *Snapshot)
		want   string
	}{
		{"a message held by one side only", func(s *Snapshot) { s.Messages = s.Messages[:2] }, "message m3 only in the first"},
		{"a differing seq", func(s *Snapshot) { s.Messages[1].Seq = 3 }, "message m2 differs in Seq"},
		{"a differing log_seq", func(s *Snapshot) { s.Messages[1].LogSeq = 5 }, "message m2 differs in LogSeq"},
		{"a differing first_unread_seq", func(s *Snapshot) { s.Conversations[0].FirstUnreadSeq = seqPtr(1) }, "conversation A differs in FirstUnreadSeq"},
		{"first_unread_seq absent on one side", func(s *Snapshot) { s.Conversations[0].FirstUnreadSeq = nil }, "conversation A differs in FirstUnreadSeq"},
		{"a differing head_seq", func(s *Snapshot) { s.Conversations[0].HeadSeq = 1 }, "conversation A differs in HeadSeq"},
		{"a conversation held by one side only", func(s *Snapshot) { s.Conversations = s.Conversations[:1] }, "conversation D only in the first"},
		{"a group's name", func(s *Snapshot) { s.Conversations[0].Name = "renamed" }, "conversation A differs in Name"},
		{"two fields of one record", func(s *Snapshot) { s.Conversations[0].HeadSeq, s.Conversations[0].MemberCount = 1, 3 }, "conversation A differs in MemberCount, HeadSeq"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, y := aDevice(), clone(aDevice())
			tc.change(&y)
			d := SameState(x, y)
			if len(d) != 1 || d[0] != tc.want {
				t.Fatalf("SameState = %q, want exactly %q", d, tc.want)
			}
			// The same pair the other way round names the other side.
			back := SameState(y, x)
			if len(back) != 1 || back[0] != strings.Replace(tc.want, "only in the first", "only in the second", 1) {
				t.Errorf("reversed, SameState = %q", back)
			}
		})
	}
}

// RULING 2 → OPTION 0: the three fields the schema marks as serve-time are
// not compared, and nothing else is excused.
func TestSameStateIgnoresOnlyTheServeTimeFields(t *testing.T) {
	readBy := int64(1)
	for _, tc := range []struct {
		name   string
		change func(s *Snapshot)
	}{
		{"Message.state", func(s *Snapshot) { s.Messages[0].State = wire.DeliveryStateRead }},
		{"Message.read_by", func(s *Snapshot) { s.Messages[0].ReadBy = &readBy }},
		{"the name of a direct conversation", func(s *Snapshot) { s.Conversations[1].Name = "Ada L." }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, y := aDevice(), clone(aDevice())
			tc.change(&y)
			if d := SameState(x, y); len(d) != 0 {
				t.Errorf("SameState = %q, want clean: the schema marks this field as of the serve that carried it", d)
			}
			if d := SameView(x, y); len(d) == 0 {
				t.Error("SameView no longer reports it; the strict comparison must")
			}
		})
	}
}

// perturb changes a value to one reflect.DeepEqual tells apart from it.
func perturb(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "+")
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
			return
		}
		fresh := reflect.New(v.Type().Elem())
		fresh.Elem().Set(v.Elem())
		perturb(t, fresh.Elem())
		v.Set(fresh)
	case reflect.Slice:
		v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
	case reflect.Struct:
		perturb(t, v.Field(0))
	default:
		t.Fatalf("perturb: a %s field; teach this helper the kind", v.Kind())
	}
}

// EVERY OTHER FIELD IS COMPARED, found by reflection rather than listed: a
// field added to the generated structs is in this walk the day it is
// generated, and SameState's DeepEqual compares it by default.
func TestSameStateComparesEveryOtherWireField(t *testing.T) {
	excused := map[string]bool{"Message.State": true, "Message.ReadBy": true}
	walk := func(kind string, typ reflect.Type, record func(s *Snapshot) reflect.Value) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := kind + "." + f.Name
			if !f.IsExported() || excused[name] || f.Name == "ID" {
				// ID is the key the walk matches records by: changing it is "held
				// by one side only", which TestSameStateNamesEveryDifference holds.
				continue
			}
			t.Run(name, func(t *testing.T) {
				x, y := aDevice(), clone(aDevice())
				perturb(t, record(&y).Field(i))
				d := SameState(x, y)
				if len(d) != 1 || !strings.HasSuffix(d[0], " differs in "+f.Name) {
					t.Errorf("SameState = %q after changing %s on one side, want one difference naming %s", d, name, f.Name)
				}
			})
		}
	}
	// The GROUP conversation: its name is compared. The direct one's is the
	// third excused field, held by TestSameStateIgnoresOnlyTheServeTimeFields.
	walk("Message", reflect.TypeFor[wire.Message](), func(s *Snapshot) reflect.Value { return reflect.ValueOf(&s.Messages[0]).Elem() })
	walk("Conversation", reflect.TypeFor[wire.Conversation](), func(s *Snapshot) reflect.Value { return reflect.ValueOf(&s.Conversations[0]).Elem() })
	walk("User", reflect.TypeFor[wire.User](), func(s *Snapshot) reflect.Value { return reflect.ValueOf(&s.Users[0]).Elem() })
}
