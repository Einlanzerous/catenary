/* The injected seams — every source of nondeterminism the transport has comes
 * in through one of these. Mirrors the injection fields of `Config` in
 * internal/client/client.go (`Now`, `Pause`, `Rand`, `HTTPClient`), and adds
 * the browser's own: the WebSocket constructor, the lifecycle events a closed
 * laptop lid produces, and the lock CANT-31 §2 names. The browser defaults are
 * here; a test or the Node driver passes its own.
 */

/** The subset of the browser `WebSocket` the transport touches. Node 24's
 *  global `WebSocket` and a test's fake both satisfy it. */
export interface WebSocketLike {
  send(data: string): void
  close(code?: number, reason?: string): void
  onopen: ((ev: unknown) => void) | null
  onmessage: ((ev: { data: unknown }) => void) | null
  onclose: ((ev: { code: number; reason?: string }) => void) | null
  onerror: ((ev: unknown) => void) | null
}

export type WebSocketCtor = new (url: string, protocols: string[]) => WebSocketLike

/** Timers. The transport sleeps through nothing else. */
export interface Timers {
  setTimeout(fn: () => void, ms: number): unknown
  clearTimeout(handle: unknown): void
}

export const browserTimers: Timers = {
  setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms),
  clearTimeout: (h) => globalThis.clearTimeout(h as ReturnType<typeof globalThis.setTimeout>),
}

/**
 * The wall clock, in ms since the epoch. NEVER a monotonic clock: CANT-31 §1
 * forbids scheduling on one, because a laptop that slept measures six hours as
 * a few seconds on it. Durations the transport measures (a session's time
 * `ready`, the wake detector's gap) are read off this too, and a clock that
 * jumps is exactly what the wake detector exists to notice.
 */
export type Clock = () => number

/** `n` random bytes. The backoff jitter draws four; CANT-152's proposals draw 32. */
export type RandomBytes = (n: number) => Uint8Array

export const cryptoRandom: RandomBytes = (n) => globalThis.crypto.getRandomValues(new Uint8Array(n))

/**
 * CANT-31 §2's lock: `navigator.locks.request` in a browser, an in-process
 * mutex in Node. The transport core takes none — it is injected into the
 * credential layer (CANT-152), which is the only thing that holds a credential
 * across a read-modify-write.
 */
export type Lock = <T>(name: string, fn: () => Promise<T>) => Promise<T>

/** A mutex per name, for one process. Two transports sharing one of these are
 *  two tabs sharing one origin's Web Locks. */
export function inProcessLock(): Lock {
  const tails = new Map<string, Promise<unknown>>()
  return <T>(name: string, fn: () => Promise<T>): Promise<T> => {
    const prev = tails.get(name) ?? Promise.resolve()
    const run = prev.then(fn, fn)
    tails.set(name, run.catch(() => {}))
    return run
  }
}

/**
 * The browser's own: a Web Lock, which every tab of the origin respects, so a
 * refresh in one waits for a refresh in another (CANT-31 §2 — an in-memory
 * mutex is not enough, because two tabs share a credential and not memory).
 * Where `navigator.locks` is absent the fallback is one tab's mutex, which is
 * all such a browser can offer.
 */
export function browserLock(): Lock {
  const locks = (globalThis.navigator as { locks?: LockManager } | undefined)?.locks
  if (!locks || typeof locks.request !== 'function') return inProcessLock()
  return <T>(name: string, fn: () => Promise<T>): Promise<T> => locks.request(name, fn) as Promise<T>
}

/** The page lifecycle, as the transport sees it. `visible` and `hidden` are
 *  `visibilitychange`; the rest are the events of the same name. */
export type LifecycleEvent = 'online' | 'offline' | 'visible' | 'hidden' | 'pageshow' | 'pagehide' | 'freeze' | 'resume'

export interface Lifecycle {
  subscribe(fn: (e: LifecycleEvent) => void): () => void
}

/** A lifecycle driven by hand: tests, and a host with no page. */
export function manualLifecycle(): Lifecycle & { emit(e: LifecycleEvent): void } {
  const fns = new Set<(e: LifecycleEvent) => void>()
  return {
    subscribe(fn) {
      fns.add(fn)
      return () => fns.delete(fn)
    },
    emit(e) {
      for (const fn of [...fns]) fn(e)
    },
  }
}

/** The browser's own. Inert where there is no `window` (SSR, Node). */
export function browserLifecycle(): Lifecycle {
  return {
    subscribe(fn) {
      if (typeof window === 'undefined' || typeof document === 'undefined') return () => {}
      const on: [EventTarget, string, () => void][] = [
        [window, 'online', () => fn('online')],
        [window, 'offline', () => fn('offline')],
        [window, 'pageshow', () => fn('pageshow')],
        [window, 'pagehide', () => fn('pagehide')],
        [document, 'visibilitychange', () => fn(document.visibilityState === 'visible' ? 'visible' : 'hidden')],
        [document, 'freeze', () => fn('freeze')],
        [document, 'resume', () => fn('resume')],
      ]
      for (const [t, name, h] of on) t.addEventListener(name, h)
      return () => {
        for (const [t, name, h] of on) t.removeEventListener(name, h)
      }
    },
  }
}

/** Structured log lines, with the Go client's event names. NEVER GIVEN A TOKEN. */
export interface Logger {
  info(msg: string, fields?: Record<string, unknown>): void
  warn(msg: string, fields?: Record<string, unknown>): void
}

export const consoleLogger: Logger = {
  info: (msg, fields) => console.info(`catenary transport: ${msg}`, fields ?? {}),
  warn: (msg, fields) => console.warn(`catenary transport: ${msg}`, fields ?? {}),
}

export const silentLogger: Logger = { info() {}, warn() {} }
