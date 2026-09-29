/* The fakes the credential layer's tests run against. Mirrors the stubs of
 * internal/client's tests: chain_test.go's `family` (a refresh family kept the
 * way the real server keeps it, which loses requests and responses on
 * instruction), hold_test.go's `estate` (the family plus `/sync` and the
 * upgrade, both refusing any access token but the one the family last minted),
 * and refused_test.go's `hop` and `door` (every request recorded, paths killed
 * and healed while the client runs, and every refusal Catenary made counted).
 *
 * THE FAMILY HAS NO GRACE WINDOW, as Go's has none: every presentation of a
 * spent token is a replay, so a recovery that passes here never presented a
 * spent token at all.
 */

import {
  decodeRefreshRequest, decodeSyncResponse, encodeServerFrame, encodeSyncResponse, type ServerFrame,
} from '@/wire/generated'
import { isCatenaryUnauthorized } from '../credential'
import type { Faults } from '../faults'
import { refreshDelay } from '../hold'
import { createRefreshingCredential, type RefreshingCredential } from '../refresh'
import { MemoryCredentialStore, type CredentialStore, type StoredCredential } from '../credential-store'
import { inProcessLock, manualLifecycle, type Lock, type Logger, type RandomBytes } from '../seams'
import { createTransport, type Transport } from '../transport'
import { DEVICE, FakeClock, FakeNet, type FakeSocket, type LogLine, ME, minDraw, page, ready } from './harness'

export const BASE = 'http://catenary.test'

/** A name padded to the wire's 43-character Token. */
export const padToken = (name: string): string => name + '_'.repeat(43 - name.length)
export const unpad = (t: string): string => t.replace(/_+$/, '')

export const FIRST_REFRESH = padToken('refresh-0')
export const FIRST_ACCESS = padToken('access-0')

// What the family does with the next /refresh. Anything past the script's end
// is answered honestly.
export const ANSWER = 'answer'
export const LOSE_RESPONSE = 'rotate, then lose the response'
export const LOSE_REQUEST = 'lose the request before it arrives'
export const PROXY_401 = 'a 401 from a hop in front'
export const BARE_503 = 'a 503 that says nothing'
export const SAY_PRESENT = '503 present_proposal, whatever is true'
export const SAY_FRESH = '503 fresh_proposal, whatever is true'
export const CATENARY_401 = "Catenary's own 401, whatever is true"
/** The request is held in the network and delivered later, by `unpark()`; the
 *  client never hears an answer to it. */
export const PARK = 'park the request, deliver it later'

export interface Presentation {
  token: string
  proposal: string
}

const json = (status: number, body: unknown, headers: Record<string, string> = {}): Response =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })

/** Catenary's own 401. */
export const unauthorized = (): Response => json(401, { code: 'unauthorized' })
/** A hop's 401: the status, none of the body. */
export const proxy401 = (): Response => new Response('access: session expired', { status: 401 })

/** Go's `family`: a refresh family, kept as the real server keeps it. */
export class FakeFamily {
  script: string[]
  /** Every token the family has held: '' while live, its successor once spent. */
  readonly successor = new Map<string, string>()
  ignoreProposals = false
  revoked = false
  minted = 0
  readonly seen: Presentation[] = []
  readonly statuses: number[] = []
  replays = 0
  /** Runs as each /refresh arrives, before it is answered. */
  onRefresh: ((p: Presentation) => void) | null = null
  /**
   * CANT-125's server half: a spent token presented again WITH THE PROPOSAL IT
   * WAS ROTATED INTO is the loser of a race with its own original, and is told
   * `present_proposal` rather than refused as a replay. Off by default, as Go's
   * stub has it; the real-server version of the race is cmd/catenary's
   * refreshchain_test.go.
   */
  routeCollisions = false
  private parked: Presentation | null = null

  constructor(private readonly clock: FakeClock, first: string, script: string[] = []) {
    this.script = [...script]
    this.successor.set(first, '')
  }

  /** The parked request arrives now, and commits; its answer goes nowhere. */
  unpark(): void {
    const p = this.parked
    this.parked = null
    if (!p) return
    this.script.unshift(ANSWER)
    const onRefresh = this.onRefresh
    this.onRefresh = null
    try {
      this.serve(p)
    } finally {
      this.onRefresh = onRefresh
    }
  }

  /** The one token the family would rotate. */
  live(): string {
    for (const [tok, next] of this.successor) if (next === '') return tok
    return ''
  }

