/* CANT-35 criteria 9 and 17: the dial backoff under ruling 3 → B, the
 * wake-signal early dial, and the browser lifecycle (row 1's half). Mirrors
 * the backoff in internal/client/client.go's Run, which has the stability rule
 * and the jitter since CANT-170 (backoff_test.go) and no wake signals. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { jitteredWait, resetsRamp, unitFromBytes } from '../backoff'
import type { LifecycleEvent } from '../seams'
import { bootstrapPage, flush, maxDraw, minDraw, page, ready, rig } from './harness'

const TEN_MINUTES = 10 * 60_000

test('backoff · the pure pieces: the jitter band and the stability rule', () => {
  assert.equal(unitFromBytes(minDraw(4)), 0)
  assert.equal(unitFromBytes(maxDraw(4)), 1)
  assert.equal(jitteredWait(5000, 0), 4000, 'the minimum draw is 0.8d')
  assert.equal(jitteredWait(5000, 1), 5000, 'the maximum is d: never above the ceiling')
  assert.equal(resetsRamp(null, 35), false, 'never ready: no reset')
  assert.equal(resetsRamp(34_999, 35), false, 'ready for less than an interval: no reset')
  assert.equal(resetsRamp(35_000, 35), true)
})

/** A 10-minute outage from the floor in which every dial fails. */
async function outage(random: (n: number) => Uint8Array, wakeEvery: number | null) {
  const r = rig({ random })
  r.sync.answer = (req) => (r.net.sockets.length === 1 ? bootstrapPage(req.after) : new Error('network down'))
  const waits: number[] = []
  let seen: number | null = null
  r.t.subscribe((s) => {
    if (s.nextDialAt !== null && s.nextDialAt !== seen) waits.push(s.nextDialAt - r.clock.now())
    seen = s.nextDialAt
  })
  r.t.start()
  if (wakeEvery !== null) {
    // A ready that announced 35 s, then the outage.
    const s = await r.connect(ready({ heartbeatIntervalSec: 35 }))
    r.net.onDial = (x) => x.fail()
    s.serverClose(1006)
  } else {
    r.net.onDial = (x) => x.fail()
  }
  await flush()
  const dialsAtStart = r.t.status().stats.dials
  const syncsAtStart = r.sync.requests.length
  let elapsed = 0
  while (elapsed < TEN_MINUTES) {
    const step = wakeEvery ?? TEN_MINUTES
    await r.clock.advance(Math.min(step, TEN_MINUTES - elapsed))
    elapsed += step
    if (wakeEvery !== null && elapsed < TEN_MINUTES) r.lifecycle.emit('online')
  }
  await flush()
  const st = r.t.status()
  return {
    // Dials INCLUDE the first dial of the outage.
    dials: st.stats.dials - dialsAtStart + (wakeEvery === null ? 0 : 1),
    syncs: r.sync.requests.length - syncsAtStart,
    waits,
    withheld: st.stats.dialsWithheld,
  }
}

test('criterion 9 · a 10-minute outage stays within 156 dials at the minimum draw, one /sync per dial at most', async (t) => {
  const o = await outage(minDraw, null)
  t.diagnostic(`minimum draw: ${o.dials} dials, ${o.syncs} /sync requests in ten minutes`)
  assert.ok(o.dials <= 156, `${o.dials} dials`)
  assert.ok(o.dials >= 150, `${o.dials} dials: the bound is tight, not vacuous`)
  assert.ok(o.syncs <= o.dials, `${o.syncs} syncs for ${o.dials} dials`)
  // The ramp: 250, 500, 1000, 2000, 4000, then the 5 s ceiling, each drawn at 0.8×.
  assert.deepEqual(o.waits.slice(0, 7), [200, 400, 800, 1600, 3200, 4000, 4000])
})

test('criterion 9 · and within it at the maximum draw', async (t) => {
  const o = await outage(maxDraw, null)
  t.diagnostic(`maximum draw: ${o.dials} dials, ${o.syncs} /sync requests in ten minutes`)
  assert.ok(o.dials <= 156, `${o.dials} dials`)
  assert.ok(o.syncs <= o.dials)
  assert.deepEqual(o.waits.slice(0, 6), [250, 500, 1000, 2000, 4000, 5000])
})

