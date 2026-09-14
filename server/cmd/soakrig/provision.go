package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magos/catenary/internal/store"
	"github.com/magos/catenary/internal/wire"
)

// provisionedClient is one account's outcome: err is nil exactly when
// everything from mkUser through POST /enroll succeeded.
type provisionedClient struct {
	index  int
	name   string
	userID uuid.UUID
	enroll wire.EnrollResponse
	err    error
}

// provisionUsers creates one shared conversation and N accounts through
// direct INSERTs — into `users` and `conversation_members`, NEITHER a
// credential table; internal/store/tokens_guard_test.go's
// TestOnlyTheStorePackageReadsACredentialTable bans naming access_tokens,
// refresh_tokens or enrollment_tokens outside internal/store, and this file
// never does — then mints and redeems an enrollment token per account through
// the REAL POST /enroll path, exactly as an install would. The credential
// itself is minted by store.IssueEnrollmentToken, not by hand: that function,
// not a second implementation here, is what CANT-28 built for this.
func (h *harness) provisionUsers(ctx context.Context) ([]provisionedClient, uuid.UUID) {
	runID := uuid.NewString()[:8]
	room := uuid.New()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO conversations (id, kind, name) VALUES ($1, 'group', $2)`,
		room, "soak room "+runID); err != nil {
		h.harnessError("create the soak conversation: %v", err)
		return nil, uuid.Nil
	}

	out := make([]provisionedClient, h.cfg.N)
	for i := 0; i < h.cfg.N; i++ {
		if ctx.Err() != nil {
			out[i] = provisionedClient{index: i, err: ctx.Err()}
			continue
		}
		name := fmt.Sprintf("soak-%s-%02d", runID, i)
		userID := uuid.New()
		if err := insertUser(ctx, h.pool, userID, name, name); err != nil {
			out[i] = provisionedClient{index: i, name: name, err: fmt.Errorf("create user: %w", err)}
			continue
		}
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`,
			room, userID); err != nil {
			out[i] = provisionedClient{index: i, name: name, err: fmt.Errorf("join the soak conversation: %w", err)}
			continue
		}
		enrolled, err := h.enroll(ctx, i, userID, name)
		out[i] = provisionedClient{index: i, name: name, userID: userID, enroll: enrolled, err: err}
	}
	return out, room
}

// enroll mints a bootstrap token with the real store function and redeems it
// over an actual HTTP POST /enroll against the subprocess — the same call an
// install makes, not RedeemEnrollment reached into directly. It is a thin
// wrapper over mintAndRedeem, which CANT-109's `provision` command calls too
// — ONE implementation of "mint, then redeem over the real endpoint" for
// both a soak run's N accounts and a one-off deployed device, per the
// ticket's own instruction not to write a second provisioning path.
func (h *harness) enroll(ctx context.Context, index int, userID uuid.UUID, deviceName string) (wire.EnrollResponse, error) {
	if h.cfg.debugFailProvision[index] {
		return wire.EnrollResponse{}, fmt.Errorf("planted provisioning failure for client %d (test)", index)
	}
	return mintAndRedeem(ctx, h.store, h.baseURL, nil, userID, deviceName)
}

// mintAndRedeem issues an enrollment token for userID with the real store
// function and redeems it over an actual HTTP POST /enroll at baseURL — the
// same call a real install makes, never RedeemEnrollment reached into
// directly. headers rides on the request unchanged; nil is a plain POST,
// which is correct for a local server CANT-109's Access headers do not gate.
func mintAndRedeem(ctx context.Context, st *store.Store, baseURL string, headers http.Header, userID uuid.UUID, deviceName string) (wire.EnrollResponse, error) {
	issued, err := st.IssueEnrollmentToken(ctx, userID)
	if err != nil {
		return wire.EnrollResponse{}, fmt.Errorf("issue enrollment token: %w", err)
	}

	body, err := json.Marshal(wire.EnrollRequest{EnrollmentToken: wire.Token(issued.Plaintext), DeviceName: deviceName})
	if err != nil {
		return wire.EnrollResponse{}, fmt.Errorf("encode EnrollRequest: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, baseURL+"/enroll", bytes.NewReader(body))
	if err != nil {
		return wire.EnrollResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return wire.EnrollResponse{}, fmt.Errorf("POST /enroll: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return wire.EnrollResponse{}, fmt.Errorf("POST /enroll: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return wire.EnrollResponse{}, fmt.Errorf("POST /enroll: %s: %s", resp.Status, respBody)
	}
	var out wire.EnrollResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return wire.EnrollResponse{}, fmt.Errorf("decode EnrollResponse: %w", err)
	}
	return out, nil
}

// insertUser is the one direct INSERT into `users` this package performs —
// never into a credential table (access_tokens, refresh_tokens,
// enrollment_tokens): internal/store/tokens_guard_test.go's
// TestOnlyTheStorePackageReadsACredentialTable bans naming those outside
// internal/store, and this scan does not even reach this module (server/ is
// skipped there), so this comment is the guard for code the test cannot see.
// A caller that wants to know whether the handle was already taken checks
// isUniqueViolation on the returned error rather than a second query racing
// this insert.
func insertUser(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, handle, displayName string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO users (id, handle, display_name) VALUES ($1, $2, $3)`,
		id, handle, displayName)
	return err
}

// isUniqueViolation reports whether err is a 23505, mirroring
// internal/store/messages.go's helper of the same name — this module cannot
// import an unexported function from another, so the one-line check is
// restated rather than reached into store internals for.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// wid mirrors cmd/catenary's own helper of the same name: the wire's Uuid is
// a lowercase-hyphenated string, and every internal id is a uuid.UUID.
func wid(id uuid.UUID) wire.Uuid { return wire.Uuid(id.String()) }
