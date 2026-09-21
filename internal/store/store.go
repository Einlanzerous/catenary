// Package store wires Catenary to its Postgres backend: connection pooling, an
// embedded in-process migrator, and the domain types the queries return.
//
// Mirrors the construct-server house pattern — no ORM, no external migration
// tool. Types sit beside the queries that return them rather than in a separate
// internal/model.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoClientID is returned by SendMessage when the idempotency key is the
// zero value. It is an error rather than a silent opt-out because a send that
// is not deduplicated, and does not say so, is how a bot double-posts.
var ErrNoClientID = errors.New("store: client_id is required")

// ErrNotFound is returned by every lookup that resolves nothing.
var ErrNotFound = errors.New("store: not found")

// Limits are the bounds a send is refused against. They live here rather than
// being read from the environment inside the store, because config is env-only
// and lives in internal/config — the store is handed its policy, it does not
// go looking for it.
type Limits struct {
	// MaxMessageBytes bounds the UTF-8 bytes of `text`, not its runes: the
	// column and the wire both count bytes, and a rune bound would refuse a
	// different set of messages than the one the database would.
	MaxMessageBytes int

	// MaxAttachments bounds how many attachments one send may carry, and it may
	// not exceed MaxAttachmentsCeiling.
	MaxAttachments int
}

// DefaultLimits is the single source for these two numbers. internal/config
// parses the overriding environment variables and does NOT restate the
// defaults, so there is nowhere for the two to drift apart.
func DefaultLimits() Limits {
	return Limits{
		// 16 KiB. Comfortably above anything a person types and far below the
		// point where a single row is a problem.
		MaxMessageBytes: 16384,
		MaxAttachments:  16,
	}
}

// MaxAttachmentsCeiling is the most CATENARY_MAX_ATTACHMENTS may be set to
// (CANT-85). The wire puts no maxItems on ClientSend.attachments, so without it
// the variable is unbounded, and TWO ceilings sit above the default:
//
//   - The socket's frame. cmd/catenary sizes it as 6 × MaxMessageBytes + 64 KiB,
//     and attachments ride in the 64 KiB — about 68 bytes of JSON each. Somewhere
//     under a thousand, a legal maximal send is severed with 1009 before the
//     store can answer, and a client whose outbox retries it reconnect-loops.
//     64 × 68 bytes is about 4.3 KiB.
//   - The extended protocol's 65,535 parameters. Position 11b writes every row
//     in one statement at 14 columns a row, so 4,681 rows is the most one
//     statement can carry, and above it pgx refuses client-side with an error
//     classified internal and not retryable. 64 × 14 is 896.
//
// A plain number rather than one derived from the parameter limit, because the
// derived one is the ceiling the socket crosses first. If a real need for more
// ever appears, the frame allowance moves with this, which is why both reasons
// are written here.
const MaxAttachmentsCeiling = 64

// LimitError is a bound Validate refuses. Field is the Limits field rather than
// an environment variable, because the store is handed its policy and does not
// know where it came from; the composition root, which does, names the source.
type LimitError struct {
	Field  string
	Value  int
	Reason string
}

// No package prefix: New and the composition root each add their own, and a
// reader of either should not see "store.New: store: ...".
func (e *LimitError) Error() string {
	return fmt.Sprintf("%s = %d: %s", e.Field, e.Value, e.Reason)
}

// Validate reports the first bound this store would refuse to run with. It is
// the ONE check: New panics on it, and the composition root calls it before
// opening the pool so an operator gets a startup error naming the variable
// rather than a panic trace after a database wait.
func (l Limits) Validate() error {
	// A non-positive bound is legal Go and silently inverts the check: a zero
	// MaxMessageBytes refuses every message carrying any text, and a zero
	// MaxAttachments every send carrying any attachment, with nothing anywhere
	// saying why.
	if l.MaxMessageBytes < 1 {
		return &LimitError{Field: "MaxMessageBytes", Value: l.MaxMessageBytes,
			Reason: "must be positive (a zero bound refuses every send rather than none)"}
	}
	if l.MaxAttachments < 1 {
		return &LimitError{Field: "MaxAttachments", Value: l.MaxAttachments,
			Reason: "must be positive (a zero bound refuses every send rather than none)"}
	}
	if l.MaxAttachments > MaxAttachmentsCeiling {
		return &LimitError{Field: "MaxAttachments", Value: l.MaxAttachments,
			Reason: fmt.Sprintf("must be at most %d (above it a maximal send outgrows the socket frame, "+
				"and then the one statement that writes the rows)", MaxAttachmentsCeiling)}
	}
	return nil
}

