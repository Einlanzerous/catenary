/* CANT-35 criteria 8 and 26: CANT-31 §4's close table, *preceded by*, and the
 * protocol terminal. Mirrors internal/client/terminal_test.go. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { ServerError } from '@/wire/generated'
import { classifyClose, closeStatusKey } from '../closes'
import type { SessionEnd } from '../transport'
import { bootstrapPage, createAgain, flush, message, rig, uuid } from './harness'

const err = (code: ServerError['code'], extra: Partial<ServerError> = {}): ServerError => ({
  type: 'error', code, message: `a ${code}`, retryable: true, ...extra,
})

test('criterion 8 · the close table, row by row', () => {
  const cases: [string, number | null, ServerError | null, string][] = [
    ['4001', 4001, null, 'terminal_protocol'],
    ['4001 after an error', 4001, err('internal'), 'terminal_protocol'],
    ['bare 1008', 1008, null, 'terminal_protocol'],
    ['1008 after error{unauthorized}', 1008, err('unauthorized'), 'terminal_protocol'],
    ['1008 after error{wire_version_unsupported}', 1008, err('wire_version_unsupported'), 'terminal_protocol'],
    ['1008 after error{internal}, retryable', 1008, err('internal', { retryable: true }), 'reconnect_at_maximum'],
    ['1008 after error{internal}, not retryable', 1008, err('internal', { retryable: false }), 'reconnect_at_maximum'],
    ['1008 after error{rate_limited}', 1008, err('rate_limited'), 'reconnect'],
    ['1008 after error{not_a_member}', 1008, err('not_a_member'), 'reconnect'],
    ['1008 after an error code this build does not know', 1008, err('unknown'), 'reconnect'],
    ['1000', 1000, null, 'reconnect'],
    ['1001', 1001, null, 'reconnect'],
    ['1005 (no status)', 1005, null, 'reconnect'],
    ['1006 (abnormal)', 1006, null, 'reconnect'],
    ['1009', 1009, null, 'reconnect'],
    ['1011', 1011, null, 'reconnect'],
    ['1012', 1012, null, 'reconnect'],
    ['4000', 4000, null, 'reconnect'],
    ['4002', 4002, null, 'reconnect'],
    ['no close frame', null, null, 'reconnect'],
    ['a code a later server adds', 4999, null, 'reconnect'],
  ]
  for (const [name, code, preceding, want] of cases) {
    assert.equal(classifyClose(code, preceding).verdict, want, name)
  }
  assert.equal(closeStatusKey(1006), -1)
  assert.equal(closeStatusKey(1005), -1)
  assert.equal(closeStatusKey(null), -1)
  assert.equal(closeStatusKey(1001), 1001)
})

/** Drives one session to a close: `before` runs after `ready`, then the server
 *  closes with `code`. */
async function closeAfter(code: number, before: (s: Awaited<ReturnType<ReturnType<typeof rig>['connect']>>) => void) {
  const r = rig()
  r.sync.answer = () => bootstrapPage()
  const ends: SessionEnd[] = []
  r.t.onSessionEnd((e) => ends.push(e))
  r.t.start()
  const s = await r.connect()
  before(s)
  await flush()
  s.serverClose(code)
  await flush()
  return { r, end: ends[0], status: r.t.status() }
}

test('criterion 8 · 1008 after error{internal} reconnects at the maximum, not terminal', async () => {
  const { end, status } = await closeAfter(1008, (s) => s.frame(err('internal')))
  assert.equal(end.verdict, 'reconnect_at_maximum')
  assert.equal(end.bare1008, false)
  assert.equal(status.terminal.kind, 'none')
  // At the maximum: the drawn wait is at least 0.8 × 5 s.
  assert.ok(status.nextDialAt !== null && status.nextDialAt - Date.UTC(2026, 8, 29, 12, 0, 0) >= 4000)
})

test('criterion 8 · an error naming a send does not precede the close', async () => {
  const { end } = await closeAfter(1008, (s) => s.frame(err('rate_limited', { clientId: uuid(77) })))
  assert.equal(end.bare1008, true)
  assert.equal(end.verdict, 'terminal_protocol')
})

