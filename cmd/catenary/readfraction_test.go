package main

// CANT-144 — wire criterion 7 of CANT-140's plan: TWO SERVES, ONE CLIENT. The
// fraction a client renders is assembled from two records with two
// freshnesses — `read_by` from the message as last served, `member_count` from
// the conversation as last served — and this harness models the client's
// render rule over one viewer's real `/sync` pages before and after an
// offboard and its reversal, for every row of the plan's table.
//
// THE TABLE IS A FIXTURE, NOT A LITERAL HERE. server/spec/testdata/
// read-fraction.json is read by this file, by web/smoke.ts (CANT-145) and,
// when E5 builds it, by the Flutter renderer's own check: one set of rows,
// three readers, so the render rule cannot drift between clients — which is
// invariant 2 applied to a rule the vectors cannot carry (they are
// decode-shaped; a render rule is not).
//
// WHAT IS CONSTRUCTED, AND WHY FIVE ROOMS. A read mark is a high-water mark
// per member per room, so "the person had read M1 but not M2" and "five had
// read M3" cannot all hold in one room. Each row gets its own room of the same
// seven members and one message from the viewer; the offboard and the
// reversal hit all five at once, which is what an offboard does.
//
// TWO DEVICES OF THE VIEWER STAND IN FOR TWO CLIENT HISTORIES. Device A
// bootstraps before the offboard and catches up after it (rows 1–3); device
// B bootstraps during the window and catches up after the reversal (rows
// 4–5). A third bootstrap is the truth column. No socket is opened: the rule
// under test is a rendering rule over pages, and a live re-emission
// (CANT-143) would refresh the very numerator this harness holds stale.
//
// THE FAILING FORM ON `main` IS KEPT, as the plan asked: with the clamp off,
// row 1 renders `7/6`, a fraction above one that no serve ever said. That
// assertion is the negative control below, and the row is in the fixture as
// `unclamped` so both clients can pin their own clamp against it.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/wire"
)

// readFractionFixture is server/spec/testdata/read-fraction.json.
type readFractionFixture struct {
	Rule string            `json:"rule"`
	Rows []readFractionRow `json:"rows"`
}

type readFractionRow struct {
	ID   string `json:"id"`
	Case string `json:"case"`
	Held struct {
		ReadBy      int64 `json:"read_by"`
		MemberCount int64 `json:"member_count"`
	} `json:"held"`
	FreshMemberCount int64  `json:"fresh_member_count"`
	Unclamped        string `json:"unclamped"`
	Clamped          string `json:"clamped"`
	Truth            string `json:"truth"`
	InSpan           bool   `json:"in_span"`
}

const readFractionPath = "../../server/spec/testdata/read-fraction.json"

func loadReadFraction(t *testing.T) readFractionFixture {
	t.Helper()
	raw, err := os.ReadFile(readFractionPath)
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	var f readFractionFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode the fixture: %v", err)
	}
	if len(f.Rows) != 5 {
		t.Fatalf("the fixture has %d rows, want the plan's five", len(f.Rows))
	}
	return f
}

// renderFraction is the client's rule, with the clamp on or off. THIS IS THE
// ONLY PLACE THE RULE IS WRITTEN IN GO, and it is written in a test: the server
// never renders a fraction, and a helper in production code would be a fourth
// implementation of a rule that is supposed to have exactly two (one per
// client) plus the sentence in the schema they both implement.
func renderFraction(readBy, memberCount int64, clamp bool) string {
	n := readBy
	if clamp && n > memberCount {
		n = memberCount
	}
	return fmt.Sprintf("%d/%d", n, memberCount)
}

// served is one page's view of one row: the message as served (or absent)
// and the conversation's member_count.
type served struct {
	readBy      *int64
	memberCount int64
	present     bool
}

func (r *rig) syncFrom(token string, after int64) wire.SyncResponse {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/sync?after="+strconv.FormatInt(after, 10)+"&limit=500", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	code, raw := do(r.t, r.d.router, req)
	if code != http.StatusOK {
		r.t.Fatalf("GET /sync?after=%d = %d: %s", after, code, raw)
	}
	var page wire.SyncResponse
	if err := json.Unmarshal(raw, &page); err != nil {
		r.t.Fatalf("decode /sync: %v", err)
	}
	if page.HasMore {
		r.t.Fatal("the page has more; the harness assumes one page")
	}
	return page
}

// view reads what one page says about one room's one message.
func view(page wire.SyncResponse, room uuid.UUID, msg wire.Uuid) served {
	var s served
	for _, c := range page.Conversations {
		if string(c.ID) == room.String() {
			s.memberCount = c.MemberCount
			s.present = true
		}
	}
	for _, m := range page.Messages {
		if m.ID == msg {
			rb := *m.ReadBy
			s.readBy = &rb
		}
	}
	return s
}