test('criterion 9 · with a wake signal every 10 s, within 174, and no early dial resets the ramp', async (t) => {
  const o = await outage(minDraw, 10_000)
  t.diagnostic(`wake every 10 s: ${o.dials} dials, ${o.withheld} wake signals withheld`)
  assert.ok(o.dials <= 174, `${o.dials} dials`)
  assert.ok(o.withheld > 0, 'wake signals inside the interval were withheld and counted')
  // Once the ramp reaches the ceiling it stays there: an early dial never put
  // it back to the floor.
  const afterRamp = o.waits.slice(6)
  assert.ok(afterRamp.length > 100)
  assert.ok(afterRamp.every((w) => w >= 4000), `smallest later wait ${Math.min(...afterRamp)}`)
})

test('criterion 9 · a wake signal during an error{internal} maximum wait does not shorten it', async () => {
  const r = rig()
  r.sync.answer = (req) => bootstrapPage(req.after)
  r.t.start()
  const s = await r.connect()
  s.frame({ type: 'error', code: 'internal', message: 'boom', retryable: false })
  s.serverClose(1008)
  await flush()
  const dials = r.t.status().stats.dials
  const wait = r.t.status().nextDialAt! - r.clock.now()
  assert.ok(wait >= 4000)
  for (const e of ['online', 'visible', 'pageshow', 'resume'] as const) r.lifecycle.emit(e)
  await r.clock.advance(wait - 1)
  assert.equal(r.t.status().stats.dials, dials, 'nothing dialed before the maximum wait ran out')
  await r.clock.advance(1)
  assert.equal(r.t.status().stats.dials, dials + 1)
})

test('criterion 9 · a wake signal before any ready makes no early dial', async () => {
  const r = rig()
  r.net.onDial = (s) => s.fail()
  r.sync.answer = () => new Error('down')
  r.t.start()
  await r.clock.advance(20_000)
  const dials = r.t.status().stats.dials
  r.lifecycle.emit('online')
  r.lifecycle.emit('resume')
  await flush()
  assert.equal(r.t.status().stats.dials, dials)
})

test('criterion 9 · a path that answers ready and drops at once stays within the outage bound', async (t) => {
  const r = rig()
  r.sync.answer = (req) => page({ logSeq: req.after })
  r.net.onDial = (s) => {
    s.open()
    s.frame(ready())
    s.serverClose(1006)
  }
  r.t.start()
  await r.clock.advance(TEN_MINUTES)
  const dials = r.t.status().stats.dials
  t.diagnostic(`ready-then-drop: ${dials} dials in ten minutes`)
  assert.ok(dials <= 156, `${dials} dials in ten minutes (reset-on-any-ready would make ~2400)`)
  assert.equal(r.t.status().stats.readys, dials)
})

test('criterion 9 · ruling 3 → B: a session ready for a whole interval resets the ramp, a shorter one does not', async () => {
  const r = rig()
  r.sync.answer = (req) => bootstrapPage(req.after)
  r.t.start()
  // Grow the ramp: five failed dials.
  r.net.onDial = (s) => s.fail()
  await r.clock.advance(1)
  r.net.last.fail()
  await r.clock.advance(6_500)
  const grown = r.t.status().nextDialAt! - r.clock.now()
  r.net.onDial = () => {}
  await r.clock.advance(grown)
  // Ready, and dropped well inside one interval: the ramp carries on.
  let s = await r.connect(ready({ heartbeatIntervalSec: 35 }))
  await r.clock.advance(10_000)
  s.serverClose(1006)
  await flush()
  const afterShort = r.t.status().nextDialAt! - r.clock.now()
  assert.ok(afterShort >= 3200, `a short session left the ramp at ${afterShort}`)
  await r.clock.advance(afterShort)
  // Ready for longer than one interval: the ramp resets to its floor.
  s = await r.connect(ready({ heartbeatIntervalSec: 35 }))
  s.frame({ type: 'pong', id: 'hb-1' })
  await r.clock.advance(36_000)
  s.frame({ type: 'pong', id: 'hb-2' })
  s.serverClose(1006)
  await flush()
  const afterStable = r.t.status().nextDialAt! - r.clock.now()
  assert.ok(afterStable <= 250, `a stable session reset the ramp to ${afterStable}`)
})

test('criterion 9 · retryNow() resets the ramp and dials now', async () => {
  const r = rig()
  r.net.onDial = (s) => s.fail()
  r.sync.answer = () => new Error('down')
  r.t.start()
  await r.clock.advance(30_000)
  const dials = r.t.status().stats.dials
  assert.ok(r.t.status().nextDialAt! - r.clock.now() >= 1)
  r.t.retryNow()
  await flush()
  assert.equal(r.t.status().stats.dials, dials + 1, 'dialed at once')
  assert.ok(r.t.status().nextDialAt! - r.clock.now() <= 250, 'from the floor')
  assert.equal(r.t.status().attempt, 1)
})

// --- criterion 17: the browser lifecycle ----------------------------------------------

