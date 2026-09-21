package client

// CANT-129: a token Catenary has refused is presented once per refresh hold.
//
// This is CANT-31's record §5, the row beneath `401 on /sync`, in the reference
// client. The rules are the record's and are implemented twice more, in
// TypeScript (CANT-35) and in Dart (CANT-42); where this file and the record
// disagree, fix the record first and then all three.
//
// WHAT IT REMOVES. A client whose access token Catenary refuses, and whose
// refresh is held by CANT-127's backoff, kept dialing at Run's 5 s ceiling — and
// every cycle was two requests the server was certain to refuse, the upgrade and
// the /sync beside it, each one a line in its log: ~1,440 an hour per device, for
// up to the fifteen-minute cap after a long fault and all day where /sync works
// and /refresh is black-holed. None of them could succeed. Authenticate answers
// 401 only for definite causes — revoked, expired, device revoked, account
// deactivated — and none of them reverses for the same token, so the only thing
// that changes the outcome is a different pair.
//
// THE RULE. Once Catenary's OWN 401 has been received on /sync for the held
// access token, that token is REFUSED. While it is refused the client does not
// dial, and presents it in exactly one kind of request: ONE /sync each time a
// refresh hold ends.
//
// AND THAT ONE REQUEST IS THE ENGINE, NOT THE LEAK, which is the whole reason
// the rule rate-limits instead of stopping. CANT-127's gate opens only on a
// Catenary answer to THIS context later than the last stamped send, and a
// /refresh response is deliberately not one (hold.go, markAnswered) — so for a
// client with no socket, a refused /sync is the only thing that can reopen the
// gate after each failed attempt. It also drives refreshAfter401, which is NOT
// due-gated, so a token the device's clock thinks is live still reaches its
// refresh and a device revoked while disconnected still reaches its terminal.
// Withhold it and the second failed attempt is the last one the client ever
// makes; Faults.NeverPresentRefusedToken is that client, kept as a control.
//
// IT ONLY EVER REMOVES REQUESTS. Every request that does go out — the one /sync
// per hold included — is one the unruled client would have sent, in the same
// order, with the same token and the same proposal. Which token is presented
// with which proposal is exchange's, and is untouched; nothing here is added to
// hold.go, and no request is added anywhere.
//
// WHAT IT IS NOT. It is not a decision on the device clock: the corrected clock
// saying a token has expired marks nothing, because a wrong clock must not be
// able to withhold a request that would have worked. It does not read the
// upgrade's status, which record §5 makes not an input at all — a browser cannot
// see it. It does not close an established socket: that session was authorized
// once at accept and outlives its access token (CANT-28 ruling 2), so it carries
// no token to be refused. And it is INERT without Config.Refresh, because a
// client that cannot refresh can never cure a refused token, and withholding its
// requests would be a silent stop rather than a rate limit.

import (
	"context"
	"time"
)

// withheld says which request a wait is standing down. The two differ in their
// predicate as much as in their counter, which is the rule's whole shape: the
// dial waits for a different PAIR, and the page waits for the end of a HOLD.
type withheld int

const (
	withheldDial withheld = iota // Run's upgrade, and the proactive refresh before it
	withheldSync                 // catchUpLoop's page
)

// markRefused records that CATENARY ITSELF refused this access token on /sync.
// It is called from ONE SITE — fetchWith, where isCatenaryUnauthorized
// recognises Catenary's own 401 — and it is keyed on the token, so the pair
// changing is what ends it and nothing has to be told about a rotation.
//
// ONE INFO LINE, THE FIRST TIME A TOKEN IS MARKED. A refused token can be met
// many times over one hold's worth of retries, and a line per meeting would be
// the stream this ticket exists to remove, moved from the server's log into the
// client's.
func (c *Client) markRefused(token string) {
	if !c.cfg.Refresh || token == "" {
		return
	}
	c.mu.Lock()
	first := c.refusedToken != token
	c.refusedToken = token
	c.mu.Unlock()
	if !first {
		return
	}
	c.log.Info("access token refused by Catenary on /sync; it will not be dialed with, " +
		"and will be presented once per refresh hold until the pair changes")
	c.notify()
}

// refusedNow is the fact the rule acts on: the pair the client would present NOW
// carries the token Catenary refused. Re-read from the Journal every time, never
// cached, so a rotation by this client or by any other context over the same
// credential ends it by construction.
//
// WITH note SET IT IS ALSO WHERE THE MARK IS RETIRED, and that is the rule's
// second and last log line. Status asks with note false: being asked how you are
// must not be able to write a line (hold.go's refreshHoldQuiet, same rule, same
// reason).
func (c *Client) refusedNow(note bool) bool {
	token := c.credential().AccessToken
	c.mu.Lock()
	marked := c.refusedToken
	gone := marked != "" && marked != token
	var dials, syncs int
	if gone && note {
		c.refusedToken = ""
		dials, syncs = c.stats.DialsWithheld, c.stats.SyncsWithheld
	}
	c.mu.Unlock()
	if gone && note {
		c.log.Info("the pair changed; the refused access token is behind us",
			"dials_withheld", dials, "syncs_withheld", syncs)
		c.notify()
	}
	return marked != "" && marked == token
}

