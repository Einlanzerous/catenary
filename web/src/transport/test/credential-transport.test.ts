/* CANT-35 criteria 11, 13 (the browser-only triggers), 14 and 27: the
 * credential layer under a running transport. Mirrors refresh_test.go's
 * single-flight case and terminal_test.go's `TestWhenARefusedRefreshIsTerminal`
 * and `TestAProactiveTerminalCountsNoDial`; the browser-only triggers and the
 * wake-signal refresh check have no Go counterpart, because Go has no page. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { LifecycleEvent } from '../seams'
import { flush } from './harness'
import {
  advanceUntil, CATENARY_401, credRig, FIRST_ACCESS, FIRST_REFRESH, noReplays, padToken, PROXY_401, type CredRig,
} from './credential-harness'

const HOUR = 3600_000

/** The token not due until the clock says so. */
async function notDue(r: CredRig, ms = HOUR) {
  const held = await r.held()
  await r.store.update(() => ({ write: { ...held, accessExpiresAt: r.clock.now() + ms }, result: undefined }))
}

test('criterion 11 · two transports over one store and one lock, refreshing at once, send exactly one /refresh', async () => {
  const r = credRig()
  const a = r.transport(r.c).t
  const b = r.transport(r.layer()).t
  const bLayer = (b as unknown as { cred: { status(): { refreshesSkipped: number } } }).cred
  // PERSIST BEFORE PRESENT: whenever a request carries a token, the store already holds it.
  const presented: { token: string; held: string }[] = []
  r.net.onSent = (q) => {
    if (q.path === '/sync' || q.path === '/ws') void r.store.read().then((h) => presented.push({ token: q.token, held: h!.accessToken }))
  }
  await Promise.all([a.refreshIfDue(), b.refreshIfDue()])
  assert.equal(r.net.count('/refresh'), 1, 'single-flight per credential')
  assert.equal(r.c.status().refreshes + bLayer.status().refreshesSkipped, 2, 'one rotated, the other re-read under the lock and skipped')
  a.start()
  b.start()
  await flush()
  for (const p of presented) assert.equal(p.token, p.held, 'the rotated pair was persisted before it was presented')
  assert.ok(presented.length > 0)
  a.stop()
  b.stop()
  assert.equal(noReplays(r.net.family), null)
})

test('criterion 11 · negative control refreshUnlocked: two refreshes of one pair', async () => {
  const r = credRig({ faults: { refreshUnlocked: true } })
  const second = r.layer({ refreshUnlocked: true })
  await Promise.all([r.c.attemptIfDue(), second.attemptIfDue()])
  assert.ok(r.net.count('/refresh') >= 2, `${r.net.count('/refresh')} /refresh requests; the unlocked pair refreshed twice`)
})

test('TestAProactiveTerminalCountsNoDial · criterion 14: Catenary’s own 401 on /refresh for the stored pair is terminal', async () => {
  const r = credRig()
  r.net.family.revoked = true
  const { t, lifecycle } = r.transport()
  t.start()
  await flush()
  const s = t.status()
  assert.equal(s.terminal.kind, 'credential')
  assert.equal(s.stats.dials, 0, 'terminal before the first dial: no dial counted')
  assert.equal(s.stats.dialErrors, 0)
  assert.equal(r.net.count('/ws') + r.net.count('/sync'), 0, 'nothing reached the server but the refresh')
  const before = await r.held()
  assert.equal(before.refreshToken, FIRST_REFRESH, 'the credential is not deleted')

  // Neither a timer, nor a wake signal, nor retryNow() ends it.
  await r.clock.advance(HOUR)
  for (const e of ['online', 'visible', 'pageshow', 'resume'] as LifecycleEvent[]) lifecycle.emit(e)
  t.retryNow()
  t.start()
  await flush()
  assert.equal(t.status().terminal.kind, 'credential')
  assert.equal(r.net.count('/ws') + r.net.count('/sync'), 0)
  assert.equal(r.net.count('/refresh'), 1)
  assert.deepEqual(await r.held(), before, 'nothing deleted, nothing changed')

  // A relaunch re-checks it, once, by simply running: its pre-dial attempt is
  // held (a new context has heard nothing), the /sync beside its dial is
  // refused, and the reactive refresh walks the one link the first attempt left
  // — the newest token, then the oldest — to the same terminal.
  const again = r.transport(r.layer()).t
  again.start()
  await flush()
  assert.equal(again.status().terminal.kind, 'credential')
  assert.equal(r.net.count('/refresh'), 1 + 2, 'one attempt, walking two links')
  assert.equal(again.status().stats.dials, 1)
})

