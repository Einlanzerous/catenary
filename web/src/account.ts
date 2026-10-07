/* CANT-38 — the account surface: enrollment, device naming and the session
 * list, as conventional forms over CANT-28/29/30/117's REST endpoints.
 *
 * STORES THE CREDENTIAL AND SHOWS THE FORMS. It reuses CANT-152's credential
 * layer (`@/transport`) — `enrollDevice`, `IdbCredentialStore`,
 * `enrollCredential`/`reenrollCredential`, `store.read()` — rather than a
 * second store, and it does NOT start the live transport itself: that is
 * `store.ts`'s `startSession` (CANT-39). The seam between the two is
 * `onEnrolled` — a login or a re-enrollment that has written a new pair to
 * the store says so, and whoever composed the app (main.ts) restarts the
 * session over it. This module never imports the transport wiring, so the
 * forms stay testable over a scripted fetch with no socket in sight.
 *
 * `createRefreshingCredential` IS used here, for the authenticated reads
 * below (`GET /devices`, `POST /devices/{id}/revoke`) — that is CANT-31 §1–§3's
 * safe, single-flight refresh, not the socket. Rolling a refresh by hand here
 * would risk exactly the replay CANT-29 exists to punish: a lost `/refresh`
 * response leaves the stored refresh token spent, and a naive retry becomes a
 * replay once the reuse grace window passes. Every authenticated call below
 * mirrors `transport.ts`'s own `syncOnce`/`fetchPage` shape for that reason —
 * one call, Catenary's own 401 read from the BODY and not just the status,
 * one retry through `onSyncUnauthorized`, `answered()` on a genuine answer.
 */

import { reactive } from 'vue'
import {
  browserLock,
  createRefreshingCredential,
  EnrollAnswerUnreadable,
  enrollCredential,
  enrollDevice,
  EnrollRefused,
  IdbCredentialStore,
  isCatenaryUnauthorized,
  MemoryCredentialStore,
  NOT_TERMINAL,
  reenrollCredential,
  type Credential,
  type CredentialHost,
  type CredentialSeam,
  type CredentialStore,
  type Lock,
  type Terminal,
} from '@/transport'
import { decodeDeviceListResponse, type Device } from '@/wire/generated'

export type AccountMode = 'login' | 'sessions'

interface AccountState {
  /** 'login' until a credential is found or minted; 'sessions' once one is
   *  held. The honest SSR default — nothing durable has been read yet, same
   *  reasoning as ConnectionBanner's "no number before a page has landed". */
  mode: AccountMode
  busy: boolean
  error: string | null
  /** Set when a credential was stored and the session over it would not start
   *  (`restartAfterEnrollment`). Its own field: `error` is cleared and rewritten
   *  by the device list's loads, which would swallow it (CANT-240). */
  sessionError: string | null
  /** The caller's own devices, oldest first, revoked ones included — exactly
   *  as `GET /devices` serves them (CANT-117). */
  devices: Device[]
  /** This browser's own device id — `EnrollResponse.device_id` /
   *  `StoredCredential.deviceId` — set whenever a credential is minted or
   *  read. Lets the session list mark which row is THIS device: CANT-117
   *  allows revoking it, and doing so ends the tab making the request, which
   *  is worth a person seeing before they click rather than discovering it. */
  deviceId: string | null
  /** Set only by the credential layer's own terminal callback: a refresh
   *  Catenary itself refused (CANT-31 §5). ConnectionBanner names this "CANT-38
   *  designs the real experience, re-enrollment included" — the sessions view
   *  below is that experience. */
  terminal: Terminal
}

export const accountState: AccountState = reactive({
  mode: 'login',
  busy: false,
  error: null,
  sessionError: null,
  devices: [],
  deviceId: null,
  terminal: NOT_TERMINAL,
})

export interface AccountSeams {
  /** The server's origin; every route below is appended to it. Default ''
   *  (same origin) — the Go binary serves the API and this static app
   *  together, so a relative path is the right one in production. */
  baseUrl?: string
  fetch?: typeof globalThis.fetch
  store?: CredentialStore
  lock?: Lock
}

let seams: AccountSeams & { baseUrl: string } = { baseUrl: '' }
let storePromise: Promise<CredentialStore> | null = null
let cred: CredentialSeam | null = null

const host: CredentialHost = {
  terminal(t) {
    accountState.terminal = t
  },
  notify() {},
}

/**
 * Rebuilds the account layer over new seams — `smoke.ts` passes a
 * `MemoryCredentialStore` and a scripted fetch, exactly as `useOutbox` does
 * for CANT-36's store. Resets every reactive field: a reconfigure is a new
 * browser context, not a continuation of the old one.
 */
