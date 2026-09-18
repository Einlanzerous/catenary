package main

// CANT-73 — the operator surface that mints and ends a service account.
//
// A CLI RATHER THAN A ROUTE, for now. The ticket allows either the admin page
// behind Cloudflare Access (CANT-69) or this; this is the half that can exist
// before that surface does, and it is the half an operator can run against a
// database with no HTTP listener at all — which is the case during a restore or
// a first deploy, when there is no working credential to authenticate an admin
// call with.
//
// Nothing here is reachable from internal/api. A bot is minted by an operator
// on the host, never by Purser, which provisions people, and never by a signup
// path, of which this service has none.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/magos/catenary/internal/config"
	"github.com/magos/catenary/internal/store"
)

func runBot(args []string) error {
	if len(args) == 0 {
		return errors.New("bot: expected create, token, list or revoke")
	}

	// THE ACTION IS CHECKED BEFORE ANYTHING IS LOADED OR DIALLED. A typo must
	// not cost a database connection, and more to the point `catenary bot
	// destroy` should say what is wrong with it rather than failing first on a
	// missing DATABASE_URL — which is the error the operator would then go and
	// try to fix, having been told nothing about the real mistake.
	switch args[0] {
	case "create", "token", "list", "revoke":
	default:
		return fmt.Errorf("bot: unknown action %q (want create, token, list or revoke)", args[0])
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := cfg.Logger(os.Stdout)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.ConnectWithRetry(ctx, cfg.DatabaseURL, dbConnectBudget)
	if err != nil {
		return err
	}
	defer pool.Close()

	// DefaultLimits rather than the configured ones, and the reason is that
	// store.New panics on a zero Limits rather than silently refusing every
	// message. Nothing this subcommand calls sends a message, so the bounds are
	// never read — but a Store must still be constructed legally.
	st := store.New(pool, store.DefaultLimits(), logger)

	switch args[0] {
	case "create":
		return botCreate(ctx, st, args[1:])
	case "token":
		return botToken(ctx, st, args[1:])
	case "list":
		return botList(ctx, st)
	case "revoke":
		return botRevoke(ctx, st, args[1:])
	default:
		return fmt.Errorf("bot: unknown action %q (want create, token, list or revoke)", args[0])
	}
}

// botCreate makes the account and mints its first credential.
//
// TWO STATEMENTS, NOT ONE TRANSACTION, and `bot token` is what makes that safe.
// If the mint fails after the row lands, re-running `create` would refuse the
// handle as taken and leave the operator with a bot that has no credential and
// no way to get one. `token` is that way, and it is also how a bot token is
// rotated — a bot token does not rotate itself, by construction.
func botCreate(ctx context.Context, st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("bot create: expected a handle")
	}
	handle := args[0]
	displayName := ""
	if len(args) > 1 {
		displayName = args[1]
	}

	id, err := st.CreateBot(ctx, handle, displayName)
	switch {
	case errors.Is(err, store.ErrHandleTaken):
		// Named rather than wrapped in a generic failure, because the row it
		// collided with may be a PERSON, and the operator needs to know that
		// retrying with force is not a thing they want.
		return fmt.Errorf("bot create: the handle %q already belongs to an account; "+
			"it was not modified", handle)
	case err != nil:
		return err
	}

	issued, err := st.IssueBotToken(ctx, id)
	if err != nil {
		return fmt.Errorf("bot create: %s was created but has no token yet — "+
			"run `catenary bot token %s`: %w", handle, handle, err)
	}

	printToken(handle, id, issued)
	return nil
}

// botToken mints a fresh credential for an account that already exists.
func botToken(ctx context.Context, st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("bot token: expected a handle")
	}
	handle := args[0]

	id, err := st.BotByHandle(ctx, handle)
	if errors.Is(err, store.ErrNoSuchBot) {
		return fmt.Errorf("bot token: no bot with the handle %q", handle)
	} else if err != nil {
		return err
	}

	issued, err := st.IssueBotToken(ctx, id)
	if err != nil {
		return err
	}

	// The OLD token is left live on purpose. Minting is not revoking: a bot
	// being rotated needs a window in which both credentials work, or the
	// rotation is an outage. `bot revoke` ends the old one once the new one is
	// deployed, and `bot list` shows how many are outstanding meanwhile.
	printToken(handle, id, issued)
	return nil
}

// printToken writes the credential to stdout and everything else to stderr.
//
// THE SPLIT IS THE POINT. `catenary bot create argosy > /tmp/tok` must leave the
// token and nothing else in the file, so it can be piped straight into Signet
// without a human copying it out of prose — and a credential that a human has
// to select with a mouse is a credential that ends up in a scrollback buffer.
func printToken(handle string, id any, issued store.IssuedToken) {
	fmt.Fprintf(os.Stderr, "bot %s (%v)\n", handle, id)
	fmt.Fprintf(os.Stderr, "the token is shown ONCE and is not recoverable — store it in Signet now\n")
	if issued.ExpiresAt.IsZero() {
		fmt.Fprintf(os.Stderr, "it does not expire; end it with `catenary bot revoke %s`\n", handle)
	}
	fmt.Println(issued.Plaintext)
}

func botList(ctx context.Context, st *store.Store) error {
	bots, err := st.Bots(ctx)
	if err != nil {
		return err
	}
	if len(bots) == 0 {
		fmt.Fprintln(os.Stderr, "no service accounts")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HANDLE\tNAME\tLIVE TOKENS\tCREATED\tID")
	for _, b := range bots {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
			b.Handle, b.DisplayName, b.LiveTokens, b.CreatedAt.UTC().Format("2006-01-02"), b.UserID)
	}
	return w.Flush()
}

func botRevoke(ctx context.Context, st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("bot revoke: expected a handle")
	}
	handle := args[0]

	id, err := st.BotByHandle(ctx, handle)
	if errors.Is(err, store.ErrNoSuchBot) {
		return fmt.Errorf("bot revoke: no bot with the handle %q", handle)
	} else if err != nil {
		return err
	}

	n, err := st.RevokeBotTokens(ctx, id)
	if err != nil {
		return err
	}
	// Zero is reported rather than treated as a failure: a retry finding its
	// work already done is success, and an operator who runs this twice during
	// an incident should not be told something went wrong.
	switch n {
	case 0:
		fmt.Fprintf(os.Stderr, "%s had no live token; nothing to revoke\n", handle)
	case 1:
		fmt.Fprintf(os.Stderr, "revoked 1 token for %s; it stops working on its next request\n", handle)
	default:
		fmt.Fprintf(os.Stderr, "revoked %d tokens for %s; they stop working on their next request\n", n, handle)
	}
	return nil
}
