/* The outbox's state machine (CANT-36, built by CANT-161).
 *
 * Pure logic over two seams — `OutboxStore` for persistence, `OutboxTransport`
 * for a session — plus ruling 4's lock and channel. It holds the FEWEST facts
 * only it can know: the entry, and whether it has been refused. Everything
 * else is derived or held in memory for this session only:
 *
 *   sending   = pending and in this session's in-flight set
 *   queued    = pending and not in flight
 *   sent      = an ack is held in memory for its clientId
 *   RETRYING  = pending with three or more retryable refusals received
 *
 * so a reload empties the in-flight set and the acks and every stored
 * `pending` entry reads QUEUED until the drain sends it again. The moment a
 * record carrying the entry's clientId is held — in ANY status — the entry is
 * deleted, and the server is the truth about that message from then on.
 *
 * AN UPLOAD WAITS FOR A READY SESSION, AND IS THE LOCK HOLDER'S (CANT-162
 * ruling 1, as CANT-201 ruling 1 picked for Dart). A recording may be composed
 * with no session at all: it is held with its Blob, `pending`, and no uploader
 * is called — one called with no connection could only throw, and a throw
 * fails the entry. It is offered when this context is `ready` AND holds the
 * drain lock, which is also how an entry reloaded after a page load, or
 * composed in another tab, is offered, and why two tabs do not both upload it.
 * An entry offered and not yet answered is not offered again: bounding an
 * upload is the `Uploader`'s.
 *
 * A TERMINAL CLIENT TAKES NO ATTACHMENT, HOWEVER IT BECAME TERMINAL (CANT-162
 * ruling 2). `compose` asks the transport, and does not rely on having seen a
 * terminal close: a credential refused before any session opened closes
 * nothing.
 *
 * The rules are `docs/decisions/cant-36-outbox.md`; section numbers below are
 * that file's.
 */

import type { ClientSend, ServerAck, ServerError, Uuid } from '@/wire/generated'
import { InProcessLockHub } from './coordination'
import {
  ComposeRefused,
  RefusingUploader,
  type ChannelMessage,
  type Clock,
  type DrainLock,
  type OutboxChannel,
  type OutboxDraft,
  type OutboxEntry,
  type OutboxError,
  type OutboxFaults,
  type OutboxItem,
  type OutboxState,
  type OutboxStore,
  type OutboxTransport,
  type OutboxTransportEvent,
  type OutboxView,
  type OutboundAttachmentDraft,
  type PersistApi,
  type PersistStatus,
  type Uploader,
} from './types'

/** §6: after this many retryable refusals a held entry reads RETRYING. */
export const RETRYING_AFTER = 3
/** §6's backoff: 2 s, doubling, capped at 5 min, full jitter. */
export const BACKOFF_BASE_MS = 2_000
export const BACKOFF_CAP_MS = 300_000

/** The inline error for a bare `1008` (CANT-31 §7): fixed, because the server
 *  said nothing — the close is all there is. */
export const BARE_1008_MESSAGE =
  'This message could not be read by the server — update the app, then retry'

export const realClock: Clock = {
  now: () => Date.now(),
  setTimeout: (fn, ms) => setTimeout(fn, ms),
  clearTimeout: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
}

export interface OutboxOptions {
  store: OutboxStore
  transport: OutboxTransport
  /** The enrolled account. Entries composed under any other are neither
   *  rendered nor sent (§1, "account scoping"). */
  accountId: Uuid | null
  lock?: DrainLock
  channel?: OutboxChannel | null
  /** `navigator.storage`, or null where it does not exist. */
  storage?: PersistApi | null
  clock?: Clock
  random?: () => number
  mintId?: () => Uuid
  /** §10: what an attachment is offered to. Default `RefusingUploader`, so
   *  a composition root that passes none fails every attachment with its
   *  message rather than holding it QUEUED for good. */
  uploader?: Uploader
  faults?: OutboxFaults
  onChange?: (view: OutboxView) => void
}

/** The retry delay for the nth retryable refusal: full jitter under the
 *  capped exponential, and never shorter than the server's `retryAfterSec`. */
