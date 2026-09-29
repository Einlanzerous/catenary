/* Two `OutboxTransport`s that are not the real one.
 *
 * `NullTransport` is what the shipped app wires until CANT-163 adapts CANT-35's
 * transport: it is never `ready`, so every entry composed renders honestly
 * QUEUED, is kept durably, and is never shown SENT by an ack nobody sent.
 *
 * `ScriptedTransport` is the tests' server. Its dedup follows the server's
 * stated semantics — `UNIQUE (author_id, client_id)`, a replay answered with
 * the ORIGINAL ids and `duplicate: true` (CANT-14/CANT-18) — and it answers
 * only when told to, so a test decides exactly when an ack, a refusal, a
 * record or a close arrives. It is not the oracle; the faults are what keep a
 * test honest against it.
 */

import type { ClientSend, ServerAck, ServerError, Uuid } from '@/wire/generated'
import type { OutboxTransport, OutboxTransportEvent } from './types'

export class NullTransport implements OutboxTransport {
  isReady(): boolean {
    return false
  }
  sendFrame(): void {
    // Unreachable: the outbox writes only while a session is ready, and this
    // one never is.
    throw new Error('NullTransport has no session')
  }
  subscribe(): () => void {
    return () => undefined
  }
}

export class ScriptedTransport implements OutboxTransport {
  /** Every frame written on this transport, across sessions, in order. */
  readonly frames: ClientSend[] = []
  /** Called synchronously as each frame is written. */
  onFrame: ((frame: ClientSend) => void) | null = null
  /** Ack every frame as it is written. */
  autoAck = false

  private ready = false
  private readonly listeners = new Set<(e: OutboxTransportEvent) => void>()

  constructor(
    /** The "server" — shared between transports that stand for one account's
     *  sessions in two tabs. */
    readonly server: ScriptedServer = new ScriptedServer(),
  ) {}

  isReady(): boolean {
    return this.ready
  }

  sendFrame(frame: ClientSend): void {
    if (!this.ready) throw new Error('ScriptedTransport: frame written with no ready session')
    this.frames.push(structuredClone(frame))
    this.server.received.push(structuredClone(frame))
    this.onFrame?.(frame)
    if (this.autoAck) this.ack(frame.clientId, frame.conversationId)
  }

  subscribe(listener: (e: OutboxTransportEvent) => void): () => void {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  emit(event: OutboxTransportEvent) {
    for (const l of [...this.listeners]) l(event)
  }

  open() {
    this.ready = true
    this.emit({ type: 'ready' })
  }

  close(bare1008 = false, terminal = false) {
    this.ready = false
    this.emit({ type: 'closed', bare1008, terminal })
  }

  /** Commit (or replay) a send and answer it with the server's ack. */
  ack(clientId: Uuid, conversationId?: Uuid): ServerAck {
    const ack = this.server.commit(clientId, conversationId ?? this.lastFrame(clientId).conversationId)
    this.emit({ type: 'ack', ack })
    return ack
  }

  refuse(clientId: Uuid, error: Omit<ServerError, 'type' | 'clientId'>) {
    this.emit({ type: 'error', error: { type: 'error', clientId, ...error } })
  }

  /** The record for a committed send arrives — a `message` frame or `/sync`. */
  deliver(clientId: Uuid) {
    this.emit({ type: 'message', message: { clientId } })
  }

  framesFor(clientId: Uuid): ClientSend[] {
    return this.frames.filter((f) => f.clientId === clientId)
  }

  private lastFrame(clientId: Uuid): ClientSend {
    const f = [...this.server.received].reverse().find((x) => x.clientId === clientId)
    if (!f) throw new Error(`ScriptedTransport: no frame for ${clientId}`)
    return f
  }
}

/** One author's view of the server's messages table. */
export class ScriptedServer {
  readonly received: ClientSend[] = []
  readonly stored = new Map<Uuid, ServerAck>()
  private logSeq = 100
  private readonly seqs = new Map<Uuid, number>()
  at = () => new Date(Date.UTC(2026, 8, 29, 12, 0, this.logSeq - 100)).toISOString()

  commit(clientId: Uuid, conversationId: Uuid): ServerAck {
    const held = this.stored.get(clientId)
    if (held) return { ...held, duplicate: true }
    const seq = (this.seqs.get(conversationId) ?? 40) + 1
    this.seqs.set(conversationId, seq)
    const ack: ServerAck = {
      type: 'ack',
      clientId,
      messageId: `srv-${clientId}`,
      conversationId,
      seq,
      logSeq: ++this.logSeq,
      at: this.at(),
    }
    this.stored.set(clientId, ack)
    return ack
  }
}
