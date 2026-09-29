/* The transport — mirrors internal/client/client.go: the session loop, the
 * dial, frame dispatch, and the seam CANT-36 adapts to.
 *
 * One transport per tab, on the main thread (CANT-35 ruling 1 → A). Framework
 * agnostic: nothing here imports Vue, and every source of nondeterminism comes
 * in through a seam (seams.ts), so the whole state machine runs under a fake
 * clock, a fake socket and a fake server.
 *
 * THE RULES ARE CITED, NOT RESTATED, so this file does not become a copy that
 * drifts: CANT-24's five obligations (docs/decisions/cant-24-resume.md),
 * CANT-103's rules 1–7 (docs/decisions/cant-103-conversation-introduction.md),
 * CANT-23's heartbeat (heartbeat.ts), CANT-31 §4's close table (closes.ts) and
 * §6's terminal states (terminal.ts), and CANT-35's rulings 3, 4 and 5
 * (backoff.ts, and the receipt and lifecycle arms below).
 *
 * THE CREDENTIAL IS A SEAM. Every refresh hook is on `CredentialSeam`
 * (credential.ts), called where Go calls its counterpart: `heldCredential` is
 * Go's `Refresh: false`, and `RefreshingCredential` (refresh.ts, CANT-152) is
 * CANT-31 §1–§6 with CANT-127's hold and CANT-129's refused wait.
 */

import {
  WIRE_VERSION,
  decodeServerFrame,
  decodeSyncResponse,
  encodeClientHello,
  encodeClientRead,
  encodeClientSend,
  encodeClientTyping,
  encodePing,
  encodePong,
  setServerWireVersion,
} from '@/wire/generated'
import type {
  ClientRead,
  ClientSend,
  ClientTyping,
  Message,
  ServerAck,
  ServerError,
  ServerFrame,
  ServerReady,
  ServerReceipt,
  ServerTyping,
  SyncResponse,
  Uuid,
} from '@/wire/generated'
import { BACKOFF_MAX_MS, BACKOFF_MIN_MS, advance, jitteredWait, resetsRamp, unitFromBytes } from './backoff'
import { CatchUp } from './catchup'
import { type CloseVerdict, classifyClose, closeStatusKey } from './closes'
import { type Credential, type CredentialSeam, isCatenaryUnauthorized } from './credential'
import { type Faults, NO_FAULTS } from './faults'
import { Heartbeat } from './heartbeat'
import { type Applied, type Journal, type JournalSnapshot, MemoryJournal } from './journal'
import {
  type Clock,
  type Lifecycle,
  type LifecycleEvent,
  type Logger,
  type RandomBytes,
  type Timers,
  type WebSocketCtor,
  type WebSocketLike,
  browserLifecycle,
  browserTimers,
  consoleLogger,
  cryptoRandom,
} from './seams'
import { type Stats, type TransportStatus, emptyStats } from './status'
import { NOT_TERMINAL, type Terminal } from './terminal'

export const SUBPROTOCOL_V1 = 'catenary.v1'
export const TOKEN_SUBPROTOCOL_PREFIX = 'catenary.token.'

/** Go's `dialTimeout` and `syncTimeout`: a socket that has not opened, or a
 *  `/sync` that has not answered, in this long is a failed attempt. */
const DIAL_TIMEOUT_MS = 30_000
const SYNC_TIMEOUT_MS = 30_000

export interface TransportConfig {
  /** The server's origin, `http://` or `https://`. `/ws` and `/sync` are appended. */
  baseUrl: string
  /** The credential layer: `createRefreshingCredential(...)` in the app,
   *  `heldCredential(...)` where the pair is presented as held (the rigs). */
  credential: CredentialSeam
  /** Default: a fresh in-memory journal (CANT-35 ruling 2 → B). */
  journal?: Journal
  /** Goes on the hello as `catenary-web/<version>`. Never parsed. */
  clientVersion?: string
  /** The page size `/sync` is asked for. Absent is the server's. */
  syncLimit?: number

  WebSocket?: WebSocketCtor
  fetch?: typeof globalThis.fetch
  now?: Clock
  timers?: Timers
  random?: RandomBytes
  lifecycle?: Lifecycle
  logger?: Logger

  /** The dial ramp's floor and ceiling. FOR RIGS ONLY, as Go's
   *  `Config.BackoffMin`/`BackoffMax` are: soakrig runs both cohorts at
   *  100 ms / 2 s so they storm alike. */
  backoffMinMs?: number
  backoffMaxMs?: number

  /** Never set outside a test or a rig proving an assertion can fail. */
  faults?: Partial<Faults>
}

/**
 * What the outbox (CANT-36) needs from this layer to apply CANT-31 §7 — the
 * fact, not the policy. Emitted once per session, when it ends, and never when
 * one becomes ready: `ready` comes from `subscribe()` and only from it.
 */
