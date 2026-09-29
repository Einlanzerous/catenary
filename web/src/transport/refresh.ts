/* Refreshing before you need to, and also when you are refused — mirrors
 * internal/client/refresh.go (CANT-124, CANT-126), with the call sites
 * internal/client/hold.go (CANT-127) and internal/client/refused.go (CANT-129)
 * hang on `Client.Run`, `catchUpLoop` and `fetchWith`.
 *
 * CANT-31's record §1–§3 and §5 (docs/decisions/cant-31-refresh-and-terminal-reconnect.md),
 * implemented for the third time. Where this file and the record disagree, fix
 * the record first and then all three.
 *
 * `RefreshingCredential` is the `CredentialSeam` the transport core calls
 * (credential.ts): the transport decides WHEN (before a dial, on Catenary's own
 * 401 on `/sync`, on a wake signal, at the dial cadence while a token is
 * refused) and this decides WHETHER and WHAT. The decisions themselves are pure
 * and live beside it — `refreshThreshold` and `refreshDue` here, the gate and
 * the delay in hold.ts, the refused wait in refused.ts — so CANT-156's vectors
 * can pin them without a clock or a store.
 *
 * SINGLE-FLIGHT PER CREDENTIAL, NOT PER TAB: every refresh runs under the Web
 * Lock `catenary.credential.<device_id>` and re-reads the persisted credential
 * under it, so a tab that waited behind another's refresh finds the pair already
 * rotated and skips. Two refreshes of one pair is not a wasted round trip —
 * outside the grace window the second is a replay, and a replay revokes the
 * device (CANT-29).
 *
 * TOKENS NEVER REACH A LOG LINE OR A STATUS FIELD, and neither does a response
 * body, which is the one place a server could echo one.
 */

import { decodeRefreshResponse, encodeRefreshRequest } from '@/wire/generated'
import type { Credential, CredentialHost, CredentialSeam, CredentialStatus, RefreshHold } from './credential'
import { isCatenaryUnauthorized } from './credential'
import {
  credentialLockName,
  NoCredential,
  parseWireTime,
  type CredentialStore,
  type StoredCredential,
} from './credential-store'
import { type Faults, NO_FAULTS } from './faults'
import { CHAIN_WARN_LENGTH, gateOpen, nextRefreshAt, readStamp, refreshHoldAt } from './hold'
import { refusedHoldAt } from './refused'
import {
  type Clock,
  type Lock,
  type Logger,
  type RandomBytes,
  type Timers,
  browserLock,
  browserTimers,
  consoleLogger,
  cryptoRandom,
} from './seams'

/** The 60 s in `max(60 s, ⅓ of the served lifetime)`. */
export const REFRESH_FLOOR_MS = 60_000
/** Bounds one `POST /refresh`. */
const REFRESH_TIMEOUT_MS = 30_000
/** 32 CSPRNG bytes, which base64url without padding renders as the wire's
 *  43-character `Token` — exactly as the server mints its own. */
export const PROPOSAL_BYTES = 32
/** How often one refresh may answer `fresh_proposal` with another proposal. */
export const MAX_FRESH_PROPOSALS = 3
/** Caps the wait a `fresh_proposal` asks for; the lock is held across it. */
const MAX_RETRY_AFTER_MS = 5_000
/** Requests one refresh may send beyond one per link: the answers that send
 *  the walk back up (`present_proposal`) or round again (`fresh_proposal`). */
const WALK_SLACK = 8

// --- the pure decisions (record §1) -------------------------------------------

/** What the §1 decisions read of a credential. */
export type DueInput = Pick<StoredCredential, 'accessExpiresAt' | 'accessIssuedAt' | 'clockOffsetMs'>

/**
 * `max(60 s, ⅓ of the served lifetime)`, in ms and NOT ROUNDED — a 100 s
 * lifetime's third is 33,333.33… ms, as Go's nanosecond duration has it. The
 * served lifetime is measured on the SERVER's clock at both ends, so a wrong
 * device clock cannot stretch it. A pair whose issue time was never learned
 * gets the floor, not a guessed lifetime.
 */
export function refreshThreshold(c: Pick<StoredCredential, 'accessExpiresAt' | 'accessIssuedAt'>): number {
  if (c.accessIssuedAt === null) return REFRESH_FLOOR_MS
  return Math.max(REFRESH_FLOOR_MS, (c.accessExpiresAt - c.accessIssuedAt) / 3)
}

