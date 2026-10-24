/* `npm run test:upload` — the real `Uploader` (CANT-313, under CANT-247).
 *
 * node:test over a vite-ssr bundle, as `account.test.ts` is, chained into
 * `npm run test:transport` so `./verify.sh` runs it with no database. The
 * round trip against a live server is `uploadroundtrip.ts`.
 *
 *   request    one POST, Bearer, the Blob as the raw body, the handle of a 201
 *   mapping    413 415 404 401 5xx, a network error and a non-Catenary body each
 *              say a different thing, and no message carries the token
 *   bound      one size-scaled total deadline over refresh and request, from a
 *              fake clock; through a real Outbox the entry reads failed with
 *              RETRY offered
 *   retry      RETRY on a two-attachment entry uploads both again, same clientId
 */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  MemoryOutboxStore,
  Outbox,
  ScriptedTransport,
  UploadRefused,
  project,
  type OutboundAttachmentDraft,
  type OutboxEntry,
  type Uploader,
} from '@/outbox'
import {
  UPLOAD_BASE_MS,
  UPLOAD_CEILING_MS,
  UPLOAD_FLOOR_BYTES_PER_SECOND,
  UPLOAD_MESSAGES,
  createUploader,
  uploadDeadlineMs,
} from '@/upload'
import { heldCredential, type Credential, type CredentialSeam } from '@/transport/credential'
import { FakeClock, flush, uuid } from '@/transport/test/harness'

const BASE = 'http://upload.test'
const TOKEN = 'SECRET-access-token-that-must-never-be-shown-0001'
const HANDLE = uuid(777)
const CONV = uuid(100)
const ME = uuid(1)
const pair: Credential = { userId: ME, deviceId: uuid(3), accessToken: TOKEN }

const CATENARY_401 = JSON.stringify({ code: 'unauthorized' })

interface Call {
  url: string
  init: RequestInit
}

/** A fetch that records every call and answers each with `answer`. A signal
 *  that aborts rejects the call, as a real fetch does. */
function scriptedFetch(answer: (n: number, call: Call) => Promise<Response> | Response) {
  const calls: Call[] = []
  const fetchFn = ((input: RequestInfo | URL, init?: RequestInit) => {
    const call = { url: String(input), init: init ?? {} }
    calls.push(call)
    const settled = Promise.resolve(answer(calls.length, call))
    return new Promise<Response>((resolve, reject) => {
      const abort = () => reject(new DOMException('aborted', 'AbortError'))
      if (init?.signal?.aborted) return abort()
      init?.signal?.addEventListener('abort', abort)
      settled.then(resolve, reject)
    })
  }) as typeof globalThis.fetch
  return { fetchFn, calls }
}

const created = () => new Response(JSON.stringify({ upload_id: HANDLE }), { status: 201 })
const status = (code: number, body = '') => new Response(body, { status: code })
const never = () => new Promise<Response>(() => undefined)

const image = (bytes = 100 * 1024, name: string | null = 'photo.jpg'): OutboundAttachmentDraft => ({
  kind: 'image',
  blob: new Blob([new Uint8Array(bytes).fill(7)], { type: 'image/jpeg' }),
  ...(name ? { filename: name } : {}),
  upload: 'pending',
})
const ENTRY = { clientId: uuid(50) } as OutboxEntry

function uploaderOver(
  fetchFn: typeof globalThis.fetch,
  opts: { credential?: CredentialSeam; clock?: FakeClock } = {},
) {
  const clock = opts.clock ?? new FakeClock()
  return {
    clock,
    uploader: createUploader({ baseUrl: BASE, credential: opts.credential ?? heldCredential(pair), fetch: fetchFn, timers: clock }),
  }
}

/* ── request ──────────────────────────────────────────────────────────────── */

test('Web uploader request · one POST /uploads?kind=, Bearer, the Blob raw, the handle of a 201', async () => {
  const { fetchFn, calls } = scriptedFetch(() => created())
  const { uploader } = uploaderOver(fetchFn)
  const att = image(2048, 'my photo.jpg')
  assert.equal(await uploader.upload(ENTRY, att), HANDLE)
  assert.equal(calls.length, 1, 'exactly one request')
  assert.equal(calls[0].url, `${BASE}/uploads?kind=image&filename=my%20photo.jpg`)
  assert.equal(calls[0].init.method, 'POST')
  assert.deepEqual(calls[0].init.headers, { Authorization: `Bearer ${TOKEN}` }, 'no Content-Type of its own, and no JSON')
  assert.equal(calls[0].init.body, att.blob, 'the Blob itself is the body')
})

