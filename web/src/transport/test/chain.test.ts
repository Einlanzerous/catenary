/* CANT-35 criterion 12, the chain half: record §3, the client half of the
 * proposed-successor exchange, against a family that keeps its tokens the way
 * the real server does and loses requests and responses on instruction. Each
 * test is named after the internal/client/chain_test.go case it ports. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MemoryCredentialStore } from '../credential-store'
import {
  BARE_503, credRig, FIRST_REFRESH, LOSE_REQUEST, LOSE_RESPONSE, noReplays, PARK, PROXY_401, SAY_FRESH, SAY_PRESENT,
  type CredRig,
} from './credential-harness'
import type { RefreshingCredential } from '../refresh'

/** Go's `refreshUntilAnswered`: explicit refreshes until one is answered;
 *  returns how many settled nothing first. A terminal fails the test. */
async function refreshUntilAnswered(c: RefreshingCredential, atMost: number): Promise<number> {
  let unknown = 0
  for (let i = 0; i < atMost; i++) {
    const out = await c.attemptIfDue()
    if (out === 'refreshed') return unknown
    assert.notEqual(out, 'terminal', `attempt ${unknown + 1} went terminal`)
    unknown++
  }
  assert.fail(`no refresh was answered in ${atMost} attempts`)
}

async function assertSettledOnLive(r: CredRig) {
  const held = await r.held()
  assert.equal(held.refreshToken, r.net.family.live(), 'the client holds the server’s live token')
  assert.deepEqual(held.chain, [], 'the chain is empty after an answer')
  assert.equal(held.lastSentAt, null, 'and the stamp went with it')
}

test('TestTheChainIsPersistedBeforeTheRequestLeaves', async () => {
  const r = credRig()
  let atArrival: unknown = null
  const store = r.store as MemoryCredentialStore
  r.net.family.onRefresh = () => {
    // Synchronously, as the request arrives: what the store holds NOW.
    void store.read().then((h) => (atArrival = h?.chain))
  }
  assert.equal(await r.c.attemptIfDue(), 'refreshed')
  const seen = r.net.family.seen
  assert.equal(seen.length, 1)
  assert.notEqual(seen[0].proposal, '')
  assert.deepEqual(atArrival, [{ token: FIRST_REFRESH, proposal: seen[0].proposal }])
  const held = await r.held()
  assert.deepEqual(held.chain, [], 'an answer collapses it')
  assert.equal(held.refreshToken, seen[0].proposal, 'the proposal the server adopted')
})

test('TestAServerThatIgnoresTheProposalIsBelieved', async () => {
  const r = credRig()
  r.net.family.ignoreProposals = true
  assert.equal(await r.c.attemptIfDue(), 'refreshed')
  const held = await r.held()
  assert.notEqual(held.refreshToken, r.net.family.seen[0].proposal, 'not its own proposal on faith')
  await assertSettledOnLive(r)
})

test('TestAnUnknownOutcomeIsRecoveredByWalkingTheChain', async (t) => {
  const cases: {
    name: string
    script: string[]
    unknown: number
    walkBacks: number
    four01s: number
    check?: (seen: { token: string; proposal: string }[]) => void
  }[] = [
    {
      name: 'one lost response: the newest token is the live one, and there is no 401',
      script: [LOSE_RESPONSE], unknown: 1, walkBacks: 0, four01s: 0,
      check: (seen) => {
        assert.equal(seen.length, 2)
        assert.equal(seen[1].token, seen[0].proposal, 'the lost rotation’s proposal is presented next')
      },
    },
    {
      name: 'two lost responses, the second during recovery: still no 401',
      script: [LOSE_RESPONSE, LOSE_RESPONSE], unknown: 2, walkBacks: 0, four01s: 0,
      check: (seen) => {
        assert.equal(seen.length, 3)
        assert.equal(seen[1].token, seen[0].proposal)
        assert.equal(seen[2].token, seen[1].proposal)
      },
    },
    {
      name: 'the first REQUEST never arrived: the newest token does not exist, the 401 is not terminal, and the walk steps back',
      script: [LOSE_REQUEST], unknown: 1, walkBacks: 1, four01s: 1,
      check: (seen) => {
        assert.equal(seen.length, 2)
        assert.equal(seen[1].token, FIRST_REFRESH)
        assert.equal(seen[1].proposal, seen[0].token, 'refresh-0 re-presented with its ORIGINAL proposal')
      },
    },
    {
      name: 'a response lost, then a request lost during recovery: one step back, original proposal reused',
      script: [LOSE_RESPONSE, LOSE_REQUEST], unknown: 2, walkBacks: 1, four01s: 1,
      check: (seen) => {
        assert.equal(seen.length, 3)
        assert.equal(seen[2].token, seen[0].proposal)
        assert.equal(seen[2].proposal, seen[1].token)
      },
    },
    { name: 'a 503 that names no `retry` is an unknown outcome, not an answer', script: [BARE_503], unknown: 1, walkBacks: 1, four01s: 1 },
  ]
  for (const tc of cases) {
    await t.test(tc.name, async () => {
      const r = credRig({ script: tc.script })
      assert.equal(await refreshUntilAnswered(r.c, 6), tc.unknown)
      assert.equal(noReplays(r.net.family), null)
      assert.equal(r.net.family.statuses.filter((s) => s === 401).length, tc.four01s)
      const s = r.c.status()
      assert.equal(s.refreshWalkBacks, tc.walkBacks)
      assert.equal(s.refreshes, 1)
      await assertSettledOnLive(r)
      tc.check?.(r.net.family.seen)
    })
  }
})

