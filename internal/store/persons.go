package store

// CANT-130 — Store 1a: a person can be created, once, from Purser's email.
//
// THIS IS ROW 1 OF CANT-33'S PLAN, THE DATA-SHAPE HALF, SPLIT FROM ROW 1B AT
// THE SEAM THE APPROVING REVIEW PROPOSED. It builds EnsurePerson, its handle
// function, PersonByEmail and the store half of `catenary user set-email`.
// DeactivateUser, reactivation and the revocation publisher are CANT-134's —
// EnsurePerson on a DEACTIVATED person returns ErrPersonDeactivated and
// changes nothing here, because reversing an offboard has to sweep every live
// device first (ruling 5), and nothing in this file can do that yet.
//
// ONE TRANSACTION PER ATTEMPT, on messages.go's own argument, which this file
// now names in its exhaustive list: the user row is held FOR NO KEY UPDATE,
// never FOR UPDATE, so this composes with a send's KEY SHARE on
// `users(author_id)` rather than cycling against it. EnsurePerson never draws
// `log_counter` — issuing or superseding an enrollment token touches none of
// the three tables that ordinal lives above — so it never enters the
// deployment-wide serialised section SendMessage and metadata.go share.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// Errors

// ErrInvalidEmail is an empty or malformed address, refused before the pool
// is touched — SendMessage's own rule for a caller's malformed request, not
// a credential that failed to check out.
var ErrInvalidEmail = errors.New("store: invalid email")

// ErrPersonNotFound is PersonByEmail resolving nobody.
var ErrPersonNotFound = errors.New("store: no person with that email")

// ErrPersonDeactivated is EnsurePerson finding an existing, but deactivated,
// person. NOT YET A REACTIVATION: CANT-134 replaces this branch with the
// sweep-then-clear reversal ruling 5 settled; until it does, reversing an
// offboard from here would mint a device for an account whose live sessions
// were never revoked, resurrecting exactly the device tokens.go:396-402
// documents as the accepted race. Nothing changes on this path.
var ErrPersonDeactivated = errors.New("store: person is deactivated; reactivation is not built here yet")

// ErrNoSuchUser is `catenary user set-email` naming a handle nothing has —
// distinct from bots.go's ErrNoSuchBot, which means "not a bot" rather than
// "not anybody": set-email's target may be any kind, and its own refusals
// tell a bot's handle apart from an unknown one.
var ErrNoSuchUser = errors.New("store: no user with that handle")

// ErrCannotEmailBot is set-email naming a bot's handle. A service account is
// never identified by an email — Purser provisions people, never bots
// (CANT-73) — and 0010's users_bot_has_no_email CHECK is the schema's own
// backstop for the same rule.
var ErrCannotEmailBot = errors.New("store: a bot cannot hold an email")

// ErrPersonAlreadyHasEmail is set-email naming a person who has one.
// Overwriting it would re-key the identity Purser looks people up by out
// from under an account that already has a working one.
var ErrPersonAlreadyHasEmail = errors.New("store: person already has an email")

// ErrEmailTaken is set-email naming an email somebody else already holds.
var ErrEmailTaken = errors.New("store: email already belongs to another account")

// ---------------------------------------------------------------------------
// Types

// PersonStatus is what EnsurePerson and PersonByEmail both report — never
// stored as its own column, derived from `deactivated_at` at the moment
// either is called, on invariant 3's rule that a fact two answers could
// disagree about is never held in two places.
const (
	PersonStatusActive      = "active"
	PersonStatusDeactivated = "deactivated"
)

// PersonAccount is one person's identity as EnsurePerson reports it. Email,
// handle and display name never disagree with what PersonByEmail reports for
// the same account, because both read the same row.
type PersonAccount struct {
	UserID      uuid.UUID
	Email       string
	Handle      string
	DisplayName string
	Status      string // PersonStatusActive | PersonStatusDeactivated
}

// PersonLookup is PersonByEmail's answer: identity, plus the one fact
// Reconcile needs beyond it. LiveDevices is counted rather than stored, on
// BotRow's own precedent: a stored count and the rows it counts are two
// things that can disagree, and "does this person still have a working
// device" must not have two answers.
type PersonLookup struct {
	PersonAccount
	LiveDevices int
}

// PersonOutcome is how EnsurePerson got to the account it is returning.
type PersonOutcome string