test('Web uploader request · a voice attachment is kind=voice, and no filename is sent when there is none', async () => {
  const { fetchFn, calls } = scriptedFetch(() => created())
  const { uploader } = uploaderOver(fetchFn)
  const att = { ...image(64, null), kind: 'voice' as const }
  await uploader.upload(ENTRY, att)
  assert.equal(calls[0].url, `${BASE}/uploads?kind=voice`)
})

test('Web uploader request · a 201 with no valid upload_id rejects', async () => {
  for (const body of ['', 'not json', '[]', '{}', '{"upload_id":7}', '{"upload_id":"not-a-uuid"}']) {
    const { fetchFn } = scriptedFetch(() => status(201, body))
    const { uploader } = uploaderOver(fetchFn)
    await assert.rejects(uploader.upload(ENTRY, image(16)), (e: Error) => e.message === UPLOAD_MESSAGES.notCatenary, body)
  }
})

test("Web uploader request · Catenary's own 401 is repaired once and the request retried with the new pair", async () => {
  let current: Credential = pair
  let refreshed = 0
  const cred: CredentialSeam = {
    ...heldCredential(() => current),
    onSyncUnauthorized: async (presented) => {
      assert.equal(presented.accessToken, TOKEN, 'the pair that was refused is the one reported')
      refreshed++
      current = { ...pair, accessToken: 'NEW-access-token' }
      return true
    },
  }
  const { fetchFn, calls } = scriptedFetch((n) => (n === 1 ? status(401, CATENARY_401) : created()))
  const { uploader } = uploaderOver(fetchFn, { credential: cred })
  assert.equal(await uploader.upload(ENTRY, image(16)), HANDLE)
  assert.equal(refreshed, 1)
  assert.equal(calls.length, 2)
  assert.deepEqual(calls[1].init.headers, { Authorization: 'Bearer NEW-access-token' })
})

/* ── error mapping ────────────────────────────────────────────────────────── */

test('Error mapping · each cause says something different, and no message carries the access token', async () => {
  const causes: [string, () => Promise<Response> | Response, string][] = [
    ['413', () => status(413, '{"error":"x"}'), UPLOAD_MESSAGES.tooLarge],
    ['415', () => status(415, '{"error":"x"}'), UPLOAD_MESSAGES.unsupportedType],
    ['404', () => status(404, 'page not found'), UPLOAD_MESSAGES.unavailable],
    ['401 after the repair', () => status(401, CATENARY_401), UPLOAD_MESSAGES.signedOut],
    ['500', () => status(500, 'oops'), UPLOAD_MESSAGES.serverError],
    ['503', () => status(503, ''), UPLOAD_MESSAGES.serverError],
    ['a network error', () => Promise.reject(new TypeError(`fetch failed for ${BASE}/uploads with Bearer ${TOKEN}`)), UPLOAD_MESSAGES.didNotFinish],
    ['a non-Catenary 201', () => status(201, '<html>captive portal</html>'), UPLOAD_MESSAGES.notCatenary],
  ]
  const seen = new Map<string, string>()
  for (const [name, answer, want] of causes) {
    const { fetchFn } = scriptedFetch(answer)
    const { uploader } = uploaderOver(fetchFn)
    await assert.rejects(uploader.upload(ENTRY, image(16)), (e: Error) => {
      assert.ok(e instanceof UploadRefused, `${name}: a rejection the outbox shows`)
      assert.equal(e.message, want, name)
      assert.ok(!e.message.includes(TOKEN) && !String(e.stack).includes(TOKEN), `${name}: no token in the message`)
      return true
    })
    if (name !== '503') seen.set(name, want)
  }
  assert.equal(new Set(seen.values()).size, seen.size, 'the seven causes read as seven different messages')
  assert.match(UPLOAD_MESSAGES.unsupportedType, /JPEG, PNG, GIF or WebP/, 'the 415 names what the server takes')
})

