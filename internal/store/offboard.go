package store

// CANT-134 — Store 1b: an offboard is one transaction, and a reactivation
// revokes for itself.
//
// ROW 1 OF CANT-33'S PLAN, THE LOCKING-AND-RACES HALF, split from 1a
// (CANT-130, persons.go) at the seam the approving review proposed. Ruling 4:
// one call, one transaction. Ruling 5: whatever reverses an offboard runs the
// offboard's own sweep first. Ruling 7: MEMBERSHIP STAYS — nothing in this
// file writes conversation_members or a receipt, and a deactivated author is
// still a member of every room they were in.
//
// WHAT AN OFFBOARD IS, IN THIS ORDER, ALL OR NONE, PUBLISHING INSIDE IT:
//
//  1. every conversation the person is a member of, locked ascending, and their
//     own membership row in each — CANT-137, metadata.go's lockRoomsOfPerson;
//  2. the user row, locked FOR NO KEY UPDATE, and `deactivated_at = now()`
//     behind its own IS NULL — metadata.go's setDeactivatedAt, which is the one
//     place that column is written;
//  3. refresh_tokens, then access_tokens, then devices — invalidateFamily's
//     write order (refresh.go), each behind its own `revoked_at IS NULL`;
//  4. any unredeemed enrollment token superseded;
//  5. ONE RevocationPayload{UserID} on RevocationChannel, which severs every
//     live socket the account holds on every instance (hub.OnRevocation);
//     and, only when step 2 actually transitioned the column, ONE receipt-shaped
//     NotifyPayload{conv, user, 0, read_seq} on NotifyChannel per room the
//     person had read — CANT-143, metadata.go's notifyReadSpansOfPerson —
//     so the hub re-emits every message whose `read_by` just stopped counting
//     them to its live author, exactly as it does for a receipt;
//  6. ONE DRAW of `log_counter`, last, writing a metadata marker to the person
//     and to each of their rooms — CANT-137, and only when step 2 actually
//     transitioned the column.
//
// STEP 6 IS NEW AND STEP 1 EXISTS BECAUSE OF IT (CANT-135 ruling 3). Since
// `member_count` and `read_by` count ACTIVE members, an offboard changes what
// /sync serves for every room the person is in — and until this ticket it drew
// no marker, so it reached nobody until something else happened to touch the
// room. The marker is what makes "nobody is offboarded without being told of"
// true, and the price is that both directions now enter the deployment-wide
// serialised section, once, briefly, after every row lock is already held.
//
// ONE TRANSACTION, AND THE NOTIFY INSIDE IT. CANT-18's ruling 2, applied here
// for the fourth time in this package on RevokeDevice's own argument: Postgres
// delivers a NOTIFY at commit, so a notification cannot exist without its
// cause or the reverse. R6's partial-failure hazard — an ACTIVE account left
// half-revoked, which nothing in Purser's model can tell from an offboard that
// never started — cannot occur here, because there is no fan-out to
// half-complete.
//
// EVERY SWEEP RUNS UNCONDITIONALLY, EACH BEHIND ITS OWN IS NULL, AND THE
// NOTIFY FIRES WHEN ANYTHING TRANSITIONED — invalidateFamily's reasoning
// (refresh.go), and rev 2 of the plan changed rev 1 to match it. Gating the
// whole transaction on the users-row transition would make a retry unable to
// REPAIR an account whose row is already set but which still holds a live
// device — the stray device RedeemEnrollment documents (tokens.go), or a hand
// UPDATE. A fully converged second call still changes nothing and publishes
// nothing, which is R6's idempotence requirement rather than tidiness.
//
// THE LOCK ARGUMENT messages.go ASKS THIS TICKET FOR, AND IT IS NAMED IN THAT
// FILE'S EXHAUSTIVE LIST. THE ORDER IS THE SAME FOR BOTH DIRECTIONS:
//
//	conversations (every room of the person, ascending id)  →
//	conversation_members  →  users  →  refresh_tokens  →  access_tokens  →
//	devices  →  enrollment_tokens  →  log_counter, LAST.
//
//   - `conversations` AND `conversation_members` COME FIRST, ABOVE `users`, AND
//     THAT IS THE WHOLE REASON THEY ARE TAKEN BY HAND RATHER THAN LEFT TO THE
//     BUMP. metadata.go's order is conversations → conversation_members → users
//     → log_counter; a bump holding `conversations(C)` and waiting on `users(U)`
//     against an offboard holding `users(U)` and waiting on `conversations(C)` is
//     a cycle, and the bump cannot reorder — it has to hold every target row
//     before it draws. So this file agrees with metadata.go instead, and takes
//     the rooms before it has even decided whether the id names a person.
//   - The user row is taken FOR NO KEY UPDATE and NEVER FOR UPDATE. Setting or
//     clearing `deactivated_at` is a non-key update, so FOR NO KEY UPDATE is
//     the lock the write itself takes; it does not conflict with the KEY SHARE
//     a send takes on `users(author_id)` at position 11, so an offboard can
//     neither block a send nor cycle against one. A key-level lock would hold
//     a user row against that send while the send holds `log_counter`, which
//     is the cycle metadata.go's own comment describes.
//   - `log_counter` IS DRAWN, ONCE, LAST — the sentence CANT-137 corrected. It
//     used to read "never", and both directions now enter the deployment-wide
//     serialised section for one draw. Against a send: a send takes
//     `conversations(X)` at position 8 and draws at 10, so both writers take
//     conversations before the counter and the counter last, and they serialise
//     on a conversation row or on the counter rather than cycling. Against a
//     metadata bump: the same table order, ascending ids within each. Against
//     `invalidateFamily` and EnsurePerson's create path: neither touches
//     `conversations` or the counter at all.
//   - THE WRITE ORDER IS invalidateFamily'S, and the reverse is a deadlock
//     cycle rather than a style question: a replay-triggered invalidation
//     takes refresh_tokens → access_tokens → devices over rows that are a
//     subset of this one's, so an offboard taking them the other way round
//     would let Postgres abort one of the two — as a 500 on the one call that
//     must not flake.
//   - enrollment_tokens IS TAKEN LAST BEFORE THE COUNTER, AFTER devices, AND THAT
//     CANNOT CYCLE AGAINST RedeemEnrollment. A redeem locks its enrollment row
//     FIRST and everything it takes afterwards is compatible with what an
//     offboard holds: KEY SHARE on `users(id)` through the devices FK (no
//     conflict with FOR NO KEY UPDATE) and a brand-new devices row nothing else
//     can lock. So a redeem never waits on an offboard, and a cycle needs both
//     directions. A redeem takes neither `conversations` nor the counter, so
//     CANT-137's two new positions do not change that argument.
//   - THE NOTIFIES ARE RAISED BEFORE THE DRAW, not after it, and that is the one
//     place this file's order differs from the send path's. pg_notify takes no
//     row lock when it runs — the queue lock is acquired inside commit
//     (messages.go's position 12) — so it is free to sit above the counter, and
//     above is where FindOrCreateDirect's review put its own read for the same
//     reason: everything between the draw and the commit is time every send in
//     the deployment spends queued. That covers both the revocation and
//     CANT-143's read-span notifies; the one statement the latter adds — the
//     `read_seq` read behind them — is a plain SELECT on member rows this
//     transaction already holds, and locks nothing it did not have.
//
// THE REVERSAL LIVES IN persons.go, INSIDE EnsurePerson, and shares this
// file's sweep as one tx-taking helper so the two cannot drift. It holds the
// user row FOR NO KEY UPDATE across sweep → publish → clear `deactivated_at` →
// issue the enrollment token, in one transaction, so a reactivation can never
// commit without its sweep — and since CANT-137 that transaction takes the
// person's rooms above the user row and draws the same single marker below the
// enrollment token. EnsurePerson owns those two steps rather than reactivateTx,
// because the room locks have to be taken before the lookup that decides the
// branch and the draw has to come after the token: neither end of the order is
// inside the helper.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// What an offboard reports