const (
	// PersonCreated is a brand new kind='person' row.
	PersonCreated PersonOutcome = "created"
	// PersonExisting is an already-active person, found by email. A fresh
	// enrollment token is minted regardless of how this call got here — a
	// re-invite must always work, the same argument IssueEnrollmentToken's
	// own doc comment makes for Provision.
	PersonExisting PersonOutcome = "existing"
)

// EnsuredPerson is what EnsurePerson hands back.
type EnsuredPerson struct {
	Account PersonAccount
	Token   IssuedToken
	Outcome PersonOutcome

	// Note is set only on the never-adopt path: the operator-facing sentence
	// naming the handle this call assigned and the email-less handle it did
	// NOT adopt, for CANT-131's connector to carry into Result.Instructions.
	// Empty on every other outcome.
	Note string
}

// ---------------------------------------------------------------------------
// personGuardFault — CANT-130 criterion 6's negative controls

// personGuardFault is a deliberately broken EnsurePerson, PersonByEmail or
// SetEmail, on collisionFault's own shape and for the same reason: CANT-73's
// closing notes found that RevokeBotTokens' two guarding predicates were
// mutually redundant and invisible to a test that removed only one of them.
// Each value below removes exactly one of this row's own guards against
// reaching a bot, so a test can watch it fail alone rather than discovering,
// later, that two of them were covering for each other.
type personGuardFault int

const (
	personFaultNone personGuardFault = iota

	// personFaultLookupIgnoresKind drops "AND kind = 'person'" from the email
	// lookup EnsurePerson and PersonByEmail share. Provably redundant with
	// 0010's users_bot_has_no_email CHECK in ordinary operation — a bot can
	// never hold an email, so a bare lower(email) match can never resolve to
	// one — which is exactly why ITS OWN test has to remove the CHECK first:
	// an untestable predicate reads as protection without being one
	// (CANT-125's own argument for isTokenHashCollision).
	personFaultLookupIgnoresKind

	// personFaultDedupIgnoresBots makes handle de-duplication check persons
	// only — the mistake this ticket exists not to make. users.handle has no
	// lower() index, so "argosy" and "Argosy" are different STRINGS to the
	// database, and this predicate is the only thing that tells them apart
	// before an INSERT ever runs.
	personFaultDedupIgnoresBots

	// personFaultSetEmailIgnoresBotCheck skips SetEmail's own kind == "bot"
	// refusal. The CHECK still stops the write, so what this fault actually
	// costs is the clean refusal: without it, an operator's typo gets an
	// opaque constraint violation instead of ErrCannotEmailBot.
	personFaultSetEmailIgnoresBotCheck
)

// ---------------------------------------------------------------------------
// Email

