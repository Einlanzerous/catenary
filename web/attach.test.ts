/* `npm run test:attach` — the ATTACH picker (CANT-314, under CANT-247).
 *
 *   fixtures   the shared manifest (testdata/media, CANT-312): the pre-check
 *              accepts and refuses exactly what the server's ingest does, and
 *              its cap is the server's cap
 *   one image  the file input is not `multiple`, a second pick replaces the
 *              first, a refused pick leaves the held one alone
 *   offline    ATTACH is enabled exactly when the outbox's own predicate says
 *              a picked file would be taken, over every connection state and
 *              every state of the outbox; RECORD is not dimmed offline; a
 *              ComposeRefused that is thrown anyway shows inline and writes nothing
 *
 * node:test over a vite-ssr bundle, chained into `npm run test:transport`. The
 * live half (a HEIC the server's 415 refuses, an oversize body its 413 refuses)
 * is `uploadroundtrip.ts`. */

import { afterEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'
import Composer from '@/components/Composer.vue'
import { IMAGE_CAP_BYTES, precheckImage, sniffImage } from '@/attach'
import { ComposeRefused, MemoryOutboxStore, ScriptedTransport } from '@/outbox'
import { canAttachFile, outboxMessages, outboxReady, pickImage, removeAttachment, send, state, useOutbox } from '@/store'
import { UPLOAD_MESSAGES } from '@/upload'
import { configureRecorder } from '@/store'
import type { ConnectionState } from '@/client-types'
import { loadManifest, oversizeImage, readFixture } from './media-fixtures'

const ME = '00000000-0000-4000-8000-000000000001'
const CONV = '00000000-0000-4000-8000-000000000064'

const file = (name: string, bytes: Uint8Array, type = ''): File => new File([bytes as BlobPart], name, { type })

/* ── the shared fixtures ──────────────────────────────────────────────────── */

test('Shared fixtures · the pre-check cap is the manifest cap, which the Go test pins to the server default', () => {
  assert.equal(IMAGE_CAP_BYTES, loadManifest().imageCapBytes)
})

test('Shared fixtures · every fixture gets the server’s verdict, from its bytes', async () => {
  const m = loadManifest()
  assert.equal(m.files.length, 7)
  const accepted: Record<string, string> = {
    'photo-exif-gps.jpg': 'image/jpeg',
    'image.png': 'image/png',
    'image.gif': 'image/gif',
    'image.webp': 'image/webp',
  }
  for (const f of m.files) {
    // The browser's own `type` is deliberately wrong for every file: it is
    // never evidence, and neither is the name.
    const verdict = await precheckImage(file(f.name, readFixture(f.name), f.verdict === 'accept' ? 'application/octet-stream' : 'image/jpeg'))
    if (f.verdict === 'accept') {
      assert.deepEqual(verdict, { ok: true, type: accepted[f.name] }, f.name)
    } else {
      assert.deepEqual(verdict, { ok: false, reason: f.reason, message: UPLOAD_MESSAGES.unsupportedType }, f.name)
    }
  }
})

test('Shared fixtures · the generated oversize body is refused for its size, and one byte under is not', async () => {
  const m = loadManifest()
  const over = oversizeImage(m)
  assert.equal(over.length, IMAGE_CAP_BYTES + 1, 'one byte past the cap')
  assert.equal(sniffImage(over.subarray(0, 12)), 'image/jpeg', 'a valid JPEG header, so only the size can refuse it')
  assert.deepEqual(await precheckImage(file('big.jpg', over)), { ok: false, reason: 'size', message: UPLOAD_MESSAGES.tooLarge })
  const atCap = await precheckImage(file('cap.jpg', over.subarray(0, IMAGE_CAP_BYTES)))
  assert.equal(atCap.ok, true, 'exactly the cap passes the pre-check; the server decides the rest')
})

test('Shared fixtures · a RIFF/WEBP body with no VP8 chunk is refused, as the server’s sniffer refuses it', async () => {
  const riff = (chunk: string) => Uint8Array.from([...'RIFF'].map((c) => c.charCodeAt(0)).concat([0, 0, 0, 0], [...'WEBP' + chunk].map((c) => c.charCodeAt(0))))
  assert.equal((await precheckImage(file('a.webp', riff('VP8 ')))).ok, true)
  assert.equal((await precheckImage(file('b.webp', riff('VP8L')))).ok, true)
  assert.equal((await precheckImage(file('c.webp', riff('VP8X')))).ok, true)
  assert.equal((await precheckImage(file('d.webp', riff('ALPH')))).ok, false)
  assert.equal((await precheckImage(file('e.webp', Uint8Array.from([...'RIFF'].map((c) => c.charCodeAt(0)).concat([0, 0, 0, 0], [...'WEBP'].map((c) => c.charCodeAt(0))))))).ok, false, 'a truncated one')
})

test('Shared fixtures · a name or type that lies does not change the verdict', async () => {
  assert.equal((await precheckImage(file('notes.txt', readFixture('image.png'), 'text/plain'))).ok, true, 'a PNG named .txt is a PNG')
  const heic = await precheckImage(file('holiday.jpg', readFixture('image.heic'), 'image/jpeg'))
  assert.equal(heic.ok, false, 'a HEIC named .jpg is a HEIC')
  assert.equal((await precheckImage(file('empty.jpg', new Uint8Array()))).ok, false, 'an empty file is nothing')
})

/* ── one image per message ────────────────────────────────────────────────── */

const reset = async () => {
  state.me = ME
  state.activeId = CONV
  state.composer.draft = ''
  state.composer.replyToId = null
  state.composer.attachment = null
  state.composer.attachError = null
  state.connection = { state: 'live' }
}
afterEach(() => {
  configureRecorder()
  return reset()
})

const render = () => renderToString(createSSRApp(Composer, { conversationName: 'Kitchen Table' }))

test('One image per message · the file input is not multiple, and accepts only what the server takes', async () => {
  await reset()
  const html = await render()
  const input = html.match(/<input[^>]*type="file"[^>]*>/)?.[0]
  assert.ok(input, 'a file input is rendered behind ATTACH')
  assert.ok(!/multiple/.test(input), 'not multiple')
  assert.match(input, /accept="image\/jpeg,image\/png,image\/gif,image\/webp"/)
})

test('One image per message · a second pick replaces the first; a refused pick leaves the held one', async () => {
  await reset()
  await pickImage(file('a.png', readFixture('image.png')))
  await pickImage(file('b.jpg', readFixture('photo-exif-gps.jpg')))
  assert.equal(state.composer.attachment?.filename, 'b.jpg', 'the second replaced the first')
  await pickImage(file('c.heic', readFixture('image.heic')))
  assert.equal(state.composer.attachment?.filename, 'b.jpg', 'a refused pick does not drop it')
  assert.equal(state.composer.attachError, UPLOAD_MESSAGES.unsupportedType)
  removeAttachment()
  assert.equal(state.composer.attachment, null)
  assert.equal(state.composer.attachError, null)
  // The composer's state has one slot, not a list: it cannot hold two.
  assert.ok(!Array.isArray(state.composer.attachment))
})

test('One image per message · the chip names the held file and offers to remove it', async () => {
  await reset()
  await pickImage(file('harbour.jpg', readFixture('photo-exif-gps.jpg')))
  const html = await render()
  assert.match(html, /harbour\.jpg/)
  assert.match(html, /aria-label="Remove attachment"/)
})

/* ── the offline rule ─────────────────────────────────────────────────────── */

async function outboxIn(step: 'fresh' | 'ready' | 'closed' | 'terminal-close' | 'terminal-quiet') {
  const transport = new ScriptedTransport()
  await useOutbox({
    store: new MemoryOutboxStore(),
    transport,
    channel: null,
    storage: null,
  })
  const outbox = await outboxReady
  if (step !== 'fresh') transport.open()
  if (step === 'closed') transport.close()
  if (step === 'terminal-close') transport.close(false, true)
  if (step === 'terminal-quiet') transport.terminal = true
  return { outbox, transport }
}

const attachButton = (html: string) => html.match(/<button([^>]*)>\s*ATTACH\s*<\/button>/)?.[1] ?? ''
const recordButton = (html: string) => html.match(/<button([^>]*)>\s*RECORD\s*<\/button>/)?.[1] ?? ''

test('Offline rule · ATTACH is enabled exactly when the outbox’s predicate says so, in every connection state and every outbox state', async () => {
  const connections: ConnectionState[] = ['connecting', 'live', 'reconnecting', 'resyncing', 'offline', 'terminal']
  const steps = ['fresh', 'ready', 'closed', 'terminal-close', 'terminal-quiet'] as const
  const seen = new Set<string>()
  for (const step of steps) {
    const { outbox } = await outboxIn(step)
    for (const c of connections) {
      state.connection = { state: c }
      const predicate = outbox.canAttachFile()
      assert.equal(canAttachFile.value, predicate, `${step}/${c}: the store reads the outbox`)
      const button = attachButton(await render())
      assert.equal(/\bdisabled\b/.test(button), !predicate, `${step}/${c}: ATTACH is enabled iff the outbox would take a file`)
      seen.add(`${predicate}`)
    }
  }
  assert.deepEqual([...seen].sort(), ['false', 'true'], 'both outcomes were exercised')
})

test('Offline rule · the old second condition is gone: a live banner over a session that is not ready does not enable ATTACH, and a resyncing one over a ready session does', async () => {
  const notReady = await outboxIn('fresh')
  state.connection = { state: 'live' }
  assert.equal(notReady.outbox.canAttachFile(), false)
  assert.ok(/\bdisabled\b/.test(attachButton(await render())), 'live banner, outbox not ready: disabled')

  await outboxIn('ready')
  state.connection = { state: 'resyncing' }
  assert.ok(!/\bdisabled\b/.test(attachButton(await render())), 'resyncing banner, outbox ready: enabled')
})

test('Offline rule · RECORD stays allowed offline, and is dimmed only when terminal', async () => {
  await outboxIn('closed')
  // The voice gate is off in the shipped app until CANT-56 (CANT-315); the offline
  // rule is about the connection, so a recorder is switched on to isolate it.
  configureRecorder({ enabled: true, start: async () => ({ stop: async () => ({ blob: new Blob(), durationMs: 1 }), cancel() {} }) })
  for (const c of ['connecting', 'reconnecting', 'resyncing', 'offline'] as const) {
    state.connection = { state: c }
    assert.ok(!/\bdisabled\b/.test(recordButton(await render())), `RECORD is enabled while ${c}`)
  }
  state.connection = { state: 'terminal' }
  assert.ok(/\bdisabled\b/.test(recordButton(await render())), 'terminal cannot send anything')
})

test('Offline rule · a ComposeRefused thrown anyway is shown inline, writes nothing, and keeps the image and the text', async () => {
  const { outbox } = await outboxIn('closed')
  state.connection = { state: 'live' } // the banner lags: the connection dropped between click and compose
  await pickImage(file('a.png', readFixture('image.png')))
  state.composer.draft = 'look at this'
  const out = await send()
  assert.equal(out, undefined)
  assert.equal(state.composer.attachError, ComposeRefused.pickedFileOffline)
  assert.equal(state.composer.attachment?.filename, 'a.png', 'the image stays')
  assert.equal(state.composer.draft, 'look at this', 'and so does the text')
  assert.equal(outbox.view().items.length, 0, 'the outbox holds nothing')
  assert.equal(outboxMessages.value.length, 0)
  assert.ok((await render()).includes('A file can&#39;t be attached while offline'), 'and it is on screen, inline')
})

test('Offline rule · when the session is ready SEND composes the image with its bytes and filename, and clears the chip', async () => {
  const { outbox } = await outboxIn('ready')
  const bytes = readFixture('photo-exif-gps.jpg')
  await pickImage(file('photo.jpg', bytes))
  const id = await send()
  assert.ok(id)
  const entry = outbox.view().items.find((i) => i.entry.clientId === id)!.entry
  assert.equal(entry.attachments?.length, 1)
  assert.equal(entry.attachments?.[0].kind, 'image')
  assert.equal(entry.attachments?.[0].filename, 'photo.jpg')
  assert.equal(entry.attachments?.[0].blob.size, bytes.length)
  assert.equal(entry.text, undefined, 'an image alone needs no text')
  assert.equal(state.composer.attachment, null)
})