export function backoffMs(n: number, random: number, retryAfterSec?: number): number {
  const ceiling = Math.min(BACKOFF_CAP_MS, BACKOFF_BASE_MS * 2 ** Math.max(0, n - 1))
  const floor = (retryAfterSec ?? 0) * 1000
  return Math.max(Math.floor(random * ceiling), floor)
}

/** §6: the only two refusals that hold rather than fail. Everything else —
 *  `internal` with `retryable: false`, `not_a_member`,
 *  `conversation_not_found`, `message_too_large`, the door codes and the
 *  `unknown` sentinel — goes to `failed` at once. */
export const isHeldRefusal = (e: ServerError) =>
  e.code === 'rate_limited' || (e.code === 'internal' && e.retryable)

export class Outbox {
  private readonly store: OutboxStore
  private readonly transport: OutboxTransport
  private readonly lock: DrainLock
  private readonly channel: OutboxChannel | null
  private readonly storage: PersistApi | null
  private readonly clock: Clock
  private readonly random: () => number
  private readonly mintId: () => Uuid
  private readonly uploader: Uploader
  private readonly faults: OutboxFaults
  private readonly onChange: ((view: OutboxView) => void) | undefined

  private accountId: Uuid | null
  private readonly entries = new Map<Uuid, OutboxEntry>()
  /** Settled or discarded this session. A re-read racing the delete must not
   *  bring one back. */
  private readonly gone = new Set<Uuid>()
  /** Entries between a refusal and its persisted disposition. The drain must
   *  not resend one in that window. */
  private readonly busy = new Set<Uuid>()
  private readonly inFlight = new Set<Uuid>()
  private readonly acks = new Map<Uuid, ServerAck>()
  /** Entries this context has offered to the uploader and not heard back on.
   *  Never stored, and `'uploading'` is never written to an attachment: an
   *  unanswered offer reads `pending` after a reload, as `sending` does. */
  private readonly uploading = new Set<Uuid>()
  /** What the holder in another tab says is in flight / acked there. */
  private remoteInFlight = new Set<Uuid>()
  private remoteAcks = new Map<Uuid, ServerAck>()

  private ready = false
  /** The last close this context saw was terminal (CANT-31 §6). Not every
   *  terminal comes with a close, so `compose` also asks the transport. */
  private terminal = false
  private holding = false
  private releaseLock: (() => void) | null = null
  private persistStatus: PersistStatus = 'unknown'
  private persistAsked = false
  private wake: unknown = null
  private wakeAt = Infinity
  private readonly unsubscribe: (() => void)[] = []
  private closed = false

  private constructor(opts: OutboxOptions) {
    this.store = opts.store
    this.transport = opts.transport
    this.lock = opts.lock ?? new InProcessLockHub().lock()
    this.channel = opts.channel ?? null
    this.storage = opts.storage ?? null
    this.clock = opts.clock ?? realClock
    this.random = opts.random ?? Math.random
    this.mintId = opts.mintId ?? (() => crypto.randomUUID())
    this.uploader = opts.uploader ?? new RefusingUploader()
    this.faults = opts.faults ?? {}
    this.onChange = opts.onChange
    this.accountId = opts.accountId
  }

  /** Load every entry from the store, then attach to the transport. */
  static async open(opts: OutboxOptions): Promise<Outbox> {
    const outbox = new Outbox(opts)
    await outbox.load()
    outbox.attach()
    return outbox
  }

  /* ── reading ─────────────────────────────────────────────────────────── */

  view(): OutboxView {
    const items: OutboxItem[] = []
    for (const entry of this.sorted()) {
      if (!this.mine(entry)) continue
      const ack = this.acks.get(entry.clientId) ?? this.remoteAcks.get(entry.clientId)
      items.push({
        entry,
        state: this.stateOf(entry, ack),
        ...(ack ? { ack } : {}),
        retrying: entry.status === 'pending' && entry.internalRetries >= RETRYING_AFTER,
      })
    }
    return { items, persist: this.persistStatus }
  }

  /** Test visibility. */
  get isHolder(): boolean {
    return this.holding
  }