const WAKE_EVENTS: LifecycleEvent[] = ['online', 'visible', 'pageshow', 'resume']

test('criterion 17 · each wake signal pings at once when a socket is open', async () => {
  for (const e of [...WAKE_EVENTS, 'clock'] as const) {
    const r = rig()
    r.sync.answer = (req) => bootstrapPage(req.after)
    r.t.start()
    const s = await r.connect(ready({ heartbeatIntervalSec: 35, missedPongLimit: 2 }))
    const pings = () => s.frames().filter((f) => f.type === 'ping').length
    assert.equal(pings(), 0)
    if (e === 'clock') {
      // The lid closed for longer than the server's backstop window.
      r.clock.jump(35_000 * 3 + 1_000)
      await r.clock.advance(0)
    } else {
      r.lifecycle.emit(e)
      await flush()
    }
    assert.equal(pings(), 1, `${e}: one immediate ping`)
    assert.equal(r.t.status().stats.dials, 1, `${e}: and no dial`)
  }
})

test('criterion 17 · each wake signal makes one early dial when no socket is open, rate-limited per interval', async () => {
  for (const e of [...WAKE_EVENTS]) {
    const r = rig()
    r.sync.answer = (req) => bootstrapPage(req.after)
    r.t.start()
    const s = await r.connect(ready({ heartbeatIntervalSec: 35 }))
    r.net.onDial = (x) => x.fail()
    s.serverClose(1006)
    await r.clock.advance(20_000) // the ramp is at its ceiling
    const dials = r.t.status().stats.dials
    const pending = r.t.status().nextDialAt! - r.clock.now()
    assert.ok(pending > 100, `${e}: a dial is pending`)
    for (let i = 0; i < 10; i++) r.lifecycle.emit(e)
    await flush()
    assert.equal(r.t.status().stats.dials, dials + 1, `${e}: ten signals within one interval make exactly one early dial`)
    assert.ok(r.t.status().nextDialAt! - r.clock.now() >= 4000, `${e}: and the ramp was not reset`)
  }
})

test('criterion 17 · a clock jump with no socket open is not a dial of its own', async () => {
  const r = rig()
  r.net.onDial = (s) => s.fail()
  r.sync.answer = () => new Error('down')
  r.t.start()
  await r.clock.advance(10_000)
  const dials = r.t.status().stats.dials
  r.clock.jump(1)
  await flush()
  assert.equal(r.t.status().stats.dials, dials)
})

test('criterion 17 · no wake signal changes a terminal state', async () => {
  const r = rig()
  r.sync.answer = (req) => bootstrapPage(req.after)
  r.t.start()
  const s = await r.connect()
  s.serverClose(4001)
  await flush()
  for (const e of WAKE_EVENTS) r.lifecycle.emit(e)
  r.clock.jump(600_000)
  await r.clock.advance(60_000)
  assert.equal(r.t.status().terminal.kind, 'protocol')
  assert.equal(r.t.status().stats.dials, 1)
})

test('criterion 17 · ruling 5 → B: visible, online and pageshow pull a catch-up once per interval, inert before ready', async () => {
  const r = rig()
  r.sync.answer = (req) => bootstrapPage(req.after)
  r.t.start()
  await flush()
  // Before the first ready: the dial pulled its own /sync, and the policy adds nothing.
  const beforeReady = r.sync.requests.length
  for (const e of ['visible', 'online', 'pageshow'] as const) r.lifecycle.emit(e)
  await flush()
  assert.equal(r.sync.requests.length, beforeReady, 'inert before the first ready')

  await r.connect(ready({ heartbeatIntervalSec: 35 }))
  await flush()
  let n = r.sync.requests.length
  r.lifecycle.emit('visible')
  await flush()
  assert.equal(r.sync.requests.length, n + 1, 'becoming visible pulls one catch-up')
  n = r.sync.requests.length
  r.lifecycle.emit('online')
  r.lifecycle.emit('pageshow')
  r.lifecycle.emit('visible')
  await flush()
  assert.equal(r.sync.requests.length, n, 'at most once per heartbeat interval')
  await r.clock.advance(35_000)
  // Keep the socket alive through the tick.
  r.net.last.frame({ type: 'pong', id: 'hb-1' })
  n = r.sync.requests.length
  r.lifecycle.emit('pageshow')
  await flush()
  assert.equal(r.sync.requests.length, n + 1, 'and again once the interval has passed')
  r.lifecycle.emit('resume')
  await flush()
  assert.equal(r.sync.requests.length, n + 1, 'resume is a wake signal, not a catch-up trigger')
})
