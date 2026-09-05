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

// Store is the query surface over Catenary's pool. Repos hang off it rather
// than off free functions so the growing set of queries has one place to live.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an existing pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

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
