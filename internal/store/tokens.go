package store

// CANT-28 — the four credential shapes, and the one function that answers
// "who is asking".
//
// THE MODEL IS FOUR SHAPES AND NOT TWO. The ticket names short-lived access
// and per-device rotating refresh; R6 settled the bootstrap ENROLLMENT token
// before this ticket existed, and a recorded ruling settled the BOT token. All
// four are minted here, in one encoding, and verified through one seam.
//
// WHAT IS NOT HERE. The refresh-for-access exchange is a sub-task of CANT-29
// filed review_mode: full (ruling 5), so that rotation gets the same
// line-by-line read as the reuse detection built over it. Its wire types are
// generated from the schema already; its handler and its conditional write are
// not in this file. The columns it writes — family_id, replaced_by — are.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrUnauthorized is every authentication failure, and there is deliberately
// only one of it.
//
// ONE REFUSAL SHAPE, GRANULAR ONLY IN THE LOG. `/enroll` is unauthenticated and
// has no rate limiter in front of it by decision, so a response that told an
// unknown token from an already-redeemed one would tell a prober which of its
// guesses was once real, and a distinct answer for a deactivated account would
// confirm that the account exists. The reasons are distinguished where they are
// useful and invisible where they are dangerous: every refusal below logs what
// it was and returns this.
var ErrUnauthorized = errors.New("store: unauthorized")

// ErrDeviceNameRequired is a redemption with nothing to call the device.
//
// A SEPARATE ERROR, AND NOT A SECOND AUTHENTICATION FAILURE. It is the
// caller's own malformed request rather than a credential that did not check
// out, so collapsing it into ErrUnauthorized would tell a client with a typo
// that its token was rejected. Nothing is leaked by saying so: the name is in
// the request the sender just wrote.
var ErrDeviceNameRequired = errors.New("store: device_name is required")

// The three lifetimes, as CANT-28 ruling 3 settled them.
//
// CONSTANTS RATHER THAN CONFIG, and the difference from Limits is the point:
// MaxMessageBytes is a deployment bound that internal/config can override, and
// these are a security posture the plan argued and a human picked. An
// environment variable here would be a way to quietly lengthen a lifetime
// without anybody re-reading the argument for it.
const (
	// AccessTokenLifetime is short because a leaked access token is useful
	// until it expires. It is NOT the revocation bound — ruling 0 makes
	// verification an indexed point-read, so a revoked credential stops
	// working on its next request whatever this says. Fifteen minutes buys
	// damage-limitation on a leak nobody noticed, which is the only thing it
	// is being asked to buy.
	AccessTokenLifetime = 15 * time.Minute

	// RefreshTokenLifetime is long enough that a phone in normal use never
	// re-authenticates, and short enough that a device forgotten in a drawer
	// falls out of the account within a quarter — which is the case device
	// revocation exists for and the one people forget to perform.
	RefreshTokenLifetime = 60 * 24 * time.Hour

	// EnrollmentTokenLifetime is the asymmetric one. Re-issue is free and R6
	// classes it as a rotation Provision may perform, so a too-short lifetime
	// costs one command and a too-long one leaves a standing key to an account
	// sitting in an old chat log. A week survives a weekend.
	EnrollmentTokenLifetime = 7 * 24 * time.Hour
)

// TokenBytes is the entropy behind every credential this service issues.
//
// ONE LENGTH FOR ALL FOUR SHAPES. A shape-specific length would leak which
// kind of credential a string is to anyone who saw one, and it would give the
// enrollment token — the only one a person ever handles — its own quiet
// pressure to be shortened.
const TokenBytes = 32

