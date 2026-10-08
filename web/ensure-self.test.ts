/* `npm run test:ensure-self` — CANT-262, the web half of CANT-254 ruling 3 → B.
 *
 * The self conversation is ensured once, after the first completed catch-up,
 * and only when the journal holds none. Driven with a fake transport status and
 * a fake fetch, no server:
 *
 *   * the call is made exactly once, and not before the catch-up completes
 *   * it is made zero times when a self conversation is already held
 *   * a 404, an offline failure and a refusal raise nothing a person reads, and
 *     are not retried inside the launch
 *   * a 200 asks the transport to catch up, which is how the conversation
 *     arrives
 *   * the request is a bodiless POST to /conversations/self, bearing the
 *     session's own credential
 *
 * node:test over a vite-ssr bundle, chained into `npm run test:transport`.
 */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createEnsureSelf } from '@/ensure-self'
import { requestSelfConversation } from '@/account'
import { enrollCredential, MemoryCredentialStore, type StoredCredential } from '@/transport'
import { inProcessLock } from '@/transport/seams'

const BASE = 'http://ensure-self.test'
const flush = () => new Promise((r) => setTimeout(r, 0))

function harness(opts: { holds: boolean; answer: () => Promise<boolean> }) {
  const calls = { request: 0, afterCreated: 0 }
  const ensure = createEnsureSelf({
    holdsSelf: () => opts.holds,
    request: () => {
      calls.request++
      return opts.answer()
    },
    afterCreated: () => void calls.afterCreated++,
  })
  return { ensure, calls }
}

test('nothing is asked before the first catch-up completes, and one call is made after it', async () => {
  const h = harness({ holds: false, answer: async () => true })
  h.ensure.observe({ ready: false, caughtUp: false })
  h.ensure.observe({ ready: true, caughtUp: false })
  h.ensure.observe({ ready: false, caughtUp: true })
  await flush()
  assert.equal(h.calls.request, 0, 'no call while the catch-up has not completed')

  h.ensure.observe({ ready: true, caughtUp: true })
  h.ensure.observe({ ready: true, caughtUp: true })
  h.ensure.observe({ ready: true, caughtUp: false })
  h.ensure.observe({ ready: true, caughtUp: true })
  await flush()
  assert.equal(h.calls.request, 1, 'exactly one call however many statuses follow')
  assert.equal(h.calls.afterCreated, 1, 'a 200 asks the transport to catch up')
})

test('no call at all when a self conversation is already held', async () => {
  const h = harness({ holds: true, answer: async () => true })
  h.ensure.observe({ ready: true, caughtUp: true })
  h.ensure.observe({ ready: true, caughtUp: true })
  await flush()
  assert.equal(h.calls.request, 0)
  assert.equal(h.calls.afterCreated, 0)
})

test('a 404, an offline failure and a refusal are quiet and are not retried in the launch', async () => {
  for (const [name, answer] of [
    ['a server that predates the route', async () => { throw new Error('self conversation: HTTP 404') }],
    ['offline', async () => { throw new TypeError('Failed to fetch') }],
    ['a refusal', async () => false],
  ] as const) {
    const h = harness({ holds: false, answer })
    const unhandled: unknown[] = []
    const onUnhandled = (e: unknown) => void unhandled.push(e)
    process.on('unhandledRejection', onUnhandled)
    try {
      h.ensure.observe({ ready: true, caughtUp: true })
      await flush()
      h.ensure.observe({ ready: true, caughtUp: false })
      h.ensure.observe({ ready: true, caughtUp: true })
      await flush()
    } finally {
      process.off('unhandledRejection', onUnhandled)
    }
    assert.equal(h.calls.request, 1, `${name}: one attempt, no retry inside the launch`)
    assert.equal(h.calls.afterCreated, 0, `${name}: nothing was created`)
    assert.deepEqual(unhandled, [], `${name}: nothing escapes to the page`)
  }
})

/* ── the request itself ───────────────────────────────────────────────── */

const uuid = (n: number): string => `00000000-0000-4000-8000-${n.toString(16).padStart(12, '0')}`
const padToken = (name: string): string => name + '_'.repeat(43 - name.length)

async function enrolledStore(): Promise<MemoryCredentialStore> {
  const store = new MemoryCredentialStore()
  const now = Date.now()
  const stored: StoredCredential = {
    userId: uuid(1),
    deviceId: uuid(7),
    accessToken: padToken('access-7'),
    accessExpiresAt: now + 3_600_000,
    refreshToken: padToken('refresh-7'),
    refreshExpiresAt: now + 86_400_000,
    accessIssuedAt: now,
    clockOffsetMs: 0,
    chain: [],
    lastSentAt: null,
  }
  await enrollCredential(store, inProcessLock(), stored)
  return store
}

test('the request is a bodiless POST to /conversations/self with the session\'s bearer', async () => {
  const store = await enrolledStore()
  const seen: { url: string; method?: string; auth?: string; body?: unknown }[] = []
  const fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    seen.push({
      url: String(input),
      method: init?.method,
      auth: (init?.headers as Record<string, string> | undefined)?.Authorization,
      body: init?.body,
    })
    return new Response('{}', { status: 200 })
  }) as typeof globalThis.fetch

  assert.equal(await requestSelfConversation({ baseUrl: BASE, store, fetch, lock: inProcessLock() }), true)
  assert.equal(seen.length, 1)
  assert.equal(seen[0].url, `${BASE}/conversations/self`)
  assert.equal(seen[0].method, 'POST')
  assert.equal(seen[0].body, undefined, 'no request body')
  assert.equal(seen[0].auth, `Bearer ${padToken('access-7')}`)
})

test('a 404 rejects, which the ensure-once swallows', async () => {
  const store = await enrolledStore()
  const fetch = (async () => new Response('not found', { status: 404 })) as typeof globalThis.fetch
  await assert.rejects(requestSelfConversation({ baseUrl: BASE, store, fetch, lock: inProcessLock() }), /HTTP 404/)
})

test('an offline failure rejects', async () => {
  const store = await enrolledStore()
  const fetch = (async () => { throw new TypeError('Failed to fetch') }) as typeof globalThis.fetch
  await assert.rejects(requestSelfConversation({ baseUrl: BASE, store, fetch, lock: inProcessLock() }), TypeError)
})
