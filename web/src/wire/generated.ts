// Code generated from schema/catenary.wire.v1.schema.json. DO NOT EDIT.
//
// Regenerate with `npm run gen` from web/. Wire version 1.
// Editing this file by hand makes it a hand-written file with extra steps,
// and `npm run gen:check` will fail in CI the moment you do.
// See IDEA-27 (R4).

/* eslint-disable */

export const WIRE_VERSION = 1

// Thrown when a frame does not match the schema. Carries the JSON path so a malformed
// field is identifiable from a log line rather than requiring a repro.
export class WireFormatError extends Error {
  constructor(public readonly path: string, message: string) {
    super(`${path}: ${message}`)
    this.name = 'WireFormatError'
  }
}

const bad = (p: string, m: string): never => { throw new WireFormatError(p, m) }
const asStr = (v: unknown, p: string): string => typeof v === 'string' ? v : bad(p, `expected string, got ${typeof v}`)
const asBool = (v: unknown, p: string): boolean => typeof v === 'boolean' ? v : bad(p, `expected boolean, got ${typeof v}`)
const asNum = (v: unknown, p: string): number => typeof v === 'number' && Number.isFinite(v) ? v : bad(p, `expected number, got ${typeof v}`)
const asInt = (v: unknown, p: string): number => {
  const n = asNum(v, p)
  if (!Number.isInteger(n)) bad(p, `expected integer, got ${n}`)
  // The schema caps every ordinal at 2^53-1 precisely so this cannot bite.
  if (!Number.isSafeInteger(n)) bad(p, `integer ${n} exceeds the safe range and has already lost precision`)
  return n
}
const asArray = (v: unknown, p: string): unknown[] => Array.isArray(v) ? v : bad(p, `expected array, got ${typeof v}`)
const asObj = (v: unknown, p: string): Record<string, unknown> => (typeof v === 'object' && v !== null && !Array.isArray(v)) ? v as Record<string, unknown> : bad(p, `expected object, got ${v === null ? 'null' : typeof v}`)
const asOneOf = <T extends string>(v: unknown, allowed: readonly T[], p: string): T => {
  const s = asStr(v, p)
  return (allowed as readonly string[]).includes(s) ? s as T : bad(p, `expected one of ${allowed.join('|')}, got ${JSON.stringify(s)}`)
}
/** Drops undefined so an absent optional is omitted rather than emitted as null. */
const compact = <T extends Record<string, unknown>>(o: T): T => {
  for (const k of Object.keys(o)) if (o[k] === undefined) delete o[k]
  return o
}

// A per-conversation, server-assigned, strictly monotonic message ordinal, starting at
// 1. Dense within a conversation for every member: if you can see seq 4 and seq 6 you
// may assume seq 5 exists and you are missing it. This is the number the UI reasons
// about — ordering, `first_unread_seq`, the "N NEW" rule.
//
// UPPER BOUND IS DELIBERATE. The natural type is int64 and Postgres will hand out
// int64, but JSON numbers land in a JavaScript double, which stops being an exact
// integer past 2^53-1 (9007199254740991). A seq above that silently rounds in the web
// client and does not in Dart or Go — the same class of bug as the non-portable
// waveform seed already recorded on IDEA-23, and far more dangerous because it
// corrupts ordering rather than pixels. 2^53 messages in one conversation is not
// reachable, so the bound costs nothing and removes the failure mode. Anything that
// could genuinely exceed it must be a string, not a number.
export type Seq = number
const asSeq = (v: unknown, p: string): Seq => {
  const x = asInt(v, p)
  if (x < 1) bad(p, `Seq must be >= 1, got ${x}`)
  if (x > 9007199254740991) bad(p, `Seq must be <= 9007199254740991, got ${x}`)
  return x
}

// A SERVER-GLOBAL, server-assigned, strictly monotonic cursor over the whole message
// log. Each account reads it as a cursor into the subset it is allowed to see. This is
// the ONLY thing `GET /sync?after=N` takes.
//
// WHY THIS EXISTS, since IDEA-23 named only one sequence: `seq` is scoped per
// conversation, so a single scalar `after=N` is not well-defined across conversations,
// and a client returning from six hours offline needs to catch up on all of them —
// including conversations it has never seen, which by definition are absent from any
// per-conversation cursor map it holds. A cursor vector solves the first problem and
// not the second, and it grows without bound. One global ordinal solves both and stays
// a single integer per device.
//
// ONE COUNTER, NOT ONE PER ACCOUNT. An earlier draft of this text said
// “account-global”, which reads as a per-account counter and contradicts the sparsity
// rule below — a per-account counter would be dense. Ratified as path A on IDEA-27,
// 2026-08-17.
//
// Gaps are normal and carry no information: an account observes only the rows of
// conversations it belongs to, so its observed log_seq values are sparse. Never treat
// a gap as a missing message. `seq` is dense, `log_seq` is not; that is the whole
// division of labour between them.
//
// NORMATIVE — WITHIN ONE CONVERSATION, ASCENDING `log_seq` IMPLIES ASCENDING `seq`. A
// client discovers messages by `log_seq` and orders them by `seq`; if the two ever
// disagree it applies a conversation out of order. This forbids the obvious
// implementation. A `bigserial` assigns at INSERT, not at COMMIT, so a transaction
// holding 100 can commit after one holding 101, and a reader whose cursor has already
// passed 101 never sees 100 again — silent, permanent loss of a delivered message.
// Assign both ordinals from a single-row counter with `UPDATE … RETURNING` inside the
// inserting transaction, so the row lock makes assignment order and commit order the
// same order.
//
// 0 means “I have nothing”, so a fresh device syncs with `after=0`.
export type LogSeq = number
const asLogSeq = (v: unknown, p: string): LogSeq => {
  const x = asInt(v, p)
  if (x < 0) bad(p, `LogSeq must be >= 0, got ${x}`)
  if (x > 9007199254740991) bad(p, `LogSeq must be <= 9007199254740991, got ${x}`)
  return x
}

// Lowercase hyphenated UUID. Lowercase is normative: these are compared as strings in
// caches and idempotency keys, and a client that sends uppercase would defeat
// deduplication.
export type Uuid = string
const UuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
const asUuid = (v: unknown, p: string): Uuid => {
  const x = asStr(v, p)
  if (!UuidPattern.test(x)) bad(p, `Uuid must match ^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$, got ${JSON.stringify(x)}`)
  return x
}

// RFC 3339, UTC, exactly three fractional digits, literal Z. The pattern is enforced
// rather than advisory because `2026-08-17T04:22:01Z` and
// `2026-08-17T04:22:01.193000Z` both parse fine in all three languages and then sort
// differently as strings.
export type Timestamp = string
const TimestampPattern = /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$/
const asTimestamp = (v: unknown, p: string): Timestamp => {
  const x = asStr(v, p)
  if (!TimestampPattern.test(x)) bad(p, `Timestamp must match ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$, got ${JSON.stringify(x)}`)
  return x
}

// D4: everything is a conversation. `direct` is two members, `group` is any number;
// neither is a distinct entity. Adding a third member to a direct conversation
// promotes it to `group` and is a row insert plus this field changing, never a
// migration.
export type ConversationKind = "direct" | "group"
export const ConversationKindValues = ["direct", "group"] as const
const asConversationKind = (v: unknown, p: string): ConversationKind => asOneOf(v, ConversationKindValues, p)