export interface SessionEnd {
  /** Monotonically increasing per dial. */
  sessionGen: number
  /** False for a pure dial failure: no socket ever opened. */
  opened: boolean
  readied: boolean
  /** The close status the peer sent; null for none (a browser's 1006 or 1005,
   *  Go's -1), which includes a socket this client severed itself and one it
   *  closed on `stop()` or on entering a terminal state. */
  closeCode: number | null
  /** CANT-31 §4's *preceded by*: the last message event before the close, if
   *  it was an `error` carrying no `client_id`. */
  preceding: ServerError | null
  /** `closeCode === 1008 && preceding === null` — §7's trigger, and the one
   *  fact CANT-36's plan calls `closed{bare1008}`. */
  bare1008: boolean
  verdict: CloseVerdict
}

export interface Transport {
  /** Idempotent. Refuses if terminal: a relaunch is a new Transport over the
   *  same stores. */
  start(): void
  /** A clean close (1000). Not terminal; `start()` again resumes. */
  stop(): void
  status(): TransportStatus
  /** Called with a fresh status on every change. Returns the unsubscribe. */
  subscribe(fn: (s: TransportStatus) => void): () => void
  /** Resolves with the `ack`. Never mints or alters `client_id`. Rejects with
   *  `NotConnected`, `SessionEnded`, `SendRefused` or `SendInFlight`. */
  send(f: ClientSend): Promise<ServerAck>
  read(f: ClientRead): void
  typing(f: ClientTyping): void
  /** A trigger (obligation 3; obligation 5's hook). Withheld while a token is
   *  refused. */
  catchUp(): void
  /** Resets the ramp to its floor and dials now. No effect while terminal or
   *  while a token is refused. */
  retryNow(): void
  /** The explicit §1 check. Never held. */
  refreshIfDue(): Promise<void>
  onSessionEnd(fn: (e: SessionEnd) => void): () => void
  onApply(fn: (e: Applied) => void): () => void
  snapshot(): JournalSnapshot
  /**
   * Inbound `typing` frames. AN ADDITION BEYOND THE PLAN'S SURFACE, which names
   * none: the frame has to reach the thread somehow, and this carries it and
   * decides nothing about it (the start→stop pairing is the composer's).
   */
  onTyping(fn: (f: ServerTyping) => void): () => void
}

/** `send()` with no open socket. Nothing was written. */
export class NotConnected extends Error {
  constructor() {
    super('transport: no socket is open')
    this.name = 'NotConnected'
  }
}

/** The socket closed before the send was answered. THE OUTCOME IS UNKNOWN:
 *  retrying with the SAME `client_id` is safe. */
export class SessionEnded extends Error {
  constructor() {
    super('transport: the session ended before the send was answered')
    this.name = 'SessionEnded'
  }
}

/** The server refused this send with an `error` frame naming its `client_id`. */
export class SendRefused extends Error {
  constructor(readonly frame: ServerError) {
    super(`transport: send refused: ${frame.code}: ${frame.message}`)
    this.name = 'SendRefused'
  }
}

/** A send with this `client_id` is already awaiting its answer. */
export class SendInFlight extends Error {
  constructor() {
    super('transport: a send with this client_id is already awaiting its answer')
    this.name = 'SendInFlight'
  }
}

export function createTransport(cfg: TransportConfig): Transport {
  return new SocketTransport(cfg)
}

type DialWaitResult = 'elapsed' | 'early' | 'retryNow' | 'stopped'

/** A session's end, with what the session loop needs beyond the public fact. */
type DialEnd = SessionEnd & {
  /** How long it stayed `ready`; null if it never was. */
  readiedForMs: number | null
  intervalSec: number | null
  /** Why a terminal verdict is terminal. */
  reason: string
}

interface Session {
  ws: WebSocketLike
  gen: number
  opened: boolean
  readied: boolean
  readyAt: number | null
  intervalSec: number | null
  preceding: ServerError | null
  ended: boolean
  dialTimer: unknown
  finish(end: DialEnd): void
}

class SocketTransport implements Transport {
  private readonly cred: CredentialSeam
  private readonly journal: Journal
  private readonly WS: WebSocketCtor
  private readonly fetchFn: typeof globalThis.fetch
  private readonly now: Clock
  private readonly timers: Timers
  private readonly random: RandomBytes
  private readonly lifecycle: Lifecycle
  private readonly log: Logger
  private readonly faults: Faults
  private readonly httpBase: string
  private readonly wsUrl: string
  private readonly clientInfo: string
  private readonly min: number
  private readonly max: number
  private readonly heartbeat: Heartbeat
  private readonly catchup: CatchUp

  private started = false
  /** Bumped by every start and stop, so a loop from an earlier run sees it is
   *  over at its next await. */
  private runId = 0
  private abort: AbortController | null = null
  private unlisten: (() => void) | null = null
  private terminal: Terminal = NOT_TERMINAL

  private session: Session | null = null
  /** The socket `send` writes to: published only once the hello is on it. */
  private conn: WebSocketLike | null = null
  private ready = false
  /** A dial is in progress and has not reached `ready` (Go's `connecting`). */
  private connecting = false
  private sessionId: string | null = null
  private intervalSec: number | null = null
  private missedPongLimit: number | null = null
  /** From the most recent `ready` in this transport's life; null before any. */
  private learnedIntervalSec: number | null = null
  private sessionGen = 0
  private selfUserId: Uuid | null = null

