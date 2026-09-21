package spec

// CANT-131 criterion 12 — provision/openapi.yaml is the contract, and a test
// fails when a handler and the file disagree.
//
// WHY IT LIVES HERE, IN THE SECOND MODULE, BESIDE THE WIRE DOCUMENT'S OWN PARSE.
// The check needs an OpenAPI parser, and this module already has exactly one:
// kin-openapi v0.135.0, the version Argosy pins and the one oapi-codegen v2.7.1
// goes through, already used by openapi_test.go in this same package to prove the
// GENERATED wire document loads. Adding a YAML dependency to the service module
// to re-answer a question a parser in the tree already answers would be a second
// tool for one job. Go's internal rule is applied to the IMPORT PATH, so
// `github.com/magos/catenary/server/spec` may import
// `github.com/magos/catenary/internal/provision` — the same direction CANT-82
// established for `internal/wire`.
//
// IT DRIVES THE REAL HANDLERS, NOT A DESCRIPTION OF THEM. provision.New is given
// stub store seams and nothing else is faked: the mux, the door, the decoder, the
// status codes and the encoders are the shipped ones, so what this compares
// against the document is the bytes a connector will receive. No database is
// needed, which is why it can run in the lane that has none.
//
// BOTH DIRECTIONS, AND EACH HAS ITS OWN FAILURE MESSAGE:
//
//   - a field a handler emits that the document does not describe FAILS — which
//     is the direction that catches a handler growing a field nobody told the
//     connector about;
//   - a field the document marks `required` that a handler omits FAILS — the
//     direction that catches the document promising something;
//   - a status a handler answers that the document does not list FAILS, and a
//     status the document lists that no case here drives FAILS TOO, so the
//     document cannot describe an answer nothing produces.
//
// And every value is validated against its own schema besides, which is what
// makes the copied Uuid, Timestamp and Token patterns load-bearing rather than
// decorative.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/magos/catenary/internal/provision"
	"github.com/magos/catenary/internal/store"
)

const provisionSpecPath = "../../provision/openapi.yaml"

// The service credential, at config.MinProvisionTokenBytes. It READS AS A SENTENCE
// rather than as a credential, on ci.yml's own argument about a password-shaped
// literal a secret scanner reports forever; internal/config's copy carries the
// whole reason.
const provisionToken = "not-a-secret-just-a-Test-token-1"

// A token-shaped enrollment credential: 43 characters of base64url, which is
// what store.MintToken produces and what the contract's Token pattern describes.
const enrollmentToken = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

func loadProvisionSpec(t *testing.T) *openapi3.T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(provisionSpecPath))
	if err != nil {
		t.Fatalf("read %s: %v", provisionSpecPath, err)
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("%s does not load under kin-openapi v0.135.0: %v", provisionSpecPath, err)
	}
	return doc
}

// TestTheProvisioningSpecLoadsAndValidates is the committed parse — the same
// claim TestEmittedSpecLoadsAndValidates makes for the generated wire document,
// for a document that is hand-written and so has nothing else checking its shape.
func TestTheProvisioningSpecLoadsAndValidates(t *testing.T) {
	doc := loadProvisionSpec(t)
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("%s is not valid OpenAPI 3.0.3: %v", provisionSpecPath, err)
	}
	if got, want := doc.Info.Version, "provision-v1"; got != want {
		t.Errorf("info.version = %q, want %q — PRSR-50 pins the tag of that name, and a document "+
			"whose version disagrees with the tag it is cut under is a document nobody can pin", got, want)
	}
	// The one security scheme, and it is the whole authentication model.
	scheme := doc.Components.SecuritySchemes["provisionToken"]
	if scheme == nil || scheme.Value.Type != "http" || scheme.Value.Scheme != "bearer" {
		t.Fatalf("the document must declare one http/bearer security scheme named provisionToken, got %#v", scheme)
	}
	if len(doc.Security) != 1 {
		t.Errorf("security = %#v, want exactly one requirement applied to the whole document — "+
			"there is no unauthenticated operation on this surface", doc.Security)
	}
}

// ---------------------------------------------------------------------------
// The stub store

