/* CANT-163: the outbox over CANT-35's transport, end to end.
 *
 * `TransportOutbox` is driven here by the transport itself — `createTransport`
 * over CANT-151's fakes (a fake socket, a fake `/sync`, a fake clock) — and the
 * outbox sits on top of it as it does in the app. So every event the outbox
 * acts on arrives by the only path the adapter allows: `ready` from
 * `subscribe()`, `closed{bare1008}` from `onSessionEnd`, `ack`/`error` from
 * `send()`, records from `onApply` and `snapshot()`. The outbox's own rules
 * are `npm run outbox`'s criteria over `ScriptedTransport`; what is asserted
 * here is that each of those facts reaches the outbox, from its one source,
 * unaltered.
 *
 * Each case also runs with the adapter replaced by a deliberately wrong one
 * where that is what the case is guarding, and requires the case to FAIL —
 * the house pattern `npm run outbox` follows.
 */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { ClientSend, Uuid } from '@/wire/generated'
import {
  MemoryOutboxStore,
  Outbox,
  TransportOutbox,
  type OutboxEntry,
  type OutboxItem,
  type OutboxStore,
  type OutboxTransportEvent,
} from '@/outbox'
import type { SessionEnd, Transport } from '@/transport'
import {
  bootstrapPage,
  CONV,
  flush,
  ME,
  message,
  messageFrame,
  page,
  ready,
  rig,
  uuid,
  type Rig,
  type RigOptions,
} from '@/transport/test/harness'

/* ── harness ─────────────────────────────────────────────────────────────── */

/** Negative controls: an adapter that gets one source wrong. */
type Broken = 'readyOnOpenSocket' | 'bare1008FromCode' | 'unansweredIsError' | 'noSnapshot' | 'pageRecordsIgnored'

/** The adapter with one rule broken — each is a plausible mistake. */
class BrokenAdapter extends TransportOutbox {
  constructor(t: Transport, readonly broken: Broken) {
    super(t)
    const self = this as unknown as {
      ready: boolean
      emit(e: OutboxTransportEvent): void
      onApply(a: { messages: { clientId?: Uuid }[]; source: string; wiped: boolean }): void
    }
    if (broken === 'readyOnOpenSocket') {
      // Ready is taken to mean "a session exists": the outbox is told on any
      // open socket, before the server's `ready`.
      t.subscribe((s) => {
        if (s.connected && !self.ready) {
          self.ready = true
          self.emit({ type: 'ready' })
        }
      })
    }
    if (broken === 'bare1008FromCode') {
      const emit = self.emit.bind(self)
      self.emit = (e) => (e.type === 'closed' ? undefined : emit(e))
      // Re-derived from the code alone: every 1008 is "bare".
      t.onSessionEnd((e: SessionEnd) => emit({ type: 'closed', bare1008: e.closeCode === 1008 }))
    }
    if (broken === 'pageRecordsIgnored') {
      const onApply = self.onApply.bind(self)
      self.onApply = (a) => (a.source === 'page' ? undefined : onApply(a))
    }
  }

  override sendFrame(frame: ClientSend): void {
    if (this.broken !== 'unansweredIsError') return super.sendFrame(frame)
    const self = this as unknown as { emit(e: OutboxTransportEvent): void; transport: Transport }
    self.transport.send(frame).then(
      (ack) => self.emit({ type: 'ack', ack }),
      (err: unknown) =>
        self.emit({
          type: 'error',
          error: {
            type: 'error',
            clientId: frame.clientId,
            code: 'internal',
            retryable: false,
            message: String(err),
            ...((err as { frame?: object }).frame ?? {}),
          },
        }),
    )
  }

  override subscribe(listener: (event: OutboxTransportEvent) => void): () => void {
    if (this.broken !== 'noSnapshot') return super.subscribe(listener)
    const self = this as unknown as { listeners: Set<(e: OutboxTransportEvent) => void> }
    self.listeners.add(listener)
    return () => self.listeners.delete(listener)
  }
}

interface Ctx {
  r: Rig
  adapter: TransportOutbox
  outbox: Outbox
  store: OutboxStore
  item(clientId: Uuid): OutboxItem | undefined
  /** Every `send` frame the transport has written, on every socket. */
  sends(): ClientSend[]
  close(): void
}