  private stats: Stats = emptyStats()
  private attempt = 0
  private nextDialAt: number | null = null
  private dialWait: { atMax: boolean; resolve(r: DialWaitResult): void } | null = null
  private lastEarlyDialAt: number | null = null
  private lastPolicyAt: number | null = null

  /** Journal writes, one at a time, in arrival order. */
  private writes: Promise<void> = Promise.resolve()
  private pendingWrites = 0
  /** Bumped when a wipe is decided: a page requested in an earlier epoch is
   *  dropped, not applied over the empty store (obligation 4). */
  private epoch = 0
  private wipes = 0

  private readonly waiters = new Map<Uuid, { resolve(a: ServerAck): void; reject(e: Error): void }>()
  private readonly statusFns = new Set<(s: TransportStatus) => void>()
  private readonly endFns = new Set<(e: SessionEnd) => void>()
  private readonly applyFns = new Set<(e: Applied) => void>()
  private readonly typingFns = new Set<(f: ServerTyping) => void>()

  constructor(cfg: TransportConfig) {
    const base = new URL(cfg.baseUrl.replace(/\/+$/, ''))
    if (base.protocol !== 'http:' && base.protocol !== 'https:') {
      throw new Error(`transport: baseUrl scheme ${base.protocol}, want http: or https:`)
    }
    this.httpBase = base.toString().replace(/\/+$/, '')
    const ws = new URL(this.httpBase)
    ws.protocol = base.protocol === 'https:' ? 'wss:' : 'ws:'
    ws.pathname = ws.pathname.replace(/\/+$/, '') + '/ws'
    this.wsUrl = ws.toString()

    this.cred = cfg.credential
    this.journal = cfg.journal ?? new MemoryJournal()
    this.WS = cfg.WebSocket ?? (globalThis.WebSocket as unknown as WebSocketCtor)
    this.fetchFn = cfg.fetch ?? ((input, init) => globalThis.fetch(input, init))
    this.now = cfg.now ?? (() => Date.now())
    this.timers = cfg.timers ?? browserTimers
    this.random = cfg.random ?? cryptoRandom
    this.lifecycle = cfg.lifecycle ?? browserLifecycle()
    this.log = cfg.logger ?? consoleLogger
    this.faults = { ...NO_FAULTS, ...cfg.faults }
    this.clientInfo = `catenary-web/${cfg.clientVersion ?? 'dev'}`
    this.min = cfg.backoffMinMs && cfg.backoffMinMs > 0 ? cfg.backoffMinMs : BACKOFF_MIN_MS
    this.max = Math.max(cfg.backoffMaxMs && cfg.backoffMaxMs > 0 ? cfg.backoffMaxMs : BACKOFF_MAX_MS, this.min)
    const syncLimit = cfg.syncLimit

    this.heartbeat = new Heartbeat({
      timers: this.timers,
      now: this.now,
      ping: (id) => this.write(encodePing({ type: 'ping', id })),
      sever: (n) => {
        this.stats.heartbeatSevers++
        this.log.warn('severing: pings unanswered', { outstanding: n, missed_pong_limit: this.missedPongLimit })
        if (this.session) this.abandon(this.session)
      },
      wake: () => void this.wake('clock'),
      pingsSent: () => {
        this.stats.pingsSent++
        this.notify()
      },
      pongReceived: (rtt) => {
        this.stats.pongsReceived++
        if (rtt !== null) this.stats.lastRttMs = rtt
        this.notify()
      },
    })

    this.catchup = new CatchUp({
      faults: this.faults,
      stats: this.stats,
      timers: this.timers,
      log: this.log,
      backoffMinMs: this.min,
      backoffMaxMs: this.max,
      sessionReady: () => this.ready,
      tokenRefused: () => this.cred.refused(),
      withholdSync: () => this.cred.withholdSync(),
      settled: () => this.settled(),
      fetchPage: (after) => this.fetchPage(after, syncLimit),
      applyPage: (page, epoch) => this.applyPage(page, epoch),
      notify: () => this.notify(),
    })

    this.cred.attach({
      terminal: (t) => this.enterTerminal(t),
      notify: () => this.notify(),
    })
  }

  // --- the public surface ------------------------------------------------------

  start(): void {
    if (this.started) return
    if (this.terminal.kind !== 'none') {
      this.log.warn('start refused: this transport is terminal; a relaunch is a new transport', {
        kind: this.terminal.kind,
      })
      return
    }
    this.started = true
    const run = ++this.runId
    this.abort = new AbortController()
    this.unlisten = this.lifecycle.subscribe((e) => this.onLifecycle(e))
    void this.catchup.run(() => this.alive(run))
    void this.runLoop(run)
    this.notify()
  }

  stop(): void {
    if (!this.started) return
    this.halt()
    this.notify()
  }

