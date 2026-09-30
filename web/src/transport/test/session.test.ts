/* CANT-35 criteria 1, 2, 7, 15 and 16: the dial, frames before `ready`, the
 * heartbeat, the seam CANT-36 adapts to, and the status. Mirrors
 * internal/client/client_test.go. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { WIRE_VERSION, type ClientSend, type ServerError } from '@/wire/generated'
import { NotConnected, SendInFlight, SendRefused, SessionEnded, type SessionEnd } from '../transport'
import type { TransportStatus } from '../status'
import {
  bootstrapPage, CONV, conversation, deferred, DEVICE, flush, message, messageFrame, OTHER, ready, rig, TOKEN, user,
  uuid,
} from './harness'
import type { SyncResponse } from '@/wire/generated'

const sendFrame = (n: number): ClientSend => ({ type: 'send', clientId: uuid(5000 + n), conversationId: CONV, text: `hi ${n}` })

test('criterion 1 · the dial: subprotocols, the hello, and a /sync before ready on every dial', async () => {
  const r = rig()
  const first = deferred<SyncResponse>()
  r.sync.answer = (req) => (r.sync.requests.length === 1 ? first.promise : bootstrapPage(Math.max(req.after, 7)))
  r.t.start()
  await flush()
  const s = r.net.last
  assert.equal(s.url, 'ws://catenary.test/ws')
  assert.deepEqual(s.protocols, ['catenary.v1', `catenary.token.${TOKEN}`])
  assert.equal(r.sync.requests.length, 1, 'the /sync went out beside the upgrade, before any ready')
  assert.equal(r.sync.requests[0].authorization, `Bearer ${TOKEN}`)
  assert.equal(r.t.status().stats.syncsBeforeReady, 1)

  // No cursor held yet: the hello carries none.
  s.open()
  const hello = s.frames()[0]
  assert.deepEqual(hello, {
    type: 'hello', wireVersion: WIRE_VERSION, deviceId: DEVICE, resumeFromLogSeq: undefined,
    clientInfo: 'catenary-web/0.0.0-test',
  })
  s.frame(ready())
  first.resolve(bootstrapPage(7))
  await flush()
  assert.equal(r.t.status().cursor, 7)

  // The second dial resumes from the cursor, and pulls its own /sync before ready.
  const before = r.t.status().stats.syncsBeforeReady
  s.serverClose(1001)
  await r.clock.advance(250)
  const s2 = r.net.last
  assert.notEqual(s2, s)
  assert.equal(r.t.status().stats.syncsBeforeReady, before + 1, 'syncsBeforeReady rises per dial')
  s2.open()
  const hello2 = s2.frames()[0]
  assert.equal(hello2.type === 'hello' && hello2.resumeFromLogSeq, 7)
})

test('criterion 2 · frames before ready are applied like any live frame and close nothing', async () => {
  const r = rig()
  r.sync.answer = (req) => (req.after === 0 ? bootstrapPage(3, [message(1), message(2), message(3)]) : bootstrapPage(req.after))
  r.t.start()
  await flush()
  const s = r.net.last
  s.open()
  await flush()
  assert.equal(r.t.status().cursor, 3)
  // Every frame type, before ready.
  s.frame(messageFrame(message(4)))
  s.frame({ type: 'conversation', conversation: conversation(uuid(101)) })
  s.frame({ type: 'user', user: user(uuid(201)) })
  s.frame({ type: 'receipt', conversationId: CONV, userId: OTHER, upToSeq: 2 })
  s.frame({ type: 'typing', conversationId: CONV, userIds: [OTHER] })
  s.frame({ type: 'ping', id: 'server-1' })
  s.frame({ type: 'pong', id: 'nobody-asked' })
  s.frame({ type: 'resync_required', reason: 'cursor_too_old', logSeq: 4 })
  s.frame({ type: 'ack', clientId: uuid(1), messageId: uuid(2), conversationId: CONV, seq: 9, logSeq: 9, at: '2026-09-29T12:00:00.000Z' })
  s.frame({ type: 'error', code: 'rate_limited', message: 'slow', retryable: true, clientId: uuid(1) })
  s.raw(JSON.stringify({ type: 'something_new' }))
  await flush()
  const snap = r.t.snapshot()
  assert.ok(snap.messages.some((m) => m.id === message(4).id), 'the pre-ready message is held')
  assert.equal(r.t.status().cursor, 3, 'and it moved no cursor')
  assert.equal(s.closedByClient, null, 'nothing before ready closed the socket')
  assert.equal(r.t.status().connected, true)
  assert.equal(r.t.status().terminal.kind, 'none')
  s.frame(ready())
  await flush()
  assert.equal(r.t.status().ready, true)
})

test('criterion 7 · the heartbeat: from ready, at the announced interval, matched by id, severed at the limit', async () => {
  const r = rig()
  r.sync.answer = () => bootstrapPage()
  r.t.start()
  await flush()
  const s = r.net.last
  s.open()
  await r.clock.advance(100_000)
  assert.equal(s.frames().filter((f) => f.type === 'ping').length, 0, 'no ping before ready')

  // An interval that is not the default, so nothing here can be a constant.
  s.frame(ready({ heartbeatIntervalSec: 12, missedPongLimit: 3 }))
  await flush()
  await r.clock.advance(12_000)
  let pings = s.frames().filter((f) => f.type === 'ping')
  assert.equal(pings.length, 1, 'one ping at the first interval')
  s.frame({ type: 'pong', id: pings[0].type === 'ping' ? pings[0].id : '' })
  await flush()
  assert.equal(r.t.status().stats.pongsReceived, 1)

  // Unanswered from here: pings 2, 3 and 4 go out, and the tick after the
  // third outstanding one severs.
  await r.clock.advance(12_000 * 3)
  pings = s.frames().filter((f) => f.type === 'ping')
  assert.equal(pings.length, 4)
  assert.equal(new Set(pings.map((p) => (p.type === 'ping' ? p.id : ''))).size, 4, 'every id is unique')
  assert.equal(r.t.status().stats.heartbeatSevers, 0)
  await r.clock.advance(12_000)
  assert.equal(r.t.status().stats.heartbeatSevers, 1, 'severed once missed_pong_limit pings were outstanding')
  assert.equal(r.t.status().connected, false)
  assert.equal(r.t.status().stats.closeStatuses[-1], 1, 'a sever is a close with no close frame')
  assert.equal(r.t.status().stats.pingsSent, 4)
})

test('criterion 7 · a server ping is answered with pong{id}', async () => {
  const r = rig()
  r.sync.answer = () => bootstrapPage()
  r.t.start()
  const s = await r.connect()
  s.frame({ type: 'ping', id: 'from-the-server' })
  await flush()
  assert.deepEqual(s.frames().filter((f) => f.type === 'pong'), [{ type: 'pong', id: 'from-the-server', at: undefined }])
})

test('criterion 15 · send: its four refusals, before ready included, and client_id never touched', async () => {
  const r = rig()
  r.sync.answer = () => bootstrapPage()
  r.t.start()
  await flush()
  await assert.rejects(r.t.send(sendFrame(1)), NotConnected, 'no socket yet')

  const s = r.net.last
  s.open()
  // After the socket opens and BEFORE ready: written, and resolves on its ack.
  const p1 = r.t.send(sendFrame(1))
  await assert.rejects(r.t.send(sendFrame(1)), SendInFlight, 'the same client_id while it awaits its answer')
  const written = s.frames().filter((f) => f.type === 'send')
  assert.deepEqual(written, [{ ...sendFrame(1), attachments: undefined, replyToMessageId: undefined }], 'the frame as the caller built it')
  const ack = {
    type: 'ack' as const, clientId: sendFrame(1).clientId, messageId: uuid(6001), conversationId: CONV, seq: 4, logSeq: 40,
    at: '2026-09-29T12:00:00.000Z',
  }
  s.frame(ack)
  assert.deepEqual(await p1, { ...ack, duplicate: undefined })

  s.frame(ready())
  const p2 = r.t.send(sendFrame(2))
  const refusal: ServerError = { type: 'error', code: 'not_a_member', message: 'no', retryable: false, clientId: sendFrame(2).clientId }
  s.frame(refusal)
  const e2 = await p2.then(() => null, (e: unknown) => e)
  assert.ok(e2 instanceof SendRefused)
  assert.equal(e2.frame.code, 'not_a_member')

  const p3 = r.t.send(sendFrame(3))
  s.serverClose(1006)
  await assert.rejects(p3, SessionEnded, 'the socket closed first: outcome unknown, retry with the same client_id')
})

test('criterion 15 · SessionEnd: bare1008 exactly when the 1008 was bare', async () => {
  const run = async (preceded: boolean) => {
    const r = rig()
    r.sync.answer = () => bootstrapPage()
    const ends: SessionEnd[] = []
    r.t.onSessionEnd((e) => ends.push(e))
    r.t.start()
    const s = await r.connect()
    if (preceded) s.frame({ type: 'error', code: 'rate_limited', message: 'slow down', retryable: true })
    s.serverClose(1008)
    await flush()
    return ends
  }
  const bare = await run(false)
  assert.equal(bare.length, 1)
  assert.deepEqual(
    { opened: bare[0].opened, readied: bare[0].readied, closeCode: bare[0].closeCode, bare1008: bare[0].bare1008, preceding: bare[0].preceding },
    { opened: true, readied: true, closeCode: 1008, bare1008: true, preceding: null },
  )
  const framed = await run(true)
  assert.equal(framed[0].bare1008, false)
  assert.equal(framed[0].preceding?.code, 'rate_limited')
  assert.equal(framed[0].verdict, 'reconnect')
})

test('criterion 15 · ready is announced by subscribe(), and a session end only by onSessionEnd', async () => {
  const r = rig()
  r.sync.answer = () => bootstrapPage()
  const readies: boolean[] = []
  const ends: SessionEnd[] = []
  r.t.subscribe((s) => {
    if (readies[readies.length - 1] !== s.ready) readies.push(s.ready)
  })
  r.t.onSessionEnd((e) => ends.push(e))
  r.t.start()
  await flush()
  assert.deepEqual(readies, [false])
  const s = await r.connect()
  assert.deepEqual(readies, [false, true])
  assert.equal(ends.length, 0, 'becoming ready is not a session end')
  s.serverClose(1001)
  await flush()
  assert.deepEqual(readies, [false, true, false])
  assert.equal(ends.length, 1)
  const gen = ends[0].sessionGen
  await r.clock.advance(250)
  r.net.last.fail()
  await flush()
  assert.equal(ends.length, 2)
  assert.ok(ends[1].sessionGen > gen, 'sessionGen rises per dial')
  assert.equal(ends[1].opened, false, 'a pure dial failure')
})

test('criterion 16 · stats carry Go\'s client.Stats names, camelCased', () => {
  const go = readFileSync(resolve(process.cwd(), '..', 'internal', 'client', 'client.go'), 'utf8')
  const body = go.slice(go.indexOf('type Stats struct {'), go.indexOf('\n}\n', go.indexOf('type Stats struct {')))
  const goNames = [...body.matchAll(/^\t([A-Z][A-Za-z]*(?:, [A-Z][A-Za-z]*)*)\s+[\w.[\]]+/gm)]
    .flatMap((m) => m[1].split(', '))
  assert.ok(goNames.length > 20, `parsed ${goNames.length} fields from client.go`)
  const renamed: Record<string, string> = { LastRTT: 'lastRttMs' }
  const want = goNames.map((n) => renamed[n] ?? n[0].toLowerCase() + n.slice(1)).sort()
  const r = rig()
  const have = Object.keys(r.t.status().stats).sort()
  assert.deepEqual(have, want)
})

test('criterion 16 · no token appears in any status field or log line', async () => {
  const r = rig()
  let n = 0
  r.sync.answer = (req) => {
    n++
    if (n === 2) return { status: 401, body: '{"code":"unauthorized"}' }
    if (n === 3) return { status: 502, body: 'bad gateway' }
    return bootstrapPage(req.after)
  }
  const statuses: TransportStatus[] = []
  r.t.subscribe((s) => statuses.push(s))
  r.t.start()
  const s = await r.connect()
  s.frame({ type: 'error', code: 'internal', message: 'boom', retryable: true })
  s.raw('not json')
  s.raw(new ArrayBuffer(2))
  s.serverClose(1008)
  await r.clock.advance(6_000)
  r.net.last.fail()
  await r.clock.advance(6_000)
  const s3 = await r.connect()
  s3.serverClose(4001)
  await flush()
  assert.ok(r.logs.length > 3, 'something was logged')
  const everything = JSON.stringify(statuses) + JSON.stringify(r.logs) + JSON.stringify(r.t.status())
  assert.ok(!everything.includes(TOKEN), 'the access token appears nowhere')
  assert.ok(!everything.includes(TOKEN.slice(0, 20)), 'nor any prefix of it')
})

test('criterion 16 · closeStatuses keys a browser 1006 under -1, and a dial failure not at all', async () => {
  const r = rig()
  r.sync.answer = () => bootstrapPage()
  r.t.start()
  const s = await r.connect()
  s.serverClose(1006)
  await r.clock.advance(250)
  r.net.last.fail()
  await r.clock.advance(1_000)
  const s3 = await r.connect()
  s3.serverClose(1001)
  await flush()
  const st = r.t.status()
  assert.deepEqual(st.stats.closeStatuses, { '-1': 1, '1001': 1 })
  assert.equal(st.stats.dialErrors, 1)
})

test('criterion 16 · a close this client made on stop() is not counted as one the peer sent', async () => {
  const r = rig()
  r.sync.answer = () => bootstrapPage()
  const ends: SessionEnd[] = []
  r.t.onSessionEnd((e) => ends.push(e))
  r.t.start()
  await r.connect()
  r.t.stop()
  await flush()
  assert.deepEqual(r.t.status().stats.closeStatuses, {})
  assert.equal(ends.length, 1, 'the outbox still learns the session ended')
  assert.equal(ends[0].closeCode, null)
  assert.equal(ends[0].bare1008, false)
  assert.equal(r.net.last.closedByClient?.code, 1000, 'a clean close on the wire')
})
