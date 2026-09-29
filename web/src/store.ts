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
 *
 * WHAT YOU HAVE WRITTEN AND THE SERVER HAS NOT YET STORED IS NOT IN HERE.
 * `state.messages` holds server records only; an unacked send lives in the
 * outbox (`@/outbox`, CANT-36), in its own IndexedDB database, and is rendered
 * as a tail after the log rather than placed in it — it has no `seq` of its
 * own to be placed by. Nothing here moves a send to `sent` except an ack.
 */

import { computed, reactive, shallowRef } from 'vue'
import type { Conversation, ReplyRef, User, VoiceAttachment } from '@/wire/generated'
import {
  isOutboxMessage,
  type ConnectionInfo,
  type ConnectionState,
  type Message,
  type OutboxMessage,
  type RenderedMessage,
} from '@/client-types'
import { CONVERSATIONS, ME, MESSAGES, OUTBOX_SEED, USERS } from '@/mock/fixtures'
import { countWords } from '@/lib/format'
import {
  BroadcastOutboxChannel,
  IdbOutboxStore,
  InProcessLockHub,
  MemoryOutboxStore,
  NullTransport,
  Outbox,
  WebLockDrainLock,
  project,
  type OutboxOptions,
  type OutboxStore,
  type OutboxView,
} from '@/outbox'

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
    state: 'live',
    attempt: 0,
    retryInSec: 0,
    synced: 0,
    total: 0,
    roomsPending: 0,
    // The mock's own counters are always present; the real transport's
    // `connectionInfo` fills only what it knows (CANT-39 swaps it in).
  } as ConnectionInfo & Required<Pick<ConnectionInfo, 'attempt' | 'retryInSec' | 'synced' | 'total' | 'roomsPending'>>,

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

/* ── the outbox, as rendered ───────────────────────────────────────────── */

/** The outbox's last emitted view. Replaced wholesale on every change. */
const outboxView = shallowRef<OutboxView>({ items: [], persist: 'unknown' })

/** Every entry of the enrolled account, projected, in `order`. */
export const outboxMessages = computed<OutboxMessage[]>(() => outboxView.value.items.map(project))

/** Acked entries sit in the log at the SERVER's seq, in the ack's
 *  conversation, until their record replaces them. */
const ackedFor = (conversationId: string) =>
  outboxMessages.value.filter((m) => m.seq !== undefined && m.conversationId === conversationId)

/** The tail: unacked entries, after the log, in `order`. */
const tailFor = (conversationId: string) =>
  outboxMessages.value.filter((m) => m.seq === undefined && m.conversationId === conversationId)

/** The thread's log — server records plus acked entries, by seq. The unread
 *  rules never read this; they read `messagesFor`. */
export const activeLog = computed<RenderedMessage[]>(() =>
  [...activeMessages.value, ...ackedFor(state.activeId)].sort((a, b) => a.seq! - b.seq!),
)

export const activeTail = computed(() => tailFor(state.activeId))

/**
 * §9 of the decision record: while the browser has not granted persistence
 * and this thread holds an unsent entry, its tail says so — one standing
 * line, never a toast. A QUEUED row implies the message will go out, and the
 * client must not imply more durability than the browser has granted.
 */
export const persistNotice = computed(() =>
  outboxView.value.persist === 'refused' &&
  outboxMessages.value.some(
    (m) => m.conversationId === state.activeId && (m.state === 'queued' || m.state === 'sending' || m.state === 'failed'),
  )
    ? 'Unsent messages are kept in this browser, which may clear them if storage runs low.'
    : null,
)

/**
 * The rail's last item: the newer of the conversation's last server record
 * and its newest unsettled outbox entry, so a send moves the preview, the
 * marker and the room's position the moment it is composed — offline
 * included. An unacked entry is dated by `composedAt`, the device clock, for
 * this local ordering only; an acked one by `ack.at`.
 */
export const lastMessageOf = (conversationId: string): RenderedMessage | undefined => {
  const list = messagesFor(conversationId)
  const record: RenderedMessage | undefined = list[list.length - 1]
  const entries = outboxMessages.value.filter((m) => m.conversationId === conversationId)
  const entry = entries[entries.length - 1]
  if (!entry) return record
  if (!record) return entry
  return entry.at >= record.at ? entry : record
}

/** Rooms above DMs, both in one column with the same row anatomy. */
export const rooms = computed(() => byRecency(state.conversations.filter((c) => c.kind === 'group')))
export const directs = computed(() => byRecency(state.conversations.filter((c) => c.kind === 'direct')))

