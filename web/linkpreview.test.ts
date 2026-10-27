/* `npm run test:linkpreview` — link previews on the web (CANT-323, under
 * CANT-308).
 *
 *   detection   schema/vectors/link-detection.json, the one table the Dart client
 *               and the app assert too
 *   the call    200 card, 204 and every other failure alike, 404 as "off",
 *               429 with retry_after_sec, a 401 repaired once; no reason and
 *               no URL ever leaves the file
 *   composer    idle for 600 ms before asking, a changed URL drops the held
 *               card, a send never waits, a dismissed preview is not asked
 *               again, a 404 stops the asking for the session
 *   send        a ready card rides as {kind: link, uploadId}; a stale handle
 *               is dropped and the message resent under the same clientId
 *   the card    host from url and never from site_name, page text as text,
 *               the image on this origin only, no E2E or verified
 *
 * node:test over a vite-ssr bundle, chained into `npm run test:transport`. */

import { afterEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'
import Composer from '@/components/Composer.vue'
import MessageRow from '@/components/MessageRow.vue'
import {
  PREVIEW_IDLE_MS,
  createLinkPreviewClient,
  detectLink,
  hostOf,
  looksComplete,
  plainText,
  openableUrl,
  sameOrigin,
  type LinkPreviewClient,
  type PreviewResult,
} from '@/linkpreview'
import { MemoryOutboxStore, ScriptedTransport } from '@/outbox'
import { configureLinkPreviews, composerDraftChanged, dismissPreview, outboxReady, send, state, useOutbox } from '@/store'
import { heldCredential, type Credential, type CredentialSeam } from '@/transport/credential'
import { FakeClock, uuid } from '@/transport/test/harness'
import type { LinkAttachment } from '@/wire/generated'
import type { Message } from '@/client-types'

const ME = uuid(1)
const CONV = uuid(100)
const HANDLE = uuid(777)
const BASE = 'http://catenary.test'
const TOKEN = 'SECRET-access-token-that-must-never-be-shown-0001'
const pair: Credential = { userId: ME, deviceId: uuid(3), accessToken: TOKEN }

/* ── detection ────────────────────────────────────────────────────────────── */

// The one table (CANT-329): schema/vectors/link-detection.json, read by the Dart
// client's and the app's tests as well. Nothing here defines a case.
interface DetectionCase {
  name: string
  text: string
  url: string | null
  ask: boolean
}
const DETECTION: DetectionCase[] = JSON.parse(
  readFileSync(resolve(process.cwd(), '..', 'schema', 'vectors', 'link-detection.json'), 'utf8'),
).cases

test('Detection · the shared table: every case produces its URL, and says whether to ask', () => {
  assert.ok(DETECTION.length >= 40, 'the table was read')
  for (const c of DETECTION) {
    const got = detectLink(c.text)
    assert.equal(got, c.url, `${c.name}: ${JSON.stringify(c.text)}`)
    assert.equal(got !== null && looksComplete(got), c.ask, `${c.name}: ask`)
  }
})

test('Detection · the table can fail: a rule without the trailing trim disagrees with it', () => {
  const noTrim = (t: string) => /https?:\/\/\S+/.exec(t)?.[0] ?? null
  assert.ok(DETECTION.some((c) => noTrim(c.text) !== c.url))
})

test('Detection · a scheme with nothing after it is a URL still being typed', () => {
  assert.equal(detectLink('https://'), null)
  assert.equal(detectLink('https://.'), null)
})

test('A card shows the host of its url, opens only http(s), and draws only this origin’s image', () => {
  assert.equal(hostOf('https://www.example.com:8443/a?b#c'), 'example.com:8443')
  assert.equal(hostOf('https://WWW.Example.COM/'), 'example.com')
  assert.equal(hostOf('https://go.nebula.tv/x'), 'go.nebula.tv')
  assert.equal(hostOf('https://bücher.example/'), 'xn--bcher-kva.example')
  assert.equal(hostOf('not a url'), '')
  assert.equal(openableUrl('javascript:alert(1)'), null)
  assert.equal(openableUrl('https://example.com/a'), 'https://example.com/a')
  assert.equal(sameOrigin('/media/abc?x=1', BASE), true)
  assert.equal(sameOrigin(`${BASE}/media/abc`, BASE), true)
  assert.equal(sameOrigin('https://evil.test/p.png', BASE), false)
  assert.equal(sameOrigin('//evil.test/p.png', BASE), false)
})

/* ── the call ─────────────────────────────────────────────────────────────── */

const card = (over: Partial<LinkAttachment> = {}): LinkAttachment => ({
  kind: 'link',
  url: 'https://www.youtube.com/watch?v=abc',
  title: 'A video',
  description: 'About a thing',
  siteName: 'YouTube',
  imageUrl: '/media/k1?exp=1&sig=2',
  imageWidth: 640,
  imageHeight: 360,
  imagePlaceholder: 'LEHV6nWB2yk8pyo0adR*.7kCMdnj',
  ...over,
})
const okBody = (c: LinkAttachment = card()) =>
  JSON.stringify({
    upload_id: HANDLE,
    preview: {
      kind: 'link',
      url: c.url,
      title: c.title,
      description: c.description,
      site_name: c.siteName,
      image_url: c.imageUrl,
      image_width: c.imageWidth,
      image_height: c.imageHeight,
    },
  })

interface Call {
  url: string
  init: RequestInit
}
function scriptedFetch(answer: (n: number) => Response | Promise<Response>) {
  const calls: Call[] = []
  const fetchFn = ((input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init: init ?? {} })
    return Promise.resolve(answer(calls.length))
  }) as typeof globalThis.fetch
  return { fetchFn, calls }
}
const clientOver = (fetchFn: typeof globalThis.fetch, credential: CredentialSeam = heldCredential(pair)) =>
  createLinkPreviewClient({ baseUrl: BASE, credential, fetch: fetchFn, timers: new FakeClock() })