// The server-authoritative half of the message lifecycle from IDEA-23, and the ONLY
// states that ever cross the wire.
//
// The full ladder the UI renders is `sending → sent → delivered → read`, plus `queued`
// and `failed`. `sending`, `queued` and `failed` are client-local: they describe a
// message's relationship to its own outbox, the server has no opinion on them, and a
// client that put them in a frame would be reporting its own state as fact. Keeping
// them out of this enum is what stops that being expressible.
//
// Generated clients therefore get this enum for decoding and are expected to widen it
// locally — see the `ClientDeliveryState` note in the generated output.
export type DeliveryState = "sent" | "delivered" | "read"
export const DeliveryStateValues = ["sent", "delivered", "read"] as const
const asDeliveryState = (v: unknown, p: string): DeliveryState => asOneOf(v, DeliveryStateValues, p)

// Lifecycle of the async Whisper job (R3/IDEA-26) that writes the finished transcript
// back onto the ATTACHMENT it belongs to.
//
// AN EARLIER DRAFT OF THIS TEXT SAID the job writes `transcript_text` back onto the
// MESSAGE row. That was wrong and it named the wrong write target. `Transcript` hangs
// off `VoiceAttachment`, and a message may carry more than one voice note —
// `Message.attachments` has no `maxItems` — so a per-message column cannot hold two
// transcripts, and the second job to finish would silently replace the first while
// `word_count` and `segments`, which are per attachment, went on describing different
// audio. Corrected on CANT-13, 2026-09-02, when the initial schema settled where each
// field lives: the authoritative copy is per attachment, and the server keeps a
// denormalised copy of the text on the message row for CANT-59's single full-text
// index, which nothing reads to serve a message.
export type TranscriptState = "pending" | "ready" | "failed"
export const TranscriptStateValues = ["pending", "ready", "failed"] as const
const asTranscriptState = (v: unknown, p: string): TranscriptState => asOneOf(v, TranscriptStateValues, p)

// A duration or offset in whole milliseconds.
//
// NO FLOATING-POINT NUMBER APPEARS ANYWHERE IN THIS PROTOCOL, and this type is why.
// JSON has a single number type; Dart has two. A `number`-typed field holding `0`
// decodes to Dart `double 0.0` and re-encodes as `0.0`, which is not what TypeScript
// or Go emit for the same field — so a value that survived a round trip in two
// languages would not survive it in the third, and the conformance vectors would fail
// on a difference with no bug behind it. Worse, the workaround (compare numbers
// loosely) would have hidden real precision differences too.
//
// Milliseconds as integers are exact everywhere, sort correctly, and are what a media
// pipeline already works in. The cost is that clients format for display, which they
// were doing anyway — nothing renders a raw duration.
export type DurationMs = number
const asDurationMs = (v: unknown, p: string): DurationMs => {
  const x = asInt(v, p)
  if (x < 0) bad(p, `DurationMs must be >= 0, got ${x}`)
  if (x > 9007199254740991) bad(p, `DurationMs must be <= 9007199254740991, got ${x}`)
  return x
}

// The four source types the canvas's replies section renders.
export type ReplyRefKind = "text" | "voice" | "image" | "link"
export const ReplyRefKindValues = ["text", "voice", "image", "link"] as const
const asReplyRefKind = (v: unknown, p: string): ReplyRefKind => asOneOf(v, ReplyRefKindValues, p)

export type TypingState = "start" | "stop"
export const TypingStateValues = ["start", "stop"] as const
const asTypingState = (v: unknown, p: string): TypingState => asOneOf(v, TypingStateValues, p)

export type ErrorCode = "unauthorized" | "wire_version_unsupported" | "rate_limited" | "not_a_member" | "conversation_not_found" | "message_too_large" | "upload_not_found" | "internal"
export const ErrorCodeValues = ["unauthorized", "wire_version_unsupported", "rate_limited", "not_a_member", "conversation_not_found", "message_too_large", "upload_not_found", "internal"] as const
const asErrorCode = (v: unknown, p: string): ErrorCode => asOneOf(v, ErrorCodeValues, p)

export interface User {
  id: Uuid
  // Display name. The typing-indicator naming rule uses the first whitespace-separated
  // token of this as the first name.
  name: string
  // Two letters for the avatar tile. Server-derived so web and Flutter cannot disagree
  // about how to abbreviate a name — deriving this client-side is a two-implementation
  // problem for zero benefit.
  initials?: string
}

export function decodeUser(v: unknown, p = "User"): User {
  const o = asObj(v, p)
  return {
    id: o["id"] === undefined || o["id"] === null ? bad(`${p}.id`, 'required field is missing') : asUuid(o["id"], `${p}.id`),
    name: o["name"] === undefined || o["name"] === null ? bad(`${p}.name`, 'required field is missing') : asStr(o["name"], `${p}.name`),
    initials: o["initials"] === undefined || o["initials"] === null ? undefined : asStr(o["initials"], `${p}.initials`),
  }
}

export function encodeUser(v: User): Record<string, unknown> {
  return compact({
    "id": v.id,
    "name": v.name,
    "initials": v.initials === undefined ? undefined : v.initials,
  })
}

// One timed span of transcript, used by search's JUMP TO and by playback highlighting.
export interface TranscriptSegment {
  // Offset from the start of the clip.
  atMs: DurationMs
  text: string
}

export function decodeTranscriptSegment(v: unknown, p = "TranscriptSegment"): TranscriptSegment {
  const o = asObj(v, p)
  return {
    atMs: o["at_ms"] === undefined || o["at_ms"] === null ? bad(`${p}.at_ms`, 'required field is missing') : asDurationMs(o["at_ms"], `${p}.at_ms`),
    text: o["text"] === undefined || o["text"] === null ? bad(`${p}.text`, 'required field is missing') : asStr(o["text"], `${p}.text`),
  }
}

export function encodeTranscriptSegment(v: TranscriptSegment): Record<string, unknown> {
  return compact({
    "at_ms": v.atMs,
    "text": v.text,
  })
}

export interface Transcript {
  state: TranscriptState
  // Present when state is `ready`.
  text?: string
  // Server-computed. The web client currently derives this from the text on screen so
  // the count cannot lie; that stays true — a client SHOULD prefer its own count of the
  // text it is actually rendering and treat this as a hint for collapsed state.
  wordCount?: number
  segments?: TranscriptSegment[]
  // e.g. `whisper.cpp/small.en`. Recorded because a transcript's quality is not
  // interpretable without knowing what produced it, and R3 expects the model to be a
  // tunable knob.
  engine?: string
  // BCP 47.
  language?: string
  // Server's own estimate of remaining work, shown while `pending`. A client must not
  // invent one.
  etaSec?: number
}

export function decodeTranscript(v: unknown, p = "Transcript"): Transcript {
  const o = asObj(v, p)
  return {
    state: o["state"] === undefined || o["state"] === null ? bad(`${p}.state`, 'required field is missing') : asTranscriptState(o["state"], `${p}.state`),
    text: o["text"] === undefined || o["text"] === null ? undefined : asStr(o["text"], `${p}.text`),
    wordCount: o["word_count"] === undefined || o["word_count"] === null ? undefined : asInt(o["word_count"], `${p}.word_count`),
    segments: o["segments"] === undefined || o["segments"] === null ? undefined : asArray(o["segments"], `${p}.segments`).map((x, i) => decodeTranscriptSegment(x, `${p}.segments[${i}]`)),
    engine: o["engine"] === undefined || o["engine"] === null ? undefined : asStr(o["engine"], `${p}.engine`),
    language: o["language"] === undefined || o["language"] === null ? undefined : asStr(o["language"], `${p}.language`),
    etaSec: o["eta_sec"] === undefined || o["eta_sec"] === null ? undefined : asInt(o["eta_sec"], `${p}.eta_sec`),
  }
}