// stub is the three seams, and a count of how many times any of them was
// reached. The count is what lets a test assert that a request was refused
// BEFORE the store — the door and the router both have to be able to say no
// without a database being involved.
type stub struct {
	ensured       store.EnsuredPerson
	ensureErr     error
	looked        store.PersonLookup
	lookupErr     error
	offboard      store.Offboard
	deactivateErr error

	calls int
}

func (s *stub) deps() provision.Deps {
	return provision.Deps{
		Logger: slog.New(slog.DiscardHandler),
		Token:  provisionToken,
		Ensure: func(context.Context, string, string) (store.EnsuredPerson, error) {
			s.calls++
			return s.ensured, s.ensureErr
		},
		Lookup: func(context.Context, string) (store.PersonLookup, error) {
			s.calls++
			return s.looked, s.lookupErr
		},
		Deactivate: func(context.Context, uuid.UUID) (store.Offboard, error) {
			s.calls++
			return s.offboard, s.deactivateErr
		},
	}
}

func personAccount(status string) store.PersonAccount {
	return store.PersonAccount{
		UserID:      uuid.MustParse("6f1d9e2a-3b4c-4d5e-8f70-1a2b3c4d5e6f"),
		Email:       "ada@example.com",
		Handle:      "ada",
		DisplayName: "Ada Lovelace",
		Status:      status,
	}
}

func ensured(outcome store.PersonOutcome, note string) store.EnsuredPerson {
	return store.EnsuredPerson{
		Account: personAccount(store.PersonStatusActive),
		Token: store.IssuedToken{
			Plaintext: enrollmentToken,
			// A fixed instant, in a zone that is NOT UTC, so the handler's own
			// .UTC() is exercised: a timestamp formatted in local time would
			// carry the wrong wall clock behind a literal Z and the contract's
			// pattern would not notice, because the shape would still be right.
			ExpiresAt: time.Date(2026, 9, 21, 4, 5, 6, 123_000_000, time.FixedZone("plus2", 2*60*60)),
		},
		Outcome: outcome,
		Note:    note,
	}
}

// ---------------------------------------------------------------------------
// The cases — every documented status, driven

// operation names a request by the CONTRACT's path template rather than by the
// concrete URL, because that is what the document is keyed on.
type operation struct {
	template string
	method   string
}

type contractCase struct {
	name string
	op   operation

	// request builds the concrete request, credential included unless the case
	// is about the door.
	request func() *http.Request

	// state is what the store answers.
	state stub

	wantStatus int

	// wantStoreCalls is 0 for every case the surface refuses by itself.
	wantStoreCalls int
}

func withToken(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+provisionToken)
	return r
}

func ensureRequest(body string) *http.Request {
	return withToken(httptest.NewRequest(http.MethodPost, "/accounts", strings.NewReader(body)))
}

const accountID = "6f1d9e2a-3b4c-4d5e-8f70-1a2b3c4d5e6f"

