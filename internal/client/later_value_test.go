package client

// CANT-177 beyond the close: every frame and page carrying a value a later
// server adds to a client-open enum is HANDLED, as TypeScript and Dart handle
// it, rather than dropped. Each test runs the client twice, once as it is and
// once under Faults.StrictDecode — every Go client before that ticket — and
// the control must fail where the client passes.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/magos/catenary/internal/wire"
)

// rawFrame marshals a frame the test means to carry a value this wire version
// does not define, so it cannot go through introRig.frame, whose check is the
// strict decoder.
func rawFrame(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.DecodeServerFrame(b); err == nil {
		t.Fatalf("the strict decoder accepts %s; the test needs a value it refuses", b)
	}
	if f, err := wire.DecodeServerFrameAsClient(b); err != nil || f == nil {
		t.Fatalf("the client decoder refuses %s: %v", b, err)
	}
	return b
}

func caughtUpAt3(s Status) bool { return s.Ready && s.CaughtUp && s.Cursor == 3 }

func bootstrapThenEmpty(after int64) wire.SyncResponse {
	if after == 0 {
		return bootstrap()
	}
	return emptyAt(3)
}

// A `resync_required` WITH A NEW REASON IS A TRIGGER. triggerLocked never reads
// the reason, so the only thing standing between the frame and the catch-up it
// asks for was the decoder.
func TestAResyncWithANewReasonTriggersAResync(t *testing.T) {
	for _, tc := range []struct {
		name    string
		faults  Faults
		handled bool
	}{
		{"the client decoder", Faults{}, true},
		{"control — strict decode", Faults{StrictDecode: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newIntroRig(t, tc.faults, theo, bootstrapThenEmpty)
			r.await("ready and caught up", caughtUpAt3)
			before, requests := r.c.Status(), r.syncRequests()

			r.frames <- rawFrame(t, wire.ServerResyncRequired{Reason: "schema_migrated", LogSeq: 3})
			r.settle()
			if !tc.handled {
				time.Sleep(20 * time.Millisecond) // a trigger pulled would have fetched by now
				s := r.c.Status()
				if s.Resyncs != before.Resyncs || s.Undecodable != before.Undecodable+1 || r.syncRequests() != requests {
					t.Errorf("resyncs %d → %d, undecodable %d → %d, sync requests %d → %d; want the frame refused and nothing fetched",
						before.Resyncs, s.Resyncs, before.Undecodable, s.Undecodable, requests, r.syncRequests())
				}
				return
			}
			awaitFor(t, "the catch-up the resync asks for", func() bool { return r.syncRequests() > requests })
			r.await("caught up again", caughtUpAt3)
			if s := r.c.Status(); s.Resyncs != before.Resyncs+1 || s.Undecodable != before.Undecodable {
				t.Errorf("resyncs %d → %d, undecodable %d → %d; want one resync and nothing refused",
					before.Resyncs, s.Resyncs, before.Undecodable, s.Undecodable)
			}
		})
	}
}

// A `conversation` FRAME WITH A NEW KIND REACHES applyIntroduction, which never
// branches on the kind, and the Journal holds the conversation with the kind
// decoded to "unknown".
func TestAConversationWithANewKindIsIntroduced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		faults  Faults
		handled bool
	}{
		{"the client decoder", Faults{}, true},
		{"control — strict decode", Faults{StrictDecode: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newIntroRig(t, tc.faults, theo, bootstrapThenEmpty)
			r.await("ready and caught up", caughtUpAt3)
			undecodable := r.c.Status().Undecodable

			roomC := id(103)
			cv := conversationOf(roomC, "channel")
			cv.Kind = "channel"
			r.frames <- rawFrame(t, wire.ServerConversationFrame{Conversation: cv})
			r.settle()

			held, ok := heldConversation(r.c, roomC)
			s := r.c.Status()
			if !tc.handled {
				if ok || s.Undecodable != undecodable+1 {
					t.Errorf("held %v, undecodable %d → %d; want the frame refused and nothing introduced", ok, undecodable, s.Undecodable)
				}
				return
			}
			if !ok || !held.Kind.IsUnknown() || held.Name != "channel" || s.Undecodable != undecodable {
				t.Errorf("held %v as %+v, undecodable %d → %d; want it introduced with kind unknown and nothing refused",
					ok, held, undecodable, s.Undecodable)
			}
		})
	}
}

// A /sync PAGE WITH A NEW KIND COMPLETES CATCH-UP. Strictly decoded, the page is
// refused on every retry: catchUpLoop asks for the same after= with backoff for
// ever, the cursor never passes the page, and the client stays connected and
// silently never catches up.
func TestASyncPageWithANewKindDoesNotWedgeCatchUp(t *testing.T) {
	roomC := id(103)
	page := func(after int64) wire.SyncResponse {
		if after != 0 {
			return emptyAt(3)
		}
		p := bootstrap()
		cv := conversationOf(roomC, "channel")
		cv.Kind = "channel"
		p.Conversations = append(p.Conversations, cv)
		return p
	}
	for _, tc := range []struct {
		name    string
		faults  Faults
		handled bool
	}{
		{"the client decoder", Faults{}, true},
		{"control — strict decode", Faults{StrictDecode: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newIntroRig(t, tc.faults, theo, page)
			if !tc.handled {
				awaitFor(t, "the page to be asked for again", func() bool { return r.syncRequests() >= 3 })
				s := r.c.Status()
				r.mu.Lock()
				requests := append([]int64(nil), r.requests...)
				r.mu.Unlock()
				for _, after := range requests {
					if after != 0 {
						t.Errorf("sync requests %v; the cursor passed a page the strict decoder refuses", requests)
						break
					}
				}
				if _, ok := heldConversation(r.c, roomC); s.CaughtUp || s.HasCursor || ok {
					t.Errorf("caught up %v, cursor %d (held %v), conversation held %v; want the page refused on every retry",
						s.CaughtUp, s.Cursor, s.HasCursor, ok)
				}
				return
			}
			r.await("caught up past the page", caughtUpAt3)
			if held, ok := heldConversation(r.c, roomC); !ok || !held.Kind.IsUnknown() {
				t.Errorf("held %v as %+v; want the page's conversation stored with kind unknown", ok, held)
			}
		})
	}
}

func heldConversation(c *Client, cid wire.Uuid) (wire.Conversation, bool) {
	for _, cv := range c.Snapshot().Conversations {
		if cv.ID == cid {
			return cv, true
		}
	}
	return wire.Conversation{}, false
}
