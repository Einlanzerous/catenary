package api

// CANT-75 — the REST write path for bots and agents: `POST
// /conversations/{id}/messages` and `POST /conversations/direct`. Neither
// route requires a device: both authenticate through Deps.Caller, which is
// store.Authenticate in full, so a bot token resolves to a Caller with
// DeviceID uuid.Nil exactly as store.Caller.IsBot documents, and a request
// from one is accepted rather than refused at the door the way the socket
// refuses it (CANT-22 refuses a bot there because a socket has nowhere to
// bind a device; REST has no such binding to make).
//
// BOTH ARE THIN HANDLERS OVER THE STORE. The insert, the dedup and the notify
// are the SAME code path a `send` frame uses — Deps.Send is store.SendMessage,
// the identical function socket.go's handleSend calls — and find-or-create is
// store.Store.FindOrCreateDirect. The transport is the only thing that
// differs, which is what keeps this Mode A rather than a second design.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
	"github.com/magos/catenary/internal/wireview"
)

// defaultMaxRESTWriteBody is the REST body bound when Deps carries none —
// the same fallback socket.go's defaultMaxFrameBytes uses, and for the same
// reason: a caller that builds Deps outside the composition root (a test)
// still gets a sane bound rather than an unbounded read.
//
// NOT THE BOUND A REAL PROCESS RUNS WITH. A fixed constant here, independent
// of CATENARY_MAX_MESSAGE_BYTES, was a review finding on CANT-75's PR: an
// operator who raised the message bound got a message the store would have
// accepted refused by the TRANSPORT first, as a 400 "malformed body" that
// looks like a JSON bug and is not one — the transport was no longer only
// the thing that differs. cmd/catenary now derives Deps.MaxRESTBodyBytes
// with the identical expression main.go already used for the socket's
// Deps.MaxFrameBytes, so the two transports share one bound rather than
// disagreeing at whatever margin CATENARY_MAX_MESSAGE_BYTES is set to.
const defaultMaxRESTWriteBody = 256 << 10

// restBodyLimit resolves the configured bound or the fallback above.
func restBodyLimit(d Deps) int64 {
	if d.MaxRESTBodyBytes > 0 {
		return d.MaxRESTBodyBytes
	}
	return defaultMaxRESTWriteBody
}

