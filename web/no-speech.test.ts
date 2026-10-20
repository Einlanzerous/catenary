/* `npm run test:no-speech` — a `ready` transcript with no words in it
 * (CANT-236 ruling 0, A; CANT-277).
 *
 *   the strip        NO SPEECH under the player for '' and for whitespace,
 *                    with nothing to expand, copy or report
 *   the reply stub   `no speech` when the source is loaded and silent,
 *                    `no transcript` when it failed, `voice note` when the
 *                    source is not loaded; never an empty line (CANT-283)
 *   one label        the strip's word is the Flutter app's, read from
 *                    `app/lib/thread.dart` so neither can move alone
 *
 * It renders the two components and seeds no corpus: the smoke render's
 * `voice` helper turns an empty transcript into `pending`. node:test over a
 * vite-ssr bundle, as `account.test.ts` is, and chained into
 * `npm run test:transport` so `./verify.sh` runs it.
 */

import { beforeEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import ReplyStub from '@/components/ReplyStub.vue'
import VoiceNote from '@/components/VoiceNote.vue'
import { NO_SPEECH_LABEL, REPLY_NO_TRANSCRIPT_LABEL, REPLY_VOICE_NOTE_LABEL, state } from '@/store'
import type { Message, ReplyRef, Transcript, VoiceAttachment } from '@/wire/generated'

const uuid = (n: number): string => `00000000-0000-4000-8000-${n.toString(16).padStart(12, '0')}`
const NADIA = uuid(2)
const ROOM = uuid(10)
const NOTE = uuid(100)

const voice = (transcript: Transcript): VoiceAttachment => ({
  kind: 'voice', url: '/media/v', durationMs: 72000, peaks: [9, 50, 100], transcript,
})

const note = (transcript: Transcript): Message => ({
  id: NOTE, seq: 1, logSeq: 1, conversationId: ROOM, authorId: NADIA, at: '2026-10-04T12:00:00.000Z',
  attachments: [voice(transcript)], state: 'sent', readBy: 1,
})

/** A reply to the note as the server sends it: its preview is rendered from
 *  the transcript text, so for a silent note it is ''. */
const reply: ReplyRef = { messageId: NOTE, authorId: NADIA, kind: 'voice', preview: '', durationMs: 72000 }

const text = (html: string) => html.replace(/<!--[\s\S]*?-->/g, '').replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()

const strip = (transcript: Transcript) => {
  const message = note(transcript)
  return renderToString(createSSRApp(VoiceNote, { message, voice: voice(transcript) }))
}

const stub = (r: ReplyRef = reply) => renderToString(createSSRApp(ReplyStub, { reply: r }))

/** The stub's preview line alone, without the sender or the chip. */
const previewOf = (html: string) => {
  const m = /<span class="preview[^"]*"[^>]*>([\s\S]*?)<\/span>/.exec(html)
  assert.ok(m, 'the stub still draws a preview span')
  return m[1].replace(/<!--[\s\S]*?-->/g, '')
}

beforeEach(() => {
  state.users = { [NADIA]: { id: NADIA, name: 'Nadia Okonkwo', initials: 'NO' } }
  state.messages = []
  state.expandedTranscripts.clear()
})

for (const [name, empty] of [['the empty string', ''], ['only whitespace', ' \n']] as const) {
  test(`a ready transcript of ${name} draws NO SPEECH and nothing to open`, async () => {
    const page = text(await strip({ state: 'ready', text: empty }))
    assert.ok(page.includes('NO SPEECH'), page)
    for (const absent of ['EXPAND', '0 W', 'COPY', 'REPORT BAD TRANSCRIPT', 'COLLAPSE', 'NO TRANSCRIPT', 'TRANSCRIBING']) {
      assert.ok(!page.includes(absent), `${absent} in: ${page}`)
    }
  })

  test(`and still does when ${name} was left expanded`, async () => {
    // The expanded arm is the one that carries COPY and REPORT BAD TRANSCRIPT.
    state.expandedTranscripts.add(NOTE)
    const page = text(await strip({ state: 'ready', text: empty }))
    assert.ok(page.includes('NO SPEECH'), page)
    for (const absent of ['COPY', 'REPORT BAD TRANSCRIPT', 'COLLAPSE']) {
      assert.ok(!page.includes(absent), `${absent} in: ${page}`)
    }
  })
}