  private stateOf(e: OutboxEntry, ack: ServerAck | undefined): OutboxState {
    if (e.status === 'failed') return 'failed'
    if (this.faults.persistSending) {
      const stored = e.status as string
      if (stored === 'sending' || stored === 'queued') return stored
    }
    if (ack) return 'sent'
    if (this.inFlight.has(e.clientId) || this.remoteInFlight.has(e.clientId)) return 'sending'
    return 'queued'
  }

  private sorted(): OutboxEntry[] {
    return [...this.entries.values()].sort((a, b) => a.order - b.order)
  }

  private mine(e: OutboxEntry): boolean {
    return this.faults.crossAccountLeak || (this.accountId !== null && e.accountId === this.accountId)
  }

  /* ── actions ─────────────────────────────────────────────────────────── */

  /**
   * §2: mint the clientId ONCE, allocate `order` and write the entry in one
   * strict transaction — and only when that transaction has completed does
   * the entry render, or become eligible for a frame. Resolves with the stored
   * entry WITHOUT WAITING FOR AN ACK OR AN UPLOAD: with no ready session the
   * entry is queued and drains when one is.
   *
   * AN ATTACHMENT, WITH NO READY SESSION (ruling 2, as CANT-36 ruling 5 drew
   * the composer): a `voice` one is taken, held with its Blob, and uploaded
   * when a session is ready; any other kind is a picked file and is refused
   * with `ComposeRefused`, as is every attachment on a terminal client. A
   * refusal writes nothing.
   */
  async compose(draft: OutboxDraft): Promise<OutboxEntry> {
    if (!this.accountId) throw new Error('outbox: no enrolled account to compose as')
    if (draft.attachments?.length && !this.ready) {
      if (this.terminal || this.transport.isTerminal()) throw new ComposeRefused(ComposeRefused.terminal)
      if (draft.attachments.some((a) => a.kind !== 'voice')) throw new ComposeRefused(ComposeRefused.pickedFileOffline)
    }
    const base: Omit<OutboxEntry, 'order'> = {
      v: 1,
      clientId: this.mintId(),
      accountId: this.accountId,
      conversationId: draft.conversationId,
      composedAt: new Date(this.clock.now()).toISOString(),
      ...(draft.text !== undefined ? { text: draft.text } : {}),
      ...(draft.replyToMessageId ? { replyToMessageId: draft.replyToMessageId } : {}),
      ...(draft.replyPreview ? { replyPreview: draft.replyPreview } : {}),
      ...(draft.attachments?.length ? { attachments: draft.attachments } : {}),
      status: 'pending',
      attempts: 0,
      internalRetries: 0,
      reuploads: 0,
    }
    this.askPersist()

    let entry: OutboxEntry
    if (this.faults.sendBeforePersist) {
      entry = { ...base, order: this.localOrder(base.accountId) }
      this.entries.set(entry.clientId, entry)
      this.drain()
      this.emit()
      await this.store.put(entry)
    } else if (this.faults.orderOutsideTxn) {
      entry = { ...base, order: this.localOrder(base.accountId) }
      await this.store.put(entry)
    } else {
      entry = await this.store.add(base)
    }

    if (this.closed) return entry
    this.entries.set(entry.clientId, entry)
    this.emit()
    this.post()
    // Not this context's to upload, or not yet: the entry is held `pending`,
    // and whoever holds the lock on a ready session is offered it — the post
    // above is what makes a holder in another tab re-read.
    if (this.awaitsUpload(entry)) {
      if (this.mayUpload) void this.upload(entry.clientId)
      return entry
    }
    this.drain()
    return entry
  }

  /** §7: `failed` back to `pending` under the SAME clientId, at its original
   *  `order`. §10 item 4: an entry failed by `upload_not_found` holds handles
   *  the server has disowned, and one failed by the uploader holds none, so
   *  RETRY clears them and the entry is offered to the uploader again —
   *  `reuploads` is not reset, so this is a person's attempt, never a loop. */
  async retry(clientId: Uuid): Promise<void> {
    const e = this.entries.get(clientId)
    if (!e || e.status !== 'failed') return
    if (this.faults.remintOnRetry) {
      this.forget(clientId)
      await this.store.delete(clientId)
      const { order: _order, lastError: _err, ...rest } = e
      const fresh = await this.store.add({ ...rest, clientId: this.mintId(), status: 'pending' })
      this.entries.set(fresh.clientId, fresh)
    } else {
      const pending = await this.mutate(clientId, (x) => {
        const afterUpload =
          x.lastError?.kind === 'upload' || (x.lastError?.kind === 'server' && x.lastError.code === 'upload_not_found')
        x.status = 'pending'
        delete x.lastError
        delete x.notBefore
        if (afterUpload) clearHandles(x)
      })
      if (pending && this.awaitsUpload(pending)) {
        this.emit()
        this.post()
        if (this.mayUpload) void this.upload(clientId)
        return
      }
    }
    this.emit()
    this.post()
    this.drain()
  }

