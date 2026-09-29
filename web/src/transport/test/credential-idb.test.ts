/* CANT-35 criterion 29 (ruling 10 → A) and criterion 11 on real adapters: the
 * IndexedDB credential store and the Web Locks wrapper, against fake-indexeddb
 * and Node's own `navigator.locks` — not against in-memory fakes of
 * themselves. These are the two pieces that can cost a device its credential
 * if they are wrong (CANT-31 §2 and §3: persist before present, and every
 * read-modify-write under the lock). */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { IDBFactory } from 'fake-indexeddb'
import {
  CredentialHeld, enrollCredential, IdbCredentialStore, reenrollCredential, type StoredCredential,
} from '../credential-store'
import { CATENARY_DB, CREDENTIAL_STORE, JOURNAL_STORES, openCatenaryDb, UPGRADES, type UpgradeStep } from '../db'
import { createRefreshingCredential } from '../refresh'
import { browserLock } from '../seams'
import { credRig, enrolled, noReplays, padToken } from './credential-harness'
import { DEVICE, uuid } from './harness'

const T0 = Date.UTC(2026, 8, 29, 12, 0, 0)

async function seeded(factory: IDBFactory, cred: StoredCredential = enrolled(T0)) {
  const store = await IdbCredentialStore.open({ factory })
  await enrollCredential(store, browserLock(), cred)
  return store
}

test('criterion 29 · the Web Locks wrapper is the platform’s, not a fallback', () => {
  assert.equal(typeof globalThis.navigator?.locks?.request, 'function', 'Node 24 has navigator.locks')
})

test('criterion 29 · openCatenaryDb creates the stores at version 1 and runs each upgrade step exactly once', async () => {
  const factory = new IDBFactory()
  // Version 1 is the credential alone; CANT-169's journal is step 2.
  const v1 = await openCatenaryDb({ factory, steps: UPGRADES.slice(0, 1) })
  assert.equal(v1.version, 1)
  assert.deepEqual([...v1.objectStoreNames], [CREDENTIAL_STORE])
  v1.close()
  const db = await openCatenaryDb({ factory })
  assert.equal(db.name, CATENARY_DB)
  assert.equal(db.version, UPGRADES.length)
  assert.equal(db.version, 2)
  assert.ok(db.objectStoreNames.contains(CREDENTIAL_STORE))
  for (const name of JOURNAL_STORES) assert.ok(db.objectStoreNames.contains(name), `step 2 created ${name}`)
  db.close()

  const ran: number[] = []
  const counted = (i: number, step: UpgradeStep): UpgradeStep => (d, tx) => {
    ran.push(i)
    step(d, tx)
  }
  // Reopening at the same version runs nothing.
  const again = await openCatenaryDb({ factory, steps: UPGRADES.map((s, i) => counted(i + 1, s)) })
  again.close()
  assert.deepEqual(ran, [])
  // A step appended is run once, and only it.
  const later = UPGRADES.length + 1
  const steps = [...UPGRADES.map((s, i) => counted(i + 1, s)), counted(later, (d) => void d.createObjectStore('later'))]
  const bumped = await openCatenaryDb({ factory, steps })
  assert.equal(bumped.version, later)
  assert.ok(bumped.objectStoreNames.contains('later'))
  bumped.close()
  const third = await openCatenaryDb({ factory, steps })
  third.close()
  assert.deepEqual(ran, [later], 'the new step once; the shipped ones never again')

  // A fresh database runs every step, in order, once.
  const fresh = new IDBFactory()
  ran.length = 0
  ;(await openCatenaryDb({ factory: fresh, steps })).close()
  assert.deepEqual(ran, [...UPGRADES.map((_, i) => i + 1), later])
})

test('criterion 29 · enroll refuses over a held pair; re-enroll replaces it, one record per device', async () => {
  const factory = new IDBFactory()
  const store = await seeded(factory)
  await assert.rejects(enrollCredential(store, browserLock(), enrolled(T0)), CredentialHeld)
  const next: StoredCredential = { ...enrolled(T0), deviceId: uuid(4), accessToken: padToken('access-new'), refreshToken: padToken('refresh-new') }
  await reenrollCredential(store, browserLock(), next)
  const other = await IdbCredentialStore.open({ factory })
  assert.equal((await other.read())?.deviceId, uuid(4))
  const db = await openCatenaryDb({ factory })
  const count = await new Promise<number>((res) => {
    const req = db.transaction(CREDENTIAL_STORE).objectStore(CREDENTIAL_STORE).count()
    req.onsuccess = () => res(req.result)
  })
  assert.equal(count, 1, 'the old device’s record went in the same transaction')
  db.close()
  store.close()
  other.close()
})

