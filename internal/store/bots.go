package store

// CANT-73 — the credential that is not a device.
//
// ITS OWN FILE rather than more of tokens.go, because every query here is
// shaped by the one fact that separates a bot from every other caller: it owns
// no device. A `users` row that says `bot`, an access token with no device and
// therefore no expiry, and a revoke that names the account rather than a phone.
// tokens.go holds the four credential shapes and the seam that resolves them;
// this holds the operator surface that mints and ends one of them.
//
// NOTHING HERE IS REACHABLE FROM internal/api, and that is deliberate. Bots are
// minted from the admin surface behind Cloudflare Access (CANT-69) or the
// `catenary bot` subcommand, never by Purser — which provisions people — and
// never by any signup path.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// trimHandle normalizes what an operator typed, and deliberately does NOT fold
// case. `users.handle` is a plain UNIQUE TEXT column — there is no citext and no
// lower(handle) index — and no other write path in this service folds case.
// Folding only here would invent a uniqueness rule the schema does not enforce:
// `bot create Argosy` would insert `argosy`, and a handle stored as `Argosy` by
// any other path would then be invisible to BotByHandle. Trim, which fixes a
// stray space, and leave identity to the column that owns it.
func trimHandle(h string) string { return strings.TrimSpace(h) }

// ErrHandleTaken means the handle is already somebody's — possibly a person's.
//
// Distinct from a generic failure because it is the one outcome an operator
// causes by typing, and because the row it collided with is NOT overwritten:
// see CreateBot on why a person can never be turned into a bot by retyping
// their handle.
var ErrHandleTaken = errors.New("store: handle already taken")

// ErrNoSuchBot means no `users` row with that handle is a bot — it does not
// exist, or it is a person. One answer for both, because the difference cannot
// change what the operator does next: neither is a bot they can revoke.
var ErrNoSuchBot = errors.New("store: no bot with that handle")

// BotRow is one service account as the operator surface lists it.
//
// LiveTokens is counted rather than stored, on invariant 3's rule: a stored
// count and the rows it counts are two things that can disagree, and the
// question "does this bot still have a working credential" must not have two
// answers.
type BotRow struct {
	UserID      uuid.UUID
	Handle      string
	DisplayName string
	CreatedAt   time.Time
	LiveTokens  int
}

// CreateBot adds the `users` row that says a caller is a service account.
//
// THE INSERT IS THE ONLY AUTHORITY, and the conflict arm is why. A read-then-
// insert would decide on a handle's availability in one statement and act on it
// in another, and the gap between them is where two operators running
// `bot create` on the same handle both win. `ON CONFLICT DO NOTHING RETURNING`
// collapses that to a single statement whose zero-row result IS the refusal —
// the same shape CANT-29's rotation uses, for the same reason.
//
// A PERSON IS NEVER CONVERTED. The conflict arm does nothing rather than
// updating `kind`, so running this against an existing person's handle refuses
// and leaves them a person. That matters more than it looks: `kind = 'bot'` is
// what lets IssueBotToken mint a credential that never expires, so an UPSERT
// here would be a path from "typed a handle that was already taken" to "handed
// somebody's account a non-expiring token".
func (s *Store) CreateBot(ctx context.Context, handle, displayName string) (uuid.UUID, error) {
	handle = trimHandle(handle)
	if handle == "" {
		return uuid.Nil, errors.New("store: create bot: a handle is required")
	}
	if displayName = strings.TrimSpace(displayName); displayName == "" {
		// A display name is NOT NULL and is what renders beside the bot's
		// messages; falling back to the handle beats refusing an operator who
		// had nothing else to say.
		displayName = handle
	}

	id := uuid.New()
	var got uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (id, handle, display_name, kind)
		VALUES ($1, $2, $3, 'bot')
		ON CONFLICT (handle) DO NOTHING
		RETURNING id`, id, handle, displayName).Scan(&got)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, ErrHandleTaken
	case err != nil:
		return uuid.Nil, fmt.Errorf("store: create bot: %w", err)
	}

	s.logger.InfoContext(ctx, "bot created", "user_id", got, "handle", handle)
	return got, nil
}

// BotByHandle resolves the name an operator types to the account it names.
//
// The `kind` predicate is in the WHERE clause rather than checked after, so a
// person's handle returns no row instead of a row this package would then have
// to remember to refuse.
func (s *Store) BotByHandle(ctx context.Context, handle string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE handle = $1 AND kind = 'bot'`,
		trimHandle(handle)).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, ErrNoSuchBot
	case err != nil:
		return uuid.Nil, fmt.Errorf("store: bot by handle: %w", err)
	}
	return id, nil
}