  status(): TransportStatus {
    const c = this.cred.status()
    const stats: Stats = {
      ...this.stats,
      closeStatuses: { ...this.stats.closeStatuses },
      refreshes: c.refreshes,
      refreshesSkipped: c.refreshesSkipped,
      refreshErrors: c.refreshErrors,
      refreshWalkBacks: c.refreshWalkBacks,
      chainLength: c.chainLength,
      refreshesHeldUnreachable: c.refreshesHeldUnreachable,
      refreshesHeldBackoff: c.refreshesHeldBackoff,
    }
    return {
      terminal: { ...this.terminal },
      refreshHold: c.refreshHold,
      tokenRefused: this.cred.refused(),
      nextRefreshAt: c.nextRefreshAt,
      connected: this.conn !== null,
      ready: this.ready,
      sessionId: this.sessionId,
      heartbeatIntervalSec: this.intervalSec,
      missedPongLimit: this.missedPongLimit,
      caughtUp: this.catchup.caughtUp(),
      cursor: this.journal.cursor(),
      attempt: this.attempt,
      nextDialAt: this.nextDialAt,
      stats,
      messages: this.journal.messageCount(),
      wipes: this.wipes,
    }
  }

  subscribe(fn: (s: TransportStatus) => void): () => void {
    this.statusFns.add(fn)
    return () => this.statusFns.delete(fn)
  }

  send(f: ClientSend): Promise<ServerAck> {
    const conn = this.conn
    if (!conn) return Promise.reject(new NotConnected())
    if (this.waiters.has(f.clientId)) return Promise.reject(new SendInFlight())
    return new Promise<ServerAck>((resolve, reject) => {
      this.waiters.set(f.clientId, { resolve, reject })
      try {
        // THE FRAME GOES AS THE CALLER BUILT IT: a retry is the same frame,
        // client_id included.
        conn.send(JSON.stringify(encodeClientSend(f)))
      } catch {
        this.waiters.delete(f.clientId)
        reject(new NotConnected())
      }
    })
  }

  read(f: ClientRead): void {
    this.write(encodeClientRead(f))
  }

  typing(f: ClientTyping): void {
    this.write(encodeClientTyping(f))
  }

  catchUp(): void {
    if (this.withheldSync()) return
    this.catchup.trigger()
  }

  retryNow(): void {
    if (this.terminal.kind !== 'none' || !this.started) return
    if (this.cred.refused()) {
      this.stats.dialsWithheld++
      this.notify()
      return
    }
    this.dialWait?.resolve('retryNow')
  }

  refreshIfDue(): Promise<void> {
    return this.cred.refreshIfDue()
  }

  onSessionEnd(fn: (e: SessionEnd) => void): () => void {
    this.endFns.add(fn)
    return () => this.endFns.delete(fn)
  }

  onApply(fn: (e: Applied) => void): () => void {
    this.applyFns.add(fn)
    return () => this.applyFns.delete(fn)
  }

  onTyping(fn: (f: ServerTyping) => void): () => void {
    this.typingFns.add(fn)
    return () => this.typingFns.delete(fn)
  }

  snapshot(): JournalSnapshot {
    return this.journal.snapshot()
  }

  // --- the session loop (Client.Run) ------------------------------------------

  private alive(run: number): boolean {
    return this.started && run === this.runId && this.terminal.kind === 'none'
  }

  private async runLoop(run: number): Promise<void> {
    let backoff = this.min
    while (this.alive(run)) {
      // CANT-129: no dial with a token Catenary has refused. Polled at the dial
      // cadence, counted, not logged per poll.
      if (await this.cred.withholdDial()) {
        if (!this.alive(run)) break
        this.stats.dialsWithheld++
        this.notify()
        if ((await this.dialWaitFor(this.max, true)) === 'stopped') break
        continue
      }
      // The proactive refresh (CANT-124), bounded by CANT-127. Held: a no-op.
      await this.cred.beforeDial()
      if (!this.alive(run)) break

      this.stats.dials++
      this.attempt++
      this.connecting = true
      // A DIAL IS A TRIGGER, PULLED BEFORE THE UPGRADE, so the /sync goes out
      // beside it rather than a round trip later on `ready`.
      this.catchup.trigger()
      this.notify()

      const end = await this.dial(run)
      this.emitSessionEnd(end)
      if (!this.alive(run)) break

      if (end.verdict === 'terminal_protocol') {
        this.enterTerminal({ kind: 'protocol', reason: end.reason })
        break
      }

      // RULING 3 → B. The ramp resets only after a session that stayed ready
      // for a heartbeat interval; `error{internal}` waits the maximum, and no
      // wake signal shortens that wait.
      const stable = resetsRamp(end.readiedForMs, end.intervalSec)
      if (stable) {
        backoff = this.min
        this.attempt = 0
      }
      const atMax = end.verdict === 'reconnect_at_maximum'
      if (atMax) backoff = this.max
      const wait = jitteredWait(backoff, unitFromBytes(this.random(4)))
      this.log.info('session ended; reconnecting', {
        close: end.closeCode ?? -1,
        opened: end.opened,
        backoff_ms: Math.round(wait),
      })
      const r = await this.dialWaitFor(wait, atMax)
      if (r === 'stopped') break
      if (r === 'retryNow') {
        backoff = this.min
        this.attempt = 0
      } else if (!stable) {
        // An early dial takes the pending dial's place and advances the ramp
        // as any failed dial does: it never resets it.
        backoff = advance(backoff, this.max)
      }
    }
  }

