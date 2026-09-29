/* CANT-35 criterion 13: a refused access token, measured (CANT-129). Each test
 * is named after the internal/client/refused_test.go case it ports, and runs a
 * real transport over the fake clock, so a simulated hour is the clock's and
 * not a per-dial charge.
 *
 * ONE DIFFERENCE FROM GO'S RIG, in the arithmetic only. Go's hop charges 5 s of
 * simulated time on every dial, so the /sync beside the first dial arrives after
 * the pre-dial attempt's stamp and drives the second attempt. Here the fake
 * clock does not move between them: that /sync is Catenary's answer at the SAME
 * instant as the stamp, later-than is strict, and the reactive refresh is held.
 * So the pages Catenary refuses equal the attempts here, where Go counts one
 * fewer; the bound — every refusal but the teaching dial is one per attempt —
 * is the same. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { refreshDelay } from '../hold'
import type { StoredCredential } from '../credential-store'
import {
  advanceUntil, attemptsWithin, credRig, fetchVia, noReplays, type CredRig, type CredRigOptions,
} from './credential-harness'
import { flush, uuid } from './harness'

const HOUR = 3600_000

function running(o: CredRigOptions = {}) {
  const r = credRig(o)
  const { t, lifecycle } = r.transport()
  t.start()
  return { r, t, lifecycle }
}

/** A second context with its own network, whose /refresh is not the dead one. */
function secondContext(r: CredRig) {
  const fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const dead = r.net.dead.has('/refresh')
    r.net.dead.delete('/refresh')
    try {
      return await r.net.fetch(input, init)
    } finally {
      if (dead) r.net.dead.add('/refresh')
    }
  }) as typeof globalThis.fetch
  return r.layer(undefined, undefined, fetch)
}

test('TestARefusedTokenIsPresentedOncePerRefreshHold', async (tt) => {
  const { r, t } = running({ dead: ['/refresh'] })
  await r.clock.advance(HOUR)
  t.stop()
  const attempts = r.net.count('/refresh')
  const allowed = attemptsWithin(HOUR)
  const s = t.status()
  tt.diagnostic(
    `over an hour the server refused ${r.net.refusals()} requests carrying the token (${r.net.refused['/ws']} upgrades, ` +
      `${r.net.refused['/sync']} pages) for ${attempts} refresh attempts; ${s.stats.dialsWithheld} dials and ` +
      `${s.stats.syncsWithheld} pages were withheld`,
  )
  assert.ok(r.net.refusals() <= attempts + 1, 'at most the attempts plus the one dial that taught the client')
  assert.equal(r.net.refused['/ws'], 1, 'the one upgrade that taught the client')
  assert.equal(r.net.refused['/sync'], attempts, 'one page per attempt (see the header for Go’s attempts − 1)')
  assert.equal(s.stats.dials, 1)
  assert.ok(attempts <= allowed, `${attempts} attempts, more than the ${allowed} the curve allows`)
  assert.ok(attempts >= allowed - 1, `${attempts} attempts, fewer than the ${allowed} the curve allows: a stall`)
  assert.equal((await r.held()).chain.length, attempts, 'an attempt that settles nothing is exactly one link')
  assert.ok(s.tokenRefused && s.stats.dialsWithheld > 0 && s.stats.syncsWithheld > 0, 'a person can see why it went quiet')
  assert.equal(r.matching('access token refused by Catenary').length, 1)
  assert.ok(r.matching('refused').length <= 2)
  assert.equal(noReplays(r.net.family), null)
})

test('TestWithoutTheRuleARefusedTokenIsPresentedOnEveryDial (negative control presentRefusedToken)', async (tt) => {
  const { r, t } = running({ dead: ['/refresh'], faults: { presentRefusedToken: true } })
  await r.clock.advance(HOUR)
  t.stop()
  const dials = t.status().stats.dials
  tt.diagnostic(`the control: the server refused ${r.net.refusals()} requests over ${dials} dials in an hour`)
  assert.ok(dials >= HOUR / 5_000 - 2, `${dials} dials`)
  assert.ok(r.net.refusals() > 1000, `${r.net.refusals()} refusals: the >1,000 CANT-129 exists to remove`)
  assert.ok(r.net.refusals() >= dials)
  assert.equal(t.status().stats.dialsWithheld, 0)
  assert.equal(noReplays(r.net.family), null)
})

