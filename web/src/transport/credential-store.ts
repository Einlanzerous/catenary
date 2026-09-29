/* The durable credential — mirrors the credential half of
 * internal/client/journal.go (`Credential`, `ChainLink`, `Journal.Enroll`,
 * `Journal.Reenroll`, `Journal.Rotate`, `Journal.propose`), with the store
 * behind a seam: IndexedDB in a browser, memory in a test or the Node driver.
 *
 * CANT-31 §2 and §3 (docs/decisions/cant-31-refresh-and-terminal-reconnect.md).
 * The record is the device's pair, the `Date` offset and issue time §1 persists
 * with it, the proposal chain §3 persists before each request, and CANT-127's
 * `last_sent_at`. EVERY WRITE IS ONE ATOMIC READ-MODIFY-WRITE (`update`), and a
 * write resolves only once it is durable — PERSIST BEFORE PRESENT. The
 * single-flight lock is the caller's (refresh.ts), taken around the whole
 * refresh, so a waiter that re-reads under it sees what the holder wrote.
 *
 * NOTHING HERE IS CLEARED BY A TERMINAL OR BY A JOURNAL WIPE (§6). The only
 * doors that replace a held pair with one that did not descend from it are
 * `enrollCredential` (refuses if one is held) and `reenrollCredential` (a
 * person re-enrolling a device the server stopped recognising).
 */

import type { EnrollResponse, Uuid } from '@/wire/generated'
import { CREDENTIAL_STORE, openCatenaryDb, type OpenCatenaryDbOptions } from './db'
import type { Lock } from './seams'

/** One refresh this device sent and has not seen answered: the token it
 *  presented and the successor it proposed. THE PROPOSAL IS THE ONE THE TOKEN
 *  WAS FIRST PRESENTED WITH, and a token presented again carries it again. */
export interface ChainLink {
  token: string
  proposal: string
}

/** What a device holds to get back in. Times are wall-clock ms since the epoch. */
export interface StoredCredential {
  /** `EnrollResponse.user_id`: who this device speaks as. */
  userId: Uuid
  /** `EnrollResponse.device_id`. A rotation never changes it. */
  deviceId: Uuid
  accessToken: string
  /** Server clock. */
  accessExpiresAt: number
  /** Single-use: presenting it rotates the pair (CANT-29). */
  refreshToken: string
  refreshExpiresAt: number
  /** The `Date` of the response that served this pair; null when it was never
   *  learned, which gets the 60 s floor (CANT-31 §1). */
  accessIssuedAt: number | null
  /** Server minus device, from that same response; 0 when never learned. */
  clockOffsetMs: number
  /** THE INVARIANT: `chain[0].token` is `refreshToken`, and each later link's
   *  token is the one before's proposal. Empty whenever the last refresh was
   *  answered. */
  chain: ChainLink[]
  /** CANT-127's `last_sent_at`: when the newest link was written, before its
   *  request left. Null with an empty chain. */
  lastSentAt: number | null
}

/**
 * The persisted credential. `update` is ONE atomic read-modify-write: `fn` runs
 * synchronously against what is stored now and says what to store, and the
 * promise resolves once that write is durable. Two contexts over one store are
 * two tabs over one origin's storage.
 */
export interface CredentialStore {
  read(): Promise<StoredCredential | null>
  update<T>(fn: (held: StoredCredential | null) => { write?: StoredCredential; result: T }): Promise<T>
}

/** CANT-31 §2's lock, named per credential. */
export function credentialLockName(deviceId: Uuid): string {
  return `catenary.credential.${deviceId}`
}

const copy = (c: StoredCredential): StoredCredential => ({ ...c, chain: c.chain.map((l) => ({ ...l })) })

/** A store in memory: tests, and a host with nowhere durable to put it. Copies
 *  in and out, as IndexedDB's structured clone does. */
export class MemoryCredentialStore implements CredentialStore {
  private held: StoredCredential | null

  constructor(initial: StoredCredential | null = null) {
    this.held = initial ? copy(initial) : null
  }

  async read(): Promise<StoredCredential | null> {
    return this.held ? copy(this.held) : null
  }

  async update<T>(fn: (held: StoredCredential | null) => { write?: StoredCredential; result: T }): Promise<T> {
    const { write, result } = fn(this.held ? copy(this.held) : null)
    if (write) this.held = copy(write)
    return result
  }
}

/**
 * The browser's store: database `catenary`, store `credential`, one record per
 * device. `update` is one readwrite transaction and resolves on `oncomplete`,
 * so a rotated pair is on disk before anything presents it.
 */
export class IdbCredentialStore implements CredentialStore {
  private constructor(private readonly db: IDBDatabase) {}

  static async open(opts: OpenCatenaryDbOptions = {}): Promise<IdbCredentialStore> {
    return new IdbCredentialStore(await openCatenaryDb(opts))
  }

  close(): void {
    this.db.close()
  }

  read(): Promise<StoredCredential | null> {
    return new Promise((resolve, reject) => {
      const tx = this.db.transaction(CREDENTIAL_STORE, 'readonly')
      const req = tx.objectStore(CREDENTIAL_STORE).getAll()
      tx.oncomplete = () => resolve(IdbCredentialStore.only(req.result as StoredCredential[]))
      tx.onerror = () => reject(tx.error ?? new Error('credential store: read failed'))
      tx.onabort = () => reject(tx.error ?? new Error('credential store: read aborted'))
    })
  }

