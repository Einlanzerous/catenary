/* TypeScript decision-vector runner — CANT-156 (CANT-35 ruling 6 → A).
 *
 * Reads internal/client/testdata/decisions.json and holds the shipped transport
 * to it. The Go runner (internal/client/decisions_test.go, TestTheDecisionVectors)
 * reads the SAME file and asserts the SAME answers against the reference client;
 * CANT-42's Dart runner will be the third. The file's note is the contract.
 *
 * CANT-31's rules are implemented three times, and two of them disagreeing is a
 * device that stops when it should reconnect, or one that keeps minting refresh
 * links on a dead network. Neither is a type error, so codegen cannot catch it.
 *
 * THIS RUNNER IMPLEMENTS NO RULE OF ITS OWN. Every kind dispatches to exactly one
 * export of `@/transport`, and `preceding` goes through the generated decoder.
 * Its only translations are an instant to epoch milliseconds (Date.parse) and a
 * null `access_expires_at` to NaN, which is how `StoredCredential` spells "no
 * expiry". What else is here — binding `$P<n>`, writing a scripted response,
 * reading the store back — is the harness, not a decision.
 *
 * The chain transcripts run three times: clean, where every case must pass, and
 * under `faults.noChain` and `faults.proposeAfresh`, where at least one case
 * must FAIL. A runner that cannot fail proves nothing. So, for CANT-177's close
 * case, must a `preceding` decoded strictly and a classifyClose that stops on a
 * code it does not know.
 */

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { decodeRefreshRequest, decodeServerFrame, ErrorCodeValues, setOnUnknownWireValue, type ServerError } from '@/wire/generated'
import {
  advance,
  classifyClose,
  createRefreshingCredential,
  gateOpen,
  inProcessLock,
  jitteredWait,
  MemoryCredentialStore,
  readStamp,
  refreshDelay,
  refreshDue,
  refreshHoldAt,
  refreshThreshold,
  refusedHoldAt,
  resetsRamp,
  silentLogger,
  unitFromBytes,
  type ChainLink,
  type Faults,
  type StoredCredential,
  type Terminal,
} from '@/transport'

/* Resolved from the package directory, as conformance.ts does: this file is
 * bundled into dist-decisions/ before it runs, and npm runs scripts from the
 * package root. */
const VECTORS = resolve(process.cwd(), '..', 'internal', 'client', 'testdata', 'decisions.json')

const KINDS = [
  'close', 'threshold', 'due', 'delay', 'stamp', 'gate', 'hold', 'refused_hold', 'chain',
  'backoff_reset', 'backoff_draw', 'backoff_jitter', 'backoff_advance',
] as const

interface Case {
  name: string
  kind: string
  why: string
  in: Record<string, unknown>
  want: Record<string, unknown>
}

type Json = Record<string, unknown>

/** A vector field the runner does not know is an error: a misspelled key would
 *  otherwise read as absent and pass. */
function only(o: Json, where: string, keys: readonly string[]): void {
  for (const k of Object.keys(o)) if (!keys.includes(k)) throw new Error(`${where}: unknown field ${k}`)
}

/** A wire timestamp to epoch ms; null is absent. */
function instant(v: unknown): number | null {
  if (v === null || v === undefined) return null
  const t = Date.parse(String(v))
  if (!Number.isFinite(t)) throw new Error(`not an instant: ${JSON.stringify(v)}`)
  return t
}

/** `access_expires_at`: null is NO EXPIRY, which a StoredCredential spells NaN. */
function expiry(v: unknown): number {
  return v === null ? NaN : (instant(v) as number)
}

function required(o: Json, k: string): unknown {
  if (!(k in o)) throw new Error(`missing field ${k}`)
  return o[k]
}

function same(what: string, got: unknown, want: unknown): string | null {
  return got === want ? null : `${what} ${JSON.stringify(got)}, want ${JSON.stringify(want)}`
}

function preceding(v: unknown): ServerError | null {
  if (v === null || v === undefined) return null
  const f = decodeServerFrame(v)
  if (f === null || f.type !== 'error') throw new Error(`preceding: ${f === null ? 'an unknown tag' : f.type}, want an error frame`)
  if (f.clientId !== undefined) {
    throw new Error("preceding: an error naming a client_id is that send's answer, never the session's (a malformed vector)")
  }
  return f
}

