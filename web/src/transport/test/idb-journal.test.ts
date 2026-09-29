/* CANT-169 — CANT-35 criterion 32, the durable half of CANT-24 obligation 1,
 * against fake-indexeddb: the journal lives in CANT-35's `catenary` database,
 * each page is one transaction, `onApply` fires only after `oncomplete`, a
 * transport torn down mid-page leaves the whole page and its cursor or
 * neither, and a new transport over the same database resumes from the stored
 * cursor. Each with the control that must make it fail.
 */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { IDBFactory } from 'fake-indexeddb'
import { enrollCredential, IdbCredentialStore } from '../credential-store'
import {
  CONVERSATIONS_STORE, COUNTED_STORE, CREDENTIAL_STORE, JOURNAL_STORE, MESSAGES_STORE, USERS_STORE, openCatenaryDb,
} from '../db'
import { IdbJournal, JournalStale, type IdbJournalOptions } from '../idb-journal'
import type { Applied } from '../journal'
import { browserLock } from '../seams'
import { enrolled } from './credential-harness'
import { bootstrapPage, conversation, flush, message, messageFrame, page, rig, user, uuid } from './harness'

/** IndexedDB completes on setImmediate turns of its own; give it enough. */
const settle = () => flush(40)

async function count(factory: IDBFactory, store: string): Promise<number> {
  const db = await openCatenaryDb({ factory })
  try {
    return await new Promise<number>((resolve, reject) => {
      const req = db.transaction(store).objectStore(store).count()
      req.onsuccess = () => resolve(req.result)
      req.onerror = () => reject(req.error)
    })
  } finally {
    db.close()
  }
}

const P1 = bootstrapPage(3, [message(1), message(2), message(3)])
/** The second page: messages 4–6 and a room it introduces. */
const C2 = uuid(102)
const P2 = page({
  logSeq: 6,
  messages: [message(4), message(5), message(6, { conversationId: C2, seq: 1 })],
  conversations: [conversation(C2)],
  users: [user(uuid(7))],
})

/**
 * Two pages, the second torn down mid-write when `tearDown` is set; then the
 * transport is stopped and the journal closed — the tab is gone — and a new
 * journal is opened over the same database, as the next page load would.
 */
async function tornDownMidPage(opts: { tearDown: boolean; journal?: IdbJournalOptions }) {
  const factory = new IDBFactory()
  let pages = 0
  const journal = await IdbJournal.open({
    factory,
    ...opts.journal,
    midWrite: (tx, source) => {
      if (source === 'page' && ++pages === 2 && opts.tearDown) tx.abort()
    },
  })
  const r = rig({ journal })
  r.sync.answer = (req) => (req.after === 0 ? { ...P1, hasMore: true } : P2)
  r.t.start()
  await settle()
  const status = r.t.status()
  r.t.stop()
  journal.close()

  const reopened = await IdbJournal.open({ factory })
  const snap = reopened.snapshot()
  const ids = new Set(snap.messages.map((m) => m.id))
  const p2Held = P2.messages.filter((m) => ids.has(m.id)).length
  const p2Convs = P2.conversations.filter((c) => reopened.holdsConversation(c.id)).length
  const p2Users = P2.users.filter((u) => reopened.holdsUser(u.id)).length
  const wholeOrNeither =
    (snap.cursor === P2.logSeq && p2Held === 3 && p2Convs === 1 && p2Users === 1) ||
    (snap.cursor === P1.logSeq && p2Held === 0 && p2Convs === 0 && p2Users === 0)
  return { factory, status, reopened, snap, p2Held, wholeOrNeither }
}

test('criterion 32 · messages, conversations, users and cursor live in the catenary database, beside the credential', async () => {
  const factory = new IDBFactory()
  const creds = await IdbCredentialStore.open({ factory })
  await enrollCredential(creds, browserLock(), enrolled(Date.UTC(2026, 8, 29)))
  const journal = await IdbJournal.open({ factory })
  const r = rig({ journal })
  r.sync.answer = () => P1
  r.t.start()
  await settle()
  assert.equal(r.t.status().cursor, 3)
  assert.equal(await count(factory, MESSAGES_STORE), 3)
  assert.equal(await count(factory, CONVERSATIONS_STORE), 1)
  assert.equal(await count(factory, USERS_STORE), 2)
  assert.equal(await count(factory, COUNTED_STORE), 3, 'the evidence log is written with the messages it counts')
  assert.equal(await count(factory, JOURNAL_STORE), 2, 'the cursor and the wipe count')
  assert.equal(await count(factory, CREDENTIAL_STORE), 1, 'and the credential is where it was')
  r.t.stop()
  journal.close()
  creds.close()
})