async function context(
  opts: { broken?: Broken; store?: OutboxStore; rig?: Rig; rigOptions?: RigOptions; start?: boolean } = {},
): Promise<Ctx> {
  const r = opts.rig ?? rig(opts.rigOptions)
  // A /sync that introduces the room and both people, so a live `message`
  // frame for it is applied rather than discarded (CANT-103 rule 1).
  r.sync.answer = (req) => bootstrapPage(req.after)
  const store = opts.store ?? new MemoryOutboxStore()
  const adapter = opts.broken ? new BrokenAdapter(r.t, opts.broken) : new TransportOutbox(r.t)
  let items: OutboxItem[] = []
  const outbox = await Outbox.open({
    store,
    transport: adapter,
    accountId: ME,
    channel: null,
    storage: { persist: async () => true, persisted: async () => true },
    clock: r.clock,
    random: () => 0.5,
    onChange: (v) => {
      items = v.items
    },
  })
  items = outbox.view().items
  if (opts.start ?? true) r.t.start()
  return {
    r,
    adapter,
    outbox,
    store,
    item: (id) => items.find((i) => i.entry.clientId === id),
    sends: () =>
      r.net.sockets.flatMap((s) => s.frames()).filter((f): f is ClientSend => f.type === 'send'),
    close: () => {
      outbox.close()
      adapter.close()
      r.t.stop()
    },
  }
}

async function compose(ctx: Ctx, text = 'hello'): Promise<OutboxEntry> {
  const e = await ctx.outbox.compose({ conversationId: CONV, text })
  await flush()
  return e
}

async function stored(ctx: Ctx, clientId: Uuid): Promise<OutboxEntry | undefined> {
  return (await ctx.store.list()).find((e) => e.clientId === clientId)
}

/** Runs `check` as written (must pass), then over a broken adapter (must fail). */
async function guarded(broken: Broken, check: (b?: Broken) => Promise<void>) {
  await check()
  await assert.rejects(check(broken), `the ${broken} adapter should have failed this check`)
}

/* ── ready ───────────────────────────────────────────────────────────────── */

test('CANT-163 · ready comes from subscribe(): nothing is written until the server says ready', () =>
  guarded('readyOnOpenSocket', async (broken) => {
    const ctx = await context({ broken })
    const e = await compose(ctx)
    await flush()
    const s = ctx.r.net.last
    s.open()
    await flush()
    // The socket is open and the hello is on it, but no `ready` has come.
    assert.equal(ctx.adapter.isReady(), false)
    assert.equal(ctx.sends().length, 0, 'no send before ready')
    assert.equal(ctx.item(e.clientId)?.state, 'queued')

    s.frame(ready())
    await flush()
    assert.equal(ctx.adapter.isReady(), true)
    assert.deepEqual(ctx.sends().map((f) => f.clientId), [e.clientId], 'sent on ready, under its clientId')
    assert.equal(ctx.item(e.clientId)?.state, 'sending')
    ctx.close()
  }))

test('CANT-163 · status().ready is read once at construction', async () => {
  const r = rig()
  r.sync.answer = (req) => bootstrapPage(req.after)
  r.t.start()
  await r.connect()
  assert.equal(r.t.status().ready, true)

  // An adapter built over a session that is already ready reports it without
  // waiting for a change that will not come, and the outbox drains at once.
  const store = new MemoryOutboxStore()
  const pending: OutboxEntry = {
    v: 1, clientId: uuid(7001), accountId: ME, conversationId: CONV, order: 1,
    composedAt: '2026-09-29T11:00:00.000Z', text: 'held from before', status: 'pending',
    attempts: 0, internalRetries: 0, reuploads: 0,
  }
  await store.put(pending)
  const ctx = await context({ rig: r, store, start: false })
  await flush()
  assert.equal(ctx.adapter.isReady(), true)
  assert.deepEqual(ctx.sends().map((f) => f.clientId), [pending.clientId])
  ctx.close()
})

test('CANT-163 · a transport that is never started is never ready (the shipped wiring)', async () => {
  const ctx = await context({ start: false })
  const e = await compose(ctx)
  await ctx.r.clock.advance(60_000)
  assert.equal(ctx.adapter.isReady(), false)
  assert.equal(ctx.r.net.sockets.length, 0, 'no socket was dialed')
  assert.equal(ctx.item(e.clientId)?.state, 'queued')
  assert.ok(await stored(ctx, e.clientId), 'kept')
  ctx.close()
})

/* ── ack and error, from what send() settles with ────────────────────────── */

