/* The credential seam — mirrors the credential half of internal/client/journal.go
 * (`Credential`) and the hooks refresh.go, hold.go and refused.go hang on
 * `Client.Run`, `catchUpLoop` and `fetchWith`.
 *
 * THIS ROW PRESENTS THE CREDENTIAL AS HELD, as Go's `Config.Refresh: false`
 * does: `heldCredential` never refreshes, never refuses, never withholds. CANT-31
 * §1–§6, CANT-127's hold and CANT-129's refused wait are CANT-152's, and they
 * arrive as a second implementation of `CredentialSeam` — the transport calls
 * every hook below at the point Go calls its counterpart, so that ticket fills
 * the hooks in and changes no line of the session loop.
 */

import type { Uuid } from '@/wire/generated'
import type { Terminal } from './terminal'

/** What the transport presents. Re-read at every use and never cached, so a
 *  rotation by this transport or another context is what the next dial and the
 *  next `/sync` carry (Go's `Client.credential`). */
export interface Credential {
  /** Who this device speaks as — `EnrollResponse.user_id`. The transport needs
   *  it for exactly one rule: a `receipt` naming it is this person's own. */
  userId: Uuid
  /** `ClientHello.device_id`. A rotation never changes it. */
  deviceId: Uuid
  /** Rides the upgrade as `catenary.token.<token>` and `/sync` as a bearer.
   *  NEVER LOGGED, and never in a status field. */
  accessToken: string
}

/** CANT-127's `RefreshHold`: why no automatic refresh is being made. */
export type RefreshHold = 'none' | 'unreachable' | 'backoff'

/** What the credential layer reports into `TransportStatus`. Stats use Go's
 *  `Stats` names, camelCased. */
export interface CredentialStatus {
  refreshHold: RefreshHold
  /** `last_sent_at + min(15 min, 5 s × 2^(n−1))`, wall-clock ms; null for an
   *  empty chain or an absent stamp. */
  nextRefreshAt: number | null
  refreshes: number
  refreshesSkipped: number
  refreshErrors: number
  refreshWalkBacks: number
  chainLength: number
  refreshesHeldUnreachable: number
  refreshesHeldBackoff: number
}

/** What the transport lends the credential layer. */
export interface CredentialHost {
  /** Enter a terminal state (§5's credential terminal). The first wins. */
  terminal(t: Terminal): void
  /** Something the status reports changed. */
  notify(): void
}

/**
 * The seam CANT-152 fills in. Every method is called where Go calls its
 * counterpart; none may throw — a failure is the credential layer's to count
 * and log, and the transport carries on as Go's `Run` does after a failed
 * proactive refresh.
 */
export interface CredentialSeam {
  attach(host: CredentialHost): void
  /** The pair to present now. */
  current(): Promise<Credential>
  /** `waitWhileRefused(withheldDial)`'s predicate: true withholds this dial.
   *  Polled at the dial cadence (the backoff ceiling), never a timer. */
  withholdDial(): Promise<boolean>
  /** `waitWhileRefused(withheldSync)`'s predicate: true withholds this `/sync`. */
  withholdSync(): Promise<boolean>
  /** CANT-129: Catenary's own 401 on `/sync` has refused the token held now.
   *  Read synchronously for the status and for every browser-only trigger the
   *  refused wait suppresses (wake signals, the policy catch-up, `catchUp()`,
   *  `retryNow()`). */
  refused(): boolean
  /** The proactive refresh before a dial (`refreshDueWhenAllowed`): bounded by
   *  CANT-127's suppressors. */
  beforeDial(): Promise<void>
  /** The explicit §1 check (`RefreshIfDue`): never held. Also run on every wake
   *  signal before the dial it may make (CANT-35 criterion 27). */
  refreshIfDue(): Promise<void>
  /** Catenary's own 401 on `/sync` for `presented` (`markRefused` plus
   *  `refreshAfter401`). Resolves true when the request should be retried ONCE
   *  with the pair `current()` now returns. */
  onSyncUnauthorized(presented: Credential): Promise<boolean>
  /** Catenary answered this context — a decoded frame, a decoded `/sync` page,
   *  or its own 401 (`markAnswered`, CANT-127's gate). */
  answered(): void
  status(): CredentialStatus
}

/**
 * The credential presented as held: no refresh, no refusal, no hold. Go's
 * `Refresh: false`, which is also what the rigs run (CANT-31 criterion 39).
 */
export function heldCredential(credential: Credential | (() => Credential | Promise<Credential>)): CredentialSeam {
  const read = typeof credential === 'function' ? credential : () => credential
  return {
    attach() {},
    current: async () => read(),
    withholdDial: async () => false,
    withholdSync: async () => false,
    refused: () => false,
    beforeDial: async () => {},
    refreshIfDue: async () => {},
    onSyncUnauthorized: async () => false,
    answered() {},
    status: () => ({
      refreshHold: 'none',
      nextRefreshAt: null,
      refreshes: 0,
      refreshesSkipped: 0,
      refreshErrors: 0,
      refreshWalkBacks: 0,
      chainLength: 0,
      refreshesHeldUnreachable: 0,
      refreshesHeldBackoff: 0,
    }),
  }
}

/** Catenary's own 401: the status AND the body `{"code":"unauthorized"}`. A
 *  401 from a hop in front carries neither body nor meaning (CANT-31 §5). */
export function isCatenaryUnauthorized(status: number, body: string): boolean {
  if (status !== 401) return false
  try {
    const v = JSON.parse(body) as unknown
    return typeof v === 'object' && v !== null && (v as { code?: unknown }).code === 'unauthorized'
  } catch {
    return false
  }
}
