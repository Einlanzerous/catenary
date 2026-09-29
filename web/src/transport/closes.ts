/* CANT-31 §4, the close table — mirrors `classifyClose` in
 * internal/client/terminal.go. Pure: the part CANT-42 must reproduce exactly,
 * and the part CANT-156's shared vectors will point a runner at.
 *
 * The table is the record's (docs/decisions/cant-31-refresh-and-terminal-reconnect.md
 * §4), and only that. Where this file and the record disagree, fix the record
 * first.
 */

import type { ServerError } from '@/wire/generated'

export type CloseVerdict = 'reconnect' | 'reconnect_at_maximum' | 'terminal_protocol'

/** Close codes with a reading in the table. `4001` is the hub's
 *  `StatusRevoked`, spelled as a number because a client does not import the
 *  server; the record is what pins it. */
export const CLOSE_REVOKED = 4001
export const CLOSE_POLICY_VIOLATION = 1008

/**
 * The key a close is counted under in `stats.closeStatuses`, which is Go's key:
 * the code the peer sent, or `-1` when no close frame arrived at all. A browser
 * reports an abnormal closure as `1006` and a close frame with no status as
 * `1005`; both are "no status", and both are counted under `-1` so a mixed
 * soak report is one map (CANT-35's plan, *Rules*).
 */
export function closeStatusKey(code: number | null | undefined): number {
  if (code === null || code === undefined || code === 1005 || code === 1006) return -1
  return code
}

/**
 * Record §4. `code` is the close status the peer sent, `null` for none.
 * `preceding` is the LAST frame before the close, and only if it was an
 * `error` carrying no `client_id` — null otherwise, and null after ANY later
 * message event, decodable or not.
 *
 * A BARE 1008 IS A CLIENT BUG: replaying the same bytes walks into the same
 * close. `error{internal}` is never terminal, whatever its `retryable` says.
 * EVERYTHING NOT LISTED RECONNECTS, including a code a later server adds: a
 * client that reconnects when it should have stopped wastes a dial, and one
 * that stops when it should have reconnected has logged its person out.
 */
export function classifyClose(
  code: number | null,
  preceding: ServerError | null,
): { verdict: CloseVerdict; reason: string } {
  const status = closeStatusKey(code)
  if (status === CLOSE_REVOKED) {
    return { verdict: 'terminal_protocol', reason: 'close 4001: the credential behind the session was revoked' }
  }
  if (status === CLOSE_POLICY_VIOLATION) {
    if (preceding === null) {
      return {
        verdict: 'terminal_protocol',
        reason: 'close 1008, bare: the server could not accept what this client wrote',
      }
    }
    if (preceding.code === 'unauthorized' || preceding.code === 'wire_version_unsupported') {
      return { verdict: 'terminal_protocol', reason: `close 1008 after error{${preceding.code}}` }
    }
    if (preceding.code === 'internal') {
      // `retryable` IS NOT CONSULTED. An unclassified server failure arrives as
      // `internal` with that flag biased toward true, and a permanent server
      // fault must not stop every client and keep them stopped after the fix.
      return { verdict: 'reconnect_at_maximum', reason: '' }
    }
  }
  return { verdict: 'reconnect', reason: '' }
}