  serve(p: Presentation): Response {
    this.onRefresh?.(p)
    const step = this.script.length > 0 ? this.script.shift()! : ANSWER
    if (step === LOSE_REQUEST) throw new TypeError('fetch failed: the request never arrived')
    if (step === PARK) {
      this.parked = p
      throw new TypeError('fetch failed: the request is still in the network')
    }
    this.seen.push(p)
    const write = (status: number, body: unknown, headers: Record<string, string> = {}) => {
      this.statuses.push(status)
      return json(status, body, headers)
    }
    switch (step) {
      case PROXY_401:
        this.statuses.push(401)
        return proxy401()
      case CATENARY_401:
        return write(401, { code: 'unauthorized' })
      case BARE_503:
        return write(503, { error: 'upstream unavailable' })
      case SAY_PRESENT:
        return write(503, { retry: 'present_proposal' })
      case SAY_FRESH:
        return write(503, { retry: 'fresh_proposal' }, { 'Retry-After': '0' })
    }
    const next = this.successor.get(p.token)
    const collides = this.successor.has(p.proposal)
    if (next === undefined || this.revoked) return write(401, { code: 'unauthorized' })
    if (next !== '' && this.routeCollisions && next === p.proposal) return write(503, { retry: 'present_proposal' })
    if (next !== '') {
      // A SPENT TOKEN, AND NO GRACE: a replay, and the family is gone.
      this.replays++
      this.revoked = true
      return write(401, { code: 'unauthorized' })
    }
    if (collides && !this.ignoreProposals) return write(503, { retry: 'fresh_proposal' }, { 'Retry-After': '0' })
    this.minted++
    let issued = p.proposal
    if (this.ignoreProposals || issued === '') issued = padToken(`minted-by-the-server-${this.minted}`)
    this.successor.set(p.token, issued)
    this.successor.set(issued, '')
    if (step === LOSE_RESPONSE) {
      this.statuses.push(200)
      throw new TypeError('fetch failed: the response was lost')
    }
    const now = this.clock.now()
    return write(
      200,
      {
        access_token: padToken(`access-${this.minted}`),
        access_expires_at: new Date(now + 15 * 60_000).toISOString(),
        refresh_token: issued,
        refresh_expires_at: new Date(now + 24 * 3600_000).toISOString(),
      },
      { Date: new Date(now).toUTCString() },
    )
  }
}

/** One request the client made, as the wire carried it. */
export interface Sent {
  path: string
  token: string
  proposal: string
  at: number
}

/**
 * The estate behind a hop: the family's /refresh, a /sync and an upgrade that
 * refuse any access token but the live one, every request taped, paths killed
 * and healed on instruction, and every refusal Catenary made counted by path.
 */
export class CredNet {
  readonly family: FakeFamily
  readonly sockets = new FakeNet()
  readonly tape: Sent[] = []
  readonly dead = new Set<string>()
  readonly dropFirst = new Map<string, number>()
  /** Paths down until the wall clock reaches this. */
  deadUntil: number | null = null
  readonly deadUntilPaths = new Set<string>()
  readonly refused = { '/sync': 0, '/ws': 0 }
  /** The enrollment access token while it is still good; '' once expired. */
  first: string
  /** Forces a refusal the token check would not make. */
  refuseSync: (() => boolean) | null = null
  refuseWs: (() => boolean) | null = null
  /** Replaces /sync's answer with something that is not Catenary answering. */
  answerSync: (() => Response) | null = null
  /** How many more /sync refusals to deliver, count, and lose the answer to. */
  loseSyncAnswers = 0
  /** Runs as each request leaves, after it is taped. */
  onSent: ((q: Sent) => void) | null = null
  /** Every live socket, for frames a test pushes. */
  readonly open: FakeSocket[] = []

  constructor(readonly clock: FakeClock, opts: { accessLive?: boolean; script?: string[] } = {}) {
    this.family = new FakeFamily(clock, FIRST_REFRESH, opts.script)
    this.first = opts.accessLive ? FIRST_ACCESS : ''
    this.sockets.onDial = (s) => this.upgrade(s)
  }

  liveAccess(): string {
    return this.family.minted === 0 ? this.first : padToken(`access-${this.family.minted}`)
  }

  on(path: string): Sent[] {
    return this.tape.filter((r) => r.path === path)
  }

  count(path: string): number {
    return this.on(path).length
  }

  refusals(): number {
    return this.refused['/sync'] + this.refused['/ws']
  }

  private down(path: string): boolean {
    if (this.dead.has(path)) return true
    if (this.deadUntil !== null && this.deadUntilPaths.has(path) && this.clock.now() < this.deadUntil) return true
    const n = this.dropFirst.get(path) ?? 0
    if (n > 0) {
      this.dropFirst.set(path, n - 1)
      return true
    }
    return false
  }

