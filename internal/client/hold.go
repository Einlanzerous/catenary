package client

// CANT-127: what bounds the chain.
//
// This is CANT-31's record §3, the bound, in the reference client. The rules are
// the record's and are implemented twice more, in TypeScript (CANT-35) and in
// Dart (CANT-42); where this file and the record disagree, fix the record first
// and then all three.
//
// THE CHAIN ITSELF CANNOT BE MADE SHORTER. Every link is a `(token, proposal)`
// request that MAY HAVE BEEN DELIVERED: if it was, the token is spent, and
// sending those bytes again outside ReuseGraceWindow is a replay that revokes
// the device. So no link can be re-sent and none can be dropped without an
// answer, and the chain's length simply IS the number of possibly-delivered
// refresh requests since the last one that settled something. The only lever is
// HOW MANY SUCH REQUESTS THE CLIENT SENDS ON ITS OWN INITIATIVE, and that is
// what the two suppressors below bound, off one persisted stamp
// (Journal.lastSent):
//
//	the gate     while the chain is non-empty, do not refresh automatically
//	             unless Catenary itself has answered THIS context later than
//	             that stamp. A night in airplane mode then costs one link
//	             instead of ~5,700.
//	the backoff  and no sooner than `last_sent_at + min(15 min, 5 s × 2^(n−1))`,
//	             n being the chain's length — for the path where /sync works and
//	             /refresh does not, which the gate cannot see. A day of that
//	             costs about a hundred links instead of ~17,000.
//
// THEY ONLY EVER REMOVE ATTEMPTS. Which token is presented, with which proposal,
// in what order, and what each answer means are exchange's and are untouched;
// opening the gate sends nothing by itself. A suppressor wrong in the unsafe
// direction sends a request that would have gone out anyway, and one wrong in
// the safe direction delays a refresh. Neither can present a spent token.
//
// THERE IS NO ABSOLUTE BOUND, and record §3 says so: that would need the server
// to answer "which of these is live?" without spending anything, which is a
// second liveness oracle on the path that issues credentials.
//
// AND THEY SIT AT THE TWO AUTOMATIC CALL SITES ONLY — Run's proactive call
// before each dial, and fetch's reactive call after Catenary's own 401 on /sync.
// RefreshIfDue is documented public API a host may call on its own clock and
// stays an unconditional "do it now"; the thing being bounded is the attempts
// the client makes without anybody deciding to.

import (
	"context"
	"errors"
	"time"
)

const (
	// refreshBackoffBase is the 5 s in `min(cap, 5 s × 2^(n−1))`: one link's
	// wait, which is also the dial backoff's ceiling — so a chain of one costs
	// no more than the dial rate already did.
	refreshBackoffBase = 5 * time.Second
	// refreshBackoffCap is the 15 minutes a person picked (CANT-127 ruling 2).
	// The first eight links take about 21 minutes and every link after that
	// takes 15, so a full day of black-holed /refresh is ~100 links.
	refreshBackoffCap = 15 * time.Minute
	// chainWarnLength is the length at which a chain is worth a WARN. The
	// backoff cannot get there in under ~14 hours of the worst case, so seeing
	// it means something is wrong with the path, not with the device.
	chainWarnLength = 64
)

// errRefreshHeld is an automatic refresh a suppressor did not make. IT IS NOT A
// FAILURE and it is not a skip: fetch answers it by handing back the 401 it
// already had instead of re-sending /sync with the same expired pair, and Run
// and catchUpLoop treat it as expected rather than logging it per attempt —
// which on a dead network would be ~720 lines an hour.
var errRefreshHeld = errors.New("client: refresh: held — Catenary has not answered this context since the last send")

// RefreshHold names why the client is making no automatic refresh (CANT-127).
// Status reports it so that a person can tell a held refresh from an exhausted
// dial backoff and from either terminal, which record §6 requires to be
// distinguishable.
type RefreshHold int

const (
	// RefreshNotHeld is a credential nothing is holding: either its chain is
	// empty — it is settled — or Catenary has answered since the last send and
	// the delay has elapsed.
	RefreshNotHeld RefreshHold = iota
	// RefreshHeldUnreachable is the gate: the chain is non-empty and no
	// Catenary-authored response has reached THIS context later than
	// `last_sent_at`.
	RefreshHeldUnreachable
	// RefreshHeldBackoff is the delay: Catenary has answered, and
	// `last_sent_at + min(cap, 5 s × 2^(n−1))` has not arrived yet.
	RefreshHeldBackoff
)