/**
 * The other party in a direct conversation, by identity rather than guess
 * (CANT-141). `Conversation.otherMemberId` is what the wire actually carries
 * for this — added because the two guesses this function used to make were
 * both wrong in reachable cases: a direct in which only `state.me` has ever
 * spoken has no foreign author to find, and a display-name match breaks on
 * two people sharing a name, on a bot named after a person, and on a rename
 * (a held `Conversation.name` is not re-emitted when the other member's own
 * name changes; their `User` record is). Neither guess is missed: this is a
 * lookup, not a fallback chain.
 */
export function otherMember(c: Conversation): User | undefined {
  if (c.kind !== 'direct' || !c.otherMemberId) return undefined
  return state.users[c.otherMemberId]
}

/**
 * What a direct is titled: the other member's LIVE name when it is known,
 * `Conversation.name` only as the fallback for the one case it is not yet
 * (a `User` record not yet held — the wire's own description of
 * `other_member_id` names this). For a group, `otherMember` returns
 * `undefined` and this is just `c.name`, unconditionally.
 *
 * ONE HELPER FOR EVERY SITE THAT NAMES A DIRECT, so "titled by the identity,
 * not the stale string" is one fact rather than five copies of the same
 * expression that could each be missed: `Thread.vue`'s header and the
 * placeholder it hands `Composer`, `ConversationRow.vue`'s rail label,
 * `SearchView.vue`'s hit label, and this file's own pending-voice search
 * fallback below.
 */
export function conversationTitle(c: Conversation): string {
  return otherMember(c)?.name ?? c.name
}

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

export const isMine = (m: RenderedMessage) => m.authorId === state.me

export const messageById = (id: string) => state.messages.find((m) => m.id === id)

export const voiceOf = (m: RenderedMessage | undefined): VoiceAttachment | undefined =>
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

/** CANT-31 §6: a terminal client drains nothing, so nothing is created as
 *  `queued` while it is one (Invariant 3; CANT-35 criterion 30). The draft
 *  stays in the box, which is the honest place for it. What the outbox
 *  already holds stays there too — a terminal state never deletes it. */
const cannotSend = () => state.connection.state === 'terminal'

/**
 * Compose into the outbox. The draft clears once the entry is durably
 * written — not before, so a refused write leaves the text where it was.
 * Resolves with the entry's clientId.
 */
export async function send(): Promise<string | undefined> {
  const text = state.composer.draft.trim()
  if (!text || cannotSend()) return undefined

  const source = state.composer.replyToId ? messageById(state.composer.replyToId) : undefined
  const replyPreview: ReplyRef | undefined = source
    ? { messageId: source.id, authorId: source.authorId, ...previewOf(source) }
    : undefined

  const outbox = await outboxReady
  const entry = await outbox.compose({
    conversationId: state.activeId,
    text,
    ...(source ? { replyToMessageId: source.id, replyPreview } : {}),
  })
  if (state.composer.draft.trim() === text) state.composer.draft = ''
  if (source && state.composer.replyToId === source.id) state.composer.replyToId = null
  return entry.clientId
}

/* NOTHING LOCALLY MOVES A SEND TO `sent` EXCEPT AN ACK, and nothing ever moves
 * your own message to `delivered`. CANT-90 settled that your own message is
 * `sent` until another member's receipt passes it and `read` after: D1
 * declined delivery receipts, so no response can carry that rung, and a
 * client inventing it is exactly the claim Invariant 3 forbids. The timer
 * that used to walk a send up the ladder is gone with the ladder. */

/** RETRY on a failed entry: back to pending, same clientId. */
export async function retry(clientId: string) {
  if (cannotSend()) return
  await (await outboxReady).retry(clientId)
}

/** DELETE on a failed entry — the local copy only. */
export async function discard(clientId: string) {
  await (await outboxReady).discard(clientId)
}