test('a ready transcript with words is a transcript, and a failed one keeps its own label', async () => {
  const ready = text(await strip({ state: 'ready', text: 'two words' }))
  assert.ok(ready.includes('EXPAND · 2 W'), ready)
  assert.ok(!ready.includes('NO SPEECH'), ready)

  const failed = text(await strip({ state: 'failed' }))
  assert.ok(failed.includes('NO TRANSCRIPT'), failed)
  assert.ok(!failed.includes('NO SPEECH'), failed)
})

test('the strip label is the Flutter app’s, character for character', async () => {
  // Read from the Dart source rather than typed twice here, so either moving
  // alone fails (CANT-236 criterion 4).
  const dart = readFileSync(resolve(process.cwd(), '../app/lib/thread.dart'), 'utf8')
  const m = /const noSpeechLabel = '([^']+)';/.exec(dart)
  assert.ok(m, 'app/lib/thread.dart still declares noSpeechLabel as one single-quoted string')
  assert.equal(NO_SPEECH_LABEL, m[1])
  assert.ok(text(await strip({ state: 'ready', text: '' })).includes(m[1]), 'and it is what the strip draws')
})

test('a reply to a loaded, silent voice note reads no speech', async () => {
  state.messages = [note({ state: 'ready', text: '' })]
  assert.equal(previewOf(await stub()), 'no speech')

  state.messages = [note({ state: 'ready', text: ' \n' })]
  assert.equal(previewOf(await stub()), 'no speech')
})

test('a reply whose source is not loaded reads voice note, whatever the empty preview is', async () => {
  for (const preview of ['', ' \n']) {
    const html = await stub({ ...reply, preview })
    assert.equal(previewOf(html), REPLY_VOICE_NOTE_LABEL)
    for (const claim of ['no speech', 'no transcript']) {
      assert.ok(!html.includes(claim), `${claim} in: ${html}`)
    }
  }
})

test('a reply to a loaded note with words is its text, and a failed one reads no transcript', async () => {
  state.messages = [note({ state: 'ready', text: 'two words' })]
  assert.equal(previewOf(await stub()), 'two words')

  state.messages = [note({ state: 'failed' })]
  assert.equal(previewOf(await stub()), REPLY_NO_TRANSCRIPT_LABEL)
})

test('a non-empty preview is drawn as sent, never replaced by a label', async () => {
  const sent = { ...reply, preview: 'a caption' }
  assert.equal(previewOf(await stub(sent)), 'a caption')
  state.messages = [note({ state: 'failed' })]
  assert.equal(previewOf(await stub(sent)), 'a caption')
})

test('no voice reply stub draws an empty preview, across the matrix', async () => {
  const rows: [string, Transcript | undefined, string | RegExp][] = [
    ['pending', { state: 'pending' }, 'transcript pending — no preview text yet'],
    ['ready with words', { state: 'ready', text: 'two words' }, 'two words'],
    ['ready silent', { state: 'ready', text: '' }, 'no speech'],
    ['failed', { state: 'failed' }, REPLY_NO_TRANSCRIPT_LABEL],
    ['unknown state', { state: 'queued' } as unknown as Transcript, REPLY_VOICE_NOTE_LABEL],
    ['not loaded', undefined, REPLY_VOICE_NOTE_LABEL],
  ]
  for (const [name, transcript, want] of rows) {
    state.messages = transcript ? [note(transcript)] : []
    const got = previewOf(await stub())
    assert.ok(got.trim() !== '', `${name} drew an empty preview`)
    assert.equal(got, want, name)
  }
})
