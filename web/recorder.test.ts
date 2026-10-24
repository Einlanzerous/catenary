/* `npm run test:recorder` — the recorder behind RECORD (CANT-315, under
 * CANT-247, ruling 0 → B).
 *
 *   the capture  MediaRecorder's output goes out as recorded, `durationMs` is
 *                the recorder's own clock, the microphone is released on every
 *                path, a denial or a missing microphone is told in words
 *   the gate     VOICE_SEND_ENABLED is off: REC is drawn disabled, a click
 *                records nothing and composes no entry
 *   the entry    with a recorder switched on, SEND composes one voice
 *                attachment of exactly the recorded bytes, with `durationMs`
 *                and no `peaks` field anywhere
 *
 * node:test over a vite-ssr bundle, chained into `npm run test:transport`. The
 * live half (a `kind=voice` upload returning a handle, and its row) is
 * `uploadroundtrip.ts`. */

import { afterEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'
import Composer from '@/components/Composer.vue'
import { MemoryOutboxStore, ScriptedTransport } from '@/outbox'
import {
  RECORDER_MESSAGES,
  RECORDER_MIME_TYPES,
  RecorderRefused,
  VOICE_SEND_ENABLED,
  browserRecorder,
  type Capture,
  type Recorder,
} from '@/recorder'
import { canRecord, configureRecorder, outboxReady, startRecording, state, stopRecording, useOutbox } from '@/store'

const ME = '00000000-0000-4000-8000-000000000001'
const CONV = '00000000-0000-4000-8000-000000000064'

/* ── a fake microphone ────────────────────────────────────────────────────── */

class FakeTrack {
  stopped = false
  stop() {
    this.stopped = true
  }
}

function fakeDevices(opts: { reject?: string } = {}) {
  const tracks = [new FakeTrack(), new FakeTrack()]
  return {
    tracks,
    getUserMedia: async () => {
      if (opts.reject) throw Object.assign(new Error('nope'), { name: opts.reject })
      return { getTracks: () => tracks } as unknown as MediaStream
    },
  }
}

function fakeMediaRecorder(supported: string[], chunks: Uint8Array[], mimeType = 'audio/webm;codecs=opus') {
  const made: { options: unknown; recorder: FakeRecorder }[] = []
  class FakeRecorder {
    static isTypeSupported = (t: string) => supported.includes(t)
    ondataavailable: ((e: { data: Blob }) => void) | null = null
    onstop: (() => void) | null = null
    onerror: (() => void) | null = null
    state: 'inactive' | 'recording' = 'inactive'
    mimeType = mimeType
    constructor(
      readonly stream: unknown,
      options?: unknown,
    ) {
      made.push({ options, recorder: this })
    }
    start() {
      this.state = 'recording'
    }
    stop() {
      this.state = 'inactive'
      for (const c of chunks) this.ondataavailable?.({ data: new Blob([c as BlobPart]) })
      this.onstop?.()
    }
  }
  return { Ctor: FakeRecorder as unknown as typeof MediaRecorder, made }
}

const bytesOf = async (b: Blob) => new Uint8Array(await b.arrayBuffer())

/* ── the capture ──────────────────────────────────────────────────────────── */

test('The capture · bytes go out as recorded, type from the recorder, duration from its own clock, microphone released', async () => {
  const devices = fakeDevices()
  const { Ctor, made } = fakeMediaRecorder([...RECORDER_MIME_TYPES], [Uint8Array.from([1, 2, 3]), Uint8Array.from([4, 5])])
  const times = [1_000, 4_250]
  const recorder = browserRecorder(true, { mediaDevices: devices, MediaRecorder: Ctor, now: () => times.shift()! })
  const capture = await recorder.start()
  assert.deepEqual(made[0].options, { mimeType: 'audio/webm;codecs=opus' }, 'webm/opus first where the browser can')
  const { blob, durationMs } = await capture.stop()
  assert.deepEqual([...(await bytesOf(blob))], [1, 2, 3, 4, 5], 'the chunks, joined, untouched')
  assert.equal(blob.type, 'audio/webm;codecs=opus')
  assert.equal(durationMs, 3_250, 'the recorder’s own clock, stop minus start')
  assert.ok(devices.tracks.every((t) => t.stopped), 'the microphone is released')
  assert.deepEqual(Object.keys({ blob, durationMs }).sort(), ['blob', 'durationMs'], 'and nothing else — no peaks')
})

test('The capture · a browser with only mp4 (Safari) records mp4', async () => {
  const { Ctor, made } = fakeMediaRecorder(['audio/mp4'], [Uint8Array.from([9])], 'audio/mp4')
  const capture = await browserRecorder(true, { mediaDevices: fakeDevices(), MediaRecorder: Ctor }).start()
  assert.deepEqual(made[0].options, { mimeType: 'audio/mp4' })
  assert.equal((await capture.stop()).blob.type, 'audio/mp4')
})

test('The capture · a take the browser already ended (every track gone) still settles with what was recorded', async () => {
  const devices = fakeDevices()
  const { Ctor, made } = fakeMediaRecorder([...RECORDER_MIME_TYPES], [])
  const capture = await browserRecorder(true, { mediaDevices: devices, MediaRecorder: Ctor }).start()
  const rec = made[0].recorder as unknown as { state: string; ondataavailable: (e: { data: Blob }) => void }
  rec.ondataavailable({ data: new Blob([Uint8Array.from([7, 7])]) })
  rec.state = 'inactive' // the UA stopped it; `stop()` would now be a no-op
  const { blob } = await capture.stop()
  assert.deepEqual([...(await bytesOf(blob))], [7, 7])
  assert.ok(devices.tracks.every((t) => t.stopped))
})

test('The capture · cancel throws the take away and releases the microphone', async () => {
  const devices = fakeDevices()
  const { Ctor } = fakeMediaRecorder([...RECORDER_MIME_TYPES], [Uint8Array.from([1])])
  const capture = await browserRecorder(true, { mediaDevices: devices, MediaRecorder: Ctor }).start()
  capture.cancel()
  assert.ok(devices.tracks.every((t) => t.stopped))
})

test('The capture · a denied microphone, no microphone and no MediaRecorder each say so in words', async () => {
  const { Ctor } = fakeMediaRecorder([], [])
  const refuse = (seams: Parameters<typeof browserRecorder>[1]) => browserRecorder(true, seams).start()
  await assert.rejects(refuse({ mediaDevices: fakeDevices({ reject: 'NotAllowedError' }), MediaRecorder: Ctor }), (e: Error) => e instanceof RecorderRefused && e.message === RECORDER_MESSAGES.denied)
  await assert.rejects(refuse({ mediaDevices: fakeDevices({ reject: 'NotFoundError' }), MediaRecorder: Ctor }), (e: Error) => e.message === RECORDER_MESSAGES.noMicrophone)
  await assert.rejects(refuse({ mediaDevices: undefined, MediaRecorder: Ctor }), (e: Error) => e.message === RECORDER_MESSAGES.unavailable)
  await assert.rejects(refuse({ mediaDevices: fakeDevices(), MediaRecorder: undefined }), (e: Error) => e.message === RECORDER_MESSAGES.unavailable)
  assert.equal(new Set(Object.values(RECORDER_MESSAGES)).size, 4, 'four different things to say')
})

/* ── the gate and the entry ───────────────────────────────────────────────── */

const reset = () => {
  state.me = ME
  state.activeId = CONV
  state.composer.recording = false
  state.composer.attachment = null
  state.composer.attachError = null
  state.connection = { state: 'live' }
  configureRecorder()
}
afterEach(reset)

async function outbox() {
  const transport = new ScriptedTransport()
  await useOutbox({ store: new MemoryOutboxStore(), transport, channel: null, storage: null })
  return { outbox: await outboxReady, transport }
}
const render = () => renderToString(createSSRApp(Composer, { conversationName: 'Kitchen Table' }))
const recordButton = (html: string) => html.match(/<button([^>]*)>\s*RECORD\s*<\/button>/)?.[1] ?? ''

function scripted(blobBytes: number[], durationMs: number, log: string[] = []): Recorder {
  return {
    enabled: true,
    start: async () => {
      log.push('start')
      const capture: Capture = {
        stop: async () => (log.push('stop'), { blob: new Blob([Uint8Array.from(blobBytes)], { type: 'audio/webm' }), durationMs }),
        cancel: () => void log.push('cancel'),
      }
      return capture
    },
  }
}

test('Voice (ruling 0 → B) · the gate is off: REC is drawn disabled and a click records nothing and composes no entry', async () => {
  reset()
  const { outbox: ob, transport } = await outbox()
  transport.open()
  assert.equal(VOICE_SEND_ENABLED, false, 'the one constant CANT-56 flips')
  assert.equal(canRecord.value, false)
  assert.ok(/\bdisabled\b/.test(recordButton(await render())), 'RECORD is drawn disabled')
  await startRecording()
  assert.equal(state.composer.recording, false, 'nothing is recording')
  await stopRecording(true)
  assert.equal(ob.view().items.length, 0, 'and no entry was composed')
})

test('Voice (ruling 0 → B) · with a recorder switched on: RECORD is enabled, offline too, and only a terminal client dims it', async () => {
  reset()
  await outbox()
  configureRecorder(scripted([1], 1))
  for (const c of ['connecting', 'reconnecting', 'resyncing', 'offline', 'live'] as const) {
    state.connection = { state: c }
    assert.ok(!/\bdisabled\b/.test(recordButton(await render())), `RECORD is enabled while ${c}`)
  }
  state.connection = { state: 'terminal' }
  assert.ok(/\bdisabled\b/.test(recordButton(await render())))
})

test('Voice (ruling 0 → B) · SEND composes one voice attachment of exactly the recorded bytes, durationMs and no peaks', async () => {
  reset()
  const { outbox: ob, transport } = await outbox()
  transport.open()
  const bytes = [0x1a, 0x45, 0xdf, 0xa3, 7, 8, 9, 10]
  configureRecorder(scripted(bytes, 4_321))
  await startRecording()
  assert.equal(state.composer.recording, true)
  await stopRecording(true)
  const entry = ob.view().items[0].entry
  assert.equal(entry.attachments?.length, 1)
  const a = entry.attachments![0]
  assert.equal(a.kind, 'voice')
  assert.deepEqual([...(await bytesOf(a.blob))], bytes, 'the bytes as recorded')
  assert.equal(a.durationMs, 4_321, 'the recorder’s duration, not the on-screen counter')
  assert.deepEqual(Object.keys(a).sort(), ['blob', 'durationMs', 'kind', 'upload'], 'a voice draft has no other field')
  assert.ok(!/peaks/i.test(JSON.stringify({ ...entry, attachments: [{ ...a, blob: undefined }] })), 'no peaks anywhere on the draft')
})

test('Voice (ruling 0 → B) · CANCEL composes nothing and ends the take', async () => {
  reset()
  const { outbox: ob } = await outbox()
  const log: string[] = []
  configureRecorder(scripted([1], 1, log))
  await startRecording()
  await stopRecording(false)
  assert.deepEqual(log, ['start', 'cancel'])
  assert.equal(state.composer.recording, false)
  assert.equal(ob.view().items.length, 0)
})

test('Voice (ruling 0 → B) · a denied microphone is an inline message and the composer returns to idle', async () => {
  reset()
  const { outbox: ob } = await outbox()
  configureRecorder({
    enabled: true,
    start: async () => {
      throw new RecorderRefused(RECORDER_MESSAGES.denied)
    },
  })
  await startRecording()
  assert.equal(state.composer.recording, false, 'idle, not a dead recording bar')
  assert.equal(state.composer.attachError, RECORDER_MESSAGES.denied)
  assert.ok((await render()).includes('Microphone access was blocked'), 'shown inline')
  assert.equal(ob.view().items.length, 0)
  // and the next take clears it
  configureRecorder(scripted([1], 1))
  await startRecording()
  assert.equal(state.composer.attachError, null)
  await stopRecording(false)
})

test('Voice (ruling 0 → B) · a second click while the permission prompt is open opens no second take', async () => {
  reset()
  await outbox()
  let starts = 0
  let release: () => void = () => undefined
  configureRecorder({
    enabled: true,
    start: () => {
      starts++
      return new Promise<Capture>((resolve) => {
        release = () => resolve({ stop: async () => ({ blob: new Blob(), durationMs: 1 }), cancel() {} })
      })
    },
  })
  const first = startRecording()
  await startRecording()
  assert.equal(starts, 1)
  release()
  await first
  await stopRecording(false)
})