function previewOf(m: Message): {
  kind: 'text' | 'voice' | 'image' | 'link'
  preview: string
  durationMs?: number
  url?: string
} {
  const voice = voiceOf(m)
  if (voice) {
    return {
      kind: 'voice',
      preview: voice.transcript.text ?? '',
      durationMs: voice.durationMs,
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

/* ── connection ────────────────────────────────────────────────────────── */

let ticker: ReturnType<typeof setInterval> | null = null

export function setConnection(next: ConnectionState) {
  if (ticker) clearInterval(ticker)
  ticker = null

  const c = state.connection
  c.state = next

  if (next === 'live') {
    // What the outbox holds goes out on the TRANSPORT's ready, not on this
    // banner's: until CANT-163 wires one, the NullTransport never is, and
    // every entry stays honestly QUEUED whatever the dev toolbar says.
    c.attempt = 0
    c.retryInSec = 0
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

export async function stopRecording(sendIt: boolean) {
  if (recordTicker) clearInterval(recordTicker)
  recordTicker = null
  state.composer.recording = false
  if (!sendIt || cannotSend()) return
  // There is no recorder yet, so the clip is empty; the entry is real. It is
  // held rather than sent until an upload handle exists — the upload queue is
  // CANT-162's, gated on CANT-48.
  const outbox = await outboxReady
  await outbox.compose({
    conversationId: state.activeId,
    attachments: [
      {
        kind: 'voice',
        blob: new Blob([], { type: 'audio/ogg' }),
        durationMs: (state.composer.recordingSec || 1) * 1000,
        upload: 'pending',
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
    // 100ms of wall-clock time per tick, scaled by rate, over the clip's own
    // duration — both sides in milliseconds now, so the fraction is the same
    // ratio the seconds-based version computed.
    p.progress += (100 * p.rate) / voice.durationMs
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
  jumpToMs?: number
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
          jumpToMs: segment?.atMs,
        })
        continue
      }
      // Silently omitting these would make search feel like it lost things.
      // conversationTitle, not conversation.name: a direct is matched on who
      // it is actually with, not on a string that can go stale the moment
      // the wire carries an identity to prefer instead (CANT-141).
      if (
        voice.transcript.state === 'pending' &&
        (conversationTitle(conversation).toLowerCase().includes(q) ||
          user(m.authorId).name.toLowerCase().includes(q))
      ) {
        hits.push({
          message: m,
          conversation,
          type: 'VOX',
          snippet: `matched on filename and sender only · ${voice.durationMs}`,
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
export function jumpTo(messageId: string, seekMs?: number) {
  const m = messageById(messageId)
  if (!m) return
  state.activeId = m.conversationId
  state.view = 'thread'
  state.arrivedAt = messageId

  const voice = voiceOf(m)
  if (voice && seekMs !== undefined) {
    seek(messageId, seekMs / voice.durationMs)
  }

  // The rule stays until the next scroll; the wash fades on its own.
  setTimeout(() => {
    if (state.arrivedAt === messageId) state.arrivedAt = null
  }, 4000)
}

/* ── the outbox's wiring ────────────────────────────────────────────────── */

let current: Outbox | null = null

/**
 * Build the outbox this app renders. Every seam is replaceable: `smoke.ts`
 * passes a MemoryOutboxStore and a ScriptedTransport, and CANT-163 passes its
 * adapter over CANT-35's transport in place of the NullTransport.
 *
 * The defaults are the shipped app's: IndexedDB `catenary-outbox` where it
 * exists, the Web Lock and BroadcastChannel where they exist, and a transport
 * that is never ready.
 */
export async function configureOutbox(
  seams: Partial<Omit<OutboxOptions, 'onChange' | 'accountId'>> & { seed?: boolean } = {},
): Promise<Outbox> {
  current?.close()
  current = null
  const browser = typeof window !== 'undefined'
  // Where IndexedDB is absent or refuses to open (SSR, or a private window),
  // the outbox still works for this page's life — and with no storage API
  // handed to it, the standing line says unsent messages may not be kept.
  let durable = typeof indexedDB !== 'undefined'
  let store: OutboxStore
  if (seams.store) store = seams.store
  else if (!durable) store = new MemoryOutboxStore()
  else {
    try {
      store = await IdbOutboxStore.open()
    } catch {
      durable = false
      store = new MemoryOutboxStore()
    }
  }
  // The canvas's failed send (fixtures). Put only where absent, so a RETRY
  // or a DELETE made in this browser is not undone by the next put.
  if (seams.seed ?? true) {
    const held = new Set((await store.list()).map((e) => e.clientId))
    for (const e of OUTBOX_SEED) if (!held.has(e.clientId)) await store.put(e)
  }
  const nav = typeof navigator !== 'undefined' ? navigator : undefined
  const outbox = await Outbox.open({
    store,
    transport: seams.transport ?? new NullTransport(),
    accountId: state.me,
    lock: seams.lock ?? (nav?.locks ? new WebLockDrainLock(nav.locks) : new InProcessLockHub().lock()),
    channel: seams.channel !== undefined ? seams.channel : browser && typeof BroadcastChannel !== 'undefined' ? new BroadcastOutboxChannel() : null,
    storage: seams.storage !== undefined ? seams.storage : durable ? (nav?.storage ?? null) : null,
    ...(seams.clock ? { clock: seams.clock } : {}),
    ...(seams.random ? { random: seams.random } : {}),
    ...(seams.faults ? { faults: seams.faults } : {}),
    onChange: (view) => {
      outboxView.value = view
    },
  })
  current = outbox
  outboxView.value = outbox.view()
  return outbox
}

/** Resolves once the outbox has loaded. Every action that touches it waits
 *  on this; a reconfigure replaces it. */
export let outboxReady: Promise<Outbox> = configureOutbox()

/** Rebuild the outbox over new seams, and make every action use it. */
export function useOutbox(seams: Parameters<typeof configureOutbox>[0]): Promise<Outbox> {
  outboxReady = configureOutbox(seams)
  return outboxReady
}

export { isOutboxMessage, state }
