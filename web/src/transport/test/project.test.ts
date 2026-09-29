/* CANT-35 criteria 31 and 16's adapter: the journal-to-Vue projection, and
 * `connectionInfo`. Neither has a Go counterpart; the Go client renders
 * nothing. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { Applied } from '../journal'
import { EMPTY_PROJECTION, project, projectApplied } from '../project'
import { connectionInfo, emptyStats, type TransportStatus } from '../status'
import { NOT_TERMINAL } from '../terminal'
import { bootstrapPage, conversation, flush, message, messageFrame, page, ready, rig, user, uuid } from './harness'

test('criterion 31 · projectApplied over every Applied equals project over the final snapshot, at every step', async () => {
  const r = rig()
  let projected = EMPTY_PROJECTION
  const seen: Applied['source'][] = []
  r.t.onApply((a) => {
    projected = projectApplied(projected, a)
    seen.push(a.source)
  })
  const same = (why: string) => assert.deepEqual(projected, project(r.t.snapshot()), why)

  const C2 = uuid(104)
  const newcomer = uuid(270)
  let stage = 0
  r.sync.answer = (req) => {
    if (stage === 0) return bootstrapPage(3, [message(1), message(2), message(3)])
    if (stage === 1) return page({ logSeq: Math.max(req.after, 3) })
    // After the restore: a regrown log.
    return req.after === 0 ? bootstrapPage(2, [message(1, { id: uuid(4001) }), message(2, { id: uuid(4002) })]) : page({ logSeq: req.after })
  }
  r.t.start()
  const s = await r.connect()
  await flush()
  same('a page')
  stage = 1

  s.frame(messageFrame(message(4)))
  await flush()
  same('a live frame')

  s.frame(messageFrame({ ...message(2), readBy: 2, state: 'read' }))
  await flush()
  same('a re-emission replacing a held message')
  assert.equal(projected.messages.find((m) => m.id === message(2).id)?.readBy, 2)

  s.frame({ type: 'conversation', conversation: conversation(C2) })
  s.frame({ type: 'user', user: user(newcomer, 'New Person') })
  s.frame(messageFrame(message(5, { conversationId: C2, seq: 1, authorId: newcomer })))
  await flush()
  same('an introduction')
  assert.ok(projected.messages.some((m) => m.conversationId === C2))
  assert.equal(projected.users[newcomer]?.name, 'New Person')

  stage = 2
  s.frame(ready({ logSeq: 1 }))
  await flush()
  same('a wipe and a bootstrap')
  assert.ok(seen.includes('wipe'))
  assert.deepEqual(projected.messages.map((m) => m.id).sort(), [uuid(4001), uuid(4002)])
  assert.deepEqual(seen.filter((x) => x === 'page').length > 0 && seen.includes('live'), true)
})

test('criterion 31 · the projection is pure', () => {
  const a: Applied = {
    source: 'page', cursor: 1, messages: [message(1)], conversations: [conversation(uuid(100))], users: [user(uuid(1))],
    receipts: [], wiped: false,
  }
  const before = JSON.stringify(EMPTY_PROJECTION)
  const out = projectApplied(EMPTY_PROJECTION, a)
  assert.equal(JSON.stringify(EMPTY_PROJECTION), before, 'the input is not mutated')
  assert.deepEqual(projectApplied(EMPTY_PROJECTION, a), out, 'the same input gives the same output')
})

function status(extra: Partial<TransportStatus>): TransportStatus {
  return {
    terminal: NOT_TERMINAL, refreshHold: 'none', tokenRefused: false, nextRefreshAt: null, connected: false,
    ready: false, sessionId: null, heartbeatIntervalSec: null, missedPongLimit: null, caughtUp: true, cursor: null,
    attempt: 0, nextDialAt: null, stats: emptyStats(), messages: 0, wipes: 0, ...extra,
  }
}

test('criterion 16 · connectionInfo maps every transport state onto the banner\'s', () => {
  const now = 1_000_000
  assert.deepEqual(connectionInfo(status({ attempt: 3, nextDialAt: now + 4_100 }), { now }), {
    state: 'reconnecting', attempt: 3, retryInSec: 5, refreshHold: 'none', tokenRefused: false,
  })
  assert.equal(connectionInfo(status({ connected: true }), { now }).state, 'reconnecting', 'open, not yet ready')
  assert.equal(connectionInfo(status({ connected: true, ready: true, caughtUp: false }), { now }).state, 'resyncing')
  assert.equal(connectionInfo(status({ connected: true, ready: true, caughtUp: true }), { now }).state, 'live')
  assert.equal(connectionInfo(status({ ready: true }), { now, online: false }).state, 'offline')
  const t = connectionInfo(status({ terminal: { kind: 'credential', reason: 'refused' } }), { now, online: false })
  assert.deepEqual(t, {
    state: 'terminal', terminal: { kind: 'credential', reason: 'refused' }, refreshHold: 'none', tokenRefused: false,
  }, 'terminal wins over offline: neither terminal ends on a network change')
  assert.equal(connectionInfo(status({ terminal: { kind: 'protocol', reason: '4001' } }), { now }).terminal?.kind, 'protocol')
  const held = connectionInfo(status({ refreshHold: 'backoff', tokenRefused: true }), { now })
  assert.equal(held.refreshHold, 'backoff')
  assert.equal(held.tokenRefused, true)
})