  /** §7: DELETE, offered only on `failed`, removes the local copy alone. If
   *  the server did store the message, its record still arrives and renders. */
  async discard(clientId: Uuid): Promise<boolean> {
    const e = this.entries.get(clientId)
    if (!e || e.status !== 'failed') return false
    this.forget(clientId)
    this.emit()
    await this.store.delete(clientId)
    this.post()
    return true
  }

  /** §4: a held record carrying this clientId settles the entry, whatever its
   *  status. Called for every `message` event, and by whoever applies records
   *  from elsewhere (a `/sync` page, a journal loaded at boot). */
  async settle(clientId: Uuid | undefined): Promise<void> {
    if (!clientId) return
    const e = this.entries.get(clientId)
    if (!e) return
    if (this.faults.settlePendingOnly && e.status !== 'pending') return
    this.forget(clientId)
    this.emit()
    await this.store.delete(clientId)
    this.post()
  }

  setAccount(accountId: Uuid | null) {
    this.accountId = accountId
    this.emit()
    this.drain()
  }

  /** Detach from everything. Does not close the store, which the caller owns. */
  close() {
    this.closed = true
    for (const u of this.unsubscribe.splice(0)) u()
    this.dropLock()
    if (this.wake !== null) this.clock.clearTimeout(this.wake)
    this.wake = null
  }

  /* ── lifecycle ───────────────────────────────────────────────────────── */

  private async load() {
    let rows = await this.store.list()
    if (this.faults.remintOnRetry) {
      const reminted: OutboxEntry[] = []
      for (const e of rows) {
        await this.store.delete(e.clientId)
        const { order: _order, ...rest } = e
        reminted.push(await this.store.add({ ...rest, clientId: this.mintId() }))
      }
      rows = reminted
    }
    this.entries.clear()
    for (const e of rows) if (!this.gone.has(e.clientId)) this.entries.set(e.clientId, e)
    // §9 after a reload: nothing is composed yet, so ask the browser what it
    // has already granted rather than prompting.
    if (this.sorted().some((e) => this.mine(e))) {
      const persisted = this.storage?.persisted
      if (!persisted) this.persistStatus = 'refused'
      else this.persistStatus = (await persisted.call(this.storage).catch(() => false)) ? 'granted' : 'refused'
    }
  }

  private attach() {
    this.unsubscribe.push(this.transport.subscribe((e) => this.onEvent(e)))
    if (this.channel) this.unsubscribe.push(this.channel.subscribe((m) => void this.onPost(m)))
    if (this.faults.lockWithoutReady) this.requestLock()
    if (this.transport.isReady()) this.onReady()
    this.emit()
  }

  private onEvent(e: OutboxTransportEvent) {
    if (this.closed) return
    switch (e.type) {
      case 'ready':
        return this.onReady()
      case 'closed':
        return void this.onClosed(e.bare1008, e.terminal ?? false)
      case 'ack':
        return this.onAck(e.ack)
      case 'error':
        return void this.onError(e.error)
      case 'message':
        return void this.settle(e.message.clientId)
      case 'bootstrap':
        // CANT-24 obligation 4 wipes server-derived state. None of this is.
        if (this.faults.deleteOnTerminal) void this.wipe()
        return
    }
  }

  private onReady() {
    this.ready = true
    this.terminal = false
    this.requestLock()
    this.drain()
    // Nothing unless this context already holds the lock; the grant is what
    // offers a held recording on the first `ready`.
    this.offerUploads()
  }

