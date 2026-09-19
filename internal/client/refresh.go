package client

// CANT-124: refreshing before you need to, and also when you are refused.
//
// These are CANT-31's record §1 and §2, in the reference client. The rules are
// the record's and are implemented twice more, in TypeScript (CANT-35) and in
// Dart (CANT-42); where this file and the record disagree, fix the record
// first and then all three.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/magos/catenary/internal/wire"
)

const (
	// refreshFloor is the 60 s in `max(60 s, ⅓ of the served lifetime)`.
	refreshFloor = 60 * time.Second
	// refreshTimeout bounds one POST /refresh.
	refreshTimeout = 30 * time.Second
	// maxRefreshBody bounds its response: two 43-character tokens and two
	// timestamps, with room to spare for an error body worth logging.
	maxRefreshBody = 16 << 10
)

// errUnauthorized is a 401 that is Catenary's OWN — the body is
// `{"code":"unauthorized"}`. A 401 from a hop in front (the deployed path runs
// through cf-access-jwt and cf-access-guard) says nothing about the Catenary
// credential, is not this error, and never triggers a refresh.
var errUnauthorized = errors.New("client: unauthorized")

// isCatenaryUnauthorized reports whether a response is Catenary's own 401.
func isCatenaryUnauthorized(status int, body []byte) bool {
	if status != http.StatusUnauthorized {
		return false
	}
	var e struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(body, &e) == nil && e.Code == "unauthorized"
}

// wallNow is the device's WALL clock, and only that.
//
// Round(0) STRIPS THE MONOTONIC READING, and that is the point of it. Go's
// monotonic clock stops while the machine is suspended, and time.Time.Sub
// prefers it whenever both operands carry one — so a laptop that slept for six
// hours would measure six hours as a few seconds and conclude a long-dead
// token had most of its life left. The wall clock counts the suspend. Its own
// error, a device clock that is simply wrong, is what Credential.ClockOffset
// corrects.
func (c *Client) wallNow() time.Time {
	now := time.Now
	if c.cfg.Now != nil {
		now = c.cfg.Now
	}
	return now().Round(0)
}

// WithServerDate records when the server says this pair was issued, and how
// far the device's wall clock stood from the server's at that moment. date is
// the response's HTTP `Date`; deviceNow is the device wall clock when the
// response arrived. Both are captured "at the moment the expiry is learned",
// which is the only moment the two clocks are known to describe the same
// instant.
func (cr Credential) WithServerDate(date, deviceNow time.Time) Credential {
	date, deviceNow = date.Round(0), deviceNow.Round(0)
	cr.AccessIssuedAt = date
	cr.ClockOffset = date.Sub(deviceNow)
	return cr
}

// refreshThreshold is `max(60 s, ⅓ of the served lifetime)`. The served
// lifetime is measured on the SERVER's clock at both ends — its expiry minus
// its own Date — so a wrong device clock cannot stretch or shrink it. A
// credential with no recorded issue time (one enrolled without going through
// HTTP, as the rigs' are) has no known lifetime and gets the floor.
func refreshThreshold(cr Credential) time.Duration {
	if cr.AccessIssuedAt.IsZero() {
		return refreshFloor
	}
	return max(refreshFloor, cr.AccessExpiresAt.Sub(cr.AccessIssuedAt)/3)
}

// refreshDue reports whether less than the threshold remains on the access
// token, as of deviceNow corrected by the offset persisted with the credential.
// A pure function of durable state and one wall-clock reading, so a cold start
// decides exactly as the process that wrote the credential would have.
func refreshDue(cr Credential, deviceNow time.Time) bool {
	if cr.AccessExpiresAt.IsZero() {
		return false // no expiry was ever learned; the reactive path is the net
	}
	serverNow := deviceNow.Round(0).Add(cr.ClockOffset)
	return cr.AccessExpiresAt.Sub(serverNow) < refreshThreshold(cr)
}

// RefreshIfDue is the PROACTIVE half. Run calls it before every dial — and
// therefore before the /sync that goes out beside the upgrade, which is pulled
// after it — and a caller may call it on its own clock too: waking from a
// suspend is the obvious moment. It does nothing unless Config.Refresh is set.
//
// Run logs a failure and dials anyway with the pair it has: if that pair
// really is dead, the reactive half finds out.
func (c *Client) RefreshIfDue(ctx context.Context) error {
	if !c.cfg.Refresh || !refreshDue(c.credential(), c.wallNow()) {
		return nil
	}
	return c.refresh(ctx, func(held Credential) bool { return refreshDue(held, c.wallNow()) })
}

