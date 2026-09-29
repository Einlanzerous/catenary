/* CANT-35 criterion 12, the hold half: CANT-127's two suppressors, measured.
 * Each test is named after the internal/client/hold_test.go case it ports.
 * Simulated hours run on the fake clock through a real transport, so the dial
 * count and the hours agree by construction rather than by a per-dial charge. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { refreshDelay } from '../hold'
import { flush } from './harness'
import {
  advanceUntil, credRig, fetchVia, FIRST_REFRESH, LOSE_RESPONSE, noReplays, subsequence, type CredRig,
} from './credential-harness'

const HOUR = 3600_000
const MIN = 60_000

/** Go's `buildLink`: an EXPLICIT refresh into a black-holed /refresh — one
 *  link, one stamp, at the wall clock of the write. */
async function buildLink(r: CredRig): Promise<number> {
  const at = r.clock.now()
  const links = (await r.held()).chain.length
  assert.equal(await r.c.attemptIfDue(), 'failed', 'the black-holed /refresh reported success')
  const held = await r.held()
  assert.equal(held.chain.length, links + 1)
  assert.equal(held.lastSentAt, at, 'last_sent_at is the wall clock at the write')
  return at
}

const refreshesSent = (r: CredRig) => r.net.count('/refresh')

/** An outage of `span` over /ws, /sync and /refresh, driven by a transport. */
function outageRig(span: number, faults = {}) {
  const r = credRig({ faults })
  r.net.deadUntil = r.clock.now() + span
  for (const p of ['/ws', '/sync', '/refresh']) r.net.deadUntilPaths.add(p)
  return r
}

test('TestEightHoursOfADeadNetworkCostsOneLink', async (tt) => {
  const span = 8 * HOUR
  const r = outageRig(span)
  const returned = r.net.deadUntil!
  const { t } = r.transport()
  t.start()
  await advanceUntil(r.clock, () => {
    const s = t.status()
    return s.stats.refreshes === 1 && s.ready
  }, 'the client to rotate and reconnect after the outage', 10 * MIN, span + HOUR)
  t.stop()

  const during = r.net.tape.filter((q) => q.at < returned)
  const dials = during.filter((q) => q.path === '/ws').length
  const sent = during.filter((q) => q.path === '/refresh').length
  const s = t.status().stats
  tt.diagnostic(`eight hours dead: ${dials} dials, ${sent} /refresh, ${s.refreshesHeldUnreachable} held by the gate, chain ${s.chainLength} after recovery`)
  assert.ok(dials >= span / 5_000, `the outage cost ${dials} dials; at a 5 s ceiling it is at least ${span / 5_000}`)
  assert.equal(sent, 1, 'one /refresh over eight hours of a dead network: the one that made the link')
  assert.ok(r.net.family.statuses.filter((x) => x === 401).length <= 1, 'at most one unknown token refused on return')
  assert.equal(noReplays(r.net.family), null)
  const held = await r.held()
  assert.notEqual(held.refreshToken, FIRST_REFRESH)
  assert.equal(held.refreshToken, r.net.family.live())
  assert.deepEqual(held.chain, [])
  assert.ok(s.refreshesHeldUnreachable >= dials - 2, `${s.refreshesHeldUnreachable} held by the gate over ${dials} dials`)
  assert.equal(s.refreshErrors, 1, 'the one real failure')
  assert.equal(s.refreshesHeldBackoff, 0)

  // O(1) lines about refreshing over thousands of dials, not one per held attempt.
  const about = r.logs.filter((l) => l.msg.includes('refresh'))
  assert.ok(about.length <= 4, `${about.length} lines about refreshing:\n${about.map((l) => l.msg).join('\n')}`)
  assert.equal(r.matching('proactive refresh failed').length, 1)
  const closed = r.matching('refresh gate closed').length
  const opened = r.matching('refresh gate open').length
  assert.ok(closed >= 1 && opened >= 1 && closed + opened <= 3, `gate closed ${closed}, opened ${opened}`)
})

test('TestWithoutTheSuppressorsEveryDialMintsALink (negative control unbounded)', async (tt) => {
  // One hour rather than Go's eight: the claim is one link per dial, and the
  // proportion is the same at 900 dials as at 7,200.
  const span = HOUR
  const r = outageRig(10 * span, { unbounded: true })
  const { t } = r.transport()
  t.start()
  await r.clock.advance(span)
  t.stop()
  const dials = r.net.count('/ws')
  const chain = (await r.held()).chain.length
  tt.diagnostic(`unbounded, one hour dead: ${dials} dials minted a chain of ${chain}`)
  assert.ok(dials >= span / 5_000, `${dials} dials`)
  assert.ok(chain >= dials - 2, `the control's chain is ${chain} links after ${dials} dials; want one per dial`)
  assert.equal(noReplays(r.net.family), null)
})

