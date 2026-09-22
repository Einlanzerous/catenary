package store

// CANT-130 — Store 1a: a person can be created, once, from Purser's email.
//
// THIS IS ROW 1 OF CANT-33'S PLAN, THE DATA-SHAPE HALF, SPLIT FROM ROW 1B AT
// THE SEAM THE APPROVING REVIEW PROPOSED. It builds EnsurePerson, its handle
// function, PersonByEmail and the store half of `catenary user set-email`.
//
// EnsurePerson's REACTIVATION BRANCH ARRIVED WITH CANT-134 (1b) and its work
// lives in offboard.go — reactivateTx — beside the offboard it reverses, so
// that both writers of users.deactivated_at are read together. What is here is
// the branch: the lookup that finds a deactivated account, and the one
// transaction that then holds that row FOR NO KEY UPDATE across sweep,
// publish, clear and issue.
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

// ErrPersonNotFound is this surface resolving nobody: PersonByEmail on an
// email nothing holds, or (since CANT-134) DeactivateUser on an id that is
// unknown OR is a bot's.
//
// ONE SENTINEL FOR BOTH SHAPES, DELIBERATELY. CANT-131 maps it to 404 on both
// operations, and a bot must not be distinguishable from nobody: telling them
// apart would make this surface an oracle for which ids name service accounts.
var ErrPersonNotFound = errors.New("store: no such person")

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
	// PersonReactivated is a person who was DEACTIVATED and is not any more —
	// ruling 5, built in CANT-134. It is reported separately from
	// PersonExisting rather than folded into it because the two are the same
	// answer to Purser and very different news to the operator who caused it:
	// any later invite naming Catenary — a bundle re-run, a re-hire, a retry —
	// un-offboards, since Purser skips a service only while its account row is
	// active. CANT-131's connector carries this value into
	// Result.Instructions, which is where that operator is looking.
	PersonReactivated PersonOutcome = "reactivated"
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

	// personFaultDeactivateIgnoresKind drops "AND kind = 'person'" from
	// DeactivateUser's own locking lookup (offboard.go). CANT-134's addition,
	// and unlike personFaultLookupIgnoresKind it needs no broken CHECK to
	// demonstrate: DeactivateUser is given an ID rather than an email, so
	// nothing about a bot's row stops this predicate being the only thing
	// between Purser and a service account's credentials.
	personFaultDeactivateIgnoresKind

	// personFaultOffboardFailsAfterFirstWrite makes DeactivateUser return an
	// error immediately after setting users.deactivated_at. NOT A REMOVED
	// GUARD — an injected failure, which is the only way to observe ruling 4's
	// all-or-nothing claim: without one transaction, the users row would be
	// committed with every credential it names still live.
	personFaultOffboardFailsAfterFirstWrite

	// personFaultSweepIgnoresIsNull drops "AND revoked_at IS NULL" from all
	// three credential sweeps (offboard.go). The writes stay correct — an
	// already-revoked row is revoked again — and what it costs is idempotence:
	// a converged retry re-stamps revoked_at and publishes a second revocation
	// for sockets that were severed the first time, which is the bug
	// invalidateFamily's own doc comment records finding.
	personFaultSweepIgnoresIsNull

	// personFaultReactivationFailsAfterSweep makes EnsurePerson's reversal
	// return an error after its sweep and its publish, before it clears
	// deactivated_at. NOT A REMOVED GUARD — an injected failure, and the only
	// way to observe from the outside that the reversal is ONE transaction:
	// with it, the sweep's revocations and the clear must both roll back, so a
	// stray device is still live and the account is still deactivated.
	personFaultReactivationFailsAfterSweep

	// personFaultReactivationSkipsSweep makes EnsurePerson's reversal clear
	// deactivated_at WITHOUT running the sweep first — rev 1 of CANT-33's plan,
	// which relied on the offboard having already revoked everything. For
	// exactly one device it had not: the stray row tokens.go documents. This is
	// criterion 5's negative control, and it is watched resurrecting that
	// device.
	personFaultReactivationSkipsSweep

	// personFaultOffboardFailsAfterTheMarker makes DeactivateUser return an
	// error after its metadata bump has drawn log_counter and written every
	// marker, and before its Commit. CANT-137's own all-or-nothing control, and
	// it watches the one thing the earlier fault cannot: that a DRAWN counter
	// value rolls back with the rest — so a failed offboard leaves no gap in
	// log_seq for a client to wonder about, and tells nobody anything.
	personFaultOffboardFailsAfterTheMarker
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
//   - Truncate to maxHandleLength, AND TRIM AGAIN — found in review (#79).
//     Truncating a byte string can land exactly on an internal dot or dash
//     that the first trim never saw, because that trim ran before the cut
//     existed; a plain slice would then hand back the "does not end in a
//     dot" promise broken by the very next line. TrimRight only, because the
//     first character of a stem that already passed the first trim is never
//     punctuation, so truncating from the right can never expose a leading
//     one — a claim worth stating because it is what lets this skip the
//     empty-after-all-that check truncation would otherwise need.
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
		stem = strings.TrimRight(stem[:maxHandleLength], "._-")
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
//
// RE-TRIMMED AFTER ITS OWN CUT, for deriveHandle's own reason (#79): this
// truncates an already-clean stem FURTHER, to make room for the suffix, and
// that second cut can exactly as easily land on an internal dot or dash that
// was never at an edge before. The stem entering this function never STARTS
// with punctuation (deriveHandle's own guarantee), so trimming only the
// right side cannot empty it.
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
		stem = strings.TrimRight(stem[:max], "._-")
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