test('CANT-163 · an ack resolves send(), and the entry renders SENT at the server\'s ordinals', async () => {
  const ctx = await context()
  const s = await ctx.r.connect()
  const e = await compose(ctx)
  const other = uuid(101)
  s.frame({ type: 'ack', clientId: e.clientId, messageId: uuid(8001), conversationId: other, seq: 57, logSeq: 900, at: '2026-09-29T12:00:09.000Z', duplicate: true })
  await flush()
  const it = ctx.item(e.clientId)
  assert.equal(it?.state, 'sent')
  assert.equal(it?.ack?.conversationId, other, "the ack's conversation, not the entry's")
  assert.equal(it?.ack?.seq, 57)
  assert.equal(it?.ack?.at, '2026-09-29T12:00:09.000Z')
  ctx.close()
})

test('CANT-163 · a refusal naming the clientId reaches the outbox as the server sent it', async () => {
  const ctx = await context()
  const s = await ctx.r.connect()
  const refused = await compose(ctx, 'too big')
  const held = await compose(ctx, 'try later')
  s.frame({ type: 'error', clientId: refused.clientId, code: 'message_too_large', retryable: false, message: 'Message is too long.' })
  s.frame({ type: 'error', clientId: held.clientId, code: 'internal', retryable: true, message: 'Try again.' })
  await flush()
  const f = await stored(ctx, refused.clientId)
  assert.equal(f?.status, 'failed')
  assert.deepEqual(f?.lastError, { kind: 'server', code: 'message_too_large', message: 'Message is too long.', retryable: false })
  const h = await stored(ctx, held.clientId)
  assert.equal(h?.status, 'pending', 'a retryable internal holds (ruling 3 C)')
  assert.equal(h?.internalRetries, 1)
  ctx.close()
})

test('CANT-163 · a send the session ended under is not a refusal: it stays pending and resends, same clientId', () =>
  guarded('unansweredIsError', async (broken) => {
    const ctx = await context({ broken })
    const s = await ctx.r.connect()
    const e = await compose(ctx)
    assert.equal(ctx.sends().length, 1)
    s.serverClose(1006)
    await flush()
    assert.equal((await stored(ctx, e.clientId))?.status, 'pending', 'SessionEnded is an unknown outcome, not a failure')
    assert.equal(ctx.item(e.clientId)?.state, 'queued')
    await ctx.r.clock.advance(1_000)
    await ctx.r.connect()
    assert.deepEqual(ctx.sends().map((f) => f.clientId), [e.clientId, e.clientId], 'resent under the same clientId')
    ctx.close()
  }))

/* ── closed{bare1008}, from onSessionEnd ─────────────────────────────────── */

test('CANT-163 · a bare 1008 fails what was in flight and it is not resent (CANT-31 §7)', async () => {
  const ctx = await context()
  const s = await ctx.r.connect()
  const e = await compose(ctx)
  s.serverClose(1008)
  await flush()
  const f = await stored(ctx, e.clientId)
  assert.equal(f?.status, 'failed')
  assert.deepEqual(f?.lastError, { kind: 'bare_1008' })
  // A bare 1008 is terminal to the transport, so there is no next session to
  // resend on — and the entry is kept, because terminal never deletes.
  await ctx.r.clock.advance(120_000)
  assert.equal(ctx.sends().length, 1)
  assert.equal(ctx.r.t.status().terminal.kind, 'protocol')
  assert.ok(await stored(ctx, e.clientId))
  ctx.close()
})

test('CANT-163 · a 1008 the transport reports as frame-preceded is not bare: the entry resends', () =>
  guarded('bare1008FromCode', async (broken) => {
    const ctx = await context({ broken })
    const s = await ctx.r.connect()
    const e = await compose(ctx)
    // error{internal} with no client_id, then 1008: CANT-35 reconnects at the
    // maximum, and SessionEnd.bare1008 is false.
    s.frame({ type: 'error', code: 'internal', retryable: true, message: 'down' })
    s.serverClose(1008)
    await flush()
    assert.equal((await stored(ctx, e.clientId))?.status, 'pending')
    await ctx.r.clock.advance(60_000)
    await ctx.r.connect()
    assert.deepEqual(ctx.sends().map((f) => f.clientId), [e.clientId, e.clientId])
    ctx.close()
  }))

test('CANT-163 · the closed a terminal state produces says so, and the outbox keeps every entry', async () => {
  const ctx = await context()
  const events: OutboxTransportEvent[] = []
  ctx.adapter.subscribe((ev) => events.push(ev))
  const s = await ctx.r.connect()
  const e = await compose(ctx)
  s.serverClose(4001)
  await flush()
  const closed = events.filter((ev) => ev.type === 'closed')
  assert.deepEqual(closed, [{ type: 'closed', bare1008: false, terminal: true }])
  assert.equal((await stored(ctx, e.clientId))?.status, 'pending', 'a 4001 is not bare: pending, kept')
  ctx.close()
})

