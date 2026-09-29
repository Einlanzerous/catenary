/* The client heartbeat — mirrors `Client.heartbeat` in internal/client/client.go
 * (CANT-23).
 *
 * `ping` every `ready.heartbeat_interval_sec`, from `ready`, each with a unique
 * id; pongs matched by id; the socket severed once `ready.missed_pong_limit`
 * pings are outstanding, rather than waiting for the OS to notice a half-open
 * connection (R1 §3). NO CLIENT CONSTANT FOR EITHER NUMBER: both arrive on
 * `ready`, because two constants in two clients drift, and the Flutter one is
 * found wrong only in the field.
 *
 * AND THE WAKE DETECTOR, which Go does not have. A background tab's timers are
 * throttled and a closed lid stops them, so the gap between two ticks is read
 * off the wall clock: longer than `interval × (missed_pong_limit + 1)` — the
 * server's own backstop window, after which it has already closed this session
 * with `4000` — and the tick is a wake signal instead of an ordinary ping.
 */

import type { Clock, Timers } from './seams'

export interface HeartbeatHost {
  timers: Timers
  now: Clock
  /** Write a `ping` with this id. */
  ping(id: string): void
  /** Sever the socket: `limit` pings are outstanding. */
  sever(outstanding: number): void
  /** The wall clock jumped past the backstop window between two ticks. */
  wake(): void
  pingsSent(): void
  pongReceived(rttMs: number | null): void
}

export class Heartbeat {
  private seq = 0
  private timer: unknown = null
  private intervalMs = 0
  private limit = 1
  private lastTick = 0
  private readonly outstanding = new Map<string, number>()

  constructor(private readonly host: HeartbeatHost) {}

  /** From `ready`: the two numbers it announced. */
  start(intervalSec: number, missedPongLimit: number): void {
    this.stop()
    if (!(intervalSec > 0)) return
    this.intervalMs = intervalSec * 1000
    this.limit = Math.max(1, missedPongLimit)
    this.lastTick = this.host.now()
    this.schedule()
  }

  stop(): void {
    if (this.timer !== null) this.host.timers.clearTimeout(this.timer)
    this.timer = null
    this.outstanding.clear()
  }

  get running(): boolean {
    return this.timer !== null
  }

  /** A wake signal's immediate ping, so a half-dead socket is found by the
   *  next tick rather than `missed_pong_limit` ticks later. Counted as
   *  outstanding like any other. */
  pingNow(): void {
    if (this.timer === null) return
    this.send()
  }

  pong(id: string): void {
    const at = this.outstanding.get(id)
    this.outstanding.delete(id)
    this.host.pongReceived(at === undefined ? null : this.host.now() - at)
  }

  private schedule(): void {
    this.timer = this.host.timers.setTimeout(() => this.tick(), this.intervalMs)
  }

  private tick(): void {
    const now = this.host.now()
    const gap = now - this.lastTick
    this.lastTick = now
    if (gap > this.intervalMs * (this.limit + 1)) {
      // The session is past the server's backstop; the wake signal's own
      // ping stands in for this tick's.
      this.schedule()
      this.host.wake()
      return
    }
    const n = this.outstanding.size
    if (n >= this.limit) {
      this.stop()
      this.host.sever(n)
      return
    }
    this.schedule()
    this.send()
  }

  private send(): void {
    const id = `hb-${++this.seq}`
    this.outstanding.set(id, this.host.now())
    this.host.pingsSent()
    this.host.ping(id)
  }
}