test('TestWithoutTheChainOneLostResponseCostsTheDevice (negative control noChain)', async () => {
  const r = credRig({ script: [LOSE_RESPONSE], faults: { noChain: true } })
  let terminal = ''
  r.c.attach({ terminal: (x) => (terminal = x.kind), notify() {} })
  assert.equal(await r.c.attemptIfDue(), 'failed', 'the lost response was noticed')
  assert.equal(await r.c.attemptIfDue(), 'terminal', 'the spent token was presented, and the device is gone')
  assert.equal(terminal, 'credential')
  assert.equal(r.net.family.replays, 1)
  assert.equal(r.net.family.revoked, true)
})

test('TestARevokedDeviceWalksToTheOldestTokenAndStops', async () => {
  const r = credRig({ script: [LOSE_RESPONSE, LOSE_RESPONSE] })
  let terminal = ''
  r.c.attach({ terminal: (x) => (terminal = x.kind), notify() {} })
  for (let i = 0; i < 2; i++) assert.equal(await r.c.attemptIfDue(), 'failed')
  r.net.family.revoked = true
  assert.equal(await r.c.attemptIfDue(), 'terminal')
  assert.equal(terminal, 'credential')
  assert.equal(r.c.status().refreshWalkBacks, 2, 'three links: the newest, then one step back per 401')
  const seen = r.net.family.seen
  assert.equal(seen[seen.length - 1].token, FIRST_REFRESH, 'the terminal 401 was for the oldest token')
  const held = await r.held()
  assert.equal(held.refreshToken, FIRST_REFRESH, 'terminal changed nothing stored')
  assert.equal(held.chain.length, 3, 'and kept every link')
})

test('TestAProxys401DoesNotStepTheWalkBack', async () => {
  const r = credRig({ script: [LOSE_RESPONSE, PROXY_401] })
  for (let i = 0; i < 2; i++) assert.equal(await r.c.attemptIfDue(), 'failed', `attempt ${i + 1} settled nothing`)
  assert.equal(r.net.family.seen.length, 2, 'one request per attempt')
  assert.equal(r.c.status().refreshWalkBacks, 0, 'the proxy’s 401 did not step the walk')
  assert.equal(noReplays(r.net.family), null)
  await refreshUntilAnswered(r.c, 2)
  assert.equal(noReplays(r.net.family), null)
  await assertSettledOnLive(r)
})

test('TestPresentProposalMovesTheWalkForward', async () => {
  const r = credRig({ script: [SAY_PRESENT] })
  let once = false
  r.net.family.onRefresh = (p) => {
    if (once) return
    once = true
    // What the stub pretends: refresh-0 was already rotated into this proposal.
    r.net.family.successor.set(p.token, p.proposal)
    r.net.family.successor.set(p.proposal, '')
  }
  assert.equal(await r.c.attemptIfDue(), 'refreshed')
  const seen = r.net.family.seen
  assert.equal(seen.length, 2)
  assert.equal(seen[1].token, seen[0].proposal, 'the proposal, presented straight after the 503')
  assert.equal(r.net.family.replays, 0)
})