const ask = (c: LinkPreviewClient, url = 'https://example.com/p') => c.preview(url, new AbortController().signal)

test('The call · one POST /link-previews with the URL as JSON and the Bearer; a 200 is a card and a handle', async () => {
  const { fetchFn, calls } = scriptedFetch(() => new Response(okBody(), { status: 200 }))
  const r = await ask(clientOver(fetchFn), 'https://www.youtube.com/watch?v=abc')
  assert.equal(calls.length, 1)
  assert.equal(calls[0].url, `${BASE}/link-previews`)
  assert.equal(calls[0].init.method, 'POST')
  assert.equal((calls[0].init.headers as Record<string, string>).Authorization, `Bearer ${TOKEN}`)
  assert.deepEqual(JSON.parse(String(calls[0].init.body)), { url: 'https://www.youtube.com/watch?v=abc' })
  assert.equal(r.kind, 'card')
  if (r.kind === 'card') {
    assert.equal(r.uploadId, HANDLE)
    assert.equal(r.preview.title, 'A video')
    assert.equal(r.preview.siteName, 'YouTube')
  }
})

test('The call · every failure is the same "none": 204, 400, 5xx, a network error, a body that is not Catenary’s, a timeout', async () => {
  const answers: [string, () => Response | Promise<Response>][] = [
    ['204', () => new Response(null, { status: 204 })],
    ['400', () => new Response('{"error":"the url is malformed"}', { status: 400 })],
    ['502', () => new Response('bad gateway', { status: 502 })],
    ['200 garbage', () => new Response('<html>', { status: 200 })],
    ['200 without a preview', () => new Response('{"upload_id":"' + HANDLE + '"}', { status: 200 })],
    ['network', () => Promise.reject(new TypeError('fetch failed: http://10.0.0.5/admin'))],
  ]
  for (const [name, answer] of answers) {
    const { fetchFn } = scriptedFetch(answer)
    const r = await ask(clientOver(fetchFn), 'http://10.0.0.5/admin')
    assert.deepEqual(r, { kind: 'none' }, name)
    // No field carries a reason or a URL.
    assert.ok(!JSON.stringify(r).includes('10.0.0.5'), name)
  }
  const clock = new FakeClock()
  const hung = createLinkPreviewClient({
    baseUrl: BASE,
    credential: heldCredential(pair),
    fetch: (() => new Promise<Response>(() => undefined)) as typeof globalThis.fetch,
    timers: clock,
  })
  const p = ask(hung)
  await clock.advance(12_001)
  assert.deepEqual(await p, { kind: 'none' }, 'a hung request is none after the deadline')
})