test('TestWithoutTheOneSyncPerHoldTheClientStalls (negative control neverPresentRefusedToken)', async () => {
  const { r, t } = running({ dead: ['/refresh'], faults: { neverPresentRefusedToken: true } })
  await r.clock.advance(HOUR)
  t.stop()
  const attempts = r.net.count('/refresh')
  // Go's control stalls at two; here the teaching /sync is held at the same
  // instant as the pre-dial stamp (see the header), so it stalls at one.
  assert.equal(attempts, 1, 'the stalled client makes its pre-dial attempt and nothing after')
  assert.ok(attempts < attemptsWithin(HOUR) - 1, 'the lower bound catches it')
  assert.ok(t.status().stats.syncsWithheld > 0)
})

test('TestASocketThatStaysUpHasItsHoldEndedByTheCatchUp', async () => {
  const { r, t } = running({ accessLive: true, dropFirst: { '/refresh': 2 } })
  // /sync is refused while the pair is the enrolled one; the socket is not.
  r.net.refuseSync = () => r.net.family.minted === 0
  await advanceUntil(r.clock, () => {
    const s = t.status()
    return s.ready && s.tokenRefused && s.stats.chainLength >= 1
  }, 'the socket up and the catch-up refused', 10)
  // THE HOLD, HELD STILL: no clock movement, so only a trigger could send a page.
  const syncs = r.net.count('/sync')
  const { dials, readys } = t.status().stats
  r.net.push({ type: 'resync_required', reason: 'membership_changed', logSeq: 0 })
  r.net.push({ type: 'pong', id: 'from-the-server' })
  t.catchUp()
  await flush()
  assert.ok(t.status().stats.resyncs >= 1 && t.status().stats.pongsReceived >= 1, 'the frames arrived')
  assert.equal(r.net.count('/sync'), syncs, 'a trigger during a hold is a no-op')
  // The socket is still writable: the send is written, and only the ack is missing.
  const pending = t.send({ type: 'send', clientId: uuid(7777), conversationId: uuid(100), text: 'still here' })
  pending.catch(() => {})
  assert.ok(r.net.open[0].written.some((w) => w.includes(uuid(7777))))

  await advanceUntil(r.clock, () => {
    const s = t.status()
    return s.stats.refreshes === 1 && s.caughtUp && !s.tokenRefused
  }, 'the refresh to be answered and the catch-up to complete')
  const s = t.status()
  assert.equal(s.stats.dials, dials, 'never redialed')
  assert.equal(s.stats.readys, readys)
  assert.ok(s.connected && s.ready, 'the established socket is never closed by this rule')
  assert.equal(s.stats.refreshWalkBacks, 2)
  assert.equal(r.net.count('/refresh'), 3 + 2, 'three attempts and the two steps the last one owes')
  assert.equal(noReplays(r.net.family), null)
  t.stop()
})

test('TestItRecoversFromAClosedGateWithNoInput', async () => {
  const r = credRig({ dropFirst: { '/refresh': 3 } })
  const { t } = r.transport()
  // THE GATE, READ AS THE THIRD ATTEMPT LEAVES — its own stamp already
  // written, and no Catenary answer later than it.
  let holdAsThirdLeft: string | null = null
  r.net.onSent = (q) => {
    if (q.path === '/refresh' && r.net.count('/refresh') === 3) holdAsThirdLeft = r.c.status().refreshHold
  }
  t.start()
  await advanceUntil(r.clock, () => {
    const s = t.status()
    return s.stats.refreshes === 1 && s.ready && s.caughtUp
  }, 'the client to rotate, dial with the new pair and catch up', 250)
  t.stop()
  assert.equal(holdAsThirdLeft, 'unreachable', 'when /refresh healed the gate was closed')

  const sent = r.net.on('/refresh')
  assert.ok(sent.length >= 4)
  assert.equal(t.status().stats.refreshWalkBacks, 3, 'one attempt that steps back per link')
  const attempts = sent.slice(0, 4)
  for (let k = 2; k <= attempts.length; k++) {
    const gap = attempts[k - 1].at - attempts[k - 2].at
    const want = refreshDelay(k - 1)
    assert.ok(gap >= want, `attempt ${k} went out ${gap} ms after attempt ${k - 1}, sooner than ${want}`)
    assert.ok(gap <= want + 8 * 5_000, `attempt ${k} went out ${gap} ms after attempt ${k - 1}, later than the curve allowed`)
  }
  const s = t.status()
  assert.ok(!s.tokenRefused && s.refreshHold === 'none')
  assert.equal((await r.held()).refreshToken, r.net.family.live())
  assert.equal(noReplays(r.net.family), null)
})