test('TestTheSuppressorsOnlyRemoveRequests', async () => {
  const tape = async (faults = {}) => {
    const r = outageRig(10 * HOUR, faults)
    const { t } = r.transport()
    t.start()
    await r.clock.advance(HOUR)
    t.stop()
    assert.equal(noReplays(r.net.family), null)
    return r.net.on('/refresh').map((q) => ({ token: q.token, proposal: q.proposal }))
  }
  const held = await tape()
  const unbounded = await tape({ unbounded: true })
  assert.ok(held.length < unbounded.length, `${held.length} vs ${unbounded.length}`)
  assert.ok(subsequence(held, unbounded), 'the suppressed tape is the unsuppressed one with requests removed')
  for (const p of held) assert.ok(p.token !== '' && p.proposal !== '')
})

test('TestOnlyCatenaryOpensTheGate', async (t) => {
  const cases: { name: string; answer?: () => Response; dead?: boolean; open: boolean }[] = [
    { name: 'a /sync 200 the generated decoder accepts', open: true },
    { name: "Catenary's own 401 on /sync", answer: () => new Response('{"code":"unauthorized"}', { status: 401 }), open: true },
    { name: 'a 401 from a hop in front', answer: () => new Response('access: session expired', { status: 401 }), open: false },
    { name: 'a 200 that does not decode as a SyncResponse', answer: () => new Response('<html>sign in to this network</html>', { status: 200 }), open: false },
    { name: 'a 502 from a hop in front', answer: () => new Response('bad gateway', { status: 502 }), open: false },
    { name: 'a network error', dead: true, open: false },
  ]
  for (const tc of cases) {
    await t.test(tc.name, async () => {
      const r = credRig({ accessLive: true, dead: ['/refresh', ...(tc.dead ? ['/sync'] : [])] })
      await buildLink(r)
      // Past the backoff and past the send: only the gate decides.
      r.clock.jump(MIN)
      r.net.answerSync = tc.answer ?? null
      const before = refreshesSent(r)
      await fetchVia(r, r.c)
      const hold = r.c.status().refreshHold
      await r.c.refreshDueWhenAllowed()
      const requests = refreshesSent(r) - before
      if (tc.open) assert.equal(requests, 1, `the gate should have opened (hold ${hold})`)
      else {
        assert.equal(requests, 0, 'the gate should have stayed closed')
        assert.equal(hold, 'unreachable')
      }
      assert.equal(noReplays(r.net.family), null)
    })
  }
})

test('TestAnAnswerNoLaterThanTheSendLeavesTheGateClosed', async (t) => {
  await t.test('the answer arrived before the send', async () => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    assert.equal(await fetchVia(r, r.c), 'page')
    r.clock.jump(10_000)
    await buildLink(r)
    r.clock.jump(MIN)
    const before = refreshesSent(r)
    assert.equal(await r.c.refreshDueWhenAllowed(), 'held')
    assert.equal(r.c.status().refreshHold, 'unreachable')
    assert.equal(refreshesSent(r) - before, 0)
  })
  await t.test('the answer and the send are the same instant', async () => {
    const r = credRig({ dead: ['/refresh'] })
    const at = r.clock.now()
    // Catenary's 401 over an EMPTY chain: the reactive attempt goes out on the
    // same clock reading as the answer that allowed it.
    assert.equal(await fetchVia(r, r.c), 'unauthorized')
    assert.equal((await r.held()).lastSentAt, at)
    assert.equal(r.c.status().refreshHold, 'unreachable', 'a tie is closed')
    const before = refreshesSent(r)
    assert.equal(await fetchVia(r, r.c), 'unauthorized')
    assert.equal(refreshesSent(r) - before, 0, 'no /refresh on a tie')
    r.clock.jump(MIN)
    assert.equal(await fetchVia(r, r.c), 'unauthorized')
    assert.equal(refreshesSent(r) - before, 1, 'strictly later, and it opens')
    assert.equal(noReplays(r.net.family), null)
  })
})