test('The call · 404 is "previews are off" and 429 carries retry_after_sec (clamped), from the body or the header', async () => {
  let r = await ask(clientOver(scriptedFetch(() => new Response('404 page not found', { status: 404 })).fetchFn))
  assert.deepEqual(r, { kind: 'unavailable' })
  r = await ask(clientOver(scriptedFetch(() => new Response('{"type":"error","code":"rate_limited","retry_after_sec":7}', { status: 429 })).fetchFn))
  assert.deepEqual(r, { kind: 'limited', retryAfterSec: 7 })
  r = await ask(clientOver(scriptedFetch(() => new Response('{}', { status: 429, headers: { 'Retry-After': '12' } })).fetchFn))
  assert.deepEqual(r, { kind: 'limited', retryAfterSec: 12 })
  r = await ask(clientOver(scriptedFetch(() => new Response('{"retry_after_sec":99999}', { status: 429 })).fetchFn))
  assert.deepEqual(r, { kind: 'limited', retryAfterSec: 300 }, 'a server cannot park the composer for a day')
})

test('The call · Catenary’s own 401 is repaired once and retried; a proxy’s is just none, and nothing shows the token', async () => {
  let current: Credential = pair
  const cred: CredentialSeam = {
    ...heldCredential(() => current),
    onSyncUnauthorized: async () => {
      current = { ...pair, accessToken: 'second-token' }
      return true
    },
  }
  const { fetchFn, calls } = scriptedFetch((n) =>
    n === 1 ? new Response('{"code":"unauthorized"}', { status: 401 }) : new Response(okBody(), { status: 200 }),
  )
  const r = await ask(clientOver(fetchFn, cred))
  assert.equal(r.kind, 'card')
  assert.equal(calls.length, 2)
  assert.equal((calls[1].init.headers as Record<string, string>).Authorization, 'Bearer second-token')

  const proxy = scriptedFetch(() => new Response('denied', { status: 401 }))
  assert.deepEqual(await ask(clientOver(proxy.fetchFn)), { kind: 'none' })
  assert.equal(proxy.calls.length, 1, 'a 401 that is not Catenary’s is not repaired')

  const dead = scriptedFetch(() => new Response('{"code":"unauthorized"}', { status: 401 }))
  assert.deepEqual(await ask(clientOver(dead.fetchFn)), { kind: 'none' }, 'a refused repair shows nothing')
})

/* ── the composer ─────────────────────────────────────────────────────────── */

/** A preview client the test answers by hand. */
function manual() {
  const calls: { url: string; resolve(r: PreviewResult): void; signal: AbortSignal }[] = []
  const client: LinkPreviewClient = {
    preview: (url, signal) => new Promise<PreviewResult>((resolve) => calls.push({ url, resolve, signal })),
  }
  return { client, calls }
}
const READY: PreviewResult = { kind: 'card', uploadId: HANDLE, preview: card() }

async function setup(opts: { ready?: boolean } = {}) {
  const clock = new FakeClock()
  const transport = new ScriptedTransport()
  await useOutbox({ store: new MemoryOutboxStore(), transport, channel: null, storage: null })
  if (opts.ready !== false) transport.open()
  const m = manual()
  configureLinkPreviews(m.client, { timers: clock, idleMs: PREVIEW_IDLE_MS })
  state.me = ME
  state.activeId = CONV
  state.connection = { state: 'live' }
  state.composer.draft = ''
  state.composer.replyToId = null
  state.composer.attachment = null
  state.composer.attachError = null
  return { clock, transport, ...m }
}
const type = (text: string) => {
  state.composer.draft = text
  composerDraftChanged()
}
const flush = () => new Promise((r) => setTimeout(r, 0))
afterEach(() => configureLinkPreviews(null))

const renderComposer = () => renderToString(createSSRApp(Composer, { conversationName: 'Kitchen Table' }))

test('Composer · nothing is asked until the draft has been idle for 600 ms, and only for the last URL typed', async () => {
  const { clock, calls } = await setup()
  type('see https://youtu')
  await clock.advance(5_000)
  assert.equal(calls.length, 0, 'a half-typed host (no dot) is not asked about')
  type('see https://www.youtube.com/watch?v=abc')
  await clock.advance(599)
  assert.equal(calls.length, 0, 'not before 600 ms')
  type('see https://www.youtube.com/watch?v=abcd')
  await clock.advance(599)
  assert.equal(calls.length, 0, 'typing restarts the wait')
  await clock.advance(1)
  assert.equal(calls.length, 1)
  assert.equal(calls[0].url, 'https://www.youtube.com/watch?v=abcd')
  assert.equal(state.composer.preview.state, 'loading')
  calls[0].resolve(READY)
  await flush()
  assert.equal(state.composer.preview.state, 'ready')
  assert.equal(state.composer.preview.uploadId, HANDLE)
  // Typing on without changing the URL asks nothing more.
  type('see https://www.youtube.com/watch?v=abcd lol')
  await clock.advance(5_000)
  assert.equal(calls.length, 1)
  assert.equal(state.composer.preview.state, 'ready')
})

