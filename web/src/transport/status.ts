/* The transport's status, and the adapter from it to what the banner reads.
 * Mirrors `Stats` and `Status` in internal/client/client.go; `connectionInfo`
 * has no Go counterpart, because the Go client renders nothing.
 *
 * STATS USE GO'S FIELD NAMES, camelCased, so a soak report reads the same for
 * both cohorts and soakrig decodes one into `client.Status` field by field. The
 * one rename is `LastRTT`, a Go duration, which is `lastRttMs` here. The one
 * addition is `introductionDiscards` (CANT-103 rule 1), which has no Go
 * counterpart until the Go client gets those rules.
 *
 * NO TOKEN APPEARS IN ANY FIELD, and a test scans for one.
 */

import type { ConnectionInfo } from '@/client-types'
import type { RefreshHold } from './credential'
import type { Terminal } from './terminal'

export interface Stats {
  /** Connection attempts. */
  dials: number
  /** Dials whose socket never opened. Not also counted in `closeStatuses`. */
  dialErrors: number
  /** `ready` frames received — sessions established. */
  readys: number
  /** `resync_required` frames received. */
  resyncs: number
  /** `ready.log_seq` below the cursor (obligation 4). */
  discards: number
  /** `message` frames applied. */
  liveFrames: number
  /** `/sync` pages applied or dropped. */
  pages: number
  syncErrors: number
  /** `/sync` requests issued while a dial had not yet received `ready`. */
  syncsBeforeReady: number
  pingsSent: number
  pongsReceived: number
  /** Sockets this client severed for unanswered pings. */
  heartbeatSevers: number
  /* The credential layer's (CANT-152); zero while the credential is held. */
  refreshes: number
  refreshesSkipped: number
  refreshErrors: number
  refreshWalkBacks: number
  chainLength: number
  refreshesHeldUnreachable: number
  refreshesHeldBackoff: number
  /** Requests CANT-129's refused wait did not make — one per poll — and the
   *  browser-only triggers it suppressed. `dialsWithheld` ALSO counts wake
   *  signals the early dial's once-per-interval limit suppressed, because
   *  CANT-35's plan (*Observability*) puts both there; Go has no wake signals,
   *  so a Go cohort's count and a TS cohort's that saw none mean the same.
   *  Counted, never logged per attempt. */
  dialsWithheld: number
  syncsWithheld: number
  /** Message events that were not a frame: a `WireFormatError`, text
   *  `JSON.parse` refuses, or a binary message. */
  undecodable: number
  lastRttMs: number
  lastClose: string
  /** How each session this client HELD ended, keyed by the close code the peer
   *  sent, or -1 for none (a browser's 1005 and 1006 included). A dial that
   *  never opened is `dialErrors`, not here. */
  closeStatuses: Record<number, number>
  /** CANT-103 rule 1: `message` frames discarded for naming a conversation or
   *  an author the journal does not hold. */
  introductionDiscards: number
}

export interface TransportStatus {
  terminal: Terminal
  refreshHold: RefreshHold
  tokenRefused: boolean
  /** Wall-clock ms, or null. */
  nextRefreshAt: number | null
  /** A socket is open and the hello is on it. */
  connected: boolean
  /** And it has received `ready`. */
  ready: boolean
  sessionId: string | null
  heartbeatIntervalSec: number | null
  missedPongLimit: number | null
  /** No trigger is outstanding: the last catch-up ended on a page requested
   *  after the most recent trigger. */
  caughtUp: boolean
  cursor: number | null
  /** Dials since the ramp last reset. The banner's "attempt N". */
  attempt: number
  /** When the pending dial goes out, wall-clock ms; null when none is pending. */
  nextDialAt: number | null
  stats: Stats
  /** Go's `Status.Messages` and `Status.Wipes`, which the soak's predicates
   *  read: how many messages the journal holds, and how many wipes it has been
   *  through in this transport's life. */
  messages: number
  wipes: number
}

export function emptyStats(): Stats {
  return {
    dials: 0, dialErrors: 0, readys: 0, resyncs: 0, discards: 0, liveFrames: 0, pages: 0,
    syncErrors: 0, syncsBeforeReady: 0, pingsSent: 0, pongsReceived: 0, heartbeatSevers: 0,
    refreshes: 0, refreshesSkipped: 0, refreshErrors: 0, refreshWalkBacks: 0, chainLength: 0,
    refreshesHeldUnreachable: 0, refreshesHeldBackoff: 0, dialsWithheld: 0, syncsWithheld: 0,
    undecodable: 0, lastRttMs: 0, lastClose: '', closeStatuses: {}, introductionDiscards: 0,
  }
}

/**
 * What `ConnectionBanner` reads, from what the transport knows. Pure: `now`
 * and `online` (`navigator.onLine`) are passed in.
 *
 * TERMINAL FIRST, because neither terminal ends on a network change (CANT-31
 * §6): an offline device that is also terminal is told it is terminal. Then
 * `offline` when the browser says so; then `live` or `resyncing` for a ready
 * session; and `reconnecting` for everything between dials.
 */
export function connectionInfo(status: TransportStatus, env: { now: number; online?: boolean }): ConnectionInfo {
  const extra = {
    refreshHold: status.refreshHold,
    tokenRefused: status.tokenRefused,
  }
  if (status.terminal.kind !== 'none') {
    return { state: 'terminal', terminal: { kind: status.terminal.kind, reason: status.terminal.reason }, ...extra }
  }
  if (env.online === false) return { state: 'offline', ...extra }
  if (status.ready) return { state: status.caughtUp ? 'live' : 'resyncing', ...extra }
  return {
    state: 'reconnecting',
    attempt: Math.max(1, status.attempt),
    retryInSec: status.nextDialAt === null ? 0 : Math.max(0, Math.ceil((status.nextDialAt - env.now) / 1000)),
    ...extra,
  }
}