test('criterion 29 · a rotated pair is persisted, and readable by a second opener, before any request presents it', async () => {
  const factory = new IDBFactory()
  const store = await seeded(factory)
  const r = credRig({ store, lock: browserLock() })
  const reader = await IdbCredentialStore.open({ factory })
  const seenAtArrival: { token: string; held: string }[] = []
  r.net.onSent = (q) => {
    if (q.path === '/sync') void reader.read().then((h) => seenAtArrival.push({ token: q.token, held: h!.accessToken }))
  }
  let chainAtRefresh = -1
  r.net.family.onRefresh = () => void reader.read().then((h) => (chainAtRefresh = h!.chain.length))
  assert.equal(await r.c.attemptIfDue(), 'refreshed')
  assert.equal(chainAtRefresh, 1, 'the proposal was on disk when the /refresh arrived')
  const rotated = await reader.read()
  assert.equal(rotated?.accessToken, padToken('access-1'))
  assert.deepEqual(rotated?.chain, [])
  // The next request presents the rotated pair, which a second opener already reads.
  const { t } = r.transport()
  t.start()
  for (let i = 0; i < 20 && seenAtArrival.length === 0; i++) await new Promise((res) => setImmediate(res))
  t.stop()
  assert.ok(seenAtArrival.length > 0)
  for (const s of seenAtArrival) assert.equal(s.token, s.held)
  reader.close()
})

test('criterion 29 · a read-modify-write under the lock sees another context’s write', async () => {
  const factory = new IDBFactory()
  const a = await seeded(factory)
  const b = await IdbCredentialStore.open({ factory })
  const lock = browserLock()
  const name = `catenary.credential.${DEVICE}`
  let release!: () => void
  const holding = new Promise<void>((res) => (release = res))
  let aHasLock!: () => void
  const aLocked = new Promise<void>((res) => (aHasLock = res))
  // Context A takes the lock and, while holding it, rotates the pair.
  const aDone = lock(name, async () => {
    aHasLock()
    await holding
    await a.update((h) => ({ write: { ...h!, accessToken: padToken('access-by-a') }, result: undefined }))
  })
  await aLocked
  // Context B queues on the same lock, then re-reads under it.
  const bSaw = lock(name, () => b.update((h) => ({ result: h!.accessToken })))
  release()
  await aDone
  assert.equal(await bSaw, padToken('access-by-a'))
  a.close()
  b.close()
})

test('criterion 29 · two transports over one database send exactly one /refresh (Web Locks, two connections)', async () => {
  const factory = new IDBFactory()
  const one = await seeded(factory)
  const two = await IdbCredentialStore.open({ factory })
  const r = credRig({ store: one, lock: browserLock() })
  const first = r.transport(r.c).t
  const secondLayer = createRefreshingCredential({
    baseUrl: 'http://catenary.test', store: two, lock: browserLock(), fetch: r.net.fetch, now: r.clock.now, timers: r.clock, logger: r.logger,
  })
  const second = r.transport(secondLayer).t
  await Promise.all([first.refreshIfDue(), second.refreshIfDue()])
  assert.equal(r.net.count('/refresh'), 1)
  assert.equal(r.c.status().refreshes + secondLayer.status().refreshes, 1)
  assert.equal(r.c.status().refreshesSkipped + secondLayer.status().refreshesSkipped, 1, 'the other re-read under the lock and skipped')
  assert.equal((await two.read())?.accessToken, padToken('access-1'))
  assert.equal(noReplays(r.net.family), null)

  // The control: the same race without the lock refreshes the pair twice.
  const f2 = new IDBFactory()
  const u1 = await seeded(f2)
  const u2 = await IdbCredentialStore.open({ factory: f2 })
  const u = credRig({ store: u1, lock: browserLock(), faults: { refreshUnlocked: true } })
  const uOther = createRefreshingCredential({
    baseUrl: 'http://catenary.test', store: u2, lock: browserLock(), fetch: u.net.fetch, now: u.clock.now, timers: u.clock,
    logger: u.logger, faults: { refreshUnlocked: true },
  })
  await Promise.all([u.c.attemptIfDue(), uOther.attemptIfDue()])
  assert.ok(u.net.count('/refresh') >= 2, 'refreshUnlocked: both contexts refreshed')
  for (const s of [one, two, u1, u2]) s.close()
})