test('criterion 14 · what is not terminal', async (tc) => {
  const cases: { name: string; arrange: (r: CredRig) => void | Promise<void> }[] = [
    { name: "a hop's 401 on /refresh", arrange: (r) => void (r.net.family.script = [PROXY_401]) },
    {
      name: 'Catenary’s 401 for a credential another context rotated in flight',
      arrange: (r) => {
        r.net.family.script = [CATENARY_401]
        r.net.family.onRefresh = () => {
          void r.store.update((h) => ({
            write: { ...h!, accessToken: padToken('access-elsewhere'), refreshToken: padToken('refresh-elsewhere'), chain: [], lastSentAt: null },
            result: undefined,
          }))
        }
      },
    },
    {
      name: 'refresh_expires_at passing',
      arrange: async (r) => {
        const h = await r.held()
        await r.store.update(() => ({ write: { ...h, refreshExpiresAt: r.clock.now() - HOUR }, result: undefined }))
      },
    },
  ]
  for (const c of cases) {
    await tc.test(c.name, async () => {
      const r = credRig()
      await c.arrange(r)
      let terminal = 'none'
      r.c.attach({ terminal: (x) => (terminal = x.kind), notify() {} })
      assert.notEqual(await r.c.attemptIfDue(), 'terminal')
      assert.equal(terminal, 'none')
    })
  }
  await tc.test('an upgrade failure, however often', async () => {
    const r = credRig({ accessLive: true })
    await notDue(r)
    r.net.refuseWs = () => true
    const { t } = r.transport()
    t.start()
    await advanceUntil(r.clock, () => t.status().stats.dials >= 5, 'five refused upgrades', 250)
    assert.equal(t.status().terminal.kind, 'none')
    t.stop()
  })
})

test('criterion 14 · negative controls neverTerminal and alwaysTerminal each break it', async () => {
  const never = credRig({ faults: { neverTerminal: true } })
  never.net.family.revoked = true
  let kind = 'none'
  never.c.attach({ terminal: (x) => (kind = x.kind), notify() {} })
  assert.equal(await never.c.attemptIfDue(), 'failed')
  assert.equal(kind, 'none', 'neverTerminal: a revoked device keeps trying for ever')

  const always = credRig({ faults: { alwaysTerminal: true }, script: [PROXY_401] })
  always.c.attach({ terminal: (x) => (kind = x.kind), notify() {} })
  assert.equal(await always.c.attemptIfDue(), 'terminal')
  assert.equal(kind, 'credential', 'alwaysTerminal: a hop’s 401 logs the person out')
})

test('criterion 27 · every wake signal runs the explicit refresh check before the dial it makes', async (tc) => {
  for (const signal of ['online', 'visible', 'pageshow', 'resume'] as LifecycleEvent[]) {
    await tc.test(signal, async () => {
      const r = credRig({ accessLive: true })
      await notDue(r)
      const { t, lifecycle } = r.transport()
      t.start()
      await flush()
      assert.ok(t.status().ready)
      r.net.open[0].serverClose(1006)
      await flush()
      // The lid was closed for two hours: the pair is now due.
      r.clock.jump(2 * HOUR)
      const ws = r.net.count('/ws')
      lifecycle.emit(signal)
      await flush()
      assert.equal(r.net.count('/refresh'), 1, 'the refresh check ran')
      const dial = r.net.on('/ws')[ws]
      assert.ok(dial, 'and the early dial went out')
      assert.equal(dial.token, padToken('access-1'), 'presenting the refreshed pair, not the due one')
      assert.ok(r.net.tape.indexOf(r.net.on('/refresh')[0]) < r.net.tape.indexOf(dial))
      t.stop()
    })
  }
  await tc.test('the wake detector’s clock jump', async () => {
    const r = credRig({ accessLive: true })
    await notDue(r)
    const { t } = r.transport()
    t.start()
    await flush()
    r.clock.jump(2 * HOUR)
    await r.clock.advance(35_000 + 1)
    assert.equal(r.net.count('/refresh'), 1, 'the jump is a wake signal, and the check ran')
    assert.equal((await r.held()).accessToken, padToken('access-1'))
    t.stop()
  })
})

