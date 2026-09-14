package store

// CANT-24 — what the server does with the cursor a hello carries.
//
// NOTHING. That is the decision (CANT-24 ruling 0, option 3): the socket does
// not resume, /sync does. Every hello is answered `ready{resumed: false}` and
// the client catches up over /sync from whatever cursor it holds, whatever
// `ready` says — and it may issue that /sync concurrently with the upgrade, so
// the round trip a socket resume would have saved is not on the critical path.
//
// What is left for the server to do with the cursor is COMPARE IT TO HEAD and
// say so, once, in a structured log line. That comparison is this file. It is
// the only measurement of reconnect gaps this deployment will ever have, and
// the histogram of `delta` is the one piece of evidence that could reopen the
// ruling: if reconnects are a handful of messages behind and the round trip
// ever shows up in a measurement, streaming has a case; if they are hours
// behind, it never did.
//
// ONE OUTCOME IS A WARNING. `cursor_ahead` — the client holds a cursor above
// the server's head — is what a client sees after a restore from backup, or
// against the wrong database, and the honest answer is to say so rather than
// to treat it as caught up: a socket that looked healthy over a log that had
// been rolled back would sync nothing forever. It is the client's obligation 4
// to discard its store and bootstrap from 0 on that signal.
//
// IT IS THE NARROW WINDOW, NOT THE RESTORE SIGNATURE. After a restore the log
// regrows: restore to head 80, thirty messages land, and a client at cursor 100
// sees head 110 — `behind`, with 101..110 being different messages from the
// 81..100 it holds, and nothing here can tell. The WARN fires only if the
// client reconnects before the log has regrown past its cursor. The fix is a
// log generation id, minted at migration and carried on `ready` and
// `SyncResponse`; that is a wire change and belongs to CANT-68's restore drill
// and CANT-74's compatibility policy, not here. In its window this outcome is
// honest and costs one comparison, so it stays.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// HelloOutcome is where a hello's cursor sits relative to the server's head.
type HelloOutcome string

const (
	// HelloNoCursor: the hello carried no cursor. A fresh install, or a
	// client that wiped its store. Its /sync starts at 0.
	HelloNoCursor HelloOutcome = "no_cursor"
	// HelloBehind: the ordinary reconnect. The cursor is below head and /sync
	// from it will carry the gap, conversations and users included.
	HelloBehind HelloOutcome = "behind"
	// HelloAtHead: nothing committed since the client's last page. Its /sync
	// from the cursor returns an empty page with has_more false, which is how
	// its catch-up ends; it is not exempt from the catch-up.
	HelloAtHead HelloOutcome = "at_head"
	// HelloCursorAhead: the cursor is above head. See the file header: the
	// server's log is behind the client's, and the client discards and
	// bootstraps from 0.
	HelloCursorAhead HelloOutcome = "cursor_ahead"
)

// HelloRequest is what the hello carried that this comparison needs. Device
// and session are here only for the log line — they are the two ids an
// operator would search by.
type HelloRequest struct {
	DeviceID  uuid.UUID
	SessionID uuid.UUID
	// Cursor is the client's `resume_from_log_seq`, nil when absent.
	//
	// A NEGATIVE CURSOR IS REFUSED HERE, not classified. The wire says LogSeq
	// is `minimum: 0` and the generated decoders enforce it, but the store is
	// the trust boundary and does not assume its callers decoded anything:
	// classified, a -1 would log `behind` with delta = head + 1 and poison the
	// one histogram ruling 0 would be reopened on, and math.MinInt64 would
	// overflow the subtraction into a `cursor_ahead` WARN — the line an
	// operator reads as a restore. So it is an error, logged as one, and the
	// hello it came from is refused upstream.
	Cursor *int64
}

// ErrNegativeCursor is returned by Hello for a cursor below zero. It is not a
// wire error code — CANT-83's guard keeps those in one file — so the socket
// edge answers it as it answers any malformed hello.
var ErrNegativeCursor = errors.New("store: hello: cursor must not be negative")

// HelloResult is the comparison, and it is what goes on the wire as
// `ready.log_seq` (Head) and — in wire version 1, always — `resumed: false`.
type HelloResult struct {
	Outcome HelloOutcome
	// Head is the server's current head, read once. It becomes
	// `ready.log_seq`: the head this session attached at, what the client's
	// catch-up is counting towards.
	Head int64
	// Cursor echoes the request, nil when absent.
	Cursor *int64
	// Delta is Head - Cursor, and it is only meaningful when Cursor is set:
	// positive for behind, zero at head, negative for cursor_ahead. Zero when
	// there is no cursor, and the Outcome says which zero it is.
	Delta int64
}

// Hello compares the cursor a hello carried against the server's head, and
// logs exactly one line saying so.
//
// It reads head with a plain SELECT — no transaction, no lock, nothing drawn.
// The number is a snapshot the moment it is read and the client's catch-up is
// bounded by its own /sync reads, not by this; a message committing a
// microsecond later belongs to the client's first page.
//
// Resumed is not a field on the result on purpose. In wire version 1 it is
// false for every hello, and a struct field that is always false is a field
// somebody eventually sets from the wrong place. CANT-22's door writes the
// literal where it builds the `ready` frame (internal/api/socket.go), beside
// the sentence that says why.
func (s *Store) Hello(ctx context.Context, req HelloRequest) (HelloResult, error) {
	if req.Cursor != nil && *req.Cursor < 0 {
		s.logger.WarnContext(ctx, "hello refused",
			"device_id", req.DeviceID, "session_id", req.SessionID,
			"cursor", *req.Cursor, "reason", "negative_cursor")
		return HelloResult{}, ErrNegativeCursor
	}
	var head int64
	if err := s.pool.QueryRow(ctx, `SELECT value FROM log_counter WHERE id = 1`).Scan(&head); err != nil {
		return HelloResult{}, fmt.Errorf("store: hello: read head: %w", err)
	}

	res := HelloResult{Head: head, Cursor: req.Cursor}
	attrs := []any{
		"device_id", req.DeviceID,
		"session_id", req.SessionID,
		"head", head,
	}
	level := slog.LevelInfo
	switch {
	case req.Cursor == nil:
		res.Outcome = HelloNoCursor
	default:
		res.Delta = head - *req.Cursor
		attrs = append(attrs, "cursor", *req.Cursor, "delta", res.Delta)
		switch {
		case res.Delta > 0:
			res.Outcome = HelloBehind
		case res.Delta == 0:
			res.Outcome = HelloAtHead
		default:
			res.Outcome = HelloCursorAhead
			level = slog.LevelWarn
		}
	}
	attrs = append(attrs, "outcome", string(res.Outcome))

	// One line, always, at the level the outcome earns. An operator reading
	// the log learns the gap distribution from INFO and the restore window
	// from WARN, and neither has to be enabled to see the other.
	s.logger.Log(ctx, level, "hello", attrs...)
	return res, nil
}
