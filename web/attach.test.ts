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
import { canAttachFile, outboxMessages, outboxReady, attachImage, pasteIntoComposer, removeAttachment, send, state, useOutbox } from '@/store'
import { UPLOAD_MESSAGES } from '@/upload'
import { configureRecorder } from '@/store'
import type { ConnectionState } from '@/client-types'
import { loadManifest, loadMessages, oversizeImage, readFixture } from './media-fixtures'

const ME = '00000000-0000-4000-8000-000000000001'
const CONV = '00000000-0000-4000-8000-000000000064'

const file = (name: string, bytes: Uint8Array, type = ''): File => new File([bytes as BlobPart], name, { type })

/* ── the shared fixtures ──────────────────────────────────────────────────── */

test('Shared fixtures · the pre-check cap is the manifest cap, which the Go test pins to the server default', () => {
  assert.equal(IMAGE_CAP_BYTES, loadManifest().imageCapBytes)
})

test('Shared fixtures · the several-files message is the one the Dart client holds (messages.json)', () => {
  assert.equal(ONE_IMAGE_MESSAGE, loadMessages().oneImage)
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
  await outboxIn('ready')
  await attachImage([file('a.png', readFixture('image.png'))], 'chooser')
  await attachImage([file('b.jpg', readFixture('photo-exif-gps.jpg'))], 'chooser')
  assert.equal(state.composer.attachment?.filename, 'b.jpg', 'the second replaced the first')
  await attachImage([file('c.heic', readFixture('image.heic'))], 'chooser')
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
  await outboxIn('ready')
  await attachImage([file('harbour.jpg', readFixture('photo-exif-gps.jpg'))], 'chooser')
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
  const { outbox, transport } = await outboxIn('ready')
  state.connection = { state: 'live' } // the banner lags: the connection dropped between click and compose
  await attachImage([file('a.png', readFixture('image.png'))], 'chooser')
  transport.close()
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
  await attachImage([file('photo.jpg', bytes)], 'chooser')
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

/* ── the one gate (CANT-352): chooser, drop and paste are the same path ───── */

import { readdirSync, readFileSync } from 'node:fs'
import { ONE_IMAGE_MESSAGE } from '@/attach'
import type { AttachSource } from '@/store'
import * as storeModule from '@/store'

const SOURCES: AttachSource[] = ['chooser', 'drop', 'paste']
const noOutboxEntry = (outbox: { view(): { items: unknown[] } }) => assert.equal(outbox.view().items.length, 0, 'no outbox entry')

test('One path · the same fixture through chooser, drop and paste holds the same attachment', async () => {
  const held: unknown[] = []
  for (const source of SOURCES) {
    await reset()
    const { outbox } = await outboxIn('ready')
    await attachImage([file('same.png', readFixture('image.png'))], source)
    assert.equal(state.composer.attachError, null, source)
    held.push({ name: state.composer.attachment?.filename, size: state.composer.attachment?.blob.size })
    noOutboxEntry(outbox)
  }
  assert.deepEqual(held[1], held[0])
  assert.deepEqual(held[2], held[0])
  assert.deepEqual(held[0], { name: 'same.png', size: readFixture('image.png').length })
})

test('One path · a FileList-shaped argument is taken as well as an array', async () => {
  await reset()
  await outboxIn('ready')
  const one = file('l.png', readFixture('image.png'))
  const list = { 0: one, length: 1, item: (i: number) => (i === 0 ? one : null) } as unknown as FileList
  await attachImage(list, 'drop')
  assert.equal(state.composer.attachment?.filename, 'l.png')
})

test('Identical refusals · HEIC, SVG and over-cap say the same thing from every source, hold nothing and write no entry', async () => {
  const over = oversizeImage(loadManifest())
  const svg = new TextEncoder().encode('<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>')
  const cases: Array<[string, () => File, string]> = [
    ['HEIC', () => file('h.heic', readFixture('image.heic')), UPLOAD_MESSAGES.unsupportedType],
    ['SVG', () => file('s.svg', svg, 'image/svg+xml'), UPLOAD_MESSAGES.unsupportedType],
    ['over the cap', () => file('big.jpg', over), UPLOAD_MESSAGES.tooLarge],
  ]
  for (const [label, make, message] of cases) {
    for (const source of SOURCES) {
      await reset()
      const { outbox } = await outboxIn('ready')
      await attachImage([make()], source)
      assert.equal(state.composer.attachError, message, `${label}/${source}`)
      assert.equal(state.composer.attachment, null, `${label}/${source}: nothing held`)
      noOutboxEntry(outbox)
    }
  }
})

test('Identical refusals · several files refuse the whole drop from every source, and leave what was held', async () => {
  for (const source of SOURCES) {
    await reset()
    const { outbox } = await outboxIn('ready')
    await attachImage([file('keep.png', readFixture('image.png'))], source)
    await attachImage([file('a.png', readFixture('image.png')), file('b.jpg', readFixture('photo-exif-gps.jpg'))], source)
    assert.equal(state.composer.attachError, ONE_IMAGE_MESSAGE, source)
    assert.equal(state.composer.attachment?.filename, 'keep.png', `${source}: neither of the two was taken, and the held one stays`)
    noOutboxEntry(outbox)
  }
})

test('Offline rule · the gate is the outbox’s predicate over 6 connection states x 5 outbox states, from every source', async () => {
  const connections: ConnectionState[] = ['connecting', 'live', 'reconnecting', 'resyncing', 'offline', 'terminal']
  const steps = ['fresh', 'ready', 'closed', 'terminal-close', 'terminal-quiet'] as const
  const outcomes = new Set<string>()
  for (const step of steps) {
    for (const c of connections) {
      for (const source of SOURCES) {
        await reset()
        const { outbox } = await outboxIn(step)
        state.connection = { state: c }
        const predicate = outbox.canAttachFile()
        await attachImage([file('p.png', readFixture('image.png'))], source)
        const label = `${step}/${c}/${source}`
        if (predicate) {
          assert.equal(state.composer.attachment?.filename, 'p.png', label)
          assert.equal(state.composer.attachError, null, label)
        } else {
          assert.equal(state.composer.attachment, null, label)
          assert.equal(state.composer.attachError, ComposeRefused.pickedFileOffline, `${label}: the one word`)
        }
        outcomes.add(String(predicate))
        noOutboxEntry(outbox)
      }
    }
  }
  assert.deepEqual([...outcomes].sort(), ['false', 'true'], 'both outcomes exercised')
})

test('Offline rule · a component-local connection.state check would fail this: the banner and the outbox disagree both ways', async () => {
  await reset()
  await outboxIn('fresh')
  state.connection = { state: 'live' }
  await attachImage([file('a.png', readFixture('image.png'))], 'paste')
  assert.equal(state.composer.attachError, ComposeRefused.pickedFileOffline, 'live banner, outbox not ready: refused')
  await reset()
  await outboxIn('ready')
  state.connection = { state: 'resyncing' }
  await attachImage([file('a.png', readFixture('image.png'))], 'paste')
  assert.equal(state.composer.attachment?.filename, 'a.png', 'resyncing banner, outbox ready: taken')
})

test('One path · pickImage is not exported, and no component mentions it', () => {
  assert.equal('pickImage' in storeModule, false, 'pickImage is not exported')
  const dir = 'src/components'
  for (const f of readdirSync(dir).filter((n) => n.endsWith('.vue'))) {
    assert.ok(!readFileSync(`${dir}/${f}`, 'utf8').includes('pickImage'), `${f} does not call pickImage`)
  }
})

/* ── paste (CANT-353) ─────────────────────────────────────────────────────── */

const clip = (files: File[], text = '') => ({
  defaultPrevented: false,
  clipboardData: { files, getData: (f: string) => (f === 'text/plain' ? text : '') },
  preventDefault() {
    this.defaultPrevented = true
  },
})
/** The gate is async and the handler does not return it; let it settle. */
const settle = () => new Promise((r) => setTimeout(r, 20))
const png = (name = 'image.png') => file(name, readFixture('image.png'), 'image/png')

test('Paste · the textarea listens for paste through pasteIntoComposer, and nothing else is called', () => {
  const src = readFileSync('src/components/Composer.vue', 'utf8')
  assert.match(src, /<textarea[\s\S]*?@paste="pasteIntoComposer"[\s\S]*?\/>/)
  assert.ok(!src.includes('precheckImage'), 'the component never calls the pre-check')
})

test('Paste · an image holds the same attachment the chooser holds, cancels the default paste and writes no entry', async () => {
  await reset()
  const { outbox } = await outboxIn('ready')
  await attachImage([png('same.png')], 'chooser')
  const viaChooser = { name: state.composer.attachment?.filename, size: state.composer.attachment?.blob.size }
  await reset()
  const ev = clip([png('same.png')])
  pasteIntoComposer(ev)
  await settle()
  assert.equal(ev.defaultPrevented, true)
  assert.deepEqual({ name: state.composer.attachment?.filename, size: state.composer.attachment?.blob.size }, viaChooser)
  assert.equal(state.composer.attachError, null)
  noOutboxEntry(outbox)
})

test('Paste · a text-only paste is not intercepted and touches nothing', async () => {
  await reset()
  await outboxIn('ready')
  for (const text of ['hello', '']) {
    const ev = clip([], text)
    pasteIntoComposer(ev)
    await settle()
    assert.equal(ev.defaultPrevented, false)
    assert.equal(state.composer.attachment, null)
    assert.equal(state.composer.attachError, null)
  }
  const pdf = clip([file('a.pdf', new Uint8Array([37, 80, 68, 70]), 'application/pdf')])
  pasteIntoComposer(pdf)
  await settle()
  assert.equal(pdf.defaultPrevented, false, 'a non-image file is left to the default')
  assert.equal(state.composer.attachError, null)
})

test('Paste · a mixed clipboard holds the image and still lets the text paste (the default is not cancelled)', async () => {
  await reset()
  await outboxIn('ready')
  const ev = clip([png('shot.png')], 'caption from the app')
  pasteIntoComposer(ev)
  await settle()
  assert.equal(ev.defaultPrevented, false, 'text reaches the textarea')
  assert.equal(state.composer.attachment?.filename, 'shot.png', 'and the image is held')
})

test('Paste · refusals read exactly as the chooser’s: HEIC, SVG, over the cap, several images, offline', async () => {
  const over = oversizeImage(loadManifest())
  const svg = new TextEncoder().encode('<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>')
  const cases: Array<[string, File[], string]> = [
    ['HEIC', [file('h.heic', readFixture('image.heic'), 'image/heic')], UPLOAD_MESSAGES.unsupportedType],
    ['SVG', [file('s.svg', svg, 'image/svg+xml')], UPLOAD_MESSAGES.unsupportedType],
    ['over the cap', [file('big.jpg', over, 'image/jpeg')], UPLOAD_MESSAGES.tooLarge],
    ['several', [png('a.png'), png('b.png')], ONE_IMAGE_MESSAGE],
  ]
  for (const [label, files, message] of cases) {
    await reset()
    const { outbox } = await outboxIn('ready')
    pasteIntoComposer(clip(files))
    await settle()
    assert.equal(state.composer.attachError, message, label)
    assert.equal(state.composer.attachment, null, label)
    noOutboxEntry(outbox)
  }
  await reset()
  await outboxIn('fresh')
  pasteIntoComposer(clip([png()]))
  await settle()
  assert.equal(state.composer.attachError, ComposeRefused.pickedFileOffline, 'offline')
  assert.equal(state.composer.attachment, null)
})

test('Paste · while recording it does nothing at all', async () => {
  await reset()
  await outboxIn('ready')
  state.composer.recording = true
  try {
    const ev = clip([png()])
    pasteIntoComposer(ev)
    await settle()
    assert.equal(ev.defaultPrevented, false)
    assert.equal(state.composer.attachment, null)
    assert.equal(state.composer.attachError, null)
  } finally {
    state.composer.recording = false
  }
})

test('Paste · a second image while one is held replaces it, as the chooser does; a refused one leaves it', async () => {
  await reset()
  await outboxIn('ready')
  pasteIntoComposer(clip([png('first.png')]))
  await settle()
  pasteIntoComposer(clip([file('second.jpg', readFixture('photo-exif-gps.jpg'), 'image/jpeg')]))
  await settle()
  assert.equal(state.composer.attachment?.filename, 'second.jpg')
  pasteIntoComposer(clip([file('h.heic', readFixture('image.heic'), 'image/heic')]))
  await settle()
  assert.equal(state.composer.attachment?.filename, 'second.jpg', 'a refused paste leaves the held one')
  assert.equal(state.composer.attachError, UPLOAD_MESSAGES.unsupportedType)
})