test('Error mapping · a voice note that is refused is not called an image', async () => {
  for (const [code, want] of [[413, UPLOAD_MESSAGES.voiceTooLarge], [415, UPLOAD_MESSAGES.voiceUnsupportedType]] as const) {
    const { fetchFn } = scriptedFetch(() => status(code))
    const { uploader } = uploaderOver(fetchFn)
    await assert.rejects(uploader.upload(ENTRY, { ...image(16), kind: 'voice' }), (e: Error) => e.message === want, String(code))
  }
  assert.ok(!/image|JPEG/i.test(UPLOAD_MESSAGES.voiceTooLarge + UPLOAD_MESSAGES.voiceUnsupportedType))
})

test('Error mapping · the token never reaches a log line', async () => {
  const lines: string[] = []
  const realLog = console.log
  const realWarn = console.warn
  console.log = (...a: unknown[]) => void lines.push(a.join(' '))
  console.warn = (...a: unknown[]) => void lines.push(a.join(' '))
  try {
    for (const answer of [() => status(500), () => status(401, CATENARY_401), () => Promise.reject(new Error('boom'))]) {
      const { fetchFn } = scriptedFetch(answer)
      await uploaderOver(fetchFn).uploader.upload(ENTRY, image(16)).catch(() => undefined)
    }
  } finally {
    console.log = realLog
    console.warn = realWarn
  }
  assert.ok(!lines.some((l) => l.includes(TOKEN)), 'nothing was logged with the token in it')
})

/* ── the bound ────────────────────────────────────────────────────────────── */

test('Bound · the deadline is 30 s plus the size at 32 KiB/s, capped at 10 minutes, as one named rule', () => {
  assert.equal(UPLOAD_BASE_MS, 30_000)
  assert.equal(UPLOAD_FLOOR_BYTES_PER_SECOND, 32 * 1024)
  assert.equal(UPLOAD_CEILING_MS, 600_000)
  assert.equal(uploadDeadlineMs(0), 30_000)
  const small = uploadDeadlineMs(100 * 1024)
  assert.ok(small > 33_000 && small < 33_200, `about 33 s for 100 KB, was ${small}`)
  assert.equal(uploadDeadlineMs(16 * 1024 * 1024), 30_000 + 512_000, 'a full 16 MiB image still fits under the ceiling')
  assert.equal(uploadDeadlineMs(100 * 1024 * 1024), UPLOAD_CEILING_MS)
})

test('Bound · a server that never answers rejects at the size-scaled deadline, aborting the request', async () => {
  const { fetchFn, calls } = scriptedFetch(never)
  const { uploader, clock } = uploaderOver(fetchFn)
  const box: { v: unknown } = { v: 'pending' }
  void uploader.upload(ENTRY, image(100 * 1024)).then((v) => (box.v = v), (e: unknown) => (box.v = e))
  await clock.advance(33_000)
  assert.equal(box.v, 'pending', 'still waiting just before the deadline')
  await clock.advance(200)
  const got = box.v as unknown
  assert.ok(got instanceof UploadRefused && got.message === UPLOAD_MESSAGES.didNotFinish, 'rejected with the did-not-finish message')
  assert.equal((calls[0].init.signal as AbortSignal).aborted, true, 'and the request was aborted')
})

test('Bound · a credential refresh that never completes is inside the same deadline', async () => {
  const cred: CredentialSeam = { ...heldCredential(pair), refreshIfDue: never as unknown as () => Promise<void> }
  const { fetchFn, calls } = scriptedFetch(() => created())
  const { uploader, clock } = uploaderOver(fetchFn, { credential: cred })
  const box: { v: unknown } = { v: 'pending' }
  void uploader.upload(ENTRY, image(100 * 1024)).then((v) => (box.v = v), (e: unknown) => (box.v = e))
  await clock.advance(33_200)
  const got = box.v as unknown
  assert.ok(got instanceof UploadRefused && got.message === UPLOAD_MESSAGES.didNotFinish)
  assert.equal(calls.length, 0, 'no request was ever made')
})

test('Bound · a refresh that completes after the deadline makes no request', async () => {
  let release: () => void = () => undefined
  const cred: CredentialSeam = { ...heldCredential(pair), refreshIfDue: () => new Promise<void>((r) => (release = r)) }
  const { fetchFn, calls } = scriptedFetch(() => created())
  const { uploader, clock } = uploaderOver(fetchFn, { credential: cred })
  const settled = uploader.upload(ENTRY, image(16)).catch((e: unknown) => e)
  await clock.advance(31_000)
  assert.ok((await settled) instanceof UploadRefused)
  release()
  await flush()
  assert.equal(calls.length, 0, 'the late refresh did not go on to send the blob')
})

