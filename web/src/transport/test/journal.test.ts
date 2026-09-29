/* CANT-35 criteria 3, 4, 5, 6, 24 and 25: CANT-24's obligations 1–4, CANT-103's
 * rules 1–4 and the receipt rule (ruling 4 → B), each with the negative
 * control that must make it fail. Mirrors internal/client/client_test.go and
 * the kill-test and restore rigs' unit half.
 *
 * EACH SCENARIO IS A FUNCTION OF ITS FAULTS. The clean run must hold the
 * property, and the run with the named fault must break it: a check nobody has
 * seen fail is a claim about the check.
 */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { SyncResponse } from '@/wire/generated'
import type { Faults } from '../faults'
import { MemoryJournal, type Applied } from '../journal'
import {
  bootstrapPage, CONV, conversation, deferred, flush, ME, message, messageFrame, OTHER, page, ready, rig, user, uuid,
} from './harness'

// --- obligation 2 -------------------------------------------------------------

async function obligation2(faults: Partial<Faults>) {
  const r = rig({ faults })
  const m = [1, 2, 3, 4].map((n) => message(n))
  let committed4 = false
  r.sync.answer = (req) => {
    if (req.after === 0) return bootstrapPage(3, m.slice(0, 3))
    // Once message 4 is committed, the catch-up re-carries it, as a page does
    // for anything that arrived live.
    if (req.after === 3) return committed4 ? page({ logSeq: 4, messages: [m[3]] }) : page({ logSeq: 3 })
    // A stale page: bounded below the held cursor.
    return page({ logSeq: 1 })
  }
  r.t.start()
  const s = await r.connect()
  await flush()
  const cursorBefore = r.t.status().cursor

  committed4 = true
  s.frame(messageFrame(m[3]))
  await flush()
  const liveMovedCursor = r.t.status().cursor !== cursorBefore

  r.t.catchUp()
  await flush()
  // A CANT-92 re-emission of message 2: the same id, its original log_seq, a
  // new read_by.
  s.frame(messageFrame({ ...m[1], readBy: 2, state: 'read' }))
  await flush()
  const held2 = r.t.snapshot().messages.find((x) => x.id === m[1].id)
  const reemissionApplied = held2?.readBy === 2

  // And a page below the cursor must not move it back.
  r.sync.answer = () => page({ logSeq: 1 })
  const cursorHigh = r.t.status().cursor
  r.t.catchUp()
  await flush()
  const cursorWentBack = (r.t.status().cursor ?? 0) < (cursorHigh ?? 0)

  const counted = r.journal.counted()
  const countedTwice = counted.filter((id, i) => counted.indexOf(id) !== i)
  return { liveMovedCursor, reemissionApplied, cursorWentBack, countedTwice, cursor: cursorHigh }
}

test('criterion 3 · obligation 2: only a page moves the cursor, forward only, and dedupe is by id', async () => {
  const clean = await obligation2({})
  assert.equal(clean.liveMovedCursor, false, 'a live message frame moved no cursor')
  assert.equal(clean.cursor, 4, 'the page moved it to its own log_seq')
  assert.equal(clean.cursorWentBack, false, 'a lower page did not move it back')
  assert.equal(clean.reemissionApplied, true, 'a later record for a held id replaced it')
  assert.deepEqual(clean.countedTwice, [], 'and nothing was counted twice')
})

test('criterion 3 · negative control cursorOnLiveFrames makes it fail', async () => {
  assert.equal((await obligation2({ cursorOnLiveFrames: true })).liveMovedCursor, true)
})

test('criterion 3 · negative control dedupeByLogSeq makes it fail', async () => {
  const broken = await obligation2({ dedupeByLogSeq: true })
  assert.ok(broken.countedTwice.length > 0 || !broken.reemissionApplied, 'a duplicate count, or a dropped re-emission')
  assert.ok(broken.countedTwice.length > 0, 'the re-carried live message is counted twice')
  assert.equal(broken.reemissionApplied, false, 'the re-emission below the cursor is dropped')
})

// --- obligation 3 and CANT-103 rules 1–3 ----------------------------------------

/**
 * A trigger that lands while a page is in flight. The page was issued before
 * it and is bounded below the triggering message's log_seq; only a catch-up
 * that re-arms its end condition asks again and finds the message.
 */
