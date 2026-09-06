package store

// CANT-83 — THE table, and the only place a refusal acquires a wire code.
//
// Both transports have to produce the identical code for the identical cause.
// A refusal decided in a handler is a refusal that gets decided twice,
// differently, on the path nobody looks at — which is Invariant 2's failure
// class arriving through the back door, on the error path. So the decision
// lives here once, and errorcode_guard_test.go fails the build if any other
// file in this module so much as names a send code.
//
// The store returns *SendError. It is not a sentinel to be string-matched and
// not a second store-side enum to be translated: a translation table is
// precisely the second place the two transports could disagree.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/magos/catenary/internal/wire"
)

// The causes. Each one is a refusal this store can decide; the table below is
// total over them, and the guard test proves it stays that way.
//
// ErrNoClientID and ErrNotFound are in store.go and predate this file.
var (
	// ErrNotAMember is a send into a conversation the author is not in.
	ErrNotAMember = errors.New("store: not a member of this conversation")

	// ErrConversationNotFound WRAPS ErrNotFound rather than replacing it, so
	// callers already written against errors.Is(err, ErrNotFound) keep
	// working, and this file can still tell the two apart.
	//
	// It has to tell them apart. sentByKey returns a bare ErrNotFound when the
	// row that just won a dedup race cannot be re-read — an internal
	// inconsistency, not a conversation the sender named wrongly. Mapping
	// every ErrNotFound to conversation_not_found would report a server bug as
	// the sender's mistake.
	ErrConversationNotFound = fmt.Errorf("store: conversation not found: %w", ErrNotFound)

	// ErrMessageTooLarge is text over CATENARY_MAX_MESSAGE_BYTES.
	ErrMessageTooLarge = errors.New("store: message body is too large")

	// ErrTooManyAttachments is more than CATENARY_MAX_ATTACHMENTS. A separate
	// cause carrying the SAME code: the table is cause-to-code and not a
	// bijection, and the sender is told the one thing the wire can say.
	ErrTooManyAttachments = errors.New("store: too many attachments")

	// ErrUploadNotFound is an upload_id that does not resolve. The resolver is
	// CANT-48's under CANT-47's storage decision; the code exists on the wire
	// already, so the seam is named here rather than invented later.
	ErrUploadNotFound = errors.New("store: upload not found")

	// ErrRateLimited is PLUMBED AND UNEMITTED. retry_after_sec travels end to
	// end and the vector server_error_rate_limited stays satisfied, but no
	// policy decides when to raise it and no ticket on the board owns one.
	// Inventing a limiter here would be scope this ticket did not ask for.
	//
	// The guard asserts the table is total over the causes the store can
	// PRODUCE, not that every ErrorCode is reachable, so this staying unemitted
	// is not a red build.
	ErrRateLimited = errors.New("store: rate limited")
)

// SendError is what a refusal looks like leaving the store. It carries
// wire.ErrorCode itself rather than a store-side twin, because a twin needs a
// translation and a translation is a second decision.
type SendError struct {
	Code wire.ErrorCode

	// Retryable is the SERVER's judgement about whether re-sending the
	// identical frame could succeed. It travels with the code because the
	// client cannot work it out: only this side knows whether the refusal came
	// from the sender's input or from a hiccup underneath it.
	Retryable bool

	// RetryAfterSec is set only with rate_limited, and only once a policy
	// exists to set it.
	RetryAfterSec *int

	// Cause is the underlying error, preserved so a log can say what actually
	// happened without the code having to carry it.
	Cause error
}

func (e *SendError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("store: send refused: %s (retryable=%t)", e.Code, e.Retryable)
	}
	return fmt.Sprintf("store: send refused: %s (retryable=%t): %v", e.Code, e.Retryable, e.Cause)
}

func (e *SendError) Unwrap() error { return e.Cause }

// sendErrorRow is one row of the table: a cause, the code it gets, and whether
// the server thinks the identical frame could succeed next time.
type sendErrorRow struct {
	cause     error
	code      wire.ErrorCode
	retryable bool
}