// Bots lists every service account, oldest first, with how many live
// credentials each still holds.
//
// The join is scoped to `device_id IS NULL` so the count is of bot tokens
// specifically. A bot has no device and so can hold no other kind, but writing
// the predicate keeps the count true rather than true-by-coincidence.
func (s *Store) Bots(ctx context.Context) ([]BotRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.handle, u.display_name, u.created_at,
		       count(a.id) FILTER (WHERE a.revoked_at IS NULL) AS live
		  FROM users u
		  LEFT JOIN access_tokens a ON a.user_id = u.id AND a.device_id IS NULL
		 WHERE u.kind = 'bot'
		 GROUP BY u.id, u.handle, u.display_name, u.created_at
		 ORDER BY u.created_at, u.handle`)
	if err != nil {
		return nil, fmt.Errorf("store: bots: %w", err)
	}
	defer rows.Close()

	out := make([]BotRow, 0, 8)
	for rows.Next() {
		var b BotRow
		if err := rows.Scan(&b.UserID, &b.Handle, &b.DisplayName, &b.CreatedAt, &b.LiveTokens); err != nil {
			return nil, fmt.Errorf("store: bots: scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: bots: %w", err)
	}
	return out, nil
}

// RevokeBotTokens ends every live credential a service account holds, and
// returns how many it ended.
//
// A COUNT RATHER THAN A BOOL, because a bot may have been minted more than
// once — re-minting is how a bot token is rotated, since it does not rotate
// itself — and "revoke the bot" must mean all of them rather than the newest.
// Zero is not an error: a retry finding its work already done is success, the
// same contract RevokeDevice states.
//
// ONE STATEMENT, AND THE SCOPING IS THE AUTHORITY — but it is a PAIR, and which
// half carries it was measured rather than assumed. `u.kind = 'bot'` and
// `a.device_id IS NULL` are MUTUALLY REDUNDANT: removing either one alone breaks
// no test, because each excludes a person by itself — a person's access token
// always carries a device, and a person is never kind = 'bot'. Removing BOTH
// fails TestRevokingBotTokensCannotReachAPersonsCredentials at once, with a
// person's live session ended by their own user id.
//
// That is worth stating in both directions, because the obvious reading is
// wrong in a dangerous way. A reader who deletes the "clearly duplicated"
// predicate gets a green suite and has removed half of the only thing standing
// between a user id and somebody else's session. A one-at-a-time control cannot
// tell redundancy from uselessness here; the both-at-once control is the only
// one that means anything, and it is the one to re-run if this clause is ever
// edited.
//
// `revoked_at IS NULL` is separate from the scoping, and is what makes a second
// call a no-op rather than a re-dating of the first.
//
// NO NOTIFICATION IS PUBLISHED, and that is a decision rather than an omission.
// RevokeDevice publishes on RevocationChannel because a revoked phone may be
// holding a live socket that must be severed inside the same transaction. A bot
// cannot be: internal/api/socket.go refuses a bot the upgrade at the door —
// "a bot has no device to bind" — so Hub.OnRevocation's UserID branch, which
// does exist and would walk this account's sessions, would always find none.
// Publishing anyway would wake every listener in the deployment to sever
// nothing and log `sessions_severed=0`, which reads as a bug the first time
// somebody greps for it. This is 0007's own rule about the family_id index: a
// mechanism nobody needs yet is a mechanism nobody later dares remove. If a bot
// ever gains a socket, the publish belongs to that change, with its own test.
//
// Revocation takes effect on the NEXT REQUEST rather than at an expiry, because
// Authenticate reads access_tokens.revoked_at on every call — one query across
// three tables, with no cached answer to go stale in between.
func (s *Store) RevokeBotTokens(ctx context.Context, userID uuid.UUID) (int, error) {
	// now() is computed database-side rather than passed in. CANT-29's reuse
	// detector was written the other way first, and a host clock ahead of
	// Postgres silently disabled it; there is no reason to reintroduce a
	// cross-host comparison for a column only Postgres ever reads.
	rows, err := s.pool.Query(ctx, `
		UPDATE access_tokens a SET revoked_at = now()
		  FROM users u
		 WHERE u.id = a.user_id
		   AND a.user_id = $1
		   AND u.kind = 'bot'
		   AND a.device_id IS NULL
		   AND a.revoked_at IS NULL
		 RETURNING a.id`, userID)
	if err != nil {
		return 0, fmt.Errorf("store: revoke bot tokens: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: revoke bot tokens: %w", err)
	}

	if n > 0 {
		s.logger.InfoContext(ctx, "bot tokens revoked", "user_id", userID, "tokens_revoked", n)
	}
	return n, nil
}