async function obligation3(faults: Partial<Faults>) {
  const r = rig({ faults })
  const C2 = uuid(102)
  const newRoom = message(5, { conversationId: C2, seq: 1 })
  const held = deferred<SyncResponse>()
  let inFlight = 0
  let maxInFlight = 0
  r.sync.answer = async (req) => {
    inFlight++
    maxInFlight = Math.max(maxInFlight, inFlight)
    try {
      if (req.after === 0) return bootstrapPage(3, [message(1), message(2), message(3)])
      if (req.after === 3) return await held.promise
      // Asked again after the trigger: the new room, introduced on its page.
      return page({ logSeq: 5, messages: [newRoom], conversations: [conversation(C2)] })
    } finally {
      inFlight--
    }
  }
  r.t.start()
  const s = await r.connect()
  await flush()
  s.frame({ type: 'resync_required', reason: 'cursor_too_old', logSeq: 4 })
  await flush()
  // The request after=3 is in flight. The new room's first message arrives
  // live, naming a conversation this client does not hold.
  s.frame(messageFrame(newRoom))
  await flush()
  const discards = r.t.status().stats.introductionDiscards
  // The in-flight page is bounded at 4: below the message's log_seq of 5.
  held.resolve(page({ logSeq: 4, messages: [message(4)] }))
  await flush()
  const snap = r.t.snapshot()
  return {
    held: snap.messages.filter((m) => m.id === newRoom.id).length,
    discards,
    caughtUp: r.t.status().caughtUp,
    maxInFlight,
    counted: r.journal.counted().filter((id) => id === newRoom.id).length,
  }
}

test('criterion 4 · obligation 3: a trigger mid-page re-arms the end condition, one catch-up at a time', async () => {
  const clean = await obligation3({})
  assert.equal(clean.discards, 1, 'the unheld conversation\'s message was discarded, not buffered')
  assert.equal(clean.held, 1, 'and is held at the end, exactly once')
  assert.equal(clean.counted, 1)
  assert.equal(clean.caughtUp, true)
  assert.equal(clean.maxInFlight, 1, 'never two catch-ups at once')
})

test('criterion 4 · negative control endCatchUpEarly makes it fail', async () => {
  assert.equal((await obligation3({ endCatchUpEarly: true })).held, 0)
})

test('criterion 4 · negative control ignoreRetrigger makes it fail', async () => {
  assert.equal((await obligation3({ ignoreRetrigger: true })).held, 0)
})

// --- obligation 4 -----------------------------------------------------------------

/**
 * A server whose log is behind the cursor — a restore. The page requested from
 * the old cursor before `ready` must be dropped, and the client must end with
 * exactly the regrown log.
 */
async function obligation4(faults: Partial<Faults>) {
  const r = rig({ faults })
  const old = [1, 2, 3, 4, 5, 6].map((n) => message(n))
  const regrown = [1, 2, 3].map((n) => message(n, { id: uuid(3000 + n), text: `regrown ${n}` }))
  r.sync.answer = (req) => (req.after === 0 ? bootstrapPage(6, old) : page({ logSeq: req.after }))
  r.t.start()
  const s = await r.connect()
  await flush()
  assert.equal(r.t.status().cursor, 6)

  // The server is restored to log_seq 3 while this client is away.
  const stale = deferred<SyncResponse>()
  let restored = false
  r.sync.answer = (req) => {
    if (!restored) return stale.promise
    return req.after === 0 ? bootstrapPage(3, regrown) : page({ logSeq: Math.max(req.after, 3) })
  }
  s.serverClose(1001)
  await r.clock.advance(250)
  // The dial's /sync (after=6) is in flight, issued before any ready.
  const s2 = r.net.last
  s2.open()
  restored = true
  s2.frame(ready({ logSeq: 3 }))
  await flush()
  // The old server's answer to the request issued before the wipe lands now.
  stale.resolve(page({ logSeq: 7, messages: [message(7)] }))
  await flush()
  const snap = r.t.snapshot()
  return {
    ids: snap.messages.map((m) => m.id).sort(),
    want: regrown.map((m) => m.id).sort(),
    cursor: snap.cursor,
    discards: r.t.status().stats.discards,
    wipes: r.t.status().wipes,
  }
}

test('criterion 5 · obligation 4: a ready below the cursor wipes, drops the stale page, and bootstraps from 0', async () => {
  const clean = await obligation4({})
  assert.equal(clean.discards, 1)
  assert.equal(clean.wipes, 1)
  assert.deepEqual(clean.ids, clean.want, 'exactly the regrown log: nothing old, and the stale page dropped')
  assert.equal(clean.cursor, 3)
})

test('criterion 5 · the wipe never touches the credential', async () => {
  // The credential is not in the journal at all: a wipe is the journal's, and
  // the next dial presents the same pair.
  const r = rig()
  r.sync.answer = (req) => (req.after === 0 ? bootstrapPage(6, [message(6)]) : page({ logSeq: req.after }))
  r.t.start()
  const s = await r.connect()
  await flush()
  s.frame(ready({ logSeq: 1 }))
  await flush()
  s.serverClose(1001)
  await r.clock.advance(250)
  assert.deepEqual(r.net.last.protocols, ['catenary.v1', `catenary.token.${r.credential.accessToken}`])
})

test('criterion 5 · negative control skipWipe makes it fail', async () => {
  const broken = await obligation4({ skipWipe: true })
  assert.notDeepEqual(broken.ids, broken.want)
})

// --- CANT-103 rules 1 and 4 -------------------------------------------------------

