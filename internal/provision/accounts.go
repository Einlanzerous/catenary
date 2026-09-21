package provision

// The three operations — CANT-131 criterion 11. Each one is a decode, a call
// into the store, a status code and an encode, and none of them decides anything
// the store has not already decided.
//
// WHAT EACH ANSWERS, AND WHY THAT CODE:
//
//	POST /accounts                    201 created · 200 existing or reactivated
//	GET  /accounts?email=             200 · 404
//	POST /accounts/{id}/deactivate    204 · 404
//
// 201 ONLY FOR `created`. Purser does not branch on it — `ensure` is one
// idempotent call and the connector reads `outcome` — but a surface that answered
// 200 for a creation would be lying in the one vocabulary every HTTP tool
// already speaks, and the operator reading a proxy log is the reader who
// benefits. `reactivated` is a 200 and not a 201 for the same reason: the account
// was already there.
//
// 204 AND NOT 200 FOR THE OFFBOARD, twice over: there is nothing to say, and a
// body would invite the connector to read something out of it. A SECOND
// deactivate of the same person is also 204 — it is idempotent, and R6's own
// requirement is that a retry converges rather than erroring.
//
// THE TWO 404s ARE BYTE-IDENTICAL, AND THE THIRD ONE IS TOO. An unknown id, a
// BOT's id and an id that is not a UUID at all all produce the same status and
// the same body. The first two are the store's own rule (CANT-134:
// ErrPersonNotFound for both, so this surface cannot be an oracle for which ids
// name service accounts); the third is this package's decision and it is written
// out below.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wireview"
)

// The operation names that reach the log. Three constants rather than three
// literals so that one grep finds every line a given operation can write.
const (
	opEnsure     = "ensure"
	opLookup     = "lookup"
	opDeactivate = "deactivate"
)

// The log outcomes that are this surface's rather than the store's.
const (
	outcomeBadRequest = "bad_request"
	outcomeNotFound   = "not_found"
	outcomeFound      = "found"
	outcomeFailed     = "failed"

	// The offboard's three. `deactivated` is the account transitioning;
	// `repaired` is CANT-134's convergent retry finding live credentials behind
	// an already-set column, which publishes and severs sockets and is the one
	// an operator most wants to see; `converged` is the call that changed
	// nothing.
	outcomeDeactivated = "deactivated"
	outcomeRepaired    = "repaired"
	outcomeConverged   = "converged"
)

// ---------------------------------------------------------------------------
// The shapes on the wire — hand-written here, and NOT generated wire types
//
// NOTHING IN THIS FILE IMPORTS internal/wire, AND THAT IS THE DECISION. The wire
// schema is the contract between this server and its three generated clients; no
// client may call this surface, so putting these shapes in it would emit
// TypeScript and Dart for an API they are forbidden to touch (the plan's own
// tradeoff list). What IS reused is the timestamp LAYOUT — wireview.TimeLayout,
// the one format the schema calls normative — because a second timestamp format
// in one service is a second thing for a client library to get wrong, and
// Purser's generated client will parse whatever this emits.

// account is one person's identity as both answers report it.
type account struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

// ensureRequest is POST /accounts' body.
//
// `display_name` IS OPTIONAL AND `email` IS NOT. The store falls back to the
// derived handle when the name is empty (CreateBot's own argument: a caller that
// supplied nothing gets something to render rather than a refusal), and it must
// not fall back to the email, because a display name reaches every member over
// the wire and an email never does. So an absent `display_name` is a documented
// request rather than a malformed one.
type ensureRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// ensureResponse is POST /accounts' answer. `note` is omitted when empty.
//
// THE ENROLLMENT TOKEN IS IN THIS BODY AND IN NO LOG LINE. It is a credential
// that redeems into a device enrolled as this person, and the only place it
// belongs is the response to the call that asked for it.
type ensureResponse struct {
	Account             account `json:"account"`
	EnrollmentToken     string  `json:"enrollment_token"`
	EnrollmentExpiresAt string  `json:"enrollment_expires_at"`
	Outcome             string  `json:"outcome"`

	// Note carries the never-adopt sentence CANT-130 writes — "assigned alice-2
	// because alice has no email; if that is the same person, run `catenary user
	// set-email`" — to the operator who caused it, through
	// Result.Instructions. OMITTED WHEN EMPTY rather than sent as "": a
	// connector that put an empty string in front of an operator would be
	// telling them there was something to read.
	Note string `json:"note,omitempty"`
}

