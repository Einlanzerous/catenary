package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
)

// runProvision is CANT-109's one-off device: create a single new account
// through the same direct insert `soak` already uses (insertUser), mint its
// enrollment token through the real store function, and redeem it over the
// real POST /enroll at baseURL — mintAndRedeem, the one path this package
// uses for both a soak run's N accounts and this single one. It refuses a
// handle that already exists rather than reusing the account: this command
// provisions exactly one NEW account per call, never a re-invite, which is
// IssueEnrollmentToken's own re-issue case and not this one.
//
// The database connection is this process's own, exactly as `soak`'s is —
// CANT-109's description says so explicitly — and it is closed before this
// returns; nothing here keeps a pool open past one provisioning run.
func runProvision(ctx context.Context, dbURL, baseURL, handle, displayName, credFile string, headers http.Header, logger *slog.Logger) (Credentials, error) {
	pool, err := store.ConnectWithRetry(ctx, dbURL, 30*time.Second)
	if err != nil {
		return Credentials{}, fmt.Errorf("connect to the database: %w", err)
	}
	defer pool.Close()
	st := store.New(pool, store.DefaultLimits(), logger)

	userID := uuid.New()
	if err := insertUser(ctx, pool, userID, handle, displayName); err != nil {
		if isUniqueViolation(err) {
			return Credentials{}, fmt.Errorf("handle %q already exists — provision creates a new account only, and never reuses one", handle)
		}
		return Credentials{}, fmt.Errorf("create user: %w", err)
	}

	enrolled, err := mintAndRedeem(ctx, st, baseURL, headers, userID, displayName)
	if err != nil {
		return Credentials{}, fmt.Errorf("enroll the new device: %w", err)
	}

	cred, err := credentialsFromEnrollment(enrolled)
	if err != nil {
		return Credentials{}, fmt.Errorf("read back the new enrollment: %w", err)
	}
	if err := writeCredentials(credFile, cred); err != nil {
		return Credentials{}, err
	}
	return cred, nil
}