test('Composer · changing or removing the URL drops the held card, and a late answer never paints a stale one', async () => {
  const { clock, calls } = await setup()
  type('https://one.test/a')
  await clock.advance(600)
  calls[0].resolve(READY)
  await flush()
  assert.equal(state.composer.preview.state, 'ready')
  type('https://two.test/b')
  assert.equal(state.composer.preview.state, 'idle', 'the old card is gone at once')
  assert.equal(state.composer.preview.card, null)
  await clock.advance(600)
  assert.equal(calls.length, 2)
  type('no link now')
  assert.equal(calls[1].signal.aborted, true, 'the request for the removed URL is abandoned')
  calls[1].resolve(READY)
  await flush()
  assert.equal(state.composer.preview.state, 'idle', 'and its late answer paints nothing')
})

test('Composer · a refusal of any kind shows nothing: no chip, no reason, no URL', async () => {
  const { clock, calls } = await setup()
  type('https://10.0.0.5/admin')
  await clock.advance(600)
  calls[0].resolve({ kind: 'none' })
  await flush()
  assert.equal(state.composer.preview.state, 'none')
  const html = await renderComposer()
  assert.ok(!html.includes('link-chip'), 'no chip')
  assert.ok(!html.replace(/<textarea[\s\S]*?<\/textarea>/, '').includes('10.0.0.5'), 'the URL is not echoed anywhere but the box the person typed it in')
})

test('Composer · a 404 means previews are off for the session: the next URL is not asked about', async () => {
  const { clock, calls } = await setup()
  type('https://one.test/a')
  await clock.advance(600)
  calls[0].resolve({ kind: 'unavailable' })
  await flush()
  assert.equal(state.composer.preview.state, 'none')
  type('https://two.test/b')
  await clock.advance(600)
  assert.equal(calls.length, 1, 'no second request')
  assert.equal(state.composer.preview.state, 'none')
})

test('Composer · a 429 waits retry_after_sec and then asks once more', async () => {
  const { clock, calls } = await setup()
  type('https://one.test/a')
  await clock.advance(600)
  calls[0].resolve({ kind: 'limited', retryAfterSec: 7 })
  await flush()
  assert.equal(state.composer.preview.state, 'idle', 'nothing shown while waiting')
  await clock.advance(6_999)
  assert.equal(calls.length, 1)
  await clock.advance(1)
  assert.equal(calls.length, 2)
  calls[1].resolve(READY)
  await flush()
  assert.equal(state.composer.preview.state, 'ready')
})

test('Composer · dismiss is per message: it drops the card, is not asked again for this draft, and clears on send', async () => {
  const { clock, calls } = await setup()
  type('https://one.test/a')
  await clock.advance(600)
  calls[0].resolve(READY)
  await flush()
  dismissPreview()
  assert.equal(state.composer.preview.state, 'dismissed')
  type('https://one.test/a and more words')
  type('https://two.test/b')
  await clock.advance(5_000)
  assert.equal(calls.length, 1, 'not re-requested for this draft')
  assert.equal(state.composer.preview.state, 'dismissed')
    type('')
  assert.equal(state.composer.preview.state, 'idle', 'an emptied draft is a new draft')
})

test('Composer · a held card shows the host of the typed URL, not the page’s site_name, and the page’s words as text', async () => {
  const { clock, calls } = await setup()
  type('https://www.example.com/p')
  await clock.advance(600)
  calls[0].resolve({
    kind: 'card',
    uploadId: HANDLE,
    preview: card({ url: 'https://www.example.com/p', siteName: 'TOTALLY OFFICIAL', title: '<img src=x onerror=alert(1)>' }),
  })
  await flush()
  const html = await renderComposer()
  assert.match(html, /data-chip="link"/)
  assert.ok(html.includes('>example.com<'), 'the host, from the url, without www.')
  assert.ok(!html.includes('TOTALLY OFFICIAL'), 'never the site_name')
  assert.ok(!html.includes('<img src=x'), 'the title is text, not markup')
  assert.ok(html.includes('&lt;img src=x'), 'escaped')
  assert.match(html, /aria-label="Remove link preview"/)
  assert.ok(!/E2E|verified/i.test(html))
})