test('criterion 32 · one transaction per page, and onApply only after its oncomplete', async () => {
  const factory = new IDBFactory()
  const order: string[] = []
  const applied: Applied[] = []
  let r!: ReturnType<typeof rig>
  const seen: { applied: number; messages: number; cursor: number | null }[] = []
  const journal = await IdbJournal.open({
    factory,
    midWrite: (tx, source) => {
      if (source !== 'page') return
      order.push('write')
      // Every request of the page is issued and the transaction is still open:
      // nothing it carries may be observable yet.
      seen.push({ applied: applied.length, messages: r.t.snapshot().messages.length, cursor: r.t.status().cursor })
      tx.addEventListener('complete', () => order.push('complete'))
    },
  })
  r = rig({ journal })
  r.t.onApply((a) => {
    applied.push(a)
    order.push('apply')
  })
  r.sync.answer = (req) => (req.after === 0 ? { ...P1, hasMore: true } : P2)
  r.t.start()
  await settle()
  assert.deepEqual(order, ['write', 'complete', 'apply', 'write', 'complete', 'apply'], 'one transaction per page, each emitted after it completed')
  assert.deepEqual(seen, [
    { applied: 0, messages: 0, cursor: null },
    { applied: 1, messages: 3, cursor: 3 },
  ], 'mid-transaction, the page is invisible: no Applied, no message, no cursor')
  assert.equal(r.t.status().cursor, 6)
  r.t.stop()
  journal.close()
})

test('criterion 32 · a transport torn down mid-page leaves the whole page and its cursor, or neither', async () => {
  const torn = await tornDownMidPage({ tearDown: true })
  assert.equal(torn.status.journalError?.name, 'JournalWriteAborted', 'the failed write is in status')
  assert.equal(torn.status.cursor, 3, 'and the live journal never showed the page')
  assert.equal(torn.snap.cursor, 3, 'reopened: page 1’s cursor')
  assert.equal(torn.snap.messages.length, 3, 'page 1 whole')
  assert.equal(torn.p2Held, 0, 'and none of page 2')
  assert.ok(torn.wholeOrNeither)
  assert.deepEqual(torn.reopened.counted(), P1.messages.map((m) => m.id), 'the evidence log is page 1’s, once each')

  const whole = await tornDownMidPage({ tearDown: false })
  assert.equal(whole.snap.cursor, 6)
  assert.equal(whole.p2Held, 3)
  assert.ok(whole.wholeOrNeither)
})

test('criterion 32 · negative control splitCursor: the cursor in its own transaction keeps a page without it', async () => {
  const torn = await tornDownMidPage({ tearDown: true, journal: { splitCursor: true } })
  assert.equal(torn.snap.cursor, 3, 'the cursor did not land')
  assert.equal(torn.p2Held, 3, 'but page 2’s messages did')
  assert.equal(torn.wholeOrNeither, false, 'the property fails')
})

test('criterion 32 · a new transport over the same database resumes with resume_from_log_seq equal to the stored cursor', async () => {
  const torn = await tornDownMidPage({ tearDown: true })
  const r = rig({ journal: torn.reopened })
  r.sync.answer = () => P2
  r.t.start()
  await r.connect()
  const hello = r.net.last.frames()[0]
  assert.equal(hello.type, 'hello')
  assert.equal(hello.type === 'hello' && hello.resumeFromLogSeq, 3, 'the hello carries the stored cursor')
  assert.equal(r.sync.requests[0].after, 3, 'and /sync asks from it, not from 0')
  await settle()
  assert.equal(r.t.status().cursor, 6)
  // A live frame after the resume is counted once, not again on the next page.
  r.net.last.frame(messageFrame(message(7)))
  await settle()
  r.t.stop()
  torn.reopened.close()

  const third = await IdbJournal.open({ factory: torn.factory })
  assert.equal(third.cursor(), 6)
  assert.equal(third.messageCount(), 7)
  const counted = third.counted()
  assert.equal(new Set(counted).size, counted.length, 'nothing counted twice across the relaunch')
  assert.equal(counted.length, 7)
  third.close()
})

test('obligation 4 · a wipe clears the stored journal and never the credential', async () => {
  const factory = new IDBFactory()
  const creds = await IdbCredentialStore.open({ factory })
  const cred = enrolled(Date.UTC(2026, 8, 29))
  await enrollCredential(creds, browserLock(), cred)
  const journal = await IdbJournal.open({ factory })
  await journal.applyPage(P1, { cursorOnLiveFrames: false, dedupeByLogSeq: false })
  const a = await journal.wipe()
  assert.equal(a.wiped, true)
  journal.close()

  const reopened = await IdbJournal.open({ factory })
  assert.deepEqual(reopened.snapshot(), { cursor: null, messages: [], conversations: [], users: [] })
  assert.deepEqual(reopened.counted(), [])
  assert.equal(reopened.wipes(), 1, 'the wipe is counted, durably')
  for (const s of [MESSAGES_STORE, CONVERSATIONS_STORE, USERS_STORE, COUNTED_STORE]) assert.equal(await count(factory, s), 0, s)
  assert.equal((await creds.read())?.deviceId, cred.deviceId, 'the credential is untouched')
  reopened.close()
  creds.close()
})