test('TestFreshProposalMintsAgainForTheSameToken', async (t) => {
  await t.test('a generator that repeats itself once', async () => {
    // The first draw is 32 zero bytes, which collides with a stored token.
    let first = true
    let n = 0
    const r = credRig({
      random: (len) => {
        if (first) {
          first = false
          return new Uint8Array(len)
        }
        const b = new Uint8Array(len)
        b[len - 1] = ++n
        return b
      },
    })
    const other = 'A'.repeat(43)
    r.net.family.successor.set(other, 'some-later-token')
    assert.equal(await r.c.attemptIfDue(), 'refreshed')
    const seen = r.net.family.seen
    assert.equal(seen.length, 2)
    assert.equal(seen[0].proposal, other)
    assert.equal(seen[1].token, FIRST_REFRESH, 'the same token')
    assert.notEqual(seen[1].proposal, other, 'with a NEW proposal')
    assert.equal(r.net.family.replays, 0)
  })
  await t.test('a server that says it to everything is given up on, and not terminally', async () => {
    const r = credRig({ script: Array(6).fill(SAY_FRESH) })
    assert.equal(await r.c.attemptIfDue(), 'failed')
    assert.equal(r.net.family.seen.length, 3 + 1, 'the first, and three more')
    const held = await r.held()
    assert.equal(held.chain.length, 1, 'one link, rewritten each time')
    assert.equal(held.chain[0].token, FIRST_REFRESH)
  })
})

test('TestAClientKilledMidRefreshRecoversOnRestart', async (t) => {
  for (const [name, fate, walkBacks] of [
    ['the rotation committed', LOSE_RESPONSE, 0],
    ['the request never arrived', LOSE_REQUEST, 1],
  ] as const) {
    await t.test(name, async () => {
      const r = credRig({ script: [fate] })
      let once = false
      r.net.family.onRefresh = () => {
        if (!once) r.c.kill()
        once = true
      }
      assert.notEqual(await r.c.attemptIfDue(), 'refreshed', 'the killed client’s refresh did not succeed')
      assert.equal((await r.held()).chain.length, 1, 'the killed client left its one proposal')

      // The relaunch: a new context over the same store.
      const second = r.layer()
      assert.equal(await second.attemptIfDue(), 'refreshed')
      assert.equal(noReplays(r.net.family), null)
      assert.equal(second.status().refreshWalkBacks, walkBacks)
      assert.equal((await r.held()).refreshToken, r.net.family.live())
    })
  }
})

test('TestAKilledClientWritesNoProposal', async () => {
  const r = credRig()
  r.c.kill()
  assert.equal(await r.c.attemptIfDue(), 'killed')
  assert.equal(r.net.family.seen.length, 0)
  assert.deepEqual((await r.held()).chain, [])
})

/**
 * cmd/catenary's `TestADelayedOriginalCollidesWithItsOwnRetry` and its control
 * `TestWithASecondProposalTheDelayedOriginalCostsTheDevice`, against a family
 * that routes a collision as CANT-125's server does. The first request is held
 * in the network; the recovery's walk back re-presents refresh-0, and the held
 * original lands just before it. REUSING THE ORIGINAL PROPOSAL makes the two
 * collide and the loser is told `present_proposal`; a second proposal
 * (`proposeAfresh`) makes the loser a spent token, and the device is gone.
 */
test('the delayed original collides with its own retry — and negative control proposeAfresh', async (t) => {
  const race = (faults = {}) => {
    const r = credRig({ script: [PARK], faults })
    r.net.family.routeCollisions = true
    r.net.family.onRefresh = (p) => {
      if (p.token === FIRST_REFRESH && r.net.family.seen.length > 0) r.net.family.unpark()
    }
    return r
  }
  await t.test('the correct client: one family, never forked', async () => {
    const r = race()
    assert.equal(await r.c.attemptIfDue(), 'failed', 'the parked request was not answered')
    assert.equal(await r.c.attemptIfDue(), 'refreshed')
    // (P0,P1) 401, P0 does not exist yet · the parked (r0,P0) lands and commits ·
    // the retry (r0,P0) 503, the original just won · (P0,P1) 200.
    assert.deepEqual(r.net.family.statuses, [401, 200, 503, 200])
    const [walk, original, retry, last] = r.net.family.seen
    assert.equal(original.token, FIRST_REFRESH)
    assert.deepEqual(retry, original, 'the retry carried the original’s proposal')
    assert.equal(last.token, original.proposal, 'then the proposal, presented as the newest token')
    assert.equal(last.proposal, walk.proposal, 'with ITS original proposal')
    assert.equal(noReplays(r.net.family), null)
    await assertSettledOnLive(r)
  })
  await t.test('proposeAfresh: the retry is a spent token, and the device is gone', async () => {
    const r = race({ proposeAfresh: true })
    let terminal = ''
    r.c.attach({ terminal: (x) => (terminal = x.kind), notify() {} })
    assert.equal(await r.c.attemptIfDue(), 'failed')
    assert.equal(await r.c.attemptIfDue(), 'terminal')
    assert.equal(terminal, 'credential')
    assert.equal(r.net.family.replays, 1)
  })
})
