/* What bounds the chain — mirrors internal/client/hold.go (CANT-127).
 *
 * CANT-31's record §3, the bound (docs/decisions/cant-31-refresh-and-terminal-reconnect.md).
 * Where this file and the record disagree, fix the record first.
 *
 * THE DECISIONS ARE PURE: every function here is a function of durable state
 * (the chain's length and its `last_sent_at` stamp), this context's latest
 * Catenary answer, and ONE wall-clock reading, all passed in. No clock and no
 * I/O inside, so CANT-156's shared vectors can pin them against Go's, and a
 * cold start decides exactly as the context that wrote the credential would
 * have. Times are wall-clock milliseconds since the epoch; durations are
 * milliseconds and are NEVER ROUNDED — Go's are integer nanoseconds, and a
 * rounded third of a lifetime is a different threshold.
 *
 * The class in refresh.ts is what reads the store, holds the per-context
 * answer time, counts, and logs the gate's two transitions.
 */

import type { RefreshHold } from './credential'

/** The 5 s in `min(cap, 5 s × 2^(n−1))`: one link's wait, which is also the
 *  dial backoff's ceiling, so a chain of one costs no more than the dial rate. */
export const REFRESH_BACKOFF_BASE_MS = 5_000
/** The 15 minutes CANT-127 ruling 2 picked. */
export const REFRESH_BACKOFF_CAP_MS = 15 * 60_000
/** The length at which a chain is worth one WARN. */
export const CHAIN_WARN_LENGTH = 64

/**
 * `min(15 min, 5 s × 2^(n−1))` for a chain of `links`, counted from the send;
 * 0 for a settled credential. Doubled rather than exponentiated, as Go does, so
 * a long chain cannot overflow on its way to a cap it passed at eight links.
 */
export function refreshDelay(links: number): number {
  if (links <= 0) return 0
  let d = REFRESH_BACKOFF_BASE_MS
  for (let i = 1; i < links; i++) {
    if (d >= REFRESH_BACKOFF_CAP_MS) break
    d *= 2
  }
  return Math.min(d, REFRESH_BACKOFF_CAP_MS)
}

/**
 * `last_sent_at` as the suppressors must read it (Go's `Client.stamp`): ONE RULE
 * FOR ABSENT. A missing stamp — a chain written before CANT-127 — and one in the
 * future — a clock set backwards — are both null, and null reads the same way
 * everywhere: the delay has elapsed, and any Catenary answer opens the gate.
 */
export function readStamp(lastSentAt: number | null, now: number): number | null {
  return lastSentAt === null || lastSentAt > now ? null : lastSentAt
}

/**
 * Record §3's gate (Go's `Client.answeredSince`), as a comparison rather than a
 * signal. `stamp` is already read through `readStamp`. A context with no answer
 * yet is CLOSED either way; an absent stamp is opened by any answer; otherwise
 * LATER THAN IS STRICT, and an answer at the same instant as the send is closed.
 */
export function gateOpen(answeredAt: number | null, stamp: number | null): boolean {
  if (answeredAt === null) return false
  if (stamp === null) return true
  return answeredAt > stamp
}

/** What `refreshHoldAt` decides over. */
export interface HoldInput {
  /** The persisted chain's length. */
  links: number
  /** The persisted `last_sent_at`, raw (not yet read through `readStamp`). */
  lastSentAt: number | null
  /** When Catenary last answered THIS context; null for never. */
  answeredAt: number | null
  now: number
}

/**
 * The two suppressors, in order (Go's `Client.holdNow`, without the logging and
 * without `Faults.Unbounded`, which the caller applies): a settled credential
 * is never held; the gate holds an unsettled one Catenary has not answered
 * since the last send; the delay holds one whose `last_sent_at + delay` has not
 * arrived.
 */
export function refreshHoldAt(h: HoldInput): RefreshHold {
  if (h.links <= 0) return 'none'
  const sent = readStamp(h.lastSentAt, h.now)
  if (!gateOpen(h.answeredAt, sent)) return 'unreachable'
  if (sent !== null && h.now < sent + refreshDelay(h.links)) return 'backoff'
  return 'none'
}

/**
 * `Status.NextRefreshAt`: `last_sent_at + min(15 min, 5 s × 2^(n−1))`, the time
 * the backoff next allows an automatic refresh — and so also the time CANT-129's
 * one `/sync` goes out. Null for an empty chain or an absent stamp. It is not
 * "when the next refresh happens": while the GATE holds, that has no answer.
 */
export function nextRefreshAt(links: number, lastSentAt: number | null, now: number): number | null {
  if (links <= 0) return null
  const sent = readStamp(lastSentAt, now)
  return sent === null ? null : sent + refreshDelay(links)
}