export function configureAccount(next: AccountSeams = {}): void {
  seams = { baseUrl: '', ...next }
  storePromise = null
  cred = null
  accountState.mode = 'login'
  accountState.busy = false
  accountState.error = null
  accountState.devices = []
  accountState.deviceId = null
  accountState.terminal = NOT_TERMINAL
}

function openStore(): Promise<CredentialStore> {
  if (seams.store) return Promise.resolve(seams.store)
  if (!storePromise) {
    // A STORE THAT WOULD NOT OPEN IS NOT REMEMBERED. `login()` tells a person
    // whose storage is blocked to allow it and try again, and a rejection
    // cached here would answer that second try without ever asking the
    // browser again.
    const opening: Promise<CredentialStore> =
      typeof indexedDB === 'undefined' ? Promise.resolve(new MemoryCredentialStore()) : IdbCredentialStore.open()
    storePromise = opening
    opening.catch(() => {
      if (storePromise === opening) storePromise = null
    })
  }
  return storePromise
}

/**
 * The store, opened and read once — `login()`'s question before it spends a
 * token. Two things here are not "it opened, so it will do":
 *
 * NOWHERE DURABLE IS A REFUSAL. `openStore()`'s in-memory fallback is for a
 * host with no `indexedDB` at all, and a login over it would look signed in
 * while holding a pair the next reload forgets, with the token gone.
 *
 * A HANDLE THAT DIED IS NOT THE BROWSER SAYING NO. The database closes itself
 * under a tab when another tab upgrades it (db.ts's `onversionchange`), and
 * every read on the cached handle then throws. So a failed read drops the
 * cache and asks once more over a fresh open before calling it unavailable.
 */
async function readableStore(): Promise<CredentialStore> {
  if (seams.store) {
    await seams.store.read()
    return seams.store
  }
  if (typeof indexedDB === 'undefined') throw new Error('account: no IndexedDB in this context')
  try {
    const store = await openStore()
    await store.read()
    return store
  } catch {
    storePromise = null
    const store = await openStore()
    await store.read()
    return store
  }
}

/** The credential store these forms write — the one `startSession` must
 *  read, so a login and the session it starts agree on which pair is held. */
export const credentialStore = (): Promise<CredentialStore> => openStore()

const enrolledFns = new Set<() => void>()

/**
 * Called after every login or re-enrollment that has written a new pair to
 * the store — the moment the app must start (or restart) its transport over
 * it. Returns the unsubscribe. A listener that throws does not stop the
 * others, and never turns a successful login into a failed one.
 */
export function onEnrolled(fn: () => void): () => void {
  enrolledFns.add(fn)
  return () => enrolledFns.delete(fn)
}

async function credential(): Promise<CredentialSeam> {
  if (cred) return cred
  const store = await openStore()
  const built = createRefreshingCredential({
    baseUrl: seams.baseUrl,
    store,
    ...(seams.fetch ? { fetch: seams.fetch } : {}),
    ...(seams.lock ? { lock: seams.lock } : {}),
  })
  built.attach(host)
  cred = built
  return built
}

function httpFetch(path: string, init?: RequestInit): Promise<Response> {
  return (seams.fetch ?? globalThis.fetch)(path, init)
}

/** Set by `beginReenroll()` and consumed by the very next
 *  `checkExistingCredential()` — the one AccountView's `onMounted` runs. A
 *  held (possibly dead) credential is exactly what a re-enroll is replacing,
 *  so THIS one mount must not auto-navigate past the form it was opened to
 *  show. Module-level rather than on `accountState` itself: it is intent for
 *  the next mount, not something a render should ever read. */
let pendingReenroll = false

/**
 * Checked once on mount (AccountView's `onMounted` — never during SSR, since
 * IndexedDB does not exist there): a credential already on this device skips
 * straight to the session list rather than asking to log in again — UNLESS
 * this mount was opened by `requestReenrollBeforeMount()` (ConnectionBanner's
 * RE-ENROLL, clicked before the account view exists at all), in which case
 * staying on the login form is the entire point of the click.
 */
export async function checkExistingCredential(): Promise<void> {
  if (pendingReenroll) {
    pendingReenroll = false
    return
  }
  const store = await openStore()
  const held = await store.read()
  accountState.mode = held ? 'sessions' : 'login'
  accountState.deviceId = held?.deviceId ?? null
  if (held) await loadDevices()
}

/**
 * The credential store would not open, or would not be read, BEFORE the token
 * was sent. Nothing has been spent, which is the whole reason `login()` asks
 * first: a browser that blocks storage blocks it on every try, and without
 * this each try would burn one single-use token to learn the same thing.
 */