// messagesHandler serves POST /conversations/{id}/messages.
//
// THE RESPONSE IS A wire.Message, NOT A wire.ServerAck. The socket's ack is
// small because the SAME connection also gets the full Message a moment
// later, from the hub's fan-out; a REST caller has no second delivery to
// wait for, and CANT-75's Done-when is explicit that the response IS "the
// same Message the socket broadcasts". So a successful send re-reads the row
// through the identical fan-out load the hub uses (Deps.MessageForFanout,
// store.Store.MessageForFanout) and maps it with the identical
// wireview.Message the hub calls (CANT-84) — never a second assembly of the
// same row, which is the whole of what would let the two transports drift.
func messagesHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := d.Caller(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}

		// A MALFORMED PATH SEGMENT IS A 400, NOT A wire ErrorCode — the same
		// rule syncHandler applies to a malformed query string: the enum has
		// no member for "your url is unparseable", and internal would be a
		// lie about whose fault it is.
		convID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is not a valid conversation id"})
			return
		}

		var req wire.MessageSendRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, restBodyLimit(d))).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body is not a valid MessageSendRequest"})
			return
		}

		m := store.NewMessage{
			ConversationID: convID,
			AuthorID:       caller.UserID,
			ClientID:       mustUUID(req.ClientID),
			Text:           req.Text,
		}
		// A BOT HAS NO DEVICE — store.Caller.DeviceID is uuid.Nil for one, per
		// IsBot's own doc — so SenderDeviceID stays nil rather than pointing
		// at the zero UUID. The socket can skip this check: a session always
		// binds to a real device, because CANT-22 refuses a bot at the door
		// before a hello is ever read.
		if caller.DeviceID != uuid.Nil {
			id := caller.DeviceID
			m.SenderDeviceID = &id
		}
		if req.ReplyToMessageID != nil {
			id := mustUUID(*req.ReplyToMessageID)
			m.ReplyTo = &id
		}
		for _, a := range req.Attachments {
			m.Attachments = append(m.Attachments, store.NewAttachment{Kind: a.Kind, UploadID: mustUUID(a.UploadID)})
		}

		sent, err := d.Send(r.Context(), m)
		if err != nil {
			// THE CODE COMES FROM THE STORE, on socket.go's own reasoning: REST
			// and the socket must answer the identical cause with the identical
			// code, and the only way to guarantee that is for neither of them
			// to choose. THE STATUS DOES TOO (CANT-75) — se.HTTPStatus() is the
			// row senderror.go carries next to the code, read rather than
			// mapped here.
			var se *store.SendError
			_ = errors.As(store.SendErrorFor(err), &se)
			d.Logger.Log(r.Context(), se.Level(), "send refused",
				append([]any{"client_id", req.ClientID, "conversation_id", convID}, se.LogAttrs()...)...)
			frame := se.Wire("send failed")
			cid := req.ClientID
			frame.ClientID = &cid
			writeJSON(w, se.HTTPStatus(), frame)
			return
		}

		// FROM sent, NEVER FROM THE REQUEST. messages.go's own Sent.ConversationID
		// doc states why: dedup is per (author, client_id) and not per
		// conversation, so a replayed key can resolve to a row in a
		// conversation other than the one this request named. Building the
		// response from anything but sent would risk exactly the ghost-message
		// class this project exists to avoid.
		fm, err := d.MessageForFanout(r.Context(), sent.ConversationID, sent.Seq)
		if err != nil {
			d.Logger.ErrorContext(r.Context(), "read back a just-sent message failed",
				"message_id", sent.ID, "conversation_id", sent.ConversationID, "seq", sent.Seq, "error", err)
			writeJSON(w, http.StatusInternalServerError, serverError(err, "message sent, but could not be read back"))
			return
		}

		// Scoped to this conversation, the same guard hub.OnNotify applies —
		// see its own comment on why the store's scope is a membership guard
		// and not the same-thread rule wireview checks on top of it.
		var src *store.ReplySource
		if fm.ReplySource != nil && fm.ReplySource.ConversationID == fm.Message.ConversationID {
			src = fm.ReplySource
		}
		readBy := fm.ReadBy
		msg := wireview.Message(fm.Message, fm.Attachments, src, wireview.Viewer{
			UserID: caller.UserID,
			// 0 rather than a lookup into fm.Members: DeliveryState reads
			// viewerReadSeq only on the branch where the viewer did NOT
			// author the message, and fm.Message.AuthorID is always
			// caller.UserID here — sent names this caller's own row, created
			// or replayed. Passing the true value would cost a lookup that
			// can never change the answer.
			State:    wireview.DeliveryState(fm.Message, caller.UserID, 0, fm.ReadBy),
			ReadBy:   &readBy,
			MediaURL: d.MediaURL,
		})
		writeJSON(w, http.StatusOK, msg)
	}
}

// directConversationHandler serves POST /conversations/direct: find-or-create
// the direct conversation with a handle, so a bot can message someone it has
// never spoken to without already knowing a conversation id. Returns the
// same wire.Conversation either way — the caller cannot tell from the
// response alone whether this call created it.
func directConversationHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := d.Caller(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}

		var req wire.DirectConversationRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, restBodyLimit(d))).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body is not a valid DirectConversationRequest"})
			return
		}

		c, err := d.FindOrCreateDirect(r.Context(), caller.UserID, req.Handle)
		if err != nil {
			var se *store.SendError
			_ = errors.As(store.SendErrorFor(err), &se)
			d.Logger.Log(r.Context(), se.Level(), "find-or-create direct refused",
				append([]any{"viewer_id", caller.UserID, "target_handle", req.Handle}, se.LogAttrs()...)...)
			writeJSON(w, se.HTTPStatus(), se.Wire("find-or-create direct failed"))
			return
		}
		writeJSON(w, http.StatusOK, wireview.Conversation(c))
	}
}