// normalizeEmail trims and lower-cases a presented address, and refuses
// anything that is not at least shaped like one. This is NOT RFC 5322
// validation — Purser is the party that actually verifies an address, by
// sending mail to it — it exists only to keep an empty string or an obvious
// typo out of the one column the server keys a person's identity on, refused
// before this touches the pool at all.
func normalizeEmail(raw string) (string, error) {
	e := strings.TrimSpace(raw)
	if e == "" {
		return "", ErrInvalidEmail
	}
	if strings.ContainsAny(e, " \t\r\n") || strings.Count(e, "@") != 1 {
		return "", ErrInvalidEmail
	}
	local, domain, _ := strings.Cut(e, "@")
	if local == "" || domain == "" || !strings.Contains(domain, ".") {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(e), nil
}

// ---------------------------------------------------------------------------
// The handle

// maxHandleLength is 0010's own bound (users.handle carries no CHECK on
// length — this is CANT-130's own choice, comfortably short of anything a
// person would type by hand and generous for an email's local part).
//
// THE CAP INCLUDES A DE-DUPLICATION SUFFIX. handleCandidate truncates the
// STEM to make room for "-2", "-3", … rather than appending past it, so a
// full-length collision still fits inside this bound.
const maxHandleLength = 32

// deriveHandle turns an email's local part into a candidate handle. Pure —
// no database, nothing but the string — because chooseHandle is the only
// thing that can answer whether the result is actually free, and a function
// that has to ask a question it does not need to ask is harder to test than
// the string logic it wraps.
//
// THE STEPS, IN ORDER, AND WHY EACH ONE IS THERE.
//
//   - Drop a `+tag`: the local part before the first `+`, which is the
//     address a person actually reads even though `alice+catenary@x.example`
//     and `alice@x.example` are the same mailbox.
//   - Lowercase.
//   - Keep `[a-z0-9._-]` and drop everything else, UNICODE INCLUDED.
//     Transliterating "héllo" to "hello" would be a second, silent decision
//     about identity nobody asked this function to make; dropping a
//     character it cannot safely fold is the honest answer.
//   - Trim leading and trailing punctuation, so a local part that led or
//     trailed with one of the four kept-but-not-alphanumeric characters does
//     not hand back a handle starting or ending in a dot or a dash.
//   - Truncate to maxHandleLength.
//   - Fall back to "user" if nothing survives.
func deriveHandle(email string) string {
	local, _, _ := strings.Cut(email, "@")
	if i := strings.IndexByte(local, '+'); i >= 0 {
		local = local[:i]
	}
	local = strings.ToLower(local)

	var b strings.Builder
	b.Grow(len(local))
	for _, r := range local {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	stem := strings.Trim(b.String(), "._-")
	if len(stem) > maxHandleLength {
		stem = stem[:maxHandleLength]
	}
	if stem == "" {
		stem = "user"
	}
	return stem
}

// handleCandidate is the n-th handle to try for a stem: the stem itself for
// n == 1, and the stem truncated to make room for "-n" otherwise. Truncating
// the STEM rather than appending past maxHandleLength is what keeps a
// full-length collision inside the cap.
func handleCandidate(stem string, n int) string {
	if n <= 1 {
		return stem
	}
	suffix := fmt.Sprintf("-%d", n)
	max := maxHandleLength - len(suffix)
	if max < 1 {
		// Pathological: thousands of collisions on one stem. Never reached
		// honestly; a floor rather than a crash if it somehow is.
		max = 1
	}
	if len(stem) > max {
		stem = stem[:max]
	}
	return stem + suffix
}

// handleConflictQuery is the de-duplication check chooseHandle runs for every
// candidate. DE-DUPLICATION FOLDS CASE, AND COMPARES AGAINST EVERY USER —
// persons AND bots — and that is a STATED STORE RULE rather than an accident
// of this query.
//
// FindOrCreateDirect and BotByHandle both resolve a handle with
// `WHERE handle = $1`, an EXACT match, because users.handle is a plain
// UNIQUE TEXT column with no citext and no lower(handle) index (bots.go's own
// comment on trimHandle records that no write path folds case, and that
// folding in one place "would invent a uniqueness rule the schema does not
// enforce"). So "argosy" and "Argosy" are NOT the same handle to either of
// those lookups — they are two different accounts, and a mistyped case opens
// a conversation with the wrong one. That is the reason this folds case, not
// that the two "look alike in a room": the wire's User carries no handle for
// a room to show.
//
// THE RULE LIVES HERE, IN THE STORE, AND NOT AS A lower(handle) UNIQUE INDEX
// IN THE SCHEMA — which would be the principled home for it. Migrations
// auto-apply on boot (migrations/migrations.go), so an index that failed to
// build against handles this deployment already holds would be a crash loop,
// on exactly the argument that rewrote 0010 to drop its person-requires-email
// CHECK. So the check runs here, in Go, against a plain query, and the
// residual race this leaves is named rather than hidden: an operator running
// `catenary bot create Argosy` in the same instant Purser ensures
// `argosy@x.example` can still produce both handles, because nothing
// serializes the two. Accepted — it is one mixed-case collision in one
// instant, against a schema change that would crash-loop the deployed
// service.
func handleConflictQuery(fault personGuardFault) string {
	q := `SELECT kind, handle, email IS NOT NULL FROM users WHERE lower(handle) = lower($1)`
	if fault == personFaultDedupIgnoresBots {
		q += ` AND kind = 'person'`
	}
	return q
}

// chooseHandle is deriveHandle's stem plus the database question it cannot
// answer alone: is this candidate already somebody's, and if the answer keeps
// being yes, what to try next.
//
// THE NEVER-ADOPT RULE LIVES HERE. If the very FIRST candidate — the
// unsuffixed stem, n == 1 — belongs to an existing `kind='person'` row that
// has NO email, that row is never treated as a match to hand back: it is one
// of soakrig's accounts, and adopting it by a handle coincidence would hand
// an existing account, and everything it can already read, to whoever's local
// part happens to start with the same word. So it is treated as taken, the
// same as any other collision, and this call moves on to the next suffix —
// logging a WARN naming both handles (never the email, which does not reach
// this far down) and returning the operator-facing note CANT-130 states.
func (s *Store) chooseHandle(ctx context.Context, tx pgx.Tx, stem string) (handle, note string, err error) {
	const maxCandidates = 1000 // pathological guard; never reached honestly
	var collidedHandle string

	for n := 1; n <= maxCandidates; n++ {
		candidate := handleCandidate(stem, n)

		var kind, existing string
		var hasEmail bool
		scanErr := tx.QueryRow(ctx, handleConflictQuery(s.personGuardFault), candidate).
			Scan(&kind, &existing, &hasEmail)
		switch {
		case errors.Is(scanErr, pgx.ErrNoRows):
			if collidedHandle != "" {
				note = fmt.Sprintf(
					"assigned %s because %s has no email; if that is the same person, run `catenary user set-email`",
					candidate, collidedHandle)
				s.logger.WarnContext(ctx, "ensure person did not adopt an email-less person's handle",
					"assigned_handle", candidate, "existing_handle", collidedHandle)
			}
			return candidate, note, nil
		case scanErr != nil:
			return "", "", fmt.Errorf("store: ensure person: check handle: %w", scanErr)
		}

		if n == 1 && kind == "person" && !hasEmail {
			collidedHandle = existing
		}
	}
	return "", "", fmt.Errorf("store: ensure person: no free handle for %q after %d candidates", stem, maxCandidates)
}

// ---------------------------------------------------------------------------
// Constraint names — pinned by TestTheHandleAndEmailConstraintsArePinned
// against pg_constraint / pg_indexes, the way isTokenHashCollision's own name
// is pinned in proposal_test.go.

// usersHandleKeyConstraint is the name Postgres gave 0001's inline
// `handle TEXT NOT NULL UNIQUE`.
const usersHandleKeyConstraint = "users_handle_key"

// usersEmailLowerIndexConstraint is 0010's own partial unique index. Postgres
// reports a violated unique INDEX the same way it reports a violated named
// constraint — the constraint_name on the wire error is the index's name —
// which is what lets isEmailCollision tell it apart from usersHandleKeyConstraint
// by name rather than by guessing from context.
const usersEmailLowerIndexConstraint = "users_email_lower_idx"

// usersBotHasNoEmailConstraint is 0010's own CHECK, named so a test can drop
// it deliberately (CANT-130 criterion 6's negative controls, which are the
// only way to put a bot in the state this CHECK exists to make impossible).
const usersBotHasNoEmailConstraint = "users_bot_has_no_email"

// isHandleCollision reports whether err is users_handle_key refusing an
// INSERT — BY CONSTRAINT NAME, as isTokenHashCollision does, so a 23505 on
// some other constraint is never routed as a handle collision.
func isHandleCollision(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == usersHandleKeyConstraint
}

// isEmailCollision reports whether err is users_email_lower_idx refusing an
// INSERT or UPDATE.
func isEmailCollision(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == usersEmailLowerIndexConstraint
}

// ---------------------------------------------------------------------------
// The email lookup EnsurePerson and PersonByEmail share

// personLookupQuery is the one query both EnsurePerson and PersonByEmail run
// to find a person by email — WHY THE SAME QUERY, IN ONE PLACE: a rule
// enforced in two hand-written queries is a rule enforced in one of them a
// quarter from now, the same argument CANT-83 makes for one error-code table.
// forUpdate appends FOR NO KEY UPDATE for EnsurePerson's own reason (see the
// file header and messages.go); PersonByEmail passes false, because it "must
// not touch any row" (criterion 2) and a lock is a touch even where nothing
// is written.
func personLookupQuery(forUpdate bool, fault personGuardFault) string {
	q := `SELECT id, email, handle, display_name, deactivated_at FROM users WHERE lower(email) = lower($1)`
	if fault != personFaultLookupIgnoresKind {
		q += ` AND kind = 'person'`
	}
	if forUpdate {
		q += ` FOR NO KEY UPDATE`
	}
	return q
}

// ---------------------------------------------------------------------------
// PersonByEmail

// PersonByEmail is Purser's Reconcile seam: read-only, case-insensitive,
// persons only. It NEVER CREATES, MINTS, ROTATES OR INVALIDATES ANYTHING —
// Purser's own contract for a look-up (PRSR-15) — and it takes no lock, so it
// cannot be the thing that makes a concurrent EnsurePerson or a future
// DeactivateUser wait.
func (s *Store) PersonByEmail(ctx context.Context, email string) (PersonLookup, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return PersonLookup{}, ErrPersonNotFound
	}

	var id uuid.UUID
	var storedEmail, handle, displayName string
	var deactivated *time.Time
	err := s.pool.QueryRow(ctx, personLookupQuery(false, s.personGuardFault), email).
		Scan(&id, &storedEmail, &handle, &displayName, &deactivated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return PersonLookup{}, ErrPersonNotFound
	case err != nil:
		return PersonLookup{}, fmt.Errorf("store: person by email: %w", err)
	}

	live, err := s.liveDeviceCount(ctx, id)
	if err != nil {
		return PersonLookup{}, err
	}

	status := PersonStatusActive
	if deactivated != nil {
		status = PersonStatusDeactivated
	}
	return PersonLookup{
		PersonAccount: PersonAccount{
			UserID: id, Email: storedEmail, Handle: handle, DisplayName: displayName, Status: status,
		},
		LiveDevices: live,
	}, nil
}

// liveDeviceCount is derived rather than stored, on BotRow.LiveTokens' own
// precedent: a count and the rows it counts must not have two answers.
func (s *Store) liveDeviceCount(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM devices WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: live device count: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// EnsurePerson

// EnsurePerson is what Purser's `ensure` calls, on a first invite and on a
// re-invite alike — IssueEnrollmentToken's own re-invite argument, carried to
// its second caller. Everything it does happens in ONE transaction per
// attempt.
//
// R5'S REACTIVATE BRANCH IS NOT HERE. A deactivated person is refused with
// ErrPersonDeactivated and nothing changes; see that error's own doc comment
// for why reversing an offboard cannot be done from this file yet.
//
// DISPLAY_NAME IS SET ON CREATION ONLY. The existing-person branch below
// never writes it, whatever name this call is carrying — metadata_guard_test.go
// fails the build on an UPDATE … SET display_name outside metadata.go, and a
// rename is a different operation this ticket does not build.
func (s *Store) EnsurePerson(ctx context.Context, email, displayName string) (EnsuredPerson, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return EnsuredPerson{}, err
	}
	displayName = strings.TrimSpace(displayName)

	// BOUNDED RETRY, NOT AN INFINITE LOOP. The only path back here is a 23505
	// on the email or the handle constraint — see ensurePersonOnce — and each
	// one means a concurrent EnsurePerson (or, for the handle, a concurrent
	// `catenary bot create`) just committed the row this attempt's stale read
	// missed. One retry always resolves the email case, because the second
	// attempt's lookup deterministically finds the row the first attempt's
	// failed INSERT proves now exists. The bound exists so a bug elsewhere
	// fails loudly instead of spinning.
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ep, retry, err := s.ensurePersonOnce(ctx, normalized, displayName)
		if !retry {
			return ep, err
		}
		lastErr = err
	}
	return EnsuredPerson{}, fmt.Errorf(
		"store: ensure person: gave up after %d attempts racing a concurrent create: %w", maxAttempts, lastErr)
}

