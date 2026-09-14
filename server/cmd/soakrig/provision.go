package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

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
		if _, err := h.pool.Exec(ctx,
			`INSERT INTO users (id, handle, display_name) VALUES ($1, $2, $3)`,
			userID, name, name); err != nil {
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
// install makes, not RedeemEnrollment reached into directly.
func (h *harness) enroll(ctx context.Context, index int, userID uuid.UUID, deviceName string) (wire.EnrollResponse, error) {
	if h.cfg.debugFailProvision[index] {
		return wire.EnrollResponse{}, fmt.Errorf("planted provisioning failure for client %d (test)", index)
	}

	issued, err := h.store.IssueEnrollmentToken(ctx, userID)
	if err != nil {
		return wire.EnrollResponse{}, fmt.Errorf("issue enrollment token: %w", err)
	}

	body, err := json.Marshal(wire.EnrollRequest{EnrollmentToken: wire.Token(issued.Plaintext), DeviceName: deviceName})
	if err != nil {
		return wire.EnrollResponse{}, fmt.Errorf("encode EnrollRequest: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, h.baseURL+"/enroll", bytes.NewReader(body))
	if err != nil {
		return wire.EnrollResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
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

// wid mirrors cmd/catenary's own helper of the same name: the wire's Uuid is
// a lowercase-hyphenated string, and every internal id is a uuid.UUID.
func wid(id uuid.UUID) wire.Uuid { return wire.Uuid(id.String()) }