// lookupResponse is GET /accounts' answer, and it is FLAT where
// ensureResponse nests.
//
// SAID OUT LOUD BECAUSE IT LOOKS LIKE AN OVERSIGHT AND IS NOT. `ensure` has
// three things to report — the account, the credential it minted, and how it got
// there — so the account is one object among them. A look-up has the account and
// one derived count, and PRSR-50 is written against exactly this shape. Nesting
// it for symmetry would be a contract change to a file another repository pins by
// tag, in exchange for tidiness.
type lookupResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`

	// LiveDevices is what Reconcile reports to an operator: "can this person
	// still get in, and from how many places". Counted by the store rather than
	// stored, so it and the rows it counts cannot disagree.
	LiveDevices int `json:"live_devices"`
}

// ---------------------------------------------------------------------------
// The two enums, mapped EXPLICITLY
//
// store.PersonOutcome and store.PersonStatus already hold exactly the strings
// this contract names, so `string(ensured.Outcome)` would compile and work. It is
// not what these do, because that agreement would be a COINCIDENCE of two
// packages' constants rather than a decision: a fourth outcome added to the store
// for some later ticket would travel straight out of this surface into a
// connector whose generated client has no case for it, and the first anyone would
// hear of it is a Purser deserialisation error in production. Mapping here makes
// the contract's enum this package's own claim, and an outcome it does not
// recognise a 500 that names itself in the log.

func outcomeFor(o store.PersonOutcome) (string, bool) {
	switch o {
	case store.PersonCreated:
		return "created", true
	case store.PersonExisting:
		return "existing", true
	case store.PersonReactivated:
		return "reactivated", true
	}
	return "", false
}

func statusFor(s string) (string, bool) {
	switch s {
	case store.PersonStatusActive:
		return "active", true
	case store.PersonStatusDeactivated:
		return "deactivated", true
	}
	return "", false
}

// accountView maps one store account onto the contract's object, refusing a
// status the contract does not describe.
func accountView(a store.PersonAccount) (account, bool) {
	status, ok := statusFor(a.Status)
	if !ok {
		return account{}, false
	}
	return account{
		ID:          a.UserID.String(),
		Email:       a.Email,
		Handle:      a.Handle,
		DisplayName: a.DisplayName,
		Status:      status,
	}, true
}

// ---------------------------------------------------------------------------
// POST /accounts — ensure

// ensure creates a person, finds an active one, or reactivates a deactivated
// one, and returns a fresh enrollment token either way.
//
// ONE CALL, IDEMPOTENT, AND THE TOKEN IS ALWAYS FRESH. A re-invite is how anybody
// gets a second device, so `ensure` on an existing person cannot refuse — and the
// new token supersedes any unredeemed one, which is the store's own behaviour and
// is why the first token stops working after a second call.
//
// REACTIVATION IS RULING 5 AND IT IS NOT THIS PACKAGE'S DOING. A deactivated
// person is swept — every device, every token, any pending invitation — the
// revocation is published, and only then is the account cleared and a token
// issued, all in one transaction inside the store. What this surface adds is the
// `reactivated` outcome, which is how the operator who triggered it ever learns:
// Purser skips a service only while its account row is active, so any later
// invite naming Catenary un-offboards a person.
func (s *Server) ensure(w http.ResponseWriter, r *http.Request) {
	var req ensureRequest
	if err := decodeStrict(w, r, &req); err != nil {
		s.d.Logger.WarnContext(r.Context(), "provisioning call refused",
			"op", opEnsure, "outcome", outcomeBadRequest, "detail", describeDecodeFailure(err))
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}

	ensured, err := s.d.Ensure(r.Context(), req.Email, req.DisplayName)
	switch {
	case errors.Is(err, store.ErrInvalidEmail):
		// THE EMAIL IS NOT IN THIS LINE, and it is the one place the temptation
		// is real: an operator debugging a 400 wants to see what was sent. The
		// store refuses a malformed address before it touches the pool and Purser
		// holds the address it sent, so the two ends between them have the fact —
		// while a log that carried it would write every provisioned person's
		// email into Dozzle and Datadog for as long as those logs are kept, which
		// is what invariant 3's honesty argument is about.
		//
		// STATED EXACTLY: no line this package COMPOSES carries an email address or
		// any part of one. The `error` attribute on the 500 path below is a store
		// error logged verbatim, and what it may contain is the store's own rule —
		// ids, SQLSTATEs and constraint names, with `pgErr.Detail` excluded by name
		// (messages.go), because that is where Postgres would echo the value a
		// unique index refused.
		s.d.Logger.WarnContext(r.Context(), "provisioning call refused",
			"op", opEnsure, "outcome", outcomeBadRequest, "detail", "malformed email")
		writeError(w, http.StatusBadRequest, errInvalidEmail)
		return
	case err != nil:
		s.d.Logger.ErrorContext(r.Context(), "provisioning call failed",
			"op", opEnsure, "outcome", outcomeFailed, "error", err)
		writeError(w, http.StatusInternalServerError, errInternal)
		return
	}

	outcome, known := outcomeFor(ensured.Outcome)
	acct, ok := accountView(ensured.Account)
	if !known || !ok {
		s.d.Logger.ErrorContext(r.Context(), "provisioning call failed",
			"op", opEnsure, "outcome", outcomeFailed, "account_id", ensured.Account.UserID,
			"error", "the store reported an outcome or a status provision/openapi.yaml does not describe",
			"store_outcome", string(ensured.Outcome), "store_status", ensured.Account.Status)
		writeError(w, http.StatusInternalServerError, errInternal)
		return
	}

	code := http.StatusOK
	if ensured.Outcome == store.PersonCreated {
		code = http.StatusCreated
	}

	// ONE LINE PER ACCEPTED CALL: the operation, the outcome, the account. The
	// store writes its own line for the WRITE it performed (and at WARN for a
	// reactivation, which undid somebody's administrative act); this one is the
	// CALL, which is what an operator correlating a Purser request to a Catenary
	// write has to match on.
	//
	// IDS AND COUNTS, AND NOT THE HANDLE EITHER — the rule every credential log in
	// the store follows, applied one notch tighter here. A handle is DERIVED from
	// the email's local part, so a line carrying `ada` for `ada@example.com` has
	// carried most of an address; the store's own `person ensured` line already
	// records the handle it chose, which is where that fact belongs, and repeating
	// it here would put a fragment of every provisioned person's email in the log
	// of the one surface that is handed their address. `note_assigned` rather than
	// the note itself, for the same reason: the sentence names two handles.
	s.d.Logger.InfoContext(r.Context(), "provisioning call",
		"op", opEnsure, "outcome", outcome, "account_id", ensured.Account.UserID,
		"note_assigned", ensured.Note != "")

	writeJSON(w, code, ensureResponse{
		Account:         acct,
		EnrollmentToken: ensured.Token.Plaintext,
		// wireview.TimeLayout, the same format EnrollResponse's own
		// *_expires_at fields carry: RFC 3339 with milliseconds and a literal
		// Z, in UTC, which the schema calls normative because it is the only
		// shape that sorts chronologically as a string.
		EnrollmentExpiresAt: ensured.Token.ExpiresAt.UTC().Format(wireview.TimeLayout),
		Outcome:             outcome,
		Note:                ensured.Note,
	})
}

// ---------------------------------------------------------------------------
// GET /accounts?email= — look up

// lookup is Purser's Reconcile: read-only, and it changes nothing.
//
// A MISSING `email` IS A 400 AND AN ADDRESS NOBODY HOLDS IS A 404. Those are the
// two answers, and the line between them is drawn where this surface can draw it
// WITHOUT WRITING A SECOND COPY OF THE STORE'S ADDRESS RULE. normalizeEmail is
// the store's, unexported, and deliberately not RFC 5322; a validator here that
// had to agree with it is a validator that will not, and the disagreement would
// be a 400 for an address the store would have looked up quite happily. So the
// parameter's PRESENCE is this handler's business — a request with no `email` at
// all is a caller bug and nothing else — and its CONTENT is the store's: an
// address it cannot parse resolves nobody, which is a true 404 for a read-only
// look-up and is the answer PRSR-50 already handles ("a look-up 404 there is
// success, nothing to revoke"). Written down here, in provision/openapi.yaml, and
// in the PR, because "invalid → 400" is the obvious other choice.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" {
		s.d.Logger.WarnContext(r.Context(), "provisioning call refused",
			"op", opLookup, "outcome", outcomeBadRequest, "detail", "no email parameter")
		writeError(w, http.StatusBadRequest, errEmailMissing)
		return
	}

	found, err := s.d.Lookup(r.Context(), email)
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		// NO ACCOUNT ID AND NO EMAIL ON THIS LINE. There is no id — that is what
		// not-found means — and the email is the one thing that would identify
		// the call, which is exactly why it is not here. A Purser operator
		// reconciling somebody who does not exist has Purser's own log for the
		// address; this end records that a look-up resolved nobody and when.
		s.d.Logger.InfoContext(r.Context(), "provisioning call",
			"op", opLookup, "outcome", outcomeNotFound)
		writeError(w, http.StatusNotFound, errNoSuchAccount)
		return
	case err != nil:
		s.d.Logger.ErrorContext(r.Context(), "provisioning call failed",
			"op", opLookup, "outcome", outcomeFailed, "error", err)
		writeError(w, http.StatusInternalServerError, errInternal)
		return
	}

	acct, ok := accountView(found.PersonAccount)
	if !ok {
		s.d.Logger.ErrorContext(r.Context(), "provisioning call failed",
			"op", opLookup, "outcome", outcomeFailed, "account_id", found.UserID,
			"error", "the store reported a status provision/openapi.yaml does not describe",
			"store_status", found.Status)
		writeError(w, http.StatusInternalServerError, errInternal)
		return
	}

	s.d.Logger.InfoContext(r.Context(), "provisioning call",
		"op", opLookup, "outcome", outcomeFound, "account_id", found.UserID,
		"status", acct.Status, "live_devices", found.LiveDevices)

	writeJSON(w, http.StatusOK, lookupResponse{
		ID:          acct.ID,
		Email:       acct.Email,
		Handle:      acct.Handle,
		DisplayName: acct.DisplayName,
		Status:      acct.Status,
		LiveDevices: found.LiveDevices,
	})
}

// ---------------------------------------------------------------------------
// POST /accounts/{id}/deactivate — the offboard

// deactivate ends one person's access to this service, everywhere, at once.
//
// ONE REQUEST, BECAUSE THE STORE MAKES IT ONE TRANSACTION. R6 found that a
// fan-out Deprovision has to disable the account before revoking devices, or a
// partial failure leaves an ACTIVE account half-revoked that nothing in Purser's
// model can tell from an offboard that never started. Ruling 4 removes the hazard
// rather than ordering around it: `users.deactivated_at`, the three credential
// tables in invalidateFamily's order, the pending invitation and the NOTIFY are
// all or none.
//
// A NON-UUID ID IS A 404, WITH THE SAME BODY AS AN UNKNOWN ONE — this package's
// one real decision, and the alternative (400) is defensible. Three reasons for
// 404. The surface has exactly one thing to say about an id it will not act on,
// and saying it in one shape means a connector needs one branch rather than two
// for a distinction it can never act on differently — PRSR-50 treats a 404 for an
// id Purser RECORDED as an operator-visible error either way, and a malformed id
// from Purser is that same bug. Purser only ever presents ids this surface gave
// it, so a non-UUID here is never a legitimate request whose shape deserves
// naming. And the negative: a 400 would make the response depend on the FORM of
// the id, which is one more thing a prober can measure at a door whose whole
// design is to answer identically for everything it refuses.
func (s *Server) deactivate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		// The id is not logged. It is caller-supplied and could be anything,
		// including something that was meant to be a credential.
		s.d.Logger.WarnContext(r.Context(), "provisioning call refused",
			"op", opDeactivate, "outcome", outcomeNotFound, "detail", "id is not a uuid")
		writeError(w, http.StatusNotFound, errNoSuchAccount)
		return
	}

	out, err := s.d.Deactivate(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		// AN UNKNOWN ID AND A BOT'S ID ARRIVE HERE AS THE SAME SENTINEL, by the
		// store's own design, so this surface cannot tell them apart and could
		// not answer differently if it wanted to. The log says no more than the
		// response does, because there is no more to say.
		s.d.Logger.InfoContext(r.Context(), "provisioning call",
			"op", opDeactivate, "outcome", outcomeNotFound, "account_id", id)
		writeError(w, http.StatusNotFound, errNoSuchAccount)
		return
	case err != nil:
		s.d.Logger.ErrorContext(r.Context(), "provisioning call failed",
			"op", opDeactivate, "outcome", outcomeFailed, "account_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, errInternal)
		return
	}

	outcome := outcomeConverged
	switch {
	case out.Deactivated:
		outcome = outcomeDeactivated
	case out.Revoked.Changed():
		outcome = outcomeRepaired
	}
	s.d.Logger.InfoContext(r.Context(), "provisioning call",
		"op", opDeactivate, "outcome", outcome, "account_id", out.UserID,
		"devices_revoked", out.Revoked.Devices)

	// NO BODY, AND NO Content-Type EITHER. 204 means there is nothing to read,
	// and a Content-Type on an empty body is a header that promises one.
	w.WriteHeader(http.StatusNoContent)
}