// ensurePersonOnce is one attempt, in one transaction. retry is true exactly
// when the caller should run a brand new attempt rather than trust err —
// which happens only when this attempt's own INSERT lost a race this
// transaction cannot resolve for itself, because the failed statement has
// already aborted it (RotateRefreshProposing's own reason for rolling back
// before routeCollision runs on the pool, applied here as "run the whole
// operation again" rather than "read committed state inline").
func (s *Store) ensurePersonOnce(ctx context.Context, email, displayName string) (_ EnsuredPerson, retry bool, _ error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EnsuredPerson{}, false, fmt.Errorf("store: ensure person: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id                            uuid.UUID
		storedEmail, handle, nameFrom string
		deactivated                   *time.Time
	)
	err = tx.QueryRow(ctx, personLookupQuery(true, s.personGuardFault), email).
		Scan(&id, &storedEmail, &handle, &nameFrom, &deactivated)
	switch {
	case err == nil:
		if deactivated != nil {
			s.logger.WarnContext(ctx, "ensure person refused", "reason", "account deactivated", "user_id", id)
			return EnsuredPerson{}, false, ErrPersonDeactivated
		}

		token, err := issueEnrollmentTokenTx(ctx, tx, id)
		if err != nil {
			return EnsuredPerson{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return EnsuredPerson{}, false, fmt.Errorf("store: ensure person: commit: %w", err)
		}
		s.logger.InfoContext(ctx, "person ensured", "user_id", id, "outcome", string(PersonExisting))
		return EnsuredPerson{
			Account: PersonAccount{
				UserID: id, Email: storedEmail, Handle: handle, DisplayName: nameFrom, Status: PersonStatusActive,
			},
			Token:   token,
			Outcome: PersonExisting,
		}, false, nil

	case !errors.Is(err, pgx.ErrNoRows):
		return EnsuredPerson{}, false, fmt.Errorf("store: ensure person: lookup: %w", err)
	}

	// NOT FOUND: create. displayName falls back to the HANDLE, on CreateBot's
	// own argument — a caller that supplied nothing gets something to render
	// rather than a refusal — and it must NOT fall back to the email:
	// display_name reaches every member over the wire, and the plan's own
	// honesty argument is that an email never does.
	stem := deriveHandle(email)
	chosenHandle, note, err := s.chooseHandle(ctx, tx, stem)
	if err != nil {
		return EnsuredPerson{}, false, err
	}
	name := displayName
	if name == "" {
		name = chosenHandle
	}

	newID := uuid.New()
	_, err = tx.Exec(ctx, `
		INSERT INTO users (id, handle, display_name, email, kind)
		VALUES ($1, $2, $3, $4, 'person')`, newID, chosenHandle, name, email)
	switch {
	case isHandleCollision(err):
		// The pre-check above raced a concurrent create of the identical
		// string (case-sensitive — the UNIQUE(handle) index is what actually
		// catches it). A whole new attempt sees every row this one's own
		// rollback releases, including the one that just won.
		return EnsuredPerson{}, true, err
	case isEmailCollision(err):
		// Somebody else's EnsurePerson for this SAME email committed between
		// this attempt's lookup and its insert. This transaction is aborted
		// by the failed INSERT and cannot read the winner's row on it; a
		// fresh attempt's lookup finds it deterministically and returns
		// PersonExisting.
		return EnsuredPerson{}, true, err
	case err != nil:
		return EnsuredPerson{}, false, fmt.Errorf("store: ensure person: insert: %w", err)
	}

	token, err := issueEnrollmentTokenTx(ctx, tx, newID)
	if err != nil {
		return EnsuredPerson{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EnsuredPerson{}, false, fmt.Errorf("store: ensure person: commit: %w", err)
	}
	s.logger.InfoContext(ctx, "person ensured", "user_id", newID, "outcome", string(PersonCreated), "handle", chosenHandle)
	return EnsuredPerson{
		Account: PersonAccount{UserID: newID, Email: email, Handle: chosenHandle, DisplayName: name, Status: PersonStatusActive},
		Token:   token,
		Outcome: PersonCreated,
		Note:    note,
	}, false, nil
}

// ---------------------------------------------------------------------------
// SetEmail — `catenary user set-email`'s store half

// SetEmail is the one-time adoption path for a person soakrig created with no
// email: CANT-130's answer to migration 0010 NOT carrying a person-requires-
// email CHECK. It refuses a bot, an unknown handle, an email already held by
// anyone, and a person who already has one.
//
// EXACT-MATCH ON HANDLE, LIKE BotByHandle — no case folding on lookup, for
// bots.go's own reason: users.handle has no lower() index, so this has to
// agree with what actually collides at the database rather than with a rule
// only this function would be enforcing.
func (s *Store) SetEmail(ctx context.Context, handle, email string) (uuid.UUID, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return uuid.Nil, err
	}
	handle = trimHandle(handle)
	if handle == "" {
		return uuid.Nil, ErrNoSuchUser
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: set email: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id uuid.UUID
	var kind string
	var hasEmail bool
	err = tx.QueryRow(ctx, `SELECT id, kind, email IS NOT NULL FROM users WHERE handle = $1`, handle).
		Scan(&id, &kind, &hasEmail)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, ErrNoSuchUser
	case err != nil:
		return uuid.Nil, fmt.Errorf("store: set email: lookup: %w", err)
	}
	if kind == "bot" && s.personGuardFault != personFaultSetEmailIgnoresBotCheck {
		return uuid.Nil, ErrCannotEmailBot
	}
	if hasEmail {
		return uuid.Nil, ErrPersonAlreadyHasEmail
	}

	if _, err := tx.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1`, id, normalized); err != nil {
		if isEmailCollision(err) {
			return uuid.Nil, ErrEmailTaken
		}
		return uuid.Nil, fmt.Errorf("store: set email: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("store: set email: commit: %w", err)
	}
	s.logger.InfoContext(ctx, "email set", "user_id", id, "handle", handle)
	return id, nil
}
