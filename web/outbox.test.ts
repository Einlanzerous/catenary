/* `npm run outbox` — the check that stands in for an oracle (CANT-36, CANT-161).
 *
 * The outbox is the one part of the client with nothing external to check it
 * against: a scripted fake built from the same understanding as the code will
 * agree with the code by construction. So every criterion below is a plain
 * function that throws when its rule is broken, run twice over: once as
 * written, where it must PASS, and once per named fault from the plan's
 * rollout table, where it must FAIL. A fault whose criterion still passes
 * fails the run — the criterion was not testing what it claims.
 *
 * Criteria are numbered as the approved CANT-36 plan numbers them. 13 and 14
 * (attachments) were built by CANT-162, against a scripted `Uploader`, with
 * the six faults its plan names; its other criteria are the plain tests under
 * "CANT-162" below. 18 (the decision record) is CANT-160's.
 *
 * node:test over a vite-ssr bundle (CANT-35 ruling 9 A), IndexedDB under
 * fake-indexeddb (CANT-35 ruling 10 A, shared with CANT-36).
 */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import {
  IDBDatabase as FakeIDBDatabase,
  IDBFactory as FakeIDBFactory,
  IDBKeyRange as FakeIDBKeyRange,
} from 'fake-indexeddb'
import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from '@/App.vue'
import type { Uuid } from '@/wire/generated'
import {
  BACKOFF_CAP_MS,
  BARE_1008_MESSAGE,
  ComposeRefused,
  IdbOutboxStore,
  InProcessChannelHub,
  InProcessLockHub,
  MemoryOutboxStore,
  NullTransport,
  Outbox,
  RefusingUploader,
  ScriptedServer,
  ScriptedTransport,
  UploadRefused,
  project,
  type Clock,
  type OutboundAttachmentDraft,
  type OutboxChannel,
  type OutboxDraft,
  type OutboxEntry,
  type OutboxFaults,
  type OutboxItem,
  type OutboxStore,
  type PersistApi,
  type StoreFaults,
  type Uploader,
} from '@/outbox'
import {
  activeMessages,
  directs,
  discard,
  lastMessageOf,
  newCount,
  outboxReady,
  retry,
  select,
  send,
  startRecording,
  state,
  stopRecording,
  unreadCount,
  useOutbox,
} from '@/store'

// CANT-163: the outbox over CANT-35's transport, through its adapter.
import './src/outbox/test/transport-adapter.test'

type Faults = OutboxFaults & StoreFaults

/* ── harness ─────────────────────────────────────────────────────────────── */

const ME = 'u-hollis'
const CONV = 'c-kitchen'

/* THE CORPUS THESE CRITERIA RENDER OVER. `store.ts` holds no fixtures since
 * CANT-39 deleted the mock — its records are whatever a live server's journal
 * projected, and there is no server here — so the few records the rail and
 * thread criteria (5, 6, 16) need are put into `state` directly, the way
 * `readstate.ts` puts a captured page there. A room with an unread run, and
 * three directs whose last messages are an hour apart so the column has a
 * top and a bottom to move between. */
{
  const hour = (n: number) => new Date(Date.parse('2026-09-29T08:00:00.000Z') + n * 3_600_000).toISOString()
  state.me = ME
  state.users = Object.fromEntries(
    [[ME, 'Hollis Byrne', 'HB'], ['u-ilse', 'Ilse Marchetti', 'IM'], ['u-nadia', 'Nadia Okonkwo', 'NO'], ['u-ted', 'Ted Almasy', 'TA']]
      .map(([id, name, initials]) => [id, { id, name, initials }]),
  )
  state.conversations = [
    { id: CONV, kind: 'group', name: 'Kitchen Table', memberCount: 4, firstUnreadSeq: 2, headSeq: 3 },
    { id: 'c-ilse', kind: 'direct', name: 'Ilse Marchetti', otherMemberId: 'u-ilse', memberCount: 2, headSeq: 1 },
    { id: 'c-nadia', kind: 'direct', name: 'Nadia Okonkwo', otherMemberId: 'u-nadia', memberCount: 2, headSeq: 1 },
    { id: 'c-ted', kind: 'direct', name: 'Ted Almasy', otherMemberId: 'u-ted', memberCount: 2, headSeq: 1 },
  ]
  let logSeq = 1
  const msg = (conversationId: string, seq: number, authorId: string, at: string, text: string) =>
    ({ id: `m-${conversationId}-${seq}`, seq, logSeq: logSeq++, conversationId, authorId, at, state: 'delivered' as const, text })
  state.messages = [
    msg(CONV, 1, ME, hour(0), 'read already'),
    msg(CONV, 2, 'u-nadia', hour(1), 'new one'),
    msg(CONV, 3, 'u-ilse', hour(2), 'new two'),
    msg('c-ilse', 1, 'u-ilse', hour(3), 'the newest direct'),
    msg('c-nadia', 1, 'u-nadia', hour(2), 'the middle direct'),
    msg('c-ted', 1, 'u-ted', hour(1), 'the oldest direct'),
  ]
  state.activeId = CONV
}

const idb = () => ({
  factory: new FakeIDBFactory() as unknown as IDBFactory,
  keyRange: FakeIDBKeyRange as unknown as typeof IDBKeyRange,
})

const storeFaults = (f: Faults): StoreFaults => ({
  ...(f.neverCompletes ? { neverCompletes: true } : {}),
  ...(f.relaxedDurability ? { relaxedDurability: true } : {}),
})

/** A clock the test moves. Timers fire only on `advance`. */
class FakeClock implements Clock {
  t = Date.parse('2026-09-29T12:00:00.000Z')
  private timers: { at: number; fn: () => void; id: number }[] = []
  private nextId = 1
  now = () => this.t
  setTimeout = (fn: () => void, ms: number) => {
    const id = this.nextId++
    this.timers.push({ at: this.t + ms, fn, id })
    return id
  }
  clearTimeout = (id: unknown) => {
    this.timers = this.timers.filter((x) => x.id !== id)
  }
  async advance(ms: number) {
    const until = this.t + ms
    for (;;) {
      const due = this.timers.filter((x) => x.at <= until).sort((a, b) => a.at - b.at)[0]
      if (!due) break
      this.timers = this.timers.filter((x) => x !== due)
      this.t = due.at
      due.fn()
      await ticks()
    }
    this.t = until
    await ticks()
  }
}

/** Let IndexedDB transactions, lock grants and channel posts run. */
async function ticks(n = 12) {
  for (let i = 0; i < n; i++) await new Promise((r) => setTimeout(r, 0))
}

async function waitFor(what: string, predicate: () => boolean | Promise<boolean>, ms = 1500) {
  const deadline = Date.now() + ms
  while (Date.now() < deadline) {
    if (await predicate()) return
    await new Promise((r) => setTimeout(r, 1))
  }
  assert.fail(`timed out waiting for: ${what}`)
}

/** Reject a promise that does not settle in time — a store whose commit
 *  never comes must fail a criterion, not hang the run. */
function within<T>(what: string, p: Promise<T>, ms = 1500): Promise<T> {
  return Promise.race([
    p,
    new Promise<T>((_, reject) => setTimeout(() => reject(new Error(`${what}: did not settle in ${ms}ms`)), ms)),
  ])
}

interface Ctx {
  outbox: Outbox
  transport: ScriptedTransport
  store: OutboxStore
  clock: FakeClock
  item(clientId: Uuid): OutboxItem | undefined
  close(): void
}

async function context(opts: {
  store?: OutboxStore
  transport?: ScriptedTransport
  faults?: Faults
  accountId?: Uuid | null
  lock?: InProcessLockHub
  channel?: OutboxChannel | null
  storage?: PersistApi | null
  clock?: FakeClock
  mintId?: () => Uuid
  uploader?: Uploader
  onView?: (items: OutboxItem[]) => void
}): Promise<Ctx> {
  const store = opts.store ?? new MemoryOutboxStore()
  const transport = opts.transport ?? new ScriptedTransport()
  const clock = opts.clock ?? new FakeClock()
  let items: OutboxItem[] = []
  const outbox = await Outbox.open({
    store,
    transport,
    accountId: opts.accountId === undefined ? ME : opts.accountId,
    ...(opts.lock ? { lock: opts.lock.lock() } : {}),
    channel: opts.channel ?? null,
    storage: opts.storage === undefined ? { persist: async () => true, persisted: async () => true } : opts.storage,
    clock,
    random: () => 0.5,
    ...(opts.mintId ? { mintId: opts.mintId } : {}),
    ...(opts.uploader ? { uploader: opts.uploader } : {}),
    faults: opts.faults ?? {},
    onChange: (view) => {
      items = view.items
      opts.onView?.(view.items)
    },
  })
  items = outbox.view().items
  return {
    outbox,
    transport,
    store,
    clock,
    item: (id) => items.find((i) => i.entry.clientId === id),
    close: () => outbox.close(),
  }
}