// CredentialsRevoked counts what one pass of revokeCredentialsTx transitioned.
//
// COUNTS RATHER THAN A BOOL, because the structured log line below is the only
// place an operator ever sees an offboard happen, and "the account was already
// deactivated but still held two live devices" is the state a retry exists to
// repair — a bool cannot say it. CANT-131's handler reads them for its own
// line too.
//
// NOT NAMED CredentialSweep: SweepCredentials (sweep.go, CANT-118) DELETES
// spent rows on a retention window, and these two would read as the same
// operation at a call site.
type CredentialsRevoked struct {
	RefreshTokens int64
	AccessTokens  int64
	Devices       int64

	// EnrollmentTokens is SUPERSEDED rather than revoked — enrollment_tokens
	// has no revoked_at, and 0007's CHECK reserves superseded_at for exactly
	// this: an invitation that was withdrawn rather than used.
	EnrollmentTokens int64
}

// Changed reports whether this pass transitioned any credential.
func (r CredentialsRevoked) Changed() bool {
	return r.RefreshTokens > 0 || r.AccessTokens > 0 || r.Devices > 0 || r.EnrollmentTokens > 0
}

// Offboard is what DeactivateUser hands back: enough for the caller to answer
// and enough for one log line, and nothing a caller could read two ways.
type Offboard struct {
	UserID uuid.UUID

	// Deactivated is whether THIS call set `users.deactivated_at`. False on a
	// retry against an account that was already deactivated — which is still
	// success, and still repairs whatever credentials survived.
	Deactivated bool

	Revoked CredentialsRevoked

	// MarkerLogSeq is the `log_counter` value this call drew for the metadata
	// marker, or 0 when it drew none — which is exactly when `Deactivated` is
	// false, because nothing /sync serves changed and there is nothing to tell
	// anybody (CANT-137).
	//
	// REPORTED RATHER THAN INFERRED, for one reason each side of the boundary.
	// Inside, it is what lets a test say "exactly one draw, however many rooms"
	// without reading the counter twice around the call and hoping nothing else
	// ran. Outside, it is the number an operator correlates against a client's
	// cursor when somebody asks why a room still shows the old count.
	MarkerLogSeq int64
}