export function encodeTranscript(v: Transcript): Record<string, unknown> {
  return compact({
    "state": v.state,
    "text": v.text === undefined ? undefined : v.text,
    "word_count": v.wordCount === undefined ? undefined : v.wordCount,
    "segments": v.segments === undefined ? undefined : v.segments.map((x) => encodeTranscriptSegment(x)),
    "engine": v.engine === undefined ? undefined : v.engine,
    "language": v.language === undefined ? undefined : v.language,
    "eta_sec": v.etaSec === undefined ? undefined : v.etaSec,
  })
}

export interface VoiceAttachment {
  readonly kind: "voice"
  // Media URL. Opaque to the client; may be presigned and time-limited.
  url: string
  durationMs: DurationMs
  // Amplitude peaks 0–100, computed server-side ONCE and stored with the message.
  // Deliberate call 13 of the design canvas: the bar pattern must be identical in web
  // and Flutter, which is only true if both render the same stored array.
  //
  // This is not a style preference. The canvas's own seeded generator multiplies by
  // 1103515245, which for seeds near 2^31 exceeds 2^53 — so JavaScript rounds where
  // Dart's 64-bit int does not, and the identical seed yields different bars. Any
  // client-side generation reintroduces that. Clients render this array and never
  // synthesise one.
  peaks: number[]
  transcript: Transcript
}

export function decodeVoiceAttachment(v: unknown, p = "VoiceAttachment"): VoiceAttachment {
  const o = asObj(v, p)
  return {
    kind: "voice",
    url: o["url"] === undefined || o["url"] === null ? bad(`${p}.url`, 'required field is missing') : asStr(o["url"], `${p}.url`),
    durationMs: o["duration_ms"] === undefined || o["duration_ms"] === null ? bad(`${p}.duration_ms`, 'required field is missing') : asDurationMs(o["duration_ms"], `${p}.duration_ms`),
    peaks: o["peaks"] === undefined || o["peaks"] === null ? bad(`${p}.peaks`, 'required field is missing') : asArray(o["peaks"], `${p}.peaks`).map((x, i) => asInt(x, `${p}.peaks[${i}]`)),
    transcript: o["transcript"] === undefined || o["transcript"] === null ? bad(`${p}.transcript`, 'required field is missing') : decodeTranscript(o["transcript"], `${p}.transcript`),
  }
}

export function encodeVoiceAttachment(v: VoiceAttachment): Record<string, unknown> {
  return compact({
    "kind": "voice",
    "url": v.url,
    "duration_ms": v.durationMs,
    "peaks": v.peaks.map((x) => x),
    "transcript": encodeTranscript(v.transcript),
  })
}

export interface ImageAttachment {
  readonly kind: "image"
  url: string
  filename: string
  // Stored intrinsic width. The client caps the rendered box at 340x400 from this ratio,
  // so the row height is final before a byte of image data arrives — which is the
  // property the blurhash placeholder exists to protect.
  width: number
  height: number
  bytes: number
  // Blurhash. Absent means render the plane flat; never means block on the image.
  placeholder?: string
}

export function decodeImageAttachment(v: unknown, p = "ImageAttachment"): ImageAttachment {
  const o = asObj(v, p)
  return {
    kind: "image",
    url: o["url"] === undefined || o["url"] === null ? bad(`${p}.url`, 'required field is missing') : asStr(o["url"], `${p}.url`),
    filename: o["filename"] === undefined || o["filename"] === null ? bad(`${p}.filename`, 'required field is missing') : asStr(o["filename"], `${p}.filename`),
    width: o["width"] === undefined || o["width"] === null ? bad(`${p}.width`, 'required field is missing') : asInt(o["width"], `${p}.width`),
    height: o["height"] === undefined || o["height"] === null ? bad(`${p}.height`, 'required field is missing') : asInt(o["height"], `${p}.height`),
    bytes: o["bytes"] === undefined || o["bytes"] === null ? bad(`${p}.bytes`, 'required field is missing') : asInt(o["bytes"], `${p}.bytes`),
    placeholder: o["placeholder"] === undefined || o["placeholder"] === null ? undefined : asStr(o["placeholder"], `${p}.placeholder`),
  }
}

export function encodeImageAttachment(v: ImageAttachment): Record<string, unknown> {
  return compact({
    "kind": "image",
    "url": v.url,
    "filename": v.filename,
    "width": v.width,
    "height": v.height,
    "bytes": v.bytes,
    "placeholder": v.placeholder === undefined ? undefined : v.placeholder,
  })
}

export interface ReplyRef {
  messageId: Uuid
  authorId: Uuid
  kind: ReplyRefKind
  // One line, rendered from the LIVE source message server-side, not frozen at send
  // time. That is what lets a reply to a voice note back-fill its preview when the
  // transcript lands.
  preview: string
  durationMs?: DurationMs
  url?: string
}

export function decodeReplyRef(v: unknown, p = "ReplyRef"): ReplyRef {
  const o = asObj(v, p)
  return {
    messageId: o["message_id"] === undefined || o["message_id"] === null ? bad(`${p}.message_id`, 'required field is missing') : asUuid(o["message_id"], `${p}.message_id`),
    authorId: o["author_id"] === undefined || o["author_id"] === null ? bad(`${p}.author_id`, 'required field is missing') : asUuid(o["author_id"], `${p}.author_id`),
    kind: o["kind"] === undefined || o["kind"] === null ? bad(`${p}.kind`, 'required field is missing') : asReplyRefKind(o["kind"], `${p}.kind`),
    preview: o["preview"] === undefined || o["preview"] === null ? bad(`${p}.preview`, 'required field is missing') : asStr(o["preview"], `${p}.preview`),
    durationMs: o["duration_ms"] === undefined || o["duration_ms"] === null ? undefined : asDurationMs(o["duration_ms"], `${p}.duration_ms`),
    url: o["url"] === undefined || o["url"] === null ? undefined : asStr(o["url"], `${p}.url`),
  }
}

export function encodeReplyRef(v: ReplyRef): Record<string, unknown> {
  return compact({
    "message_id": v.messageId,
    "author_id": v.authorId,
    "kind": v.kind,
    "preview": v.preview,
    "duration_ms": v.durationMs === undefined ? undefined : v.durationMs,
    "url": v.url === undefined ? undefined : v.url,
  })
}

export interface Message {
  id: Uuid
  seq: Seq
  logSeq: LogSeq
  conversationId: Uuid
  authorId: Uuid
  at: Timestamp
  text?: string
  attachments?: Attachment[]
  replyTo?: ReplyRef
  state: DeliveryState
  // How many members other than the author have read it. Rooms render the fraction
  // against `Conversation.member_count`: READ 5/7.
  readBy?: number
  // Echoed back to the sender only, so a client can match a broadcast message against
  // its own outbox entry when the `ack` and the `message` frame race. Other members
  // never receive it.
  clientId?: Uuid
  editedAt?: Timestamp
  // A tombstone. The row keeps its seq — deleting a message must not renumber the
  // conversation, or every other client's unread arithmetic breaks.
  deleted?: boolean
}

