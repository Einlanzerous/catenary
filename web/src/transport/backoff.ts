/* The dial backoff — mirrors the ramp inline in `Client.Run`
 * (internal/client/client.go), with CANT-35 ruling 3's amendment. Pure.
 *
 * FLOOR AND CEILING ARE GO'S: 250 ms, doubling, 5 s. The ceiling is coupled to
 * CANT-127 (`refreshBackoffBase = 5 s` "is also the dial backoff's ceiling")
 * and to CANT-129 (the refused wait's cadence IS the dial ceiling), so it is
 * not this file's to change.
 *
 * RULING 3 → B adds two rules Go does not have yet (the Go port is a follow-up
 * ticket, so the three clients stay equal):
 *
 *   1. THE STABILITY RULE. The ramp resets to its floor only after a session
 *      stayed `ready` for at least one `heartbeat_interval_sec`. Go resets after
 *      ANY session that reached `ready`, so a path that answers `ready` and drops
 *      at once is redialed about four times a second, each dial with a `/sync`
 *      beside it.
 *   2. CEILING-SAFE JITTER. Each wait is uniform in `[0.8d, d]`, drawn through
 *      the `random` seam, so a cohort severed together does not redial in step.
 *      Never above `d`, so the ceiling stays a ceiling.
 */

export const BACKOFF_MIN_MS = 250
export const BACKOFF_MAX_MS = 5_000

/** The low end of the jitter band, as a fraction of the nominal wait. */
export const JITTER_FLOOR = 0.8

/** A wait drawn in `[0.8d, d]`. `unit` is a draw in `[0, 1]`: 0 is the minimum
 *  draw (the worst case for dial counts), 1 the maximum. */
export function jitteredWait(nominalMs: number, unit: number): number {
  const u = Math.min(1, Math.max(0, unit))
  return nominalMs * (JITTER_FLOOR + (1 - JITTER_FLOOR) * u)
}

/** Turns the `random` seam's bytes into a draw in `[0, 1]`: four bytes, big
 *  endian, over 2^32 − 1, so all-zero bytes are exactly 0 and all-0xff exactly 1. */
export function unitFromBytes(bytes: Uint8Array): number {
  const n = ((bytes[0] << 24) >>> 0) + (bytes[1] << 16) + (bytes[2] << 8) + bytes[3]
  return n / 0xffffffff
}

/** Rule 1: a session that reached `ready` resets the ramp only if it stayed
 *  ready for at least one heartbeat interval. */
export function resetsRamp(readiedForMs: number | null, heartbeatIntervalSec: number | null): boolean {
  if (readiedForMs === null || heartbeatIntervalSec === null) return false
  return readiedForMs >= heartbeatIntervalSec * 1000
}

/** The next nominal wait after one that did not reset: doubled, to the ceiling. */
export function advance(nominalMs: number, maxMs: number): number {
  return Math.min(nominalMs * 2, maxMs)
}
