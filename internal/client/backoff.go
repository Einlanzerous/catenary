package client

// The dial backoff's pure pieces, and CANT-35 ruling 3 → B's two rules
// (CANT-170). web/src/transport/backoff.ts is the same four functions in
// TypeScript, and testdata/decisions.json's backoff_* kinds hold both to one
// answer.
//
// FLOOR AND CEILING ARE UNCHANGED: 250 ms, doubling, 5 s. The ceiling is coupled
// to CANT-127 (refreshBackoffBase is "also the dial backoff's ceiling") and to
// CANT-129 (the refused wait polls at it), so it is not this file's to change.
//
//  1. THE STABILITY RULE. The ramp resets to its floor only after a session
//     stayed `ready` for at least one heartbeat_interval_sec — the interval its
//     own `ready` announced. Before this, ANY session that reached `ready` reset
//     it, so a path that answers `ready` and drops at once was redialed about
//     four times a second, each dial with a /sync beside it.
//  2. CEILING-SAFE JITTER. Each wait is uniform in [0.8d, d], drawn through
//     Config.Jitter, so a cohort severed together does not redial in step.
//     Never above d, so the ceiling stays a ceiling. The maximum wait an
//     error{internal} close asks for is jittered too, as transport.ts does: it
//     is drawn in [0.8 × BackoffMax, BackoffMax].

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math"
	"time"
)

// jitterFloor is the low end of the jitter band, as a fraction of the nominal
// wait. A VARIABLE, NOT A CONSTANT, and that is load-bearing: Go evaluates a
// constant expression exactly, so `1 - jitterFloor` would be 0.2 rounded once,
// while TypeScript's `1 - JITTER_FLOOR` is float64 arithmetic and gives
// 0.19999999999999996. The two must be the same bits or the vectors disagree.
var jitterFloor = 0.8

// jitteredWait is a wait drawn in [0.8d, d]. unit is a draw in [0, 1], clamped:
// 0 is the minimum draw (the worst case for dial counts), 1 the maximum. Rounded
// to the nearest nanosecond, which cannot exceed d because the factor never
// exceeds 1.
func jitteredWait(nominal time.Duration, unit float64) time.Duration {
	u := min(1, max(0, unit))
	return time.Duration(math.Round(float64(nominal) * (jitterFloor + (1-jitterFloor)*u)))
}

// unitFromBytes turns four bytes of Config.Jitter into a draw in [0, 1]: big
// endian, over 2^32 − 1, so all-zero bytes are exactly 0 and all-0xff exactly 1.
func unitFromBytes(b [4]byte) float64 {
	return float64(binary.BigEndian.Uint32(b[:])) / math.MaxUint32
}

// resetsRamp is rule 1: a session that reached `ready` resets the ramp only if
// it stayed ready for at least one heartbeat interval. readied false is a
// session that never got that far, whatever readiedFor says.
func resetsRamp(readied bool, readiedFor, interval time.Duration) bool {
	return readied && readiedFor >= interval
}

// advance is the next nominal wait after one that did not reset: doubled, to
// the ceiling.
func advance(nominal, ceiling time.Duration) time.Duration {
	return min(nominal*2, ceiling)
}

// draw is one jitter draw from Config.Jitter, or crypto/rand. A failed read is
// the maximum draw: the nominal wait, never above the ceiling.
func (c *Client) draw() float64 {
	src := c.cfg.Jitter
	if src == nil {
		src = rand.Reader
	}
	var b [4]byte
	if _, err := io.ReadFull(src, b[:]); err != nil {
		return 1
	}
	return unitFromBytes(b)
}

// sleepFor is Client.sleep's default: d, or until ctx ends, reporting which.
func sleepFor(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