async function composeText(ctx: Ctx, text = 'hello', conversationId = CONV): Promise<OutboxEntry> {
  return within('compose', ctx.outbox.compose({ conversationId, text }))
}

async function stored(store: OutboxStore, clientId: Uuid): Promise<OutboxEntry | undefined> {
  return (await store.list()).find((e) => e.clientId === clientId)
}

/* ── attachments (CANT-162) ──────────────────────────────────────────────── */

/** The tests' uploader: it records every call — the entry as it was handed,
 *  copied, and the attachment — and answers each with what `answer` returns:
 *  a handle, a promise held open, or a rejection. It is not the oracle; the
 *  faults on criteria 13 and 14 are what keep those criteria honest. */
class ScriptedUploader implements Uploader {
  readonly calls: { entry: OutboxEntry; attachment: OutboundAttachmentDraft }[] = []
  constructor(
    private readonly answer: (entry: OutboxEntry, attachment: OutboundAttachmentDraft, n: number) => Promise<Uuid>,
  ) {}
  upload(entry: OutboxEntry, attachment: OutboundAttachmentDraft): Promise<Uuid> {
    this.calls.push({ entry: structuredClone(entry), attachment })
    return this.answer(entry, attachment, this.calls.length)
  }
}

/** A new handle on every call. */
const minting = () => {
  let n = 0
  return new ScriptedUploader(async () => `up-${++n}`)
}
/** A promise that never settles. */
const never = () => new ScriptedUploader(() => new Promise<Uuid>(() => undefined))
const rejecting = (message = 'would not go') =>
  new ScriptedUploader(() => Promise.reject(new UploadRefused(message)))

interface Deferred<T> {
  promise: Promise<T>
  resolve(value: T): void
}
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((r) => (resolve = r))
  return { promise, resolve }
}

/** Not text, not short, and not the same at every index: a store that kept a
 *  prefix, or a string, would not give these back. */
const recording = (seed = 7) => Uint8Array.from({ length: 4096 }, (_, i) => (i * 31 + seed) & 0xff)
const bytesOf = async (blob: Blob) => new Uint8Array(await blob.arrayBuffer())

const voiceAttachment = (bytes: Uint8Array): OutboundAttachmentDraft => ({
  kind: 'voice',
  blob: new Blob([bytes], { type: 'audio/ogg' }),
  durationMs: 1700,
  upload: 'pending',
})
const voiceDraft = (bytes: Uint8Array, text?: string): OutboxDraft => ({
  conversationId: CONV,
  ...(text !== undefined ? { text } : {}),
  attachments: [voiceAttachment(bytes)],
})
const imageDraft = (bytes: Uint8Array): OutboxDraft => ({
  conversationId: CONV,
  attachments: [{ kind: 'image', blob: new Blob([bytes], { type: 'image/png' }), filename: 'a.png', upload: 'pending' }],
})

const composeVoice = (ctx: Ctx, bytes = recording(), text?: string) =>
  within('compose voice', ctx.outbox.compose(voiceDraft(bytes, text)))

const handlesOf = (e: OutboxEntry | undefined) => (e?.attachments ?? []).map((a) => [a.upload, a.uploadId])

const UPLOAD_NOT_FOUND = { code: 'upload_not_found', message: 'unknown upload', retryable: false } as const

/** Every readwrite transaction opened on any fake-indexeddb connection, with
 *  the durability it was opened with — asserted on, because a browser's flush
 *  is not observable from node. */
const writes: { durability: string; stores: string[] }[] = []
{
  const proto = FakeIDBDatabase.prototype as unknown as {
    transaction: (...args: unknown[]) => { durability: string; mode: string; objectStoreNames: ArrayLike<string> }
  }
  const original = proto.transaction
  proto.transaction = function (this: unknown, ...args: unknown[]) {
    const tx = original.apply(this, args)
    if (tx.mode === 'readwrite') writes.push({ durability: tx.durability, stores: Array.from(tx.objectStoreNames) })
    return tx
  }
}

/* ── criterion 0 · durable before render and before any frame ────────────── */

async function c0(f: Faults) {
  const deps = idb()
  const store = await IdbOutboxStore.open(deps, storeFaults(f))
  // A second, independent connection sees only what has committed.
  const probe = await IdbOutboxStore.open(deps)
  // Settled into a value at once, so a failed check is reported by this
  // criterion rather than escaping as an unhandled rejection.
  const visible = (id: Uuid, at: string): Promise<Error | null> =>
    probe.list().then(
      (rows) => (rows.some((r) => r.clientId === id) ? null : new Error(`entry not committed at the first ${at}`)),
      (e: Error) => e,
    )
  const checks: Promise<Error | null>[] = []
  const firstRender = new Set<Uuid>()
  const transport = new ScriptedTransport()
  let frames = 0
  transport.onFrame = (frame) => {
    if (frames++ === 0) checks.push(visible(frame.clientId, 'frame'))
  }
  const ctx = await context({
    store,
    transport,
    faults: f,
    onView: (items) => {
      for (const i of items) {
        if (firstRender.has(i.entry.clientId)) continue
        firstRender.add(i.entry.clientId)
        checks.push(visible(i.entry.clientId, 'render'))
      }
    },
  })
  transport.open()
  await ticks()

  writes.length = 0
  const entry = await composeText(ctx)
  await ticks()
  assert.equal(transport.framesFor(entry.clientId).length, 1, 'the entry was sent')
  assert.ok(firstRender.has(entry.clientId), 'the entry was rendered')
  for (const failure of await Promise.all(checks)) if (failure) throw failure
  assert.ok(writes.length > 0, 'an entry write was observed')
  for (const w of writes) assert.equal(w.durability, 'strict', `an entry write on ${w.stores} was not strict`)

  // The crash: the moment send() resolves, drop everything without awaiting.
  const crashStore = await IdbOutboxStore.open(deps, storeFaults(f))
  const crash = await context({ store: crashStore })
  const pending = crash.outbox.compose({ conversationId: CONV, text: 'then the power went' })
  const crashed = await within('compose before the crash', pending)
  crash.close()
  crashStore.close()
  const fresh = await IdbOutboxStore.open(deps)
  assert.ok(await stored(fresh, crashed.clientId), 'the entry is loadable after the crash')
  ctx.close()
  store.close()
  probe.close()
  fresh.close()
}

/* ── criterion 1 · one clientId, minted once, on every frame ─────────────── */

async function c1(f: Faults) {
  const original = crypto.randomUUID.bind(crypto)
  let mints = 0
  const spy = () => {
    mints++
    return original()
  }
  crypto.randomUUID = spy as typeof crypto.randomUUID
  try {
    const store = new MemoryOutboxStore()
    const server = new ScriptedServer()
    const clock = new FakeClock()
    const t1 = new ScriptedTransport(server)
    let ctx = await context({ store, transport: t1, clock, faults: f })
    t1.open()
    await ticks()
    const entry = await composeText(ctx)
    await ticks()
    assert.equal(mints, 1, 'minted once, at compose, with crypto.randomUUID()')
    // Dropped before the ack.
    t1.close()
    t1.open()
    await ticks()
    // A reload.
    ctx.close()
    const t2 = new ScriptedTransport(server)
    ctx = await context({ store, transport: t2, clock, faults: f })
    t2.open()
    await ticks()
    // A retryable refusal, held and resent.
    const inflight = t2.frames.at(-1)!.clientId
    t2.refuse(inflight, { code: 'internal', message: 'try again', retryable: true })
    await ticks()
    await clock.advance(BACKOFF_CAP_MS)
    // A non-retryable refusal, then a manual RETRY.
    const again = t2.frames.at(-1)!.clientId
    t2.refuse(again, { code: 'not_a_member', message: 'not a member', retryable: false })
    await ticks()
    const failedId = (await store.list())[0].clientId
    await ctx.outbox.retry(failedId)
    await ticks()

    const all = server.received.map((fr) => fr.clientId)
    assert.ok(all.length >= 5, `five frames written (first, after a drop, after a reload, after a hold, after RETRY): ${all.length}`)
    for (const id of all) assert.equal(id, entry.clientId, 'every frame carries the minted clientId')
    assert.equal((await store.list()).length, 1, 'still one entry')
    ctx.close()
  } finally {
    crypto.randomUUID = original
  }
}