class StorageUnavailable extends Error {
  constructor(cause: unknown) {
    super('account: the credential store could not be opened or read', { cause })
    this.name = 'StorageUnavailable'
  }
}

/**
 * `POST /enroll` answered 200 with a pair, and this device then failed to keep
 * it (CANT-228, the web's half of CANT-220 ruling 8). The token is spent by
 * then, so the unreachable text's "try again" would be refused.
 */
class CredentialNotStored extends Error {
  constructor(cause: unknown) {
    super('account: the server issued a credential and this device could not store it', { cause })
    this.name = 'CredentialNotStored'
  }
}

/** The app's `_notStored` (app/lib/store/enrollment.dart), word for word — one
 *  event, one sentence, on both clients. `account.test.ts` reads the Dart
 *  source and fails when either moves alone. */
export const CREDENTIAL_NOT_STORED_TEXT =
  'The server accepted that token, but this device could not save the credential. The token is now used — ask whoever invited you for a fresh one.'

/**
 * TOLD APART BY TYPE, never by matching a message. What is left once the typed
 * cases are gone is a failure before any status arrived — the request itself —
 * and that alone is "could not reach the server". Of the statuses, only 400
 * and 401 are Catenary refusing (internal/api/router.go); any other — its own
 * 500, or a 502, 404 or 429 from a hop in front of it — is the server failing
 * to answer, and it does not get to tell a person to throw away a token nobody
 * refused.
 */
function enrollErrorText(e: unknown): string {
  if (e instanceof CredentialNotStored) return CREDENTIAL_NOT_STORED_TEXT
  if (e instanceof StorageUnavailable) {
    return 'This browser would not let Catenary store a credential, so the token was not sent and is still unused. Allow storage for this site — a private window may block it — and try again.'
  }
  // A 200 THAT COULD NOT BE READ. Not the storage text — nothing reached the
  // store — and not a promise that the token is spent either: Catenary's own
  // 200 has spent it, but a 200 from something in front of Catenary (a captive
  // portal's page) has not, and from here the two look the same. So it says
  // what is known and what to do in both cases (invariant 3).
  if (e instanceof EnrollAnswerUnreadable) {
    return 'The server answered, but not with anything this app could read. The token may already be used — if trying again is refused, ask whoever invited you for a fresh one.'
  }
  if (!(e instanceof EnrollRefused)) return 'Could not reach the server — check your connection and try again.'
  if (e.status === 400) return 'That does not look like a valid enrollment token or device name.'
  if (e.status !== 401) return 'The server could not answer just now — the token was not refused. Try again in a moment.'
  // CANT-28's ONE REFUSAL SHAPE: unknown, expired, already-redeemed and
  // deactivated-account tokens all answer identically, on purpose — so this
  // is the one message for all of them too.
  return 'That enrollment token was not accepted — it may be wrong, expired or already used. Ask whoever invited you for a fresh one.'
}

/**
 * `POST /enroll`, the login form's submit. First enrollment on a fresh
 * device (`enrollCredential`) unless this device already holds a credential
 * — including one Catenary stopped recognizing, CANT-31 §6's credential
 * terminal — in which case the SAME form re-enrolls it (`reenrollCredential`).
 */
export async function login(enrollmentToken: string, deviceName: string): Promise<boolean> {
  accountState.busy = true
  accountState.error = null
  try {
    // ASKED BEFORE THE TOKEN IS SPENT (`readableStore`), so that storage this
    // browser will not give — the common way to fail — costs nothing. It
    // proves the store opens and reads, not that the write below will land (a
    // full quota still fails there), and what it reads is discarded: which of
    // enroll and re-enroll applies is decided on the read after the answer, as
    // it always was.
    let store: CredentialStore
    try {
      store = await readableStore()
    } catch (e) {
      throw new StorageUnavailable(e)
    }
    const stored = await enrollDevice(
      { baseUrl: seams.baseUrl, ...(seams.fetch ? { fetch: seams.fetch } : {}) },
      enrollmentToken,
      deviceName,
    )
    // Past this line the token is spent, and a failure is this device's. It is
    // NOT swallowed to let the screen proceed: a session that looks logged in
    // and holds no durable credential is a worse lie than the error text.
    try {
      const lock = seams.lock ?? browserLock()
      const held = await store.read()
      if (held) await reenrollCredential(store, lock, stored)
      else await enrollCredential(store, lock, stored)
    } catch (e) {
      throw new CredentialNotStored(e)
    }
    // The store just changed under whatever seam was built before; a stale
    // one would answer from the OLD pair's cache. Rebuilt lazily, next use.
    cred = null
    accountState.terminal = NOT_TERMINAL
    accountState.mode = 'sessions'
    accountState.deviceId = stored.deviceId
    for (const fn of [...enrolledFns]) {
      try {
        fn()
      } catch (e) {
        console.error('catenary account: an onEnrolled listener threw', e)
      }
    }
    await loadDevices()
    return true
  } catch (e) {
    // The text is all a person sees; the cause is for whoever is asked why.
    if (e instanceof Error && e.cause !== undefined) console.error(`catenary account: ${e.message}`, e.cause)
    accountState.error = enrollErrorText(e)
    return false
  } finally {
    accountState.busy = false
  }
}

