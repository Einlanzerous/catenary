/* Client state.
 *
 * Deliberately not Pinia — one reactive object and a handful of actions is the
 * whole surface, and the dependency list stays at `vue`.
 *
 * The shape here is the shape the sync protocol implies: messages are an
 * append-only log keyed by a per-conversation `seq`, sends carry an
 * idempotency key, and nothing is ever mutated in place except a message's
 * own delivery state. Swapping the mock transport for a WebSocket should not
 * require reshaping any of this.
 */

import { computed, reactive } from 'vue'
import type {
  Conversation,
  ConnectionState,
  Message,
  VoiceAttachment,
} from '@/types'
import { CONVERSATIONS, ME, MESSAGES, USERS } from '@/mock/fixtures'
import { countWords } from '@/lib/format'

export type View = 'thread' | 'search'
export type Theme = 'dark' | 'light'

interface Playback {
  messageId: string | null
  /** 0–1 through the clip. */
  progress: number
  rate: number
  playing: boolean
}

interface Composer {
  draft: string
  replyToId: string | null
  recording: boolean
  recordingSec: number
  /** Attachments cannot be queued safely, so ATTACH dims when offline. */
}

const state = reactive({
  me: ME,
  users: USERS,
  conversations: [...CONVERSATIONS] as Conversation[],
  messages: [...MESSAGES] as Message[],

  activeId: 'c-kitchen',
  view: 'thread' as View,
  theme: 'dark' as Theme,

  connection: {
    state: 'live' as ConnectionState,
    attempt: 0,
    retryInSec: 0,
    synced: 0,
    total: 0,
    roomsPending: 0,
  },

  composer: {
    draft: '',
    replyToId: null,
    recording: false,
    recordingSec: 0,
  } as Composer,

  playback: {
    messageId: null,
    progress: 0,
    rate: 1,
    playing: false,
  } as Playback,

  /** Expansion is per-message and remembered per device, not synced. */
  expandedTranscripts: new Set<string>(),

  /** Conversations read during this visit. Clearing the badge and keeping the
   *  "N NEW" rule are two different things, so they are two different facts. */
  read: new Set<string>(),

  /** Who is typing, per conversation, in the order they started. */
  typing: { 'c-kitchen': ['u-nadia'] } as Record<string, string[]>,

  query: '',
  /** The message a search result or reply stub jumped to; drives the wash. */
  arrivedAt: null as string | null,

  nextSeq: 2000,
})

/* ── derived ───────────────────────────────────────────────────────────── */

const messagesFor = (conversationId: string) =>
  state.messages
    .filter((m) => m.conversationId === conversationId)
    .sort((a, b) => a.seq - b.seq)

export const activeConversation = computed(
  () => state.conversations.find((c) => c.id === state.activeId)!,
)

export const activeMessages = computed(() => messagesFor(state.activeId))

export const lastMessageOf = (conversationId: string): Message | undefined => {
  const list = messagesFor(conversationId)
  return list[list.length - 1]
}

/** Rooms above DMs, both in one column with the same row anatomy. */
export const rooms = computed(() => byRecency(state.conversations.filter((c) => c.kind === 'group')))
export const directs = computed(() => byRecency(state.conversations.filter((c) => c.kind === 'direct')))

function byRecency(list: Conversation[]): Conversation[] {
  return [...list].sort((a, b) => {
    const la = lastMessageOf(a.id)?.at ?? ''
    const lb = lastMessageOf(b.id)?.at ?? ''
    return lb.localeCompare(la)
  })
}

/**
 * How many messages in this conversation the reader has not seen.
 *
 * Own messages never count: you cannot have an unread message you sent. The
 * canvas makes this concrete — its thread shows "3 NEW" above a run of five
 * messages, three of them from other people.
 */
export function newCount(c: Conversation): number {
  if (c.firstUnreadSeq === undefined) return 0
  return messagesFor(c.id).filter(
    (m) => m.seq >= c.firstUnreadSeq! && m.authorId !== state.me,
  ).length
}