func contractCases() []contractCase {
	ensureOp := operation{"/accounts", http.MethodPost}
	lookupOp := operation{"/accounts", http.MethodGet}
	offboardOp := operation{"/accounts/{id}/deactivate", http.MethodPost}

	return []contractCase{
		{
			name: "ensure creates",
			op:   ensureOp,
			request: func() *http.Request {
				return ensureRequest(`{"email":"ada@example.com","display_name":"Ada Lovelace"}`)
			},
			state:          stub{ensured: ensured(store.PersonCreated, "")},
			wantStatus:     http.StatusCreated,
			wantStoreCalls: 1,
		},
		{
			name: "ensure finds an existing person",
			op:   ensureOp,
			request: func() *http.Request {
				return ensureRequest(`{"email":"ada@example.com","display_name":"Ada Lovelace"}`)
			},
			state:          stub{ensured: ensured(store.PersonExisting, "")},
			wantStatus:     http.StatusOK,
			wantStoreCalls: 1,
		},
		{
			// The optional `note` is only ever set on the never-adopt path, and
			// this is the case that puts it in a body the checker sees — an
			// optional field the document describes and no other case exercises
			// is an optional field nothing checks.
			name:    "ensure reactivates, and carries a note",
			op:      ensureOp,
			request: func() *http.Request { return ensureRequest(`{"email":"ada@example.com"}`) },
			state: stub{ensured: ensured(store.PersonReactivated,
				"assigned ada-2 because ada has no email; if that is the same person, run `catenary user set-email`")},
			wantStatus:     http.StatusOK,
			wantStoreCalls: 1,
		},
		{
			name:           "ensure refuses a malformed email",
			op:             ensureOp,
			request:        func() *http.Request { return ensureRequest(`{"email":"not-an-address"}`) },
			state:          stub{ensureErr: store.ErrInvalidEmail},
			wantStatus:     http.StatusBadRequest,
			wantStoreCalls: 1,
		},
		{
			name:           "ensure refuses a body with a field the contract does not describe",
			op:             ensureOp,
			request:        func() *http.Request { return ensureRequest(`{"email":"ada@example.com","handle":"ada"}`) },
			wantStatus:     http.StatusBadRequest,
			wantStoreCalls: 0,
		},
		{
			name:           "ensure answers 500 when the store fails",
			op:             ensureOp,
			request:        func() *http.Request { return ensureRequest(`{"email":"ada@example.com"}`) },
			state:          stub{ensureErr: errors.New("the pool is on fire")},
			wantStatus:     http.StatusInternalServerError,
			wantStoreCalls: 1,
		},
		{
			name: "ensure without a credential",
			op:   ensureOp,
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/accounts", strings.NewReader(`{"email":"ada@example.com"}`))
			},
			wantStatus:     http.StatusUnauthorized,
			wantStoreCalls: 0,
		},
		{
			name: "look up finds a person",
			op:   lookupOp,
			request: func() *http.Request {
				return withToken(httptest.NewRequest(http.MethodGet, "/accounts?email=ada@example.com", nil))
			},
			state: stub{looked: store.PersonLookup{
				PersonAccount: personAccount(store.PersonStatusDeactivated), LiveDevices: 2,
			}},
			wantStatus:     http.StatusOK,
			wantStoreCalls: 1,
		},
		{
			name: "look up finds nobody",
			op:   lookupOp,
			request: func() *http.Request {
				return withToken(httptest.NewRequest(http.MethodGet, "/accounts?email=nobody@example.com", nil))
			},
			state:          stub{lookupErr: store.ErrPersonNotFound},
			wantStatus:     http.StatusNotFound,
			wantStoreCalls: 1,
		},
		{
			name:           "look up with no email parameter",
			op:             lookupOp,
			request:        func() *http.Request { return withToken(httptest.NewRequest(http.MethodGet, "/accounts", nil)) },
			wantStatus:     http.StatusBadRequest,
			wantStoreCalls: 0,
		},
		{
			name: "look up answers 500 when the store fails",
			op:   lookupOp,
			request: func() *http.Request {
				return withToken(httptest.NewRequest(http.MethodGet, "/accounts?email=ada@example.com", nil))
			},
			state:          stub{lookupErr: errors.New("the pool is on fire")},
			wantStatus:     http.StatusInternalServerError,
			wantStoreCalls: 1,
		},
		{
			name: "look up without a credential",
			op:   lookupOp,
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/accounts?email=ada@example.com", nil)
			},
			wantStatus:     http.StatusUnauthorized,
			wantStoreCalls: 0,
		},
		{
			name: "deactivate",
			op:   offboardOp,
			request: func() *http.Request {
				return withToken(httptest.NewRequest(http.MethodPost, "/accounts/"+accountID+"/deactivate", nil))
			},
			state: stub{offboard: store.Offboard{
				UserID: uuid.MustParse(accountID), Deactivated: true,
				Revoked: store.CredentialsRevoked{Devices: 2, AccessTokens: 2, RefreshTokens: 2},
			}},
			wantStatus:     http.StatusNoContent,
			wantStoreCalls: 1,
		},
		{
			name: "deactivate an id nothing holds, or a bot's",
			op:   offboardOp,
			request: func() *http.Request {
				return withToken(httptest.NewRequest(http.MethodPost, "/accounts/"+accountID+"/deactivate", nil))
			},
			state:          stub{deactivateErr: store.ErrPersonNotFound},
			wantStatus:     http.StatusNotFound,
			wantStoreCalls: 1,
		},
		{
			name: "deactivate an id that is not a uuid",
			op:   offboardOp,
			request: func() *http.Request {
				return withToken(httptest.NewRequest(http.MethodPost, "/accounts/not-a-uuid/deactivate", nil))
			},
			wantStatus:     http.StatusNotFound,
			wantStoreCalls: 0,
		},
		{
			name: "deactivate answers 500 when the store fails",
			op:   offboardOp,
			request: func() *http.Request {
				return withToken(httptest.NewRequest(http.MethodPost, "/accounts/"+accountID+"/deactivate", nil))
			},
			state:          stub{deactivateErr: errors.New("the pool is on fire")},
			wantStatus:     http.StatusInternalServerError,
			wantStoreCalls: 1,
		},
		{
			name: "deactivate without a credential",
			op:   offboardOp,
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/accounts/"+accountID+"/deactivate", nil)
			},
			wantStatus:     http.StatusUnauthorized,
			wantStoreCalls: 0,
		},
	}
}