  /** Waits for the next dial: the backoff, a wake signal's early dial, or
   *  `retryNow()`. */
  private dialWaitFor(ms: number, atMax: boolean): Promise<DialWaitResult> {
    return new Promise((resolve) => {
      const w = {
        atMax,
        resolve: (r: DialWaitResult) => {
          if (this.dialWait !== w) return
          this.dialWait = null
          this.nextDialAt = null
          this.timers.clearTimeout(handle)
          resolve(r)
          this.notify()
        },
      }
      const handle = this.timers.setTimeout(() => w.resolve('elapsed'), ms)
      this.dialWait = w
      this.nextDialAt = this.now() + ms
      this.notify()
    })
  }

  /** One socket, from dial to close (Go's `session`). */
  private dial(run: number): Promise<DialEnd> {
    return new Promise((resolve) => {
      const failed = (why: string | null) => {
        this.connecting = false
        if (why !== null) {
          this.stats.dialErrors++
          this.stats.lastClose = why
        }
        resolve({
          sessionGen: ++this.sessionGen, opened: false, readied: false, closeCode: null, preceding: null,
          bare1008: false, verdict: 'reconnect', readiedForMs: null, intervalSec: null, reason: '',
        })
      }
      this.cred.current().then(
        (cred) => (this.alive(run) ? this.openSocket(cred, resolve, failed) : failed(null)),
        () => failed('dial: no credential'),
      )
    })
  }

  private openSocket(cred: Credential, resolve: (e: DialEnd) => void, failed: (why: string) => void): void {
    this.selfUserId = cred.userId
    let ws: WebSocketLike
    try {
      ws = new this.WS(this.wsUrl, [SUBPROTOCOL_V1, TOKEN_SUBPROTOCOL_PREFIX + cred.accessToken])
    } catch {
      // Not the error's text: a constructor may quote its protocols, and one
      // of them carries the token.
      return failed('dial: the socket could not be constructed')
    }
    const gen = ++this.sessionGen
    const s: Session = {
      ws, gen, opened: false, readied: false, readyAt: null, intervalSec: null, preceding: null, ended: false,
      dialTimer: null,
      finish: resolve,
    }
    this.session = s
    s.dialTimer = this.timers.setTimeout(() => {
      if (!s.opened) this.abandon(s)
    }, DIAL_TIMEOUT_MS)

    ws.onopen = () => {
      if (s.ended) return
      s.opened = true
      this.timers.clearTimeout(s.dialTimer)
      // THE HELLO IS WRITTEN BEFORE THE SOCKET IS PUBLISHED. The server closes
      // a session whose first frame is not a hello with a bare 1008, and
      // `send` writes to whatever `conn` holds — so until the hello is on the
      // wire, a send gets NotConnected rather than severing the session.
      const cursor = this.journal.cursor()
      const hello = encodeClientHello({
        type: 'hello',
        wireVersion: WIRE_VERSION,
        deviceId: cred.deviceId,
        resumeFromLogSeq: cursor === null ? undefined : cursor,
        clientInfo: this.clientInfo,
      })
      try {
        ws.send(JSON.stringify(hello))
      } catch {
        this.abandon(s)
        return
      }
      this.conn = ws
      this.notify()
    }
    ws.onmessage = (ev) => {
      if (!s.ended) this.onMessage(s, ev.data)
    }
    ws.onclose = (ev) => this.endSession(s, ev.code)
    ws.onerror = () => {
      // A close always follows; that is where the session ends.
    }
  }

  /** Ends a session this client gave up on — a heartbeat sever, a dial
   *  timeout — with no close frame received: Go's `CloseNow`. */
  private abandon(s: Session): void {
    try {
      s.ws.close()
    } catch {
      // Closing a socket that never opened may throw; it is gone either way.
    }
    this.endSession(s, null)
  }

