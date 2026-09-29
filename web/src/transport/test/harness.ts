/* The fakes the transport's unit tests run against: a clock and its timers,
 * a socket, a `/sync` server, a lifecycle, a log. Mirrors the fake servers of
 * internal/client's tests (client_test.go, terminal_test.go), except that the
 * TypeScript tests produce every close from the fake directly rather than by
 * making a real server produce it.
 *
 * Every frame a fake sends is wire JSON built by the generated ENCODERS, and
 * every frame the transport writes is read back through the generated
 * DECODERS, so the tests exercise the same codecs the app does.
 */

import { decodeClientFrame, encodeServerFrame, encodeSyncResponse } from '@/wire/generated'
import type { ClientFrame, Conversation, Message, ServerFrame, SyncResponse, User } from '@/wire/generated'
import { heldCredential, type Credential } from '../credential'
import { MemoryJournal, type StagedJournal } from '../journal'
import { manualLifecycle, type Logger, type Timers, type WebSocketLike } from '../seams'
import { createTransport, type Transport, type TransportConfig } from '../transport'
import type { Faults } from '../faults'

/** Drains every pending microtask (and anything they chain). */
export const flush = async (rounds = 3): Promise<void> => {
  for (let i = 0; i < rounds; i++) await new Promise<void>((r) => setImmediate(r))
}

/** A clock whose timers run only when told to. `now()` is the wall clock. */
export class FakeClock implements Timers {
  t = Date.UTC(2026, 8, 29, 12, 0, 0)
  private seq = 0
  private readonly timers = new Map<number, { due: number; fn: () => void }>()

  now = (): number => this.t

  setTimeout = (fn: () => void, ms: number): unknown => {
    const id = ++this.seq
    this.timers.set(id, { due: this.t + Math.max(0, ms), fn })
    return id
  }

  clearTimeout = (h: unknown): void => {
    this.timers.delete(h as number)
  }

  pending(): number {
    return this.timers.size
  }

  /** Moves the clock `ms` forward, running every timer that falls due in
   *  order, with microtasks drained after each. */
  async advance(ms: number): Promise<void> {
    const target = this.t + ms
    await flush()
    for (;;) {
      let next: [number, { due: number; fn: () => void }] | null = null
      for (const e of this.timers) if (e[1].due <= target && (next === null || e[1].due < next[1].due)) next = e
      if (next === null) break
      this.timers.delete(next[0])
      this.t = Math.max(this.t, next[1].due)
      next[1].fn()
      await flush()
    }
    this.t = target
    await flush()
  }

  /** The wall clock jumps — a lid closed and opened — with no timer running. */
  jump(ms: number): void {
    this.t += ms
  }
}

export class FakeSocket implements WebSocketLike {
  onopen: ((ev: unknown) => void) | null = null
  onmessage: ((ev: { data: unknown }) => void) | null = null
  onclose: ((ev: { code: number; reason?: string }) => void) | null = null
  onerror: ((ev: unknown) => void) | null = null
  readonly written: string[] = []
  closedByClient: { code?: number } | null = null
  isOpen = false

  constructor(readonly url: string, readonly protocols: string[]) {}

  send(data: string): void {
    if (!this.isOpen) throw new Error('InvalidStateError: not open')
    this.written.push(data)
  }

  close(code?: number): void {
    this.closedByClient = { code }
    this.isOpen = false
  }

  /** What the client wrote, through the generated decoder. */
  frames(): ClientFrame[] {
    return this.written.map((w) => decodeClientFrame(JSON.parse(w))!).filter((f) => f !== null)
  }

  open(): void {
    this.isOpen = true
    this.onopen?.({})
  }

  /** A server frame, encoded by the generated encoder. */
  frame(f: ServerFrame): void {
    this.raw(JSON.stringify(encodeServerFrame(f)))
  }

  raw(data: unknown): void {
    this.onmessage?.({ data })
  }

  /** The server closes with `code`; 1006 is an abnormal closure. */
  serverClose(code: number): void {
    this.isOpen = false
    this.onclose?.({ code })
  }

  /** A dial that never opens: error, then close 1006. */
  fail(): void {
    this.onerror?.({})
    this.onclose?.({ code: 1006 })
  }
}

export class FakeNet {
  readonly sockets: FakeSocket[] = []
  /** Runs on each new socket; the default leaves it to the test. */
  onDial: (s: FakeSocket) => void = () => {}

  readonly WebSocket = (() => {
    const net = this
    return class extends FakeSocket {
      constructor(url: string, protocols: string[]) {
        super(url, protocols)
        net.sockets.push(this)
        queueMicrotask(() => net.onDial(this))
      }
    }
  })()

  get last(): FakeSocket {
    return this.sockets[this.sockets.length - 1]
  }
}

export interface SyncRequest {
  after: number
  limit: string | null
  authorization: string | null
}

type SyncAnswer = SyncResponse | { status: number; body: string } | Error | Promise<SyncAnswer>

/** A `/sync` endpoint. `answer` decides each response; `requests` records them. */
export class FakeSync {
  readonly requests: SyncRequest[] = []
  answer: (req: SyncRequest) => SyncAnswer = (req) => page({ logSeq: req.after })