// THE TABLE.
//
// Matched in order with errors.Is, so a wrapped cause resolves and the more
// specific rows come first — ErrConversationNotFound wraps ErrNotFound, so it
// must be tested before anything matching the bare sentinel.
//
// retryable is per cause, and it is false almost everywhere for one reason:
// the identical frame will fail identically. Membership, existence, size and
// upload resolution are all facts about the request, and re-sending it changes
// none of them. rate_limited is the exception, and internal is not a single
// cause at all — see internalSendError.
//
// conversation_not_found versus not_a_member LEAKS EXISTENCE, deliberately. A
// sender who is not a member learns that an id resolves. The wire defines both
// codes and CANT-18's Done-when requires not_a_member for a non-member send;
// collapsing them would tell a member of a deleted conversation the wrong
// thing. Among a small trusted group with authenticated senders that is the
// right trade, and it is written down here rather than discovered later.
var sendErrorTable = []sendErrorRow{
	{cause: ErrNotAMember, code: wire.ErrorCodeNotAMember, retryable: false},
	{cause: ErrConversationNotFound, code: wire.ErrorCodeConversationNotFound, retryable: false},
	{cause: ErrMessageTooLarge, code: wire.ErrorCodeMessageTooLarge, retryable: false},
	{cause: ErrTooManyAttachments, code: wire.ErrorCodeMessageTooLarge, retryable: false},
	{cause: ErrUploadNotFound, code: wire.ErrorCodeUploadNotFound, retryable: false},
	{cause: ErrRateLimited, code: wire.ErrorCodeRateLimited, retryable: true},
}

// sendErrorFor is the single decision. Everything the store refuses goes
// through here, so there is exactly one answer per cause.
func sendErrorFor(err error) *SendError {
	if err == nil {
		return nil
	}

	// Already classified — a nested call that decided, wrapped on the way out.
	// Returned unchanged rather than re-decided, so a code cannot be
	// overwritten by a caller further up.
	var se *SendError
	if errors.As(err, &se) {
		return se
	}

	for _, row := range sendErrorTable {
		if errors.Is(err, row.cause) {
			return &SendError{Code: row.code, Retryable: row.retryable, Cause: err}
		}
	}
	return internalSendError(err)
}

// internalSendError is the table's last row, and the only one that reads the
// error rather than matching it. `internal` is not one cause: a transient
// failure underneath us is retryable and everything else is not.
func internalSendError(err error) *SendError {
	return &SendError{Code: wire.ErrorCodeInternal, Retryable: isTransient(err), Cause: err}
}

// isTransient reports whether re-running the identical statement could
// plausibly succeed.
//
// Being liberal here is safe, and that is a property of THIS operation rather
// than of Postgres clients generally: client_id deduplicates, so a retry of a
// send that did in fact commit returns the original ack instead of a second
// message. "Did it land?" does not need answering, which is what makes the
// whole class safe to call retryable rather than sloppy. Reporting a deadlock
// as permanent turns a hiccup into an outbox entry stuck at `failed`.
func isTransient(err error) bool {
	// A cancelled or expired context first, because context.DeadlineExceeded
	// satisfies net.Error and would otherwise be classified by accident three
	// branches down.
	//
	// Canceled: the caller deliberately stopped. There is no frame in flight
	// to retry and nobody waiting to be told to.
	// DeadlineExceeded: the canonical transient. The send may or may not have
	// landed, and client_id is what makes not knowing free.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// A PgError means the server considered the statement. If it is not one of
	// the three transient classes it is a decision, and re-sending sends the
	// identical statement — so this returns rather than falling through.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 40001 serialization failure, 40P01 deadlock. Deadlock is not
		// hypothetical here: this file's lock-order comment exists because two
		// writers taking the row locks in opposite orders deadlock, and
		// Postgres resolves that by aborting somebody's send.
		if pgErr.Code == "40001" || pgErr.Code == "40P01" {
			return true
		}
		// Class 08 — connection exception.
		return strings.HasPrefix(pgErr.Code, "08")
	}

	// Nothing reached the server, so nothing can have committed. Deliberately
	// narrow in pgx — it reports only the case it can prove — which is why it
	// is one branch of three rather than the whole test.
	if pgconn.SafeToRetry(err) {
		return true
	}

	// A dropped connection usually carries NO PgError at all: pgx surfaces it
	// as io.EOF, a net.Error, or its own closed-connection error. This is the
	// branch that catches the common case, and the one a SQLSTATE-only test
	// would never reach.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