  /** `stopping`: this client closed it on purpose (`stop()`, terminal), so a
   *  socket that never opened is not a dial error. */
  private endSession(s: Session, rawCode: number | null, stopping = false): void {
    if (s.ended) return
    s.ended = true
    this.timers.clearTimeout(s.dialTimer)
    s.ws.onopen = s.ws.onmessage = s.ws.onclose = s.ws.onerror = null
    if (this.session === s) this.session = null
    if (this.conn === s.ws) this.conn = null
    this.ready = false
    this.connecting = false
    this.heartbeat.stop()

    const waiters = [...this.waiters.values()]
    this.waiters.clear()
    for (const w of waiters) w.reject(new SessionEnded())

    // A close this client made itself is not one the peer sent: it is
    // neither counted nor classified, and its SessionEnd carries no code.
    const key = closeStatusKey(stopping ? null : rawCode)
    const closeCode = key === -1 ? null : key
    let verdict: CloseVerdict = 'reconnect'
    let reason = ''
    if (stopping) {
      this.stats.lastClose = 'closed by this client'
    } else if (s.opened) {
      this.stats.closeStatuses[key] = (this.stats.closeStatuses[key] ?? 0) + 1
      this.stats.lastClose = closeCode === null ? 'closed with no close frame' : `close ${closeCode}`
      ;({ verdict, reason } = classifyClose(closeCode, s.preceding))
      if (this.faults.neverTerminal) {
        verdict = 'reconnect'
      } else if (this.faults.alwaysTerminal) {
        verdict = 'terminal_protocol'
        reason = 'fault: every close is terminal'
      }
    } else {
      // A pure dial failure: never a close status (Go's CANT-27 review found
      // it double-counted into -1 once), and never a verdict to read.
      this.stats.dialErrors++
      this.stats.lastClose = 'dial failed: the socket never opened'
    }
    s.finish({
      sessionGen: s.gen,
      opened: s.opened,
      readied: s.readied,
      closeCode,
      preceding: s.preceding,
      bare1008: closeCode === 1008 && s.preceding === null,
      verdict,
      readiedForMs: s.readyAt === null ? null : this.now() - s.readyAt,
      intervalSec: s.intervalSec,
      reason,
    })
    this.notify()
  }

  private emitSessionEnd(end: SessionEnd): void {
    const e: SessionEnd = {
      sessionGen: end.sessionGen, opened: end.opened, readied: end.readied, closeCode: end.closeCode,
      preceding: end.preceding, bare1008: end.bare1008, verdict: end.verdict,
    }
    for (const fn of [...this.endFns]) {
      try {
        fn(e)
      } catch (err) {
        this.log.warn('a session-end listener threw', { error: String(err) })
      }
    }
  }

  // --- frames ------------------------------------------------------------------

  private onMessage(s: Session, data: unknown): void {
    // EVERY MESSAGE EVENT CLEARS *PRECEDED BY*, BEFORE ANY DECODING (CANT-31
    // §4): a frame the decoder returns null for, one it throws on, text that
    // is not JSON, a binary message. Only a session-level `error` sets it again.
    s.preceding = null
    if (typeof data !== 'string') {
      this.stats.undecodable++
      this.log.warn('server sent a binary message; ignored')
      this.notify()
      return
    }
    let f: ServerFrame | null
    try {
      f = decodeServerFrame(JSON.parse(data))
    } catch (e) {
      this.stats.undecodable++
      this.log.warn('server frame the generated decoder refuses', { error: e instanceof Error ? e.message : String(e) })
      this.notify()
      return
    }
    // CATENARY ANSWERED THIS CONTEXT (CANT-127's gate): a frame the generated
    // decoder accepted, an unknown tag included. One it refused is not.
    this.cred.answered()
    if (f === null) return // an unknown tag: ignored, as every decoder is told to

    switch (f.type) {
      case 'ready':
        this.onReady(s, f)
        break
      case 'ping':
        this.write(encodePong({ type: 'pong', id: f.id }))
        break
      case 'pong':
        this.heartbeat.pong(f.id)
        break
      case 'ack': {
        const w = this.waiters.get(f.clientId)
        this.waiters.delete(f.clientId)
        w?.resolve(f)
        break
      }
      case 'error':
        if (f.clientId !== undefined) {
          const w = this.waiters.get(f.clientId)
          this.waiters.delete(f.clientId)
          w?.reject(new SendRefused(f))
        } else {
          s.preceding = f
          this.log.warn('server error', { code: f.code, message: f.message })
        }
        break
      case 'message':
        this.onLiveMessage(f.message)
        break
      case 'conversation':
        // CANT-103: applied idempotently by id; moves no cursor.
        this.enqueue(async () => this.emitApply(await this.journal.applyLive({ conversations: [f.conversation] }, this.faults)))
        break
      case 'user':
        this.enqueue(async () => this.emitApply(await this.journal.applyLive({ users: [f.user] }, this.faults)))
        break
      case 'receipt':
        this.onReceipt(f)
        break
      case 'typing':
        for (const fn of [...this.typingFns]) fn(f)
        break
      case 'resync_required':
        // A trigger (obligation 3), and nothing else: the cursor is the last
        // page's high water, so a catch-up from it covers the gap.
        this.stats.resyncs++
        this.catchup.trigger()
        break
    }
    this.notify()
  }

  private onReady(s: Session, r: ServerReady): void {
    s.readied = true
    s.readyAt = this.now()
    s.intervalSec = r.heartbeatIntervalSec
    this.ready = true
    this.connecting = false
    this.sessionId = r.sessionId
    this.intervalSec = r.heartbeatIntervalSec
    this.missedPongLimit = r.missedPongLimit
    this.learnedIntervalSec = r.heartbeatIntervalSec
    this.stats.readys++
    setServerWireVersion(r.wireVersion)

    // OBLIGATION 4, in the journal's own write order so it is checked against
    // the cursor every earlier write left. The server's log is behind the
    // cursor: a restore, or the wrong server. The epoch moves as the wipe is
    // decided, so a page requested from the old cursor is dropped rather than
    // landing on the empty store with none of the messages below its high water.
    this.enqueue(async () => {
      const cursor = this.journal.cursor()
      if (cursor === null || r.logSeq >= cursor) return
      this.stats.discards++
      if (this.faults.skipWipe) {
        this.catchup.restartFromZero()
        return
      }
      this.epoch++
      const a = await this.journal.wipe()
      this.wipes++
      this.emitApply(a)
    })
    // Every `ready` is a trigger: `resumed: true` is treated as false, which
    // is always correct.
    this.catchup.trigger()
    this.heartbeat.start(r.heartbeatIntervalSec, r.missedPongLimit)
    this.log.info('ready', {
      session_id: r.sessionId,
      head: r.logSeq,
      heartbeat_interval_sec: r.heartbeatIntervalSec,
      missed_pong_limit: r.missedPongLimit,
    })
  }

