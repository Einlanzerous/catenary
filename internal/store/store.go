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

	// MaxAttachments bounds how many attachments one send may carry.
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

// Store is the query surface over Catenary's pool. Repos hang off it rather
// than off free functions so the growing set of queries has one place to live.
type Store struct {
	pool   *pgxpool.Pool
	limits Limits
}

// New wraps an existing pool.
//
// Limits is a REQUIRED parameter rather than an option with a fallback. An
// optional bound is a bound the composition root forgets to wire, and the
// failure is silent — sends stop being refused and nothing says so. Callers
// that do not care pass DefaultLimits().
func New(pool *pgxpool.Pool, limits Limits) *Store {
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
	// A panic rather than an error return: New has no error result and ~20
	// call sites, this is a wiring mistake rather than a runtime condition,
	// and it happens once at composition where a dead process is the clearest
	// possible signal.
	if limits.MaxMessageBytes < 1 || limits.MaxAttachments < 1 {
		panic(fmt.Sprintf("store.New: limits must be positive, got MaxMessageBytes=%d MaxAttachments=%d "+
			"(a zero bound refuses every send rather than none; pass DefaultLimits() if you do not care)",
			limits.MaxMessageBytes, limits.MaxAttachments))
	}
	return &Store{pool: pool, limits: limits}
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