/** The rail badge: the same count, until the conversation has been opened. */
export function unreadCount(c: Conversation): number {
  return state.read.has(c.id) ? 0 : newCount(c)
}

export const totalUnread = computed(() =>
  state.conversations.reduce((n, c) => n + unreadCount(c), 0),
)

export const user = (id: string) => state.users[id]

export const isMine = (m: Message) => m.authorId === state.me

export const messageById = (id: string) => state.messages.find((m) => m.id === id)

export const voiceOf = (m: Message | undefined): VoiceAttachment | undefined =>
  m?.attachments?.find((a): a is VoiceAttachment => a.kind === 'voice')

/** Transcript word count, derived rather than stored — "EXPAND · 96 W" has to
 *  agree with the text actually on screen. */
export const transcriptWords = (v: VoiceAttachment): number =>
  v.transcript.text ? countWords(v.transcript.text) : 0

/* ── actions ───────────────────────────────────────────────────────────── */

export function select(id: string) {
  state.activeId = id
  state.view = 'thread'
  state.composer.replyToId = null
  // Reading clears the badge, but the "N NEW" rule stays put for this visit —
  // losing your place the instant you arrive is the bug.
  state.read.add(id)
}

export function setTheme(theme: Theme) {
  state.theme = theme
  document.documentElement.dataset.theme = theme
}

export function toggleTranscript(messageId: string) {
  if (state.expandedTranscripts.has(messageId)) {
    state.expandedTranscripts.delete(messageId)
  } else {
    state.expandedTranscripts.add(messageId)
  }
}

export function isExpanded(messageId: string) {
  return state.expandedTranscripts.has(messageId)
}

export function replyTo(messageId: string | null) {
  state.composer.replyToId = messageId
}

/* ── typing ────────────────────────────────────────────────────────────── */

export function typingIn(conversationId: string): string[] {
  return state.typing[conversationId] ?? []
}

/**
 * Deliberate call 10a's naming rule, and it is a rule rather than a string:
 * one person is a first name, two or three are comma-separated in the order
 * they started, four or more are "Several people" — because past three the
 * list churns faster than it can be read. It lives here rather than in the
 * component so the Flutter client can be held to the same three cases.
 */
export function typingLabel(conversationId: string): string | null {
  const ids = typingIn(conversationId)
  if (ids.length === 0) return null
  if (ids.length >= 4) return 'Several people'
  return ids.map((id) => user(id)?.name.split(' ')[0]).join(', ')
}

/** Harness only: the canvas ships three typing cards, so make all three
 *  reachable without a peer to type at you. */
export function cycleTyping() {
  const steps = [
    [],
    ['u-nadia'],
    ['u-nadia', 'u-ted'],
    ['u-nadia', 'u-ted', 'u-marek', 'u-rosa'],
  ]
  const now = typingIn(state.activeId).length
  const next = steps.find((s) => s.length > now) ?? steps[0]
  state.typing[state.activeId] = next
}

export function send() {
  const text = state.composer.draft.trim()
  if (!text) return

  const offline = state.connection.state !== 'live'
  const source = state.composer.replyToId
    ? messageById(state.composer.replyToId)
    : undefined

  const message: Message = {
    id: `m-local-${state.nextSeq}`,
    seq: state.nextSeq++,
    conversationId: state.activeId,
    authorId: state.me,
    at: new Date().toISOString(),
    text,
    state: offline ? 'queued' : 'sending',
    idempotencyKey: cryptoKey(),
    ...(source
      ? {
          replyTo: {
            messageId: source.id,
            authorId: source.authorId,
            ...previewOf(source),
          },
        }
      : {}),
  }

  state.messages.push(message)
  state.composer.draft = ''
  state.composer.replyToId = null

  if (!offline) advance(message)
}

/** Walks the mock message up the ladder. A real client advances on server acks. */
function advance(message: Message) {
  setTimeout(() => {
    if (message.state === 'sending') message.state = 'sent'
  }, 600)
  setTimeout(() => {
    if (message.state === 'sent') message.state = 'delivered'
  }, 1800)
}