/** The close kind's two functions, which a teeth check replaces one at a time. */
interface CloseFns {
  preceding: typeof preceding
  classify: typeof classifyClose
}

const shippedClose: CloseFns = { preceding, classify: classifyClose }

/** One pure case: null when it agrees, else what disagreed. */
function pure(c: Case, fns: CloseFns = shippedClose): string | null {
  const i = c.in
  const w = c.want
  switch (c.kind) {
    case 'close':
      only(i, 'in', ['status', 'preceding'])
      only(w, 'want', ['verdict'])
      return same(
        'verdict',
        fns.classify(required(i, 'status') as number | null, fns.preceding(required(i, 'preceding'))).verdict,
        required(w, 'verdict'),
      )
    case 'threshold':
      only(i, 'in', ['access_issued_at', 'access_expires_at'])
      only(w, 'want', ['threshold_ms'])
      return same(
        'threshold_ms',
        refreshThreshold({ accessIssuedAt: instant(required(i, 'access_issued_at')), accessExpiresAt: expiry(required(i, 'access_expires_at')) }),
        required(w, 'threshold_ms'),
      )
    case 'due':
      only(i, 'in', ['access_issued_at', 'access_expires_at', 'clock_offset_ms', 'device_now'])
      only(w, 'want', ['due'])
      return same(
        'due',
        refreshDue(
          {
            accessIssuedAt: instant(required(i, 'access_issued_at')),
            accessExpiresAt: expiry(required(i, 'access_expires_at')),
            clockOffsetMs: required(i, 'clock_offset_ms') as number,
          },
          instant(required(i, 'device_now')) as number,
        ),
        required(w, 'due'),
      )
    case 'delay':
      only(i, 'in', ['links'])
      only(w, 'want', ['delay_ms'])
      return same('delay_ms', refreshDelay(required(i, 'links') as number), required(w, 'delay_ms'))
    case 'stamp':
      only(i, 'in', ['last_sent', 'now'])
      only(w, 'want', ['stamp'])
      return same(
        'stamp',
        readStamp(instant(required(i, 'last_sent')), instant(required(i, 'now')) as number),
        instant(required(w, 'stamp')),
      )
    case 'gate':
      only(i, 'in', ['answered_at', 'stamp'])
      only(w, 'want', ['open'])
      return same('open', gateOpen(instant(required(i, 'answered_at')), instant(required(i, 'stamp'))), required(w, 'open'))
    case 'hold':
      only(i, 'in', ['links', 'answered_at', 'last_sent', 'now'])
      only(w, 'want', ['hold'])
      return same(
        'hold',
        refreshHoldAt({
          links: required(i, 'links') as number,
          answeredAt: instant(required(i, 'answered_at')),
          lastSentAt: instant(required(i, 'last_sent')),
          now: instant(required(i, 'now')) as number,
        }),
        required(w, 'hold'),
      )
    case 'refused_hold':
      only(i, 'in', ['refused', 'links', 'last_sent', 'now'])
      only(w, 'want', ['hold'])
      return same(
        'hold',
        refusedHoldAt({
          refused: required(i, 'refused') as boolean,
          links: required(i, 'links') as number,
          lastSentAt: instant(required(i, 'last_sent')),
          now: instant(required(i, 'now')) as number,
        }),
        required(w, 'hold'),
      )
    // The dial backoff's pure pieces (CANT-170).
    case 'backoff_reset':
      only(i, 'in', ['readied_for_ms', 'heartbeat_interval_sec'])
      only(w, 'want', ['resets'])
      return same(
        'resets',
        resetsRamp(required(i, 'readied_for_ms') as number | null, required(i, 'heartbeat_interval_sec') as number | null),
        required(w, 'resets'),
      )
    case 'backoff_draw': {
      only(i, 'in', ['bytes'])
      only(w, 'want', ['unit'])
      const hex = String(required(i, 'bytes'))
      if (!/^[0-9a-f]{8}$/.test(hex)) throw new Error(`in.bytes ${JSON.stringify(hex)} is not four bytes of hex`)
      return same('unit', unitFromBytes(Uint8Array.from(Buffer.from(hex, 'hex'))), required(w, 'unit'))
    }
    case 'backoff_jitter':
      only(i, 'in', ['nominal_ms', 'unit'])
      only(w, 'want', ['wait_ms'])
      return same('wait_ms', jitteredWait(required(i, 'nominal_ms') as number, required(i, 'unit') as number), required(w, 'wait_ms'))
    case 'backoff_advance':
      only(i, 'in', ['nominal_ms', 'max_ms'])
      only(w, 'want', ['next_ms'])
      return same('next_ms', advance(required(i, 'nominal_ms') as number, required(i, 'max_ms') as number), required(w, 'next_ms'))
  }
  return `unknown kind ${c.kind}`
}