/* ── the send ─────────────────────────────────────────────────────────────── */

test('Send · with a preview still loading the message goes now with no attachment, and the late card is ignored', async () => {
  const { clock, transport, calls } = await setup()
  type('check https://www.youtube.com/watch?v=abc')
  await clock.advance(600)
  assert.equal(state.composer.preview.state, 'loading')
  const id = await send()
  assert.ok(id)
  const frames = transport.framesFor(id!)
  assert.equal(frames.length, 1, 'sent without waiting')
  assert.equal(frames[0].attachments, undefined, 'no attachment')
  assert.equal(frames[0].text, 'check https://www.youtube.com/watch?v=abc')
  assert.equal(calls[0].signal.aborted, true)
  calls[0].resolve(READY)
  await flush()
  assert.equal(state.composer.preview.state, 'idle', 'the late card paints nothing on the next draft')
})

test('Send · with no preview at all (idle, none, dismissed) the message carries no attachment', async () => {
  const { clock, transport, calls } = await setup()
  type('see https://one.test/a')
  await clock.advance(600)
  calls[0].resolve({ kind: 'none' })
  await flush()
  const none = await send()
  assert.equal(transport.framesFor(none!)[0].attachments, undefined)

  type('see https://two.test/b')
  dismissPreview()
  const dismissed = await send()
  assert.equal(transport.framesFor(dismissed!)[0].attachments, undefined)
  assert.equal(state.composer.preview.state, 'idle', 'and the next draft starts clean')
})

test('Send · a ready card rides as {kind: link, uploadId}, is shown on the pending row, and the composer is clean after', async () => {
  const { clock, transport, calls } = await setup()
  type('watch https://www.youtube.com/watch?v=abc')
  await clock.advance(600)
  calls[0].resolve(READY)
  await flush()
  const id = await send()
  const frames = transport.framesFor(id!)
  assert.deepEqual(frames[0].attachments, [{ kind: 'link', uploadId: HANDLE }])
  assert.equal(state.composer.preview.state, 'idle')
  assert.equal(state.composer.draft, '')
  const item = (await outboxReady).view().items.find((i) => i.entry.clientId === id)!
  assert.equal(item.entry.attachments?.[0].kind, 'link')
  assert.equal(item.entry.attachments?.[0].upload, 'uploaded', 'born with its handle: the uploader is never offered a card')
})

test('Send · a ready card is also taken offline (the session is not ready); it is just a handle', async () => {
  const { clock, transport, calls } = await setup({ ready: false })
  type('watch https://www.youtube.com/watch?v=abc')
  await clock.advance(600)
  calls[0].resolve(READY)
  await flush()
  const id = await send()
  assert.ok(id, 'composed, not refused')
  assert.equal(transport.frames.length, 0, 'queued')
  transport.open()
  await flush()
  assert.deepEqual(transport.framesFor(id!)[0].attachments, [{ kind: 'link', uploadId: HANDLE }])
})

test('Stale handle · upload_not_found drops the card and resends the same message, same clientId, once', async () => {
  const { clock, transport, calls } = await setup()
  type('watch https://www.youtube.com/watch?v=abc')
  await clock.advance(600)
  calls[0].resolve(READY)
  await flush()
  const id = (await send())!
  assert.equal(transport.framesFor(id).length, 1)
  transport.refuse(id, { code: 'upload_not_found', message: 'unknown upload', retryable: false })
  await flush()
  await flush()
  const frames = transport.framesFor(id)
  assert.equal(frames.length, 2, 'resent')
  assert.equal(frames[1].clientId, id, 'under the same clientId')
  assert.equal(frames[1].text, 'watch https://www.youtube.com/watch?v=abc', 'with the same text')
  assert.equal(frames[1].attachments, undefined, 'and no card')
  const item = (await outboxReady).view().items.find((i) => i.entry.clientId === id)!
  assert.notEqual(item.state, 'failed', 'never lost')
  assert.equal(item.entry.attachments, undefined)
  // Acked once: never duplicated.
  assert.equal((await outboxReady).view().items.filter((i) => i.entry.text?.includes('youtube')).length, 1)
})

/* ── the card in the thread ───────────────────────────────────────────────── */

const row = (c: LinkAttachment, text?: string): Message =>
  ({
    id: uuid(900),
    conversationId: CONV,
    authorId: ME,
    seq: 1,
    logSeq: 1,
    at: '2026-10-25T10:00:00.000Z',
    state: 'sent',
    attachments: [c],
    ...(text ? { text } : {}),
  }) as unknown as Message