export function decodeMessage(v: unknown, p = "Message"): Message {
  const o = asObj(v, p)
  return {
    id: o["id"] === undefined || o["id"] === null ? bad(`${p}.id`, 'required field is missing') : asUuid(o["id"], `${p}.id`),
    seq: o["seq"] === undefined || o["seq"] === null ? bad(`${p}.seq`, 'required field is missing') : asSeq(o["seq"], `${p}.seq`),
    logSeq: o["log_seq"] === undefined || o["log_seq"] === null ? bad(`${p}.log_seq`, 'required field is missing') : asLogSeq(o["log_seq"], `${p}.log_seq`),
    conversationId: o["conversation_id"] === undefined || o["conversation_id"] === null ? bad(`${p}.conversation_id`, 'required field is missing') : asUuid(o["conversation_id"], `${p}.conversation_id`),
    authorId: o["author_id"] === undefined || o["author_id"] === null ? bad(`${p}.author_id`, 'required field is missing') : asUuid(o["author_id"], `${p}.author_id`),
    at: o["at"] === undefined || o["at"] === null ? bad(`${p}.at`, 'required field is missing') : asTimestamp(o["at"], `${p}.at`),
    text: o["text"] === undefined || o["text"] === null ? undefined : asStr(o["text"], `${p}.text`),
    attachments: o["attachments"] === undefined || o["attachments"] === null ? undefined : asArray(o["attachments"], `${p}.attachments`).map((x, i) => decodeAttachment(x, `${p}.attachments[${i}]`)).filter((x): x is Attachment => x !== null),
    replyTo: o["reply_to"] === undefined || o["reply_to"] === null ? undefined : decodeReplyRef(o["reply_to"], `${p}.reply_to`),
    state: o["state"] === undefined || o["state"] === null ? bad(`${p}.state`, 'required field is missing') : asDeliveryState(o["state"], `${p}.state`),
    readBy: o["read_by"] === undefined || o["read_by"] === null ? undefined : asInt(o["read_by"], `${p}.read_by`),
    clientId: o["client_id"] === undefined || o["client_id"] === null ? undefined : asUuid(o["client_id"], `${p}.client_id`),
    editedAt: o["edited_at"] === undefined || o["edited_at"] === null ? undefined : asTimestamp(o["edited_at"], `${p}.edited_at`),
    deleted: o["deleted"] === undefined || o["deleted"] === null ? undefined : asBool(o["deleted"], `${p}.deleted`),
  }
}

export function encodeMessage(v: Message): Record<string, unknown> {
  return compact({
    "id": v.id,
    "seq": v.seq,
    "log_seq": v.logSeq,
    "conversation_id": v.conversationId,
    "author_id": v.authorId,
    "at": v.at,
    "text": v.text === undefined ? undefined : v.text,
    "attachments": v.attachments === undefined ? undefined : v.attachments.map((x) => encodeAttachment(x)),
    "reply_to": v.replyTo === undefined ? undefined : encodeReplyRef(v.replyTo),
    "state": v.state,
    "read_by": v.readBy === undefined ? undefined : v.readBy,
    "client_id": v.clientId === undefined ? undefined : v.clientId,
    "edited_at": v.editedAt === undefined ? undefined : v.editedAt,
    "deleted": v.deleted === undefined ? undefined : v.deleted,
  })
}

export interface Conversation {
  id: Uuid
  kind: ConversationKind
  name: string
  memberCount: number
  muted?: boolean
  // The first seq the reader has not seen; absent means fully read. Both the rail's
  // badge and the thread's "N NEW" rule derive from this. There is deliberately NO
  // stored unread count on the wire, because a count and a marker can disagree and this
  // one does: your own messages are never unread, so a count computed anywhere but from
  // this marker gets it wrong. Absent-means-read also keeps a resync that delivers out
  // of order from producing a wrong badge.
  firstUnreadSeq?: Seq
  // Newest seq the server holds for this conversation. What a resync counts towards, and
  // what the numeric resync progress is a fraction of.
  headSeq: Seq
  // Per-conversation override of the global infinite retention. Absent means keep
  // forever.
  retentionDays?: number
}

export function decodeConversation(v: unknown, p = "Conversation"): Conversation {
  const o = asObj(v, p)
  return {
    id: o["id"] === undefined || o["id"] === null ? bad(`${p}.id`, 'required field is missing') : asUuid(o["id"], `${p}.id`),
    kind: o["kind"] === undefined || o["kind"] === null ? bad(`${p}.kind`, 'required field is missing') : asConversationKind(o["kind"], `${p}.kind`),
    name: o["name"] === undefined || o["name"] === null ? bad(`${p}.name`, 'required field is missing') : asStr(o["name"], `${p}.name`),
    memberCount: o["member_count"] === undefined || o["member_count"] === null ? bad(`${p}.member_count`, 'required field is missing') : asInt(o["member_count"], `${p}.member_count`),
    muted: o["muted"] === undefined || o["muted"] === null ? undefined : asBool(o["muted"], `${p}.muted`),
    firstUnreadSeq: o["first_unread_seq"] === undefined || o["first_unread_seq"] === null ? undefined : asSeq(o["first_unread_seq"], `${p}.first_unread_seq`),
    headSeq: o["head_seq"] === undefined || o["head_seq"] === null ? bad(`${p}.head_seq`, 'required field is missing') : asSeq(o["head_seq"], `${p}.head_seq`),
    retentionDays: o["retention_days"] === undefined || o["retention_days"] === null ? undefined : asInt(o["retention_days"], `${p}.retention_days`),
  }
}

export function encodeConversation(v: Conversation): Record<string, unknown> {
  return compact({
    "id": v.id,
    "kind": v.kind,
    "name": v.name,
    "member_count": v.memberCount,
    "muted": v.muted === undefined ? undefined : v.muted,
    "first_unread_seq": v.firstUnreadSeq === undefined ? undefined : v.firstUnreadSeq,
    "head_seq": v.headSeq,
    "retention_days": v.retentionDays === undefined ? undefined : v.retentionDays,
  })
}

// First frame the client sends after the socket opens. The server answers with `ready`
// or `error`.
export interface ClientHello {
  readonly type: "hello"
  // The version of THIS schema the client was generated from. The server refuses a
  // version it cannot speak with an `error` frame rather than failing halfway through a
  // session.
  wireVersion: number
  // Stable per install. D2 makes the device the unit of credential and of revocation, so
  // it is the unit here too.
  deviceId: Uuid
  // The client's cursor. Absent means "do not stream backlog, I will call /sync myself"
  // — which is the correct choice for a client with a large gap, since a backlog burst
  // over the socket has no progress indication.
  resumeFromLogSeq?: LogSeq
  // Free-form build identifier for logs, e.g. `catenary-web/0.3.1`. Never parsed.
  clientInfo?: string
}

export function decodeClientHello(v: unknown, p = "ClientHello"): ClientHello {
  const o = asObj(v, p)
  return {
    type: "hello",
    wireVersion: o["wire_version"] === undefined || o["wire_version"] === null ? bad(`${p}.wire_version`, 'required field is missing') : asInt(o["wire_version"], `${p}.wire_version`),
    deviceId: o["device_id"] === undefined || o["device_id"] === null ? bad(`${p}.device_id`, 'required field is missing') : asUuid(o["device_id"], `${p}.device_id`),
    resumeFromLogSeq: o["resume_from_log_seq"] === undefined || o["resume_from_log_seq"] === null ? undefined : asLogSeq(o["resume_from_log_seq"], `${p}.resume_from_log_seq`),
    clientInfo: o["client_info"] === undefined || o["client_info"] === null ? undefined : asStr(o["client_info"], `${p}.client_info`),
  }
}

export function encodeClientHello(v: ClientHello): Record<string, unknown> {
  return compact({
    "type": "hello",
    "wire_version": v.wireVersion,
    "device_id": v.deviceId,
    "resume_from_log_seq": v.resumeFromLogSeq === undefined ? undefined : v.resumeFromLogSeq,
    "client_info": v.clientInfo === undefined ? undefined : v.clientInfo,
  })
}