// errReversalRacedAnOffboard is never returned to a caller: EnsurePerson's
// bounded retry swallows it and runs a fresh attempt. It exists so the reason for
// that retry is a named value rather than a bare `true` — the same courtesy
// isHandleCollision and isEmailCollision do for the other two retry causes, and
// the one the `gave up after N attempts` message wraps.
var errReversalRacedAnOffboard = errors.New(
	"store: ensure person: an offboard committed between this attempt's peek and its locked lookup")

// peekForReversal runs the SHARED email lookup UNLOCKED, to answer one question
// ahead of the lock: is this email a deactivated person, and which one?
//
// WHY IT REUSES personLookupQuery RATHER THAN A NARROWER ONE. The kind filter and
// the case-insensitive match are the two rules that decide whether an email
// resolves to a person at all, and a second query would be a second place for
// either to be got wrong — which is the argument that put both callers on one
// query in the first place. Three of the five columns are discarded here, and
// naming them as discards is cheaper than a lookup that could disagree with the
// one that decides.
//
// A MISS IS NOT AN ERROR. Nobody by this email, or an active person, both answer
// "no room locks needed" — and ensurePersonOnce's locked lookup is what turns
// either into an outcome.
func (s *Store) peekForReversal(ctx context.Context, tx pgx.Tx, email string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	var discardEmail, discardHandle, discardName string
	var deactivated *time.Time
	err := tx.QueryRow(ctx, personLookupQuery(false, s.personGuardFault), email).
		Scan(&id, &discardEmail, &discardHandle, &discardName, &deactivated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, false, nil
	case err != nil:
		return uuid.Nil, false, fmt.Errorf("store: ensure person: peek for the reversal: %w", err)
	}
	return id, deactivated != nil, nil
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
// RULING 5: IT REACTIVATES, AND THE REVERSAL REVOKES FOR ITSELF. A deactivated
// person found by email is swept — every live device, every refresh and access
// token, any unredeemed invitation — the revocation is published, and only then
// is `deactivated_at` cleared and a fresh enrollment token issued, all on the
// one transaction that has held the row FOR NO KEY UPDATE since the lookup.
// Outcome PersonReactivated, logged at WARN. offboard.go's reactivateTx carries
// the argument for the order.
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

	// BOUNDED RETRY, NOT AN INFINITE LOOP. THREE PATHS BACK HERE, AND THEY ARE
	// NOT THE SAME SHAPE — the third was added by CANT-137 and this paragraph
	// used to say "the only path".
	//
	//   - A 23505 on the EMAIL constraint. A concurrent EnsurePerson for the same
	//     email committed the row this attempt's stale read missed. ONE retry
	//     always resolves it, because the second attempt's lookup
	//     deterministically finds the row the first attempt's failed INSERT
	//     proves now exists.
	//   - A 23505 on the HANDLE constraint, from a concurrent `catenary bot
	//     create` or EnsurePerson choosing the identical string. Resolved by
	//     re-running chooseHandle against committed state.
	//   - errReversalRacedAnOffboard: this attempt's unlocked peek said the email
	//     was not a deactivated person and its LOCKED lookup said it was, so an
	//     offboard committed in between and the reversal needs room locks this
	//     transaction cannot take any more. See ensurePersonOnce's own comment for
	//     why the peek exists at all. Unlike the email case this is NOT
	//     single-retry by construction: a second offboard committing inside the
	//     next attempt's own peek window sends it back here again. It is
	//     vanishingly unlikely — it needs a deprovision landing in a window a few
	//     statements wide, twice — and the bound is what makes "unlikely" safe
	//     rather than a hope.
	//
	// The bound exists so a bug elsewhere, or a pathological run of any of the
	// three, fails loudly instead of spinning.
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

	// CANT-137 — THE REVERSAL'S ROOM LOCKS, TAKEN FROM AN UNLOCKED PEEK, AND WHY
	// THE ORDER LEAVES NO CHOICE.
	//
	// A reactivation now draws a metadata marker for the person and for each of
	// their rooms, and metadata.go's order puts `conversations` ABOVE `users`. But
	// the branch that decides whether this is a reactivation at all is chosen by
	// READING `deactivated_at` under the user lock — so by the time this function
	// knows it needs the room locks, taking them would invert the order against a
	// concurrent metadata bump, which is a cycle no ordering removes (offboard.go's
	// file header).
	//
	// So the shared lookup runs TWICE: once unlocked, to learn whether this email
	// belongs to a deactivated person and which one, and once locked, exactly as
	// before, to decide. The unlocked read is a peek and is treated as one — it is
	// allowed to be wrong, and the locked read is still the only thing that
	// decides anything.
	//
	// THE TWO WAYS IT CAN BE WRONG, AND WHAT EACH COSTS.
	//
	//   - The peek says deactivated and the locked read says active (an offboard
	//     was reversed in between). This transaction holds room locks it does not
	//     need, in the right order, and releases them at commit. Nothing to do.
	//   - The peek says active or nothing and the locked read says deactivated (an
	//     offboard committed in between). The reversal needs locks this attempt
	//     cannot take any more, so the attempt is ABANDONED and EnsurePerson runs
	//     a whole new one — the retry seam that is already here for a lost INSERT
	//     race, used for the second time and for the same reason: the state this
	//     attempt read is stale and a fresh attempt reads it committed. The next
	//     peek sees the offboard and takes the locks.
	peekID, peekDeactivated, err := s.peekForReversal(ctx, tx, email)
	if err != nil {
		return EnsuredPerson{}, false, err
	}
	var rooms []uuid.UUID
	if peekDeactivated {
		if rooms, err = lockRoomsOfPerson(ctx, tx, peekID); err != nil {
			return EnsuredPerson{}, false, fmt.Errorf("store: ensure person: %w", err)
		}
	}

	var (
		id                            uuid.UUID
		storedEmail, handle, nameFrom string
		deactivated                   *time.Time
	)
	err = tx.QueryRow(ctx, personLookupQuery(true, s.personGuardFault), email).
		Scan(&id, &storedEmail, &handle, &nameFrom, &deactivated)
	switch {
	case err == nil:
		outcome := PersonExisting
		var revoked CredentialsRevoked
		var cleared bool
		if deactivated != nil {
			if !peekDeactivated || peekID != id {
				// The peek missed an offboard that committed in between. See the
				// comment above: a whole new attempt, rather than reaching for
				// room locks below the user lock this transaction already holds.
				return EnsuredPerson{}, true, errReversalRacedAnOffboard
			}
			// THE REVERSAL — ruling 5, and the whole of CANT-134's second half.
			// It runs on THIS transaction, which has held the user row FOR NO
			// KEY UPDATE since the lookup above, so a reactivation can never
			// commit without its sweep: sweep → publish → clear → issue.
			swept, didClear, err := s.reactivateTx(ctx, tx, id)
			if err != nil {
				return EnsuredPerson{}, false, err
			}
			revoked, cleared, outcome = swept, didClear, PersonReactivated
		}

		token, err := issueEnrollmentTokenTx(ctx, tx, id)
		if err != nil {
			return EnsuredPerson{}, false, err
		}

		// CANT-137 — THE MARKER, LAST, AFTER THE ENROLLMENT TOKEN AND BEFORE THE
		// COMMIT. `enrollment_tokens` is position 7 of the order and the counter
		// is position 8, so the draw cannot move above the token; and it is gated
		// on the clear having actually moved the column, because that is the only
		// thing here that changes a value /sync serves. An ordinary re-invite of
		// an active person draws nothing and never enters the serialised section,
		// which is what it did before this ticket and must go on doing.
		if cleared {
			if _, err := bumpDeactivationMarkers(ctx, tx, id, rooms); err != nil {
				return EnsuredPerson{}, false, fmt.Errorf("store: ensure person: %w", err)
			}
		}

		if err := tx.Commit(ctx); err != nil {
			return EnsuredPerson{}, false, fmt.Errorf("store: ensure person: commit: %w", err)
		}
		if outcome == PersonReactivated {
			// AT WARN, AND IT IS THE POINT OF THE LEVEL RATHER THAN THE VOLUME.
			// Every other outcome of this function is an ordinary provisioning
			// call; this one silently undid an administrative act somebody
			// performed on purpose, and the operator who triggered it almost
			// certainly did not mean to. Ids and counts only — never the email,
			// never a token.
			s.logger.WarnContext(ctx, "person reactivated; an offboard was reversed",
				"user_id", id,
				"rooms_marked", len(rooms),
				"refresh_tokens_revoked", revoked.RefreshTokens,
				"access_tokens_revoked", revoked.AccessTokens,
				"devices_revoked", revoked.Devices,
				"enrollment_tokens_superseded", revoked.EnrollmentTokens)
		} else {
			s.logger.InfoContext(ctx, "person ensured", "user_id", id, "outcome", string(outcome))
		}
		return EnsuredPerson{
			Account: PersonAccount{
				UserID: id, Email: storedEmail, Handle: handle, DisplayName: nameFrom, Status: PersonStatusActive,
			},
			Token:   token,
			Outcome: outcome,
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
//
// THE LOOKUP LOCKS THE ROW, FOR NO KEY UPDATE — found in review (#79). Reading
// `email IS NOT NULL` and then writing on a separate statement, with nothing
// locked between them, is a TOCTOU: two `set-email` calls racing the same
// handle can both read "no email yet", and the second's UPDATE re-evaluates
// its WHERE against the row T1 just committed and overwrites it — success
// reported twice, ErrPersonAlreadyHasEmail never returned, and the account
// silently re-keyed to whichever call went second. FOR NO KEY UPDATE closes
// it exactly as EnsurePerson's own lookup does: the second caller blocks on
// the first's commit and then reads the row as it now is, not as it was.
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
	err = tx.QueryRow(ctx,
		`SELECT id, kind, email IS NOT NULL FROM users WHERE handle = $1 FOR NO KEY UPDATE`, handle).
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
