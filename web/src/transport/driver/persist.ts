/* The Node driver's durable backing for the journal (CANT-169) — the stand-in,
 * in a process with no browser disk, for the IndexedDB a browser keeps across a
 * page load. Mirrors nothing in internal/client: the Go client's journal
 * (internal/client/journal.go) outlives a killed Client by being a struct the
 * rig keeps, and a TypeScript client's must outlive a killed PROCESS, so it
 * goes to a file.
 *
 * THE DATABASE IS fake-indexeddb, and `IdbJournal` runs over it unchanged: the
 * same `openCatenaryDb()`, the same one transaction per page. What fake-indexeddb
 * lacks is a disk, so `IdbJournal`'s `durable` hook — awaited after a write's
 * `oncomplete` and before anything it carries is observable — writes every
 * store of the `catenary` database to `file`, whole, through a temporary file
 * and a rename. A SIGKILL at any instant leaves the file as it was after some
 * completed write, never between two, and never ahead of what the driver has
 * shown: the rename is the durability point, as `oncomplete` is a browser's.
 *
 * A relaunch reads the file back into a fresh fake-indexeddb, through the
 * database's own upgrade steps and one readwrite transaction, before the
 * journal is opened over it.
 */

import { existsSync } from 'node:fs'
import { readFile, rename, writeFile } from 'node:fs/promises'
import { IDBFactory } from 'fake-indexeddb'
import { openCatenaryDb } from '../db'
import { IdbJournal } from '../idb-journal'

/** Every store of the database, as its records. */
type Dump = Record<string, unknown[]>

/** Opens the durable journal persisted at `file`, creating it if absent. */
export async function openFileJournal(file: string): Promise<IdbJournal> {
  const factory = new IDBFactory()
  if (existsSync(file)) await restore(factory, JSON.parse(await readFile(file, 'utf8')) as Dump)
  const db = await openCatenaryDb({ factory })
  const tmp = `${file}.tmp`
  return IdbJournal.open({
    factory,
    durable: async () => {
      await writeFile(tmp, JSON.stringify(await dump(db)))
      await rename(tmp, file)
    },
  })
}

function dump(db: IDBDatabase): Promise<Dump> {
  return new Promise((resolve, reject) => {
    const names = [...db.objectStoreNames]
    const tx = db.transaction(names, 'readonly')
    const reqs = names.map((n) => [n, tx.objectStore(n).getAll()] as const)
    tx.oncomplete = () => resolve(Object.fromEntries(reqs.map(([n, r]) => [n, r.result as unknown[]])))
    tx.onabort = () => reject(tx.error ?? new Error('driver: the journal dump aborted'))
  })
}

async function restore(factory: IDBFactory, d: Dump): Promise<void> {
  const db = await openCatenaryDb({ factory })
  try {
    await new Promise<void>((resolve, reject) => {
      const names = Object.keys(d)
      const tx = db.transaction(names, 'readwrite')
      for (const n of names) for (const rec of d[n]) tx.objectStore(n).put(rec)
      tx.oncomplete = () => resolve()
      tx.onabort = () => reject(tx.error ?? new Error('driver: the journal restore aborted'))
    })
  } finally {
    db.close()
  }
}