  /** A `message` frame (obligation 2, CANT-103 rules 1 and 4): applied by id,
   *  moving no cursor — unless it names a conversation or an author the
   *  journal does not hold, when it is discarded, never buffered and never a
   *  placeholder, and pulls a catch-up that is guaranteed to return it. */
  private onLiveMessage(m: Message): void {
    this.enqueue(async () => {
      if (!this.journal.holdsConversation(m.conversationId) || !this.journal.holdsUser(m.authorId)) {
        this.stats.introductionDiscards++
        this.catchup.trigger()
        return
      }
      const a = await this.journal.applyLive({ messages: [m] }, this.faults)
      this.stats.liveFrames++
      this.emitApply(a)
    })
  }

  /**
   * A live `receipt`. RULING 4 → B: one naming this person pulls a catch-up,
   * and `first_unread_seq` moves only when a page lands. One naming anybody
   * else is a no-op for the store: `read_by` arrives on the re-emitted
   * `message` frames (CANT-92). Nothing is inferred from the ABSENCE of a
   * receipt — a device on another instance learns a mark only at `/sync`.
   */
  private onReceipt(r: ServerReceipt): void {
    if (this.selfUserId !== null && r.userId === this.selfUserId) this.catchup.trigger()
    this.enqueue(async () =>
      this.emitApply({
        source: 'live', cursor: this.journal.cursor(), messages: [], conversations: [], users: [],
        receipts: [r], wiped: false,
      }),
    )
  }

  private write(frame: Record<string, unknown>): void {
    const conn = this.conn
    if (!conn) return
    try {
      conn.send(JSON.stringify(frame))
    } catch {
      // The close event that follows is where this session ends.
    }
  }

  // --- /sync ---------------------------------------------------------------------

  private async fetchPage(after: number, limit: number | undefined): Promise<SyncResponse> {
    const used = await this.cred.current()
    let page = await this.syncOnce(after, limit, used)
    if (page === null) {
      // Catenary's own 401: the reactive refresh, then ONE retry — not a loop.
      if (await this.cred.onSyncUnauthorized(used)) page = await this.syncOnce(after, limit, await this.cred.current())
      if (page === null) throw new Error('sync: 401 unauthorized')
    }
    return page
  }

  /** One `GET /sync`; null for Catenary's own 401. */
  private async syncOnce(after: number, limit: number | undefined, cred: Credential): Promise<SyncResponse | null> {
    if (this.connecting) {
      this.stats.syncsBeforeReady++
      this.notify()
    }
    const q = new URLSearchParams({ after: String(after) })
    if (limit !== undefined && limit > 0) q.set('limit', String(limit))
    const ctl = new AbortController()
    const outer = this.abort
    const onAbort = () => ctl.abort()
    outer?.signal.addEventListener('abort', onAbort)
    const timer = this.timers.setTimeout(() => ctl.abort(), SYNC_TIMEOUT_MS)
    try {
      const res = await this.fetchFn(`${this.httpBase}/sync?${q}`, {
        headers: { Authorization: `Bearer ${cred.accessToken}` },
        signal: ctl.signal,
      })
      const body = await res.text()
      if (isCatenaryUnauthorized(res.status, body)) {
        // CATENARY'S OWN 401 IS CATENARY ANSWERING. A hop's 401 carries no
        // such body and falls through to the status branch, answering nothing.
        this.cred.answered()
        return null
      }
      if (res.status !== 200) throw new Error(`sync: HTTP ${res.status}`)
      const page = decodeSyncResponse(JSON.parse(body))
      // A PAGE THE GENERATED DECODER ACCEPTED, not merely a 200: a captive
      // portal's interception page is a 200 and is not an answer from Catenary.
      this.cred.answered()
      return page
    } finally {
      this.timers.clearTimeout(timer)
      outer?.signal.removeEventListener('abort', onAbort)
    }
  }

  // --- the journal's write queue -------------------------------------------------

  private enqueue(op: () => Promise<void>): Promise<void> {
    this.pendingWrites++
    const p = this.writes
      .then(op)
      .catch((e) => this.log.warn('journal write failed', { error: e instanceof Error ? e.message : String(e) }))
      .finally(() => {
        this.pendingWrites--
        this.notify()
      })
    this.writes = p
    return p
  }