// run drives one case against a real provision.Server and hands back what a
// connector would receive.
func (tc contractCase) run(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	state := tc.state
	srv := provision.New(state.deps())
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, tc.request())
	if rec.Code != tc.wantStatus {
		t.Fatalf("%s = %d, want %d: %s", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
	}
	if state.calls != tc.wantStoreCalls {
		t.Errorf("%s reached the store %d time(s), want %d", tc.name, state.calls, tc.wantStoreCalls)
	}
	return rec
}

// TestTheProvisioningContractDescribesWhatTheHandlersDo is criterion 12's
// central claim.
func TestTheProvisioningContractDescribesWhatTheHandlersDo(t *testing.T) {
	doc := loadProvisionSpec(t)
	for _, tc := range contractCases() {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.run(t)
			if err := conforms(doc, tc.op, rec); err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
		})
	}
}

// TestEveryDocumentedStatusIsDriven is the other direction at the status level:
// the document may not describe an answer nothing here produces.
//
// A contract that lists a `409` no handler can write is a contract a connector
// author writes a branch for — and the branch is dead code that will never be
// exercised until the day somebody makes it live by accident.
func TestEveryDocumentedStatusIsDriven(t *testing.T) {
	doc := loadProvisionSpec(t)

	driven := map[string]bool{}
	for _, tc := range contractCases() {
		rec := tc.run(t)
		driven[fmt.Sprintf("%s %s %d", tc.op.method, tc.op.template, rec.Code)] = true
	}

	var missing []string
	for template, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			for status := range op.Responses.Map() {
				key := fmt.Sprintf("%s %s %s", method, template, status)
				if !driven[key] {
					missing = append(missing, key)
				}
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("provision/openapi.yaml documents %d answer(s) no case in this file drives:\n  %s\n\n"+
			"Either a handler can produce it and this table is short one case, or it cannot and the "+
			"document is describing an answer that does not exist.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestAMethodTheContractDoesNotListIsRefusedWithoutReachingTheStore pins what
// the document says about methods by saying nothing: `/accounts` lists `get` and
// `post`, so every other method is net/http's 405, decided by the mux and never
// by a handler.
func TestAMethodTheContractDoesNotListIsRefusedWithoutReachingTheStore(t *testing.T) {
	doc := loadProvisionSpec(t)
	item := doc.Paths.Find("/accounts")
	if item == nil {
		t.Fatal("the document does not describe /accounts")
	}
	for _, method := range []string{http.MethodDelete, http.MethodPut, http.MethodPatch} {
		if item.GetOperation(method) != nil {
			t.Fatalf("this test assumes the contract does not list %s /accounts", method)
		}
		state := stub{}
		srv := provision.New(state.deps())
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, withToken(httptest.NewRequest(method, "/accounts", strings.NewReader(`{}`))))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /accounts = %d, want 405", method, rec.Code)
		}
		if state.calls != 0 {
			t.Errorf("%s /accounts reached the store %d time(s)", method, state.calls)
		}
	}
}

// ---------------------------------------------------------------------------
// The checker

// conforms compares one real response to the contract, in both directions.
func conforms(doc *openapi3.T, op operation, rec *httptest.ResponseRecorder) error {
	item := doc.Paths.Find(op.template)
	if item == nil {
		return fmt.Errorf("the contract describes no path %q", op.template)
	}
	operation := item.GetOperation(op.method)
	if operation == nil {
		return fmt.Errorf("the contract describes no %s on %q", op.method, op.template)
	}
	resp := operation.Responses.Status(rec.Code)
	if resp == nil || resp.Value == nil {
		return fmt.Errorf("the handler answered %d and the contract does not document it for %s %s",
			rec.Code, op.method, op.template)
	}

	body := bytes.TrimSpace(rec.Body.Bytes())
	media := resp.Value.Content.Get("application/json")

	if media == nil {
		// A documented response with no content: the handler must send none.
		if len(body) != 0 {
			return fmt.Errorf("the contract documents %d on %s %s with no body and the handler sent %q",
				rec.Code, op.method, op.template, body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "" {
			return fmt.Errorf("the contract documents %d on %s %s with no body and the handler set Content-Type %q",
				rec.Code, op.method, op.template, ct)
		}
		return nil
	}
	if len(body) == 0 {
		return fmt.Errorf("the contract documents a JSON body for %d on %s %s and the handler sent none",
			rec.Code, op.method, op.template)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("the contract documents application/json for %d on %s %s and the handler sent Content-Type %q",
			rec.Code, op.method, op.template, ct)
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("the handler's %d body on %s %s is not JSON: %v (%q)", rec.Code, op.method, op.template, err, body)
	}

	where := fmt.Sprintf("%d on %s %s", rec.Code, op.method, op.template)
	if err := fieldsAgree(media.Schema, decoded, where, "$"); err != nil {
		return err
	}
	// AND THE VALUES, against the same schema: types, enums, and the Uuid,
	// Timestamp and Token patterns. fieldsAgree above reports WHICH field and in
	// WHICH direction; this reports a value the document would refuse.
	if err := media.Schema.Value.VisitJSON(decoded); err != nil {
		return fmt.Errorf("the handler's %s body does not validate against its own schema: %v", where, err)
	}
	return nil
}

// fieldsAgree walks an object both ways, recursing into nested objects.
func fieldsAgree(ref *openapi3.SchemaRef, value any, where, at string) error {
	if ref == nil || ref.Value == nil {
		return fmt.Errorf("%s: the contract has no schema at %s", where, at)
	}
	schema := ref.Value

	obj, isObject := value.(map[string]any)
	if !isObject {
		// Not an object: there is nothing to walk, and VisitJSON in the caller
		// is what checks the value itself.
		return nil
	}
	if len(schema.Properties) == 0 {
		// An object the contract does not describe field by field. Nothing here
		// can be checked field-wise, and saying so is better than passing
		// silently, so this is only reachable for a schema that genuinely
		// describes a free-form object — which this document has none of.
		return fmt.Errorf("%s: the contract describes %s as an object with no properties, "+
			"so nothing checks the %d field(s) the handler emitted there", where, at, len(obj))
	}

	// Direction one: everything the handler emitted is described.
	var undescribed []string
	for name := range obj {
		if schema.Properties[name] == nil {
			undescribed = append(undescribed, name)
		}
	}
	sort.Strings(undescribed)
	if len(undescribed) > 0 {
		return fmt.Errorf("%s: the handler emits %s that provision/openapi.yaml does not describe at %s — "+
			"a field the contract lacks is a field PRSR-50's generated client will not decode",
			where, strings.Join(undescribed, ", "), at)
	}

	// Direction two: everything the contract requires is present.
	var absent []string
	for _, name := range schema.Required {
		if _, has := obj[name]; !has {
			absent = append(absent, name)
		}
	}
	sort.Strings(absent)
	if len(absent) > 0 {
		return fmt.Errorf("%s: provision/openapi.yaml requires %s at %s and the handler omitted %s — "+
			"the contract is promising a field the connector will read",
			where, strings.Join(absent, ", "), at, plural(len(absent)))
	}

	// And down, for the nested account object.
	for name, prop := range schema.Properties {
		child, present := obj[name]
		if !present {
			continue
		}
		if prop.Value != nil && len(prop.Value.Properties) > 0 {
			if err := fieldsAgree(prop, child, where, at+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// ---------------------------------------------------------------------------
// The controls — the checker is watched failing
//
// A contract check that has only ever run against an agreeing pair is a check
// nobody has seen work, which is this repository's own standing argument for
// negative controls (verify.sh's planted line, the store's fault switches). Each
// case below mutates the PARSED document — never the file — so the real handlers
// stay untouched and what is being measured is the checker.

func TestTheContractCheckerHasTeeth(t *testing.T) {
	ensureOp := operation{"/accounts", http.MethodPost}
	created := contractCase{
		name:           "ensure creates",
		op:             ensureOp,
		request:        func() *http.Request { return ensureRequest(`{"email":"ada@example.com"}`) },
		state:          stub{ensured: ensured(store.PersonCreated, "")},
		wantStatus:     http.StatusCreated,
		wantStoreCalls: 1,
	}

	for _, ctl := range []struct {
		name    string
		mutate  func(*testing.T, *openapi3.T)
		wantSub string
	}{
		{
			// A field the handler emits that the contract does not describe.
			name: "a described field is taken away",
			mutate: func(t *testing.T, doc *openapi3.T) {
				acct := doc.Components.Schemas["Account"].Value
				if acct.Properties["handle"] == nil {
					t.Fatal("the control needs Account.handle to exist")
				}
				delete(acct.Properties, "handle")
			},
			wantSub: "does not describe",
		},
		{
			// A required field the handler does not emit.
			name: "the contract requires a field nothing sends",
			mutate: func(t *testing.T, doc *openapi3.T) {
				resp := doc.Components.Schemas["EnsureResponse"].Value
				resp.Required = append(resp.Required, "invitation_url")
				resp.Properties["invitation_url"] = &openapi3.SchemaRef{Value: openapi3.NewStringSchema()}
			},
			wantSub: "requires invitation_url",
		},
		{
			// A status the handler answers that the contract stops documenting.
			name: "the created status is removed",
			mutate: func(t *testing.T, doc *openapi3.T) {
				op := doc.Paths.Find("/accounts").GetOperation(http.MethodPost)
				if op.Responses.Status(http.StatusCreated) == nil {
					t.Fatal("the control needs a documented 201")
				}
				op.Responses.Delete("201")
			},
			wantSub: "the contract does not document it",
		},
		{
			// A value the schema would refuse — the half the copied patterns buy.
			name: "the timestamp pattern stops matching",
			mutate: func(t *testing.T, doc *openapi3.T) {
				ts := doc.Components.Schemas["Timestamp"].Value
				ts.Pattern = "^never-matches-anything$"
				// kin-openapi compiles the pattern lazily and caches it; clearing
				// the compiled form is what makes the new one take effect.
				ts.WithPattern(ts.Pattern)
			},
			wantSub: "does not validate against its own schema",
		},
	} {
		t.Run(ctl.name, func(t *testing.T) {
			doc := loadProvisionSpec(t)

			// The pair agrees before the mutation, or the control proves nothing.
			rec := created.run(t)
			if err := conforms(doc, created.op, rec); err != nil {
				t.Fatalf("the unmutated contract already disagrees with the handler: %v", err)
			}

			ctl.mutate(t, doc)
			err := conforms(doc, created.op, rec)
			if err == nil {
				t.Fatalf("the checker accepted a mutated contract — %s made no difference, so criterion 12's "+
					"test would not notice a handler and the file disagreeing", ctl.name)
			}
			if !strings.Contains(err.Error(), ctl.wantSub) {
				t.Errorf("the checker failed, with a message that does not name the disagreement: %v", err)
			}
			t.Logf("%s: %v", ctl.name, err)
		})
	}
}

// TestTheContractIsNotPartOfTheWireSchema keeps criterion 12's last clause true
// mechanically rather than by inspection: this file is in provision/, no
// generator has ever been pointed at it, and it must not acquire the generated
// documents' header by somebody copying one.
func TestTheContractIsNotPartOfTheWireSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(provisionSpecPath))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("DO NOT EDIT")) || bytes.Contains(raw, []byte("GENERATED from")) {
		t.Error("provision/openapi.yaml carries a generated-file marker. It is hand-written and " +
			"nothing regenerates it; a marker here would send the next reader looking for a generator " +
			"and then hand-editing the one document in this repository that is meant to be hand-edited.")
	}
	// The generated wire document lives in schema/ and is 85 vectors' worth of
	// contract. This one names no vectors at all.
	if bytes.Contains(raw, []byte("catenary.wire.v1")) {
		t.Error("provision/openapi.yaml references the wire schema. No client calls this surface, " +
			"which is why it is not in it.")
	}
}
