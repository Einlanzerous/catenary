/* The outbox's types and its three seams (CANT-36, built by CANT-161; the
 * `Uploader` by CANT-162).
 *
 * `docs/decisions/cant-36-outbox.md` is the rule set; the approved CANT-36 plan
 * is the decision of record. Nothing here is a wire type: an `OutboxEntry` is
 * client-local storage, and the only thing from it that ever reaches a socket
 * is the `ClientSend` the drain builds from it.
 */

import type { ClientSend, ErrorCode, ReplyRef, ServerAck, ServerError, Uuid } from '@/wire/generated'

/** Persisted in database `catenary-outbox`, store `outbox`. keyPath clientId.
 *  Index: by_account_order [accountId, order]. */
export interface OutboxEntry {
  v: 1
  /** THE wire idempotency key. Minted once at compose; never re-minted by
   *  retry, reload, reconnect or re-upload. */
  clientId: Uuid
  /** The author it is sent as. Other accounts' entries are never sent. */
  accountId: Uuid
  /** What the send names; `ack.conversationId` wins once acked. */
  conversationId: Uuid
  /** Per-device, drawn inside the put's own transaction. Drain order. NOT the
   *  device clock. */
  order: number
  /** Device clock. Local display and rail ordering only — never sent, never
   *  the message's time. */
  composedAt: string
  text?: string
  /** The only reply field sent. */
  replyToMessageId?: Uuid
  /** Display-only, from `previewOf()`. Never sent. */
  replyPreview?: ReplyRef
  attachments?: OutboundAttachmentDraft[]
  /** `sending` versus `queued` is DERIVED, never stored, and there is no
   *  stored ack. */
  status: 'pending' | 'failed'
  /** Frames written for this entry. */
  attempts: number
  /** Retryable refusals received — one per refusal, not per tab. */
  internalRetries: number
  /** Automatic re-uploads after `upload_not_found`. Bounded at 1, and not
   *  reset by RETRY (§10 item 4). */
  reuploads: number
  /** Earliest resend under backoff / `retryAfterSec`, wall clock. */
  notBefore?: string
  lastError?: OutboxError
}

export type OutboxError =
  | { kind: 'server'; code: ErrorCode; message: string; retryable: boolean }
  | { kind: 'bare_1008' }
  | { kind: 'upload'; message: string }

/** An attachment as composed. Its `Blob` is held with the entry, which is what
 *  lets it survive a reload and lets a stale handle be re-uploaded rather than
 *  lost. The `Uploader` moves it to `uploaded`; until then the send drain skips
 *  the entry rather than sending it without its handle (§10 item 1). */
export interface OutboundAttachmentDraft {
  kind: 'voice' | 'image'
  /** The media as recorded or chosen; the service decodes. */
  blob: Blob
  filename?: string
  /** Voice: local render only; the server measures its own. */
  durationMs?: number
  /** Set when the Uploader returns; cleared on `upload_not_found`. */
  uploadId?: Uuid
  /** Only `pending` and `uploaded` are ever stored. An offer in flight is
   *  this context's memory alone, for the reason `sending` is never stored
   *  (§3): after a reload every unanswered offer reads `pending` again. */
  upload: 'pending' | 'uploading' | 'uploaded'
}

/** What `compose` is given; everything else on an entry is the outbox's. */
export interface OutboxDraft {
  conversationId: Uuid
  text?: string
  replyToMessageId?: Uuid
  replyPreview?: ReplyRef
  attachments?: OutboundAttachmentDraft[]
}

/* ── seam 1: persistence ─────────────────────────────────────────────────── */

/**
 * Every write resolves only after its transaction has committed. `add`
 * allocates `order` inside the same transaction as the put, so two contexts
 * composing at once cannot draw the same value.
 */