/* ── criterion 2 · a send survives a reload before its ack ───────────────── */

async function c2(f: Faults) {
  const store = new MemoryOutboxStore()
  const server = new ScriptedServer()
  const t1 = new ScriptedTransport(server)
  let ctx = await context({ store, transport: t1, faults: f })
  t1.open()
  await ticks()
  const entry = await composeText(ctx)
  await ticks()
  ctx.close()

  const t2 = new ScriptedTransport(server)
  ctx = await context({ store, transport: t2, faults: f })
  assert.equal(ctx.item(entry.clientId)?.state, 'queued', 'renders QUEUED after the reload')
  t2.open()
  await ticks()
  assert.equal(t2.framesFor(entry.clientId).length, 1, 'written again on the next ready')
  t2.ack(entry.clientId)
  await ticks()
  assert.equal(ctx.item(entry.clientId)?.state, 'sent', 'SENT on the ack')
  assert.equal((await store.list()).length, 1, 'no second entry')
  ctx.close()
}

/* ── criterion 3 · sending, queued and the ack are never persisted ───────── */

const ENTRY_KEYS = new Set([
  'v', 'clientId', 'accountId', 'conversationId', 'order', 'composedAt', 'text', 'replyToMessageId',
  'replyPreview', 'attachments', 'status', 'attempts', 'internalRetries', 'reuploads', 'notBefore', 'lastError',
])

async function assertRecordShape(store: OutboxStore) {
  for (const e of await store.list()) {
    assert.ok(e.status === 'pending' || e.status === 'failed', `stored status is ${e.status}`)
    for (const k of Object.keys(e)) assert.ok(ENTRY_KEYS.has(k), `stored field ${k} is not in the entry shape`)
  }
}

async function c3(f: Faults) {
  const store = new MemoryOutboxStore()
  const server = new ScriptedServer()
  const t1 = new ScriptedTransport(server)
  let ctx = await context({ store, transport: t1, faults: f })
  t1.open()
  await ticks()
  const flying = await composeText(ctx, 'in flight')
  const acked = await composeText(ctx, 'acked')
  await ticks()
  t1.ack(acked.clientId)
  await ticks()
  await assertRecordShape(store)
  assert.equal(ctx.item(flying.clientId)?.state, 'sending', 'in flight renders SENDING, from memory')
  assert.equal(ctx.item(acked.clientId)?.state, 'sent', 'acked renders SENT, from memory')

  ctx.close()
  ctx = await context({ store, transport: new ScriptedTransport(server), faults: f })
  assert.equal(ctx.item(flying.clientId)?.state, 'queued', 'an in-flight entry reloads QUEUED')
  assert.equal(ctx.item(acked.clientId)?.state, 'queued', 'an acked, unsettled entry reloads QUEUED')
  await assertRecordShape(store)
  ctx.close()
}

/* ── criterion 4 · the ack places it; a record settles it, in any status ─── */

async function c4(f: Faults) {
  const store = new MemoryOutboxStore()
  const hub = new InProcessChannelHub()
  const lock = new InProcessLockHub()
  const transport = new ScriptedTransport()
  const ctx = await context({ store, transport, faults: f, lock, channel: hub.channel() })
  // A second tab on the same store, never ready: it renders what the first does.
  const other = await context({ store, faults: f, lock, channel: hub.channel() })
  transport.open()
  await ticks()

  // The SERVER's conversation, seq and at — not the entry's.
  const moved = await composeText(ctx, 'answered elsewhere', 'c-a')
  await ticks()
  const ack = transport.ack(moved.clientId, 'c-b')
  await ticks()
  const shown = project(ctx.item(moved.clientId)!)
  assert.equal(shown.state, 'sent')
  assert.equal(shown.conversationId, 'c-b', "rendered in the ack's conversation")
  assert.equal(shown.seq, ack.seq, "at the ack's seq")
  assert.equal(shown.at, ack.at, "at the ack's time, not composedAt")
  assert.notEqual(shown.at, moved.composedAt)

  // duplicate: true is handled identically.
  const dup = transport.ack(moved.clientId, 'c-b')
  assert.equal(dup.duplicate, true)
  await ticks()
  assert.equal(project(ctx.item(moved.clientId)!).state, 'sent')
  assert.equal(project(ctx.item(moved.clientId)!).seq, ack.seq)

  // One entry in each status, each then met by its record.
  const internal = await composeText(ctx, 'internal')
  const tooLarge = await composeText(ctx, 'too large')
  await ticks()
  transport.refuse(internal.clientId, { code: 'internal', message: 'unknown outcome', retryable: false })
  transport.refuse(tooLarge.clientId, { code: 'message_too_large', message: 'too large', retryable: false })
  await ticks()
  const bare = await composeText(ctx, 'bare 1008')
  await ticks()
  transport.close(true)
  await ticks()
  transport.open()
  await ticks()
  const flying = await composeText(ctx, 'in flight')
  const queued = await composeText(ctx, 'queued')
  await ticks()
  transport.refuse(queued.clientId, { code: 'rate_limited', message: 'later', retryable: true, retryAfterSec: 60 })
  await ticks()
  for (const e of [internal, tooLarge, bare]) assert.equal(ctx.item(e.clientId)?.state, 'failed', `${e.text} failed`)
  assert.equal(ctx.item(flying.clientId)?.state, 'sending', 'one in flight')
  assert.equal(ctx.item(queued.clientId)?.state, 'queued', 'one queued')
  assert.equal(ctx.item(moved.clientId)?.state, 'sent', 'one acked')

  for (const e of [moved, flying, internal, tooLarge, bare, queued]) transport.deliver(e.clientId)
  await ticks()
  for (const e of [moved, flying, internal, tooLarge, bare, queued]) {
    assert.equal(await stored(store, e.clientId), undefined, `${e.text} settled from the store`)
    assert.equal(ctx.item(e.clientId), undefined, `${e.text} settled from this tab's render`)
  }
  await waitFor("the other tab's render settles", () => other.outbox.view().items.length === 0)
  assert.ok(!ctx.outbox.view().items.some((i) => i.state === 'failed'), 'no FAILED row beside a record')
  ctx.close()
  other.close()
}

/* ── criterion 5 · no ordinal of its own; unread rules unmoved ───────────── */