test('quota · a QuotaExceededError is surfaced in status, lands nothing, and the page is retried on the backoff', async () => {
  const factory = new IDBFactory()
  let refuse = true
  const journal = await IdbJournal.open({
    factory,
    midWrite: (_tx, source) => {
      if (source === 'page' && refuse) throw new DOMException('the origin is out of quota', 'QuotaExceededError')
    },
  })
  const r = rig({ journal })
  r.sync.answer = () => P1
  r.t.start()
  await r.connect()
  await settle()
  const s = r.t.status()
  assert.equal(s.journalError?.name, 'QuotaExceededError', 'surfaced by name')
  assert.equal(s.cursor, null, 'nothing landed')
  assert.equal(s.messages, 0)
  assert.equal(await count(factory, MESSAGES_STORE), 0, 'nothing on disk either')
  assert.ok(s.stats.syncErrors >= 1, 'a page that did not land is a failed catch-up')
  const asked = r.sync.requests.length
  await settle()
  assert.equal(r.sync.requests.length, asked, 'and it is not asked again at once — the backoff holds it')

  refuse = false
  await r.clock.advance(60_000)
  await settle()
  assert.equal(r.t.status().journalError, null, 'cleared once a write lands')
  assert.equal(r.t.status().cursor, 3)
  r.t.stop()
  journal.close()
})

test('a stored record the wire schema refuses is refused at open, not rendered', async () => {
  const factory = new IDBFactory()
  const db = await openCatenaryDb({ factory })
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(MESSAGES_STORE, 'readwrite')
    tx.objectStore(MESSAGES_STORE).put({ id: uuid(1), text: 'no seq, no log_seq' })
    tx.oncomplete = () => resolve()
    tx.onabort = () => reject(tx.error)
  })
  db.close()
  await assert.rejects(IdbJournal.open({ factory }))
})

const NO_FAULTS = { cursorOnLiveFrames: false, dedupeByLogSeq: false }

/**
 * Two tabs over one database, each with its own mirror (CANT-35 ruling 1 → A),
 * through a restore that shrinks the log from head 10 to head 6. Tab A wipes and
 * bootstraps to 6; tab B, whose `ready` arrives later, wipes again — clearing
 * A's pages — and lands only its first page (to 5) before it is closed. Then a
 * live frame reaches A. The next page load must resume from a cursor whose
 * every message is held (PR #110's review).
 */
async function twoTabsAcrossAWipe(opts: IdbJournalOptions) {
  const factory = new IDBFactory()
  const before = [...Array(10)].map((_, i) => message(i + 1))
  const seed = await IdbJournal.open({ factory })
  await seed.applyPage(bootstrapPage(10, before), NO_FAULTS)
  seed.close()

  const a = await IdbJournal.open({ factory, ...opts })
  const b = await IdbJournal.open({ factory, ...opts })
  const after = [...Array(7)].map((_, i) => message(i + 1, { id: uuid(5000 + i + 1) }))
  await a.wipe()
  await a.applyPage(bootstrapPage(6, after.slice(0, 6)), NO_FAULTS)
  await b.wipe()
  await b.applyPage(bootstrapPage(5, after.slice(0, 5)), NO_FAULTS)
  b.close()
  const live = await a.applyLive({ messages: [after[6]] }, NO_FAULTS).then(() => null, (e: unknown) => e)
  const aCursor = a.cursor()
  a.close()

  const next = await IdbJournal.open({ factory })
  const held = new Set(next.snapshot().messages.map((m) => m.id))
  const cursor = next.cursor() ?? 0
  const uncovered = after.filter((m) => m.logSeq <= cursor && !held.has(m.id)).map((m) => m.logSeq)
  next.close()
  return { live, aCursor, cursor, uncovered }
}

test('two tabs · a write over a journal another tab wiped is refused, and the tab reloads what is stored', async () => {
  const r = await twoTabsAcrossAWipe({})
  assert.ok(r.live instanceof JournalStale, 'A’s live write is refused')
  assert.equal(r.aCursor, 5, 'and A’s mirror is now what is stored')
  assert.equal(r.cursor, 5)
  assert.deepEqual(r.uncovered, [], 'every message at or below the stored cursor is held')
})

test('two tabs · negative control ignoreGeneration leaves a cursor above messages the store no longer holds', async () => {
  const r = await twoTabsAcrossAWipe({ ignoreGeneration: true })
  assert.equal(r.live, null, 'the write went through')
  assert.equal(r.cursor, 6)
  assert.deepEqual(r.uncovered, [6], 'message 6 is below the cursor and will never be fetched again')
})

test('two tabs · a tab behind the stored cursor never moves it backward', async () => {
  const factory = new IDBFactory()
  const a = await IdbJournal.open({ factory })
  const b = await IdbJournal.open({ factory })
  await a.applyPage(bootstrapPage(6, [message(1), message(6)]), NO_FAULTS)
  await b.applyPage(bootstrapPage(3, [message(1)]), NO_FAULTS)
  a.close()
  b.close()
  const next = await IdbJournal.open({ factory })
  assert.equal(next.cursor(), 6)
  assert.equal(next.messageCount(), 2)
  next.close()
})
