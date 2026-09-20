package client

// CANT-124: refreshing before you need to, and also when you are refused.
//
// These are CANT-31's record §1 and §2, in the reference client. The rules are
// the record's and are implemented twice more, in TypeScript (CANT-35) and in
// Dart (CANT-42); where this file and the record disagree, fix the record
// first and then all three.
//
// CANT-126 adds §3, the client half of the proposed-successor exchange: every
// refresh proposes its own successor and writes that down before it is sent,
// so a rotation whose response never arrives is one the device can still find.
// See exchange.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

	// proposalBytes is what the server's own tokens are made of: 32 bytes from
	// a CSPRNG, which base64url without padding renders as the wire's
	// 43-character Token. Restated rather than imported from internal/store,
	// for wireTimestampLayout's reason; TestAProposalIsAWireToken pins it to
	// the generated decoder.
	proposalBytes = 32
	// maxFreshProposals bounds how often one refresh answers `fresh_proposal`
	// with another proposal. One collision is a reused secret; four in a row
	// is a generator that cannot be trusted, or a server that says this to
	// everything, and neither is cured by a fifth.
	maxFreshProposals = 3
	// maxRetryAfter caps the wait a `fresh_proposal` asks for. The single-
	// flight lock is held across it.
	maxRetryAfter = 5 * time.Second
	// walkSlack is how many requests one refresh may send beyond one per link.
	// The walk is downward and ends; the slack is for the answers that send it
	// back up (`present_proposal`) or round again (`fresh_proposal`).
	walkSlack = 8
)

// errUnauthorized is a 401 that is Catenary's OWN — the body is
// `{"code":"unauthorized"}`. A 401 from a hop in front (the deployed path runs
// through cf-access-jwt and cf-access-guard) says nothing about the Catenary
// credential, is not this error, and never triggers a refresh.
var errUnauthorized = errors.New("client: unauthorized")

var (
	// errPresentProposal is the 503 `present_proposal`: the rotation being
	// asked for ALREADY COMMITTED, moments ago, and the proposal is the
	// device's refresh token now. Sending the same request again is the one
	// wrong answer — past the grace window those bytes are a replay.
	errPresentProposal = errors.New("client: refresh: already rotated into the proposal")
	// errChainMoved is a refresh that found the persisted credential rotated
	// underneath it by a context that does not share the lock. Not a failure:
	// what is there now is what the caller uses.
	errChainMoved = errors.New("client: refresh: the persisted credential moved")
	// errWalkExhausted is a walk the server kept redirecting. Not terminal.
	errWalkExhausted = errors.New("client: refresh: gave up after too many answers that settled nothing")
)

// freshProposalError is the 503 `fresh_proposal`: the presented token is STILL
// GOOD and its proposal can never commit, because it collides with a token the
// server already stores. Same token, a newly minted proposal, after `after`.
type freshProposalError struct{ after time.Duration }

func (e *freshProposalError) Error() string {
	return "client: refresh: the proposal collided with a stored token"
}

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

	next, err := c.exchange(ctx, held)
	if errors.Is(err, errChainMoved) {
		c.mu.Lock()
		c.stats.RefreshesSkipped++
		c.mu.Unlock()
		return nil
	}
	if errors.Is(err, errUnauthorized) {
		return c.refused(held, err)
	}
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