  readonly fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input))
    const headers = new Headers(init?.headers)
    const req: SyncRequest = {
      after: Number(url.searchParams.get('after')),
      limit: url.searchParams.get('limit'),
      authorization: headers.get('Authorization'),
    }
    this.requests.push(req)
    const a = await this.answer(req)
    if (a instanceof Error) throw a
    if ('status' in a && 'body' in a) return fakeResponse(a.status, a.body)
    return fakeResponse(200, JSON.stringify(encodeSyncResponse(a as SyncResponse)))
  }) as unknown as typeof globalThis.fetch
}

function fakeResponse(status: number, body: string) {
  return { status, ok: status >= 200 && status < 300, text: async () => body } as unknown as Response
}

/** A promise a test resolves by hand. */
export function deferred<T>(): { promise: Promise<T>; resolve(v: T): void; reject(e: unknown): void } {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((a, b) => {
    resolve = a
    reject = b
  })
  return { promise, resolve, reject }
}

// --- wire fixtures ----------------------------------------------------------

export const uuid = (n: number): string => `00000000-0000-4000-8000-${n.toString(16).padStart(12, '0')}`
export const AT = '2026-09-29T12:00:00.000Z'

export const ME = uuid(1)
export const OTHER = uuid(2)
export const DEVICE = uuid(3)
export const CONV = uuid(100)
export const TOKEN = 'SECRET-access-token-that-must-never-be-logged-0001'

export function user(id: string, name = `User ${id.slice(-4)}`): User {
  return { id, name }
}

export function conversation(id: string, extra: Partial<Conversation> = {}): Conversation {
  return { id, kind: 'group', name: `Room ${id.slice(-4)}`, memberCount: 2, headSeq: 0, ...extra }
}

export function message(n: number, extra: Partial<Message> = {}): Message {
  return {
    id: uuid(1000 + n), seq: n, logSeq: n, conversationId: CONV, authorId: OTHER, at: AT, state: 'sent',
    text: `message ${n}`, ...extra,
  }
}

export function page(p: Partial<SyncResponse> & { logSeq: number }): SyncResponse {
  return { messages: [], conversations: [], users: [], hasMore: false, serverTime: AT, ...p }
}

/** A first page that introduces CONV, ME and OTHER. */
export function bootstrapPage(logSeq = 0, messages: Message[] = []): SyncResponse {
  return page({ logSeq, messages, conversations: [conversation(CONV)], users: [user(ME), user(OTHER)] })
}

export function ready(extra: Partial<Extract<ServerFrame, { type: 'ready' }>> = {}): ServerFrame {
  return {
    type: 'ready', sessionId: uuid(9000), serverTime: AT, heartbeatIntervalSec: 35, missedPongLimit: 2,
    // A head above anything a test holds: a `ready` below the cursor is
    // obligation 4's case, and a test asks for it by name.
    logSeq: 1000, resumed: false, ...extra,
  }
}

export function messageFrame(m: Message): ServerFrame {
  return { type: 'message', message: m }
}

// --- the rig ------------------------------------------------------------------

export interface LogLine {
  level: 'info' | 'warn'
  msg: string
  fields?: Record<string, unknown>
}

export interface Rig {
  t: Transport
  clock: FakeClock
  net: FakeNet
  sync: FakeSync
  lifecycle: ReturnType<typeof manualLifecycle>
  journal: StagedJournal
  logs: LogLine[]
  credential: Credential
  /** Opens the newest socket and answers its hello with `ready`. */
  connect(r?: ServerFrame): Promise<FakeSocket>
}

export interface RigOptions {
  faults?: Partial<Faults>
  journal?: StagedJournal
  random?: (n: number) => Uint8Array
  backoffMinMs?: number
  backoffMaxMs?: number
  syncLimit?: number
  config?: Partial<TransportConfig>
}

/** The minimum jitter draw: the worst case for dial counts. */
export const minDraw = (n: number) => new Uint8Array(n)
/** The maximum jitter draw. */
export const maxDraw = (n: number) => new Uint8Array(n).fill(0xff)

export function rig(opts: RigOptions = {}): Rig {
  const clock = new FakeClock()
  const net = new FakeNet()
  const sync = new FakeSync()
  const lifecycle = manualLifecycle()
  const journal = opts.journal ?? new MemoryJournal()
  const logs: LogLine[] = []
  const logger: Logger = {
    info: (msg, fields) => logs.push({ level: 'info', msg, fields }),
    warn: (msg, fields) => logs.push({ level: 'warn', msg, fields }),
  }
  const credential: Credential = { userId: ME, deviceId: DEVICE, accessToken: TOKEN }
  const t = createTransport({
    baseUrl: 'http://catenary.test',
    credential: heldCredential(credential),
    journal,
    clientVersion: '0.0.0-test',
    WebSocket: net.WebSocket,
    fetch: sync.fetch,
    now: clock.now,
    timers: clock,
    random: opts.random ?? minDraw,
    lifecycle,
    logger,
    faults: opts.faults,
    backoffMinMs: opts.backoffMinMs,
    backoffMaxMs: opts.backoffMaxMs,
    syncLimit: opts.syncLimit,
    ...opts.config,
  })
  return {
    t, clock, net, sync, lifecycle, journal, logs, credential,
    async connect(r = ready()) {
      await flush()
      const s = net.last
      s.open()
      s.frame(r)
      await flush()
      return s
    },
  }
}

/** A relaunch: a new transport over the same journal and credential. */
export function createAgain(r: Rig, opts: RigOptions = {}): Rig {
  return rig({ ...opts, journal: r.journal })
}