export interface ServerReady {
  readonly type: "ready"
  sessionId: Uuid
  // Lets a client with a skewed clock render correct relative timestamps by offset
  // rather than trusting its own clock.
  serverTime: Timestamp
  // How often the client must send `ping`. SERVER-DRIVEN ON PURPOSE (R1/IDEA-24): the
  // 30–45 s window sits under Cloudflare's ~100 s idle timeout, and if that constant
  // lived in two clients it would be two constants that drift, with the Flutter one
  // discovered wrong only in the field. Here the number is deployed, not shipped.
  heartbeatIntervalSec: number
  // How many unanswered pings before the client severs deliberately and reconnects. R1
  // sets this at 2. Waiting for the OS to notice a half-open socket is the failure this
  // exists to prevent.
  missedPongLimit: number
  // The server's current head. A client that asked to resume can compare this against
  // its own cursor to decide between streaming and a `/sync` call.
  logSeq: LogSeq
  // True when the server accepted `resume_from_log_seq` and will stream the gap. False
  // means the client must catch up over `/sync` before trusting anything it renders.
  resumed: boolean
}

export function decodeServerReady(v: unknown, p = "ServerReady"): ServerReady {
  const o = asObj(v, p)
  return {
    type: "ready",
    sessionId: o["session_id"] === undefined || o["session_id"] === null ? bad(`${p}.session_id`, 'required field is missing') : asUuid(o["session_id"], `${p}.session_id`),
    serverTime: o["server_time"] === undefined || o["server_time"] === null ? bad(`${p}.server_time`, 'required field is missing') : asTimestamp(o["server_time"], `${p}.server_time`),
    heartbeatIntervalSec: o["heartbeat_interval_sec"] === undefined || o["heartbeat_interval_sec"] === null ? bad(`${p}.heartbeat_interval_sec`, 'required field is missing') : asInt(o["heartbeat_interval_sec"], `${p}.heartbeat_interval_sec`),
    missedPongLimit: o["missed_pong_limit"] === undefined || o["missed_pong_limit"] === null ? bad(`${p}.missed_pong_limit`, 'required field is missing') : asInt(o["missed_pong_limit"], `${p}.missed_pong_limit`),
    logSeq: o["log_seq"] === undefined || o["log_seq"] === null ? bad(`${p}.log_seq`, 'required field is missing') : asLogSeq(o["log_seq"], `${p}.log_seq`),
    resumed: o["resumed"] === undefined || o["resumed"] === null ? bad(`${p}.resumed`, 'required field is missing') : asBool(o["resumed"], `${p}.resumed`),
  }
}

export function encodeServerReady(v: ServerReady): Record<string, unknown> {
  return compact({
    "type": "ready",
    "session_id": v.sessionId,
    "server_time": v.serverTime,
    "heartbeat_interval_sec": v.heartbeatIntervalSec,
    "missed_pong_limit": v.missedPongLimit,
    "log_seq": v.logSeq,
    "resumed": v.resumed,
  })
}

// APPLICATION-level heartbeat, sent in both directions. Deliberately not the WebSocket
// protocol's own ping/pong control frames: those are handled inside the transport
// stack, are not observable from application code in a browser at all, and can be
// answered by an intermediary. R1 requires a heartbeat observable from both ends,
// which means it has to be a data frame.
export interface Ping {
  readonly type: "ping"
  // Echoed verbatim in the matching `pong`. Matching by id rather than by arrival order
  // is what makes a missed pong countable — with unmatched pings you cannot tell a lost
  // pong from a slow one.
  id: string
  at?: Timestamp
}

export function decodePing(v: unknown, p = "Ping"): Ping {
  const o = asObj(v, p)
  return {
    type: "ping",
    id: o["id"] === undefined || o["id"] === null ? bad(`${p}.id`, 'required field is missing') : asStr(o["id"], `${p}.id`),
    at: o["at"] === undefined || o["at"] === null ? undefined : asTimestamp(o["at"], `${p}.at`),
  }
}

export function encodePing(v: Ping): Record<string, unknown> {
  return compact({
    "type": "ping",
    "id": v.id,
    "at": v.at === undefined ? undefined : v.at,
  })
}

export interface Pong {
  readonly type: "pong"
  // The id of the ping being answered, verbatim.
  id: string
  at?: Timestamp
}

export function decodePong(v: unknown, p = "Pong"): Pong {
  const o = asObj(v, p)
  return {
    type: "pong",
    id: o["id"] === undefined || o["id"] === null ? bad(`${p}.id`, 'required field is missing') : asStr(o["id"], `${p}.id`),
    at: o["at"] === undefined || o["at"] === null ? undefined : asTimestamp(o["at"], `${p}.at`),
  }
}

export function encodePong(v: Pong): Record<string, unknown> {
  return compact({
    "type": "pong",
    "id": v.id,
    "at": v.at === undefined ? undefined : v.at,
  })
}

export interface OutboundAttachment {
  kind: "voice" | "image"
  // Handle returned by the presigned-upload flow. The client sends this, never the
  // media: dimensions, duration, peaks, EXIF stripping and the blurhash are all computed
  // server-side on ingest, so the client cannot report them and cannot get them wrong.
  uploadId: Uuid
}

export function decodeOutboundAttachment(v: unknown, p = "OutboundAttachment"): OutboundAttachment {
  const o = asObj(v, p)
  return {
    kind: o["kind"] === undefined || o["kind"] === null ? bad(`${p}.kind`, 'required field is missing') : asOneOf(o["kind"], ["voice","image"], `${p}.kind`),
    uploadId: o["upload_id"] === undefined || o["upload_id"] === null ? bad(`${p}.upload_id`, 'required field is missing') : asUuid(o["upload_id"], `${p}.upload_id`),
  }
}

export function encodeOutboundAttachment(v: OutboundAttachment): Record<string, unknown> {
  return compact({
    "kind": v.kind,
    "upload_id": v.uploadId,
  })
}

export interface ClientSend {
  readonly type: "send"
  // The idempotency key, generated by the client once per logical message and REUSED
  // verbatim on every retry. Same pattern as Switchyard. The server deduplicates on
  // (account, client_id) and answers a duplicate with the original `ack`, so a retry
  // after an ambiguous failure is free and safe — which is what makes an offline outbox
  // correct rather than hopeful.
  //
  // A client that mints a fresh key on retry has re-implemented double-sending.
  clientId: Uuid
  conversationId: Uuid
  text?: string
  attachments?: OutboundAttachment[]
  // Just the id — the server builds the whole ReplyRef from the live source message, so
  // the preview is never a stale copy taken at send time.
  replyToMessageId?: Uuid
}

export function decodeClientSend(v: unknown, p = "ClientSend"): ClientSend {
  const o = asObj(v, p)
  return {
    type: "send",
    clientId: o["client_id"] === undefined || o["client_id"] === null ? bad(`${p}.client_id`, 'required field is missing') : asUuid(o["client_id"], `${p}.client_id`),
    conversationId: o["conversation_id"] === undefined || o["conversation_id"] === null ? bad(`${p}.conversation_id`, 'required field is missing') : asUuid(o["conversation_id"], `${p}.conversation_id`),
    text: o["text"] === undefined || o["text"] === null ? undefined : asStr(o["text"], `${p}.text`),
    attachments: o["attachments"] === undefined || o["attachments"] === null ? undefined : asArray(o["attachments"], `${p}.attachments`).map((x, i) => decodeOutboundAttachment(x, `${p}.attachments[${i}]`)),
    replyToMessageId: o["reply_to_message_id"] === undefined || o["reply_to_message_id"] === null ? undefined : asUuid(o["reply_to_message_id"], `${p}.reply_to_message_id`),
  }
}