// --- the chain transcripts ----------------------------------------------------------

interface Respond {
  status?: number
  json?: unknown
  text?: string
  headers?: Record<string, string>
  drop?: boolean
}
interface Step {
  expect: ChainLink
  respond: Respond
}

/** `$P<n>` binds ON FIRST APPEARANCE to what the client presented, and every
 *  later appearance must equal it. Anything else is a literal fixture. */
class Symbols {
  private readonly bound = new Map<string, string>()

  match(what: string, want: string, got: string): string | null {
    if (!want.startsWith('$')) return got === want ? null : `${what} ${JSON.stringify(got)}, want ${JSON.stringify(want)}`
    const b = this.bound.get(want)
    if (b !== undefined && b !== got) return `${what} ${JSON.stringify(got)}, want ${want} = ${JSON.stringify(b)}`
    this.bound.set(want, got)
    return null
  }

  /** A symbol in a response or in `want`: nothing the client has not presented
   *  can be answered or held. */
  resolve(v: string): string {
    if (!v.startsWith('$')) return v
    const b = this.bound.get(v)
    if (b === undefined) throw new Error(`symbol ${v} is used before the client presented it`)
    return b
  }

  resolveJson(v: unknown): unknown {
    if (typeof v === 'string') return this.resolve(v)
    if (Array.isArray(v)) return v.map((x) => this.resolveJson(x))
    if (v && typeof v === 'object') {
      const out: Json = {}
      for (const [k, x] of Object.entries(v)) out[k] = this.resolveJson(x)
      return out
    }
    return v
  }
}

const DEVICE = '00000000-0000-4000-8000-000000000156'
const USER = '00000000-0000-4000-8000-000000000035'

