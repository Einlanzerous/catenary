/* Client state.
 *
 * Deliberately not Pinia — one reactive object and a handful of actions is the
 * whole surface, and the dependency list stays at `vue`.
 *
 * The shape here is the shape the sync protocol implies: messages are an
 * append-only log keyed by a per-conversation `seq`, sends carry an
 * idempotency key, and nothing is ever mutated in place except a message's
 * own delivery state.
 *
 * EVERY RECORD IN HERE CAME FROM A SERVER (CANT-39). `state.messages`,
 * `conversations` and `users` are the transport's journal, projected by
 * CANT-35's `project`/`projectApplied` — there is no fixture corpus behind
 * them any more, and before a device is enrolled they are simply empty.
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
  type Message,
  type OutboxMessage,
  type RenderedMessage,
} from '@/client-types'
import { requestSelfConversation } from '@/account'
import { createEnsureSelf } from '@/ensure-self'
import { countWords } from '@/lib/format'
import {
  BroadcastOutboxChannel,
  IdbOutboxStore,
  InProcessLockHub,
  MemoryOutboxStore,
  Outbox,
  TransportOutbox,
  WebLockDrainLock,
  project,
  type OutboxOptions,
  type OutboxStore,
  type OutboxView,
} from '@/outbox'
import {
  EMPTY_PROJECTION,
  connectionInfo,
  consoleLogger,
  createRefreshingCredential,
  createTransport,
  heldCredential,
  project as projectJournal,
  projectApplied,
  type CredentialStore,
  type Journal,
  type Lock,
  type Logger,
  type Projection,
  type Transport,
  type TransportStatus,
  type WebSocketCtor,
} from '@/transport'

export type View = 'thread' | 'search' | 'account' | 'new'
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
  /** The enrolled account's user id; '' until a credential has been read. */
  me: '',
  users: {} as Record<string, User>,
  conversations: [] as Conversation[],
  messages: [] as Message[],

  /** The origin the session talks to — `startSession`'s `baseUrl`, which is
   *  `location.origin` in a browser. '' until a session has started, and kept
   *  when one ends, like everything else it put on screen. The thread
   *  header's transport word is derived from this and from nothing else. */
  origin: '',

  /** '' until the first projection names a conversation to open on. */
  activeId: '',
  /** A conversation this device has just been told exists (CANT-270) and has
   *  not yet received through the journal. Opened the moment its record
   *  lands; '' when nothing is waiting. */
  pendingOpenId: '',
  view: 'thread' as View,
  theme: 'dark' as Theme,

  /** `connectionInfo` over the live transport's status — it fills only what
   *  it knows, so `synced`/`total`/`roomsPending` are absent, not 0, whenever
   *  there is no number to show. */
  connection: { state: 'reconnecting' } as ConnectionInfo,

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

  /** Who is typing, per conversation, in the order they started — the
   *  server's `typing` frame, verbatim (the order is normative). */
  typing: {} as Record<string, string[]>,

  query: '',
  /** The message a search result or reply stub jumped to; drives the wash. */
  arrivedAt: null as string | null,
})

/* ── derived ───────────────────────────────────────────────────────────── */

const messagesFor = (conversationId: string) =>
  state.messages
    .filter((m) => m.conversationId === conversationId)
    .sort((a, b) => a.seq - b.seq)

/** Undefined until a server has named a conversation — App renders the
 *  thread pane only once there is one. */
