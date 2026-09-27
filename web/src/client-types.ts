/* Client-local types (CANT-34).
 *
 * `types.ts` is gone: every wire concept is defined once, in the schema, and
 * this app reads it from `@/wire/generated`. What lives here is deliberately
 * the complement of that rather than a second copy of it — a wire type
 * widened with something the server has no opinion on, or a concept with no
 * wire equivalent at all. Most components need neither and import `User`,
 * `Conversation`, `ReplyRef`, `VoiceAttachment`, `ConversationKind` and
 * `TranscriptState` straight from `@/wire/generated`.
 */

import type {
  Attachment as WireAttachment,
  ImageAttachment as WireImageAttachment,
  Message as WireMessage,
  DeliveryState as WireDeliveryState,
} from '@/wire/generated'

/**
 * `sending`, `queued` and `failed` describe a message's relationship to its
 * own outbox, which the server has no opinion on — CANT-36's own ticket says
 * so, and the schema's `DeliveryState` is deliberately narrower. Invariant 3:
 * the client never claims something the server cannot keep, so these three
 * are additions on top of the wire's own states, never a replacement of one.
 */
export type DeliveryState = WireDeliveryState | 'sending' | 'queued' | 'failed'

/**
 * `uploadedBytes` is present only while a presigned upload (CANT-48) is still
 * in flight — a client-local progress fact, not something the server ever
 * serves once the message exists to be fetched.
 */
export type ImageAttachment = WireImageAttachment & { uploadedBytes?: number }

export type Attachment = Exclude<WireAttachment, WireImageAttachment> | ImageAttachment

/** `Message`, widened three ways: a client-authored delivery state,
 *  attachments that may carry local-only upload progress, an outbox error
 *  (populated only in the `failed` state, shown inline, never as a toast —
 *  the server has no opinion on why a send never left the building), and the
 *  idempotency key a client mints for its own retries; the wire has no
 *  opinion on that either, since `client_id` is what dedups a send once it
 *  arrives. */
export type Message = Omit<WireMessage, 'state' | 'attachments'> & {
  state: DeliveryState
  attachments?: Attachment[]
  error?: string
  idempotencyKey?: string
}

/** The reconnect state machine's own labels. No wire equivalent — a session
 *  either is or isn't attached, and everything in between (backing off,
 *  resyncing) is this client's own bookkeeping about getting there. */
export type ConnectionState = 'live' | 'reconnecting' | 'resyncing' | 'offline'

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
