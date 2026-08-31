// Package catenary is a SPIKE-LEVEL Purser connector for Catenary — R6
// (IDEA-29). It is not production code and is not in the Purser repo: the
// point is to find out where Purser's Provision/Reconcile/Deprovision contract
// pinches against an identity model it was not designed for, and the deliverable
// is that finding rather than this file.
//
// Catenary is a genuinely different shape from the existing four connectors:
// per-device credentials, individually revocable refresh tokens, and a device
// list that is part of the product. Each of the gate's three questions is
// answered in the doc comment of the method it is about.
package catenary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Einlanzerous/purser/internal/connector"
)

// Config configures the connector.
type Config struct {
	// BaseURL is Catenary's internal admin API.
	BaseURL string
	// AdminToken authenticates to /admin, which sits behind Cloudflare Access
	// in the deployed topology (D2) — so this is a service token, not a session.
	AdminToken string
	// AppURL is where the invited person redeems their enrolment token.
	AppURL     string
	HTTPClient *http.Client

	// RevokeDevicesFirst inverts Deprovision's ordering. It exists ONLY to
	// demonstrate the finding in the test — see Deprovision. Production code
	// would not have this knob, because only one of the two orders is correct.
	RevokeDevicesFirst bool
}

// Connector provisions Catenary accounts.
type Connector struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) (*Connector, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("catenary: BaseURL is required")
	}
	if strings.TrimSpace(cfg.AdminToken) == "" {
		return nil, errors.New("catenary: AdminToken is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Connector{cfg: cfg, http: hc}, nil
}

func (c *Connector) Key() string         { return "catenary" }
func (c *Connector) DisplayName() string { return "Catenary" }
func (c *Connector) Icon() string        { return "🚋" }

type account struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Devices     []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Revoked bool   `json:"revoked"`
	} `json:"devices"`
}

// Provision creates the person's Catenary account and returns ONE enrolment
// token.
//
// GATE QUESTION 1 — "Provision returning per-device credentials. The existing
// connectors mint one secret per person per service; Catenary's unit is a
// device. Does the contract express that, or does it want one secret and get a
// list?"
//
// ANSWER: it wants one secret and gets one secret. The mismatch is real in
// Catenary's data model and does not reach this boundary, because of WHEN
// things happen. At provisioning time the person has ZERO devices — they have
// not installed anything yet. So the only credential that can exist is a
// bootstrap enrolment token, which is exactly one string. Per-device refresh
// tokens are minted by Catenary when a device redeems it, long after Purser is
// out of the picture.
//
// Purser provisions PEOPLE. Catenary tracks DEVICES. The two never have to
// agree on a unit because they never exchange one. `connector.Result.Secret`
// fits without strain, and `Extra` is not needed.
//
// Idempotent: an existing account re-issues an enrolment token rather than
// failing. That is a rotation, which Provision is permitted to do (unlike
// Reconcile) — and it is the right behaviour, since a single-use token that has
// already been redeemed cannot be handed out again, so the Lyceum-style "409 →
// success with no secret" branch would leave a re-invited person with nothing
// to redeem.
func (c *Connector) Provision(ctx context.Context, in connector.Input) (connector.Result, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.Result{}, errors.New("catenary: an email is required to create an account")
	}
	name := strings.TrimSpace(in.PersonName)
	if name == "" {
		name = email
	}

	status, raw, err := c.do(ctx, http.MethodPost, "/admin/accounts",
		map[string]any{"email": email, "display_name": name})
	if err != nil {
		return connector.Result{}, err
	}

	switch status {
	case http.StatusCreated, http.StatusOK:
		var cr struct {
			Account        account `json:"account"`
			EnrolmentToken string  `json:"enrolment_token"`
		}
		if err := json.Unmarshal(raw, &cr); err != nil {
			return connector.Result{}, fmt.Errorf("catenary: decode create response: %w", err)
		}
		return c.result(cr.Account.ID, cr.Account.DisplayName, cr.EnrolmentToken), nil

	case http.StatusConflict:
		// Already has an account. Re-issue so a re-invite is useful.
		existing, err := c.lookup(ctx, email)
		if err != nil {
			return connector.Result{}, err
		}
		if existing == nil {
			return connector.Result{}, errors.New("catenary: 409 on create but no account found on lookup")
		}
		st, raw, err := c.do(ctx, http.MethodPost, "/admin/accounts/"+existing.ID+"/enrolment", nil)
		if err != nil {
			return connector.Result{}, err
		}
		if st != http.StatusOK {
			return connector.Result{}, apiError("reissue enrolment", st, raw)
		}
		var rr struct {
			EnrolmentToken string `json:"enrolment_token"`
		}
		if err := json.Unmarshal(raw, &rr); err != nil {
			return connector.Result{}, fmt.Errorf("catenary: decode enrolment response: %w", err)
		}
		return c.result(existing.ID, existing.DisplayName, rr.EnrolmentToken), nil

	default:
		return connector.Result{}, apiError("create account", status, raw)
	}
}

func (c *Connector) result(id, username, token string) connector.Result {
	return connector.Result{
		ExternalID:  id,
		Username:    username,
		Secret:      token,
		SecretLabel: "enrolment token (single-use, redeemed on your first device)",
		LoginURL:    c.cfg.AppURL,
		Instructions: "Install Catenary, then paste this on the sign-in screen. " +
			"It registers that device; each further device is added from an " +
			"already signed-in one and can be revoked on its own.",
	}
}