test('TestAServerFrameOpensTheGateAndNothingElseOnTheSocketDoes', async () => {
  const r = credRig({ accessLive: true, dead: ['/refresh'] })
  // Not due yet, so the dial's own proactive check leaves the chain empty.
  const held = await r.held()
  await r.store.update(() => ({ write: { ...held, accessExpiresAt: r.clock.now() + HOUR }, result: undefined }))
  const { t } = r.transport()
  t.start()
  await flush()
  assert.ok(t.status().ready && t.status().caughtUp, 'the socket is up')
  // The ready is a frame, so the gate is open before there is a chain. Build a
  // link after it; then only the gate is left.
  r.clock.jump(HOUR)
  const sent = await buildLink(r)
  r.clock.jump(MIN)
  assert.equal(r.c.status().refreshHold, 'unreachable')

  const before = refreshesSent(r)
  r.net.push('{"type":"ready","session_id":"not-a-uuid"}')
  await flush()
  assert.ok(t.status().stats.undecodable > 0)
  assert.equal(r.c.status().refreshHold, 'unreachable', 'a frame the decoder refuses opens nothing')

  r.net.push({ type: 'ping', id: 'hb-from-the-server' })
  await flush()
  assert.equal(r.c.status().refreshHold, 'none', 'an accepted frame on the socket that was already up opens it')
  assert.equal(refreshesSent(r) - before, 0, 'opening it sends nothing: it is not a trigger')
  assert.ok(sent < r.clock.now())
  t.stop()
})

test('TestAnExplicitRefreshIsNeverHeld', async () => {
  const r = credRig({ dead: ['/refresh'] })
  await buildLink(r)
  assert.equal(r.c.status().refreshHold, 'unreachable')
  assert.equal(await r.c.refreshDueWhenAllowed(), 'held')
  const before = refreshesSent(r)
  for (let i = 0; i < 3; i++) assert.equal(await r.c.attemptIfDue(), 'failed', 'an attempt actually made')
  assert.equal(refreshesSent(r) - before, 3, 'each costs a link')
  assert.equal((await r.held()).chain.length, 4)
  assert.equal(noReplays(r.net.family), null)
})

test('TestTheStampIsTheSendAndSurvivesAKill', async (t) => {
  await t.test('the store holds the link AND its stamp when the request arrives', async () => {
    const r = credRig()
    let atArrival: { links: number; stamp: number | null } | null = null
    r.net.family.onRefresh = () => {
      void r.store.read().then((h) => (atArrival = { links: h!.chain.length, stamp: h!.lastSentAt }))
    }
    const at = r.clock.now()
    assert.equal(await r.c.attemptIfDue(), 'refreshed')
    assert.deepEqual(atArrival, { links: 1, stamp: at })
    assert.equal((await r.held()).lastSentAt, null, 'the answer took the stamp away')
  })
  await t.test('a client killed before any response leaves the stamp, and the next one is held by it', async () => {
    const r = credRig({ accessLive: true, script: [LOSE_RESPONSE] })
    let once = false
    r.net.family.onRefresh = () => {
      if (!once) r.c.kill()
      once = true
    }
    const at = r.clock.now()
    assert.notEqual(await r.c.attemptIfDue(), 'refreshed')
    assert.equal((await r.held()).lastSentAt, at)
    r.clock.jump(HOUR)
    const next = r.layer()
    const before = refreshesSent(r)
    assert.equal(await next.refreshDueWhenAllowed(), 'held', 'no free attempt for a context that has heard nothing')
    assert.equal(next.status().refreshHold, 'unreachable')
    assert.equal(refreshesSent(r) - before, 0)
    assert.equal(noReplays(r.net.family), null)
  })
  await t.test('a stamp in the future counts as absent', async () => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    await buildLink(r)
    r.clock.jump(1_000)
    assert.equal(await fetchVia(r, r.c), 'page')
    r.clock.jump(-5_000) // the clock set BACKWARDS: the stamp is in the future
    assert.equal(r.c.status().refreshHold, 'none')
    const before = refreshesSent(r)
    await r.c.refreshDueWhenAllowed()
    assert.equal(refreshesSent(r) - before, 1)
  })
  await t.test('a chain with no stamp, as written before CANT-127, reads as absent', async () => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    await buildLink(r)
    const held = await r.held()
    await r.store.update(() => ({ write: { ...held, lastSentAt: null }, result: undefined }))
    r.clock.jump(MIN)
    const before = refreshesSent(r)
    assert.equal(await r.c.refreshDueWhenAllowed(), 'held', 'absent is not a free attempt')
    assert.equal(await fetchVia(r, r.c), 'page')
    assert.equal(await r.c.refreshDueWhenAllowed(), 'failed', 'one answer, and the attempt goes')
    assert.equal(refreshesSent(r) - before, 1)
    assert.equal((await r.held()).lastSentAt, r.clock.now(), 'and writes a stamp')
  })
})