async function chain(c: Case, faults: Partial<Faults>): Promise<string | null> {
  const i = c.in
  const w = c.want
  only(i, 'in', ['now', 'start', 'calls'])
  only(w, 'want', ['refresh_token', 'chain', 'stamped', 'terminal'])
  const now = instant(required(i, 'now')) as number
  const start = required(i, 'start') as Json
  only(start, 'in.start', ['refresh_token', 'chain', 'last_sent'])
  const calls = required(i, 'calls') as { steps: Step[] }[]
  const startChain = required(start, 'chain') as ChainLink[]
  if (startChain.length > 0 && startChain[0].token !== start.refresh_token) {
    throw new Error("start.chain[0].token must be start.refresh_token (the store's invariant)")
  }

  // A PAIR EXPIRED AN HOUR BEFORE `now`, so the refresh is due whatever the
  // floor, on a clock that does not move.
  const seed: StoredCredential = {
    userId: USER,
    deviceId: DEVICE,
    accessToken: 'access_token_FIXTURE_before_rotation_______',
    accessExpiresAt: now - 3_600_000,
    refreshToken: required(start, 'refresh_token') as string,
    refreshExpiresAt: now + 86_400_000,
    accessIssuedAt: null,
    clockOffsetMs: 0,
    chain: startChain.map((l) => ({ token: l.token, proposal: l.proposal })),
    lastSentAt: instant(required(start, 'last_sent')),
  }
  const store = new MemoryCredentialStore(seed)
  const sym = new Symbols()
  let steps: Step[] = []
  let next = 0
  let mismatch: string | null = null

  /* The scripted /refresh (ruling 1 → A): no model of a server, only the next
   * step's check and its bytes. The first disagreement is kept, and every
   * request after it gets a 500 — an unknown outcome, which ends the attempt. */
  const fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const fail = (why: string): Response => {
      mismatch ??= why
      return new Response('decision vector mismatch', { status: 500 })
    }
    if (mismatch !== null) return fail(mismatch)
    const path = new URL(String(input)).pathname
    if (path !== '/refresh') return fail(`a request to ${path}; only /refresh is scripted`)
    if (next >= steps.length) return fail(`request ${next + 1} arrived, and this call scripts ${steps.length}`)
    const n = next + 1
    const st = steps[next++]

    // THE GENERATED DECODER, so a proposal that is not a wire Token fails here.
    let token: string
    let proposal: string
    try {
      const req = decodeRefreshRequest(JSON.parse(String(init?.body)))
      if (req.proposedRefreshToken === undefined) return fail(`step ${n}: the request carries no proposal`)
      token = req.refreshToken
      proposal = req.proposedRefreshToken
    } catch (e) {
      return fail(`step ${n}: the request does not decode as a wire RefreshRequest: ${e instanceof Error ? e.message : String(e)}`)
    }
    const bad = sym.match('presented token', st.expect.token, token) ?? sym.match('proposal', st.expect.proposal, proposal)
    if (bad) return fail(`step ${n}: ${bad}`)
    // PERSISTED BEFORE SENT, at every request: the link and its stamp are in the
    // store before the request arrives.
    const held = await store.read()
    if (!held?.chain.some((l) => l.token === token && l.proposal === proposal)) {
      return fail(`step ${n}: the store does not hold the link being presented; it holds ${JSON.stringify(held?.chain)}`)
    }
    if (held.lastSentAt === null) return fail(`step ${n}: the store holds no last_sent stamp for the request in flight`)

    const r = st.respond
    for (const [k, v] of Object.entries(r.headers ?? {})) {
      if (k !== 'Retry-After' || v !== '0') return fail(`step ${n}: the only header a vector may set is Retry-After: 0`)
    }
    if (r.drop) {
      if (r.json !== undefined || r.text !== undefined || r.status !== undefined || r.headers !== undefined) {
        return fail(`step ${n}: a drop carries nothing else`)
      }
      throw new TypeError('fetch failed') // the connection closed with no response
    }
    if ((r.json === undefined) === (r.text === undefined)) return fail(`step ${n}: respond is exactly one of json, text or drop`)
    let body: string
    try {
      body = r.json !== undefined ? JSON.stringify(sym.resolveJson(r.json)) : (r.text as string)
    } catch (e) {
      return fail(`step ${n}: respond: ${e instanceof Error ? e.message : String(e)}`)
    }
    const headers = { 'Content-Type': r.json !== undefined ? 'application/json' : 'text/plain; charset=utf-8', ...r.headers }
    return new Response(body, { status: r.status, headers })
  }

  const terminals: Terminal[] = []
  const cred = createRefreshingCredential({
    baseUrl: 'http://decisions.invalid',
    store,
    lock: inProcessLock(),
    fetch: fetch as typeof globalThis.fetch,
    now: () => now,
    logger: silentLogger,
    faults,
  })
  cred.attach({ terminal: (t) => terminals.push(t), notify: () => {} })

  for (const [k, call] of calls.entries()) {
    steps = call.steps
    next = 0
    await cred.refreshIfDue()
    if (mismatch !== null) return `call ${k + 1}: ${mismatch}`
    if (next !== steps.length) return `call ${k + 1}: ${next} requests arrived, and it scripts ${steps.length}`
  }

  // THE END STATE, READ FROM STATE: no counter is asserted.
  const rec = await store.read()
  if (!rec) return 'the store holds no credential'
  const wantToken = sym.resolve(required(w, 'refresh_token') as string)
  if (rec.refreshToken !== wantToken) return `the store holds refresh token ${JSON.stringify(rec.refreshToken)}, want ${JSON.stringify(wantToken)}`
  const wantChain = (required(w, 'chain') as ChainLink[]).map((l) => ({ token: sym.resolve(l.token), proposal: sym.resolve(l.proposal) }))
  if (JSON.stringify(rec.chain.map((l) => ({ token: l.token, proposal: l.proposal }))) !== JSON.stringify(wantChain)) {
    return `the chain is ${JSON.stringify(rec.chain)}, want ${JSON.stringify(wantChain)}`
  }
  const stamped = same('stamped', rec.lastSentAt !== null, required(w, 'stamped'))
  if (stamped) return stamped
  // CredentialStatus has no terminal field: the attached host is the only source.
  return same('credential terminal', terminals.some((t) => t.kind === 'credential'), required(w, 'terminal'))
}