func TestTheRenderRuleOverTwoServesMatchesTheTableOnEveryRow(t *testing.T) {
	fx := loadReadFraction(t)
	r := newRig(t)

	// SEVEN MEMBERS: the viewer and author, the person who will be offboarded,
	// and five others. Five rooms, one per row, all seven in each.
	theo := mkUser(r.ctx, t, r.pool, "theo", "Theo")
	adaAcct, err := r.st.EnsurePerson(r.ctx, "ada@example.com", "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	ada := adaAcct.Account.UserID
	others := make([]uuid.UUID, 5)
	for i := range others {
		others[i] = mkUser(r.ctx, t, r.pool, fmt.Sprintf("b%d", i), fmt.Sprintf("B%d", i))
	}
	members := append([]uuid.UUID{theo, ada}, others...)
	rooms := map[string]uuid.UUID{}
	for _, row := range fx.Rows {
		rooms[row.ID] = mkGroup(r.ctx, t, r.pool, row.ID, members...)
	}
	read := func(room uuid.UUID, who uuid.UUID, seq int64) {
		t.Helper()
		if _, err := r.st.MarkRead(r.ctx, room, who, seq); err != nil {
			t.Fatalf("mark read: %v", err)
		}
	}

	// THE MESSAGES THAT EXIST BEFORE THE OFFBOARD, and who has read each.
	msgs := map[string]wire.Uuid{}
	commit := func(rowID string) int64 {
		s := r.commit(rooms[rowID], theo, rowID)
		msgs[rowID] = wire.Uuid(s.ID.String())
		return s.Seq
	}
	// Row 1: all seven have read it.
	seq := commit("offboard-all-read")
	for _, u := range append([]uuid.UUID{ada}, others...) {
		read(rooms["offboard-all-read"], u, seq)
	}
	// Row 2: the other six have; the person has not.
	seq = commit("offboard-unread-by-person")
	for _, u := range others {
		read(rooms["offboard-unread-by-person"], u, seq)
	}
	// Row 3: five have — theo, the person, and three others.
	seq = commit("offboard-partially-read")
	for _, u := range append([]uuid.UUID{ada}, others[:3]...) {
		read(rooms["offboard-partially-read"], u, seq)
	}
	// Row 5: all seven have; this row is evaluated after the reversal, from
	// a device that was served it during the window.
	seq = commit("reversal-all-read")
	for _, u := range append([]uuid.UUID{ada}, others...) {
		read(rooms["reversal-all-read"], u, seq)
	}

	// DEVICE A BOOTSTRAPS BEFORE THE OFFBOARD.
	deviceA := r.enroll(theo, "phone")
	pageA1 := r.syncFrom(string(deviceA.AccessToken), 0)

	// THE OFFBOARD.
	if _, err := r.st.DeactivateUser(r.ctx, ada); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// Row 4: sent during the window, read by the other five (six with theo).
	seq = commit("reversal-sent-in-window")
	for _, u := range others {
		read(rooms["reversal-sent-in-window"], u, seq)
	}

	// DEVICE A CATCHES UP: the rooms at member_count 6, and no message.
	pageA2 := r.syncFrom(string(deviceA.AccessToken), int64(pageA1.LogSeq))
	if n := len(pageA2.Messages); n != 1 {
		// Exactly the one sent in the window — nothing held is re-served.
		t.Fatalf("device A's catch-up carries %d messages, want 1 (row 4's, sent in the window)", n)
	}
	// DEVICE B BOOTSTRAPS DURING THE WINDOW. Its page is the truth for rows
	// 1–3 and what device B holds for rows 4–5.
	deviceB := r.enroll(theo, "laptop")
	pageB1 := r.syncFrom(string(deviceB.AccessToken), 0)

	// THE REVERSAL.
	if _, err := r.st.EnsurePerson(r.ctx, "ada@example.com", "Ada Lovelace"); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	// DEVICE B CATCHES UP: the rooms at member_count 7, and no message.
	pageB2 := r.syncFrom(string(deviceB.AccessToken), int64(pageB1.LogSeq))
	if n := len(pageB2.Messages); n != 0 {
		t.Fatalf("device B's catch-up after the reversal carries %d messages, want 0", n)
	}
	// AND A THIRD BOOTSTRAP IS THE TRUTH FOR ROWS 4–5.
	deviceC := r.enroll(theo, "tablet")
	pageC := r.syncFrom(string(deviceC.AccessToken), 0)

	// The person's read mark per room, for `in_span`: the offboard's
	// re-emission covers (0, read_seq], the person's own messages excepted.
	adaReadSeq := func(room uuid.UUID) int64 {
		var n int64
		if err := r.pool.QueryRow(r.ctx,
			`SELECT read_seq FROM conversation_members WHERE conversation_id = $1 AND user_id = $2`, room, ada).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for _, row := range fx.Rows {
		t.Run(row.ID, func(t *testing.T) {
			room, msg := rooms[row.ID], msgs[row.ID]
			var held, fresh, truth served
			switch row.ID {
			case "offboard-all-read", "offboard-unread-by-person", "offboard-partially-read":
				held, fresh, truth = view(pageA1, room, msg), view(pageA2, room, msg), view(pageB1, room, msg)
			case "reversal-sent-in-window":
				// Device B was served M on its bootstrap, in the window.
				held, fresh, truth = view(pageB1, room, msg), view(pageB2, room, msg), view(pageC, room, msg)
			case "reversal-all-read":
				held, fresh, truth = view(pageB1, room, msg), view(pageB2, room, msg), view(pageC, room, msg)
			default:
				t.Fatalf("the fixture has a row this harness does not construct: %s", row.ID)
			}
			if held.readBy == nil || !held.present {
				t.Fatalf("the held serve did not carry the message and the room: %+v", held)
			}
			if fresh.readBy != nil {
				t.Fatalf("the later serve re-served the held message; the premise of this row is that it does not")
			}
			if !fresh.present {
				t.Fatalf("the later serve did not carry the room; the offboard's marker did not reach this viewer")
			}
			if truth.readBy == nil {
				t.Fatalf("the truth bootstrap did not carry the message")
			}

			// THE HELD VALUES ARE THE TABLE'S — otherwise the rest is about a
			// different row.
			if *held.readBy != row.Held.ReadBy || held.memberCount != row.Held.MemberCount {
				t.Errorf("held = %d/%d, the table says %d/%d", *held.readBy, held.memberCount, row.Held.ReadBy, row.Held.MemberCount)
			}
			if fresh.memberCount != row.FreshMemberCount {
				t.Errorf("fresh member_count = %d, the table says %d", fresh.memberCount, row.FreshMemberCount)
			}

			// THE THREE COLUMNS.
			if got := renderFraction(*held.readBy, fresh.memberCount, false); got != row.Unclamped {
				t.Errorf("unclamped (main's rule) renders %s, the table says %s", got, row.Unclamped)
			}
			if got := renderFraction(*held.readBy, fresh.memberCount, true); got != row.Clamped {
				t.Errorf("clamped renders %s, the table says %s", got, row.Clamped)
			}
			if got := renderFraction(*truth.readBy, truth.memberCount, true); got != row.Truth {
				t.Errorf("a fresh bootstrap renders %s, the table says %s", got, row.Truth)
			}
			// AND THE CLAMPED FRACTION NEVER EXCEEDS ONE — the property the rule
			// exists for, asserted directly rather than through the column.
			n := min(*held.readBy, fresh.memberCount)
			if n > fresh.memberCount {
				t.Errorf("the clamped numerator %d exceeds the denominator %d", n, fresh.memberCount)
			}

			// IN SPAN: whether CANT-143's re-emission reaches this message live.
			var msgSeq int64
			for _, m := range append(pageA1.Messages, pageB1.Messages...) {
				if m.ID == msg {
					msgSeq = int64(m.Seq)
				}
			}
			if inSpan := msgSeq <= adaReadSeq(room); inSpan != row.InSpan {
				t.Errorf("in_span = %v (seq %d, the person's read_seq %d), the table says %v", inSpan, msgSeq, adaReadSeq(room), row.InSpan)
			}
		})
	}
}

// THE FAILING FORM ON `main`, KEPT. Without the clamp, the first row renders a
// numerator above its denominator — `7/6` — which no serve ever said and which
// StatusLabel.vue rendered verbatim before CANT-145. This is what the rule is
// for; it is asserted from the fixture so the row cannot quietly change.
func TestWithoutTheClampTheHeldNumeratorExceedsTheDenominator(t *testing.T) {
	fx := loadReadFraction(t)
	var seen bool
	for _, row := range fx.Rows {
		unclamped := renderFraction(row.Held.ReadBy, row.FreshMemberCount, false)
		clamped := renderFraction(row.Held.ReadBy, row.FreshMemberCount, true)
		if unclamped != row.Unclamped || clamped != row.Clamped {
			t.Errorf("%s: the fixture's own columns disagree with the rule: unclamped %s vs %s, clamped %s vs %s",
				row.ID, unclamped, row.Unclamped, clamped, row.Clamped)
		}
		if row.Held.ReadBy > row.FreshMemberCount {
			seen = true
			if clamped == unclamped {
				t.Errorf("%s: the clamp changed nothing on a row where the held numerator exceeds the denominator", row.ID)
			}
		}
	}
	if !seen {
		t.Fatal("no row of the fixture has a held numerator above its fresh denominator; the `7/6` case " +
			"the rule exists for has been dropped from the table")
	}
}