func (h RefreshHold) String() string {
	switch h {
	case RefreshHeldUnreachable:
		return "refresh held — server not reached"
	case RefreshHeldBackoff:
		return "refresh backing off"
	default:
		return "none"
	}
}

// refreshNeed is what a refresh's stillNeeded closure answers. A bool cannot
// say HELD — which is neither "already done by somebody else" nor a failure —
// so the closure the single-flight section asks says which of the three it is.
type refreshNeed int

const (
	refreshNotNeeded refreshNeed = iota // another context already rotated the pair
	refreshNeeded                       // go ahead
	refreshHeld                         // a suppressor says not now (CANT-127)
)

// refreshDelay is `min(cap, 5 s × 2^(n−1))` for a chain of n links, counted
// from the send. Doubled rather than shifted, so a long chain cannot overflow
// the duration on its way to a cap it passed at eight links.
func refreshDelay(links int) time.Duration {
	if links <= 0 {
		return 0
	}
	d := refreshBackoffBase
	for range links - 1 {
		if d >= refreshBackoffCap {
			break
		}
		d *= 2
	}
	return min(d, refreshBackoffCap)
}

// stamp reads `last_sent_at` as the suppressors must read it: ONE RULE FOR
// ABSENT. A stamp that is missing — a journal written before CANT-127 — and one
// in the future — a device whose clock was set backwards — are both absent, and
// absent means the same thing in both places it is read: for the backoff the
// wait has elapsed, and for the gate any Catenary-authored response opens it.
// The worst a wrong clock can do in either direction is allow one attempt early.
func (c *Client) stamp(now time.Time) (sent time.Time, ok bool) {
	sent, ok = c.j.LastSent()
	if ok && sent.After(now) {
		return time.Time{}, false
	}
	return sent, ok
}

// answeredSince is record §3's gate, as a comparison rather than a signal.
//
// DERIVED, NOT TRACKED, and that is what makes it work across processes. A
// second tab, or an app beside its push worker, shares storage and not memory,
// so a context whose gate is open has no way to be told another context's
// attempt failed. What crosses is what is persisted: another context's attempt
// moves the stamp, and every context's gate closes with it, by this comparison.
//
// LATER THAN IS STRICT, and equal is closed. The safe direction is one held
// attempt, never an extra one.
//
// A CONTEXT WITH NO RESPONSE YET IS CLOSED EITHER WAY — relaunch, a fresh push
// worker and a second tab are one property, not three — and an ABSENT stamp is
// opened by any Catenary-authored response, because there is no send to be later
// than.
func (c *Client) answeredSince(sent time.Time, stamped bool) bool {
	c.mu.Lock()
	at := c.answeredAt
	c.mu.Unlock()
	switch {
	case at.IsZero():
		return false
	case !stamped:
		return true
	}
	return at.After(sent)
}

// markAnswered records that CATENARY ITSELF answered this context, now. It is
// called from exactly three places, which are record §3's three: a /sync 200
// whose body passed the generated decoder, Catenary's own `{"code":
// "unauthorized"}` 401 on /sync, and a server frame the generated decoder
// accepted. A proxy's 401, a captive portal's 200, a 5xx from a hop in front and
// a frame the decoder refuses are none of them, and none of them opens the gate.
//
// A /refresh RESPONSE IS DELIBERATELY NOT ONE OF THE THREE. It is proof of
// reachability, but letting a refresh's own answer open the gate would let one
// attempt authorize the next, which is the loop being bounded; the backoff is
// what covers the path where Catenary answers and /refresh does not.
func (c *Client) markAnswered() {
	now := c.wallNow()
	c.mu.Lock()
	if now.After(c.answeredAt) {
		c.answeredAt = now
	}
	c.mu.Unlock()
	// THE GATE MAY HAVE JUST OPENED, and record §3's observability asks for one
	// line per transition — so it is evaluated here, where the response is,
	// rather than at the next attempt. NOTHING IS SENT: opening the gate is not
	// a trigger.
	_ = c.refreshHold()
}

// refreshHold is the two suppressors, in order, and the only place either is
// decided. It reads persisted state — the chain and its stamp — plus this
// context's own latest response time, and it sends nothing.
func (c *Client) refreshHold() RefreshHold { return c.holdNow(true) }

// refreshHoldQuiet is the same decision for an OBSERVER, and Status is the only
// one. Asking a client how it is must not be able to produce a log line: "one
// INFO when the gate closes and one when it opens" would otherwise become one
// per poll that fell between a send and its answer, and how often a caller polls
// is nobody's business but the caller's.
func (c *Client) refreshHoldQuiet() RefreshHold { return c.holdNow(false) }