// Changed reports whether this call transitioned anything at all.
//
// DERIVED RATHER THAN STORED, AND IT IS THE PUBLISH GATE ITSELF rather than a
// second opinion about it — invariant 3's rule applied to a return value. A
// `Published bool` beside these fields would be a fact that could disagree
// with them, and the disagreement would be invisible: the caller would log
// "nothing changed" about a transaction that had just severed four sockets.
func (o Offboard) Changed() bool { return o.Deactivated || o.Revoked.Changed() }

// ---------------------------------------------------------------------------
// The lock

// deactivateLockQuery takes the user row and, in the same statement, decides
// whether this id is one this surface may act on at all.
//
// A SEPARATE STATEMENT FROM THE WRITE, AND THAT IS WHAT MAKES THE THREE
// ANSWERS DISTINGUISHABLE. The obvious single statement —
// `UPDATE users SET deactivated_at = now() WHERE id = $1 AND kind = 'person'
// AND deactivated_at IS NULL RETURNING id` — cannot tell "already
// deactivated" from "unknown id or a bot's", and those are the surface's 204
// and its 404 (CANT-131). Splitting them also puts the lock before the write,
// where a reader looks for it.
//
// PERSONS ONLY, AND A BOT IS INDISTINGUISHABLE FROM NOBODY. Purser provisions
// people and never bots (CANT-73), so a deprovision must not be able to reach
// a service account — and it must not be able to learn that an id belongs to
// one either, which is why both answers are the same not-found.
func deactivateLockQuery(fault personGuardFault) string {
	q := `SELECT id FROM users WHERE id = $1`
	if fault != personFaultDeactivateIgnoresKind {
		q += ` AND kind = 'person'`
	}
	// FOR NO KEY UPDATE, never FOR UPDATE — see the file header's lock
	// argument and messages.go's own list.
	return q + ` FOR NO KEY UPDATE`
}

// ---------------------------------------------------------------------------
// DeactivateUser