test('criterion 13 · while a token is refused, no browser-only trigger sends a dial or an extra /sync', async (tc) => {
  await tc.test('no socket: online, visible, pageshow, resume, catchUp() and retryNow()', async () => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    await notDue(r)
    const { t, lifecycle } = r.transport()
    t.start()
    await flush()
    assert.ok(t.status().ready, 'a ready first, so the early dial has an interval to be limited by')
    // The access token dies on the server's side, and the socket with it.
    r.net.first = ''
    r.net.open[0].serverClose(1006)
    await advanceUntil(r.clock, () => t.status().tokenRefused && t.status().stats.chainLength >= 1, 'the token refused and a hold in force', 50)
    const ws = r.net.count('/ws')
    const syncs = r.net.count('/sync')
    const refreshes = r.net.count('/refresh')
    const { dialsWithheld, syncsWithheld } = t.status().stats
    for (const e of ['online', 'visible', 'pageshow', 'resume'] as LifecycleEvent[]) lifecycle.emit(e)
    t.catchUp()
    t.retryNow()
    await flush()
    const s = t.status().stats
    assert.equal(r.net.count('/ws'), ws, 'no dial')
    assert.equal(r.net.count('/sync'), syncs, 'no /sync')
    assert.equal(r.net.count('/refresh'), refreshes, 'the pair is not due, so the refresh check sent nothing')
    assert.equal(s.dialsWithheld - dialsWithheld, 5, 'four wake signals and retryNow() counted as withheld dials')
    assert.equal(s.syncsWithheld - syncsWithheld, 1, 'catchUp() counted as a withheld /sync')

    // The refresh check STILL RUNS: with the pair due, a wake signal makes its
    // explicit attempt — and still no dial and no /sync.
    await notDue(r, 0)
    lifecycle.emit('online')
    await flush()
    assert.equal(r.net.count('/refresh'), refreshes + 1, 'the explicit check ran')
    assert.equal(r.net.count('/ws'), ws)
    assert.equal(r.net.count('/sync'), syncs)

    // And exactly one /sync goes out per hold, when it ends.
    const links = t.status().stats.chainLength
    await advanceUntil(r.clock, () => r.net.count('/sync') > syncs, 'the hold to end', 250)
    await flush()
    assert.equal(r.net.count('/sync'), syncs + 1, 'one /sync for the hold')
    assert.equal(t.status().stats.chainLength, links + 1, 'and its answer drove one attempt')
    assert.equal(r.net.count('/ws'), ws, 'still no dial')
    t.stop()
  })
  await tc.test('socket up: the policy trigger, the wake detector and catchUp()', async () => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    // Eight unsettled links: a hold of minutes, so nothing ends it inside the window.
    for (let i = 0; i < 8; i++) await r.c.attemptIfDue()
    await notDue(r)
    r.net.refuseSync = () => true
    const { t, lifecycle } = r.transport()
    t.start()
    await advanceUntil(r.clock, () => t.status().ready && t.status().tokenRefused, 'the socket up and the page refused', 10)
    const syncs = r.net.count('/sync')
    const ws = r.net.count('/ws')
    const { syncsWithheld } = t.status().stats
    lifecycle.emit('visible') // ruling 5's policy trigger, and a wake signal
    t.catchUp()
    r.clock.jump(200_000) // the lid: past the backstop window
    await r.clock.advance(35_000) // the next heartbeat tick sees the jump
    await flush()
    assert.equal(r.net.count('/sync'), syncs, 'no /sync')
    assert.equal(r.net.count('/ws'), ws, 'no dial')
    assert.ok(t.status().stats.syncsWithheld - syncsWithheld >= 2, 'the policy trigger and catchUp() counted')
    assert.ok(t.status().connected, 'the socket was not closed')
    t.stop()
  })
})

test('no token reaches a log line or a status field', async () => {
  const r = credRig({ script: [PROXY_401], dead: [] })
  const { t } = r.transport()
  t.start()
  await r.clock.advance(10 * 60_000)
  t.stop()
  const tokens = [FIRST_ACCESS, FIRST_REFRESH, ...r.net.tape.map((q) => q.proposal).filter((p) => p !== '')]
  for (let i = 1; i <= r.net.family.minted; i++) tokens.push(padToken(`access-${i}`))
  const text = JSON.stringify(r.logs) + JSON.stringify(t.status()) + JSON.stringify(r.c.status())
  for (const tok of tokens) assert.ok(!text.includes(tok), `a token leaked: ${tok.slice(0, 8)}…`)
  assert.ok(r.logs.length > 0)
})