// Store is the query surface over Catenary's pool. Repos hang off it rather
// than off free functions so the growing set of queries has one place to live.
type Store struct {
	pool   *pgxpool.Pool
	limits Limits
	logger *slog.Logger

	// uploads resolves the upload handles a send names. RefuseUploads unless
	// WithUploadResolver said otherwise — CANT-85 Ruling 2 — so a store that
	// nobody wired refuses attachments loudly rather than dropping them.
	uploads UploadResolver

	// collisionFault breaks routeCollision on purpose, for CANT-125's two
	// negative controls. Zero in every Store the composition root builds:
	// nothing outside this package's tests can set it. PER STORE, not a
	// package variable — CANT-115 is what a shared mutable test knob costs.
	collisionFault collisionFault

	// personGuardFault breaks one of EnsurePerson, PersonByEmail or SetEmail's
	// own guards against reaching a bot, for CANT-130 criterion 6's negative
	// controls — collisionFault's own shape, for the same reason: zero in
	// every Store the composition root builds, and PER STORE rather than a
	// package variable.
	personGuardFault personGuardFault
}

// New wraps an existing pool.
//
// Limits is a REQUIRED parameter rather than an option with a fallback. An
// optional bound is a bound the composition root forgets to wire, and the
// failure is silent — sends stop being refused and nothing says so. Callers
// that do not care pass DefaultLimits().
//
// So is logger, for the same reason and one more. Every refusal logs once
// (CANT-83), and a store that silently discards those is a store whose refusals
// are invisible in production — which is the failure mode the structured-log
// house rule exists to prevent. A caller that genuinely wants them dropped says
// so: slog.New(slog.DiscardHandler).
func New(pool *pgxpool.Pool, limits Limits, logger *slog.Logger) *Store {
	// PANICS on a non-positive bound, because the zero value is legal Go and
	// silently inverts the check: `New(pool, Limits{})` compiles and then
	// refuses every message carrying any text as message_too_large, and every
	// send carrying any attachment, with nothing anywhere saying why.
	//
	// The doc above argues that an optional bound fails silently by not
	// refusing. That is true, and the zero value fails silently in the other
	// and worse direction — a service that accepts nothing looks like a
	// service that is down. config.positiveInt already refuses 0 for both
	// environment variables for the same reason, so this cannot fire from a
	// real configuration; it fires when something constructs a Store by hand.
	//
	// A panic rather than an error return: New has no error result and ~90
	// call sites, this is a wiring mistake rather than a runtime condition,
	// and it happens once at composition where a dead process is the clearest
	// possible signal.
	//
	// Limits.Validate is the check, and since CANT-85 it also refuses
	// MaxAttachments above MaxAttachmentsCeiling. The composition root runs
	// the same Validate before opening the pool, so this still cannot fire
	// from a real configuration.
	if err := limits.Validate(); err != nil {
		panic(fmt.Sprintf("store.New: %v (pass DefaultLimits() if you do not care)", err))
	}
	// Nil for the same reason, and it is the cheaper mistake to make: a nil
	// *slog.Logger panics at the first refusal rather than at composition, so
	// the crash arrives in the middle of somebody's send instead of at boot.
	if logger == nil {
		panic("store.New: logger must not be nil (pass slog.New(slog.DiscardHandler) to drop refusal logs deliberately)")
	}
	return &Store{pool: pool, limits: limits, logger: logger, uploads: RefuseUploads{}}
}

// Limits reports the bounds this store enforces.
func (s *Store) Limits() Limits { return s.limits }

// Pool exposes the underlying pool for the migrator and for tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping satisfies api.Pinger, which is what /readyz calls.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Connect opens a pgx pool for dsn and verifies it is reachable with a Ping.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: new pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return pool, nil
}

// ConnectWithRetry opens the pool, retrying transient failures with exponential
// backoff up to maxWait.
//
// Catenary shares one Postgres with the rest of the estate, so a restart of
// that container is an ordinary event rather than an incident. Riding it out
// beats crash-looping — and crash-looping is worse here than elsewhere,
// because every restart severs every open WebSocket at once and the clients
// all reconnect together. A genuinely bad DSN still fails, just after the
// budget rather than instantly.
func ConnectWithRetry(ctx context.Context, dsn string, maxWait time.Duration) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(maxWait)
	backoff := 250 * time.Millisecond

	var lastErr error
	for {
		pool, err := Connect(ctx, dsn)
		if err == nil {
			return pool, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().Add(backoff).After(deadline) {
			return nil, fmt.Errorf("store: unreachable after %s: %w", maxWait, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
}