// refreshAfter401 is the REACTIVE half: the pair `used` was just refused by
// Catenary. It refreshes unless some other context has already replaced that
// pair, in which case the replacement is simply what the retry carries.
func (c *Client) refreshAfter401(ctx context.Context, used Credential) error {
	return c.refresh(ctx, func(held Credential) bool { return held.AccessToken == used.AccessToken })
}

// refresh is SINGLE-FLIGHT PER CREDENTIAL, NOT PER PROCESS (record §2).
//
// The lock is the Journal's, so every Client over this store — two tabs, an
// app and its push worker — queues on the same one. stillNeeded is asked
// UNDER that lock about the credential as it is now persisted: a caller that
// waited behind another context's refresh finds the pair already rotated,
// skips, and uses the result. Two refreshes of one pair is not a wasted round
// trip; outside the grace window the second is a replay, and a replay revokes
// the device (CANT-29).
//
// PERSIST BEFORE USE. The rotated pair is in the Journal before the lock is
// released and before anything presents it — CANT-24 obligation 1's persist
// before render, applied to a credential.
func (c *Client) refresh(ctx context.Context, stillNeeded func(held Credential) bool) error {
	if !c.cfg.Faults.RefreshUnlocked {
		release, err := c.j.lockRefresh(ctx)
		if err != nil {
			return err
		}
		defer release()
	}

	held := c.credential()
	if !stillNeeded(held) {
		c.mu.Lock()
		c.stats.RefreshesSkipped++
		c.mu.Unlock()
		return nil
	}

	next, err := c.postRefresh(ctx, held)
	if err != nil {
		c.mu.Lock()
		c.stats.RefreshErrors++
		c.mu.Unlock()
		return err
	}
	if err := c.rotate(next); err != nil {
		return err
	}
	c.mu.Lock()
	c.stats.Refreshes++
	c.mu.Unlock()
	c.notify()
	return nil
}

// postRefresh is one POST /refresh, presenting held's refresh token.
func (c *Client) postRefresh(ctx context.Context, held Credential) (Credential, error) {
	body, err := json.Marshal(wire.RefreshRequest{RefreshToken: wire.Token(held.RefreshToken)})
	if err != nil {
		return Credential{}, fmt.Errorf("client: refresh: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.httpBase+"/refresh", bytes.NewReader(body))
	if err != nil {
		return Credential{}, fmt.Errorf("client: refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range c.cfg.ExtraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("client: refresh: %w", err)
	}
	defer resp.Body.Close()
	arrived := c.wallNow()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if err != nil {
		return Credential{}, fmt.Errorf("client: refresh: read: %w", err)
	}
	switch {
	case isCatenaryUnauthorized(resp.StatusCode, raw):
		return Credential{}, fmt.Errorf("client: refresh: %w", errUnauthorized)
	case resp.StatusCode != http.StatusOK:
		if len(raw) > 256 {
			raw = raw[:256]
		}
		return Credential{}, fmt.Errorf("client: refresh: %s: %s", resp.Status, raw)
	}
	var out wire.RefreshResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Credential{}, fmt.Errorf("client: refresh: decode: %w", err)
	}
	next, err := CredentialFromEnroll(wire.EnrollResponse{
		DeviceID:    held.DeviceID,
		AccessToken: out.AccessToken, AccessExpiresAt: out.AccessExpiresAt,
		RefreshToken: out.RefreshToken, RefreshExpiresAt: out.RefreshExpiresAt,
	})
	if err != nil {
		return Credential{}, err
	}
	// A RESPONSE WITH NO USABLE Date KEEPS THE OFFSET IT HAD. The offset is a
	// property of the device's clock, not of one pair, and a stale correction
	// is a better guess than none; the issue time is then the corrected
	// arrival, which is what Date would have said.
	if date, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		next = next.WithServerDate(date, arrived)
	} else {
		next.ClockOffset = held.ClockOffset
		next.AccessIssuedAt = arrived.Add(held.ClockOffset)
	}
	return next, nil
}