  update<T>(fn: (held: StoredCredential | null) => { write?: StoredCredential; result: T }): Promise<T> {
    return new Promise((resolve, reject) => {
      const tx = this.db.transaction(CREDENTIAL_STORE, 'readwrite')
      const store = tx.objectStore(CREDENTIAL_STORE)
      let result: T
      const req = store.getAll()
      req.onsuccess = () => {
        const held = IdbCredentialStore.only(req.result as StoredCredential[])
        let out: { write?: StoredCredential; result: T }
        try {
          out = fn(held)
        } catch (e) {
          tx.abort()
          reject(e)
          return
        }
        result = out.result
        if (out.write) {
          // ONE RECORD PER DEVICE, and one device: a re-enrollment's new id
          // replaces the old device's record in the same transaction.
          if (held && held.deviceId !== out.write.deviceId) store.delete(held.deviceId)
          store.put(out.write)
        }
      }
      tx.oncomplete = () => resolve(result)
      tx.onerror = () => reject(tx.error ?? new Error('credential store: write failed'))
      tx.onabort = () => reject(tx.error ?? new Error('credential store: write aborted'))
    })
  }

  private static only(rows: StoredCredential[]): StoredCredential | null {
    return rows.length === 0 ? null : rows[0]
  }
}

/**
 * The pair `POST /enroll` minted, as durable state (Go's `CredentialFromEnroll`
 * plus `WithServerDate`). `date` is the response's HTTP `Date` header and
 * `deviceNow` the wall clock when it arrived: the one moment the two clocks are
 * known to describe the same instant. With no usable `Date` the pair has no
 * offset and no issue time, and the proactive check reads it against the 60 s
 * floor until its first rotation.
 */
export function credentialFromEnroll(e: EnrollResponse, date: string | null, deviceNow: number): StoredCredential {
  const cred: StoredCredential = {
    userId: e.userId,
    deviceId: e.deviceId,
    accessToken: e.accessToken,
    accessExpiresAt: parseWireTime(e.accessExpiresAt, 'access_expires_at'),
    refreshToken: e.refreshToken,
    refreshExpiresAt: parseWireTime(e.refreshExpiresAt, 'refresh_expires_at'),
    accessIssuedAt: null,
    clockOffsetMs: 0,
    chain: [],
    lastSentAt: null,
  }
  const d = date === null ? NaN : Date.parse(date)
  return Number.isFinite(d) ? { ...cred, accessIssuedAt: d, clockOffsetMs: d - deviceNow } : cred
}

export function parseWireTime(s: string, field: string): number {
  const t = Date.parse(s)
  if (!Number.isFinite(t)) throw new Error(`credential: ${field} is not a timestamp`)
  return t
}

/** `enrollCredential` over a store that already holds a pair. */
export class CredentialHeld extends Error {
  constructor() {
    super('credential: this store already holds a credential; re-enroll to replace it')
    this.name = 'CredentialHeld'
  }
}

/** `reenrollCredential` over a store nobody enrolled. */
export class NoCredential extends Error {
  constructor() {
    super('credential: this store holds no credential')
    this.name = 'NoCredential'
  }
}

const complete = (c: StoredCredential) => c.deviceId !== '' && c.accessToken !== '' && c.refreshToken !== ''

/**
 * Seeds the store with a first enrollment's pair, ONCE (Go's `Journal.Enroll`).
 * A store that already holds one refuses: the held pair may be a rotation ahead
 * of the one the caller has, and overwriting it is the spent-token restart
 * CANT-121 exists to prevent. Under the credential's lock, as every
 * read-modify-write is.
 */
export function enrollCredential(store: CredentialStore, lock: Lock, cred: StoredCredential): Promise<void> {
  if (!complete(cred)) return Promise.reject(new Error('credential: a pair needs a device id, an access token and a refresh token'))
  const fresh: StoredCredential = { ...copy(cred), chain: [], lastSentAt: null }
  return lock(credentialLockName(cred.deviceId), () =>
    store.update((held) => {
      if (held) throw new CredentialHeld()
      return { write: fresh, result: undefined }
    }),
  )
}

/**
 * Replaces the held pair with a NEW enrollment's — a different device, as far as
 * the server is concerned (Go's `Journal.Reenroll`). The only thing that ends a
 * credential terminal (§6). The old device's chain and stamp mean nothing now
 * and go with it.
 */
export async function reenrollCredential(store: CredentialStore, lock: Lock, cred: StoredCredential): Promise<void> {
  if (!complete(cred)) throw new Error('credential: a pair needs a device id, an access token and a refresh token')
  const held = await store.read()
  if (!held) throw new NoCredential()
  const fresh: StoredCredential = { ...copy(cred), chain: [], lastSentAt: null }
  await lock(credentialLockName(held.deviceId), () =>
    store.update((now) => {
      if (!now) throw new NoCredential()
      return { write: fresh, result: undefined }
    }),
  )
}