  /** §5 and §7: a close before the ack leaves an in-flight entry `pending`, to
   *  resend on the next `ready` — unless CANT-35 classified the close as a
   *  bare `1008`, which fails it and does not requeue it (CANT-31 §7). */
  private async onClosed(bare1008: boolean, terminal: boolean) {
    this.ready = false
    this.terminal = terminal
    const flying = [...this.inFlight]
    this.inFlight.clear()
    if (!this.faults.lockWithoutReady) this.dropLock()
    this.emit()
    if (bare1008 && !this.faults.resendAfterBare1008) {
      for (const id of flying) await this.fail(id, { kind: 'bare_1008' })
    }
    // CANT-31 §6: a terminal state never deletes the local store.
    if (terminal && this.faults.deleteOnTerminal) await this.wipe()
  }

  private onAck(ack: ServerAck) {
    if (!this.entries.has(ack.clientId)) return
    this.inFlight.delete(ack.clientId)
    this.acks.set(ack.clientId, ack)
    this.emit()
    this.post()
  }

  /** §6, under ruling 3 C: a retryable refusal holds under capped backoff and
   *  never reaches `failed`; every other refusal fails at once — except §10
   *  item 3's stale handle, ahead of that split: `upload_not_found` naming an
   *  entry with attachments clears every handle and re-uploads the held Blobs
   *  under the same `clientId`, ONCE. A second one fails it like any
   *  non-retryable refusal. */
  private async onError(err: ServerError) {
    const id = err.clientId
    if (!id || !this.entries.has(id)) return
    this.inFlight.delete(id)
    this.busy.add(id)
    try {
      if (await this.reupload(id, err)) return
      let hold = isHeldRefusal(err)
      if (hold && this.faults.misclassifyRetryable) hold = false
      if (!hold && this.faults.retryNonRetryable) hold = true

      if (!hold) {
        await this.fail(id, { kind: 'server', code: err.code, message: err.message, retryable: err.retryable })
        return
      }
      const now = this.clock.now()
      const updated = await this.mutate(id, (x) => {
        x.internalRetries++
        const wait = backoffMs(x.internalRetries, this.random(), err.retryAfterSec)
        x.notBefore = new Date(now + wait).toISOString()
      })
      if (updated && this.faults.misjudgeRetryBudget && updated.internalRetries >= RETRYING_AFTER) {
        await this.fail(id, { kind: 'server', code: err.code, message: err.message, retryable: err.retryable })
      }
    } finally {
      this.busy.delete(id)
    }
    this.emit()
    this.post()
    this.drain()
  }

  private async fail(clientId: Uuid, error: OutboxError) {
    await this.mutate(clientId, (x) => {
      x.status = 'failed'
      x.lastError = error
      delete x.notBefore
    })
    this.emit()
    this.post()
  }

  /* ── uploads (§10) ───────────────────────────────────────────────────── */

  private awaitsUpload(e: OutboxEntry): boolean {
    const atts = e.attachments ?? []
    if (this.faults.sendBeforeLastUpload) return atts.length > 0 && !uploaded(atts[0])
    return atts.some((a) => !uploaded(a))
  }

  /** Ruling 1: an upload is attempted only on a ready session and only by the
   *  drain lock's holder — §8's one writer, for uploads as for frames. */
  private get mayUpload(): boolean {
    return this.ready && this.holding
  }

  /**
   * Each attachment not yet uploaded is offered to the `Uploader`, in order:
   * a handle it returns is persisted on the entry before the next is asked
   * for, and the entry joins the drain at its `order` once its last one has
   * (§10 item 1). A rejection fails the entry with the uploader's own message
   * and nothing further is uploaded — so an attachment entry is never left
   * `pending` with nothing that will ever send it. An entry offered and not
   * yet answered is passed over, whatever asks.
   */
  private async upload(clientId: Uuid) {
    if (this.uploading.has(clientId)) return
    this.uploading.add(clientId)
    try {
      const count = this.entries.get(clientId)?.attachments?.length ?? 0
      for (let i = 0; i < count; i++) {
        const entry = this.entries.get(clientId)
        if (!entry || this.closed) return
        const a = entry.attachments?.[i]
        if (!a || uploaded(a)) continue
        let handle: Uuid
        try {
          handle = await this.uploader.upload(entry, a)
        } catch (e) {
          // The fault: the rejection is dropped, the entry stays `pending`
          // with its attachment unuploaded, and the next offer asks again.
          if (this.faults.refusalLoops) return
          await this.fail(clientId, { kind: 'upload', message: e instanceof Error ? e.message : String(e) })
          return
        }
        // A result for an entry already settled or discarded is dropped by
        // `store.update`'s no-op.
        await this.mutate(clientId, (x) => {
          const b = x.attachments?.[i]
          if (!b) return
          b.uploadId = handle
          b.upload = 'uploaded'
        })
        if (this.faults.sendBeforeLastUpload) this.drain()
      }
    } finally {
      this.uploading.delete(clientId)
    }
    this.emit()
    this.post()
    this.drain()
  }

