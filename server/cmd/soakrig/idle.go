package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/magos/catenary/internal/client"
)

// CANT-23's remaining clause is an hour of idle through the real Cloudflare
// tunnel and Traefik; CANT-109 is the code half of what that run needs. This
// file is the client — the choice of WHERE to point it, and whether to run
// it at all against anything deployed, belongs to whoever calls
// `soakrig idle`, never to this code.
//
// THE CLAIM IS "ONE SOCKET SURVIVES", NOT "THE CLIENT EVENTUALLY
// RECONNECTS". internal/client.Client reconnects with backoff by design —
// that is what makes it a correct client of the real protocol — but a
// reconnect during this measurement means the thing being measured (does an
// idle socket survive the tunnel) did not hold, whether or not the
// reconnect itself then succeeds. So the pass/fail rule below is stricter
// than "the client is healthy": it is "exactly one dial, ready once, and
// nothing in CloseStatuses" — Dials > 1 or any recorded close both fail it,
// even when the client would call that a normal recovery.

// closeHeartbeatTimeout is CANT-23's private-use close status for a
// server-side severance, named here rather than pulled in from
// coder/websocket only to label a number in a report string.
const closeHeartbeatTimeout = 4000

// startupTimeout bounds the FIRST connect only. A socket that never opens at
// all is a different failure than one that opened and was later severed —
// the second is this measurement's whole subject, so it gets the run's own
// -for bound instead of this short one.
const startupTimeout = 30 * time.Second

// settleWindow bounds how long runIdle waits, AFTER the first sign of
// trouble (a close recorded, or a second dial), for the client's own
// reconnect attempt to resolve one way or another — so the report can say
// WHY a reconnect failed (expired vs. some other refusal) rather than only
// that one was attempted. Bounded and short: this is extra diagnostic
// detail for the report, never a second chance at a pass, and a run that is
// going to fail this measurement does not need long to prove it locally.
var settleWindow = 3 * time.Second

// livenessInterval is how often runIdle narrates that it is still holding
// the socket, on stderr — unconditional, the same narration this tool has
// always printed here.
const livenessInterval = 5 * time.Minute

// idleReport is runIdle's own account of one run, independent of how it gets
// printed or turned into an exit code — kept separate so a test can assert
// on the report directly rather than scraping stderr.
type idleReport struct {
	Pass    bool
	Reason  string
	Elapsed time.Duration

	Dials, Readys            int
	PingsSent, PongsReceived int
	HeartbeatSevers          int
	CloseStatuses            map[int]int
	LastClose                string

	// TokenExpired is set only when a dial actually failed AND the wall
	// clock is past the access token's own recorded expiry — never merely
	// because a close happened to occur near that time.
	TokenExpired bool
}