export function retry(messageId: string) {
  const m = messageById(messageId)
  if (!m) return
  m.error = undefined
  m.state = 'sending'
  advance(m)
}

export function discard(messageId: string) {
  const i = state.messages.findIndex((m) => m.id === messageId)
  if (i >= 0) state.messages.splice(i, 1)
}

function previewOf(m: Message): {
  kind: 'text' | 'voice' | 'image' | 'link'
  preview: string
  durationSec?: number
  url?: string
} {
  const voice = voiceOf(m)
  if (voice) {
    return {
      kind: 'voice',
      preview: voice.transcript.text ?? '',
      durationSec: voice.durationSec,
    }
  }
  const image = m.attachments?.find((a) => a.kind === 'image')
  if (image && image.kind === 'image') {
    return { kind: 'image', preview: image.filename }
  }
  const url = m.text?.match(/\b[\w.-]+\.[a-z]{2,}\/\S*/i)?.[0]
  if (url) return { kind: 'link', preview: url, url: `https://${url}` }
  return { kind: 'text', preview: m.text ?? '' }
}

function cryptoKey(): string {
  return crypto.randomUUID()
}

/* ── connection ────────────────────────────────────────────────────────── */

let ticker: ReturnType<typeof setInterval> | null = null

export function setConnection(next: ConnectionState) {
  if (ticker) clearInterval(ticker)
  ticker = null

  const c = state.connection
  c.state = next

  if (next === 'live') {
    c.attempt = 0
    c.retryInSec = 0
    // Anything the outbox held goes out.
    for (const m of state.messages) {
      if (m.state === 'queued') {
        m.state = 'sending'
        advance(m)
      }
    }
    return
  }

  if (next === 'reconnecting') {
    // The banner counts: attempt number and a retry countdown, not a spinner.
    c.attempt = 3
    c.retryInSec = 8
    ticker = setInterval(() => {
      if (c.retryInSec > 0) {
        c.retryInSec--
      } else {
        c.attempt++
        c.retryInSec = 8
      }
    }, 1000)
    return
  }

  if (next === 'resyncing') {
    c.synced = 0
    c.total = 1180
    c.roomsPending = 4
    ticker = setInterval(() => {
      c.synced = Math.min(c.total, c.synced + 37)
      c.roomsPending = Math.max(0, 4 - Math.floor((c.synced / c.total) * 5))
      if (c.synced >= c.total) setConnection('live')
    }, 120)
  }
}

export function retryNow() {
  setConnection('resyncing')
}

/* ── recording ─────────────────────────────────────────────────────────── */

let recordTicker: ReturnType<typeof setInterval> | null = null

export function startRecording() {
  state.composer.recording = true
  state.composer.recordingSec = 0
  recordTicker = setInterval(() => state.composer.recordingSec++, 1000)
}

export function stopRecording(sendIt: boolean) {
  if (recordTicker) clearInterval(recordTicker)
  recordTicker = null
  state.composer.recording = false
  if (!sendIt) return
  // A real client uploads the Opus blob and the server schedules transcription;
  // here it lands as a pending-transcript voice note.
  state.messages.push({
    id: `m-local-${state.nextSeq}`,
    seq: state.nextSeq++,
    conversationId: state.activeId,
    authorId: state.me,
    at: new Date().toISOString(),
    state: state.connection.state === 'live' ? 'sending' : 'queued',
    attachments: [
      {
        kind: 'voice',
        durationSec: state.composer.recordingSec || 1,
        peaks: [],
        transcript: { state: 'pending', etaSec: 20 },
      },
    ],
  })
}

/* ── playback ──────────────────────────────────────────────────────────── */

let playTicker: ReturnType<typeof setInterval> | null = null

export function togglePlay(messageId: string) {
  const p = state.playback
  if (p.messageId === messageId && p.playing) {
    p.playing = false
    if (playTicker) clearInterval(playTicker)
    playTicker = null
    return
  }
  if (p.messageId !== messageId) {
    p.messageId = messageId
    p.progress = 0
  }
  p.playing = true
  if (playTicker) clearInterval(playTicker)
  playTicker = setInterval(() => {
    const voice = voiceOf(messageById(p.messageId ?? ''))
    if (!voice) return
    p.progress += (0.1 * p.rate) / voice.durationSec
    if (p.progress >= 1) {
      p.progress = 1
      p.playing = false
      if (playTicker) clearInterval(playTicker)
      playTicker = null
    }
  }, 100)
}