// refusedDial reports whether this dial is one the rule withholds. THERE IS NO
// TIME IN THIS ONE: while the access token is refused the client does not dial at
// all, hold or no hold, because the request a hold's end allows is a /sync and
// never an upgrade. It ends when the pair changes, or when the context does.
func (c *Client) refusedDial() bool {
	return !c.cfg.Faults.PresentRefusedToken && c.refusedNow(true)
}

// refusedHold is the wait that bounds the one /sync, AS A PREDICATE RATHER THAN
// A TIMER: hold while the pair's access token is the refused one and the delay
// counted from `last_sent_at` has not elapsed, with the chain, the stamp and the
// clock re-read on every poll.
//
// A PREDICATE, BECAUSE THE DEADLINE IS ON THE WALL CLOCK. `last_sent_at` is a
// wall-clock stamp and time.After runs on the monotonic clock, which stops while
// the machine is suspended — a timer computed once would fire six hours late on a
// laptop that slept, and record §1 forbids scheduling on that clock at all.
// Polling is also what lets the wait end on things that send this context no
// signal whatsoever: another context's rotation, another context's failed attempt
// lengthening the chain, or an answered RefreshIfDue on this one.
//
// THE FOUR WAYS IT ENDS, every one of them read off state rather than told: the
// pair changes, the chain collapses because somebody got an answer, the stamp
// goes absent, or the clock passes `last_sent_at + min(15 min, 5 s × 2^(n−1))`.
// An UNANSWERED explicit RefreshIfDue is the case that looks like an end and is
// not: it lengthens the chain and moves the stamp, so the wait EXTENDS to the new
// deadline, and the count of refused requests does not rise.
//
// ABSENT READS AS ELAPSED, exactly as hold.go's backoff reads it (Client.stamp):
// a chain written before CANT-127 and a stamp in the future from a clock set
// backwards both leave the probe free to go.
func (c *Client) refusedHold() bool {
	if c.cfg.Faults.PresentRefusedToken || !c.refusedNow(true) {
		return false
	}
	if c.cfg.Faults.NeverPresentRefusedToken {
		// THE WITHDRAWN RULE: never again, and so never at all. The client stalls
		// here, which is what the control exists to be watched doing.
		return true
	}
	links := c.j.chainLen()
	if links == 0 {
		return false
	}
	now := c.wallNow()
	sent, stamped := c.stamp(now)
	return stamped && now.Before(sent.Add(refreshDelay(links)))
}

// waitWhileRefused polls one of the two predicates at the DIAL CADENCE and
// reports whether the caller may carry on — false only for a context that ended.
//
// THE CADENCE IS THE DIAL BACKOFF'S CEILING and not a number of its own, because
// it is the rate the requests being withheld would have gone out at: one withheld
// dial, or one withheld /sync, per poll. So the wait cannot be slower to notice a
// changed pair than the loop it stands in for, and Stats' two counters are in the
// same units as the requests they removed.
func (c *Client) waitWhileRefused(ctx context.Context, what withheld) bool {
	for {
		holding := c.refusedDial()
		if what == withheldSync {
			holding = c.refusedHold()
		}
		if !holding {
			return ctx.Err() == nil
		}
		c.mu.Lock()
		if what == withheldDial {
			c.stats.DialsWithheld++
		} else {
			c.stats.SyncsWithheld++
		}
		c.mu.Unlock()
		if !c.pause(ctx, c.backoffMax) {
			return false
		}
	}
}

// pause waits d out, or until ctx ends, and reports whether it waited. It is the
// one seam the rule's wait sleeps through — see Config.Pause for why a test needs
// to substitute it, and why nothing deployed does.
func (c *Client) pause(ctx context.Context, d time.Duration) bool {
	if c.cfg.Pause != nil {
		return c.cfg.Pause(ctx, d)
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// nextRefreshAt is Status.NextRefreshAt: `last_sent_at + min(15 min, 5 s ×
// 2^(n−1))`, the time CANT-127's backoff next allows an automatic refresh and so
// also the time this rule's one /sync goes out. THE ZERO TIME for an empty chain
// or an absent stamp — nothing is being waited for, and record §3 reads an absent
// stamp as elapsed. It is deliberately not "the time the next refresh will
// happen": while the GATE is what holds, that has no answer at all, because the
// gate has no time and opens on an answer.
func (c *Client) nextRefreshAt() time.Time {
	links := c.j.chainLen()
	if links == 0 {
		return time.Time{}
	}
	sent, stamped := c.stamp(c.wallNow())
	if !stamped {
		return time.Time{}
	}
	return sent.Add(refreshDelay(links))
}