/* ── records, from onApply and snapshot() ────────────────────────────────── */

test('CANT-163 · a live message frame carrying the clientId settles the entry', async () => {
  const ctx = await context()
  const s = await ctx.r.connect()
  const e = await compose(ctx)
  s.frame({ type: 'ack', clientId: e.clientId, messageId: uuid(8002), conversationId: CONV, seq: 5, logSeq: 5, at: '2026-09-29T12:00:05.000Z' })
  // Somebody else's record carries no clientId, and settles nothing.
  s.frame(messageFrame(message(4)))
  await flush()
  assert.ok(await stored(ctx, e.clientId), 'an ack alone does not settle')
  s.frame(messageFrame(message(5, { id: uuid(8002), authorId: ME, clientId: e.clientId })))
  await flush()
  assert.equal(await stored(ctx, e.clientId), undefined, 'settled: the server holds it now')
  assert.equal(ctx.item(e.clientId), undefined)
  ctx.close()
})

test('CANT-163 · a /sync record carrying the clientId settles a FAILED entry', () =>
  guarded('pageRecordsIgnored', async (broken) => {
    const ctx = await context({ broken })
    const s = await ctx.r.connect()
    const e = await compose(ctx)
    s.frame({ type: 'error', clientId: e.clientId, code: 'internal', retryable: false, message: 'Unknown outcome.' })
    await flush()
    assert.equal((await stored(ctx, e.clientId))?.status, 'failed')
    // The commit had in fact landed; the next page carries it.
    ctx.r.sync.answer = (req) => bootstrapPage(req.after + 1, [message(6, { authorId: ME, clientId: e.clientId, logSeq: req.after + 1 })])
    s.frame({ type: 'resync_required', reason: 'cursor_too_old', logSeq: 1 })
    await flush(6)
    assert.equal(await stored(ctx, e.clientId), undefined, 'no FAILED row left beside the record')
    ctx.close()
  }))

test('CANT-163 · a record already held when the outbox attaches settles its entry, and it is never sent', () =>
  guarded('noSnapshot', async (broken) => {
    const r = rig()
    r.sync.answer = (req) => bootstrapPage(req.after)
    const clientId = uuid(7002)
    r.t.start()
    await r.connect()
    r.sync.answer = () => page({ logSeq: 9, messages: [message(9, { authorId: ME, clientId })] })
    r.t.catchUp()
    await flush(6)
    assert.ok(r.t.snapshot().messages.some((m) => m.clientId === clientId))

    const store = new MemoryOutboxStore()
    await store.put({
      v: 1, clientId, accountId: ME, conversationId: CONV, order: 1, composedAt: '2026-09-29T11:00:00.000Z',
      text: 'landed before the reload', status: 'pending', attempts: 1, internalRetries: 0, reuploads: 0,
    })
    const ctx = await context({ rig: r, store, start: false, broken })
    await flush()
    assert.equal(await stored(ctx, clientId), undefined, 'settled from the snapshot')
    assert.equal(ctx.sends().length, 0, 'and never resent')
    ctx.close()
  }))

test('CANT-163 · a discard-and-bootstrap reaches the outbox as bootstrap, and deletes nothing', async () => {
  const ctx = await context()
  const events: OutboxTransportEvent[] = []
  ctx.adapter.subscribe((ev) => events.push(ev))
  ctx.r.sync.answer = (req) => bootstrapPage(Math.max(req.after, 50))
  const s = await ctx.r.connect()
  await flush(6)
  assert.equal(ctx.r.t.status().cursor, 50)
  const e = await compose(ctx)
  s.serverClose(1006)
  await flush()
  await ctx.r.clock.advance(1_000)
  // The server's log is behind the cursor: a restore (obligation 4).
  await ctx.r.connect(ready({ logSeq: 10 }))
  await flush(6)
  assert.ok(events.some((ev) => ev.type === 'bootstrap'), 'bootstrap delivered')
  assert.ok(await stored(ctx, e.clientId), 'the outbox is not server-derived state')
  assert.deepEqual(ctx.sends().map((f) => f.clientId), [e.clientId, e.clientId], 'and it resends')
  ctx.close()
})

test('CANT-163 · close() detaches the adapter from the transport', async () => {
  const ctx = await context()
  const events: OutboxTransportEvent[] = []
  ctx.adapter.subscribe((ev) => events.push(ev))
  ctx.adapter.close()
  await ctx.r.connect()
  assert.deepEqual(events, [])
  ctx.close()
})