  readonly fetch = (async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = new URL(String(input))
    const rq: Sent = { path: url.pathname, token: '', proposal: '', at: this.clock.now() }
    let presentation: Presentation | null = null
    if (rq.path === '/refresh') {
      const req = decodeRefreshRequest(JSON.parse(String(init?.body)))
      presentation = { token: req.refreshToken, proposal: req.proposedRefreshToken ?? '' }
      rq.token = presentation.token
      rq.proposal = presentation.proposal
    } else if (rq.path === '/sync') {
      rq.token = (new Headers(init?.headers).get('Authorization') ?? '').replace(/^Bearer /, '')
    }
    this.tape.push(rq)
    this.onSent?.(rq)
    if (this.down(rq.path)) throw new TypeError('fetch failed: the network is down')
    if (presentation) return this.family.serve(presentation)
    if (rq.path === '/sync') return this.sync(rq.token)
    return new Response('not found', { status: 404 })
  }) as unknown as typeof globalThis.fetch

  private sync(token: string): Response {
    if (this.answerSync) return this.answerSync()
    const live = this.liveAccess()
    if (this.refuseSync?.() || live === '' || token !== live) {
      this.refused['/sync']++
      if (this.loseSyncAnswers > 0) {
        // DELIVERED, REFUSED, COUNTED — AND THE ANSWER LOST.
        this.loseSyncAnswers--
        throw new TypeError('fetch failed: the answer was lost')
      }
      return unauthorized()
    }
    return json(200, encodeSyncResponse(page({ logSeq: 0 })))
  }

  /** The upgrade. A browser never sees its status: a refused one is `error`,
   *  then close 1006, and the socket never opened (CANT-31 §5). */
  private upgrade(s: FakeSocket): void {
    const token = s.protocols.find((p) => p.startsWith('catenary.token.'))?.slice('catenary.token.'.length) ?? ''
    this.tape.push({ path: '/ws', token, proposal: '', at: this.clock.now() })
    if (this.down('/ws')) return s.fail()
    const live = this.liveAccess()
    if (this.refuseWs?.() || live === '' || token !== live) {
      this.refused['/ws']++
      return s.fail()
    }
    s.open()
    // The server answers the heartbeat, so a socket that is up stays up.
    const send = s.send.bind(s)
    s.send = (d: string) => {
      send(d)
      const f = JSON.parse(d) as { type?: string; id?: string }
      if (f.type === 'ping' && f.id) queueMicrotask(() => s.isOpen && s.frame({ type: 'pong', id: f.id! }))
    }
    s.frame(ready())
    this.open.push(s)
  }

  /** A frame on every open socket, raw or encoded. */
  push(f: ServerFrame | string): void {
    for (const s of this.open) if (s.isOpen) s.raw(typeof f === 'string' ? f : JSON.stringify(encodeServerFrame(f)))
  }
}

/** A device enrolled with the family's first refresh token and an access token
 *  already inside the floor, so every check finds it due. */
export function enrolled(now: number): StoredCredential {
  return {
    userId: ME,
    deviceId: DEVICE,
    accessToken: FIRST_ACCESS,
    accessExpiresAt: now + 10_000,
    refreshToken: FIRST_REFRESH,
    refreshExpiresAt: now + 60 * 24 * 3600_000,
    accessIssuedAt: null,
    clockOffsetMs: 0,
    chain: [],
    lastSentAt: null,
  }
}

/** Go's `countingRand`: deterministic, never repeating, seeded apart per context. */
export function countingRandom(seed = 0): RandomBytes {
  let n = seed * 1_000_000
  return (len) => {
    n++
    const b = new Uint8Array(len)
    for (let i = 0; i < 6 && i < len; i++) b[len - 1 - i] = Math.floor(n / 256 ** i) % 256
    return b
  }
}

export interface CredRigOptions {
  accessLive?: boolean
  script?: string[]
  dead?: string[]
  dropFirst?: Record<string, number>
  faults?: Partial<Faults>
  random?: RandomBytes
  cred?: (c: StoredCredential) => StoredCredential
  store?: CredentialStore
  lock?: Lock
}

export interface CredRig {
  clock: FakeClock
  net: CredNet
  store: CredentialStore
  lock: Lock
  logs: LogLine[]
  logger: Logger
  /** The first context. */
  c: RefreshingCredential
  /** Another context over the same store and lock: a second tab. */
  layer(faults?: Partial<Faults>, random?: RandomBytes, fetch?: typeof globalThis.fetch): RefreshingCredential
  /** A transport over a context (default the first). */
  transport(c?: RefreshingCredential, faults?: Partial<Faults>): { t: Transport; lifecycle: ReturnType<typeof manualLifecycle> }
  held(): Promise<StoredCredential>
  matching(sub: string): LogLine[]
}