/** One `GET /devices`; null for Catenary's own 401 — mirrors
 *  `transport.ts`'s `syncOnce`, the one place this protocol already reads a
 *  refusal from the body rather than the status alone. */
async function devicesOnce(c: CredentialSeam, presented: Credential): Promise<Device[] | null> {
  const res = await httpFetch(`${seams.baseUrl}/devices`, {
    headers: { Authorization: `Bearer ${presented.accessToken}` },
  })
  const body = await res.text()
  if (isCatenaryUnauthorized(res.status, body)) {
    c.answered()
    return null
  }
  if (res.status !== 200) throw new Error(`devices: HTTP ${res.status}`)
  c.answered()
  return decodeDeviceListResponse(JSON.parse(body)).devices
}

/** `GET /devices` — the caller's own devices, as the generated wire type. */
export async function loadDevices(): Promise<void> {
  accountState.busy = true
  accountState.error = null
  try {
    const c = await credential()
    await c.refreshIfDue()
    const presented = await c.current()
    let devices = await devicesOnce(c, presented)
    if (devices === null) {
      devices = (await c.onSyncUnauthorized(presented)) ? await devicesOnce(c, await c.current()) : null
      if (devices === null) throw new Error('devices: 401 unauthorized')
    }
    accountState.devices = devices
  } catch {
    accountState.error = 'Could not load your devices — check your connection and try again.'
  } finally {
    accountState.busy = false
  }
}

/** One `POST /devices/{id}/revoke`; mirrors `devicesOnce` above. */
async function revokeOnce(c: CredentialSeam, presented: Credential, id: string): Promise<'ok' | 'unauthorized'> {
  const res = await httpFetch(`${seams.baseUrl}/devices/${id}/revoke`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${presented.accessToken}` },
  })
  if (res.status === 204) {
    c.answered()
    return 'ok'
  }
  const body = await res.text()
  if (isCatenaryUnauthorized(res.status, body)) {
    c.answered()
    return 'unauthorized'
  }
  throw new Error(`revoke: HTTP ${res.status}`)
}

/**
 * `POST /devices/{id}/revoke`. One 204 for not-yours, unknown and
 * already-revoked alike (CANT-117), so this always re-reads the list rather
 * than editing it locally — there is nothing here to distinguish "revoked"
 * from "was already revoked" from "not actually yours", by design.
 *
 * Revoking the device you are calling from is allowed and not special-cased
 * (CANT-117): the next authenticated call from THIS tab starts failing, and
 * the credential terminal above is what explains it when it does.
 */
export async function revokeDevice(id: string): Promise<void> {
  accountState.busy = true
  accountState.error = null
  try {
    const c = await credential()
    const presented = await c.current()
    let outcome = await revokeOnce(c, presented, id)
    if (outcome === 'unauthorized') {
      outcome = (await c.onSyncUnauthorized(presented)) ? await revokeOnce(c, await c.current(), id) : 'unauthorized'
      if (outcome === 'unauthorized') throw new Error('revoke: 401 unauthorized')
    }
    await loadDevices()
  } catch {
    accountState.error = 'Could not revoke that device — check your connection and try again.'
  } finally {
    accountState.busy = false
  }
}

/** Back to the login form after a credential terminal — AccountView's own
 *  RE-ENROLL banner, called on an already-mounted view. Nothing is cleared:
 *  `reenrollCredential` is the only thing that ever replaces a held pair
 *  (CANT-31 §6), and `login()` reaches it because a credential is still held. */
export function beginReenroll(): void {
  accountState.mode = 'login'
  accountState.error = null
}

/**
 * The same, for ConnectionBanner's RE-ENROLL — clicked from OUTSIDE the
 * account view, which is about to mount for the first time. `beginReenroll`
 * alone is not enough here: AccountView's `onMounted` runs
 * `checkExistingCredential()` right after, which would find the held (dead)
 * credential and navigate straight past the very form this click opened —
 * the pr-review finding this closes. `pendingReenroll` is consumed by that
 * one mount, so it never lingers past it.
 */
export function requestReenrollBeforeMount(): void {
  pendingReenroll = true
  beginReenroll()
}
