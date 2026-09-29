/* CANT-35 criterion 10: record §1's two halves under a running transport and
 * against the refresh response's `Date`. Each test is named after the
 * internal/client/refresh_test.go case it ports. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createRefreshingCredential } from '../refresh'
import { flush } from './harness'
import { credRig, FIRST_ACCESS, padToken, type CredRig } from './credential-harness'

const HOUR = 3600_000

async function setHeld(r: CredRig, patch: Record<string, unknown>) {
  const held = await r.held()
  await r.store.update(() => ({ write: { ...held, ...patch }, result: undefined }))
}

test('TestADuePairIsRotatedBeforeTheDialAndTheSyncBesideIt', async () => {
  const r = credRig({ accessLive: true })
  const { t } = r.transport()
  t.start()
  await flush()
  const order = r.net.tape.map((q) => q.path)
  assert.deepEqual(order.slice(0, 1), ['/refresh'], 'the proactive refresh goes first')
  for (const q of r.net.tape.filter((x) => x.path !== '/refresh')) assert.equal(q.token, padToken('access-1'), `${q.path} carries the rotated pair`)
  assert.ok(t.status().ready)
  t.stop()
})

test('TestNoRefreshWhenNotDueOrNotEnabled', async () => {
  const r = credRig({ accessLive: true })
  await setHeld(r, { accessExpiresAt: r.clock.now() + HOUR })
  const { t } = r.transport()
  t.start()
  await flush()
  assert.equal(r.net.count('/refresh'), 0, 'not due: nothing')
  t.stop()

  const off = credRig({ accessLive: true })
  const disabled = createRefreshingCredential({
    baseUrl: 'http://catenary.test', store: off.store, lock: off.lock, fetch: off.net.fetch, now: off.clock.now, timers: off.clock,
    logger: off.logger, refresh: false,
  })
  assert.equal(await disabled.attemptIfDue(), 'not_due', 'Refresh off: nothing, however due')
  assert.equal(await disabled.onSyncUnauthorized({ userId: '', deviceId: '', accessToken: FIRST_ACCESS }), false)
  assert.equal(disabled.refused(), false, 'and the refused-token rule is inert')
  assert.equal(off.net.count('/refresh'), 0)
})

test('TestAStaleSyncIsRecoveredInOneRoundTrip · a Catenary 401 on /sync: one refresh, then exactly one retry', async () => {
  const r = credRig({ accessLive: true })
  await setHeld(r, { accessExpiresAt: r.clock.now() + HOUR }) // the clock thinks it is live
  r.net.first = '' // the server does not
  const { t } = r.transport()
  t.start()
  await flush()
  const syncs = r.net.on('/sync')
  assert.equal(r.net.count('/refresh'), 1)
  assert.equal(syncs.length, 2, 'the refused page and its one retry')
  assert.equal(syncs[0].token, FIRST_ACCESS)
  assert.equal(syncs[1].token, padToken('access-1'))
  assert.ok(t.status().caughtUp)
  t.stop()
})

test('TestASecond401IsNotAnsweredWithASecondRefresh', async () => {
  const r = credRig({ accessLive: true })
  await setHeld(r, { accessExpiresAt: r.clock.now() + HOUR })
  r.net.refuseSync = () => true // even the freshly minted pair is refused
  const { t } = r.transport()
  t.start()
  await flush()
  assert.equal(r.net.count('/refresh'), 1, 'one refresh')
  assert.equal(r.net.count('/sync'), 2, 'one retry, not a loop')
  t.stop()
})

test('TestAProxys401DoesNotTriggerARefresh', async () => {
  const r = credRig({ accessLive: true })
  await setHeld(r, { accessExpiresAt: r.clock.now() + HOUR })
  r.net.answerSync = () => new Response('access: session expired', { status: 401 })
  const { t } = r.transport()
  t.start()
  await flush()
  assert.equal(r.net.count('/refresh'), 0, 'a 401 without Catenary’s body refreshes nothing')
  assert.equal(r.net.count('/sync'), 1, 'and is not retried as one')
  assert.equal(t.status().tokenRefused, false)
  t.stop()
})

test('TestAColdStartReadsItsExpiryThroughThePersistedOffset · TestARefreshWithNoDateKeepsTheOffset', async () => {
  // The device clock is an hour slow. /refresh's Date records that.
  const r = credRig()
  const slow = HOUR
  r.clock.t -= slow
  const server = r.clock.now() + slow
  await setHeld(r, { accessExpiresAt: r.clock.now() + 10_000 })
  const fetch = r.net.fetch
  // The server's answer, on the server's clock: its Date, and an expiry
  // fifteen minutes past it.
  const withDate = (date: 'server' | null) =>
    (async (input: string | URL | Request, init?: RequestInit) => {
      const res = await fetch(input, init)
      const serverNow = r.clock.now() + slow
      const headers = new Headers(res.headers)
      if (date === null) headers.delete('Date')
      else headers.set('Date', new Date(serverNow).toUTCString())
      const body = JSON.parse(await res.text()) as Record<string, string>
      body.access_expires_at = new Date(serverNow + 15 * 60_000).toISOString()
      return new Response(JSON.stringify(body), { status: res.status, headers })
    }) as typeof globalThis.fetch
  const layer = (f: typeof globalThis.fetch) =>
    createRefreshingCredential({ baseUrl: 'http://catenary.test', store: r.store, lock: r.lock, fetch: f, now: r.clock.now, timers: r.clock, logger: r.logger })
  assert.equal(await layer(withDate('server')).attemptIfDue(), 'refreshed')
  const a = await r.held()
  assert.equal(a.clockOffsetMs, slow, 'server minus device, from the Date')
  assert.equal(a.accessIssuedAt, server)

  // A cold start an hour-slow device later: 11 minutes into a 15-minute pair,
  // the raw clock says 71 minutes are left; through the offset it is due.
  r.clock.t += 11 * 60_000
  // No Date on this one: the offset is the device's, and is kept.
  const noDate = layer(withDate(null))
  assert.equal(await noDate.attemptIfDue(), 'refreshed', 'due through the persisted offset')
  const b = await r.held()
  assert.equal(b.clockOffsetMs, slow, 'a response with no Date keeps the offset')
  assert.equal(b.accessIssuedAt, r.clock.now() + slow, 'and the issue time is the corrected arrival')
})
