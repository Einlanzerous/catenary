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
    storePromise =
      typeof indexedDB === 'undefined' ? Promise.resolve(new MemoryCredentialStore()) : IdbCredentialStore.open()
  }
  return storePromise
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

function enrollErrorText(e: unknown): string {
  if (!(e instanceof EnrollRefused)) return 'Could not reach the server — check your connection and try again.'
  if (e.status === 400) return 'That does not look like a valid enrollment token or device name.'
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
    const stored = await enrollDevice(
      { baseUrl: seams.baseUrl, ...(seams.fetch ? { fetch: seams.fetch } : {}) },
      enrollmentToken,
      deviceName,
    )
    const store = await openStore()
    const lock = seams.lock ?? browserLock()
    const held = await store.read()
    if (held) await reenrollCredential(store, lock, stored)
    else await enrollCredential(store, lock, stored)
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
