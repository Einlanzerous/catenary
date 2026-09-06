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
	//
	// *int64 because ServerError.retry_after_sec is *int64. The plan's snippet
	// said *int, and an int here would need a conversion at the boundary —
	// which is the translation this type exists to not have.
	RetryAfterSec *int64

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
// Matched in order with errors.Is, so a wrapped cause resolves.
//
// There is no row for the bare ErrNotFound and there must not be one. That is
// what sends it to `internal`, which is correct: the only place the store
// returns a bare ErrNotFound on this path is sentByKey failing to re-read the
// winner of a dedup race, and that is our inconsistency rather than the
// sender's mistake. Adding a row for it would quietly relabel a server bug as
// conversation_not_found — so the ordering is not what protects that
// distinction, the absence of a row is.
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

// SendErrorFor is the single decision. Everything the store refuses goes
// through here, so there is exactly one answer per cause.
//
// EXPORTED because the guard makes it the only door. With the six send codes
// banned outside this file, a transport that needs to report a failure — a sync
// that errors, a receipt write that fails — has no way to name `internal` and
// no way to construct the refusal itself. That is the ban working as intended,
// and this is the door it leaves: hand it a cause, get back the decision.
//
// The same argument reaches one place further. A REST transport has to map a
// code to an HTTP status, and it cannot do that without naming codes either —
// so the status belongs on the row, in this file, when CANT-75 needs it. It is
// not added now because nothing consumes it yet; it is recorded on CANT-83 so
// CANT-75 does not rediscover it as a blocked build.
func SendErrorFor(err error) *SendError {
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
// whole class safe to call retryable rather than sloppy.
//
// The asymmetry runs one way and it decides every close call below. Reporting a
// transient failure as permanent parks a real message at `failed` forever.
// Reporting a permanent failure as transient costs a retry the client was going
// to back off from anyway. So where a case is genuinely unclear, it is
// transient.
func isTransient(err error) bool {
	// Context first, because context.DeadlineExceeded satisfies net.Error and
	// would otherwise be classified three branches down by accident.
	//
	// BOTH are transient, and Canceled is the one that took an argument.
	// A cancel has two possible authors and the flag is only ever read by one
	// of them. If the CLIENT cancelled, its socket is gone and nobody observes
	// what we set. If the SERVER cancelled — a drain, a shutdown — the client
	// is alive and listening, and `false` marks a perfectly good message failed
	// forever, which is the outcome the paragraph above exists to prevent. True
	// is therefore right-or-unobserved in both cases and false is
	// wrong-or-unobserved, so the asymmetry buys nothing.
	//
	// pgx normalises a mid-query cancel to a bare context.Canceled, so this
	// branch is reached in practice and not only in theory —
	// TestTerminatedBackendIsClassifiedTransient's sibling probe showed it
	// arriving as *errors.errorString, which is context.Canceled itself.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// A PgError means the server considered the statement and answered. If it
	// is not one of the classes below, that answer was a decision and
	// re-sending sends the identical statement — so this RETURNS rather than
	// falling through to the connection-shaped branches, which would otherwise
	// rescue a permanent refusal.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Severity cuts across class, so it is tested first. pgx CLOSES the
		// connection on a FATAL, so whatever happens next happens on a fresh
		// one — which is the definition of worth retrying.
		//
		// SeverityUnlocalized when the server offers it: Severity is
		// translated under a non-English lc_messages and "FATAL" would then
		// silently never match.
		// This deliberately catches CONNECT-time failures too, which arrive as
		// a *pgconn.ConnectError wrapping a PgError — measured: a nonexistent
		// database gives 3D000 at FATAL. A wrong DSN never fixes itself, so
		// calling it transient is arguably generous. It is the right side of
		// the asymmetry anyway: a misconfiguration fails EVERY send, and an
		// outbox that holds until an operator fixes it is a better outcome
		// than one that marks every message failed on the way past.
		switch sev := firstNonEmpty(pgErr.SeverityUnlocalized, pgErr.Severity); sev {
		case "FATAL", "PANIC":
			return true
		}

		// 40001 serialization failure, 40P01 deadlock. Deadlock is not
		// hypothetical here: this file's lock-order comment exists because two
		// writers taking the row locks in opposite orders deadlock, and
		// Postgres resolves that by aborting somebody's send.
		if pgErr.Code == "40001" || pgErr.Code == "40P01" {
			return true
		}

		// Three classes, all of them "not now" rather than "not ever":
		//
		//   08  connection exception.
		//   53  insufficient resources — 53300 too many connections, 53200 out
		//       of memory, 53100 disk full. A restart or a quieter minute
		//       fixes every one of them.
		//   57  operator intervention — 57P01 admin shutdown, 57P02 crash
		//       shutdown, 57P03 cannot connect now, 57014 query cancelled.
		//
		// Class 57 is the one this file got wrong first. `pg_terminate_backend`
		// against the sending connection — which is how criterion 10 tests a
		// real drop — makes the NEXT statement return 57P01 at FATAL severity,
		// not a class 08 and not a bare connection error. Classifying that
		// permanent would have failed criterion 10's own test. 57014 arrives at
		// ERROR severity rather than FATAL, so the severity rule above does not
		// cover it and the class rule has to.
		for _, class := range [...]string{"08", "53", "57"} {
			if strings.HasPrefix(pgErr.Code, class) {
				return true
			}
		}
		return false
	}

	// Nothing reached the server, so nothing can have committed. Deliberately
	// narrow in pgx — it reports only the case it can prove — which is why it
	// is one branch of several rather than the whole test. It is what catches
	// the COMMIT that follows a terminated backend, which surfaces as
	// pgconn's connLockError with no PgError attached.
	if pgconn.SafeToRetry(err) {
		return true
	}

	// A dropped connection often carries no PgError at all: pgx surfaces it as
	// io.EOF, a net.Error, or a closed-connection error. This is also the
	// branch that catches a refused connection, which arrives as a
	// *pgconn.ConnectError wrapping a net.Error.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