  /** Every `pending` entry of this account still awaiting an upload is
   *  offered, once: `upload` passes over one already offered and unanswered. */
  private offerUploads() {
    if (this.closed || !this.mayUpload) return
    for (const e of this.sorted()) {
      if (!this.mine(e) || e.status !== 'pending' || !this.awaitsUpload(e)) continue
      void this.upload(e.clientId)
    }
  }

  /** §10 item 3. True when the refusal was taken as a stale handle and the
   *  entry re-offered; false when it is the caller's to classify. */
  private async reupload(id: Uuid, err: ServerError): Promise<boolean> {
    const e = this.entries.get(id)
    if (!e || err.code !== 'upload_not_found' || !e.attachments?.length) return false
    if (this.faults.staleHandleFails) return false
    if (e.reuploads >= 1 && !this.faults.reuploadUnbounded) return false
    if (this.faults.remintOnReupload) {
      this.forget(id)
      await this.store.delete(id)
      const { order: _order, lastError: _err, ...rest } = structuredClone(e)
      clearHandles(rest as OutboxEntry)
      const fresh = await this.store.add({ ...rest, clientId: this.mintId(), reuploads: e.reuploads + 1 })
      this.entries.set(fresh.clientId, fresh)
      this.emit()
      this.post()
      if (this.mayUpload) void this.upload(fresh.clientId)
      return true
    }
    await this.mutate(id, (x) => {
      x.reuploads++
      delete x.notBefore
      clearHandles(x)
    })
    this.emit()
    this.post()
    if (this.mayUpload) void this.upload(id)
    return true
  }

  /* ── the drain ───────────────────────────────────────────────────────── */

  /**
   * Ruling 4: only the lock holder writes, and it holds only while its own
   * session is ready. Ruling 6: every eligible entry is written at once, in
   * `order`, without waiting for each ack; one held back is overtaken.
   */
  private drain() {
    if (this.closed || !this.ready) return
    if (!this.holding && !this.faults.everyTabDrains) return

    const now = this.clock.now()
    let wakeAt = Infinity
    let wrote = false
    for (const e of this.sorted()) {
      if (!this.mine(e)) continue
      if (e.status !== 'pending' && !this.faults.resendFailedWithoutRetry) continue
      const id = e.clientId
      if (this.inFlight.has(id) || this.acks.has(id) || this.busy.has(id)) continue
      // §10 item 1: an attachment not yet uploaded is skipped, never waited
      // on, and the entry is overtaken (§11).
      if (this.awaitsUpload(e)) {
        if (this.faults.uploadBlocksDrain) break
        continue
      }
      if (e.notBefore) {
        const t = Date.parse(e.notBefore)
        if (t > now) {
          wakeAt = Math.min(wakeAt, t)
          continue
        }
      }

      this.transport.sendFrame(frameOf(e))
      this.inFlight.add(id)
      wrote = true
      void this.mutate(id, (x) => {
        x.attempts++
        if (this.faults.persistSending) (x as { status: string }).status = 'sending'
      })
    }
    this.schedule(wakeAt)
    if (wrote) {
      this.emit()
      this.post()
    }
  }

  private schedule(at: number) {
    if (at === this.wakeAt) return
    if (this.wake !== null) this.clock.clearTimeout(this.wake)
    this.wake = null
    this.wakeAt = at
    if (at === Infinity) return
    this.wake = this.clock.setTimeout(() => {
      this.wake = null
      this.wakeAt = Infinity
      this.drain()
    }, Math.max(0, at - this.clock.now()))
  }