export const activeConversation = computed<Conversation | undefined>(() =>
  state.conversations.find((c) => c.id === state.activeId),
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

/**
 * A conversation with only yourself (CANT-254): the wire's `self` kind. The
 * server keeps the one fact this stands on, that it has exactly one member.
 */
export const isSelf = (c: Conversation): boolean => c.kind === 'self'

/**
 * Rooms above DMs, both in one column with the same row anatomy.
 *
 * A ROOM IS EVERYTHING THAT IS NOT A DIRECT OR A SELF, which is CANT-74's
 * policy for a kind this build does not know: the `unknown` sentinel (what a
 * build without `self` decodes it to) renders as a group. It used to be
 * `kind === 'group'`, which dropped an unknown kind from the rail altogether.
 */
export const rooms = computed(() =>
  byRecency(state.conversations.filter((c) => c.kind !== 'direct' && !isSelf(c))),
)

/**
 * The DIRECT section: the self conversation first, then the directs by
 * recency (CANT-254 ruling 1, A). `Notes` is pinned rather than ordered by its
 * last message, so it is where you left it however busy a direct is; it is
 * counted in the section like any other row.
 */
export const directs = computed(() => [
  ...state.conversations.filter(isSelf),
  ...byRecency(state.conversations.filter((c) => c.kind === 'direct')),
])

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

/** A transcript's text, held only once it is ready — the one place the state
 *  is consulted, so no surface quotes text the strip says is not there. A
 *  failed or unknown transcript has none whatever the record carries, which
 *  is the Flutter app's rule too (`app/lib/store/app_store.dart`, CANT-227). */
export const transcriptText = (v: VoiceAttachment | undefined): string | undefined =>
  v?.transcript.state === 'ready' ? v.transcript.text : undefined

/** Transcript word count, derived rather than stored — "EXPAND · 96 W" has to
 *  agree with the text actually on screen. */
export const transcriptWords = (v: VoiceAttachment): number => {
  const text = transcriptText(v)
  return text ? countWords(text) : 0
}

/* ── actions ───────────────────────────────────────────────────────────── */

export function select(id: string) {
  // Going somewhere else abandons a conversation still on its way: it must
  // not pull the reader out of what they chose when it lands.
  if (state.pendingOpenId && state.pendingOpenId !== id) state.pendingOpenId = ''
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

/** CANT-31 §6: a terminal client drains nothing, so nothing is created as
 *  `queued` while it is one (Invariant 3; CANT-35 criterion 30). The draft
 *  stays in the box, which is the honest place for it. What the outbox
 *  already holds stays there too — a terminal state never deletes it. */
const cannotSend = () => state.connection.state === 'terminal'

/**
 * Compose into the outbox. Resolves with the entry's clientId.
 *
 * THE DRAFT IS TAKEN BEFORE THE FIRST AWAIT. The entry renders only once its
 * strict write has committed, and a second `send()` inside that window — a
 * second Enter, a key-repeat, Enter plus SEND — must find nothing to send,
 * or the same text is authored twice under two clientIds, which no server
 * dedup can merge. If the write is refused, the text and the armed reply go
 * back where they were, unless something new has been typed since.
 */
export async function send(): Promise<string | undefined> {
  const text = state.composer.draft.trim()
  if (!text || cannotSend()) return undefined
  const draft = state.composer.draft
  const replyToId = state.composer.replyToId
  const conversationId = state.activeId
  state.composer.draft = ''
  state.composer.replyToId = null

  const source = replyToId ? messageById(replyToId) : undefined
  const replyPreview: ReplyRef | undefined = source
    ? { messageId: source.id, authorId: source.authorId, ...previewOf(source) }
    : undefined

  try {
    const outbox = await outboxReady
    const entry = await outbox.compose({
      conversationId,
      text,
      ...(source ? { replyToMessageId: source.id, replyPreview } : {}),
    })
    return entry.clientId
  } catch (e) {
    if (!state.composer.draft) {
      state.composer.draft = draft
      state.composer.replyToId = replyToId
    }
    throw e
  }
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
      preview: transcriptText(voice) ?? '',
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

/**
 * The rooms still behind their `head_seq`: a conversation holds fewer
 * messages than its head says exist. DERIVED, NEVER STORED (CANT-37's loose
 * end) — from the same two facts `connectionInfo`'s `synced / total` is, per
 * room rather than summed, so the two cannot disagree about whether there is
 * anything left to fetch.
 */
export function roomsPendingIn(conversations: readonly Conversation[], messages: readonly Message[]): number {
  const held = new Map<string, number>()
  for (const m of messages) held.set(m.conversationId, (held.get(m.conversationId) ?? 0) + 1)
  return conversations.filter((c) => (held.get(c.id) ?? 0) < c.headSeq).length
}

/** The banner's facts, from the transport's status. `roomsPending` rides
 *  only beside a real `total`: "0 ROOMS PENDING" before a page has landed
 *  would be the same false claim a 0 / 0 is. */
function showStatus(s: TransportStatus) {
  const info = connectionInfo(s, {
    now: Date.now(),
    online: typeof navigator !== 'undefined' ? navigator.onLine : undefined,
  })
  if (info.state === 'resyncing' && info.total !== undefined) {
    info.roomsPending = roomsPendingIn(state.conversations, state.messages)
  }
  state.connection = info
  // A typing list is a fact about a live session; one that has ended says
  // nothing about who is typing now.
  if (!s.ready) state.typing = {}
}

/** RETRY NOW / RECONNECT: dial at once rather than at the end of the wait. */
export function retryNow() {
  live?.transport.retryNow()
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
  // There is no recorder yet (CANT-247), so the clip is empty; the entry is
  // real. The outbox offers it to its `Uploader` once a session is ready, and
  // the shipped app has none, so it fails there with "Attachments can't be
  // sent yet" and RETRY and DELETE — never a row that reads QUEUED for good.
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
      const text = transcriptText(voice)
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

/** CANT-270 — the new-conversation picker takes the main pane. */
export function openNew() {
  state.view = 'new'
}

export function closeNew() {
  state.pendingOpenId = ''
  if (state.view === 'new') state.view = 'thread'
}

/**
 * Open a conversation the server has just returned from a create (CANT-270).
 * Nothing is written to the journal from here: a conversation enters `state`
 * only through the transport's projection, so that one holding is the only
 * holding. A conversation the journal already holds (a second pick of the same
 * person) opens at once. One it does not hold yet is the server's to deliver —
 * creating it moved a metadata marker, so a catch-up carries it — and it opens
 * when `showProjection` sees it land. Resolves true when it opened at once.
 */
export function openConversation(c: Conversation): boolean {
  if (state.conversations.some((x) => x.id === c.id)) {
    state.pendingOpenId = ''
    select(c.id)
    return true
  }
  state.pendingOpenId = c.id
  live?.transport.catchUp()
  return false
}

/** CANT-38 — login, device naming and the session list. */
export function openAccount() {
  state.view = 'account'
}

export function closeAccount() {
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

/* ── the transport word (CANT-221) ──────────────────────────────────────── */

/**
 * The last word of the thread header: `TLS` for an `https:` origin and
 * `CLEARTEXT` for anything else — the Flutter client's two words and its rule
 * (`addressIsSecure`, app/lib/store/address.dart), so both clients say the
 * same thing about one server.
 *
 * THE SAME TEST THE TRANSPORT DIALS BY. `createTransport` picks `wss:` over
 * `ws:` on `new URL(baseUrl).protocol === 'https:'`, so the word is read the
 * same way and cannot contradict the socket it describes.
 *
 * AN UNKNOWN ORIGIN IS NOT A SECURE ONE. '' — no session yet — and anything
 * that does not parse read CLEARTEXT, because TLS is a claim and this is the
 * surface that must not make one it cannot keep (invariant 3). Never `E2E`:
 * there is no input for which this returns it.
 */
export function transportWord(origin: string): 'TLS' | 'CLEARTEXT' {
  try {
    return new URL(origin).protocol === 'https:' ? 'TLS' : 'CLEARTEXT'
  } catch {
    return 'CLEARTEXT'
  }
}

/** `transportWord` over the origin the session actually talks to. */
export const transportLabel = computed(() => transportWord(state.origin))

/* ── the outbox's wiring ────────────────────────────────────────────────── */

let current: Outbox | null = null
/** The adapter the current outbox is wired to, when this module built it;
 *  detached on a reconfigure. */
let adapter: TransportOutbox | null = null
let shipped: Transport | null = null

/**
 * The transport the outbox is wired to BEFORE A DEVICE IS ENROLLED: built and
 * never started. Until `startSession` hands `useTransport` a live one, no
 * socket opens, `ready` never goes true, and every entry renders honestly
 * QUEUED. The credential refuses rather than inventing an identity, so a
 * stray `start()` fails its dial and claims nothing.
 */
function shippedTransport(): Transport {
  shipped ??= createTransport({
    baseUrl: typeof location !== 'undefined' ? location.origin : 'http://localhost',
    credential: heldCredential(() => {
      throw new Error('no enrolled device: startSession() has not run')
    }),
  })
  return shipped
}

/**
 * Build the outbox this app renders. Every seam is replaceable: the outbox's
 * own tests pass a MemoryOutboxStore and a ScriptedTransport, and
 * `useTransport` an adapter over a live CANT-35 transport.
 *
 * The defaults are the shipped app's: IndexedDB `catenary-outbox` where it
 * exists, the Web Lock and BroadcastChannel where they exist, the adapter
 * over the unstarted transport above until `startSession` replaces it, and
 * NO UPLOADER — the outbox's own default refuses every attachment with
 * "Attachments can't be sent yet". `seams.uploader` is the one line CANT-247
 * uses to pass the real one through.
 */
export async function configureOutbox(
  seams: Partial<Omit<OutboxOptions, 'onChange' | 'accountId'>> = {},
): Promise<Outbox> {
  current?.close()
  current = null
  // Detach the previous adapter and build this one BEFORE the first await, so
  // a reconfigure that overtakes this one while the store opens still finds
  // it — and closes it — rather than leaving it attached.
  adapter?.close()
  adapter = null
  const transport = seams.transport ?? (adapter = new TransportOutbox(shippedTransport()))
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
  const nav = typeof navigator !== 'undefined' ? navigator : undefined
  const outbox = await Outbox.open({
    store,
    transport,
    accountId: state.me,
    lock: seams.lock ?? (nav?.locks ? new WebLockDrainLock(nav.locks) : new InProcessLockHub().lock()),
    channel: seams.channel !== undefined ? seams.channel : browser && typeof BroadcastChannel !== 'undefined' ? new BroadcastOutboxChannel() : null,
    storage: seams.storage !== undefined ? seams.storage : durable ? (nav?.storage ?? null) : null,
    ...(seams.clock ? { clock: seams.clock } : {}),
    ...(seams.random ? { random: seams.random } : {}),
    ...(seams.uploader ? { uploader: seams.uploader } : {}),
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

/** Rebuild the outbox over a CANT-35 transport — `startSession`'s one call
 *  once it has built and started the live one. The transport stays the
 *  caller's to stop; a reconfigure detaches the adapter from it. */
export function useTransport(transport: Transport, logger: Logger = consoleLogger): Promise<Outbox> {
  const a = new TransportOutbox(transport, logger)
  const ready = useOutbox({ transport: a })
  // `configureOutbox` has run to its first await, so this is recorded after
  // it detached the previous adapter, and the next reconfigure detaches it.
  adapter = a
  return ready
}

/* ── the live session (CANT-39) ─────────────────────────────────────────── */

export interface SessionSeams {
  /** The server's origin, `http://` or `https://` — `location.origin` in the
   *  app, where the Go binary (or Vite's dev proxy) serves both. */
  baseUrl: string
  /** Where the enrolled credential is held: `IdbCredentialStore` in a
   *  browser (account.ts's `credentialStore()`), a memory store in the smoke. */
  store: CredentialStore
  /** Default: a fresh in-memory journal, the transport's own. */
  journal?: Journal
  lock?: Lock
  fetch?: typeof globalThis.fetch
  WebSocket?: WebSocketCtor
  logger?: Logger
}

interface LiveSession {
  transport: Transport
  end(): void
}

let live: LiveSession | null = null
/** Bumped by every start and end, so a start overtaken while it awaited the
 *  credential store abandons itself rather than running beside its successor. */
let sessionRun = 0

/** The live transport, or null before a device is enrolled. */
export const liveTransport = (): Transport | null => live?.transport ?? null

/**
 * Wire this app to a live server (CANT-35 ruling 8 → A): the enrolled
 * credential, `start()`, the journal projected into `state`, and the outbox
 * rebuilt over the same transport. Resolves true once the transport is
 * started, false when this device holds no credential — the one case that
 * starts no transport at all (CANT-152), and the caller's cue to show the
 * login form.
 *
 * ENDS ANY SESSION ALREADY RUNNING FIRST, so the same call is also the
 * restart a login or a re-enrollment needs: a credential terminal ends only
 * on a relaunch or a re-enrollment (CANT-31 §6), and a terminal transport
 * refuses `start()` — a new credential gets a new transport.
 *
 * THE PROJECTION IS CANT-35's, NOT A SECOND ONE: `project` over whatever the
 * journal already holds (a durable journal holds the last visit), then
 * `projectApplied` folded over every `Applied` after it — the equivalence
 * CANT-151 tests is what makes the two a single view.
 */
export async function startSession(seams: SessionSeams): Promise<boolean> {
  endSession()
  const run = sessionRun
  const held = await seams.store.read()
  if (run !== sessionRun || !held) return false

  // THE JOURNAL IS CLAIMED BEFORE ANYTHING IS BUILT OVER IT (CANT-230). A
  // re-enrollment writes the pair and then wipes the journal, as two writes;
  // a tab closed between them leaves this account's credential over another
  // account's records and cursor. The claim wipes a journal that is someone
  // else's, here, whatever was or was not written before. It is a second
  // await, so the overtake check is made again after it: a start overtaken
  // while its claim was being written builds no transport.
  if (seams.journal) {
    await seams.journal.claim(held.userId)
    if (run !== sessionRun) return false
  }

  // Another account's records are not this one's, whatever a reused journal
  // says; a fresh start for the same account keeps what is on screen until
  // the journal's own projection replaces it.
  if (state.me !== held.userId) {
    state.me = held.userId
    state.users = {}
    state.conversations = []
    state.messages = []
    state.activeId = ''
    state.pendingOpenId = ''
    state.read.clear()
  }
  state.typing = {}
  const logger = seams.logger ?? consoleLogger
  const transport = createTransport({
    baseUrl: seams.baseUrl,
    credential: createRefreshingCredential({
      baseUrl: seams.baseUrl,
      store: seams.store,
      logger,
      ...(seams.lock ? { lock: seams.lock } : {}),
      ...(seams.fetch ? { fetch: seams.fetch } : {}),
    }),
    logger,
    ...(seams.journal ? { journal: seams.journal } : {}),
    ...(seams.fetch ? { fetch: seams.fetch } : {}),
    ...(seams.WebSocket ? { WebSocket: seams.WebSocket } : {}),
  })
  // The one place the header's transport word gets its input, and only once
  // a transport exists over it: `createTransport` throws on an origin it
  // cannot dial, and an origin nothing was built over is not one to name.
  state.origin = seams.baseUrl

  let projection: Projection = EMPTY_PROJECTION
  const show = (next: Projection) => {
    projection = next
    showProjection(projection)
    showStatus(transport.status())
  }
  show(projectJournal(transport.snapshot()))

  // The countdown the banner reads ticks between status changes, so the
  // status is re-read once a second while nothing else moves it.
  const ticker = setInterval(() => showStatus(transport.status()), 1000)
  // CANT-262: the self conversation is ensured once, after this session's first
  // completed catch-up, and only when the journal holds none (CANT-254 ruling
  // 3 → B). Nothing a person reads is written on failure.
  const ensureSelf = createEnsureSelf({
    holdsSelf: () => state.conversations.some(isSelf),
    request: () =>
      requestSelfConversation({
        baseUrl: seams.baseUrl,
        store: seams.store,
        ...(seams.lock ? { lock: seams.lock } : {}),
        ...(seams.fetch ? { fetch: seams.fetch } : {}),
      }),
    afterCreated: () => transport.catchUp(),
  })
  const offs = [
    transport.onApply((applied) => show(projectApplied(projection, applied))),
    transport.subscribe(showStatus),
    transport.subscribe((s) => ensureSelf.observe(s)),
    transport.onTyping((f) => {
      state.typing[f.conversationId] = [...f.userIds]
    }),
  ]
  live = {
    transport,
    end() {
      clearInterval(ticker)
      for (const off of offs) off()
      transport.stop()
    },
  }
  transport.start()
  showStatus(transport.status())
  await useTransport(transport, logger)
  return true
}

/** Stop the live transport and stop listening to it. What it projected stays
 *  on screen: ending a session is not forgetting what the server said. */
export function endSession() {
  sessionRun++
  live?.end()
  live = null
}

/**
 * Put a projection on screen, and — the first time there is anything to open
 * — open on the top of the rail, exactly as the old boot did with its fixed
 * Kitchen Table: the conversation you open with is, by definition, one you
 * are reading, so it carries no badge, and its "N NEW" rule stays put for the
 * visit.
 */
function showProjection(p: Projection) {
  state.messages = p.messages
  state.conversations = p.conversations
  state.users = p.users
  if (state.pendingOpenId && state.conversations.some((c) => c.id === state.pendingOpenId)) {
    const id = state.pendingOpenId
    state.pendingOpenId = ''
    select(id)
    return
  }
  if (!state.conversations.some((c) => c.id === state.activeId)) {
    const first = rooms.value[0] ?? directs.value[0]
    state.activeId = first?.id ?? ''
    if (first) state.read.add(first.id)
  }
}

export { isOutboxMessage, state }