async function renderRow(c: LinkAttachment, text?: string) {
  state.origin = BASE
  state.me = ME
  return renderToString(createSSRApp(MessageRow, { message: row(c, text), memberCount: 3 }))
}

test('Thread card · the host comes from url and never from site_name; page words are text; no E2E, no verified', async () => {
  const html = await renderRow(
    card({ url: 'https://www.example.com/p', siteName: 'Bank Of Somewhere', title: 'Hi <b>there</b>', description: '<script>x()</script>' }),
    'look',
  )
  assert.ok(html.includes('>example.com<'))
  assert.ok(!html.includes('Bank Of Somewhere'), 'site_name is never drawn')
  assert.ok(html.includes('Hi &lt;b&gt;there&lt;/b&gt;'), 'title as text')
  assert.ok(html.includes('&lt;script&gt;x()&lt;/script&gt;'), 'description as text')
  assert.ok(!/<b>there|<script>x/.test(html), 'no markup from the page')
  assert.ok(!/E2E|verified|secure|safe/i.test(html.replace(/https?:\/\/\S+/g, '')), 'no claim the server cannot keep')
  assert.match(html, /href="https:\/\/www\.example\.com\/p"/)
  assert.match(html, /rel="noopener noreferrer"/)
})

test('Thread card · the image request is same-origin and the thumbnail variant; an off-origin image is not drawn', async () => {
  const same = await renderRow(card())
  const src = same.match(/<img[^>]*class="pic[^"]*"[^>]*src="([^"]+)"/)?.[1] ?? same.match(/<img[^>]*src="([^"]+)"/)?.[1]
  assert.ok(src, 'an image is drawn')
  assert.equal(sameOrigin(src!.replace(/&amp;/g, '&'), BASE), true)
  assert.match(src!, /v=thumb/)
  const off = await renderRow(card({ imageUrl: 'https://tracker.test/p.png' }))
  assert.ok(!off.includes('tracker.test'), 'a third party is never contacted')
  assert.ok(!/<img[^>]*class="pic/.test(off))
})

test('Thread card · the box is final before the image: stored dimensions size the frame', async () => {
  const html = await renderRow(card({ imageWidth: 680, imageHeight: 340 }))
  assert.match(html, /aspect-ratio:\s*680 \/ 340/, 'the stored 2:1, as stored')
  assert.match(await renderRow(card({ imageWidth: 1748, imageHeight: 1187 })), /aspect-ratio:\s*1748 \/ 1187/, 'the canvas’s own example')
  assert.match(await renderRow(card({ imageWidth: 1000, imageHeight: 100 })), /aspect-ratio:\s*2 \/ 1/, 'a sliver clamps to 2:1')
  assert.match(await renderRow(card({ imageWidth: 300, imageHeight: 900 })), /aspect-ratio:\s*1 \/ 1/, 'a portrait caps at square')
})

test('Thread card · no stored dimensions or no stored blurhash: text-only, a ratio is never guessed', async () => {
  assert.ok(!/<img|aspect-ratio/.test(await renderRow(card({ imageWidth: undefined, imageHeight: undefined }))))
  assert.ok(!/<img|aspect-ratio/.test(await renderRow(card({ imagePlaceholder: undefined }))))
})

test('Thread card · own cards sit on surface.base, the whole card is one link, the full URL is its tooltip', async () => {
  const html = await renderRow(card())
  assert.match(html, /<a class="card own"/)
  assert.match(html, /title="https:\/\/www\.youtube\.com\/watch\?v=abc"/)
})

test('Thread card · a text-only card, a non-http url and a missing host degrade without inventing anything', async () => {
  const text = await renderRow(card({ imageUrl: undefined, imageWidth: undefined, imageHeight: undefined }))
  assert.ok(!/<img/.test(text))
  assert.ok(text.includes('A video'))
  const bad = await renderRow(card({ url: 'javascript:alert(1)' }))
  assert.ok(!bad.includes('href="javascript'), 'never an href')
  assert.ok(bad.includes('A video'), 'the page’s title is still a card')
})

test('Untrusted text · whitespace collapses, control and bidi-override characters are stripped', () => {
  assert.equal(plainText('  a\n\n b\t\tc  '), 'a b c')
  assert.equal(plainText('x\u202Eevil\u202C y\u0007z'), 'xevil y z')
  assert.equal(plainText(null), '')
})