test('Bound · a large image gets the ceiling, not more', async () => {
  const { fetchFn } = scriptedFetch(never)
  const { uploader, clock } = uploaderOver(fetchFn)
  const big = image(20 * 1024 * 1024)
  const box: { v: unknown } = { v: 'pending' }
  void uploader.upload(ENTRY, big).then((v) => (box.v = v), (e: unknown) => (box.v = e))
  await clock.advance(UPLOAD_CEILING_MS - 1000)
  assert.equal(box.v, 'pending')
  await clock.advance(1500)
  assert.ok((box.v as unknown) instanceof UploadRefused)
})

test('Bound · a success clears the timer', async () => {
  const { fetchFn } = scriptedFetch(() => created())
  const { uploader, clock } = uploaderOver(fetchFn)
  await uploader.upload(ENTRY, image(16))
  assert.equal(clock.pending(), 0, 'no deadline is left armed')
})

/* ── through a real Outbox ────────────────────────────────────────────────── */

async function outboxOver(uploader: Uploader) {
  const transport = new ScriptedTransport()
  const outbox = await Outbox.open({
    store: new MemoryOutboxStore(),
    transport,
    accountId: ME,
    channel: null,
    storage: { persist: async () => true, persisted: async () => true },
    random: () => 0.5,
    uploader,
    onChange: () => undefined,
  })
  transport.open()
  return { outbox, transport }
}

test('Bound · through a real Outbox the entry reads failed with the did-not-finish message and is never left pending', async () => {
  const { fetchFn } = scriptedFetch(never)
  const { uploader, clock } = uploaderOver(fetchFn)
  const { outbox, transport } = await outboxOver(uploader)
  const e = await outbox.compose({ conversationId: CONV, attachments: [image(100 * 1024)] })
  await flush()
  assert.equal(outbox.view().items[0].entry.status, 'pending', 'while the upload is in flight')
  await clock.advance(34_000)
  await flush()
  const item = outbox.view().items.find((i) => i.entry.clientId === e.clientId)!
  assert.equal(item.entry.status, 'failed')
  assert.equal(item.state, 'failed')
  assert.equal(project(item).error, UPLOAD_MESSAGES.didNotFinish)
  assert.equal(transport.frames.length, 0, 'nothing was sent without its handle')
  outbox.close()
})

test('RETRY after an upload failure · a two-attachment entry uploads BOTH again under the same clientId', async () => {
  let n = 0
  const handles: string[] = []
  const { fetchFn, calls } = scriptedFetch((k) => {
    n = k
    // The first pass: the first image uploads, the second fails.
    if (k === 2) return status(500, 'oops')
    const id = uuid(900 + k)
    handles.push(id)
    return new Response(JSON.stringify({ upload_id: id }), { status: 201 })
  })
  const { uploader } = uploaderOver(fetchFn)
  const { outbox, transport } = await outboxOver(uploader)
  const e = await outbox.compose({ conversationId: CONV, attachments: [image(64, 'a.jpg'), image(64, 'b.jpg')] })
  await flush()
  assert.equal(n, 2, 'two requests in the first pass')
  let item = outbox.view().items.find((i) => i.entry.clientId === e.clientId)!
  assert.equal(item.state, 'failed')
  assert.equal(project(item).error, UPLOAD_MESSAGES.serverError)

  await outbox.retry(e.clientId)
  await flush()
  assert.equal(calls.length, 4, 'RETRY offered both attachments again, not only the one that failed')
  assert.deepEqual(
    calls.slice(2).map((c) => new URL(c.url).searchParams.get('filename')),
    ['a.jpg', 'b.jpg'],
    'in order',
  )
  const frames = transport.framesFor(e.clientId)
  assert.equal(frames.length, 1, 'one frame, under the entry’s own clientId')
  assert.equal(frames[0].clientId, e.clientId)
  assert.deepEqual(frames[0].attachments?.map((a) => a.uploadId), [handles[1], handles[2]], 'with the handles of the second pass')
  outbox.close()
})