  private async settled(): Promise<{ cursor: number | null; epoch: number }> {
    while (this.pendingWrites > 0) await this.writes
    return { cursor: this.journal.cursor(), epoch: this.epoch }
  }

  private async applyPage(page: SyncResponse, epoch: number): Promise<boolean> {
    let applied = false
    await this.enqueue(async () => {
      if (epoch !== this.epoch) return
      this.emitApply(await this.journal.applyPage(page, this.faults))
      applied = true
    })
    return applied
  }

  /** Only ever called after the write it describes has landed. */
  private emitApply(a: Applied): void {
    for (const fn of [...this.applyFns]) {
      try {
        fn(a)
      } catch (err) {
        this.log.warn('an apply listener threw', { error: String(err) })
      }
    }
  }

  // --- lifecycle -----------------------------------------------------------------

  private onLifecycle(e: LifecycleEvent): void {
    switch (e) {
      case 'online':
      case 'visible':
      case 'pageshow':
        this.policyCatchUp()
        void this.wake(e)
        break
      case 'resume':
        void this.wake(e)
        break
      default:
        // offline, hidden, pagehide, freeze: nothing to do. `offline` reaches
        // the banner through `navigator.onLine` (connectionInfo), and a frozen
        // page's socket is found dead by the wake detector when it thaws.
        break
    }
  }

  /**
   * RULING 5 → B: visible, online and pageshow each pull a trigger, at most
   * once per `heartbeat_interval_sec` learned from the most recent `ready`.
   * INERT BEFORE THE FIRST `ready`, and whenever no session is ready: the dial
   * pulls its own trigger, so there is nothing for this to add. Withheld while
   * a token is refused, like every `/sync` source.
   */
  private policyCatchUp(): void {
    const interval = this.learnedIntervalSec
    if (!this.started || !this.ready || interval === null) return
    const now = this.now()
    if (this.lastPolicyAt !== null && now - this.lastPolicyAt < interval * 1000) return
    if (this.withheldSync()) return
    this.lastPolicyAt = now
    this.catchup.trigger()
  }

  /**
   * A wake signal: `online`, visible, `pageshow`, `resume`, or the heartbeat's
   * wall-clock jump. The explicit §1 refresh check first (CANT-152's half,
   * never held); then an immediate ping when a socket is open, so a half-dead
   * one is found at the next tick; or, when none is, ONE EARLY DIAL. The early
   * dial does not reset the ramp, is limited to one per learned
   * `heartbeat_interval_sec`, is not made before any `ready` (the backoff alone
   * governs, and its worst cost is one ceiling wait), never shortens an
   * `error{internal}` maximum wait, and is withheld while a token is refused.
   * NO WAKE SIGNAL ENDS A TERMINAL STATE.
   */
  private async wake(_source: LifecycleEvent | 'clock'): Promise<void> {
    if (!this.started || this.terminal.kind !== 'none') return
    const run = this.runId
    await this.cred.refreshIfDue()
    if (!this.alive(run)) return
    if (this.conn) {
      this.heartbeat.pingNow()
      return
    }
    // CANT-129 FIRST: a wake signal is a dial source, and while the token is
    // refused it is withheld and COUNTED, whichever other rule would also have
    // stood it down — the refused wait's own poll included, which waits at the
    // maximum and is never shortened by a wake signal.
    if (this.cred.refused()) {
      this.stats.dialsWithheld++
      this.notify()
      return
    }
    const w = this.dialWait
    const interval = this.learnedIntervalSec
    if (!w || interval === null || w.atMax) return
    const now = this.now()
    if (this.lastEarlyDialAt !== null && now - this.lastEarlyDialAt < interval * 1000) {
      this.stats.dialsWithheld++
      this.notify()
      return
    }
    this.lastEarlyDialAt = now
    w.resolve('early')
  }

  private withheldSync(): boolean {
    if (!this.cred.refused()) return false
    this.stats.syncsWithheld++
    this.notify()
    return true
  }

  // --- terminal, stop, notify -------------------------------------------------------

  /** The first terminal wins. Nothing is deleted: not the credential, not the
   *  journal. */
  private enterTerminal(t: Terminal): void {
    if (this.terminal.kind !== 'none' || t.kind === 'none') return
    this.terminal = { kind: t.kind, reason: t.reason }
    this.log.warn('terminal: this client will not reconnect', { kind: t.kind, reason: t.reason })
    this.halt()
    this.notify()
  }

  private halt(): void {
    this.started = false
    this.runId++
    this.abort?.abort()
    this.abort = null
    this.unlisten?.()
    this.unlisten = null
    this.dialWait?.resolve('stopped')
    this.heartbeat.stop()
    const s = this.session
    if (s) {
      try {
        s.ws.close(1000, 'bye')
      } catch {
        // Already closing.
      }
      this.endSession(s, 1000, true)
    }
    this.catchup.wake()
  }

  private notify(): void {
    if (this.statusFns.size === 0) return
    const s = this.status()
    for (const fn of [...this.statusFns]) {
      try {
        fn(s)
      } catch (err) {
        this.log.warn('a status listener threw', { error: String(err) })
      }
    }
  }
}
