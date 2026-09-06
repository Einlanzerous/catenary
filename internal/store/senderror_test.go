package store

// CANT-83 criterion 2's other half — the table is TOTAL over the causes the
// store can produce — and criterion 10's classification, which is the part of
// the internal split that needs no database.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/magos/catenary/internal/wire"
)

// Every cause this store can decide, and the refusal it gets. Adding a cause
// without adding a row here is the failure this test exists to catch: the code
// would still compile and the sender would be told `internal` for something
// that is not the server's fault.
func TestTheTableIsTotalOverTheCausesTheStoreCanProduce(t *testing.T) {
	for _, tc := range []struct {
		cause     error
		code      wire.ErrorCode
		retryable bool
	}{
		{ErrNotAMember, wire.ErrorCodeNotAMember, false},
		{ErrConversationNotFound, wire.ErrorCodeConversationNotFound, false},
		{ErrMessageTooLarge, wire.ErrorCodeMessageTooLarge, false},
		{ErrTooManyAttachments, wire.ErrorCodeMessageTooLarge, false},
		{ErrUploadNotFound, wire.ErrorCodeUploadNotFound, false},
		{ErrRateLimited, wire.ErrorCodeRateLimited, true},
	} {
		t.Run(string(tc.code)+"/"+tc.cause.Error(), func(t *testing.T) {
			got := SendErrorFor(tc.cause)
			if got.Code != tc.code {
				t.Errorf("code = %q, want %q", got.Code, tc.code)
			}
			if got.Retryable != tc.retryable {
				t.Errorf("retryable = %t, want %t", got.Retryable, tc.retryable)
			}
			if got.Code == wire.ErrorCodeInternal {
				t.Error("fell through to internal — the cause has no row in the table")
			}
			if !errors.Is(got, tc.cause) {
				t.Error("the cause is not reachable through errors.Is")
			}
		})
	}
}

// The store wraps its causes with context before returning them, so matching
// has to be errors.Is and not equality.
func TestAWrappedCauseStillResolves(t *testing.T) {
	err := fmt.Errorf("store: conversation %s: %w", "some-id", ErrNotAMember)
	if got := SendErrorFor(err); got.Code != wire.ErrorCodeNotAMember {
		t.Errorf("code = %q, want %q", got.Code, wire.ErrorCodeNotAMember)
	}
}

// sentByKey returns a BARE ErrNotFound when the row that just won a dedup race
// cannot be re-read. That is a server inconsistency, not a conversation the
// sender named wrongly, and telling the sender conversation_not_found would
// report our bug as their mistake.
func TestABareNotFoundIsNotConversationNotFound(t *testing.T) {
	if got := SendErrorFor(ErrNotFound); got.Code != wire.ErrorCodeInternal {
		t.Errorf("bare ErrNotFound → %q, want %q", got.Code, wire.ErrorCodeInternal)
	}
	if got := SendErrorFor(ErrConversationNotFound); got.Code != wire.ErrorCodeConversationNotFound {
		t.Errorf("ErrConversationNotFound → %q, want %q", got.Code, wire.ErrorCodeConversationNotFound)
	}
	// And the wrapping holds, so callers written against the older sentinel
	// keep working.
	if !errors.Is(ErrConversationNotFound, ErrNotFound) {
		t.Error("ErrConversationNotFound no longer wraps ErrNotFound")
	}
}

// safeToRetryErr wraps rather than embeds, so the error underneath stays
// reachable through errors.As. Embedding `error` promotes Error() but NOT
// Unwrap(), which makes anything inside invisible to errors.As — the first
// draft of TestANonTransientPgErrorIsNotRescuedByALaterBranch did exactly that
// and tested nothing.
type safeToRetryErr struct{ err error }

func (e safeToRetryErr) Error() string   { return e.err.Error() }
func (e safeToRetryErr) Unwrap() error   { return e.err }
func (safeToRetryErr) SafeToRetry() bool { return true }

type timeoutErr struct{ error }