test('criterion 6 · conversation and user frames apply by id and move no cursor; an unheld author is discarded and fetched', async () => {
  const r = rig()
  const stranger = uuid(250)
  const byStranger = message(4, { authorId: stranger })
  let committed = false
  r.sync.answer = (req) => {
    if (req.after === 0) return bootstrapPage(3, [message(1), message(2), message(3)])
    if (!committed) return page({ logSeq: 3 })
    return page({ logSeq: 4, messages: [byStranger], users: [user(stranger)] })
  }
  r.t.start()
  const s = await r.connect()
  await flush()
  const C2 = uuid(103)
  s.frame({ type: 'conversation', conversation: conversation(C2, { name: 'first' }) })
  s.frame({ type: 'conversation', conversation: conversation(C2, { name: 'second' }) })
  s.frame({ type: 'user', user: user(uuid(260), 'Once') })
  s.frame({ type: 'user', user: user(uuid(260), 'Twice') })
  await flush()
  let snap = r.t.snapshot()
  assert.equal(snap.conversations.filter((c) => c.id === C2).length, 1)
  assert.equal(snap.conversations.find((c) => c.id === C2)?.name, 'second', 'a later record replaces')
  assert.equal(snap.users.find((u) => u.id === uuid(260))?.name, 'Twice')
  assert.equal(snap.cursor, 3, 'introductions move no cursor')

  const requests = r.sync.requests.length
  committed = true
  s.frame(messageFrame(byStranger))
  await flush()
  assert.equal(r.t.status().stats.introductionDiscards, 1)
  assert.ok(r.sync.requests.length > requests, 'the discard pulled a catch-up')
  snap = r.t.snapshot()
  assert.equal(snap.messages.filter((m) => m.id === byStranger.id).length, 1, 'held exactly once after it')
  assert.equal(r.journal.counted().filter((id) => id === byStranger.id).length, 1)
})

// --- ruling 4 → B: receipts ---------------------------------------------------------

test('criterion 24 · another user\'s receipt changes nothing and triggers nothing', async () => {
  const r = rig()
  r.sync.answer = (req) => (req.after === 0 ? bootstrapPage(3, [message(1), message(2), message(3)]) : page({ logSeq: req.after }))
  r.t.start()
  const s = await r.connect()
  await flush()
  const before = r.t.snapshot()
  const requests = r.sync.requests.length
  s.frame({ type: 'receipt', conversationId: CONV, userId: OTHER, upToSeq: 3 })
  await flush()
  assert.deepEqual(r.t.snapshot(), before)
  assert.equal(r.sync.requests.length, requests)
})

test('criterion 24 · an own-user receipt pulls exactly one catch-up, and first_unread_seq moves only with the page', async () => {
  const r = rig()
  r.sync.answer = (req) =>
    req.after === 0
      ? page({ logSeq: 3, messages: [message(1), message(2), message(3)], conversations: [conversation(CONV, { firstUnreadSeq: 1, headSeq: 3 })], users: [user(ME), user(OTHER)] })
      : page({ logSeq: req.after })
  r.t.start()
  const s = await r.connect()
  await flush()
  const requests = r.sync.requests.length
  const held = deferred<SyncResponse>()
  r.sync.answer = () => held.promise
  s.frame({ type: 'receipt', conversationId: CONV, userId: ME, upToSeq: 3 })
  await flush()
  assert.equal(r.sync.requests.length, requests + 1, 'exactly one catch-up')
  assert.equal(r.t.snapshot().conversations[0].firstUnreadSeq, 1, 'unchanged until a page lands')
  held.resolve(page({ logSeq: 3, conversations: [conversation(CONV, { headSeq: 3 })] }))
  await flush()
  assert.equal(r.t.snapshot().conversations[0].firstUnreadSeq, undefined, 'the page is what moved it')
  assert.equal(r.sync.requests.length, requests + 1)
})

// --- obligation 1 -----------------------------------------------------------------

test('criterion 25 · obligation 1: nothing a page carries is observable before its cursor is written', async () => {
  const gate = deferred<void>()
  let holding = true
  const journal = new MemoryJournal({
    beforeCommit: async (source) => {
      if (source === 'page' && holding) await gate.promise
    },
  })
  const r = rig({ journal })
  const applied: Applied[] = []
  r.t.onApply((a) => applied.push(a))
  r.sync.answer = () => bootstrapPage(3, [message(1), message(2), message(3)])
  r.t.start()
  await flush()
  await flush()
  assert.equal(r.sync.requests.length, 1, 'the page arrived and its write is held open')
  assert.equal(applied.length, 0, 'no Applied before the write lands')
  assert.deepEqual(r.t.snapshot().messages, [], 'no message visible')
  assert.equal(r.t.snapshot().cursor, null, 'and no cursor')
  assert.equal(r.t.status().messages, 0)
  holding = false
  gate.resolve()
  await flush()
  assert.equal(applied.length, 1)
  assert.equal(applied[0].cursor, 3, 'the Applied carries the cursor it landed with')
  assert.equal(r.t.snapshot().messages.length, 3)
  assert.equal(r.t.snapshot().cursor, 3)
})