// DeactivateUser ends one person's access to this service, everywhere, at
// once — the store half of Purser's `Deprovision` (PRSR-17: revoke, never
// delete).
//
// ONE CALL, ONE TRANSACTION, ALL OR NONE, PUBLISHING INSIDE IT. See the file
// header for the order, the lock argument, and why every sweep is
// unconditional.
//
// IDEMPOTENT, AND CONVERGENT, WHICH ARE NOT THE SAME CLAIM. A second call
// against a fully converged account writes nothing and publishes nothing; a
// second call against an account whose `deactivated_at` is set but which still
// holds a live device REPAIRS it and publishes, because the notification is
// the only thing that severs that device's socket and the first call is the
// one that did not send it.
//
// MEMBERSHIP IS UNTOUCHED (ruling 7). A membership row is not a credential:
// the person is frozen in place, their messages stay attributed, their
// receipts stop moving, and a reactivation finds their rooms as they left
// them. What that costs other members' `read_by` and `N MEMBERS` was CANT-135
// (the counts stop counting them), CANT-137 (the rooms are told) and CANT-143
// (the messages they had read are re-emitted to their authors), in that order.
//
// NOT FOUND IS ErrPersonNotFound FOR AN UNKNOWN ID AND FOR A BOT'S ALIKE.
func (s *Store) DeactivateUser(ctx context.Context, userID uuid.UUID) (Offboard, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Offboard{}, fmt.Errorf("store: deactivate user: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// STEP 1 — THE ROOMS, ABOVE THE USER ROW, AND BEFORE THIS CALL KNOWS
	// WHETHER IT MAY ACT AT ALL (CANT-137). The order is metadata.go's and the
	// argument is in the file header: a bump takes `conversations` before
	// `users`, so an offboard that took `users` first would close a cycle
	// against it. The id's kind is not known until the lock below, so a call
	// that is about to answer not-found for a bot's id holds that bot's room
	// locks for the microseconds before this transaction rolls back. That is the
	// price of agreeing with metadata.go, it is paid only by a refused
	// administrative call, and an unknown id locks nothing because it is in no
	// rooms.
	rooms, err := lockRoomsOfPerson(ctx, tx, userID)
	if err != nil {
		return Offboard{}, fmt.Errorf("store: deactivate user: %w", err)
	}

	var locked uuid.UUID
	err = tx.QueryRow(ctx, deactivateLockQuery(s.personGuardFault), userID).Scan(&locked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Offboard{}, ErrPersonNotFound
	case err != nil:
		return Offboard{}, fmt.Errorf("store: deactivate user: lock: %w", err)
	}

	out := Offboard{UserID: userID}

	// STEP 2 — THE COLUMN, WRITTEN IN metadata.go BECAUSE THE GUARD NOW WATCHES
	// IT, and its own IS NULL is still the transition test. See setDeactivatedAt
	// for why the bool rather than a read, and the guard's own message for why
	// the statement cannot live here any more.
	if out.Deactivated, err = setDeactivatedAt(ctx, tx, userID); err != nil {
		return Offboard{}, fmt.Errorf("store: deactivate user: %w", err)
	}

	// CANT-134 criterion 3's all-or-nothing control, injected exactly here:
	// after the first write and before anything else, which is the only place
	// a partial offboard could become visible if this were not one
	// transaction.
	if s.personGuardFault == personFaultOffboardFailsAfterFirstWrite {
		return Offboard{}, errors.New("store: deactivate user: injected fault after the first write")
	}

	revoked, err := s.revokeCredentialsTx(ctx, tx, userID)
	if err != nil {
		return Offboard{}, err
	}
	out.Revoked = revoked

	// THE NOTIFY BEFORE THE DRAW. See the file header's last bullet: it takes no
	// row lock when it runs, and everything between the draw and the commit is
	// time every send in the deployment spends queued.
	if out.Changed() {
		if err := publishUserRevocation(ctx, tx, userID); err != nil {
			return Offboard{}, fmt.Errorf("store: deactivate user: %w", err)
		}
	}

	// STEP 5b — THE READ SPANS, ONE RECEIPT-SHAPED NOTIFY PER ROOM THE PERSON HAD
	// READ, AND ONLY IF THE COLUMN MOVED (CANT-143, CANT-140 ruling 1). Beside the
	// revocation because it is the same kind of statement — a notify that takes
	// no row lock and belongs above the draw — and gated like the marker rather
	// than like the revocation, for the marker's reason: what this announces is
	// a change to a value /sync serves (`read_by`, and the `state` derived from
	// it, on every message the person had read), and a retry that moved no
	// column changed none. metadata.go's CANT-143 section is the argument;
	// nothing here locks anything, draws anything, or touches the hub.
	if out.Deactivated {
		if err := notifyReadSpansOfPerson(ctx, tx, userID, rooms); err != nil {
			return Offboard{}, fmt.Errorf("store: deactivate user: %w", err)
		}
	}

	// STEP 6 — THE MARKER, LAST, AND ONLY IF THE COLUMN MOVED (CANT-137). Gated
	// on `Deactivated` rather than on `Changed()`, and the difference is exactly
	// what /sync serves: revoking a stray device changes no value on the wire, so
	// a credential-repairing retry has nothing to tell a co-member and must not
	// enter the serialised section to say it. Which also means an account whose
	// column was set by something other than this function — a hand UPDATE, the
	// state TestASecondOffboardRepairs… builds — is never announced by the retry
	// that repairs it. The guard in metadata_guard_test.go exists so that state
	// cannot arise from code in this repository.
	if out.Deactivated {
		if out.MarkerLogSeq, err = bumpDeactivationMarkers(ctx, tx, userID, rooms); err != nil {
			return Offboard{}, fmt.Errorf("store: deactivate user: %w", err)
		}
	}

	// CANT-137's own all-or-nothing control, injected after the marker writes and
	// before the commit — the one point where "the offboard and the news of it
	// are one transaction" becomes observable from outside. A drawn-but-rolled-back
	// counter value is the case worth watching: it must leave no gap a client
	// could see, which it cannot, because the draw is an UPDATE of a row and
	// rolls back with everything else.
	if s.personGuardFault == personFaultOffboardFailsAfterTheMarker {
		return Offboard{}, errors.New("store: deactivate user: injected fault after the marker bump")
	}

	if err := tx.Commit(ctx); err != nil {
		return Offboard{}, fmt.Errorf("store: deactivate user: commit: %w", err)
	}

	// ONE STRUCTURED LINE, IDS AND COUNTS, NEVER A TOKEN AND NEVER THE EMAIL —
	// the rule every credential log in this package follows. A converged retry
	// logs too, with `changed=false`: a Purser retry arriving repeatedly is
	// worth seeing, and silence is what makes it invisible.
	s.logger.InfoContext(ctx, "user deactivated",
		"user_id", userID,
		"changed", out.Changed(),
		"deactivated_now", out.Deactivated,
		"rooms_marked", len(rooms),
		"metadata_log_seq", out.MarkerLogSeq,
		"refresh_tokens_revoked", revoked.RefreshTokens,
		"access_tokens_revoked", revoked.AccessTokens,
		"devices_revoked", revoked.Devices,
		"enrollment_tokens_superseded", revoked.EnrollmentTokens)
	return out, nil
}

// ---------------------------------------------------------------------------
// The sweep both directions share

// revokeCredentialsTx revokes every live credential one person holds and
// supersedes any unredeemed enrollment token, ON THE CALLER'S TRANSACTION.
//
// ONE HELPER, TWO CALLERS, AND THAT IS THE POINT OF IT. DeactivateUser runs it
// after setting `deactivated_at`; EnsurePerson's reversal runs it before
// clearing it (ruling 5). Two copies of this would be two copies of the write
// order, the IS NULL guards and the enrollment supersede — and the one that
// drifted would be the reversal, which is the path a reader exercises least
// and the one where a missed row means a resurrected device.
//
// IT TAKES THE TRANSACTION AND IT DOES NOT PUBLISH OR COMMIT, on
// publishRevocation's own argument: the property both callers need is that
// every write and the notify land together, and a helper that took the pool
// could be called outside the transaction with nothing in the signature to say
// so. What the two callers genuinely differ about — the users row, and what
// the log line says — stays with them.
//
// WHY EACH PREDICATE IS THE ONE IT IS:
//
//   - refresh_tokens has no user_id, so the person's devices are the join.
//     Every rotation carries device_id forward, so this is every family the
//     account has ever held.
//   - access_tokens IS SCOPED BY user_id AND NOT BY DEVICE. A person's tokens
//     all carry their user_id, and 0007's CHECK makes a device-less token a
//     bot's — IssueBotToken refuses to mint one for anyone else — so for a
//     person the two sets are the same today. Scoping by the account is the
//     one that stays correct if that ever stops being true, and an offboard is
//     the wrong place to discover it. (There is no index on
//     access_tokens(user_id); the scan is over one small group's credentials,
//     which CANT-118 sweeps, and an offboard is a rare administrative act. A
//     new index here would be a migration this ticket deliberately does not
//     take.)
//   - devices is what makes every other mechanism agree: Authenticate refuses
//     it, DeadDevices names it, and CANT-30's gap re-check reaches the same
//     verdict as the notification.
//   - enrollment_tokens is superseded with redeemed_at IS NULL in the
//     predicate as well, because 0007 CHECKs that a row is never both.
func (s *Store) revokeCredentialsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (CredentialsRevoked, error) {
	var out CredentialsRevoked

	// `AND revoked_at IS NULL` ON EACH OF THE THREE, and it is not decoration:
	// without it a converged retry re-stamps revoked_at — moving the moment a
	// credential died — and reports a transition that did not happen, which
	// publishes a second revocation for sockets that were severed the first
	// time.
	guard := " AND revoked_at IS NULL"
	if s.personGuardFault == personFaultSweepIgnoresIsNull {
		guard = ""
	}

	fam, err := tx.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		 WHERE device_id IN (SELECT id FROM devices WHERE user_id = $1)`+guard, userID)
	if err != nil {
		return CredentialsRevoked{}, fmt.Errorf("store: revoke credentials: refresh_tokens: %w", err)
	}
	out.RefreshTokens = fam.RowsAffected()

	acc, err := tx.Exec(ctx, `
		UPDATE access_tokens SET revoked_at = now()
		 WHERE user_id = $1`+guard, userID)
	if err != nil {
		return CredentialsRevoked{}, fmt.Errorf("store: revoke credentials: access_tokens: %w", err)
	}
	out.AccessTokens = acc.RowsAffected()

	dev, err := tx.Exec(ctx, `
		UPDATE devices SET revoked_at = now()
		 WHERE user_id = $1`+guard, userID)
	if err != nil {
		return CredentialsRevoked{}, fmt.Errorf("store: revoke credentials: devices: %w", err)
	}
	out.Devices = dev.RowsAffected()

	// THE TEST-ONLY PAUSE SITS HERE, BETWEEN devices AND enrollment_tokens, AND
	// IT HAS TO. It exists to drive the race RedeemEnrollment documents as
	// accepted (tokens.go): a redeem whose device row commits after this
	// sweep's own snapshot. A redeem holds its enrollment row FOR UPDATE for
	// the whole of its transaction, so a pause placed AFTER the supersede below
	// would wedge — the test would wait for a redeem that is waiting for this
	// transaction to commit — rather than drive anything. Nil in every Store
	// the composition root builds.
	if s.offboardPause != nil {
		s.offboardPause()
	}

	enr, err := tx.Exec(ctx, `
		UPDATE enrollment_tokens SET superseded_at = now()
		 WHERE user_id = $1 AND redeemed_at IS NULL AND superseded_at IS NULL`, userID)
	if err != nil {
		return CredentialsRevoked{}, fmt.Errorf("store: revoke credentials: enrollment_tokens: %w", err)
	}
	out.EnrollmentTokens = enr.RowsAffected()

	return out, nil
}

// ---------------------------------------------------------------------------
// The reversal

// reactivateTx is the other half of this file: the only thing in the service
// that CLEARS users.deactivated_at. It is called from EnsurePerson's
// existing-person branch (persons.go) when that branch finds a deactivated
// account — ruling 5 — and it lives here, beside the offboard, so that both
// writers of that one column are read together.
//
// ITS CALLER'S TRANSACTION IS ALREADY HOLDING THE USER ROW FOR NO KEY UPDATE,
// from the lookup that decided this branch, and it must be: the branch is
// chosen by reading `deactivated_at`, and without the lock a concurrent
// offboard landing in between makes that read stale — a reactivation that
// commits on top of an offboard it never saw, leaving an account Purser
// believes it just disabled holding a fresh enrollment token.
//
// SWEEP, PUBLISH, THEN CLEAR, AND THE ORDER IS THE CLAIM. RedeemEnrollment
// accepts that a redeem racing an offboard can leave a device row the
// offboard's sweep could not see — its snapshot predates the insert — because
// Authenticate refuses it while the account is disabled. CLEAR
// deactivated_at AND THAT DEVICE, WITH ITS REFRESH AND ACCESS TOKENS, IS LIVE
// AGAIN. Rev 1 of the plan relied on the offboard having revoked everything;
// for exactly that device it had not. So the reversal revokes for itself, in
// the same transaction that clears the column, and the person enrolls again
// from nothing.
//
// THE PUBLISH IS NOT REDUNDANT EITHER. The sweep can find a live device — the
// stray one above — whose socket is attached to some instance right now, and
// that socket is severed by the notification and by nothing else.
//
// THE SUPERSEDE INSIDE THE SWEEP IS REDUNDANT HERE, AND STAYS. EnsurePerson
// calls issueEnrollmentTokenTx immediately afterwards, which supersedes any
// unredeemed token itself. Sharing one sweep with the offboard is worth more
// than saving that statement: what must never differ between the two
// directions is which rows get revoked.
//
// THE MARKER IS NOT DRAWN HERE, AND THAT IS NOT AN OMISSION (CANT-137). The
// counter goes last in the transaction, and this function is not last: its
// caller issues an enrollment token afterwards, which writes `enrollment_tokens`
// — position 7 of the order, above the counter at 8. So EnsurePerson draws, one
// statement before its Commit, and the room locks this bump needs were taken by
// EnsurePerson too, above the lookup that chose this branch. Both ends of the
// order therefore live with the caller; what lives here is what the two
// directions must not differ about. CANT-143's read-span notify is the
// caller's for the same reason: it is raised over the rooms EnsurePerson
// locked, and gated on the bool this function returns.
func (s *Store) reactivateTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (CredentialsRevoked, bool, error) {
	var revoked CredentialsRevoked

	// CANT-134 criterion 5's negative control: rev 1's shape, which trusted the
	// offboard's own sweep. Watched resurrecting the stray device.
	if s.personGuardFault != personFaultReactivationSkipsSweep {
		var err error
		if revoked, err = s.revokeCredentialsTx(ctx, tx, userID); err != nil {
			return CredentialsRevoked{}, false, err
		}
		if revoked.Changed() {
			if err := publishUserRevocation(ctx, tx, userID); err != nil {
				return CredentialsRevoked{}, false, fmt.Errorf("store: reactivate person: %w", err)
			}
		}
	}

	// CANT-134 criterion 5's all-or-nothing control, injected between the sweep
	// and the clear — the one point where "the reversal is one transaction"
	// becomes observable from outside.
	if s.personGuardFault == personFaultReactivationFailsAfterSweep {
		return CredentialsRevoked{}, false, errors.New("store: reactivate person: injected fault after the sweep")
	}

	// THE CLEAR, IN metadata.go, AND ITS BOOL IS THE CALLER'S DRAW GATE. Under
	// the user lock this branch was chosen by reading `deactivated_at`, so the
	// predicate is all but certain to match — `all but` because the guard's own
	// point is that the column has one writer and this is the assertion, not the
	// assumption.
	cleared, err := clearDeactivatedAt(ctx, tx, userID)
	if err != nil {
		return CredentialsRevoked{}, false, fmt.Errorf("store: reactivate person: %w", err)
	}
	return revoked, cleared, nil
}

// ---------------------------------------------------------------------------
// The publisher

// publishUserRevocation is the user-subject half of the revocation channel,
// and CANT-134 is the first thing in this service that ever writes it.
//
// THE CONSUMER HAS BEEN BUILT AND TESTED SINCE CANT-30: hub.OnRevocation's
// UserID branch closes every session the account holds on every device, and
// RevocationPayload has carried the field since CANT-28 ruling 7 precisely so
// this would not be a payload migration across a running deployment. Every
// comment that said the field had no publisher is corrected in this ticket's
// own PR.
//
// IT WRAPS NOTHING, AND ITS TWO CALLERS DO — found in review (#81). This has
// two of them, an offboard and the reversal of one, and a prefix chosen here
// would name the wrong operation for one of them: a pg_notify failure during a
// reactivation reported as "store: deactivate user: notify: …" sends an
// operator looking at the transaction that did not fail. The subject is this
// function's; the operation belongs to the call site, exactly as it does for
// revokeCredentialsTx.
func publishUserRevocation(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	return publishRevocationPayload(ctx, tx, RevocationPayload{UserID: &userID})
}