func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// `internal` is not one cause. This is criterion 10, and the cases that matter
// are the ones carrying no SQLSTATE at all — a dropped pgx connection usually
// surfaces as io.EOF or a net.Error, which a SQLSTATE-only test never reaches.
func TestInternalSplitsOnTransience(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{"deadlock 40P01", &pgconn.PgError{Code: "40P01"}, true,
			"this file's lock order exists because deadlock is real, and Postgres resolves it by aborting a send"},
		{"serialization failure 40001", &pgconn.PgError{Code: "40001"}, true, "same shape as a deadlock"},
		{"connection exception class 08", &pgconn.PgError{Code: "08006"}, true, "the class the wire never sees twice"},
		{"unique violation 23505", &pgconn.PgError{Code: "23505"}, false,
			"the server considered the statement and refused it; the identical statement fails identically"},
		{"check violation 23514", &pgconn.PgError{Code: "23514"}, false, "a decision, not a hiccup"},
		{"admin shutdown 57P01 at FATAL", &pgconn.PgError{Code: "57P01", Severity: "FATAL"}, true,
			"pg_terminate_backend against the sending connection — criterion 10's own test — makes the NEXT statement return exactly this"},
		{"query cancelled 57014 at ERROR", &pgconn.PgError{Code: "57014", Severity: "ERROR"}, true,
			"statement_timeout, and also what a server-side drain cancels a query with; ERROR severity, so the class rule has to catch it"},
		{"cannot connect now 57P03", &pgconn.PgError{Code: "57P03", Severity: "FATAL"}, true, "the server is still starting"},
		{"too many connections 53300", &pgconn.PgError{Code: "53300", Severity: "FATAL"}, true,
			"insufficient resources is not now, not not ever"},
		{"a FATAL outside the three classes", &pgconn.PgError{Code: "28000", Severity: "FATAL"}, true,
			"pgx closes the connection on every FATAL, so whatever happens next happens on a fresh one"},
		{"a localised FATAL", &pgconn.PgError{Code: "57P01", Severity: "SCHWERWIEGEND", SeverityUnlocalized: "FATAL"}, true,
			"Severity is translated under a non-English lc_messages; SeverityUnlocalized is not"},
		{"io.EOF with no PgError", io.EOF, true, "the common shape of a dropped connection in pgx"},
		{"net.ErrClosed", net.ErrClosed, true, "a closed connection with nothing else attached"},
		{"a net.Error", timeoutErr{errors.New("dial tcp: i/o timeout")}, true,
			"nothing reached the server; this is also the shape of a refused connection, which arrives as *pgconn.ConnectError"},
		{"pgconn.SafeToRetry", safeToRetryErr{err: errors.New("write failed")}, true,
			"pgx proving the query never reached the server — and what the COMMIT after a terminated backend returns, as connLockError"},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true,
			"the canonical transient; client_id is what makes not knowing whether it landed free"},
		{"context.Canceled", context.Canceled, true,
			"a cancel has two possible authors: if the client cancelled nobody reads the flag, and if the SERVER cancelled during a drain then false parks a good message at failed forever"},
		{"a plain error", errors.New("something else"), false, "unknown is not transient"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SendErrorFor(tc.err)
			if got.Code != wire.ErrorCodeInternal {
				t.Fatalf("code = %q, want %q", got.Code, wire.ErrorCodeInternal)
			}
			if got.Retryable != tc.want {
				t.Errorf("retryable = %t, want %t — %s", got.Retryable, tc.want, tc.why)
			}
		})
	}
}

// A PgError that is not transient must not fall through to the
// connection-shaped branches and be rescued by one of them.
func TestANonTransientPgErrorIsNotRescuedByALaterBranch(t *testing.T) {
	// A 23505 that ALSO satisfies SafeToRetry, with the PgError reachable
	// through Unwrap. If the PgError branch fell through instead of returning,
	// SafeToRetry would rescue it and a permanent refusal would be reported as
	// a hiccup.
	err := safeToRetryErr{err: &pgconn.PgError{Code: "23505", Severity: "ERROR"}}
	if got := SendErrorFor(err); got.Retryable {
		t.Error("a unique violation was reported retryable — the PgError branch is falling through")
	}
}

// A code decided deeper in the call stack is not re-decided on the way out.
func TestAnAlreadyClassifiedErrorKeepsItsCode(t *testing.T) {
	inner := SendErrorFor(ErrNotAMember)
	wrapped := fmt.Errorf("store: send: %w", inner)
	if got := SendErrorFor(wrapped); got.Code != wire.ErrorCodeNotAMember {
		t.Errorf("code = %q, want %q — a caller re-decided a refusal", got.Code, wire.ErrorCodeNotAMember)
	}
}

// rate_limited is plumbed and unemitted, and RetryAfterSec is the field that
// exists for it. Pinned so the field is not deleted as dead weight before the
// policy that sets it arrives.
func TestRateLimitedCarriesNoRetryAfterUntilAPolicySetsOne(t *testing.T) {
	got := SendErrorFor(ErrRateLimited)
	if !got.Retryable {
		t.Error("rate_limited is the one refusal that is retryable by cause rather than by transience")
	}
	if got.RetryAfterSec != nil {
		t.Error("nothing decides retry_after_sec yet; no ticket on the board owns a limiter")
	}
	// *int64, matching ServerError.retry_after_sec on the wire. An int here
	// would need a conversion at the boundary — which is exactly the
	// translation SendError carrying wire types exists to avoid.
	var _ *int64 = got.RetryAfterSec
}