export function encodeClientSend(v: ClientSend): Record<string, unknown> {
  return compact({
    "type": "send",
    "client_id": v.clientId,
    "conversation_id": v.conversationId,
    "text": v.text === undefined ? undefined : v.text,
    "attachments": v.attachments === undefined ? undefined : v.attachments.map((x) => encodeOutboundAttachment(x)),
    "reply_to_message_id": v.replyToMessageId === undefined ? undefined : v.replyToMessageId,
  })
}

// The other half of the send/ack pair. Receiving this moves the outbox entry from
// `sending` to `sent` and fixes its seq.
export interface ServerAck {
  readonly type: "ack"
  // The key from the `send` this answers.
  clientId: Uuid
  messageId: Uuid
  conversationId: Uuid
  seq: Seq
  logSeq: LogSeq
  // The server's timestamp for the message. Authoritative — the client's own send time
  // is never persisted, so two devices with different clocks cannot order a conversation
  // differently.
  at: Timestamp
  // True when this ack replays an earlier send with the same `client_id`. The client
  // treats it identically; it exists so the deduplication path is observable in logs and
  // in R1's zero-duplication test rather than being invisibly correct.
  duplicate?: boolean
}

export function decodeServerAck(v: unknown, p = "ServerAck"): ServerAck {
  const o = asObj(v, p)
  return {
    type: "ack",
    clientId: o["client_id"] === undefined || o["client_id"] === null ? bad(`${p}.client_id`, 'required field is missing') : asUuid(o["client_id"], `${p}.client_id`),
    messageId: o["message_id"] === undefined || o["message_id"] === null ? bad(`${p}.message_id`, 'required field is missing') : asUuid(o["message_id"], `${p}.message_id`),
    conversationId: o["conversation_id"] === undefined || o["conversation_id"] === null ? bad(`${p}.conversation_id`, 'required field is missing') : asUuid(o["conversation_id"], `${p}.conversation_id`),
    seq: o["seq"] === undefined || o["seq"] === null ? bad(`${p}.seq`, 'required field is missing') : asSeq(o["seq"], `${p}.seq`),
    logSeq: o["log_seq"] === undefined || o["log_seq"] === null ? bad(`${p}.log_seq`, 'required field is missing') : asLogSeq(o["log_seq"], `${p}.log_seq`),
    at: o["at"] === undefined || o["at"] === null ? bad(`${p}.at`, 'required field is missing') : asTimestamp(o["at"], `${p}.at`),
    duplicate: o["duplicate"] === undefined || o["duplicate"] === null ? undefined : asBool(o["duplicate"], `${p}.duplicate`),
  }
}

export function encodeServerAck(v: ServerAck): Record<string, unknown> {
  return compact({
    "type": "ack",
    "client_id": v.clientId,
    "message_id": v.messageId,
    "conversation_id": v.conversationId,
    "seq": v.seq,
    "log_seq": v.logSeq,
    "at": v.at,
    "duplicate": v.duplicate === undefined ? undefined : v.duplicate,
  })
}

// Carries the WHOLE message, deliberately. IDEA-23 is explicit that this must not
// degrade into a ping that makes the client go and fetch: that doubles latency on
// every message in the steady state. The internal Postgres NOTIFY payload between
// server instances is the thing that carries only (conversation_id, seq) under its
// 8000-byte cap — that is a different layer and never appears on this wire.
export interface ServerMessageFrame {
  readonly type: "message"
  message: Message
}

export function decodeServerMessageFrame(v: unknown, p = "ServerMessageFrame"): ServerMessageFrame {
  const o = asObj(v, p)
  return {
    type: "message",
    message: o["message"] === undefined || o["message"] === null ? bad(`${p}.message`, 'required field is missing') : decodeMessage(o["message"], `${p}.message`),
  }
}

export function encodeServerMessageFrame(v: ServerMessageFrame): Record<string, unknown> {
  return compact({
    "type": "message",
    "message": encodeMessage(v.message),
  })
}

export interface ServerReceipt {
  readonly type: "receipt"
  conversationId: Uuid
  userId: Uuid
  // Read receipts are a high-water mark, never per-message. One frame settles an
  // arbitrary backlog and they cannot arrive out of order in a way that matters, since a
  // lower mark than one already held is simply discarded.
  upToSeq: Seq
}

export function decodeServerReceipt(v: unknown, p = "ServerReceipt"): ServerReceipt {
  const o = asObj(v, p)
  return {
    type: "receipt",
    conversationId: o["conversation_id"] === undefined || o["conversation_id"] === null ? bad(`${p}.conversation_id`, 'required field is missing') : asUuid(o["conversation_id"], `${p}.conversation_id`),
    userId: o["user_id"] === undefined || o["user_id"] === null ? bad(`${p}.user_id`, 'required field is missing') : asUuid(o["user_id"], `${p}.user_id`),
    upToSeq: o["up_to_seq"] === undefined || o["up_to_seq"] === null ? bad(`${p}.up_to_seq`, 'required field is missing') : asSeq(o["up_to_seq"], `${p}.up_to_seq`),
  }
}

export function encodeServerReceipt(v: ServerReceipt): Record<string, unknown> {
  return compact({
    "type": "receipt",
    "conversation_id": v.conversationId,
    "user_id": v.userId,
    "up_to_seq": v.upToSeq,
  })
}

export interface ClientRead {
  readonly type: "read"
  conversationId: Uuid
  upToSeq: Seq
}

export function decodeClientRead(v: unknown, p = "ClientRead"): ClientRead {
  const o = asObj(v, p)
  return {
    type: "read",
    conversationId: o["conversation_id"] === undefined || o["conversation_id"] === null ? bad(`${p}.conversation_id`, 'required field is missing') : asUuid(o["conversation_id"], `${p}.conversation_id`),
    upToSeq: o["up_to_seq"] === undefined || o["up_to_seq"] === null ? bad(`${p}.up_to_seq`, 'required field is missing') : asSeq(o["up_to_seq"], `${p}.up_to_seq`),
  }
}

export function encodeClientRead(v: ClientRead): Record<string, unknown> {
  return compact({
    "type": "read",
    "conversation_id": v.conversationId,
    "up_to_seq": v.upToSeq,
  })
}

export interface ClientTyping {
  readonly type: "typing"
  conversationId: Uuid
  state: TypingState
}

export function decodeClientTyping(v: unknown, p = "ClientTyping"): ClientTyping {
  const o = asObj(v, p)
  return {
    type: "typing",
    conversationId: o["conversation_id"] === undefined || o["conversation_id"] === null ? bad(`${p}.conversation_id`, 'required field is missing') : asUuid(o["conversation_id"], `${p}.conversation_id`),
    state: o["state"] === undefined || o["state"] === null ? bad(`${p}.state`, 'required field is missing') : asTypingState(o["state"], `${p}.state`),
  }
}

export function encodeClientTyping(v: ClientTyping): Record<string, unknown> {
  return compact({
    "type": "typing",
    "conversation_id": v.conversationId,
    "state": v.state,
  })
}

export interface ServerTyping {
  readonly type: "typing"
  conversationId: Uuid
  // Who is currently typing, IN THE ORDER THEY STARTED. The order is normative because
  // spec card F's naming rule depends on it: one person is a first name, two or three
  // are comma-separated in start order, four or more collapse to "Several people"
  // because past three the list churns faster than it can be read. Sorting this list in
  // a client silently changes the rendered string, which is exactly the kind of
  // divergence R4 exists to prevent — the rule is shared, so its input has to be too.
  userIds: Uuid[]
}