test('TestTheGateIsDerivedFromTheStamp', async (t) => {
  await t.test('a context over a non-empty chain sends nothing until Catenary answers it', async () => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    await buildLink(r)
    r.clock.jump(MIN)
    const second = r.layer()
    const before = refreshesSent(r)
    assert.equal(await second.refreshDueWhenAllowed(), 'held')
    assert.equal(second.status().refreshesHeldUnreachable, 1)
    assert.equal(await fetchVia(r, second), 'page')
    assert.equal(await second.refreshDueWhenAllowed(), 'failed')
    assert.equal(refreshesSent(r) - before, 1)
  })
  await t.test("another context's attempt closes an open gate", async () => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    await buildLink(r)
    const first = r.c
    const second = r.layer()
    r.clock.jump(30_000)
    for (const c of [first, second]) assert.equal(await fetchVia(r, c), 'page')
    assert.equal(first.status().refreshHold, 'none')
    r.clock.jump(30_000)
    assert.equal(await second.refreshDueWhenAllowed(), 'failed', 'the second context attempts')
    const before = refreshesSent(r)
    assert.equal(await first.refreshDueWhenAllowed(), 'held', "the first is held by the other's stamp")
    assert.equal(first.status().refreshHold, 'unreachable')
    r.clock.jump(10 * MIN)
    assert.equal(await fetchVia(r, first), 'page')
    assert.equal(first.status().refreshHold, 'none', 'an answer of its own, later than the new stamp')
    assert.equal(refreshesSent(r) - before, 0)
  })
  await t.test('a context over an empty chain refreshes proactively as it always did', async () => {
    const r = credRig({ dead: ['/refresh'] })
    assert.equal(r.c.status().refreshHold, 'none')
    assert.equal(await r.c.refreshDueWhenAllowed(), 'failed')
    assert.equal(refreshesSent(r), 1)
  })
})

test('TestTheBackoffBelongsToTheCredentialNotTheProcess', async (t) => {
  const hour = async (contexts: number) => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    const cs = [r.c]
    for (let i = 1; i < contexts; i++) cs.push(r.layer())
    await buildLink(r)
    for (let step = 0; step < 120; step++) {
      r.clock.jump(30_000)
      for (const c of cs) {
        assert.equal(await fetchVia(r, c), 'page')
        await c.refreshDueWhenAllowed()
      }
    }
    return refreshesSent(r)
  }
  const one = await hour(1)
  const two = await hour(2)
  assert.ok(one >= 5)
  assert.equal(two, one, 'the delay is the credential’s, not the context’s')

  await t.test('a fresh context mid-backoff waits for the stamp plus the delay', async () => {
    const r = credRig({ accessLive: true, dead: ['/refresh'] })
    const sent = await buildLink(r)
    r.clock.jump(2_000)
    const worker = r.layer()
    assert.equal(await fetchVia(r, worker), 'page')
    const before = refreshesSent(r)
    assert.equal(await worker.refreshDueWhenAllowed(), 'held')
    assert.equal(worker.status().refreshesHeldBackoff, 1, 'the gate was open; the delay held it')
    r.clock.t = sent + refreshDelay(1) + 1_000
    assert.equal(await fetchVia(r, worker), 'page')
    assert.equal(await worker.refreshDueWhenAllowed(), 'failed')
    assert.equal(refreshesSent(r) - before, 1)
  })
})

test('TestAHeldRefreshIsNotAFailure', async () => {
  const r = credRig({ dead: ['/refresh'] })
  await buildLink(r)
  // One second in: Catenary's 401 opens the gate, and the 5 s delay holds it.
  r.clock.jump(1_000)
  const before = refreshesSent(r)
  const syncs = r.net.count('/sync')
  assert.equal(await fetchVia(r, r.c), 'unauthorized', 'the 401 it already had')
  assert.equal(r.net.count('/sync') - syncs, 1, 'no retry with the same pair')
  assert.equal(refreshesSent(r) - before, 0)
  const s = r.c.status()
  assert.equal(s.refreshesHeldBackoff, 1)
  assert.equal(s.refreshesHeldUnreachable, 0)
  assert.equal(s.refreshErrors, 1, 'only the explicit attempt’s failure')
  assert.equal(s.refreshesSkipped, 0)
  assert.equal(s.refreshHold, 'backoff')
})

test('TestAPersonCanSeeTheHold', async () => {
  const r = credRig({ accessLive: true, dead: ['/refresh'] })
  assert.equal(r.c.status().chainLength, 0)
  assert.equal(r.c.status().refreshHold, 'none')
  await buildLink(r)
  assert.equal(r.c.status().chainLength, 1)
  assert.equal(r.c.status().refreshHold, 'unreachable')
  for (let i = 0; i < 5; i++) await r.c.refreshDueWhenAllowed()
  assert.equal(r.matching('refresh gate closed').length, 1, 'one line on close, however many held attempts')
  r.clock.jump(MIN)
  assert.equal(await fetchVia(r, r.c), 'page')
  assert.equal(r.matching('refresh gate open').length, 1)
  // A long chain is one WARN.
  for (let i = 0; i < 70; i++) await r.c.attemptIfDue()
  for (let i = 0; i < 3; i++) await r.c.refreshDueWhenAllowed()
  assert.equal(r.matching('the refresh chain is long').length, 1)
})