// Reconcile reports whether the person currently has access. Read-only, and it
// mints nothing.
//
// GATE QUESTION 2 — "Purser records secrets only as a sha256 hash, and
// reconcile is read-only by contract. What does it compare against when the
// upstream truth is 'this person has three registered devices, one revoked'?"
//
// ANSWER: nothing, and it should not try. `ReconcileResult` carries identity
// only, and the question Purser is asking is "does this person have access to
// this service" — which is an ACCOUNT-level fact. Device counts are Catenary's
// operational detail, and Purser has no column to put them in; a connector that
// stuffed "3 devices, 1 revoked" into `Username` to make it visible would be
// inventing a schema in a string field.
//
// The real trap is subtler and worth stating, because it is easy to get wrong:
// Exists must key on ACCOUNT STATUS, not on the row existing. A person whose
// account is active but whose devices have all been revoked still has access —
// they can enrol a new device. A person whose account is disabled does not,
// however many un-revoked device rows are lying around. Returning
// `Exists: row != nil` would report the second person as having access, and an
// audit would then leave a disabled account looking healthy.
func (c *Connector) Reconcile(ctx context.Context, in connector.Input) (connector.ReconcileResult, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.ReconcileResult{}, errors.New("catenary: an email is required to reconcile")
	}
	a, err := c.lookup(ctx, email)
	if err != nil {
		return connector.ReconcileResult{}, err
	}
	if a == nil || a.Status != "active" {
		return connector.ReconcileResult{Exists: false}, nil
	}
	return connector.ReconcileResult{Exists: true, ExternalID: a.ID, Username: a.DisplayName}, nil
}

// Deprovision revokes the person's access: disable the account, then revoke
// every device's refresh token.
//
// GATE QUESTION 3 — "Revoking a person means revoking every device's refresh
// token. That is a fan-out the other three connectors do not have."
//
// ANSWER: the fan-out itself is fine — it is internal to the connector, and the
// contract never promised a single upstream call. What the fan-out introduces
// is something the other three genuinely do not have: **Deprovision is no
// longer atomic upstream, so it can half-succeed.**
//
// That matters because of how it interacts with Reconcile. Revoke the devices
// first and fail on the third, and you are left with an ACTIVE account whose
// devices are partly revoked — and Reconcile, which can only see account
// status, correctly reports `Exists: true`. The person still has access (they
// can enrol a new device), so that is not even a wrong answer. But it means a
// half-completed offboard is indistinguishable from one that never started,
// on the one command you cannot take back.
//
// Disable the account FIRST and the failure mode inverts: a partial failure
// leaves a DISABLED account and some un-revoked device rows. Access is
// genuinely gone — a disabled account cannot refresh a token or enrol a device
// — Reconcile reports `Exists: false` truthfully, and the retry mops up the
// stragglers. Idempotent, and safe to leave half-done.
//
// So the ordering is load-bearing, and it is the kind of thing you only find
// by building the thing. Config.RevokeDevicesFirst exists solely so the test
// can demonstrate the wrong order; production code would not offer the choice.
func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return errors.New("catenary: an email is required to deprovision")
	}
	a, err := c.lookup(ctx, email)
	if err != nil {
		return err
	}
	if a == nil {
		return nil // nothing upstream: a success, so a failed-only retry is safe
	}

	revokeAll := func() error {
		for _, d := range a.Devices {
			if d.Revoked {
				continue
			}
			st, raw, err := c.do(ctx, http.MethodPost,
				"/admin/accounts/"+a.ID+"/devices/"+d.ID+"/revoke", nil)
			if err != nil {
				return err
			}
			if st != http.StatusNoContent && st != http.StatusOK && st != http.StatusNotFound {
				return apiError("revoke device "+d.ID, st, raw)
			}
		}
		return nil
	}

	disable := func() error {
		st, raw, err := c.do(ctx, http.MethodPost, "/admin/accounts/"+a.ID+"/disable", nil)
		if err != nil {
			return err
		}
		if st != http.StatusNoContent && st != http.StatusOK {
			return apiError("disable account", st, raw)
		}
		return nil
	}

	if c.cfg.RevokeDevicesFirst {
		if err := revokeAll(); err != nil {
			return err
		}
		return disable()
	}

	// Correct order: take away access, then clean up.
	if err := disable(); err != nil {
		return err
	}
	return revokeAll()
}

func (c *Connector) lookup(ctx context.Context, email string) (*account, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/admin/accounts?email="+strings.ToLower(email), nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var a account
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("catenary: decode account: %w", err)
		}
		return &a, nil
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, apiError("lookup account", status, raw)
	}
}

func (c *Connector) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("catenary: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("catenary: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.AdminToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("catenary: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("catenary: read body: %w", err)
	}
	return resp.StatusCode, raw, nil
}

func apiError(op string, status int, raw []byte) error {
	return fmt.Errorf("catenary: %s: %d: %s", op, status, strings.TrimSpace(string(raw)))
}

// Compile-time proof that the spike connector satisfies the real contract.
// This is a meaningful part of the R6 answer on its own: the interface did not
// need widening, a new method, or an optional side-channel.
var _ connector.Connector = (*Connector)(nil)