  /* ── ruling 4 ────────────────────────────────────────────────────────── */

  private requestLock() {
    if (this.releaseLock || this.closed) return
    this.releaseLock = this.lock.request(() => {
      if (this.closed) return
      if (!this.ready && !this.faults.lockWithoutReady) {
        this.dropLock()
        return
      }
      this.holding = true
      // Whatever a previous holder had in flight is unacked-and-pending now.
      this.remoteInFlight = new Set()
      this.remoteAcks = new Map()
      this.drain()
      this.offerUploads()
      this.emit()
      this.post()
    })
  }

  private dropLock() {
    const release = this.releaseLock
    this.releaseLock = null
    const was = this.holding
    this.holding = false
    release?.()
    if (was) this.channel?.post({ released: true })
  }

  private post() {
    if (!this.channel || this.closed) return
    this.channel.post(
      this.holding ? { holder: { inFlight: [...this.inFlight], acks: [...this.acks.values()] } } : {},
    )
  }

  private async onPost(m: ChannelMessage) {
    if (this.closed) return
    if (m.released) {
      this.remoteInFlight = new Set()
      this.remoteAcks = new Map()
    }
    if (m.holder && !this.holding) {
      this.remoteInFlight = new Set(m.holder.inFlight)
      this.remoteAcks = new Map(m.holder.acks.map((a) => [a.clientId, a]))
    }
    const rows = await this.store.list()
    if (this.closed) return
    this.entries.clear()
    for (const e of rows) if (!this.gone.has(e.clientId)) this.entries.set(e.clientId, e)
    this.emit()
    this.drain()
    // An entry composed or retried in another tab is the holder's to upload.
    this.offerUploads()
  }

  /* ── plumbing ────────────────────────────────────────────────────────── */

  /** Every change to a stored entry goes through here, and the in-memory copy
   *  is only ever replaced by what the store committed. */
  private async mutate(clientId: Uuid, fn: (e: OutboxEntry) => void): Promise<OutboxEntry | undefined> {
    const next = await this.store.update(clientId, fn)
    if (next && !this.gone.has(clientId) && !this.closed) this.entries.set(clientId, next)
    return next
  }

  private forget(clientId: Uuid) {
    this.gone.add(clientId)
    this.entries.delete(clientId)
    this.inFlight.delete(clientId)
    this.acks.delete(clientId)
  }

  private async wipe() {
    for (const id of [...this.entries.keys()]) {
      this.forget(id)
      await this.store.delete(id)
    }
    this.emit()
  }

  private askPersist() {
    if (this.persistAsked) return
    this.persistAsked = true
    const persist = this.storage?.persist
    if (!persist) {
      this.persistStatus = 'refused'
      return
    }
    persist
      .call(this.storage)
      .catch(() => false)
      .then((granted) => {
        this.persistStatus = granted ? 'granted' : 'refused'
        this.emit()
      })
  }

  private localOrder(accountId: Uuid): number {
    let highest = 0
    for (const e of this.entries.values()) if (e.accountId === accountId) highest = Math.max(highest, e.order)
    return highest + 1
  }

  private emit() {
    if (!this.closed) this.onChange?.(this.view())
  }
}

const uploaded = (a: OutboundAttachmentDraft) => a.upload === 'uploaded' && !!a.uploadId

/** Every handle the server has disowned, or that was never minted, goes; the
 *  Blobs stay, which is what the next offer uploads. */
function clearHandles(e: OutboxEntry) {
  for (const a of e.attachments ?? []) {
    delete a.uploadId
    a.upload = 'pending'
  }
}

/** The only path from an entry to the wire. `replyPreview`, `composedAt` and
 *  every counter stay behind. */
export function frameOf(e: OutboxEntry): ClientSend {
  return {
    type: 'send',
    clientId: e.clientId,
    conversationId: e.conversationId,
    ...(e.text !== undefined ? { text: e.text } : {}),
    ...(e.attachments?.length
      ? { attachments: e.attachments.map((a) => ({ kind: a.kind, uploadId: a.uploadId! })) }
      : {}),
    ...(e.replyToMessageId ? { replyToMessageId: e.replyToMessageId } : {}),
  }
}