test('TestAnotherContextsRotationEndsTheWait', async (tc) => {
  await tc.test('with no socket, another context’s rotation resumes the dial within one dial interval', async () => {
    const { r, t } = running({ dead: ['/refresh'] })
    await advanceUntil(r.clock, () => t.status().tokenRefused && t.status().stats.chainLength >= 2, 'a hold in force', 250)
    const attempts = r.net.count('/refresh')
    const refused = r.net.refused['/sync']
    assert.equal(await secondContext(r).attemptIfDue(), 'refreshed', 'the second context rotates')
    const spent = await advanceUntil(r.clock, () => {
      const s = t.status()
      return s.ready && s.caughtUp && !s.tokenRefused
    }, 'the first client to dial with the new pair and catch up', 250)
    assert.ok(spent <= 5_000, `resumed ${spent} ms after the rotation; within one dial interval`)
    assert.equal(r.net.count('/refresh') - attempts - 3, 0, 'the first client sent no /refresh of its own (3 were the other’s walk)')
    assert.equal(r.net.refused['/sync'], refused, 'the new token is not refused')
    assert.equal(r.c.status().refreshes, 0, 'it used what the other context wrote')
    assert.equal(noReplays(r.net.family), null)
    t.stop()
  })
  await tc.test('with the socket up, the parked catch-up completes within one poll', async () => {
    const { r, t } = running({ accessLive: true, dead: ['/refresh'] })
    r.net.refuseSync = () => r.net.family.minted === 0
    await advanceUntil(r.clock, () => {
      const s = t.status()
      return s.ready && s.tokenRefused && s.stats.chainLength >= 1 && !s.caughtUp
    }, 'the socket up, the catch-up refused and parked', 10)
    const { dials, readys } = t.status().stats
    assert.equal(await secondContext(r).attemptIfDue(), 'refreshed')
    const spent = await advanceUntil(r.clock, () => t.status().caughtUp && !t.status().tokenRefused, 'the parked catch-up', 250)
    assert.ok(spent <= 5_000, `${spent} ms`)
    const s = t.status()
    assert.ok(s.stats.dials === dials && s.stats.readys === readys && s.connected, 'the socket stayed up throughout')
    t.stop()
  })
  await tc.test('an answered explicit refresh on this client ends it too', async () => {
    const r = credRig({ dead: ['/refresh'] })
    assert.equal(await fetchVia(r, r.c), 'unauthorized')
    r.net.dead.delete('/refresh')
    assert.ok(r.c.refused())
    assert.equal((await r.held()).chain.length, 1)
    assert.equal(await r.c.withholdSync(), true, 'the reactive attempt’s own stamp holds the next page')
    assert.equal(await r.c.attemptIfDue(), 'refreshed')
    assert.equal(await r.c.withholdSync(), false)
    assert.equal(await r.c.withholdDial(), false)
    assert.equal(r.matching('the pair changed').length, 1)
  })
  await tc.test('an unanswered explicit refresh extends it, and costs no page', async () => {
    const r = credRig({ dead: ['/refresh'] })
    assert.equal(await fetchVia(r, r.c), 'unauthorized')
    const held = await r.held()
    r.clock.t = held.lastSentAt! + refreshDelay(held.chain.length) + 1_000
    assert.equal(await r.c.withholdSync(), false, 'the hold ended at its own deadline')
    const refused = r.net.refused['/sync']
    assert.equal(await r.c.attemptIfDue(), 'failed')
    assert.equal(await r.c.withholdSync(), true, 'the stamp moved, so the wait extends')
    const moved = await r.held()
    assert.equal(r.c.status().nextRefreshAt, moved.lastSentAt! + refreshDelay(moved.chain.length))
    assert.equal(r.net.refused['/sync'], refused)
  })
})

