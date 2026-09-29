/* CANT-35 criteria 10 and 12: the pure decisions — the §1 threshold and due
 * check, CANT-127's delay, stamp, gate and hold, CANT-129's refused wait — and
 * the proposal's shape. Mirrors refresh_test.go's threshold and due tables and
 * hold_test.go's `TestTheDelayDoublesToTheCap`. CANT-156's shared vectors will
 * point at these same functions. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { decodeRefreshRequest } from '@/wire/generated'
import { gateOpen, nextRefreshAt, readStamp, refreshDelay, refreshHoldAt, REFRESH_BACKOFF_BASE_MS, REFRESH_BACKOFF_CAP_MS } from '../hold'
import { refusedHoldAt } from '../refused'
import { mintProposal, refreshDue, refreshThreshold, REFRESH_FLOOR_MS } from '../refresh'
import { cryptoRandom } from '../seams'
import { attemptsWithin, credRig, FIRST_REFRESH, noReplays } from './credential-harness'

const MIN = 60_000
const ISSUED = Date.UTC(2026, 8, 19, 12, 0, 0)

test('criterion 10 · the threshold is a third of the served lifetime, with a 60 s floor, and unrounded', () => {
  for (const [name, lifetime, want] of [
    ["a person's fifteen minutes", 15 * MIN, 5 * MIN],
    ['exactly at the floor', 3 * MIN, MIN],
    ['a short lifetime is held up by the floor', 90_000, MIN],
    ['an hour', 60 * MIN, 20 * MIN],
  ] as const) {
    assert.equal(refreshThreshold({ accessIssuedAt: ISSUED, accessExpiresAt: ISSUED + lifetime }), want, name)
  }
  assert.equal(refreshThreshold({ accessIssuedAt: null, accessExpiresAt: ISSUED + 15 * MIN }), REFRESH_FLOOR_MS, 'no issue time learned: the floor')
  // NOT ROUNDED TO WHOLE MILLISECONDS: Go's third of 200 s is 66.666666666 s.
  const third = refreshThreshold({ accessIssuedAt: ISSUED, accessExpiresAt: ISSUED + 200_000 })
  assert.equal(third, 200_000 / 3)
  assert.ok(!Number.isInteger(third))
  // At the boundary the fraction decides: 66,666.4 ms left is due, 66,667 is not.
  const c = { accessIssuedAt: ISSUED, accessExpiresAt: ISSUED + 200_000, clockOffsetMs: 0 }
  assert.equal(refreshDue(c, ISSUED + 200_000 - 66_666.4), true)
  assert.equal(refreshDue(c, ISSUED + 200_000 - 66_667), false)
})

test('criterion 10 · due is decided on the server’s clock, through the persisted offset', () => {
  const server = ISSUED
  // A pair served at `server`, learned on a device whose clock read server+skew.
  const pair = (skew: number) => ({ accessExpiresAt: server + 15 * MIN, accessIssuedAt: server, clockOffsetMs: -skew })
  for (const [name, skew, elapsed, want] of [
    ['a correct clock, fresh', 0, MIN, false],
    ['a correct clock, inside the last third', 0, 10 * MIN + 1000, true],
    ['an hour fast, fresh', 60 * MIN, MIN, false],
    ['an hour fast, inside the last third', 60 * MIN, 11 * MIN, true],
    ['an hour slow, inside the last third', -60 * MIN, 11 * MIN, true],
    ['an hour slow, fresh', -60 * MIN, MIN, false],
  ] as const) {
    assert.equal(refreshDue(pair(skew), server + skew + elapsed), want, name)
  }
})

test('TestTheDelayDoublesToTheCap', () => {
  assert.equal(refreshDelay(0), 0, 'a settled credential waits nothing')
  let want = REFRESH_BACKOFF_BASE_MS
  for (let links = 1; links <= 8; links++) {
    assert.equal(refreshDelay(links), want, `${links} links`)
    want = Math.min(want * 2, REFRESH_BACKOFF_CAP_MS)
  }
  for (const links of [9, 64, 1_000, 1_000_000]) assert.equal(refreshDelay(links), REFRESH_BACKOFF_CAP_MS, `${links} links`)
  let total = 0
  for (let links = 1; links <= 8; links++) total += refreshDelay(links)
  assert.ok(total >= 20 * MIN && total <= 22 * MIN, `the first eight links take ${total} ms, want about 21 minutes`)
  // refused_test.go's attemptsWithin: 11 in an hour, at 0, 5, 15, 35, … 3075 s.
  assert.equal(attemptsWithin(60 * MIN), 11)
})

test('the stamp, the gate and the hold: one rule for absent, and later-than is strict', () => {
  const now = ISSUED
  assert.equal(readStamp(null, now), null, 'missing')
  assert.equal(readStamp(now + 1, now), null, 'in the future')
  assert.equal(readStamp(now, now), now)

  assert.equal(gateOpen(null, now), false, 'a context that has heard nothing is closed')
  assert.equal(gateOpen(null, null), false, 'closed even over an absent stamp')
  assert.equal(gateOpen(now, null), true, 'an absent stamp is opened by any answer')
  assert.equal(gateOpen(now, now), false, 'equal is closed')
  assert.equal(gateOpen(now + 1, now), true)

  const at = { links: 1, lastSentAt: now, answeredAt: null as number | null, now: now + MIN }
  assert.equal(refreshHoldAt({ ...at, links: 0 }), 'none', 'settled')
  assert.equal(refreshHoldAt(at), 'unreachable')
  assert.equal(refreshHoldAt({ ...at, answeredAt: now + 1, now: now + 2 }), 'backoff')
  assert.equal(refreshHoldAt({ ...at, answeredAt: now + 1 }), 'none', 'answered, and past the delay')
  assert.equal(refreshHoldAt({ ...at, lastSentAt: now + 2 * MIN, answeredAt: now }), 'none', 'a future stamp is absent: open and elapsed')
  assert.equal(nextRefreshAt(0, now, now), null)
  assert.equal(nextRefreshAt(3, now, now), now + 20_000)
  assert.equal(nextRefreshAt(3, now + 1, now), null, 'a future stamp waits for nothing')
})

test('the refused wait: a predicate over the pair, the chain, the stamp and the clock', () => {
  const now = ISSUED
  const w = { refused: true, links: 2, lastSentAt: now, now: now + 9_999 }
  assert.equal(refusedHoldAt(w), true, 'inside 10 s of a two-link chain')
  assert.equal(refusedHoldAt({ ...w, now: now + 10_000 }), false, 'the deadline passed')
  assert.equal(refusedHoldAt({ ...w, refused: false }), false, 'the pair changed')
  assert.equal(refusedHoldAt({ ...w, links: 0 }), false, 'the chain collapsed')
  assert.equal(refusedHoldAt({ ...w, lastSentAt: null }), false, 'the stamp went absent')
  assert.equal(refusedHoldAt({ ...w, lastSentAt: now + 10 * MIN }), false, 'a future stamp is absent')
})

test('TestAProposalIsAWireToken', async () => {
  const seen = new Set<string>()
  for (let i = 0; i < 64; i++) {
    const p = mintProposal(cryptoRandom)
    assert.equal(p.length, 43)
    // The generated decoder, which holds a proposal to the server's own rule.
    decodeRefreshRequest({ refresh_token: FIRST_REFRESH, proposed_refresh_token: p })
    assert.ok(!seen.has(p), 'never the same twice')
    seen.add(p)
  }
  // A generator that cannot fill 32 bytes sends nothing and writes nothing.
  const r = credRig({ random: () => new Uint8Array(9) })
  assert.equal(await r.c.attemptIfDue(), 'failed')
  assert.equal(r.net.family.seen.length, 0)
  assert.equal(r.net.count('/refresh'), 0)
  assert.deepEqual((await r.held()).chain, [])
  assert.equal(noReplays(r.net.family), null)
})