export interface OutboxStore {
  /** Allocate `order` (highest for the account, plus one) and put, in one
   *  strict readwrite transaction. */
  add(entry: Omit<OutboxEntry, 'order'>): Promise<OutboxEntry>
  /** Put as given. Nothing in the outbox calls this for a new entry — the
   *  `orderOutsideTxn` fault does, and seeding does. */
  put(entry: OutboxEntry): Promise<void>
  /** Read-modify-write in one transaction. Does nothing, and resolves
   *  `undefined`, when the entry is gone — so a late counter update can never
   *  resurrect an entry a record has already settled. */
  update(clientId: Uuid, mutate: (e: OutboxEntry) => void): Promise<OutboxEntry | undefined>
  delete(clientId: Uuid): Promise<void>
  /** Every entry, every account, in `order`. */
  list(): Promise<OutboxEntry[]>
  close(): void
}

/* ── seam 2: the transport ───────────────────────────────────────────────── */

/**
 * What the outbox needs from a session. CANT-163 adapts CANT-35's transport
 * onto it: `ready` from `subscribe()` (and `status().ready`, read once, for
 * `isReady()`), `closed` from `onSessionEnd` — `bare1008` is CANT-35's
 * classification, consumed here and never re-derived — `ack` and `error` from
 * what `send` resolves or rejects with, and `message` from `onApply`.
 *
 * `sendFrame` returns nothing: an answer, if any, arrives as an event.
 */
export interface OutboxTransport {
  isReady(): boolean
  /** CANT-31 §6's terminal state, as the transport holds it NOW. Asked at
   *  compose, because a `closed` event is not how every terminal arrives: a
   *  credential refused at `/refresh` before any socket opened ends no
   *  session and emits nothing (CANT-220 ruling 2; CANT-162 ruling 2 gave
   *  the web seam the member the Dart seam already had). */
  isTerminal(): boolean
  sendFrame(frame: ClientSend): void
  subscribe(listener: (event: OutboxTransportEvent) => void): () => void
}

export type OutboxTransportEvent =
  | { type: 'ready' }
  | { type: 'ack'; ack: ServerAck }
  | { type: 'error'; error: ServerError }
  /** A record held — a `message` frame or a `/sync` record. Only its
   *  `clientId` is read; a wire `Message` is assignable. */
  | { type: 'message'; message: { clientId?: Uuid } }
  /** The session is no longer ready. `terminal` is CANT-31 §6's terminal
   *  state, which never deletes the local store. */
  | { type: 'closed'; bare1008: boolean; terminal?: boolean }
  /** CANT-24 obligation 4's discard-and-bootstrap. It wipes server-derived
   *  state, and the outbox is not server-derived. */
  | { type: 'bootstrap' }

/* ── seam 3: the uploader (§10, CANT-162) ────────────────────────────────── */

/**
 * Uploads one attachment and resolves with its handle.
 *
 * EVERY UPLOAD SETTLES — RESOLVES OR REJECTS — WITHIN A BOUND THE UPLOADER
 * OWNS. The outbox keeps no deadline: it offers an entry once, by the drain
 * lock's holder on a ready session, and does not offer it again while that
 * offer is unanswered, across any number of reconnects (ruling 1). Only the
 * uploader can tell a slow upload from a dead one. A rejection fails the
 * entry, which is what gives the person RETRY; a promise that never settles
 * holds it `pending` until a reload.
 *
 * It takes the entry and the attachment rather than the blob and the kind
 * alone, so a presign that needs the conversation, or wants to key an object
 * by `clientId`, can be served without the seam changing.
 */
export interface Uploader {
  upload(entry: OutboxEntry, attachment: OutboundAttachmentDraft): Promise<Uuid>
}

export class UploadRefused extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'UploadRefused'
  }
}

/** §10's default until a real uploader exists (CANT-247): it rejects at once
 *  with a clear message, and an attachment entry goes to `failed` with that
 *  inline error — never a loop. `configureOutbox` builds the shipped app with
 *  no uploader, so this is what every attachment meets there. */
export class RefusingUploader implements Uploader {
  static readonly message = "Attachments can't be sent yet"

  upload(): Promise<Uuid> {
    return Promise.reject(new UploadRefused(RefusingUploader.message))
  }
}

/** `compose` would not take the draft, and wrote nothing. The message is for
 *  the person who composed it. */
