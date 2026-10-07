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

/** `Message`, widened two ways: a client-authored delivery state, and
 *  attachments that may carry local-only upload progress. Also carries an
 *  outbox `error`, populated only in the `failed` state and shown inline,
 *  never as a toast — the server has no opinion on why a send never left the
 *  building. NOT widened with a second idempotency key: `WireMessage.clientId`
 *  already is one (`schema/mapping/wire-fields.json`: "Idempotency key, unique
 *  per (author_id, client_id)"), echoed back to the sender so an outbox entry
 *  can be matched against the `ack`/`message` frame it produces — a client
 *  mints it with `send()`'s own `clientId`, not a second field. */
export type Message = Omit<WireMessage, 'state' | 'attachments'> & {
  state: DeliveryState
  attachments?: Attachment[]
  error?: string
}

/**
 * An outbox entry as the thread and the rail render it (CANT-36). NOT a
 * `Message`: it has no `logSeq`, and no `seq` until the server acks it — an
 * entry is never given an ordinal of its own, because `seq` is dense and a
 * locally invented one reads as a gap that does not exist (invariant 1). Its
 * `id` is its `clientId`, and `pending` marks it as something nothing may
 * jump to or reply to. `retrying` is ruling 3 C's RETRYING label, derived.
 */
export type OutboxMessage = Omit<Message, 'seq' | 'logSeq'> & {
  seq?: number
  pending: true
  retrying: boolean
}

/** What a row renders: a server record, or an outbox entry's projection. */
export type RenderedMessage = Message | OutboxMessage

export const isOutboxMessage = (m: RenderedMessage): m is OutboxMessage =>
  'pending' in m && m.pending === true

/** One of your own that has not left this device: an outbox entry that is
 *  queued, sending or failed. Not an acked one — the server holds that, and
 *  whatever it says of the message is a claim it can keep. The same rule as
 *  `unsent` in `preview` (app/lib/store/conversation.dart): a note that never
 *  reached the server is not "transcript pending", because nothing is
 *  transcribing it (Invariant 3, CANT-235). */
export const isUnsent = (m: RenderedMessage): boolean => isOutboxMessage(m) && m.state !== 'sent'

/** The reconnect state machine's own labels. No wire equivalent — a session
 *  either is or isn't attached, and everything in between (backing off,
 *  resyncing) is this client's own bookkeeping about getting there.
 *
 *  `terminal` is CANT-31 §6's: this client will not reconnect until it is
 *  relaunched or re-enrolled, which is not the same thing as a backoff that
 *  has grown to its ceiling — and nothing queued will drain, so nothing may say
 *  it will (Invariant 3, CANT-35 criterion 30). */
export type ConnectionState = 'live' | 'reconnecting' | 'resyncing' | 'offline' | 'terminal'

export interface ConnectionInfo {
  state: ConnectionState
  /** terminal: which one, and the observation that put the client there. */
  terminal?: { kind: 'credential' | 'protocol'; reason: string }
  /** CANT-127: why no automatic refresh is being made — a third thing beside
   *  a grown backoff and either terminal. */
  refreshHold?: 'none' | 'unreachable' | 'backoff'
  /** CANT-129: Catenary refused the access token held now. */
  tokenRefused?: boolean
  /** CANT-169: the journal's last write did not land (a quota refused, a
   *  transaction aborted) — by the error's name — until one does. */
  journalError?: { name: string; message: string }
  /** reconnecting: which attempt, and how long until the next one. */
  attempt?: number
  retryInSec?: number
  /** resyncing: numeric progress, never a spinner. */
  synced?: number
  total?: number
  roomsPending?: number
}