// runIdle holds ONE socket against baseURL for runFor (bounded whenever it
// is positive; zero runs until ctx ends on its own, e.g. a signal) and
// reports whether that one socket survived the whole duration.
func runIdle(ctx context.Context, baseURL string, cred Credentials, headers http.Header, clientInfo string, runFor time.Duration, logger *slog.Logger) idleReport {
	if runFor > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runFor)
		defer cancel()
	}

	c, err := client.New(client.Config{
		BaseURL: baseURL, AccessToken: cred.AccessToken, DeviceID: cred.DeviceID,
		ClientInfo: clientInfo, Logger: logger, ExtraHeaders: headers,
	})
	if err != nil {
		return idleReport{Reason: fmt.Sprintf("build the client: %v", err)}
	}

	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	started := time.Now()
	defer func() { c.Close(); <-done }()

	readyCtx, cancelReady := context.WithTimeout(ctx, startupTimeout)
	readyErr := c.Await(readyCtx, func() bool { return c.Status().Ready })
	cancelReady()
	if readyErr != nil {
		s := c.Status()
		return idleReport{
			Elapsed: time.Since(started), Dials: s.Dials, LastClose: s.LastClose,
			Reason: fmt.Sprintf("never reached ready within %s (dial errors: %d, last: %s)", startupTimeout, s.DialErrors, s.LastClose),
		}
	}

	stopLiveness := make(chan struct{})
	livenessDone := make(chan struct{})
	go func() {
		defer close(livenessDone)
		t := time.NewTicker(livenessInterval)
		defer t.Stop()
		for {
			select {
			case <-stopLiveness:
				return
			case <-t.C:
				s := c.Status()
				fmt.Fprintf(os.Stderr, "[idle] alive %s · ready=%v · dials=%d · readys=%d · pings=%d · pongs=%d · severs=%d · last_rtt=%s\n",
					time.Since(started).Round(time.Second), s.Ready, s.Dials, s.Readys,
					s.PingsSent, s.PongsReceived, s.HeartbeatSevers, s.LastRTT.Round(time.Millisecond))
			}
		}
	}()

	// THE WATCH: wakes on every client state change (c.Await's own
	// mechanism) or on ctx ending, whichever comes first. Either
	// CloseStatuses gained an entry or Dials moved past 1 — the socket this
	// run is measuring did not survive — or ctx ended first, which (having
	// already reached ready with neither of those true) means it did.
	severedErr := c.Await(ctx, func() bool {
		s := c.Status()
		return len(s.CloseStatuses) > 0 || s.Dials > 1
	})

	if severedErr == nil {
		// Trouble was seen. Give the client's own reconnect loop a short,
		// bounded window to resolve — succeed (Readys grows again) or fail
		// outright (DialErrors grows) — so TokenExpired below has something
		// to reason about instead of a reconnect attempt still in flight.
		settleCtx, cancelSettle := context.WithTimeout(ctx, settleWindow)
		_ = c.Await(settleCtx, func() bool {
			s := c.Status()
			return s.Dials > 1 && (s.Readys > 1 || s.DialErrors > 0)
		})
		cancelSettle()
	}
	close(stopLiveness)
	<-livenessDone

	elapsed := time.Since(started)
	s := c.Status()
	rep := idleReport{
		Elapsed: elapsed, Dials: s.Dials, Readys: s.Readys,
		PingsSent: s.PingsSent, PongsReceived: s.PongsReceived,
		HeartbeatSevers: s.HeartbeatSevers, CloseStatuses: s.CloseStatuses, LastClose: s.LastClose,
	}

	if severedErr != nil {
		// ctx ended before the predicate ever went true: no close was ever
		// recorded and there was only ever the one dial.
		rep.Pass = len(s.CloseStatuses) == 0 && s.Dials == 1
		if !rep.Pass {
			rep.Reason = "ended in an inconsistent state — a close was recorded without the watch observing it; treat as a harness bug, not a pass"
		}
		return rep
	}

	// The predicate went true: the socket did not survive the whole
	// duration, whether or not a reconnect then succeeded.
	rep.Pass = false
	rep.TokenExpired = s.DialErrors > 0 && !cred.AccessExpiresAt.IsZero() && !time.Now().Before(cred.AccessExpiresAt)
	rep.Reason = closeReason(s, rep.TokenExpired)
	return rep
}

// closeReason names what ended the one socket this run was holding. A dial
// failure after the recorded access-token expiry is named as expiry rather
// than left as the raw "401 Unauthorized" text a bare dial failure would
// otherwise read as — CANT-109's own Done-when clause.
func closeReason(s client.Status, expired bool) string {
	var codes []string
	for code, n := range s.CloseStatuses {
		codes = append(codes, fmt.Sprintf("%s x%d", closeCodeName(code), n))
	}
	sort.Strings(codes)

	var b strings.Builder
	b.WriteString("the socket did not survive the run")
	if len(codes) > 0 {
		fmt.Fprintf(&b, ": closed with %s", strings.Join(codes, ", "))
	}
	if s.Dials > 1 {
		fmt.Fprintf(&b, "; %d dial(s) total — a reconnect was attempted, which alone fails this measurement even if it succeeded", s.Dials)
	}
	switch {
	case expired:
		b.WriteString("; the access token had expired by then — not a bare unauthorized")
	case s.Dials > 1 && s.LastClose != "":
		fmt.Fprintf(&b, " (last: %s)", s.LastClose)
	}
	return b.String()
}

func closeCodeName(code int) string {
	switch code {
	case -1:
		return "no-close-frame(-1)"
	case closeHeartbeatTimeout:
		return "heartbeat-timeout(4000)"
	case 1001:
		return "drain(1001)"
	case 1012:
		return "head-unreadable(1012)"
	default:
		return fmt.Sprintf("%d", code)
	}
}

// printIdleReport is what an operator reads on stderr, and what gets pasted
// into the ticket that ran it.
func printIdleReport(w io.Writer, rep idleReport) {
	fmt.Fprintf(w, "[idle] DONE elapsed=%s dials=%d readys=%d pings=%d pongs=%d severs=%d close_statuses=%v\n",
		rep.Elapsed.Round(time.Second), rep.Dials, rep.Readys, rep.PingsSent, rep.PongsReceived, rep.HeartbeatSevers, rep.CloseStatuses)
	if rep.Pass {
		fmt.Fprintf(w, "[idle] PASS — one socket survived the whole %s with no reconnect\n", rep.Elapsed.Round(time.Second))
		return
	}
	fmt.Fprintf(w, "[idle] FAIL — %s\n", rep.Reason)
}