export function seek(messageId: string, fraction: number) {
  state.playback.messageId = messageId
  state.playback.progress = Math.min(1, Math.max(0, fraction))
}

export function cycleRate() {
  const rates = [1, 1.5, 2]
  const i = rates.indexOf(state.playback.rate)
  state.playback.rate = rates[(i + 1) % rates.length]
}

export function progressFor(messageId: string): number {
  return state.playback.messageId === messageId ? state.playback.progress : 0
}

/* ── search ────────────────────────────────────────────────────────────── */

export interface SearchHit {
  message: Message
  conversation: Conversation
  type: 'TXT' | 'VOX'
  snippet: string
  /** Voice hits seek the audio to the matched word, not just the message. */
  jumpToSec?: number
  /** A pending transcript still appears — labelled as not-yet-searchable. */
  notSearchableYet?: boolean
}

export const searchHits = computed<SearchHit[]>(() => {
  const q = state.query.trim().toLowerCase()
  if (!q) return []
  const hits: SearchHit[] = []

  for (const m of state.messages) {
    const conversation = state.conversations.find((c) => c.id === m.conversationId)
    if (!conversation) continue
    const voice = voiceOf(m)

    if (m.text && m.text.toLowerCase().includes(q)) {
      hits.push({ message: m, conversation, type: 'TXT', snippet: m.text })
      continue
    }

    if (voice) {
      const text = voice.transcript.text
      if (text && text.toLowerCase().includes(q)) {
        const segment = voice.transcript.segments?.find((s) =>
          s.text.toLowerCase().includes(q),
        )
        hits.push({
          message: m,
          conversation,
          type: 'VOX',
          snippet: ellipsize(text, q),
          jumpToSec: segment?.at,
        })
        continue
      }
      // Silently omitting these would make search feel like it lost things.
      if (
        voice.transcript.state === 'pending' &&
        (conversation.name.toLowerCase().includes(q) ||
          user(m.authorId).name.toLowerCase().includes(q))
      ) {
        hits.push({
          message: m,
          conversation,
          type: 'VOX',
          snippet: `matched on filename and sender only · ${voice.durationSec}`,
          notSearchableYet: true,
        })
      }
    }
  }

  return hits.sort((a, b) => b.message.at.localeCompare(a.message.at))
})

export const hitCounts = computed(() => {
  const all = searchHits.value
  return {
    all: all.length,
    text: all.filter((h) => h.type === 'TXT').length,
    voice: all.filter((h) => h.type === 'VOX').length,
    images: 0,
  }
})

/** Trims a long transcript to the neighbourhood of the match. */
function ellipsize(text: string, q: string, radius = 64): string {
  const i = text.toLowerCase().indexOf(q)
  if (i < 0) return text
  const start = Math.max(0, i - radius)
  const end = Math.min(text.length, i + q.length + radius)
  return (start > 0 ? '…' : '') + text.slice(start, end).trim() + (end < text.length ? '…' : '')
}

export function openSearch() {
  state.view = 'search'
}

export function closeSearch() {
  state.view = 'thread'
}

/** Jump to a message from search or a reply stub, and play the arrival wash. */
export function jumpTo(messageId: string, seekSec?: number) {
  const m = messageById(messageId)
  if (!m) return
  state.activeId = m.conversationId
  state.view = 'thread'
  state.arrivedAt = messageId

  const voice = voiceOf(m)
  if (voice && seekSec !== undefined) {
    seek(messageId, seekSec / voice.durationSec)
  }

  // The rule stays until the next scroll; the wash fades on its own.
  setTimeout(() => {
    if (state.arrivedAt === messageId) state.arrivedAt = null
  }, 4000)
}

export { state }
