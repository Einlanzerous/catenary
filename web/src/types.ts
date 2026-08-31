/* Wire types.
 *
 * HAND-WRITTEN, AND THAT IS TEMPORARY. R4 (IDEA-27) says these are generated
 * from one schema alongside the Dart equivalents, before either client is
 * written for real. This file is the shape that schema has to describe — it
 * is a draft of the contract, not the contract. Do not let a second
 * hand-written copy of it appear in the Flutter client; that is precisely the
 * drift R4 exists to prevent.
 */

export type ConversationKind = 'direct' | 'group'

/** The lifecycle from the architecture sketch, plus the two states a client
 *  owns on its own: `queued` (offline outbox) and `failed`. */
export type DeliveryState =
  | 'sending'
  | 'queued'
  | 'sent'
  | 'delivered'
  | 'read'
  | 'failed'

export type TranscriptState = 'pending' | 'ready' | 'failed'

export interface User {
  id: string
  name: string
  /** Two letters. Rooms have none — the rail is text-only for rooms. */
  initials: string
}

export interface VoiceAttachment {
  kind: 'voice'
  durationSec: number
  /**
   * Amplitude peaks, 0–100, precomputed server-side and stored with the
   * message. Deliberate call 13: the bar pattern must be identical in web and
   * Flutter, which is only true if both render the *same stored array*. A
   * client-side generator would not be — see lib/waveform.ts.
   */
  peaks: number[]
  transcript: {
    state: TranscriptState
    text?: string
    wordCount?: number
    /** Word offsets in seconds, for search's JUMP TO and playback highlight. */
    segments?: { at: number; text: string }[]
    engine?: string
    language?: string
    /** Server's own estimate, shown while pending. */
    etaSec?: number
  }
}

export interface ImageAttachment {
  kind: 'image'
  filename: string
  width: number
  height: number
  bytes: number
  /** Stands in for a real blurhash; the aspect ratio is what fixes row
   *  height before a byte of the image arrives. */
  placeholder?: string
  /** Present only while in flight. */
  uploadedBytes?: number
}

export type Attachment = VoiceAttachment | ImageAttachment

export interface ReplyRef {
  messageId: string
  authorId: string
  /** One line, pre-truncated by the sender's client is wrong — the *stub* is
   *  rendered from the live source so it back-fills when a transcript lands. */
  kind: 'text' | 'voice' | 'image' | 'link'
  preview: string
  durationSec?: number
  url?: string
}

export interface Message {
  id: string
  /** Server-assigned, monotonic, scoped per conversation. The whole sync
   *  protocol hangs off this: clients resync with "everything after seq N". */
  seq: number
  conversationId: string
  authorId: string
  at: string // ISO-8601
  text?: string
  attachments?: Attachment[]
  replyTo?: ReplyRef
  state: DeliveryState
  /** Populated only in the `failed` state. Shown inline, never as a toast. */
  error?: string
  /** Rooms report a fraction: "READ 5/7". */
  readBy?: number
  /** Client-generated; retries are free and safe. */
  idempotencyKey?: string
}

export interface Conversation {
  id: string
  kind: ConversationKind
  name: string
  memberCount: number
  muted?: boolean
  /**
   * First seq the reader has not seen. Both the rail's badge and the thread's
   * "N NEW" rule derive from this — there is deliberately no stored count, so
   * the two can never disagree, and the rule still lands correctly when a
   * resync delivers messages out of order.
   */
  firstUnreadSeq?: number
  /** Newest seq the server holds — what a resync counts towards. */
  headSeq: number
}

export type ConnectionState =
  | 'live'
  | 'reconnecting'
  | 'resyncing'
  | 'offline'

export interface ConnectionInfo {
  state: ConnectionState
  /** reconnecting: which attempt, and how long until the next one. */
  attempt?: number
  retryInSec?: number
  /** resyncing: numeric progress, never a spinner. */
  synced?: number
  total?: number
  roomsPending?: number
}