async function c5(_f: Faults) {
  const source = readFileSync(resolve(process.cwd(), 'src/store.ts'), 'utf8')
  assert.ok(!/\bnextSeq\b/.test(source), 'state.nextSeq is gone')
  assert.ok(!source.includes('1_000_000'), 'the logSeq placeholder is gone')
  assert.ok(!/\badvance\s*\(/.test(source), "advance()'s timer is gone")

  const transport = new ScriptedTransport()
  await useOutbox({ store: new MemoryOutboxStore(), transport, storage: null })
  select(CONV)
  const kitchen = state.conversations.find((c) => c.id === CONV)!
  state.read.delete(CONV)
  const before = { log: activeMessages.value.length, n: newCount(kitchen), u: unreadCount(kitchen) }
  const same = (at: string) => {
    assert.equal(activeMessages.value.length, before.log, `${at}: messagesFor() is unchanged`)
    assert.equal(newCount(kitchen), before.n, `${at}: newCount is unchanged`)
    assert.equal(unreadCount(kitchen), before.u, `${at}: unreadCount is unchanged`)
  }
  state.composer.draft = 'counted nowhere'
  const failing = (await send())!
  same('queued')
  transport.open()
  await ticks()
  transport.refuse(failing, { code: 'not_a_member', message: 'no', retryable: false })
  await ticks()
  same('failed')
  state.composer.draft = 'acked'
  const acked = (await send())!
  await ticks()
  transport.ack(acked)
  await ticks()
  same('acked')
  assert.ok(!state.messages.some((m) => m.clientId === failing || m.clientId === acked), 'no entry is in the log')
}

/* ── criterion 6 · the rail merges the outbox tail ───────────────────────── */

function railRow(html: string, id: string): string {
  const at = html.indexOf(`data-conversation-id="${id}"`)
  if (at < 0) return ''
  return html.slice(html.lastIndexOf('<button', at), html.indexOf('</button>', at))
}
const text = (html: string) => html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()

async function c6(_f: Faults) {
  // The device clock, after every fixture: the fixtures are dated relative to
  // today, so "now" can fall before some of them.
  const clock = new FakeClock()
  clock.t = Math.max(...state.messages.map((m) => Date.parse(m.at))) + 3_600_000
  const transport = new ScriptedTransport()
  transport.server.at = () => new Date(clock.t + 60_000).toISOString()
  await useOutbox({ store: new MemoryOutboxStore(), transport, storage: null, clock })
  const column = directs.value
  const bottom = column[column.length - 1]
  assert.notEqual(column[0].id, bottom.id)
  select(bottom.id)
  const render = () => renderToString(createSSRApp(App))

  state.composer.draft = 'from the bottom of the column'
  const id = (await send())!
  let row = railRow(await render(), bottom.id)
  assert.equal(lastMessageOf(bottom.id)?.id, id, "lastMessageOf returns the entry's projection")
  assert.ok(text(row).includes('You: from the bottom of the column'), text(row))
  assert.ok(text(row).includes('QUEUED'), text(row))
  assert.equal(directs.value[0].id, bottom.id, 'an offline send moves its room to the top')

  transport.open()
  await ticks()
  row = railRow(await render(), bottom.id)
  assert.ok(text(row).includes('SENDING'), text(row))

  transport.refuse(id, { code: 'conversation_not_found', message: 'gone', retryable: false })
  await ticks()
  row = railRow(await render(), bottom.id)
  assert.ok(text(row).includes('FAILED'), text(row))
  assert.ok(/class="(fault marker|marker fault)"/.test(row), 'FAILED as a fault')

  state.composer.draft = 'acked later'
  const acked = (await send())!
  await ticks()
  const ack = transport.ack(acked)
  await ticks()
  assert.equal(lastMessageOf(bottom.id)?.at, ack.at, 'an acked entry is dated by ack.at')
  assert.equal(directs.value[0].id, bottom.id)

  // smoke.ts's half of this criterion: sections 2 and 5 exist as rewritten.
  const smoke = readFileSync(resolve(process.cwd(), 'smoke.ts'), 'utf8')
  // Since CANT-39 the acking transport is a live server's, not a script's.
  assert.ok(smoke.includes('never invents DELIVERED') && smoke.includes('liveTransport()'), 'smoke section 5 drives an acking transport')
  assert.ok(smoke.includes('the failed row comes from the outbox store'), "smoke section 2's FAILED comes from the outbox")
}

/* ── criterion 7 · the error table under ruling 3 C ──────────────────────── */

async function c7(f: Faults) {
  const clock = new FakeClock()
  const transport = new ScriptedTransport()
  const ctx = await context({ transport, clock, faults: f })
  transport.open()
  await ticks()

  // Every non-retryable row fails at once with the server's message.
  const failing = [
    { code: 'not_a_member', retryable: false },
    { code: 'conversation_not_found', retryable: false },
    { code: 'message_too_large', retryable: false },
    { code: 'internal', retryable: false },
    { code: 'unauthorized', retryable: false },
    { code: 'wire_version_unsupported', retryable: false },
    { code: 'unknown', retryable: true },
  ] as const
  const bystander = await composeText(ctx, 'bystander')
  await ticks()
  for (const row of failing) {
    const e = await composeText(ctx, row.code)
    await ticks()
    const before = await stored(ctx.store, bystander.clientId)
    transport.refuse(e.clientId, { code: row.code, message: `refused: ${row.code}`, retryable: row.retryable })
    await ticks()
    const item = ctx.item(e.clientId)!
    assert.equal(item.state, 'failed', `${row.code} fails at once`)
    assert.equal(project(item).error, `refused: ${row.code}`, "the server's message is the inline error")
    assert.deepEqual(await stored(ctx.store, bystander.clientId), before, 'an error naming another clientId changes nothing else')
    assert.equal(ctx.item(bystander.clientId)?.state, 'sending')
    const sent = transport.framesFor(e.clientId).length
    await clock.advance(BACKOFF_CAP_MS * 2)
    assert.equal(transport.framesFor(e.clientId).length, sent, `${row.code} is not resent`)
  }

  // rate_limited: held, with retryAfterSec as a floor.
  const limited = await composeText(ctx, 'rate limited')
  await ticks()
  transport.refuse(limited.clientId, { code: 'rate_limited', message: 'slow down', retryable: true, retryAfterSec: 30 })
  await ticks()
  assert.equal(ctx.item(limited.clientId)?.state, 'queued', 'rate_limited holds')
  await clock.advance(29_000)
  assert.equal(transport.framesFor(limited.clientId).length, 1, 'not before retryAfterSec')
  await clock.advance(1_000)
  assert.equal(transport.framesFor(limited.clientId).length, 2, 'resent at retryAfterSec')

  // internal, retryable: held under 2 s doubling, capped at 5 min; RETRYING
  // after three; never failed.
  const held = await composeText(ctx, 'held')
  await ticks()
  for (let n = 1; n <= 10; n++) {
    const now = clock.now()
    transport.refuse(held.clientId, { code: 'internal', message: 'db down', retryable: true })
    await ticks()
    const item = ctx.item(held.clientId)!
    assert.notEqual(item.state, 'failed', `refusal ${n} of a retryable internal never fails`)
    assert.equal(item.entry.internalRetries, n, 'one count per refusal')
    const ceiling = Math.min(BACKOFF_CAP_MS, 2000 * 2 ** (n - 1))
    assert.equal(Date.parse(item.entry.notBefore!) - now, Math.floor(0.5 * ceiling), `backoff ${n}`)
    assert.equal(item.retrying, n >= 3, `RETRYING after three (at ${n})`)
    await clock.advance(ceiling)
    assert.equal(transport.framesFor(held.clientId).length, n + 1, `resent after backoff ${n}`)
  }
  ctx.close()
}

/* ── criterion 8 · CANT-31 §7, consumed ──────────────────────────────────── */

async function c8(f: Faults) {
  const transport = new ScriptedTransport()
  const ctx = await context({ transport, faults: f })
  transport.open()
  await ticks()
  const bare = await composeText(ctx, 'bare')
  await ticks()
  transport.close(true)
  await ticks()
  const item = ctx.item(bare.clientId)!
  assert.equal(item.state, 'failed', 'a bare 1008 fails the in-flight entry')
  assert.deepEqual(item.entry.lastError, { kind: 'bare_1008' })
  assert.equal(project(item).error, BARE_1008_MESSAGE)
  transport.open()
  await ticks()
  assert.equal(transport.framesFor(bare.clientId).length, 1, 'and it is not resent')

  // Every other close, including a 1008 the transport reports as
  // frame-preceded: the transport's classification is bare1008 false.
  for (const close of ['1008 frame-preceded', '4001', '4000', '4002', '1001', 'abnormal']) {
    const e = await composeText(ctx, close)
    await ticks()
    transport.close(false)
    await ticks()
    assert.equal(ctx.item(e.clientId)?.state, 'queued', `${close}: stays pending`)
    transport.open()
    await ticks()
    assert.equal(transport.framesFor(e.clientId).length, 2, `${close}: resent on the next ready`)
    transport.deliver(e.clientId)
    await ticks()
  }
  ctx.close()
}

/* ── criterion 9 · failed is reachable and recoverable ───────────────────── */

async function c9(f: Faults) {
  const store = new MemoryOutboxStore()
  const hub = new InProcessChannelHub()
  const lock = new InProcessLockHub()
  const server = new ScriptedServer()
  const clock = new FakeClock()
  const t1 = new ScriptedTransport(server)
  let ctx = await context({ store, transport: t1, clock, faults: f, lock, channel: hub.channel() })
  t1.open()
  await ticks()
  const e = await composeText(ctx, 'will fail')
  const other = await composeText(ctx, 'will be deleted')
  await ticks()
  t1.refuse(e.clientId, { code: 'not_a_member', message: 'removed from the room', retryable: false })
  t1.refuse(other.clientId, { code: 'message_too_large', message: 'too large', retryable: false })
  await ticks()
  ctx.close()

  const t2 = new ScriptedTransport(server)
  ctx = await context({ store, transport: t2, clock, faults: f, lock, channel: hub.channel() })
  const second = await context({ store, faults: f, lock, channel: hub.channel() })
  const item = ctx.item(e.clientId)!
  assert.equal(item.state, 'failed', 'survives a reload as FAILED')
  assert.equal(project(item).error, 'removed from the room', 'with its inline error')
  t2.open()
  await ticks()
  await clock.advance(BACKOFF_CAP_MS * 2)
  assert.equal(t2.frames.length, 0, 'never resent without a RETRY')

  await ctx.outbox.retry(e.clientId)
  await ticks()
  assert.equal(t2.framesFor(e.clientId).length, 1, 'RETRY sends it, under the same clientId')
  t2.ack(e.clientId)
  await ticks()
  assert.equal(ctx.item(e.clientId)?.state, 'sent', 'and it reaches SENT')

  assert.equal(await ctx.outbox.discard(e.clientId), false, 'DELETE is not offered on a non-failed entry')
  assert.ok(await stored(store, e.clientId))
  assert.equal(await ctx.outbox.discard(other.clientId), true)
  assert.equal(await stored(store, other.clientId), undefined, 'DELETE removes it from the store')
  assert.equal(ctx.item(other.clientId), undefined, 'and from this render')
  await waitFor("and from the other tab's render", () =>
    !second.outbox.view().items.some((i) => i.entry.clientId === other.clientId))
  ctx.close()
  second.close()
}

/* ── criterion 10 · nothing deletes an entry on terminal or bootstrap ────── */

async function c10(f: Faults) {
  const deps = idb()
  // CANT-35's database, as it might hold a credential and a journal.
  await new Promise<void>((res, rej) => {
    const req = deps.factory.open('catenary', 1)
    req.onupgradeneeded = () => req.result.createObjectStore('journal')
    req.onsuccess = () => {
      const tx = req.result.transaction('journal', 'readwrite')
      tx.objectStore('journal').put({ cursor: 42 }, 'cursor')
      tx.oncomplete = () => {
        req.result.close()
        res()
      }
    }
    req.onerror = () => rej(req.error)
  })
  const store = await IdbOutboxStore.open(deps)
  const transport = new ScriptedTransport()
  const ctx = await context({ store, transport, faults: f })
  transport.open()
  await ticks()
  const failed = await composeText(ctx, 'failed')
  const pending = await composeText(ctx, 'pending')
  await ticks()
  transport.refuse(failed.clientId, { code: 'not_a_member', message: 'no', retryable: false })
  await ticks()

  await new Promise<void>((res, rej) => {
    const req = deps.factory.deleteDatabase('catenary')
    req.onsuccess = () => res()
    req.onerror = () => rej(req.error)
  })
  assert.equal((await store.list()).length, 2, "deleting CANT-35's database leaves every entry")

  transport.close(false, true) // terminal
  await ticks()
  assert.equal((await store.list()).length, 2, 'a terminal state leaves every entry')
  transport.emit({ type: 'bootstrap' })
  await ticks()
  assert.equal((await store.list()).length, 2, 'a bootstrap leaves every entry')

  transport.open()
  await ticks()
  assert.equal(transport.framesFor(pending.clientId).length, 2, 'the pending entry resends once live again')
  assert.equal(ctx.item(failed.clientId)?.state, 'failed', 'the failed one is still there, still failed')
  ctx.close()
  store.close()
}

/* ── criterion 11 · entries belong to an account ─────────────────────────── */

async function c11(f: Faults) {
  const store = new MemoryOutboxStore()
  const theirs = await context({ store, accountId: 'u-someone-else', faults: f })
  const foreign = await composeText(theirs, 'composed as someone else')
  theirs.close()

  const transport = new ScriptedTransport()
  const ctx = await context({ store, transport, faults: f })
  const mine = await composeText(ctx, 'mine')
  assert.equal(ctx.item(foreign.clientId), undefined, "another account's entry is rendered nowhere")
  transport.open()
  await ticks()
  assert.equal(transport.framesFor(mine.clientId).length, 1)
  assert.equal(transport.framesFor(foreign.clientId).length, 0, "another account's entry is never sent")
  ctx.close()
}

/* ── criterion 12 · order is drawn inside the put's own transaction ──────── */

async function c12(f: Faults) {
  const deps = idb()
  const a = await context({ store: await IdbOutboxStore.open(deps), faults: f })
  const b = await context({ store: await IdbOutboxStore.open(deps), faults: f })
  const composed: OutboxEntry[] = []
  for (let gap = 0; gap < 6; gap++) {
    const first = a.outbox.compose({ conversationId: CONV, text: `a${gap}` })
    for (let i = 0; i < gap; i++) await new Promise((r) => setTimeout(r, 0))
    const second = b.outbox.compose({ conversationId: CONV, text: `b${gap}` })
    composed.push(...(await within('concurrent compose', Promise.all([first, second]))))
  }
  const orders = composed.map((e) => e.order)
  assert.equal(new Set(orders).size, orders.length, `distinct orders: ${orders.join(',')}`)
  a.close()
  b.close()

  const transport = new ScriptedTransport()
  const drainer = await context({ store: await IdbOutboxStore.open(deps), transport, faults: f })
  transport.open()
  await ticks()
  const expected = [...composed].sort((x, y) => x.order - y.order).map((e) => e.clientId)
  assert.deepEqual(transport.frames.map((fr) => fr.clientId), expected, 'drained in allocation order')
  drainer.close()
}

/* ── criterion 13 · no head-of-line blocking ─────────────────────────────── */

async function c13(f: Faults) {
  const transport = new ScriptedTransport()
  // Every upload is held open until the test resolves it.
  const answers: Deferred<Uuid>[] = []
  const uploader = new ScriptedUploader(() => {
    const d = deferred<Uuid>()
    answers.push(d)
    return d.promise
  })
  const ctx = await context({ transport, faults: f, uploader })
  transport.open()
  await ticks()

  // An attachment entry, then a text, in one conversation on a ready session:
  // the text is written and acked while the upload is still open.
  const att = await composeVoice(ctx, recording(1))
  const text = await composeText(ctx, 'overtakes the upload')
  await ticks()
  assert.equal(answers.length, 1, 'the attachment was offered once')
  assert.equal(transport.framesFor(att.clientId).length, 0, 'no frame for the attachment while its upload is open')
  assert.equal(transport.framesFor(text.clientId).length, 1, 'the text is written past it')
  transport.ack(text.clientId)
  await ticks()
  assert.equal(ctx.item(text.clientId)?.state, 'sent', 'and acked')
  assert.equal(ctx.item(att.clientId)?.state, 'queued', 'the attachment entry waits')

  // When the upload resolves, the frame carries the handle under the ORIGINAL
  // clientId — and at the moment it is written the stored entry already
  // carries that handle (read synchronously in onFrame, as criterion 0 does).
  const atFrame: Promise<OutboxEntry | undefined>[] = []
  transport.onFrame = (frame) => {
    if (frame.clientId === att.clientId) atFrame.push(stored(ctx.store, att.clientId))
  }
  answers[0].resolve('up-1')
  await ticks()
  const frames = transport.framesFor(att.clientId)
  assert.equal(frames.length, 1, 'sent once its upload landed')
  assert.equal(frames[0].clientId, att.clientId, 'under its original clientId')
  assert.deepEqual(frames[0].attachments, [{ kind: 'voice', uploadId: 'up-1' }], 'with the handle the uploader returned')
  assert.deepEqual(handlesOf(await atFrame[0]), [['uploaded', 'up-1']], 'the stored entry carried the handle when the frame was written')
  transport.ack(att.clientId)
  await ticks()
  assert.equal(ctx.item(att.clientId)?.state, 'sent')

  // Two attachments on one entry: offered in order, each handle stored before
  // the next is asked for, and no frame until the second resolves.
  const two = await within(
    'compose two',
    ctx.outbox.compose({ conversationId: CONV, attachments: [voiceAttachment(recording(2)), voiceAttachment(recording(3))] }),
  )
  await ticks()
  assert.equal(answers.length, 2, 'the first of the two is offered')
  answers[1].resolve('up-2')
  await ticks()
  assert.equal(answers.length, 3, "then the second, once the first's handle is stored")
  assert.deepEqual(handlesOf(await stored(ctx.store, two.clientId)), [['uploaded', 'up-2'], ['pending', undefined]])
  // A text composed between the two uploads drains past the entry.
  const between = await composeText(ctx, 'between the two uploads')
  await ticks()
  assert.equal(transport.framesFor(two.clientId).length, 0, 'no frame until the last upload resolves')
  assert.equal(transport.framesFor(between.clientId).length, 1, 'the entry is overtaken (§11)')
  answers[2].resolve('up-3')
  await ticks()
  assert.deepEqual(transport.framesFor(two.clientId).map((fr) => fr.attachments), [
    [{ kind: 'voice', uploadId: 'up-2' }, { kind: 'voice', uploadId: 'up-3' }],
  ])
  ctx.close()
}

/* ── criterion 14 · a stale handle re-uploads, once ──────────────────────── */

async function c14(f: Faults) {
  const transport = new ScriptedTransport()
  const uploader = minting()
  const ctx = await context({ transport, faults: f, uploader })
  transport.open()
  await ticks()
  const bytes = recording(4)
  const e = await composeVoice(ctx, bytes)
  await ticks()
  assert.equal(uploader.calls.length, 1)
  assert.deepEqual(transport.framesFor(e.clientId).map((fr) => fr.attachments), [[{ kind: 'voice', uploadId: 'up-1' }]], 'sent with the first handle')

  // The server disowns the handle: the entry stays pending, the held Blob is
  // uploaded again, and the resend carries the SAME clientId — which is also
  // the evidence for criterion 1's "resend after a stale-upload re-upload".
  transport.refuse(e.clientId, UPLOAD_NOT_FOUND)
  await ticks()
  const after = await stored(ctx.store, e.clientId)
  assert.equal(after?.status, 'pending', 'pending after the first upload_not_found')
  assert.notEqual(ctx.item(e.clientId)?.state, 'failed')
  assert.equal(uploader.calls.length, 2, 'uploaded a second time')
  assert.deepEqual(await bytesOf(uploader.calls[1].attachment.blob), bytes, 'with a Blob of the same bytes')
  assert.deepEqual(handlesOf(uploader.calls[1].entry), [['pending', undefined]], 'the stale handle was cleared before the offer')
  const frames = transport.framesFor(e.clientId)
  assert.equal(frames.length, 2, 'resent')
  assert.equal(frames[1].clientId, e.clientId, 'under the same clientId')
  assert.deepEqual(frames[1].attachments, [{ kind: 'voice', uploadId: 'up-2' }], 'with the second handle')
  assert.equal(after?.reuploads, 1, 'reuploads is 1')

  // A second upload_not_found: failed, with the server's code, and no third
  // upload.
  transport.refuse(e.clientId, UPLOAD_NOT_FOUND)
  await ticks()
  const item = ctx.item(e.clientId)!
  assert.equal(item.state, 'failed', 'a second upload_not_found fails it')
  assert.equal(item.entry.lastError?.kind === 'server' ? item.entry.lastError.code : item.entry.lastError?.kind, 'upload_not_found')
  assert.equal(uploader.calls.length, 2, 'no third upload')
  assert.equal(transport.framesFor(e.clientId).length, 2, 'and no third frame')
  await ctx.clock.advance(BACKOFF_CAP_MS * 2)
  assert.equal(uploader.calls.length, 2)
  assert.equal(transport.framesFor(e.clientId).length, 2)

  // An Uploader that rejects: failed with the uploader's message after
  // exactly one call, and no frame — never a loop.
  const refusing = rejecting()
  const t2 = new ScriptedTransport()
  const ctx2 = await context({ transport: t2, faults: f, uploader: refusing })
  t2.open()
  await ticks()
  const r = await composeVoice(ctx2, recording(5))
  await ticks()
  const ri = ctx2.item(r.clientId)!
  assert.equal(ri.state, 'failed', 'a rejection fails the entry')
  assert.equal(ri.entry.lastError?.kind, 'upload')
  assert.equal(project(ri).error, 'would not go', "with the uploader's message inline")
  assert.equal(refusing.calls.length, 1, 'after exactly one call')
  assert.equal(t2.frames.length, 0, 'and no frame')
  await ctx2.clock.advance(BACKOFF_CAP_MS * 2)
  assert.equal(refusing.calls.length, 1, 'and it is not offered again by itself')
  ctx.close()
  ctx2.close()
}

/* ── criterion 15 · one drainer, held only while ready ───────────────────── */

async function c15(f: Faults) {
  const deps = idb()
  const lock = new InProcessLockHub()
  const hub = new InProcessChannelHub()
  const server = new ScriptedServer()
  const tA = new ScriptedTransport(server)
  const tB = new ScriptedTransport(server)
  const A = await context({ store: await IdbOutboxStore.open(deps), transport: tA, faults: f, lock, channel: hub.channel() })
  const B = await context({ store: await IdbOutboxStore.open(deps), transport: tB, faults: f, lock, channel: hub.channel() })
  const violations: string[] = []
  tA.onFrame = (fr) => { if (!A.outbox.isHolder) violations.push(`A wrote ${fr.text} without the lock`) }
  tB.onFrame = (fr) => { if (!B.outbox.isHolder) violations.push(`B wrote ${fr.text} without the lock`) }
  await ticks()
  assert.equal(lock.held, false, 'nobody requests the lock without a ready session')

  tA.open()
  await ticks()
  assert.ok(A.outbox.isHolder, 'A holds while ready')
  tB.open()
  await ticks()
  assert.ok(!B.outbox.isHolder, 'B waits')

  // Composed in the tab that does not drain; sent by the one that does.
  const fromB = await composeText(B, 'from B')
  await waitFor('the holder sends it', () => tA.framesFor(fromB.clientId).length === 1)
  assert.equal(tB.frames.length, 0, 'only the holder writes frames')
  await waitFor('both tabs render SENDING', () =>
    A.item(fromB.clientId)?.state === 'sending' && B.item(fromB.clientId)?.state === 'sending')
  tA.ack(fromB.clientId)
  await waitFor('both tabs render SENT', () =>
    A.item(fromB.clientId)?.state === 'sent' && B.item(fromB.clientId)?.state === 'sent')

  // The holder drops mid-drain; the other takes over and resends.
  const dropped = await composeText(A, 'in flight when A dropped')
  await ticks()
  assert.equal(tA.framesFor(dropped.clientId).length, 1)
  tA.close()
  await ticks()
  assert.ok(!A.outbox.isHolder, 'A releases when its session stops being ready')
  await waitFor('B takes over', () => B.outbox.isHolder)
  await waitFor('B resends under the same clientId', () => tB.framesFor(dropped.clientId).length === 1)

  // Counted once per refusal received, not once per tab.
  tB.refuse(dropped.clientId, { code: 'internal', message: 'db down', retryable: true })
  await ticks()
  const record = (await B.store.list()).find((e) => e.clientId === dropped.clientId)!
  assert.equal(record.internalRetries, 1, 'one refusal, one count')
  assert.equal(record.attempts, 2, 'two frames, two attempts')

  tB.close()
  await ticks()
  assert.equal(lock.held, false, 'released when the last ready session ends')
  assert.deepEqual(violations, [], 'no frame was written by a context without the lock')
  A.close()
  B.close()
}

/* ── criterion 16 · a refused persist() is surfaced in the tail ──────────── */

const NOTICE = 'Unsent messages are kept in this browser, which may clear them if storage runs low.'

async function c16(_f: Faults) {
  const page = () => renderToString(createSSRApp(App))
  const cases: { name: string; storage: PersistApi | null; compose: boolean; shown: boolean }[] = [
    { name: 'persist() resolves false', storage: { persist: async () => false }, compose: true, shown: true },
    { name: 'no storage API', storage: null, compose: true, shown: true },
    { name: 'persistence granted', storage: { persist: async () => true }, compose: true, shown: false },
    { name: 'no unsent entry', storage: { persist: async () => false }, compose: false, shown: false },
  ]
  select(CONV)
  for (const c of cases) {
    await useOutbox({ store: new MemoryOutboxStore(), transport: new ScriptedTransport(), storage: c.storage })
    if (c.compose) {
      state.composer.draft = `unsent: ${c.name}`
      await send()
    }
    await ticks()
    const html = await page()
    assert.equal(html.includes(NOTICE), c.shown, `${c.name}: the standing line is ${c.shown ? 'shown' : 'absent'}`)
    if (c.shown) {
      const tail = html.slice(html.indexOf(`unsent: ${c.name}`))
      assert.ok(tail.includes('class="persist-notice"'), 'in the thread, after the tail')
    }
  }
}

/* ── the runner ──────────────────────────────────────────────────────────── */

const criteria: Record<number, (f: Faults) => Promise<void>> = {
  0: c0, 1: c1, 2: c2, 3: c3, 4: c4, 5: c5, 6: c6, 7: c7, 8: c8, 9: c9, 10: c10, 11: c11, 12: c12, 13: c13, 14: c14, 15: c15, 16: c16,
}

/** The plan's rollout table, row for row. Ruling 3 picked C, so
 *  `misjudgeRetryBudget` runs (it is moot only under B). The six rows on 13
 *  and 14 are CANT-162's plan's, in its order; `remintOnReupload` sits on 14
 *  rather than 1 so the Dart suite's skip covers it until CANT-212. */
const FAULTS: [keyof Faults, number][] = [
  ['sendBeforePersist', 0],
  ['neverCompletes', 0],
  ['relaxedDurability', 0],
  ['remintOnRetry', 1],
  ['persistSending', 3],
  ['settlePendingOnly', 4],
  ['misclassifyRetryable', 7],
  ['misjudgeRetryBudget', 7],
  ['retryNonRetryable', 7],
  ['resendAfterBare1008', 8],
  ['resendFailedWithoutRetry', 9],
  ['deleteOnTerminal', 10],
  ['crossAccountLeak', 11],
  ['orderOutsideTxn', 12],
  ['everyTabDrains', 15],
  ['lockWithoutReady', 15],
  ['uploadBlocksDrain', 13],
  ['sendBeforeLastUpload', 13],
  ['staleHandleFails', 14],
  ['reuploadUnbounded', 14],
  ['refusalLoops', 14],
  ['remintOnReupload', 14],
]

for (const [n, run] of Object.entries(criteria)) {
  test(`criterion ${n} holds`, () => run({}))
}

for (const [fault, n] of FAULTS) {
  test(`fault ${fault} makes criterion ${n} fail`, async (t) => {
    await assert.rejects(
      criteria[n]({ [fault]: true }),
      (e: Error) => {
        // Which assertion caught it, so a fault failing for the wrong reason
        // is visible in the run's output.
        t.diagnostic(e.message ? e.message.split('\n')[0] : e.name)
        return true
      },
      `criterion ${n} still passed with ${fault} on`,
    )
  })
}

/* ── CANT-162 · the plan's other criteria, as plain tests ────────────────── */

test('CANT-162 · the refusing default, through the shipped wiring: RECORD then SEND fails with RefusingUploader.message, with RETRY and DELETE', async () => {
  const clock = new FakeClock()
  const store = new MemoryOutboxStore()
  const transport = new ScriptedTransport()
  // No `uploader`: what the shipped app builds.
  await useOutbox({ store, transport, storage: null, clock })
  select(CONV)
  startRecording()
  await stopRecording(true)
  transport.open()
  await ticks()

  const outbox = await outboxReady
  let items = outbox.view().items
  assert.equal(items.length, 1, 'exactly one entry')
  assert.equal(items[0].state, 'failed', 'failed — not QUEUED for good')
  assert.equal(RefusingUploader.message, "Attachments can't be sent yet")
  assert.equal(project(items[0]).error, RefusingUploader.message, 'with the inline message')
  assert.equal(transport.frames.length, 0, 'no frame')
  await clock.advance(BACKOFF_CAP_MS)
  assert.equal(transport.frames.length, 0, 'and none after the backoff cap')

  const id = items[0].entry.clientId
  await retry(id)
  await ticks()
  items = outbox.view().items
  assert.equal(items[0]?.state, 'failed', 'RETRY ends failed again')
  assert.equal(project(items[0]).error, RefusingUploader.message, 'with the same message')
  assert.equal(transport.frames.length, 0, 'and still no frame')

  await discard(id)
  assert.equal((await store.list()).length, 0, 'DELETE empties the store')
  assert.equal(outbox.view().items.length, 0)
})

test('CANT-162 · RETRY after an upload failure uploads afresh, under the same clientId, and reuploads stays 1 (§10 item 4)', async () => {
  const transport = new ScriptedTransport()
  const uploader = minting()
  const ctx = await context({ transport, uploader })
  transport.open()
  await ticks()

  /** An entry failed by a second `upload_not_found`. */
  const failedTwice = async (seed: number) => {
    const e = await composeVoice(ctx, recording(seed))
    await ticks()
    for (let i = 0; i < 2; i++) {
      transport.refuse(e.clientId, UPLOAD_NOT_FOUND)
      await ticks()
    }
    const held = (await stored(ctx.store, e.clientId))!
    assert.equal(held.status, 'failed')
    assert.equal(held.reuploads, 1)
    assert.equal(transport.framesFor(e.clientId).length, 2)
    return e
  }
  const acked = await failedTwice(1)
  const refused = await failedTwice(2)
  const calls = uploader.calls.length

  // RETRY: no handle on any attachment, offered exactly once more, one frame
  // under the same clientId with the new handle — and SENT on the ack.
  await ctx.outbox.retry(acked.clientId)
  await ticks()
  assert.equal(uploader.calls.length, calls + 1, 'offered to the uploader exactly once more')
  assert.deepEqual(handlesOf(uploader.calls.at(-1)!.entry), [['pending', undefined]], 'with no uploadId on any attachment')
  const frames = transport.framesFor(acked.clientId)
  assert.equal(frames.length, 3, 'one frame more')
  assert.equal(frames[2].clientId, acked.clientId, 'under the same clientId')
  assert.deepEqual(frames[2].attachments, [{ kind: 'voice', uploadId: `up-${calls + 1}` }], 'with the new handle')
  transport.ack(acked.clientId)
  await ticks()
  assert.equal(ctx.item(acked.clientId)?.state, 'sent', 'acked, it renders SENT')

  // Refused upload_not_found again after a RETRY: failed, with no further
  // upload call, and reuploads still 1 — the automatic re-upload happens once
  // in an entry's life.
  await ctx.outbox.retry(refused.clientId)
  await ticks()
  assert.equal(uploader.calls.length, calls + 2)
  assert.equal(transport.framesFor(refused.clientId).length, 3)
  transport.refuse(refused.clientId, UPLOAD_NOT_FOUND)
  await ticks()
  const again = (await stored(ctx.store, refused.clientId))!
  assert.equal(again.status, 'failed', 'failed again')
  assert.equal(uploader.calls.length, calls + 2, 'with no further upload call')
  assert.equal(again.reuploads, 1, 'and reuploads is still 1')
  ctx.close()
})

const VERSION_LINE = /^const VERSION = (\d+)$/m

for (const durable of [true, false]) {
  const name = durable ? 'IdbOutboxStore, across a reload' : 'MemoryOutboxStore'
  test(`CANT-162 · the Blob is held with the entry — ${name}`, async () => {
    const deps = idb()
    const memory = new MemoryOutboxStore()
    const open = async (): Promise<OutboxStore> => (durable ? IdbOutboxStore.open(deps) : memory)
    const bytes = recording(6)
    const store = await open()
    const transport = new ScriptedTransport()
    const held = never()
    const ctx = await context({ store, transport, uploader: held })
    transport.open()
    await ticks()
    const e = await composeVoice(ctx, bytes)
    await ticks()
    assert.equal(held.calls.length, 1, 'the upload is in flight and will never answer')
    // The reload: the outbox is dropped, and a fresh store is opened on the
    // same factory.
    ctx.close()
    if (durable) store.close()
    const fresh = await open()
    const rows = await fresh.list()
    assert.equal(rows.length, 1)
    assert.equal(rows[0].clientId, e.clientId)
    assert.equal(rows[0].status, 'pending')
    assert.deepEqual(handlesOf(rows[0]), [['pending', undefined]], "listed pending — never 'uploading'")
    const blob = rows[0].attachments![0].blob
    assert.ok(blob instanceof Blob, 'a Blob came back')
    assert.equal(blob.type, 'audio/ogg', 'of the same type')
    assert.deepEqual(await bytesOf(blob), bytes, 'with identical bytes')
    assert.equal(rows[0].attachments![0].durationMs, 1700)
    const source = readFileSync(resolve(process.cwd(), 'src/outbox/idb-store.ts'), 'utf8')
    assert.equal(source.match(VERSION_LINE)?.[1], '1', 'VERSION is still 1: no schema work')

    // After settle or discard the store lists no entry for the clientId. A
    // rejecting uploader fails the reloaded entry on the first ready, which
    // is what makes DELETE available on it.
    const t2 = new ScriptedTransport()
    const ctx2 = await context({ store: fresh, transport: t2, uploader: rejecting() })
    t2.open()
    await ticks()
    assert.equal(ctx2.item(e.clientId)?.state, 'failed')
    assert.equal(await ctx2.outbox.discard(e.clientId), true)
    assert.equal(await stored(fresh, e.clientId), undefined, 'discarded: gone from the store')
    const settled = await composeVoice(ctx2, recording(8))
    await ticks()
    await ctx2.outbox.settle(settled.clientId)
    assert.equal(await stored(fresh, settled.clientId), undefined, 'settled: gone from the store')
    ctx2.close()
    fresh.close()
  })
}

test("CANT-162 · uploads are the drain lock holder's, on a ready session (ruling 1)", async () => {
  // Not ready: zero calls, the entry pending; on ready, one call.
  const t = new ScriptedTransport()
  const u = minting()
  const ctx = await context({ transport: t, uploader: u })
  const e = await composeVoice(ctx, recording(1))
  await ticks()
  await ctx.clock.advance(BACKOFF_CAP_MS)
  assert.equal(u.calls.length, 0, 'no upload is attempted with no ready session')
  assert.equal((await stored(ctx.store, e.clientId))?.status, 'pending', 'pending, not failed')
  assert.equal(ctx.item(e.clientId)?.state, 'queued')
  t.open()
  await ticks()
  assert.equal(u.calls.length, 1, 'called once on ready')
  assert.equal(t.framesFor(e.clientId).length, 1, 'and then sent')
  ctx.close()

  // Two contexts over one store, each with its own uploader: an entry
  // composed in the context that does not hold the lock is uploaded exactly
  // once, by the holder.
  const store = new MemoryOutboxStore()
  const hub = new InProcessChannelHub()
  const lock = new InProcessLockHub()
  const server = new ScriptedServer()
  const tA = new ScriptedTransport(server)
  const tB = new ScriptedTransport(server)
  const uA = minting()
  const uB = minting()
  const A = await context({ store, transport: tA, lock, channel: hub.channel(), uploader: uA })
  const B = await context({ store, transport: tB, lock, channel: hub.channel(), uploader: uB })
  tA.open()
  await ticks()
  tB.open()
  await ticks()
  assert.ok(A.outbox.isHolder && !B.outbox.isHolder)
  const fromB = await composeVoice(B, recording(2))
  await waitFor('the holder uploads it', () => uA.calls.length === 1)
  await waitFor('and sends it', () => tA.framesFor(fromB.clientId).length === 1)
  await ticks()
  assert.equal(uB.calls.length, 0, 'the context without the lock uploads nothing')
  assert.equal(uA.calls.length, 1, 'exactly once')
  assert.equal(tB.frames.length, 0)
  A.close()
  B.close()

  // An entry reloaded with a pending attachment is offered once on the first
  // ready; while that offer is unanswered, closing and reopening the session
  // any number of times makes no second call.
  const store2 = new MemoryOutboxStore()
  const first = await context({ store: store2, uploader: never() })
  const reloaded = await composeVoice(first, recording(3))
  first.close()
  const t2 = new ScriptedTransport()
  const held = never()
  const ctx2 = await context({ store: store2, transport: t2, uploader: held })
  assert.equal(ctx2.item(reloaded.clientId)?.state, 'queued')
  await ticks()
  assert.equal(held.calls.length, 0, 'nothing is offered before a session is ready')
  t2.open()
  await ticks()
  assert.equal(held.calls.length, 1, 'offered once on the first ready')
  for (let i = 0; i < 3; i++) {
    t2.close()
    await ticks()
    t2.open()
    await ticks()
  }
  await ctx2.clock.advance(BACKOFF_CAP_MS)
  assert.equal(held.calls.length, 1, 'and not again while the offer is unanswered')
  assert.equal((await stored(store2, reloaded.clientId))?.status, 'pending')
  assert.equal(t2.frames.length, 0)
  ctx2.close()
})

test('CANT-162 · compose refuses what the composer disables, and the seam has isTerminal() (ruling 2)', async () => {
  assert.equal(new NullTransport().isTerminal(), false, 'no session is not a terminal one')

  // Not ready, not terminal: a picked file is refused and nothing is written;
  // a recording and a text are each stored.
  const t = new ScriptedTransport()
  const store = new MemoryOutboxStore()
  const ctx = await context({ store, transport: t, uploader: never() })
  await assert.rejects(
    ctx.outbox.compose(imageDraft(recording(1))),
    (e: Error) => e instanceof ComposeRefused && e.message === "A file can't be attached while offline",
  )
  assert.equal((await store.list()).length, 0, 'the refusal wrote nothing')
  const voice = await composeVoice(ctx, recording(2))
  const text = await composeText(ctx, 'queued offline')
  assert.deepEqual((await store.list()).map((e) => e.clientId), [voice.clientId, text.clientId], 'a voice draft and a text are stored')
  ctx.close()

  // Terminal, with no `closed` event ever emitted: every attachment is
  // refused and nothing is written; a text is stored.
  const tt = new ScriptedTransport()
  tt.terminal = true
  const s2 = new MemoryOutboxStore()
  const ctx2 = await context({ store: s2, transport: tt, uploader: never() })
  for (const draft of [voiceDraft(recording(3)), imageDraft(recording(4))]) {
    await assert.rejects(ctx2.outbox.compose(draft), (e: Error) => e instanceof ComposeRefused && e.message === 'This device cannot send')
  }
  assert.equal((await s2.list()).length, 0, 'nothing stored')
  await composeText(ctx2, 'kept for a re-enrollment')
  assert.equal((await s2.list()).length, 1, 'a text draft is stored')
  ctx2.close()

  // The same after a terminal close; a close that is not terminal is the
  // offline rule again.
  const t3 = new ScriptedTransport()
  const s3 = new MemoryOutboxStore()
  const ctx3 = await context({ store: s3, transport: t3, uploader: never() })
  t3.open()
  await ticks()
  t3.close(false, true)
  await ticks()
  await assert.rejects(ctx3.outbox.compose(voiceDraft(recording(5))), (e: Error) => e.message === 'This device cannot send')
  t3.open()
  await ticks()
  t3.close()
  await ticks()
  await composeVoice(ctx3, recording(6))
  assert.equal((await s3.list()).length, 1)
  ctx3.close()
})

/* ── the shipped wiring in store.ts (PR #103's review) ───────────────────── */

test('the shipped wiring never seeds a durable store, so a reload after DELETE then compose still opens', async () => {
  const deps = idb()
  const first = await IdbOutboxStore.open(deps)
  await useOutbox({ store: first, transport: new ScriptedTransport(), storage: null })
  assert.equal((await first.list()).length, 0, 'no fixture in catenary-outbox')
  select(CONV)
  state.composer.draft = 'order 1, in a durable store'
  const id = (await send())!
  assert.equal((await stored(first, id))?.order, 1)
  // The reload.
  const again = await IdbOutboxStore.open(deps)
  await within('reconfigure over the same database', useOutbox({ store: again, transport: new ScriptedTransport(), storage: null }))
  state.composer.draft = 'and another'
  assert.ok(await send(), 'sending still works')
  assert.equal((await again.list()).length, 2)
})

test('a second send() while the first is still writing sends nothing', async () => {
  const store = new MemoryOutboxStore()
  await useOutbox({ store, transport: new ScriptedTransport(), storage: null })
  select(CONV)
  state.composer.draft = 'twice'
  const ids = await Promise.all([send(), send()])
  assert.equal(ids.filter(Boolean).length, 1, 'one clientId')
  assert.equal((await store.list()).filter((e) => e.text === 'twice').length, 1, 'one entry')
  assert.equal(state.composer.draft, '', 'the draft is taken')
})

test('a refused write puts the draft back', async () => {
  const store = new MemoryOutboxStore()
  store.add = async () => {
    throw new Error('quota exceeded')
  }
  await useOutbox({ store, transport: new ScriptedTransport(), storage: null })
  select(CONV)
  state.composer.draft = 'keep me'
  await assert.rejects(send())
  assert.equal(state.composer.draft, 'keep me')
})

test('criterion 17: npm run outbox is part of npm run smoke', () => {
  const pkg = JSON.parse(readFileSync(resolve(process.cwd(), 'package.json'), 'utf8')) as {
    scripts: Record<string, string>
  }
  assert.ok(pkg.scripts.smoke.includes('npm run outbox'), pkg.scripts.smoke)
  assert.ok(pkg.scripts.outbox.includes('node --test'), pkg.scripts.outbox)
})