test('criterion 8 · every kind of message event clears preceded-by before decoding', async () => {
  const events: [string, unknown, boolean][] = [
    ['a frame the decoder returns null for', JSON.stringify({ type: 'a_frame_from_the_future' }), false],
    ['a frame the decoder throws WireFormatError on', JSON.stringify({ type: 'ready' }), true],
    ['text JSON.parse throws SyntaxError on', '{"type": "error", "code": ', true],
    ['a binary message as a Blob', new Blob(['{}']), true],
    ['a binary message as an ArrayBuffer', new ArrayBuffer(8), true],
  ]
  for (const [name, data, undecodable] of events) {
    const { end, status } = await closeAfter(1008, (s) => {
      s.frame(err('internal'))
      s.raw(data)
    })
    assert.equal(end.preceding, null, `${name}: preceded-by is cleared`)
    assert.equal(end.bare1008, true, `${name}: the close reads as a BARE 1008`)
    assert.equal(end.verdict, 'terminal_protocol', `${name}: which is terminal`)
    assert.equal(status.stats.undecodable, undecodable ? 1 : 0, `${name}: undecodable`)
  }
})

test('criterion 26 · a protocol terminal ends on nothing but a relaunch, and deletes nothing', async () => {
  const r = rig()
  r.sync.answer = (req) =>
    req.after === 0 ? bootstrapPage(3, [message(1), message(2), message(3)]) : bootstrapPage(req.after)
  r.t.start()
  const s = await r.connect()
  await flush()
  const held = r.t.snapshot()
  assert.equal(held.messages.length, 3)
  s.serverClose(4001)
  await flush()
  assert.equal(r.t.status().terminal.kind, 'protocol')
  assert.match(r.t.status().terminal.reason, /4001/)
  const dials = r.t.status().stats.dials

  // Not a timer, not a wake signal, not retryNow, not start().
  await r.clock.advance(10 * 60_000)
  for (const e of ['online', 'visible', 'pageshow', 'resume'] as const) r.lifecycle.emit(e)
  r.clock.jump(10 * 60_000)
  await r.clock.advance(0)
  r.t.retryNow()
  r.t.start()
  await flush()
  assert.equal(r.t.status().stats.dials, dials, 'no dial after terminal')
  assert.equal(r.net.sockets.length, 1)
  assert.equal(r.t.status().terminal.kind, 'protocol', 'still terminal')

  // Nothing deleted: the journal is whole, and the credential is what it was.
  assert.deepEqual(r.t.snapshot(), held)

  // A relaunch — a new transport over the same stores — dials again, from
  // the cursor it left.
  const again = createAgain(r)
  again.t.start()
  await flush()
  assert.equal(again.net.sockets.length, 1)
  again.net.last.open()
  const hello = again.net.last.frames()[0]
  assert.equal(hello.type, 'hello')
  assert.equal(hello.type === 'hello' && hello.resumeFromLogSeq, 3)
})

test('criterion 26 · a maxed-out backoff with no terminal reports none', async () => {
  const r = rig()
  r.net.onDial = (s) => s.fail()
  r.sync.answer = () => new Error('network down')
  r.t.start()
  await r.clock.advance(60_000)
  const st = r.t.status()
  assert.equal(st.terminal.kind, 'none')
  assert.ok(st.nextDialAt !== null, 'a dial is pending')
  assert.ok(st.stats.dials > 10)
})

test('criterion 26 · 1008 after error{unauthorized} and after error{wire_version_unsupported} are terminal', async () => {
  for (const code of ['unauthorized', 'wire_version_unsupported'] as const) {
    const { end, status } = await closeAfter(1008, (s) => s.frame(err(code, { retryable: false })))
    assert.equal(end.verdict, 'terminal_protocol', code)
    assert.equal(end.bare1008, false, `${code}: frame-preceded, not bare`)
    assert.equal(status.terminal.kind, 'protocol', code)
  }
})

test('negative controls · neverTerminal and alwaysTerminal each break the table', async () => {
  const never = rig({ faults: { neverTerminal: true } })
  never.sync.answer = () => bootstrapPage()
  never.t.start()
  ;(await never.connect()).serverClose(4001)
  await flush()
  assert.equal(never.t.status().terminal.kind, 'none', 'neverTerminal: a revoked session reconnects')

  const always = rig({ faults: { alwaysTerminal: true } })
  always.sync.answer = () => bootstrapPage()
  always.t.start()
  ;(await always.connect()).serverClose(1001)
  await flush()
  assert.equal(always.t.status().terminal.kind, 'protocol', 'alwaysTerminal: a drain stops the client')
})