async function run(c: Case, faults: Partial<Faults> = {}): Promise<string | null> {
  try {
    return c.kind === 'chain' ? await chain(c, faults) : pure(c)
  } catch (e) {
    return `threw: ${e instanceof Error ? e.message : String(e)}`
  }
}

// --- the run ------------------------------------------------------------------------

// A tolerated unknown enum value is a warning the generated decoder prints. One
// vector produces one on purpose — close_1008_after_an_error_code_a_later_server_adds
// (CANT-177) — and any other that did must not be lost in the noise.
setOnUnknownWireValue((m) => console.log(`warn  ${m}`))

const fail: string[] = []
const check = (name: string, ok: boolean, detail = '') => {
  if (!ok) fail.push(`${name}${detail ? ` — ${detail}` : ''}`)
  console.log(`${ok ? 'ok  ' : 'FAIL'}  ${name}${detail ? `  (${detail})` : ''}`)
}

// An async main rather than top-level await: the SSR bundle targets an
// environment without it, as conformance.ts's does.
async function main(): Promise<number> {
  const { cases } = JSON.parse(readFileSync(VECTORS, 'utf8')) as { cases: Case[] }
  const names = new Set<string>()
  for (const c of cases) {
    if (names.has(c.name)) check(c.name, false, 'two cases have this name')
    names.add(c.name)
    if (!c.why || !c.why.trim()) check(c.name, false, 'no why')
    const err = await run(c)
    check(c.name, err === null, err ?? '')
  }

  // Every listed kind has cases: a kind nobody wrote a vector for pins nothing.
  const RUNNER_CHECKS = KINDS.length + 4
  for (const k of KINDS) {
    const n = cases.filter((c) => c.kind === k).length
    check(`kind ${k} has cases`, n > 0, `${n}`)
  }

  // TEETH: the chain transcripts must catch a client that forgets its proposals,
  // and one that mints a new proposal where it must reuse the original.
  const chains = cases.filter((c) => c.kind === 'chain')
  for (const [name, faults] of [
    ['faults.noChain', { noChain: true }],
    ['faults.proposeAfresh', { proposeAfresh: true }],
  ] as const) {
    let caught = 0
    for (const c of chains) if ((await run(c, faults)) !== null) caught++
    check(`${name} fails at least one chain transcript`, caught > 0, `${caught} of ${chains.length}`)
  }

  // TEETH for CANT-177's case, the Go runner's two mutants: `preceding` decoded as
  // the server decodes it, which refuses an error code this wire version does not
  // define, and a classifyClose that stops on such a code. Each must fail that
  // case by name.
  const strictPreceding: typeof preceding = (v) => {
    const f = preceding(v)
    if (f !== null && !(ErrorCodeValues as readonly string[]).includes(f.code)) {
      throw new Error('preceding: the strict decoder refuses an error code this wire version does not define')
    }
    return f
  }
  const stopsOnUnknown: typeof classifyClose = (code, p) =>
    code === 1008 && p !== null && !(ErrorCodeValues as readonly string[]).includes(p.code)
      ? { verdict: 'terminal_protocol', reason: 'close 1008 after a code this client does not know' }
      : classifyClose(code, p)
  const laterCode = 'close_1008_after_an_error_code_a_later_server_adds'
  for (const [name, fns] of [
    ['preceding decoded by the server\'s strict decoder', { ...shippedClose, preceding: strictPreceding }],
    ['classifyClose stops on a code it does not know', { ...shippedClose, classify: stopsOnUnknown }],
  ] as const) {
    const c = cases.find((c) => c.name === laterCode)
    if (!c) {
      check(`${name} fails ${laterCode}`, false, 'no case has that name')
      continue
    }
    let err: string | null
    try {
      err = pure(c, fns)
    } catch (e) {
      err = `threw: ${e instanceof Error ? e.message : String(e)}`
    }
    check(`${name} fails ${laterCode}`, err !== null, err ?? 'it passed')
  }

  console.log(
    fail.length
      ? `\n${fail.length} of ${cases.length + RUNNER_CHECKS} FAILED`
      : `\nall green — ${cases.length} decision vectors + ${RUNNER_CHECKS} runner checks`,
  )
  return fail.length ? 1 : 0
}

void main().then((code) => process.exit(code))
