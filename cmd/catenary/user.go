package main

// CANT-130 — `catenary user set-email`, the one-time adoption path for a
// person `soakrig provision` created with no email address to key on.
//
// A CLI, ON bot.go's OWN SHAPE, AND FOR THE SAME REASON: it is a one-time
// operator act, run against the handful of people the deployed database
// holds before Purser and migration 0010 exist together, and it is not
// Purser's — Purser identifies a person by email from the moment it ever
// hears of them, and never needs to adopt one. A person EnsurePerson creates
// has an email from the moment it exists and never needs this either.
//
// Nothing here is reachable from internal/api, on CreateBot's own argument:
// this is a host command, not a signup path.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/store"
)

func runUser(args []string) error {
	if len(args) == 0 {
		return errors.New("user: expected set-email")
	}

	// THE ACTION IS CHECKED BEFORE ANYTHING IS LOADED OR DIALLED, on bot.go's
	// own argument: a typo must not cost a database connection.
	switch args[0] {
	case "set-email":
	default:
		return fmt.Errorf("user: unknown action %q (want set-email)", args[0])
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := cfg.Logger(os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.ConnectWithRetry(ctx, cfg.DatabaseURL, dbConnectBudget)
	if err != nil {
		return err
	}
	defer pool.Close()

	// DefaultLimits, on botCreate's own reasoning: nothing this subcommand
	// calls sends a message, so the bounds are never read, but a Store must
	// still be constructed legally.
	st := store.New(pool, store.DefaultLimits(), logger)

	return userSetEmail(ctx, st, args[1:])
}

// userSetEmail is SetEmail's CLI wiring: the store's own tests are the
// oracle for every refusal below, so this only has to name each one for an
// operator reading a terminal.
func userSetEmail(ctx context.Context, st *store.Store, args []string) error {
	if len(args) < 2 {
		return errors.New("user set-email: expected a handle and an email")
	}
	handle, email := args[0], args[1]

	id, err := st.SetEmail(ctx, handle, email)
	switch {
	case errors.Is(err, store.ErrNoSuchUser):
		return fmt.Errorf("user set-email: no account with the handle %q", handle)
	case errors.Is(err, store.ErrCannotEmailBot):
		return fmt.Errorf("user set-email: %q is a bot; a bot never holds an email", handle)
	case errors.Is(err, store.ErrPersonAlreadyHasEmail):
		return fmt.Errorf("user set-email: %q already has an email; it was not changed", handle)
	case errors.Is(err, store.ErrEmailTaken):
		return fmt.Errorf("user set-email: %q is already somebody's email", email)
	case errors.Is(err, store.ErrInvalidEmail):
		return fmt.Errorf("user set-email: %q is not a valid email", email)
	case err != nil:
		return err
	}

	fmt.Fprintf(os.Stderr, "%s (%s) now has the email %s\n", handle, id, email)
	return nil
}