export class ComposeRefused extends Error {
  /** CANT-36 ruling 5, enforced in the outbox (CANT-162 ruling 2): a picked
   *  file is not composed without a ready session. */
  static readonly pickedFileOffline = "A file can't be attached while offline"
  /** A terminal client drains nothing, so it takes no new attachment to
   *  hold. */
  static readonly terminal = 'This device cannot send'

  constructor(message: string) {
    super(message)
    this.name = 'ComposeRefused'
  }
}

/* ── the multi-context pieces (ruling 4) ─────────────────────────────────── */

/** `catenary.outbox`. `request` resolves nothing; `onGranted` runs once the
 *  lock is held, and the returned function releases it — or withdraws the
 *  request if it has not been granted yet. */
export interface DrainLock {
  request(onGranted: () => void): () => void
}

/** `BroadcastChannel('catenary.outbox')`. A post reaches every OTHER context. */
export interface OutboxChannel {
  post(message: ChannelMessage): void
  subscribe(listener: (message: ChannelMessage) => void): () => void
  close(): void
}

/** Every post means "re-read the store". A holder also carries what only it
 *  knows — which entries are in flight on its session and which it has been
 *  acked — so every tab renders the same state; `released` clears that. None
 *  of it is persisted. */
export interface ChannelMessage {
  holder?: { inFlight: Uuid[]; acks: ServerAck[] }
  released?: boolean
}

/** `navigator.storage`, injectable. Either method may be absent. */
export interface PersistApi {
  persist?(): Promise<boolean>
  persisted?(): Promise<boolean>
}

export interface Clock {
  now(): number
  setTimeout(fn: () => void, ms: number): unknown
  clearTimeout(handle: unknown): void
}

/* ── the render projection's input ───────────────────────────────────────── */

/** `queued` / `sending` / `sent` are derived; `failed` is the stored status. */
export type OutboxState = 'queued' | 'sending' | 'sent' | 'failed'

export interface OutboxItem {
  entry: OutboxEntry
  state: OutboxState
  /** Held in memory only, this session. */
  ack?: ServerAck
  /** Ruling 3 C: `pending` with three or more retryable refusals received. */
  retrying: boolean
}

export type PersistStatus = 'unknown' | 'granted' | 'refused'

export interface OutboxView {
  items: OutboxItem[]
  persist: PersistStatus
}

/**
 * Test-only negative controls (the rollout section's fault table). Each one
 * breaks exactly one rule, and `npm run outbox` requires the criterion that
 * rule serves to FAIL with it on. Never set outside `outbox.test.ts`.
 */
export interface OutboxFaults {
  sendBeforePersist?: boolean
  remintOnRetry?: boolean
  persistSending?: boolean
  settlePendingOnly?: boolean
  misclassifyRetryable?: boolean
  misjudgeRetryBudget?: boolean
  retryNonRetryable?: boolean
  resendAfterBare1008?: boolean
  resendFailedWithoutRetry?: boolean
  deleteOnTerminal?: boolean
  crossAccountLeak?: boolean
  orderOutsideTxn?: boolean
  everyTabDrains?: boolean
  lockWithoutReady?: boolean
  /* CANT-162's six, on criteria 13 and 14. */
  /** The drain stops at the first entry awaiting an upload. */
  uploadBlocksDrain?: boolean
  /** An entry joins the drain when its first attachment has a handle, not
   *  its last. */
  sendBeforeLastUpload?: boolean
  /** `upload_not_found` sends the entry to `failed` with no re-upload. */
  staleHandleFails?: boolean
  /** Every `upload_not_found` re-uploads, with no bound. */
  reuploadUnbounded?: boolean
  /** An `Uploader` rejection leaves the entry `pending`, to be offered
   *  again, rather than failing it. */
  refusalLoops?: boolean
  /** The resend after a re-upload carries a fresh `clientId`. */
  remintOnReupload?: boolean
}

/** The store's own two faults, which live below the `Outbox`. */
export interface StoreFaults {
  /** The put succeeds and the transaction is then aborted, so `complete`
   *  never fires. */
  neverCompletes?: boolean
  /** Opens entry writes without `{ durability: 'strict' }`. */
  relaxedDurability?: boolean
}
