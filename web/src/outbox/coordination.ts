/* Ruling 4's two primitives, each with a browser implementation and an
 * in-process one.
 *
 * The in-process pair is not a mock of the browser's: it has the semantics the
 * outbox relies on — a queued lock request is granted only on the holder's
 * release, in request order, and a channel post reaches every other context
 * and never the poster — so two `Outbox` instances sharing one hub behave as
 * two tabs sharing one origin. The shipped app uses it too, as the fallback
 * where `navigator.locks` or `BroadcastChannel` does not exist.
 */

import type { ChannelMessage, DrainLock, OutboxChannel } from './types'

export const LOCK_NAME = 'catenary.outbox'
export const CHANNEL_NAME = 'catenary.outbox'

/** `navigator.locks.request('catenary.outbox', …)`, held until released. */
export class WebLockDrainLock implements DrainLock {
  constructor(private readonly locks: LockManager) {}

  request(onGranted: () => void): () => void {
    const abort = new AbortController()
    let release: (() => void) | null = null
    let released = false
    this.locks
      .request(LOCK_NAME, { signal: abort.signal }, () => {
        if (released) return undefined
        return new Promise<void>((resolve) => {
          release = resolve
          onGranted()
        })
      })
      // An aborted, never-granted request rejects with AbortError. Expected.
      .catch(() => undefined)
    return () => {
      released = true
      if (release) release()
      else abort.abort()
    }
  }
}

/** A lock shared by every `DrainLock` handed out by one hub. */
export class InProcessLockHub {
  private holder: symbol | null = null
  private readonly queue: { id: symbol; onGranted: () => void }[] = []

  lock(): DrainLock {
    return {
      request: (onGranted) => {
        const id = Symbol('request')
        this.queue.push({ id, onGranted })
        this.grant()
        return () => {
          const i = this.queue.findIndex((q) => q.id === id)
          if (i >= 0) this.queue.splice(i, 1)
          if (this.holder === id) {
            this.holder = null
            this.grant()
          }
        }
      },
    }
  }

  /** Test visibility: is anyone holding it? */
  get held(): boolean {
    return this.holder !== null
  }

  private grant() {
    if (this.holder !== null) return
    const next = this.queue.shift()
    if (!next) return
    this.holder = next.id
    // Granted asynchronously, as the browser does.
    queueMicrotask(next.onGranted)
  }
}

export class BroadcastOutboxChannel implements OutboxChannel {
  private readonly channel = new BroadcastChannel(CHANNEL_NAME)

  post(message: ChannelMessage): void {
    this.channel.postMessage(message)
  }

  subscribe(listener: (message: ChannelMessage) => void): () => void {
    const handler = (e: MessageEvent) => listener(e.data as ChannelMessage)
    this.channel.addEventListener('message', handler)
    return () => this.channel.removeEventListener('message', handler)
  }

  close(): void {
    this.channel.close()
  }
}

/** Every channel from one hub hears every other one's posts, asynchronously
 *  and never its own. */
export class InProcessChannelHub {
  private readonly members = new Set<Set<(m: ChannelMessage) => void>>()

  channel(): OutboxChannel {
    const listeners = new Set<(m: ChannelMessage) => void>()
    this.members.add(listeners)
    return {
      post: (message) => {
        const copy = structuredClone(message)
        for (const other of this.members) {
          if (other === listeners) continue
          for (const l of other) setTimeout(() => l(copy), 0)
        }
      },
      subscribe: (listener) => {
        listeners.add(listener)
        return () => listeners.delete(listener)
      },
      close: () => {
        this.members.delete(listeners)
      },
    }
  }
}