func (c *Client) holdNow(note bool) RefreshHold {
	links := c.j.chainLen()
	if links == 0 {
		// SETTLED: there is no possibly-delivered request outstanding, so there
		// is nothing to bound. This is every client before its first unknown
		// outcome, and every client after its next answer.
		if note {
			c.forgetGate()
		}
		return RefreshNotHeld
	}
	if note {
		c.warnLongChain(links)
	}
	if c.cfg.Faults.Unbounded {
		return RefreshNotHeld
	}
	now := c.wallNow()
	sent, stamped := c.stamp(now)
	open := c.answeredSince(sent, stamped)
	if note {
		c.noteGate(open, links)
	}
	if !open {
		return RefreshHeldUnreachable
	}
	if stamped && now.Before(sent.Add(refreshDelay(links))) {
		return RefreshHeldBackoff
	}
	return RefreshNotHeld
}

// noteGate logs one INFO when this context's gate closes and one when it opens,
// carrying the chain's length — NOT one line per held attempt, which is the
// whole reason errRefreshHeld is a distinct error.
func (c *Client) noteGate(open bool, links int) {
	c.mu.Lock()
	known, was := c.gateKnown, c.gateOpen
	c.gateKnown, c.gateOpen = true, open
	c.mu.Unlock()
	if known && was == open {
		return
	}
	if open {
		c.log.Info("refresh gate open: Catenary has answered this context since the last send", "chain_length", links)
		return
	}
	c.log.Info("refresh gate closed: no answer from Catenary since the last send", "chain_length", links)
}

// forgetGate is what an answered refresh does to the gate's log state: the next
// unsettled stretch is a new one and says so once.
func (c *Client) forgetGate() {
	c.mu.Lock()
	c.gateKnown, c.gateOpen = false, false
	c.mu.Unlock()
}

// warnLongChain is one WARN the first time a chain passes chainWarnLength, and
// not one per attempt after it.
func (c *Client) warnLongChain(links int) {
	if links <= chainWarnLength {
		return
	}
	c.mu.Lock()
	warned := c.warnedLongChain
	c.warnedLongChain = true
	c.mu.Unlock()
	if !warned {
		c.log.Warn("the refresh chain is long: the path is failing, not the device",
			"chain_length", links, "above", chainWarnLength)
	}
}

// refreshWhenAllowed is the ONE gated entry point, and the two automatic call
// sites — Run's pre-dial check and fetch's answer to a Catenary 401 — are its
// only callers. It returns errRefreshHeld when a suppressor stood the attempt
// down, and otherwise exactly what refresh returns.
//
// THE HOLD IS CHECKED TWICE, and both checks are load-bearing. Once HERE,
// before the Journal's single-flight lock, as the cheap skip: a held attempt
// must not queue on a lock it has no business taking. And once UNDER the lock,
// as the authority, in the same closure that re-reads the persisted credential
// there — because another context may have attempted while this one waited,
// which lengthens the chain and moves the stamp, and the second check reads both
// as they now are.
func (c *Client) refreshWhenAllowed(ctx context.Context, stillNeeded func(held Credential) refreshNeed) error {
	if h := c.refreshHold(); h != RefreshNotHeld {
		c.countHeld(h)
		return errRefreshHeld
	}
	return c.refresh(ctx, func(held Credential) refreshNeed {
		if h := c.refreshHold(); h != RefreshNotHeld {
			c.countHeld(h)
			return refreshHeld
		}
		return stillNeeded(held)
	})
}

// refreshDueWhenAllowed is Run's proactive call: RefreshIfDue's own decision,
// behind the suppressors. It is not RefreshIfDue, because that one is a host's
// explicit "do it now" and is never held.
func (c *Client) refreshDueWhenAllowed(ctx context.Context) error {
	if !c.cfg.Refresh || !refreshDue(c.credential(), c.wallNow()) {
		return nil
	}
	return c.refreshWhenAllowed(ctx, func(held Credential) refreshNeed {
		if refreshDue(held, c.wallNow()) {
			return refreshNeeded
		}
		return refreshNotNeeded
	})
}

// countHeld counts a held attempt under the suppressor that held it, and NEVER
// in RefreshErrors: a refresh nobody made did not fail.
func (c *Client) countHeld(h RefreshHold) {
	c.mu.Lock()
	switch h {
	case RefreshHeldUnreachable:
		c.stats.RefreshesHeldUnreachable++
	case RefreshHeldBackoff:
		c.stats.RefreshesHeldBackoff++
	}
	c.mu.Unlock()
}