export function decodeServerTyping(v: unknown, p = "ServerTyping"): ServerTyping {
  const o = asObj(v, p)
  return {
    type: "typing",
    conversationId: o["conversation_id"] === undefined || o["conversation_id"] === null ? bad(`${p}.conversation_id`, 'required field is missing') : asUuid(o["conversation_id"], `${p}.conversation_id`),
    userIds: o["user_ids"] === undefined || o["user_ids"] === null ? bad(`${p}.user_ids`, 'required field is missing') : asArray(o["user_ids"], `${p}.user_ids`).map((x, i) => asUuid(x, `${p}.user_ids[${i}]`)),
  }
}

export function encodeServerTyping(v: ServerTyping): Record<string, unknown> {
  return compact({
    "type": "typing",
    "conversation_id": v.conversationId,
    "user_ids": v.userIds.map((x) => x),
  })
}

export interface ServerError {
  readonly type: "error"
  code: ErrorCode
  // Human-readable, for logs and for the inline error on a failed send. The canvas shows
  // send failures inline on the message, never as a toast.
  message: string
  // Whether re-sending the identical frame could succeed. This is the server's
  // judgement, not the client's guess: `rate_limited` is retryable, `not_a_member` is
  // not, and a client that retried the latter forever would look exactly like a client
  // with no error handling.
  retryable: boolean
  // Present when the error is attributable to a specific `send`, so the right outbox
  // entry goes to `failed` instead of all of them.
  clientId?: Uuid
  retryAfterSec?: number
}

export function decodeServerError(v: unknown, p = "ServerError"): ServerError {
  const o = asObj(v, p)
  return {
    type: "error",
    code: o["code"] === undefined || o["code"] === null ? bad(`${p}.code`, 'required field is missing') : asErrorCode(o["code"], `${p}.code`),
    message: o["message"] === undefined || o["message"] === null ? bad(`${p}.message`, 'required field is missing') : asStr(o["message"], `${p}.message`),
    retryable: o["retryable"] === undefined || o["retryable"] === null ? bad(`${p}.retryable`, 'required field is missing') : asBool(o["retryable"], `${p}.retryable`),
    clientId: o["client_id"] === undefined || o["client_id"] === null ? undefined : asUuid(o["client_id"], `${p}.client_id`),
    retryAfterSec: o["retry_after_sec"] === undefined || o["retry_after_sec"] === null ? undefined : asInt(o["retry_after_sec"], `${p}.retry_after_sec`),
  }
}

export function encodeServerError(v: ServerError): Record<string, unknown> {
  return compact({
    "type": "error",
    "code": v.code,
    "message": v.message,
    "retryable": v.retryable,
    "client_id": v.clientId === undefined ? undefined : v.clientId,
    "retry_after_sec": v.retryAfterSec === undefined ? undefined : v.retryAfterSec,
  })
}

// The server cannot stream the requested gap and the client must catch up over `/sync`
// instead. Naming this as its own frame rather than letting the client infer it from a
// short stream is deliberate: silent partial resume is how a client ends up
// confidently missing messages.
export interface ServerResyncRequired {
  readonly type: "resync_required"
  reason: "cursor_too_old" | "membership_changed" | "retention_purge"
  // The server's current head, so the client knows what it is syncing towards.
  logSeq: LogSeq
}

export function decodeServerResyncRequired(v: unknown, p = "ServerResyncRequired"): ServerResyncRequired {
  const o = asObj(v, p)
  return {
    type: "resync_required",
    reason: o["reason"] === undefined || o["reason"] === null ? bad(`${p}.reason`, 'required field is missing') : asOneOf(o["reason"], ["cursor_too_old","membership_changed","retention_purge"], `${p}.reason`),
    logSeq: o["log_seq"] === undefined || o["log_seq"] === null ? bad(`${p}.log_seq`, 'required field is missing') : asLogSeq(o["log_seq"], `${p}.log_seq`),
  }
}

export function encodeServerResyncRequired(v: ServerResyncRequired): Record<string, unknown> {
  return compact({
    "type": "resync_required",
    "reason": v.reason,
    "log_seq": v.logSeq,
  })
}

// Response to `GET /sync?after=<log_seq>&limit=<n>`. The RECONNECT AND CATCH-UP path,
// not the steady state — steady state is a `message` frame over the socket. Applying a
// page is idempotent: every message carries its own seq and id, so replaying an
// overlapping page cannot duplicate anything, which is what makes reconnection a query
// rather than a guess.
export interface SyncResponse {
  // The cursor to send on the NEXT call. Always the caller's new high-water mark — not
  // the server's head, which may be further along when `has_more` is true. Deriving this
  // client-side by maxing over `messages` is wrong the moment a page contains no
  // messages the client can see.
  logSeq: LogSeq
  // Ascending by log_seq. The ordering is normative so a client can apply the page as a
  // stream and stop anywhere without leaving a hole behind its cursor.
  messages: Message[]
  // Every conversation touched by this page, plus any whose metadata changed — head_seq,
  // first_unread_seq, membership. A client learns about a brand-new conversation here,
  // which is why the sync cursor cannot be a per-conversation map.
  conversations: Conversation[]
  // Every user referenced by this page — as a message author, a reply's author, or a
  // member of a returned conversation.
  //
  // Without this there is NO path from `author_id` to a display name, and both clients
  // would have to invent one. Messages carry ids rather than embedded authors so a
  // rename lands everywhere at once instead of being frozen into every message ever
  // sent; this array is the other half of that decision, and omitting it would have made
  // the id-only design unusable rather than normalised.
  //
  // Send the full record whenever a user first appears on a page or their name or
  // initials changed. Clients cache by id and treat a later record as authoritative.
  users: User[]
  // Call again with the returned `log_seq`. The resync UI renders numeric progress
  // rather than a spinner, which is only possible because `Conversation.head_seq` says
  // what it is counting towards.
  hasMore: boolean
  serverTime: Timestamp
}

export function decodeSyncResponse(v: unknown, p = "SyncResponse"): SyncResponse {
  const o = asObj(v, p)
  return {
    logSeq: o["log_seq"] === undefined || o["log_seq"] === null ? bad(`${p}.log_seq`, 'required field is missing') : asLogSeq(o["log_seq"], `${p}.log_seq`),
    messages: o["messages"] === undefined || o["messages"] === null ? bad(`${p}.messages`, 'required field is missing') : asArray(o["messages"], `${p}.messages`).map((x, i) => decodeMessage(x, `${p}.messages[${i}]`)),
    conversations: o["conversations"] === undefined || o["conversations"] === null ? bad(`${p}.conversations`, 'required field is missing') : asArray(o["conversations"], `${p}.conversations`).map((x, i) => decodeConversation(x, `${p}.conversations[${i}]`)),
    users: o["users"] === undefined || o["users"] === null ? bad(`${p}.users`, 'required field is missing') : asArray(o["users"], `${p}.users`).map((x, i) => decodeUser(x, `${p}.users[${i}]`)),
    hasMore: o["has_more"] === undefined || o["has_more"] === null ? bad(`${p}.has_more`, 'required field is missing') : asBool(o["has_more"], `${p}.has_more`),
    serverTime: o["server_time"] === undefined || o["server_time"] === null ? bad(`${p}.server_time`, 'required field is missing') : asTimestamp(o["server_time"], `${p}.server_time`),
  }
}

export function encodeSyncResponse(v: SyncResponse): Record<string, unknown> {
  return compact({
    "log_seq": v.logSeq,
    "messages": v.messages.map((x) => encodeMessage(x)),
    "conversations": v.conversations.map((x) => encodeConversation(x)),
    "users": v.users.map((x) => encodeUser(x)),
    "has_more": v.hasMore,
    "server_time": v.serverTime,
  })
}