test('TestOnlyCatenarysOwn401OnSyncMarksATokenRefused', async (tc) => {
  const cases: { name: string; o: CredRigOptions; arrange: (r: CredRig) => void }[] = [
    { name: 'a 401 on the upgrade alone, with the page answering honestly', o: { accessLive: true, dead: ['/refresh'] }, arrange: () => {} },
    {
      name: 'a 401 from a hop in front, on /sync',
      o: { accessLive: true, dead: ['/refresh'] },
      arrange: (r) => (r.net.answerSync = () => new Response('access: session expired', { status: 401 })),
    },
    {
      name: 'a 502 from a hop in front',
      o: { accessLive: true, dead: ['/refresh'] },
      arrange: (r) => (r.net.answerSync = () => new Response('bad gateway', { status: 502 })),
    },
    { name: 'a network error on /sync', o: { accessLive: true, dead: ['/refresh', '/sync'] }, arrange: () => {} },
    {
      name: 'the corrected clock passing access_expires_at',
      o: { accessLive: true, dead: ['/refresh'], cred: (c: StoredCredential) => ({ ...c, accessExpiresAt: c.accessExpiresAt - HOUR }) },
      arrange: () => {},
    },
  ]
  for (const c of cases) {
    await tc.test(c.name, async () => {
      const r = credRig(c.o)
      r.net.refuseWs = () => true // the upgrade is refused in every case, so "it keeps dialing" is observable
      c.arrange(r)
      const { t } = r.transport()
      t.start()
      await advanceUntil(r.clock, () => t.status().stats.dials >= 3, 'the client to keep dialing', 250)
      t.stop()
      const s = t.status()
      assert.equal(s.tokenRefused, false, 'only Catenary’s own 401 on /sync marks a token')
      assert.equal(s.stats.dialsWithheld, 0)
      assert.equal(s.stats.syncsWithheld, 0)
      assert.equal(noReplays(r.net.family), null)
    })
  }
  await tc.test('the mark is keyed on the access token, so a new pair is presented at once', async () => {
    const r = credRig({ dead: ['/refresh'] })
    assert.equal(await fetchVia(r, r.c), 'unauthorized')
    const before = await r.held()
    assert.equal(await r.c.withholdDial(), true, 'the refused token is marked')
    r.net.dead.delete('/refresh')
    assert.equal(await r.c.attemptIfDue(), 'refreshed')
    const after = await r.held()
    assert.notEqual(after.accessToken, before.accessToken)
    assert.equal(await r.c.withholdDial(), false)
    assert.equal(r.c.status().chainLength, 0)
    assert.equal(r.c.refused(), false)
    assert.equal(await fetchVia(r, r.c), 'page')
    const last = r.net.on('/sync').at(-1)!
    assert.equal(last.token, after.accessToken)
  })
})

test('TestTheHoldsOneSyncIsRetriedUntilCatenaryAnswers', async (tc) => {
  await tc.test('nothing reached Catenary: retried on the ordinary backoff, and one refusal in the end', async () => {
    const { r, t } = running({ dead: ['/refresh'] })
    await advanceUntil(r.clock, () => t.status().tokenRefused && t.status().stats.chainLength >= 1, 'a hold in force', 250)
    r.net.dead.add('/sync')
    const refusals = r.net.refused['/sync']
    const syncs = r.net.count('/sync')
    const links = (await r.held()).chain.length
    await advanceUntil(r.clock, () => r.net.count('/sync') >= syncs + 3, 'the probe retried on the catch-up backoff', 250)
    assert.equal(r.net.refused['/sync'], refusals, 'nothing that never arrived is counted as refused')
    assert.equal((await r.held()).chain.length, links, 'no attempt without an answer from Catenary')
    r.net.dead.delete('/sync')
    const chain = () => t.status().stats.chainLength
    await advanceUntil(r.clock, () => chain() > links, 'the probe delivered, driving its attempt', 250)
    assert.equal(r.net.refused['/sync'] - refusals, 1)
    assert.equal(chain(), links + 1)
    t.stop()
  })
  await tc.test('delivered and refused, and the answer lost: the server counts a second, the client sees one', async () => {
    const { r, t } = running({ dead: ['/refresh'] })
    await advanceUntil(r.clock, () => t.status().tokenRefused && t.status().stats.chainLength >= 1, 'a hold in force', 250)
    const refusals = r.net.refused['/sync']
    const links = (await r.held()).chain.length
    r.net.loseSyncAnswers = 1
    await advanceUntil(r.clock, () => t.status().stats.chainLength > links, 'the lost answer, then the seen one, then its attempt', 250)
    assert.equal(r.net.refused['/sync'] - refusals, 2)
    assert.equal(t.status().stats.chainLength - links, 1)
    t.stop()
  })
})