// MintToken returns a new credential and the hash to store for it.
//
// THE PLAINTEXT IS RETURNED EXACTLY ONCE AND NEVER STORED. Everything below
// writes the hash; the string itself exists in the response that carries it
// and nowhere else, which is what makes a database copy something other than a
// key ring.
//
// base64url WITHOUT padding, because CANT-28 ruling 1 puts the access token on
// Sec-WebSocket-Protocol — the only request header a browser can set on a
// WebSocket — and that header carries RFC 6455 subprotocol names, each an RFC
// 7230 `token`. `/` and `=` are NOT legal in one; `+` is, which is exactly why
// the rule is stated as an alphabet rather than as a list of characters to
// avoid. Standard base64 produces `+`, `/` and, padded, `=`; base64url's `-`
// and `_` sidestep the question entirely. An intermediary is entitled to
// mangle an illegal value rather than refuse it, which is a bug debugged from
// a browser console at the far end of CANT-22.
//
// The wire schema's `Token` pattern is that alphabet, so all three generated
// decoders enforce the same rule and neither client can get it wrong quietly.
func MintToken() (plaintext string, hash []byte, err error) {
	raw := make([]byte, TokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("store: mint token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(raw)
	return plaintext, HashToken(plaintext), nil
}

// HashToken is how a presented credential becomes a lookup key.
//
// SHA-256 AND NOT A PASSWORD KDF, DELIBERATELY. Argon2 or bcrypt is the right
// answer for something a person chose and the wrong one for these. A token is
// 32 bytes from a CSPRNG, so there is no dictionary and no candidate set for a
// slow hash to slow an attacker down over — and ruling 0 makes verification an
// indexed point-read on every authenticated request, which a per-row salt
// makes impossible. The entropy is the defence. The hash is only here so that
// a leaked database is not a set of working credentials.
func HashToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// tokensEqual compares two hashes without a timing signal.
//
// Belt and braces over an indexed equality lookup, which already leaks nothing
// useful: it is here so that a later caller comparing hashes in Go does not
// have to notice the question.
func tokensEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// Caller is who a credential resolves to.
//
// DeviceID is uuid.Nil for a bot, which has none and must not pretend to. That
// is not a missing value: it is the distinction between a credential that can
// be revoked as a phone and one that can only be revoked as itself.
type Caller struct {
	UserID   uuid.UUID
	DeviceID uuid.UUID
	Kind     string
}

// IsBot reports whether this caller is a service account.
func (c Caller) IsBot() bool { return c.Kind == "bot" }

// IssuedToken is a credential as its holder receives it: once, in plaintext,
// with the moment it stops working.
//
// ExpiresAt is served rather than left for a client to compute from a lifetime
// it hard-codes, which is how the two drift apart the first time a lifetime
// changes. A bot token's is the zero time, meaning it does not expire.
type IssuedToken struct {
	Plaintext string
	ExpiresAt time.Time
}

// Enrollment is everything a new install learns at redemption.
type Enrollment struct {
	UserID   uuid.UUID
	DeviceID uuid.UUID
	Access   IssuedToken
	Refresh  IssuedToken
}

// MaxDeviceNameBytes bounds what a device may call itself.
//
// Bounded HERE rather than in the wire schema, and the reason is worth
// recording: the generators enforce `pattern`, `minimum` and `maximum` and
// would silently ignore a `minLength`. A schema constraint that no decoder
// checks is worse than no constraint, because in review it reads as protection.
const MaxDeviceNameBytes = 128

// Authenticate resolves a presented credential to its caller, and is the ONLY
// place this service decides that a request is allowed to proceed.
//
// ONE SEAM, FOUR REFUSALS. A rule enforced in three handlers is a rule that is
// enforced in two of them a quarter from now — the same argument CANT-83 makes
// for having one error-code table. CallerID on the router is a thin adapter
// over this, and CANT-22's upgrade calls the same function.
//
// THE DEACTIVATED-USER CHECK IS NOT AN EXTRA. R6 chose disable-then-revoke for
// Purser's offboard over the reverse ordering, and the whole argument rests on
// one sentence: "a disabled account cannot refresh a token or enroll a device".
// That is a claim about this function. If only devices.revoked_at were checked,
// R6's deliberately-accepted half-done state — a disabled account with some
// devices still un-revoked — would keep authenticating, and the offboard would
// fail open in exactly the way the rejected ordering was rejected for.
//
// Before this ticket, no query in this service read users.deactivated_at.
func (s *Store) Authenticate(ctx context.Context, presented string) (Caller, error) {
	if presented == "" {
		// DEBUG, NOT WARN, AND THIS IS THE ONE CASE THAT GETS THE QUIETER
		// LEVEL. Every other refusal below is somebody presenting something;
		// this is somebody presenting nothing, which the request log already
		// records as a 401 on an authenticated route. At WARN it would be a
		// second line per anonymous probe on a service with no rate limiter,
		// which is a log-volume lever anyone on the internet could pull.
		s.logger.DebugContext(ctx, "authentication refused", "reason", "no credential presented")
		return Caller{}, ErrUnauthorized
	}

	// Hashed once. The lookup and the comparison below must be the same bytes,
	// and computing it twice is how they would stop being.
	presentedHash := HashToken(presented)

	var (
		tokenID       uuid.UUID
		c             Caller
		deviceID      *uuid.UUID
		expiresAt     *time.Time
		tokenRevoked  *time.Time
		deviceRevoked *time.Time
		deactivated   *time.Time
		storedHash    []byte
	)
	// ONE QUERY, THREE TABLES. The join is what makes revocation immediate:
	// devices.revoked_at and users.deactivated_at are read on the same request
	// that reads the token, so there is no cached answer to go stale between a
	// revocation and the next call.
	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.token_hash, a.user_id, a.device_id, a.expires_at, a.revoked_at,
		       d.revoked_at, u.deactivated_at, u.kind
		  FROM access_tokens a
		  JOIN users u ON u.id = a.user_id
		  LEFT JOIN devices d ON d.id = a.device_id
		 WHERE a.token_hash = $1`,
		presentedHash).
		Scan(&tokenID, &storedHash, &c.UserID, &deviceID, &expiresAt, &tokenRevoked,
			&deviceRevoked, &deactivated, &c.Kind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The token id is unknown here by definition, so there is nothing to
		// name but the fact. The credential itself is never logged.
		s.logger.WarnContext(ctx, "authentication refused", "reason", "unknown credential")
		return Caller{}, ErrUnauthorized
	case err != nil:
		return Caller{}, fmt.Errorf("store: authenticate: %w", err)
	}

	// The lookup was an equality match on an indexed column, so this can only
	// fail if the index and the row disagree. Checked anyway, in constant time,
	// so that the comparison exists in the one place a reader looks for it.
	if !tokensEqual(storedHash, presentedHash) {
		s.logger.WarnContext(ctx, "authentication refused", "reason", "hash mismatch", "token_id", tokenID)
		return Caller{}, ErrUnauthorized
	}

	now := ServerTime()
	switch {
	case tokenRevoked != nil:
		s.logger.WarnContext(ctx, "authentication refused", "reason", "token revoked", "token_id", tokenID)
		return Caller{}, ErrUnauthorized
	// NULL expires_at means "never", and 0007's CHECK ties it to a NULL
	// device_id so only a bot's token can hold one. A person's fifteen minutes
	// is never one bad INSERT away from forever.
	case expiresAt != nil && !expiresAt.After(now):
		s.logger.WarnContext(ctx, "authentication refused", "reason", "token expired", "token_id", tokenID)
		return Caller{}, ErrUnauthorized
	case deviceRevoked != nil:
		s.logger.WarnContext(ctx, "authentication refused", "reason", "device revoked", "token_id", tokenID, "device_id", deviceID)
		return Caller{}, ErrUnauthorized
	case deactivated != nil:
		s.logger.WarnContext(ctx, "authentication refused", "reason", "account deactivated", "token_id", tokenID, "user_id", c.UserID)
		return Caller{}, ErrUnauthorized
	}

	if deviceID != nil {
		c.DeviceID = *deviceID
	}
	return c, nil
}

// IssueEnrollmentToken mints the bootstrap credential for one person, and is
// what Purser's Provision calls — on a first invite and on a re-invite alike.
//
// R6'S RE-INVITE CASE, WHICH LYCEUM'S CONNECTOR GETS WRONG. Provision must be
// able to re-issue, and re-issuing must leave exactly ONE redeemable token. The
// supersede below is half of that; the other half is the partial unique index
// in 0007, which turns forgetting it into a constraint violation rather than
// into a second live invitation nobody notices until two devices enroll.
func (s *Store) IssueEnrollmentToken(ctx context.Context, userID uuid.UUID) (IssuedToken, error) {
	plaintext, hash, err := MintToken()
	if err != nil {
		return IssuedToken{}, err
	}
	expires := ServerTime().Add(EnrollmentTokenLifetime)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Superseded rather than deleted, on the same argument redeemed_at gets:
	// "what became of that invitation?" is a question somebody asks precisely
	// when something has gone wrong, and a deleted row cannot answer it.
	if _, err := tx.Exec(ctx, `
		UPDATE enrollment_tokens SET superseded_at = now()
		 WHERE user_id = $1 AND redeemed_at IS NULL AND superseded_at IS NULL`, userID); err != nil {
		return IssuedToken{}, fmt.Errorf("store: supersede enrollment token: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO enrollment_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`, uuid.New(), userID, hash, expires); err != nil {
		return IssuedToken{}, fmt.Errorf("store: issue enrollment token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return IssuedToken{}, fmt.Errorf("store: commit: %w", err)
	}
	return IssuedToken{Plaintext: plaintext, ExpiresAt: expires}, nil
}

// RedeemEnrollment turns a bootstrap token into a device and its first pair.
//
// THE LOCKS THIS TAKES, in the form internal/store/messages.go asks for —
// "anything that later locks a user or member row owes the same argument":
//
//   - enrollment_tokens, FOR UPDATE, on the one row being redeemed. Nothing else
//     in this service touches that table, so it orders against nothing.
//   - users is NOT LOCKED. deactivated_at is read with no lock at all; the
//     devices insert then takes KEY SHARE on users(id) through its foreign key,
//     which is exactly the lock messages.go already argues is safe against a
//     FOR NO KEY UPDATE deactivation. A SELECT … FOR UPDATE here would be
//     strictly stronger than that argument allows.
//   - the refresh and access inserts take KEY SHARE on the devices row this
//     transaction just created, so nothing can contend for it.
//   - log_counter is never drawn. No message is inserted, so this never enters
//     the deployment-wide serialised section and orders against nothing in it.
//
// THE RACE THAT LEAVES OPEN IS CLOSED AT AUTHENTICATION, NOT HERE, AND THAT IS
// R6'S OWN ARGUMENT. A deactivation committing between the unlocked read and
// the insert lets a device row be created for an account that is now disabled,
// because KEY SHARE and FOR NO KEY UPDATE do not conflict and both commit. That
// device is then refused on every request by Authenticate. What is left behind
// is a stray row and no access — R6's explicitly-accepted half-done state,
// arriving from the other direction.
func (s *Store) RedeemEnrollment(ctx context.Context, presented, deviceName string) (Enrollment, error) {
	// Before the pool is touched, on the SendMessage pattern: a malformed
	// request should not cost a connection.
	if deviceName == "" || utf8.RuneCountInString(deviceName) == 0 {
		return Enrollment{}, ErrDeviceNameRequired
	}
	if len(deviceName) > MaxDeviceNameBytes {
		return Enrollment{}, fmt.Errorf("store: device_name is %d bytes, limit is %d: %w",
			len(deviceName), MaxDeviceNameBytes, ErrDeviceNameRequired)
	}
	if presented == "" {
		s.logger.WarnContext(ctx, "enrollment refused", "reason", "empty credential")
		return Enrollment{}, ErrUnauthorized
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		tokenID      uuid.UUID
		userID       uuid.UUID
		expiresAt    time.Time
		redeemedAt   *time.Time
		supersededAt *time.Time
	)
	// FOR UPDATE is what makes single-use hold against two simultaneous
	// redemptions rather than only against a later one. Both find the row; one
	// waits; the loser then reads redeemed_at set and is refused.
	err = tx.QueryRow(ctx, `
		SELECT id, user_id, expires_at, redeemed_at, superseded_at
		  FROM enrollment_tokens WHERE token_hash = $1 FOR UPDATE`,
		HashToken(presented)).Scan(&tokenID, &userID, &expiresAt, &redeemedAt, &supersededAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		s.logger.WarnContext(ctx, "enrollment refused", "reason", "unknown token")
		return Enrollment{}, ErrUnauthorized
	case err != nil:
		return Enrollment{}, fmt.Errorf("store: redeem: %w", err)
	}

	now := ServerTime()
	switch {
	case redeemedAt != nil:
		s.logger.WarnContext(ctx, "enrollment refused", "reason", "already redeemed", "token_id", tokenID)
		return Enrollment{}, ErrUnauthorized
	case supersededAt != nil:
		s.logger.WarnContext(ctx, "enrollment refused", "reason", "superseded", "token_id", tokenID)
		return Enrollment{}, ErrUnauthorized
	case !expiresAt.After(now):
		s.logger.WarnContext(ctx, "enrollment refused", "reason", "expired", "token_id", tokenID)
		return Enrollment{}, ErrUnauthorized
	}

	// Read, not locked. See the lock note above.
	var deactivated *time.Time
	var kind string
	if err := tx.QueryRow(ctx,
		`SELECT deactivated_at, kind FROM users WHERE id = $1`, userID).
		Scan(&deactivated, &kind); err != nil {
		return Enrollment{}, fmt.Errorf("store: redeem: load user: %w", err)
	}
	switch {
	case deactivated != nil:
		s.logger.WarnContext(ctx, "enrollment refused", "reason", "account deactivated",
			"token_id", tokenID, "user_id", userID)
		return Enrollment{}, ErrUnauthorized
	case kind != "person":
		// A bot has no device and must not acquire one. Purser never issues a
		// bot an enrollment token, so reaching this means something else did.
		s.logger.WarnContext(ctx, "enrollment refused", "reason", "not a person",
			"token_id", tokenID, "user_id", userID)
		return Enrollment{}, ErrUnauthorized
	}

	deviceID := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO devices (id, user_id, name) VALUES ($1, $2, $3)`,
		deviceID, userID, deviceName); err != nil {
		return Enrollment{}, fmt.Errorf("store: redeem: insert device: %w", err)
	}

	refresh, err := issueRefresh(ctx, tx, deviceID, now)
	if err != nil {
		return Enrollment{}, err
	}
	access, err := issueAccess(ctx, tx, userID, &deviceID, now)
	if err != nil {
		return Enrollment{}, err
	}

	// redeemed_by_device is set here and not left NULL: 0007 CHECKs that it
	// travels with redeemed_at, so a redemption always says what redeemed it.
	if _, err := tx.Exec(ctx, `
		UPDATE enrollment_tokens SET redeemed_at = now(), redeemed_by_device = $2
		 WHERE id = $1`, tokenID, deviceID); err != nil {
		return Enrollment{}, fmt.Errorf("store: redeem: mark redeemed: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Enrollment{}, fmt.Errorf("store: commit: %w", err)
	}
	s.logger.InfoContext(ctx, "device enrolled",
		"token_id", tokenID, "user_id", userID, "device_id", deviceID)
	return Enrollment{UserID: userID, DeviceID: deviceID, Access: access, Refresh: refresh}, nil
}

// issueRefresh writes the first token of a family.
//
// THE FIRST TOKEN'S family_id IS ITS OWN id, and every rotation carries it
// forward. That is what makes "invalidate the family" one predicate over one
// column rather than a walk back up a chain of replaced_by — which matters
// because CANT-29 runs it at the moment it has just decided something is
// wrong, and a walk is a loop that can be interrupted half-done.
func issueRefresh(ctx context.Context, tx pgx.Tx, deviceID uuid.UUID, now time.Time) (IssuedToken, error) {
	plaintext, hash, err := MintToken()
	if err != nil {
		return IssuedToken{}, err
	}
	id := uuid.New()
	expires := now.Add(RefreshTokenLifetime)
	if _, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (id, device_id, token_hash, family_id, expires_at)
		VALUES ($1, $2, $3, $1, $4)`, id, deviceID, hash, expires); err != nil {
		return IssuedToken{}, fmt.Errorf("store: issue refresh token: %w", err)
	}
	return IssuedToken{Plaintext: plaintext, ExpiresAt: expires}, nil
}

// issueAccess writes one access token. device is nil for a bot, and 0007's
// CHECK then requires the expiry to be NULL too.
func issueAccess(ctx context.Context, tx pgx.Tx, userID uuid.UUID, device *uuid.UUID, now time.Time) (IssuedToken, error) {
	plaintext, hash, err := MintToken()
	if err != nil {
		return IssuedToken{}, err
	}
	var expires *time.Time
	var issued IssuedToken
	if device != nil {
		e := now.Add(AccessTokenLifetime)
		expires, issued.ExpiresAt = &e, e
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO access_tokens (id, token_hash, user_id, device_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, uuid.New(), hash, userID, device, expires); err != nil {
		return IssuedToken{}, fmt.Errorf("store: issue access token: %w", err)
	}
	issued.Plaintext = plaintext
	return issued, nil
}

// IssueBotToken mints the long-lived non-rotating credential for a service
// account. CANT-73 owns the surface that calls it — the admin page behind
// Access, or a CLI subcommand — and never Purser, which provisions people.
//
// The plaintext is returned exactly once and cannot be recovered afterwards.
func (s *Store) IssueBotToken(ctx context.Context, userID uuid.UUID) (IssuedToken, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM users WHERE id = $1`, userID).Scan(&kind); err != nil {
		return IssuedToken{}, fmt.Errorf("store: issue bot token: load user: %w", err)
	}
	// The half of the shape a CHECK constraint cannot see. 0007 requires a
	// device-less token to have no expiry; it cannot also require that such a
	// row's user is a bot, because a CHECK reads one table. This is that half,
	// and it is why a person can never be handed a credential that never expires.
	if kind != "bot" {
		return IssuedToken{}, fmt.Errorf("store: issue bot token: user %s is a %s, not a bot", userID, kind)
	}

	issued, err := issueAccess(ctx, tx, userID, nil, ServerTime())
	if err != nil {
		return IssuedToken{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuedToken{}, fmt.Errorf("store: commit: %w", err)
	}
	s.logger.InfoContext(ctx, "bot token issued", "user_id", userID)
	return issued, nil
}

// RevokeDevice ends a device's access, everywhere, at once.
//
// THE WRITE LANDS HERE RATHER THAN IN CANT-30, and the reason is the property
// rather than the size. A revocation and its notification must be atomic — the
// same argument CANT-18's ruling 2 makes for pg_notify inside SendMessage's
// transaction: Postgres delivers a NOTIFY at commit, so a notification cannot
// exist without its cause or the reverse. A helper handed to another ticket
// could be called outside a transaction, or before the UPDATE, or after a
// branch that returns early, and nothing would say so. CANT-30 builds the list
// and the surface over this; it does not have to get this right again.
//
// IDEMPOTENT, BY THE revoked_at IS NULL GUARD, and that is R6's requirement
// rather than tidiness. Deprovision fans out over a person's devices and may
// half-succeed, so the retry that mops up the stragglers must not re-notify
// every device it already revoked. A second call returns false and publishes
// nothing.
//
// THE LOCK IS FOR NO KEY UPDATE, which is what a non-key UPDATE takes, and
// internal/store/messages.go:66 has already argued it: it does not conflict
// with the KEY SHARE the send path takes on the same row through
// messages.sender_device_id, so a revocation cannot block a send or deadlock
// against one. That note anticipated this write; it is not a new obligation.
//
// SEVERING THE LIVE SOCKET IS NOT HERE. It is CANT-30's, by its own `Done
// when`, over the channel this publishes on — and CANT-30 also owns the gap
// case, because Postgres queues nothing for a disconnected listener and a
// revocation has no cursor to resync from. The answer there is to re-check the
// instance's own live sessions on OnGap; it is cheap, and it is named so that
// it is not discovered.
func (s *Store) RevokeDevice(ctx context.Context, deviceID uuid.UUID) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID uuid.UUID
	err = tx.QueryRow(ctx, `
		UPDATE devices SET revoked_at = now()
		 WHERE id = $1 AND revoked_at IS NULL
		 RETURNING user_id`, deviceID).Scan(&userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Unknown device, or already revoked. Both are "nothing changed", and
		// the caller is told which by the false rather than by an error: a
		// retry finding its work already done is success.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: revoke device: %w", err)
	}

	payload, err := RevocationPayload{DeviceID: &deviceID}.Encode()
	if err != nil {
		// Loud rather than silent. A revocation that committed without its
		// notification would leave a severed device holding a live socket, and
		// the person who clicked the button would have no way to know.
		return false, fmt.Errorf("store: revoke device: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, RevocationChannel, payload); err != nil {
		return false, fmt.Errorf("store: revoke device: notify: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit: %w", err)
	}
	s.logger.InfoContext(ctx, "device revoked", "device_id", deviceID, "user_id", userID)
	return true, nil
}