// Tagged union on `kind`. Decoders MUST ignore an attachment whose `kind` they do not
// know rather than failing the whole message — a client that hard-errors on an unknown
// attachment type cannot be shipped ahead of a server that adds one.
export type Attachment = VoiceAttachment | ImageAttachment

// Decode a Attachment. Returns null for an unrecognised "kind", which callers MUST
// treat as "ignore this frame and carry on" rather than as an error — that is what
// lets the server ship a new frame type before every client understands it.
export function decodeAttachment(v: unknown, p = "Attachment"): Attachment | null {
  const o = asObj(v, p)
  switch (o["kind"]) {
    case "voice": return decodeVoiceAttachment(o, p)
    case "image": return decodeImageAttachment(o, p)
    default: return null
  }
}

export function encodeAttachment(v: Attachment): Record<string, unknown> {
  switch (v.kind) {
    case "voice": return encodeVoiceAttachment(v as VoiceAttachment)
    case "image": return encodeImageAttachment(v as ImageAttachment)
  }
}

// Anything a client may send over the socket. One JSON object per WebSocket text
// frame; never batched, never binary.
export type ClientFrame = ClientHello | Ping | Pong | ClientSend | ClientRead | ClientTyping

// Decode a ClientFrame. Returns null for an unrecognised "type", which callers MUST
// treat as "ignore this frame and carry on" rather than as an error — that is what
// lets the server ship a new frame type before every client understands it.
export function decodeClientFrame(v: unknown, p = "ClientFrame"): ClientFrame | null {
  const o = asObj(v, p)
  switch (o["type"]) {
    case "hello": return decodeClientHello(o, p)
    case "ping": return decodePing(o, p)
    case "pong": return decodePong(o, p)
    case "send": return decodeClientSend(o, p)
    case "read": return decodeClientRead(o, p)
    case "typing": return decodeClientTyping(o, p)
    default: return null
  }
}

export function encodeClientFrame(v: ClientFrame): Record<string, unknown> {
  switch (v.type) {
    case "hello": return encodeClientHello(v as ClientHello)
    case "ping": return encodePing(v as Ping)
    case "pong": return encodePong(v as Pong)
    case "send": return encodeClientSend(v as ClientSend)
    case "read": return encodeClientRead(v as ClientRead)
    case "typing": return encodeClientTyping(v as ClientTyping)
  }
}

// Anything the server may send over the socket.
export type ServerFrame = ServerReady | Ping | Pong | ServerAck | ServerMessageFrame | ServerReceipt | ServerTyping | ServerError | ServerResyncRequired

// Decode a ServerFrame. Returns null for an unrecognised "type", which callers MUST
// treat as "ignore this frame and carry on" rather than as an error — that is what
// lets the server ship a new frame type before every client understands it.
export function decodeServerFrame(v: unknown, p = "ServerFrame"): ServerFrame | null {
  const o = asObj(v, p)
  switch (o["type"]) {
    case "ready": return decodeServerReady(o, p)
    case "ping": return decodePing(o, p)
    case "pong": return decodePong(o, p)
    case "ack": return decodeServerAck(o, p)
    case "message": return decodeServerMessageFrame(o, p)
    case "receipt": return decodeServerReceipt(o, p)
    case "typing": return decodeServerTyping(o, p)
    case "error": return decodeServerError(o, p)
    case "resync_required": return decodeServerResyncRequired(o, p)
    default: return null
  }
}

export function encodeServerFrame(v: ServerFrame): Record<string, unknown> {
  switch (v.type) {
    case "ready": return encodeServerReady(v as ServerReady)
    case "ping": return encodePing(v as Ping)
    case "pong": return encodePong(v as Pong)
    case "ack": return encodeServerAck(v as ServerAck)
    case "message": return encodeServerMessageFrame(v as ServerMessageFrame)
    case "receipt": return encodeServerReceipt(v as ServerReceipt)
    case "typing": return encodeServerTyping(v as ServerTyping)
    case "error": return encodeServerError(v as ServerError)
    case "resync_required": return encodeServerResyncRequired(v as ServerResyncRequired)
  }
}

export interface WireCodec {
  decode(v: unknown): unknown
  encode(v: unknown): unknown
}

export const codecs: Record<string, WireCodec> = {
  User: { decode: (v) => decodeUser(v), encode: (v) => encodeUser(v as User) },
  TranscriptSegment: { decode: (v) => decodeTranscriptSegment(v), encode: (v) => encodeTranscriptSegment(v as TranscriptSegment) },
  Transcript: { decode: (v) => decodeTranscript(v), encode: (v) => encodeTranscript(v as Transcript) },
  VoiceAttachment: { decode: (v) => decodeVoiceAttachment(v), encode: (v) => encodeVoiceAttachment(v as VoiceAttachment) },
  ImageAttachment: { decode: (v) => decodeImageAttachment(v), encode: (v) => encodeImageAttachment(v as ImageAttachment) },
  Attachment: { decode: (v) => decodeAttachment(v), encode: (v) => encodeAttachment(v as Attachment) },
  ReplyRef: { decode: (v) => decodeReplyRef(v), encode: (v) => encodeReplyRef(v as ReplyRef) },
  Message: { decode: (v) => decodeMessage(v), encode: (v) => encodeMessage(v as Message) },
  Conversation: { decode: (v) => decodeConversation(v), encode: (v) => encodeConversation(v as Conversation) },
  ClientHello: { decode: (v) => decodeClientHello(v), encode: (v) => encodeClientHello(v as ClientHello) },
  ServerReady: { decode: (v) => decodeServerReady(v), encode: (v) => encodeServerReady(v as ServerReady) },
  Ping: { decode: (v) => decodePing(v), encode: (v) => encodePing(v as Ping) },
  Pong: { decode: (v) => decodePong(v), encode: (v) => encodePong(v as Pong) },
  OutboundAttachment: { decode: (v) => decodeOutboundAttachment(v), encode: (v) => encodeOutboundAttachment(v as OutboundAttachment) },
  ClientSend: { decode: (v) => decodeClientSend(v), encode: (v) => encodeClientSend(v as ClientSend) },
  ServerAck: { decode: (v) => decodeServerAck(v), encode: (v) => encodeServerAck(v as ServerAck) },
  ServerMessageFrame: { decode: (v) => decodeServerMessageFrame(v), encode: (v) => encodeServerMessageFrame(v as ServerMessageFrame) },
  ServerReceipt: { decode: (v) => decodeServerReceipt(v), encode: (v) => encodeServerReceipt(v as ServerReceipt) },
  ClientRead: { decode: (v) => decodeClientRead(v), encode: (v) => encodeClientRead(v as ClientRead) },
  ClientTyping: { decode: (v) => decodeClientTyping(v), encode: (v) => encodeClientTyping(v as ClientTyping) },
  ServerTyping: { decode: (v) => decodeServerTyping(v), encode: (v) => encodeServerTyping(v as ServerTyping) },
  ServerError: { decode: (v) => decodeServerError(v), encode: (v) => encodeServerError(v as ServerError) },
  ServerResyncRequired: { decode: (v) => decodeServerResyncRequired(v), encode: (v) => encodeServerResyncRequired(v as ServerResyncRequired) },
  ClientFrame: { decode: (v) => decodeClientFrame(v), encode: (v) => encodeClientFrame(v as ClientFrame) },
  ServerFrame: { decode: (v) => decodeServerFrame(v), encode: (v) => encodeServerFrame(v as ServerFrame) },
  SyncResponse: { decode: (v) => decodeSyncResponse(v), encode: (v) => encodeSyncResponse(v as SyncResponse) },
}

