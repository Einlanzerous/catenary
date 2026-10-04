package client

// CANT-46 — Faults.KeepHeldConversation, the convergence rig's control: with
// it off a re-served Conversation replaces the held record, on a page and on a
// live frame; with it on the held record stays, and nothing else about the
// journal changes — the message the same page carried is held and the cursor
// moves, which is why Compare cannot see this fault and SameState has to.

import (
	"sync/atomic"
	"testing"

	"github.com/magos/catenary/internal/wire"
)

func unreadAt(cid wire.Uuid, firstUnread, head int64) wire.Conversation {
	cv := conversationOf(cid, "A")
	seq := wire.Seq(firstUnread)
	cv.FirstUnreadSeq, cv.HeadSeq = &seq, wire.Seq(head)
	return cv
}

func TestAReServedConversationReplacesTheHeldOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		faults   Faults
		replaced bool
	}{
		{"a correct client", Faults{}, true},
		{"control — KeepHeldConversation", Faults{KeepHeldConversation: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The client asks more than once on its way to ready, so the second
			// serve waits for the test to say the room has moved.
			var moved atomic.Bool
			r := newIntroRig(t, tc.faults, theo, func(after int64) wire.SyncResponse {
				if after == 0 {
					p := bootstrap()
					p.Conversations = []wire.Conversation{unreadAt(roomA, 1, 3)}
					return p
				}
				if !moved.Load() {
					return emptyAt(3)
				}
				if after == 3 {
					p := emptyAt(4)
					p.Messages = []wire.Message{messageAt(4, roomA, ada, 4)}
					p.Conversations = []wire.Conversation{unreadAt(roomA, 3, 4)}
					return p
				}
				return emptyAt(4)
			})
			r.await("ready and caught up", caughtUpAt3)
			if cv, ok := heldConversation(r.c, roomA); !ok || cv.FirstUnreadSeq == nil || *cv.FirstUnreadSeq != 1 {
				t.Fatalf("after the bootstrap the room is held as %+v (held %v), want first_unread_seq 1", cv, ok)
			}

			// A PAGE re-serves the room with the marker moved.
			moved.Store(true)
			r.c.CatchUp()
			r.await("the catch-up page", func(s Status) bool { return s.CaughtUp && s.Cursor == 4 })
			if !r.c.Holds(id(1004)) {
				t.Fatal("the page's message is not held; the fault must leave messages alone")
			}
			wantUnread, wantHead := int64(3), int64(4)
			if !tc.replaced {
				wantUnread, wantHead = 1, 3
			}
			cv, _ := heldConversation(r.c, roomA)
			if cv.FirstUnreadSeq == nil || int64(*cv.FirstUnreadSeq) != wantUnread || int64(cv.HeadSeq) != wantHead {
				t.Errorf("after the page the room is held as %+v, want first_unread_seq %d and head_seq %d", cv, wantUnread, wantHead)
			}

			// A LIVE `conversation` frame re-serves it again.
			r.frame(wire.ServerConversationFrame{Conversation: unreadAt(roomA, 5, 4)})
			r.settle()
			if tc.replaced {
				wantUnread = 5
			}
			cv, _ = heldConversation(r.c, roomA)
			if cv.FirstUnreadSeq == nil || int64(*cv.FirstUnreadSeq) != wantUnread {
				t.Errorf("after the live frame the room is held as %+v, want first_unread_seq %d", cv, wantUnread)
			}
		})
	}
}