export function credRig(o: CredRigOptions = {}): CredRig {
  const clock = new FakeClock()
  const net = new CredNet(clock, { accessLive: o.accessLive, script: o.script })
  for (const p of o.dead ?? []) net.dead.add(p)
  for (const [p, n] of Object.entries(o.dropFirst ?? {})) net.dropFirst.set(p, n)
  const base = enrolled(clock.now())
  const store = o.store ?? new MemoryCredentialStore(o.cred ? o.cred(base) : base)
  const lock = o.lock ?? inProcessLock()
  const logs: LogLine[] = []
  const logger: Logger = {
    info: (msg, fields) => logs.push({ level: 'info', msg, fields }),
    warn: (msg, fields) => logs.push({ level: 'warn', msg, fields }),
  }
  let seed = 0
  const layer = (faults?: Partial<Faults>, random?: RandomBytes, fetch?: typeof globalThis.fetch) =>
    createRefreshingCredential({
      baseUrl: BASE, store, lock, fetch: fetch ?? net.fetch, now: clock.now, timers: clock,
      random: random ?? countingRandom(++seed), logger, faults,
    })
  const c = layer(o.faults, o.random)
  return {
    clock, net, store, lock, logs, logger, c, layer,
    transport(cc = c, faults = o.faults) {
      const lifecycle = manualLifecycle()
      const t = createTransport({
        baseUrl: BASE, credential: cc, clientVersion: '0.0.0-test', WebSocket: net.sockets.WebSocket, fetch: net.fetch,
        now: clock.now, timers: clock, random: minDraw, lifecycle, logger, faults,
      })
      return { t, lifecycle }
    },
    async held() {
      const h = await store.read()
      if (!h) throw new Error('no credential held')
      return h
    },
    matching: (sub) => logs.filter((l) => l.msg.includes(sub)),
  }
}

/**
 * Go's `c.fetch(ctx, 0)` for a context with no transport: one `GET /sync`, and
 * on Catenary's own 401 the reactive refresh and ONE retry — the same calls,
 * in the same order, the transport's `fetchPage` makes on the seam. Catenary
 * answered only for a page the generated decoder accepts, or its own 401.
 */
export async function fetchVia(r: CredRig, c: RefreshingCredential): Promise<'page' | 'unauthorized' | 'error'> {
  const once = async (token: string): Promise<'page' | 'unauthorized' | 'error'> => {
    let res: Response
    try {
      res = await r.net.fetch(`${BASE}/sync?after=0`, { headers: { Authorization: `Bearer ${token}` } })
    } catch {
      return 'error'
    }
    const body = await res.text()
    if (isCatenaryUnauthorized(res.status, body)) {
      c.answered()
      return 'unauthorized'
    }
    if (res.status !== 200) return 'error'
    try {
      decodeSyncResponse(JSON.parse(body))
    } catch {
      return 'error'
    }
    c.answered()
    return 'page'
  }
  const used = await c.current()
  const first = await once(used.accessToken)
  if (first !== 'unauthorized') return first
  if (!(await c.onSyncUnauthorized(used))) return 'unauthorized'
  return once((await c.current()).accessToken)
}

/** Advances the fake clock in steps until `pred` holds; returns the time spent. */
export async function advanceUntil(clock: FakeClock, pred: () => boolean, what: string, step = 1_000, max = 3 * 3600_000): Promise<number> {
  let spent = 0
  while (!pred()) {
    if (spent >= max) throw new Error(`timed out after ${spent} simulated ms waiting for ${what}`)
    await clock.advance(step)
    spent += step
  }
  return spent
}

/** Criterion 12's floor, which no test is exempt from: a spent token is never
 *  presented, and the family is never revoked. */
export function noReplays(f: FakeFamily): string | null {
  return f.replays === 0 && !f.revoked ? null : `replays ${f.replays}, revoked ${f.revoked}; presentations ${JSON.stringify(f.seen)}`
}

/** Go's `subsequence`: `sub` is `all` with elements removed, none added or reordered. */
export function subsequence(sub: Presentation[], all: Presentation[]): boolean {
  let i = 0
  for (const p of all) if (i < sub.length && sub[i].token === p.token && sub[i].proposal === p.proposal) i++
  return i === sub.length
}

/** Go's `attemptsWithin`: how many automatic attempts CANT-127's curve allows
 *  in `ms` — the first, then one each time the delay has elapsed. */
export function attemptsWithin(ms: number): number {
  let n = 0
  let at = 0
  while (at <= ms) {
    n++
    at += refreshDelay(n)
  }
  return n
}
