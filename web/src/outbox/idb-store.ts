/* The IndexedDB `OutboxStore` — ruling 1, and the only durable one.
 *
 * DATABASE `catenary-outbox`, OWNED BY THE OUTBOX ALONE. It is not a store in
 * CANT-35's `catenary` database: two tickets' stores on one version lineage
 * would need one opener owning every upgrade, and a `deleteDatabase('catenary')`
 * — CANT-24 obligation 4's discard-and-bootstrap — must not be able to reach an
 * unsent message. This file never opens `catenary`, and nothing there opens
 * this. Every future upgrade step of `catenary-outbox` belongs here.
 *
 * STRICT DURABILITY ON EVERY ENTRY WRITE. Chromium's default is relaxed, where
 * `complete` means visible to other transactions rather than flushed, and a row
 * the UI shows QUEUED has to survive a power cut just after compose. Every
 * operation resolves on its transaction's `complete`, never on a request's
 * `success` — a request can succeed inside a transaction that then aborts.
 */

import type { Uuid } from '@/wire/generated'
import type { OutboxEntry, OutboxStore, StoreFaults } from './types'
import { byOrder } from './memory-store'

export const OUTBOX_DB = 'catenary-outbox'
const STORE = 'outbox'
const BY_ACCOUNT_ORDER = 'by_account_order'
const VERSION = 1

export interface IdbDeps {
  factory: IDBFactory
  keyRange: typeof IDBKeyRange
}

export class IdbOutboxStore implements OutboxStore {
  private constructor(
    private readonly db: IDBDatabase,
    private readonly keyRange: typeof IDBKeyRange,
    private readonly faults: StoreFaults,
  ) {}

  static async open(
    deps: IdbDeps = { factory: indexedDB, keyRange: IDBKeyRange },
    faults: StoreFaults = {},
    name = OUTBOX_DB,
  ): Promise<IdbOutboxStore> {
    const db = await new Promise<IDBDatabase>((resolve, reject) => {
      const req = deps.factory.open(name, VERSION)
      req.onupgradeneeded = () => {
        const store = req.result.createObjectStore(STORE, { keyPath: 'clientId' })
        // Unique: two entries of one account can never share an `order`, so a
        // collision is a refused write rather than a silent tie in the drain.
        store.createIndex(BY_ACCOUNT_ORDER, ['accountId', 'order'], { unique: true })
      }
      req.onsuccess = () => resolve(req.result)
      req.onerror = () => reject(req.error)
      req.onblocked = () => reject(new Error(`${name}: open blocked by another connection`))
    })
    // A future upgrade from another tab must never be blocked by this one.
    db.onversionchange = () => db.close()
    return new IdbOutboxStore(db, deps.keyRange, faults)
  }

  private write(): IDBTransaction {
    return this.faults.relaxedDurability
      ? this.db.transaction(STORE, 'readwrite')
      : this.db.transaction(STORE, 'readwrite', { durability: 'strict' })
  }

  /** Resolves on `complete` — and, under `neverCompletes`, aborts the
   *  transaction after its last request so `complete` never comes. */
  private done<T>(tx: IDBTransaction, value: () => T): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      tx.oncomplete = () => resolve(value())
      // A failed request aborts its transaction, and `tx.error` then names
      // why (a ConstraintError on the order index, say).
      tx.onabort = () => reject(tx.error ?? new Error('outbox transaction aborted'))
    })
  }

  add(entry: Omit<OutboxEntry, 'order'>): Promise<OutboxEntry> {
    const tx = this.write()
    const store = tx.objectStore(STORE)
    let stored: OutboxEntry | undefined
    const range = this.keyRange.bound([entry.accountId, -Infinity], [entry.accountId, Infinity])
    const cursor = store.index(BY_ACCOUNT_ORDER).openCursor(range, 'prev')
    cursor.onsuccess = () => {
      const highest = (cursor.result?.value as OutboxEntry | undefined)?.order ?? 0
      stored = { ...entry, order: highest + 1 } as OutboxEntry
      const put = store.put(stored)
      if (this.faults.neverCompletes) put.onsuccess = () => tx.abort()
    }
    return this.done(tx, () => stored!)
  }

  put(entry: OutboxEntry): Promise<void> {
    const tx = this.write()
    const put = tx.objectStore(STORE).put(entry)
    if (this.faults.neverCompletes) put.onsuccess = () => tx.abort()
    return this.done(tx, () => undefined)
  }

  update(clientId: Uuid, mutate: (e: OutboxEntry) => void): Promise<OutboxEntry | undefined> {
    const tx = this.write()
    const store = tx.objectStore(STORE)
    let next: OutboxEntry | undefined
    const get = store.get(clientId)
    get.onsuccess = () => {
      const held = get.result as OutboxEntry | undefined
      if (!held) return
      mutate(held)
      next = held
      store.put(held)
    }
    return this.done(tx, () => next)
  }

  delete(clientId: Uuid): Promise<void> {
    const tx = this.write()
    tx.objectStore(STORE).delete(clientId)
    return this.done(tx, () => undefined)
  }

  list(): Promise<OutboxEntry[]> {
    const tx = this.db.transaction(STORE, 'readonly')
    const req = tx.objectStore(STORE).getAll()
    return this.done(tx, () => (req.result as OutboxEntry[]).sort(byOrder))
  }

  close(): void {
    this.db.close()
  }
}