// refused is record §5's /refresh row: Catenary's OWN 401 to the refresh token
// `presented`. It is terminal only when BOTH qualifiers hold, and postRefresh
// has already established the first — the body was `{"code":"unauthorized"}`,
// so this is not a proxy or an expired Access session in front, whose 401 says
// nothing about the Catenary credential.
//
// The second is checked here: THE STORED CREDENTIAL IS STILL THE ONE
// PRESENTED. A context that does not share this lock — another process over
// the same storage — may have rotated the pair while this request was in
// flight, and the server then refuses the token this one sent because it is
// spent, not because the device is gone. Re-read before concluding anything;
// if the pair moved, the caller simply uses what is there now.
//
// This is how a device revoked WHILE IT WAS DISCONNECTED stops, which is the
// common case: it never sees a close code. Its upgrade is refused with a 401,
// the /sync beside it is refused with a 401, the reactive refresh that answers
// that is refused here, and the client stops instead of dialing for ever.
func (c *Client) refused(presented Credential, err error) error {
	if now := c.credential(); now.RefreshToken != presented.RefreshToken {
		c.mu.Lock()
		c.stats.RefreshesSkipped++
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	c.stats.RefreshErrors++
	c.mu.Unlock()
	if c.cfg.Faults.NeverTerminal {
		return err
	}
	c.stop(Terminal{Kind: TerminalCredential, Reason: "POST /refresh: Catenary refused the stored refresh token"})
	// THE TERMINAL THAT WON, not necessarily this one: stop keeps the first,
	// and a close code can race a refused refresh. The error handed back and
	// the state Status names must be the same terminal.
	c.mu.Lock()
	won := c.terminal
	c.mu.Unlock()
	return &TerminalError{won}
}

// exchange is record §3: WHEN THE OUTCOME OF A REFRESH IS UNKNOWN, WALK THE
// CHAIN. It returns the pair the server answered with, errUnauthorized when
// Catenary refused the OLDEST token — the only refusal that can mean the
// credential is gone, and refused decides whether it does — and any other
// error for an attempt that settled nothing.
//
// A rotation can commit and its response never arrive: the request was the
// only one there ever was, so single-flight cannot help, and the device is
// left holding a token that may be spent without knowing its successor. So the
// device CHOOSES the successor, and Journal.propose makes that choice durable
// before the request leaves. After a lost response the successor is a string
// it already has.
//
// NEWEST FIRST. The chain's last proposal is the newest token that can exist,
// so that is what is presented, with a newly minted proposal of its own. If
// the lost rotation committed it is live and this is an ordinary refresh. If
// it did not, the server has never heard of it and says 401 — which costs
// nothing, because a token that never existed belongs to no family and
// revokes none. The other order is the fatal one: a spent token presented
// outside the grace window is a replay, and a replay revokes the device.
//
// ONE STEP BACK PER 401, REUSING THE ORIGINAL PROPOSAL. Catenary's own 401
// for a token that is not the oldest means only "not that one": step to the
// token before it and present it with the proposal it was FIRST presented
// with. If that first request is still on its way, the two now race to the
// SAME successor, and the loser collides and is told `present_proposal`
// instead of being refused as a replay. A 401 that is NOT Catenary's — a hop
// in front — is not an answer about any token and never steps the walk: it
// ends the attempt like any other unknown outcome.
//
// THE CHAIN GROWS BY ONE LINK FOR EVERY ATTEMPT THAT SETTLES NOTHING, and it
// is cleared by the first answer (Journal.Rotate). Nothing shorter is safe —
// each link is a token that may be live — so a device that goes on attempting
// refreshes into a dead network comes back to a walk as long as its absence.
// The record does not bound that and neither does this; it is CANT-127's.
func (c *Client) exchange(ctx context.Context, held Credential) (Credential, error) {
	chain := c.j.Chain()
	token := held.RefreshToken
	if n := len(chain); n > 0 && !c.cfg.Faults.NoChain {
		token = chain[n-1].Proposal
	}
	replace, collisions := false, 0
	for steps := len(chain) + walkSlack; steps > 0; steps-- {
		proposal, ok, err := c.propose(token, replace)
		if err != nil {
			return Credential{}, err
		}
		if !ok {
			return Credential{}, errChainMoved
		}
		replace = false

		next, err := c.postRefresh(ctx, held, token, proposal)
		var collided *freshProposalError
		switch {
		case err == nil:
			// THE RESPONSE'S refresh_token, NEVER THE PROPOSAL ON FAITH. A
			// server that predates the field ignores it and mints its own.
			return next, nil
		case errors.Is(err, errUnauthorized):
			older, ok := c.j.older(token)
			if !ok {
				return Credential{}, err
			}
			c.mu.Lock()
			c.stats.RefreshWalkBacks++
			c.mu.Unlock()
			token = older
		case errors.Is(err, errPresentProposal):
			// STOP PRESENTING THAT TOKEN. Its proposal is the newest token
			// now; if this walk already has a link for it, that link's
			// original proposal goes with it.
			token = proposal
		case errors.As(err, &collided):
			if collisions++; collisions > maxFreshProposals {
				return Credential{}, err
			}
			select {
			case <-time.After(min(collided.after, maxRetryAfter)):
			case <-ctx.Done():
				return Credential{}, ctx.Err()
			}
			replace = true
		default:
			return Credential{}, err
		}
	}
	return Credential{}, errWalkExhausted
}

// propose is Journal.propose behind the Kill guard, as rotate is Journal.Rotate
// behind it: a client killed before its proposal was written sends nothing.
func (c *Client) propose(token string, replace bool) (proposal string, ok bool, err error) {
	if c.cfg.Faults.NoChain {
		proposal, err = c.mintProposal()
		return proposal, err == nil, err
	}
	c.j.mu.Lock()
	defer c.j.mu.Unlock()
	if c.killed.Load() {
		return "", false, ErrKilled
	}
	return c.j.propose(token, replace || c.cfg.Faults.ProposeAfresh, c.mintProposal)
}

// mintProposal is a successor secret: 32 CSPRNG bytes in the wire's Token
// shape, exactly as the server mints its own, and NEVER DERIVED FROM ANYTHING
// — not the token it succeeds, not a counter, not the clock. It is a bearer
// credential from the moment the server stores its hash.
func (c *Client) mintProposal() (string, error) {
	src := c.cfg.Rand
	if src == nil {
		src = rand.Reader
	}
	raw := make([]byte, proposalBytes)
	if _, err := io.ReadFull(src, raw); err != nil {
		return "", fmt.Errorf("client: refresh: mint a proposal: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// postRefresh is one POST /refresh: token, presented with proposal. held is
// the confirmed pair, for the two things a rotation keeps — the device, and
// the clock offset when the response carries no Date.
func (c *Client) postRefresh(ctx context.Context, held Credential, token, proposal string) (Credential, error) {
	proposed := wire.Token(proposal)
	body, err := json.Marshal(wire.RefreshRequest{RefreshToken: wire.Token(token), ProposedRefreshToken: &proposed})
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
	case resp.StatusCode == http.StatusServiceUnavailable:
		// `retry` SAYS WHICH, and the two ask for opposite things (record §5).
		// A 503 without one the client recognises — a proxy's, or a value a
		// later server adds — is an unknown outcome and falls through.
		var e struct {
			Retry string `json:"retry"`
		}
		_ = json.Unmarshal(raw, &e)
		switch e.Retry {
		case "present_proposal":
			return Credential{}, errPresentProposal
		case "fresh_proposal":
			secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			return Credential{}, &freshProposalError{after: time.Duration(max(secs, 0)) * time.Second}
		}
		fallthrough
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
