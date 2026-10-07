/* The outbox's `OutboxTransport` over CANT-35's transport (CANT-36 row 3,
 * built by CANT-163).
 *
 * THIS IS THE OUTBOX'S ADAPTER, NOT CANT-35'S: the transport implements none of
 * `OutboxTransport` and owns no queue. Each outbox event is sourced from
 * exactly one transport call, and the adapter decides nothing the transport
 * has already decided:
 *
 *   ready            `subscribe()`: a `TransportStatus` whose `ready` has gone
 *                    true. `status().ready` is read once, at construction, for
 *                    the value before the first change. `onSessionEnd` fires
 *                    only when a session ends, so it is never a source of it.
 *   closed{bare1008} `onSessionEnd`: `SessionEnd.bare1008` is CANT-35's
 *                    classification of CANT-31 §4's close, consumed as given
 *                    and never re-derived from a close code here.
 *   ack / error      what `send()` resolves or rejects with. `SendRefused`
 *                    carries the server's `error` frame; every other rejection
 *                    means the frame was not answered, and the `closed` that
 *                    follows is what the outbox acts on.
 *   message          `onApply`: every record an `Applied` carries — a live
 *                    `message` frame or a `/sync` page — and, for a listener
 *                    that attaches after some were applied, the records
 *                    `snapshot()` already holds.
 *   bootstrap        `onApply` with `wiped` (CANT-24 obligation 4).
 *   isTerminal()     `status().terminal.kind`, read on each call (CANT-162
 *                    ruling 2).
 *
 * Only a record carrying a `clientId` can settle an entry: the server echoes
 * it to its author alone, so every other record is passed over here.
 */

import type { ClientSend, Uuid } from '@/wire/generated'
import { SendRefused, silentLogger, type Applied, type Logger, type Transport, type TransportStatus } from '@/transport'
import type { OutboxTransport, OutboxTransportEvent } from './types'

export class TransportOutbox implements OutboxTransport {
  private ready: boolean
  private readonly listeners = new Set<(e: OutboxTransportEvent) => void>()
  private readonly detach: (() => void)[]

  constructor(
    private readonly transport: Transport,
    private readonly log: Logger = silentLogger,
  ) {
    // Read once. From here on `ready` moves only with what `subscribe()`
    // delivers.
    this.ready = transport.status().ready
    this.detach = [
      transport.subscribe((s) => this.onStatus(s)),
      transport.onSessionEnd((e) =>
        this.emit({
          type: 'closed',
          bare1008: e.bare1008,
          // CANT-35's verdict on this close, or a terminal state it had already
          // entered (a `stop()` on the way into one). Informational: the
          // outbox deletes nothing on either (CANT-31 §6).
          terminal: e.verdict === 'terminal_protocol' || transport.status().terminal.kind !== 'none',
        }),
      ),
      transport.onApply((a) => this.onApply(a)),
    ]
  }

  isReady(): boolean {
    return this.ready
  }

  /** Read from the transport each time, never cached: a terminal reached with
   *  no session open (a credential refused at `/refresh`) ends no session and
   *  emits nothing, and `compose` asks at the moment it matters. */
  isTerminal(): boolean {
    return this.transport.status().terminal.kind !== 'none'
  }

  sendFrame(frame: ClientSend): void {
    this.transport.send(frame).then(
      (ack) => this.emit({ type: 'ack', ack }),
      (err: unknown) => {
        if (err instanceof SendRefused) {
          this.emit({ type: 'error', error: err.frame })
          return
        }
        // `SessionEnded`: the outcome is unknown, and the session's `closed`
        // is on its way — the outbox keeps the entry pending and resends it
        // under the same clientId. `NotConnected`: nothing was written, which
        // happens only between a session's end and its `closed`, so the same
        // `closed` returns the entry to the drain. `SendInFlight`: the outbox
        // never writes a clientId twice on one session. None is a refusal,
        // and none may fail an entry.
        this.log.info('outbox send unanswered', {
          client_id: frame.clientId,
          reason: err instanceof Error ? err.name : String(err),
        })
      },
    )
  }

  /** A listener attaching late is handed every record already held with a
   *  clientId, so an entry whose record landed before the outbox loaded
   *  settles at once rather than being resent. */
  subscribe(listener: (event: OutboxTransportEvent) => void): () => void {
    this.listeners.add(listener)
    for (const clientId of held(this.transport.snapshot().messages)) {
      if (!this.listeners.has(listener)) break
      listener({ type: 'message', message: { clientId } })
    }
    return () => this.listeners.delete(listener)
  }

  /** Detaches from the transport. Does not stop it, which the caller owns. */
  close(): void {
    for (const d of this.detach.splice(0)) d()
    this.listeners.clear()
  }

  private onStatus(s: TransportStatus) {
    const was = this.ready
    this.ready = s.ready
    if (s.ready && !was) this.emit({ type: 'ready' })
  }

  private onApply(a: Applied) {
    if (a.wiped) this.emit({ type: 'bootstrap' })
    for (const clientId of held(a.messages)) this.emit({ type: 'message', message: { clientId } })
  }

  private emit(event: OutboxTransportEvent) {
    for (const l of [...this.listeners]) l(event)
  }
}

function* held(messages: readonly { clientId?: Uuid }[]): Generator<Uuid> {
  for (const m of messages) if (m.clientId) yield m.clientId
}
