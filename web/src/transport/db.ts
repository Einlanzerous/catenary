/* The versioned open of CANT-35's own IndexedDB database, `catenary`. The
 * browser's counterpart of internal/client/journal.go's storage, which in Go is
 * a struct in memory and here is a database on disk.
 *
 * THIS IS THE ONLY CODE THAT OPENS `catenary`, and the database holds only
 * CANT-35's stores: `credential` (CANT-31 §2 and §3 require it durable), and
 * the journal's five (CANT-169, idb-journal.ts). CANT-36's outbox lives in its
 * own database, so the two never bump one version.
 *
 * AN UPGRADE IS A STEP APPENDED TO `UPGRADES`, never an edit to one already
 * shipped: the database's version is the list's length, and `onupgradeneeded`
 * runs exactly the steps between the stored version and that one, in order,
 * inside the one versionchange transaction.
 */

export const CATENARY_DB = 'catenary'
export const CREDENTIAL_STORE = 'credential'

/** The journal's stores (CANT-169). Messages, conversations and users are held
 *  as wire JSON keyed by `id`; `journal` holds the cursor and the wipe count,
 *  one record each, keyed by `key`; `counted` is the evidence log, keyed by
 *  its position `n`. */
export const MESSAGES_STORE = 'messages'
export const CONVERSATIONS_STORE = 'conversations'
export const USERS_STORE = 'users'
export const JOURNAL_STORE = 'journal'
export const COUNTED_STORE = 'counted'
export const JOURNAL_STORES = [MESSAGES_STORE, CONVERSATIONS_STORE, USERS_STORE, JOURNAL_STORE, COUNTED_STORE] as const

/** One upgrade step: runs once, inside the versionchange transaction. */
export type UpgradeStep = (db: IDBDatabase, tx: IDBTransaction) => void

export const UPGRADES: readonly UpgradeStep[] = [
  // 1 · the credential: one record per device, keyed by its id (CANT-31 §2).
  (db) => {
    db.createObjectStore(CREDENTIAL_STORE, { keyPath: 'deviceId' })
  },
  // 2 · the durable journal (CANT-169): cursor, messages, conversations, users
  // and the evidence log, written one transaction per page.
  (db) => {
    db.createObjectStore(MESSAGES_STORE, { keyPath: 'id' })
    db.createObjectStore(CONVERSATIONS_STORE, { keyPath: 'id' })
    db.createObjectStore(USERS_STORE, { keyPath: 'id' })
    db.createObjectStore(JOURNAL_STORE, { keyPath: 'key' })
    db.createObjectStore(COUNTED_STORE, { keyPath: 'n' })
  },
]

export interface OpenCatenaryDbOptions {
  /** Default: `globalThis.indexedDB`. Tests pass fake-indexeddb's. */
  factory?: IDBFactory
  /** Default: `catenary`. */
  name?: string
  /** Default: `UPGRADES`. A test passes a longer list to exercise a bump. */
  steps?: readonly UpgradeStep[]
}

/** Opens `catenary` at `steps.length`, running each missing step once. */
export function openCatenaryDb(opts: OpenCatenaryDbOptions = {}): Promise<IDBDatabase> {
  const factory = opts.factory ?? globalThis.indexedDB
  const steps = opts.steps ?? UPGRADES
  return new Promise((resolve, reject) => {
    if (!factory) {
      reject(new Error('catenary db: no IndexedDB in this context'))
      return
    }
    const req = factory.open(opts.name ?? CATENARY_DB, steps.length)
    req.onupgradeneeded = (ev) => {
      const db = req.result
      const tx = req.transaction!
      for (let v = ev.oldVersion; v < steps.length; v++) steps[v](db, tx)
    }
    req.onsuccess = () => {
      const db = req.result
      // ANOTHER TAB IS UPGRADING: step aside rather than block it. The next
      // open here gets the new version.
      db.onversionchange = () => db.close()
      resolve(db)
    }
    req.onerror = () => reject(req.error ?? new Error('catenary db: open failed'))
    req.onblocked = () => {
      // An older connection in another tab has not closed yet; its
      // onversionchange closes it, and this request then proceeds.
    }
  })
}