/**
 * Whether less than the threshold remains on the access token, as of
 * `deviceNow` (the device's WALL clock, never a monotonic one) corrected by the
 * offset persisted with the credential. A pure function of durable state and
 * one clock reading, so a cold start decides as the writer would have.
 */
export function refreshDue(c: DueInput, deviceNow: number): boolean {
  if (!Number.isFinite(c.accessExpiresAt)) return false // no expiry learned: the reactive path is the net
  const serverNow = deviceNow + c.clockOffsetMs
  return c.accessExpiresAt - serverNow < refreshThreshold(c)
}

/** A successor secret: 32 random bytes in the wire's Token shape, NEVER DERIVED
 *  FROM ANYTHING. Throws, sending and writing nothing, on a short draw. */
export function mintProposal(random: RandomBytes): string {
  const raw = random(PROPOSAL_BYTES)
  if (raw.length !== PROPOSAL_BYTES) throw new Error('refresh: the generator could not fill 32 bytes')
  let bin = ''
  for (const b of raw) bin += String.fromCharCode(b)
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

// --- the layer ------------------------------------------------------------------

/**
 * What one refresh came to. `held` is a suppressor's stand-down (CANT-127) and
 * is neither a skip nor a failure; `skipped` is another context having rotated
 * the pair already; `failed` is an attempt that settled nothing.
 */
export type RefreshOutcome = 'not_due' | 'refreshed' | 'skipped' | 'held' | 'failed' | 'terminal' | 'killed'

type Need = 'needed' | 'not_needed' | 'held'

export interface RefreshingCredentialConfig {
  /** The server's origin; `/refresh` is appended. */
  baseUrl: string
  store: CredentialStore
  /** Default: `browserLock()`, a Web Lock. */
  lock?: Lock
  fetch?: typeof globalThis.fetch
  now?: Clock
  timers?: Timers
  random?: RandomBytes
  logger?: Logger
  /** Go's `Config.Refresh`. False presents the pair as held: no refresh, and
   *  the refused-token rule is inert, since a client that cannot refresh can
   *  never cure a refused token. Default true. */
  refresh?: boolean
  /** Never set outside a test or a rig proving an assertion can fail. */
  faults?: Partial<Faults>
}

type Exchanged =
  | { kind: 'ok'; next: StoredCredential }
  | { kind: 'unauthorized' | 'moved' | 'failed' | 'killed'; why: string }

type Posted =
  | { kind: 'ok'; next: StoredCredential }
  | { kind: 'unauthorized'; why: string }
  | { kind: 'present_proposal'; why: string }
  | { kind: 'failed'; why: string }
  | { kind: 'fresh_proposal'; afterMs: number }

type Proposed = { kind: 'ok'; proposal: string } | { kind: 'moved' | 'failed' | 'killed'; why: string }

export function createRefreshingCredential(cfg: RefreshingCredentialConfig): RefreshingCredential {
  return new RefreshingCredential(cfg)
}

export class RefreshingCredential implements CredentialSeam {
  private readonly store: CredentialStore
  private readonly lock: Lock
  private readonly fetchFn: typeof globalThis.fetch
  private readonly now: Clock
  private readonly timers: Timers
  private readonly random: RandomBytes
  private readonly log: Logger
  private readonly faults: Faults
  private readonly enabled: boolean
  private readonly httpBase: string
  private host: CredentialHost | null = null

  /** The record as last read or written. AUTHORITATIVE DECISIONS RE-READ THE
   *  STORE; this is for the synchronous questions — `refused()` and `status()`
   *  — which may lag another context's write by one dial interval at most. */
  private rec: StoredCredential | null = null
  /** When CATENARY ITSELF last answered THIS context (Go's `answeredAt`). Per
   *  context and in memory on purpose: a relaunch, a second tab and a push
   *  worker are one property — a context that has heard nothing is closed. */
  private answeredAt: number | null = null
  /** The access token Catenary refused on `/sync` (CANT-129); null for none. */
  private refusedToken: string | null = null
  private gateKnown = false
  private gateWasOpen = false
  private warnedLongChain = false
  /** `kill -9`: nothing this context had in flight reaches the store after it. */
  private killed = false

  private readonly counts = {
    refreshes: 0,
    refreshesSkipped: 0,
    refreshErrors: 0,
    refreshWalkBacks: 0,
    refreshesHeldUnreachable: 0,
    refreshesHeldBackoff: 0,
  }

  constructor(cfg: RefreshingCredentialConfig) {
    this.store = cfg.store
    this.lock = cfg.lock ?? browserLock()
    this.fetchFn = cfg.fetch ?? ((input, init) => globalThis.fetch(input, init))
    this.now = cfg.now ?? (() => Date.now())
    this.timers = cfg.timers ?? browserTimers
    this.random = cfg.random ?? cryptoRandom
    this.log = cfg.logger ?? consoleLogger
    this.faults = { ...NO_FAULTS, ...cfg.faults }
    this.enabled = cfg.refresh ?? true
    this.httpBase = cfg.baseUrl.replace(/\/+$/, '')
  }

  // --- the seam -------------------------------------------------------------------

  attach(host: CredentialHost): void {
    this.host = host
  }

  async current(): Promise<Credential> {
    const c = await this.load()
    return { userId: c.userId, deviceId: c.deviceId, accessToken: c.accessToken }
  }

  /** Go's `refusedDial`: NO TIME IN THIS ONE. While the access token is refused
   *  the client does not dial at all; the request a hold's end allows is a
   *  `/sync`, never an upgrade. */
  async withholdDial(): Promise<boolean> {
    if (!(await this.reload())) return false
    return !this.faults.presentRefusedToken && this.refusedNow(true)
  }

  /** Go's `refusedHold`, re-read from the store on every poll. */
  async withholdSync(): Promise<boolean> {
    const rec = await this.reload()
    if (!rec || this.faults.presentRefusedToken || !this.refusedNow(true)) return false
    // THE WITHDRAWN RULE: never again, and so never at all — the control that
    // is watched stalling.
    if (this.faults.neverPresentRefusedToken) return true
    return refusedHoldAt({ refused: true, links: rec.chain.length, lastSentAt: rec.lastSentAt, now: this.now() })
  }

  refused(): boolean {
    return this.refusedNow(false)
  }

  /** Go's `Run` calling `refreshDueWhenAllowed`: the proactive refresh, bounded
   *  by CANT-127. A held attempt is expected and not logged; a failed one is
   *  logged once, and the dial goes ahead with the held pair. */
  async beforeDial(): Promise<void> {
    const outcome = await this.refreshDueWhenAllowed()
    if (outcome === 'failed') this.log.info('proactive refresh failed; dialing with the held pair')
  }

  async refreshIfDue(): Promise<void> {
    await this.attemptIfDue()
  }

  /** Go's `fetchWith` marking the token and `fetch` calling `refreshAfter401`:
   *  one refresh, then ONE retry — true when the retry should go out. A held
   *  refresh hands back the 401 it already had, without the retry. */
  async onSyncUnauthorized(presented: Credential): Promise<boolean> {
    if (!this.enabled) return false
    this.markRefused(presented.accessToken)
    const outcome = await this.refreshAfter401(presented)
    return outcome === 'refreshed' || outcome === 'skipped'
  }

  /** Go's `markAnswered`. Opening the gate is not a trigger: nothing is sent. */
  answered(): void {
    const now = this.now()
    if (this.answeredAt === null || now > this.answeredAt) this.answeredAt = now
    this.holdNow(true)
  }

  status(): CredentialStatus {
    const rec = this.rec
    const links = rec?.chain.length ?? 0
    return {
      refreshHold: this.holdNow(false),
      nextRefreshAt: nextRefreshAt(links, rec?.lastSentAt ?? null, this.now()),
      ...this.counts,
      chainLength: links,
    }
  }

  // --- the entry points -------------------------------------------------------------

  /**
   * Go's `RefreshIfDue`: the EXPLICIT §1 check, and never held (CANT-127). It is
   * an unconditional "do it now" when due, and costs one link if it settles
   * nothing — what is bounded is the attempts made on the client's own
   * initiative, because those repeat without anybody deciding to.
   */
  async attemptIfDue(): Promise<RefreshOutcome> {
    const rec = await this.reload()
    if (!this.enabled || !rec || !refreshDue(rec, this.now())) return 'not_due'
    return this.refresh((held) => (refreshDue(held, this.now()) ? 'needed' : 'not_needed'))
  }

  /** Go's `refreshDueWhenAllowed`: the same decision, behind the suppressors. */
  async refreshDueWhenAllowed(): Promise<RefreshOutcome> {
    const rec = await this.reload()
    if (!this.enabled || !rec || !refreshDue(rec, this.now())) return 'not_due'
    return this.refreshWhenAllowed((held) => (refreshDue(held, this.now()) ? 'needed' : 'not_needed'))
  }

  /** Go's `refreshAfter401`: refreshes unless some other context already
   *  replaced the pair `used` carried. Not due-gated: a token the device's
   *  clock thinks is live still reaches its refresh. */
  refreshAfter401(used: Pick<Credential, 'accessToken'>): Promise<RefreshOutcome> {
    return this.refreshWhenAllowed((held) => (held.accessToken === used.accessToken ? 'needed' : 'not_needed'))
  }

  /** `kill -9` (Go's `Client.Kill`): nothing in flight reaches the store after
   *  this, and nothing new is proposed. A relaunch is a new layer over the
   *  same store. */
  kill(): void {
    this.killed = true
  }

  // --- CANT-127: the suppressors ----------------------------------------------------

  /**
   * The ONE gated entry point (Go's `refreshWhenAllowed`). The hold is checked
   * twice: here, before the lock, as the cheap skip — a held attempt must not
   * queue on a lock it has no business taking — and under it, as the
   * authority, because another context may have attempted while this one
   * waited, lengthening the chain and moving the stamp.
   */
  private async refreshWhenAllowed(stillNeeded: (held: StoredCredential) => Need): Promise<RefreshOutcome> {
    const h = this.holdNow(true)
    if (h !== 'none') {
      this.countHeld(h)
      return 'held'
    }
    return this.refresh((held) => {
      const again = this.holdNow(true, held)
      if (again !== 'none') {
        this.countHeld(again)
        return 'held'
      }
      return stillNeeded(held)
    })
  }

  /** The hold, from `rec` (the cache unless a fresher read is passed), with
   *  the gate's transitions logged when `note` — Status asks with note false,
   *  because being asked how you are must not write a line. */
  private holdNow(note: boolean, rec: StoredCredential | null = this.rec): RefreshHold {
    const links = rec?.chain.length ?? 0
    if (links === 0) {
      if (note) this.gateKnown = this.gateWasOpen = false
      return 'none'
    }
    if (note) this.warnLongChain(links)
    if (this.faults.unbounded) return 'none'
    const now = this.now()
    if (note) this.noteGate(gateOpen(this.answeredAt, readStamp(rec!.lastSentAt, now)), links)
    return refreshHoldAt({ links, lastSentAt: rec!.lastSentAt, answeredAt: this.answeredAt, now })
  }

  /** One INFO when this context's gate closes and one when it opens — not one
   *  per held attempt, which on a dead network would be ~720 an hour. */
  private noteGate(open: boolean, links: number): void {
    const known = this.gateKnown
    const was = this.gateWasOpen
    this.gateKnown = true
    this.gateWasOpen = open
    if (known && was === open) return
    if (open) this.log.info('refresh gate open: Catenary has answered this context since the last send', { chain_length: links })
    else this.log.info('refresh gate closed: no answer from Catenary since the last send', { chain_length: links })
  }

  private warnLongChain(links: number): void {
    if (links <= CHAIN_WARN_LENGTH || this.warnedLongChain) return
    this.warnedLongChain = true
    this.log.warn('the refresh chain is long: the path is failing, not the device', {
      chain_length: links,
      above: CHAIN_WARN_LENGTH,
    })
  }

  /** A refresh nobody made did not fail: counted under its suppressor, NEVER in
   *  `refreshErrors`. */
  private countHeld(h: RefreshHold): void {
    if (h === 'unreachable') this.counts.refreshesHeldUnreachable++
    else if (h === 'backoff') this.counts.refreshesHeldBackoff++
    this.host?.notify()
  }

  // --- CANT-129: the refused token ----------------------------------------------------

  /** Go's `markRefused`: the ONE site that marks a token, keyed on the token
   *  the request carried, so the pair changing is what ends it. One INFO the
   *  first time a token is marked. */
  private markRefused(token: string): void {
    if (!this.enabled || token === '') return
    const first = this.refusedToken !== token
    this.refusedToken = token
    if (!first) return
    this.log.info(
      'access token refused by Catenary on /sync; until the pair changes it is not dialed with, and it is presented at most once per refresh hold',
      { chain_length: this.rec?.chain.length ?? 0 },
    )
    this.host?.notify()
  }

  /** Go's `refusedNow`: the pair the client would present NOW carries the
   *  refused token. With `note`, also where the mark is retired, once. */
  private refusedNow(note: boolean): boolean {
    const token = this.rec?.accessToken ?? ''
    const marked = this.refusedToken
    if (marked !== null && marked !== token) {
      if (!note) return false
      this.refusedToken = null
      this.log.info('the pair changed; the refused access token is behind us')
      this.host?.notify()
      return false
    }
    return marked !== null && marked === token
  }

  // --- record §2: single-flight -------------------------------------------------------

  /**
   * Go's `refresh`. `stillNeeded` is asked UNDER the lock about the credential
   * as it is persisted now, and answers three ways, because a bool cannot say
   * HELD. PERSIST BEFORE USE: the rotated pair is in the store before the lock
   * is released and before anything presents it.
   */
  private async refresh(stillNeeded: (held: StoredCredential) => Need): Promise<RefreshOutcome> {
    const run = async (): Promise<RefreshOutcome> => {
      const held = await this.load()
      const need = stillNeeded(held)
      if (need === 'not_needed') {
        this.counts.refreshesSkipped++
        this.host?.notify()
        return 'skipped'
      }
      if (need === 'held') return 'held'

      const ex = await this.exchange(held)
      switch (ex.kind) {
        case 'moved':
          this.counts.refreshesSkipped++
          this.host?.notify()
          return 'skipped'
        case 'unauthorized':
          return this.refusedRefresh(held)
        case 'failed':
        case 'killed':
          this.counts.refreshErrors++
          this.host?.notify()
          if (ex.kind === 'failed' && this.faults.alwaysTerminal) return this.terminal('fault: every unsettled refresh is terminal')
          return ex.kind
        case 'ok': {
          const rotated = await this.rotate(ex.next)
          if (rotated !== 'ok') return rotated
          this.counts.refreshes++
          this.host?.notify()
          return 'refreshed'
        }
      }
    }
    try {
      const deviceId = (this.rec ?? (await this.load())).deviceId
      return await (this.faults.refreshUnlocked ? run() : this.lock(credentialLockName(deviceId), run))
    } catch {
      // The store or the lock failed under us: an attempt that settled nothing.
      this.counts.refreshErrors++
      this.host?.notify()
      return 'failed'
    }
  }

  /**
   * Record §5's `/refresh` row (Go's `refused`): Catenary's OWN 401 to the
   * oldest token is terminal only when THE STORED CREDENTIAL IS STILL THE ONE
   * PRESENTED. A context that does not share the lock may have rotated the pair
   * while this request was in flight; then the refused token is merely spent,
   * and the caller uses what is there now.
   */
  private async refusedRefresh(presented: StoredCredential): Promise<RefreshOutcome> {
    const now = await this.load()
    if (now.refreshToken !== presented.refreshToken) {
      this.counts.refreshesSkipped++
      this.host?.notify()
      return 'skipped'
    }
    this.counts.refreshErrors++
    if (this.faults.neverTerminal) {
      this.host?.notify()
      return 'failed'
    }
    return this.terminal('POST /refresh: Catenary refused the stored refresh token')
  }

  /** The credential terminal. NOTHING IS DELETED — not the credential, not the
   *  journal — and the transport's first terminal wins. */
  private terminal(reason: string): RefreshOutcome {
    this.host?.terminal({ kind: 'credential', reason })
    this.host?.notify()
    return 'terminal'
  }

  // --- record §3: the chain -----------------------------------------------------------

  /**
   * Go's `exchange`: WHEN THE OUTCOME OF A REFRESH IS UNKNOWN, WALK THE CHAIN.
   * NEWEST FIRST — the chain's last proposal, with a new link of its own — then
   * ONE STEP BACK PER CATENARY 401, reusing each token's original proposal. A
   * 401 that is not Catenary's never steps the walk; `present_proposal` moves it
   * forward; `fresh_proposal` rewrites the link, bounded. What bounds how OFTEN
   * this is asked is hold.ts's, not this.
   */
  private async exchange(held: StoredCredential): Promise<Exchanged> {
    const chain = held.chain
    let token = held.refreshToken
    if (chain.length > 0 && !this.faults.noChain) token = chain[chain.length - 1].proposal
    let replace = false
    let collisions = 0
    for (let steps = chain.length + WALK_SLACK; steps > 0; steps--) {
      const p = await this.propose(token, replace)
      if (p.kind !== 'ok') return p
      replace = false

      const res = await this.postRefresh(held, token, p.proposal)
      switch (res.kind) {
        case 'ok':
          // THE RESPONSE'S refresh_token, NEVER THE PROPOSAL ON FAITH.
          return res
        case 'unauthorized': {
          const older = await this.older(token)
          if (older === null) return res
          this.counts.refreshWalkBacks++
          token = older
          break
        }
        case 'present_proposal':
          // STOP PRESENTING THAT TOKEN: its proposal is the newest token now.
          token = p.proposal
          break
        case 'fresh_proposal':
          if (++collisions > MAX_FRESH_PROPOSALS) return { kind: 'failed', why: 'refresh: the proposal kept colliding' }
          await this.pause(Math.min(res.afterMs, MAX_RETRY_AFTER_MS))
          replace = true
          break
        default:
          return res
      }
    }
    return { kind: 'failed', why: 'refresh: gave up after too many answers that settled nothing' }
  }

  /**
   * Go's `Journal.propose` with CANT-127's stamp riding on the mint: PERSIST
   * BEFORE SEND. A token already in the chain gets the proposal it was first
   * presented with and writes nothing; otherwise it must be the chain's newest,
   * and one link is appended with `last_sent_at` in the same write. `moved` is
   * another context having rotated the credential since it was read.
   */
  private async propose(token: string, replace: boolean): Promise<Proposed> {
    if (this.faults.noChain) {
      try {
        return { kind: 'ok', proposal: mintProposal(this.random) }
      } catch (e) {
        return { kind: 'failed', why: String(e) }
      }
    }
    const rep = replace || this.faults.proposeAfresh
    const written: { rec: StoredCredential | null } = { rec: null }
    const out = await this.store.update<Proposed>((held) => {
      if (this.killed) return { result: { kind: 'killed', why: 'refresh: killed' } }
      if (!held) return { result: { kind: 'failed', why: 'refresh: no credential' } }
      const link = (i: number): { write?: StoredCredential; result: Proposed } => {
        let proposal: string
        try {
          proposal = mintProposal(this.random)
        } catch (e) {
          // NOTHING IS WRITTEN AND NOTHING IS STAMPED: no request, no send.
          return { result: { kind: 'failed', why: String(e) } }
        }
        written.rec = { ...held, chain: [...held.chain.slice(0, i), { token, proposal }], lastSentAt: this.now() }
        return { write: written.rec, result: { kind: 'ok', proposal } }
      }
      const at = held.chain.findIndex((l) => l.token === token)
      if (at >= 0) return rep ? link(at) : { result: { kind: 'ok', proposal: held.chain[at].proposal } }
      const newest = held.chain.length > 0 ? held.chain[held.chain.length - 1].proposal : held.refreshToken
      if (token !== newest) return { result: { kind: 'moved', why: 'refresh: the persisted credential moved' } }
      return link(held.chain.length)
    })
    if (written.rec) this.rec = written.rec
    return out
  }

  /** The token presented BEFORE `token`; null for the oldest, or one not in the chain. */
  private async older(token: string): Promise<string | null> {
    const rec = await this.load()
    const i = rec.chain.findIndex((l) => l.token === token)
    return i > 0 ? rec.chain[i - 1].token : null
  }

  /** Go's `rotate` + `Journal.Rotate`: AN ANSWER COLLAPSES THE CHAIN, and the
   *  stamp goes with it. Refused after a kill, and for a different device. */
  private async rotate(next: StoredCredential): Promise<'ok' | 'failed' | 'killed'> {
    const written: { rec: StoredCredential | null } = { rec: null }
    const out = await this.store.update<'ok' | 'failed' | 'killed'>((held) => {
      if (this.killed) return { result: 'killed' }
      if (!held || held.deviceId !== next.deviceId || next.accessToken === '' || next.refreshToken === '') {
        return { result: 'failed' }
      }
      written.rec = { ...next, userId: held.userId, chain: [], lastSentAt: null }
      return { write: written.rec, result: 'ok' }
    })
    if (written.rec) this.rec = written.rec
    return out
  }

  /** One `POST /refresh`: `token`, presented with `proposal`. `held` is the
   *  confirmed pair, for what a rotation keeps — the device, and the clock
   *  offset when the response carries no `Date`. */
  private async postRefresh(held: StoredCredential, token: string, proposal: string): Promise<Posted> {
    const ctl = new AbortController()
    const timer = this.timers.setTimeout(() => ctl.abort(), REFRESH_TIMEOUT_MS)
    let res: Response
    let body: string
    try {
      res = await this.fetchFn(`${this.httpBase}/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(encodeRefreshRequest({ refreshToken: token, proposedRefreshToken: proposal })),
        signal: ctl.signal,
      })
      body = await res.text()
    } catch {
      return { kind: 'failed', why: 'refresh: no response' }
    } finally {
      this.timers.clearTimeout(timer)
    }
    const arrived = this.now()
    if (isCatenaryUnauthorized(res.status, body)) return { kind: 'unauthorized', why: 'refresh: 401 unauthorized' }
    if (res.status === 503) {
      // `retry` SAYS WHICH, and the two ask for opposite things (record §5). A
      // 503 without one this client recognises is an unknown outcome.
      let retry: unknown
      try {
        retry = (JSON.parse(body) as { retry?: unknown }).retry
      } catch {
        retry = undefined
      }
      if (retry === 'present_proposal') return { kind: 'present_proposal', why: 'refresh: already rotated into the proposal' }
      if (retry === 'fresh_proposal') {
        const secs = Number.parseInt(res.headers.get('Retry-After') ?? '', 10)
        return { kind: 'fresh_proposal', afterMs: Number.isFinite(secs) && secs > 0 ? secs * 1000 : 0 }
      }
    }
    if (res.status !== 200) return { kind: 'failed', why: `refresh: HTTP ${res.status}` }
    let next: StoredCredential
    try {
      const out = decodeRefreshResponse(JSON.parse(body))
      next = {
        userId: held.userId,
        deviceId: held.deviceId,
        accessToken: out.accessToken,
        accessExpiresAt: parseWireTime(out.accessExpiresAt, 'access_expires_at'),
        refreshToken: out.refreshToken,
        refreshExpiresAt: parseWireTime(out.refreshExpiresAt, 'refresh_expires_at'),
        accessIssuedAt: null,
        clockOffsetMs: held.clockOffsetMs,
        chain: [],
        lastSentAt: null,
      }
    } catch {
      return { kind: 'failed', why: 'refresh: the response did not decode' }
    }
    // A RESPONSE WITH NO USABLE Date KEEPS THE OFFSET IT HAD: the offset
    // describes the device's clock, not one pair, and a stale correction is a
    // better guess than none. The issue time is then the corrected arrival.
    const date = Date.parse(res.headers.get('Date') ?? '')
    if (Number.isFinite(date)) {
      next.accessIssuedAt = date
      next.clockOffsetMs = date - arrived
    } else {
      next.accessIssuedAt = arrived + held.clockOffsetMs
    }
    return { kind: 'ok', next }
  }

  // --- the store ------------------------------------------------------------------------

  /** The persisted credential, re-read, and cached for the synchronous questions. */
  private async load(): Promise<StoredCredential> {
    const rec = await this.store.read()
    if (!rec) throw new NoCredential()
    this.rec = rec
    return rec
  }

  /** `load` for the polls, where no credential is an answer rather than an error. */
  private async reload(): Promise<StoredCredential | null> {
    try {
      return await this.load()
    } catch {
      return null
    }
  }

  private pause(ms: number): Promise<void> {
    if (ms <= 0) return Promise.resolve()
    return new Promise((resolve) => this.timers.setTimeout(resolve, ms))
  }
}
