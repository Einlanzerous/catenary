// Code generated from schema/catenary.wire.v1.schema.json. DO NOT EDIT.
//
// Regenerate with `npm run gen` from web/. Wire version 1.
// Editing this file by hand makes it a hand-written file with extra steps,
// and `npm run gen:check` will fail in CI the moment you do.
// See IDEA-27 (R4).

// ignore_for_file: unnecessary_this, prefer_const_constructors, lines_longer_than_80_chars

const int wireVersion = 1;

/// Thrown when a frame does not match the schema. Carries the JSON path so a
/// malformed field is identifiable from a log line rather than needing a repro.
class WireFormatException implements Exception {
  const WireFormatException(this.path, this.message);
  final String path;
  final String message;
  @override
  String toString() => 'WireFormatException: $path: $message';
}

Never _bad(String p, String m) => throw WireFormatException(p, m);

Map<String, dynamic> _obj(Object? v, String p) => v is Map<String, dynamic>
    ? v
    : _bad(p, 'expected object, got ${v.runtimeType}');
String _str(Object? v, String p) => v is String ? v : _bad(p, 'expected String, got ${v.runtimeType}');
bool _bool(Object? v, String p) => v is bool ? v : _bad(p, 'expected bool, got ${v.runtimeType}');
int _int(Object? v, String p) {
  if (v is! int) _bad(p, 'expected int, got ${v.runtimeType}');
  // Mirrors the TypeScript guard. The schema caps ordinals at 2^53-1 so that a
  // value legal in Dart cannot be a value JavaScript has already rounded; a Dart
  // client that accepted more would be the half of the pair that disagrees.
  if (v > 9007199254740991 || v < -9007199254740991) _bad(p, 'integer $v exceeds the range JavaScript can represent exactly');
  return v;
}
List<Object?> _arr(Object? v, String p) => v is List ? v : _bad(p, 'expected List, got ${v.runtimeType}');
String _oneOf(Object? v, List<String> allowed, String p) {
  final s = _str(v, p);
  return allowed.contains(s) ? s : _bad(p, 'expected one of ${allowed.join('|')}, got "$s"');
}
/// Drops null entries so an absent optional is omitted rather than encoded as null.
Map<String, dynamic> _compact(Map<String, dynamic> m) {
  m.removeWhere((_, v) => v == null);
  return m;
}

/// A per-conversation, server-assigned, strictly monotonic message ordinal, starting at
/// 1. Dense within a conversation for every member: if you can see seq 4 and seq 6 you
/// may assume seq 5 exists and you are missing it. This is the number the UI reasons
/// about — ordering, `first_unread_seq`, the "N NEW" rule.
///
/// UPPER BOUND IS DELIBERATE. The natural type is int64 and Postgres will hand out
/// int64, but JSON numbers land in a JavaScript double, which stops being an exact
/// integer past 2^53-1 (9007199254740991). A seq above that silently rounds in the web
/// client and does not in Dart or Go — the same class of bug as the non-portable
/// waveform seed already recorded on IDEA-23, and far more dangerous because it
/// corrupts ordering rather than pixels. 2^53 messages in one conversation is not
/// reachable, so the bound costs nothing and removes the failure mode. Anything that
/// could genuinely exceed it must be a string, not a number.
typedef Seq = int;
int _asSeq(Object? v, String p) {
  final x = _int(v, p);
  if (x < 1) _bad(p, 'Seq must be >= 1, got $x');
  if (x > 9007199254740991) _bad(p, 'Seq must be <= 9007199254740991, got $x');
  return x;
}

/// A SERVER-GLOBAL, server-assigned, strictly monotonic cursor over the whole message
/// log. Each account reads it as a cursor into the subset it is allowed to see. This is
/// the ONLY thing `GET /sync?after=N` takes.
///
/// WHY THIS EXISTS, since IDEA-23 named only one sequence: `seq` is scoped per
/// conversation, so a single scalar `after=N` is not well-defined across conversations,
/// and a client returning from six hours offline needs to catch up on all of them —
/// including conversations it has never seen, which by definition are absent from any
/// per-conversation cursor map it holds. A cursor vector solves the first problem and
/// not the second, and it grows without bound. One global ordinal solves both and stays
/// a single integer per device.
///
/// ONE COUNTER, NOT ONE PER ACCOUNT. An earlier draft of this text said
/// “account-global”, which reads as a per-account counter and contradicts the sparsity
/// rule below — a per-account counter would be dense. Ratified as path A on IDEA-27,
/// 2026-08-17.
///
/// Gaps are normal and carry no information: an account observes only the rows of
/// conversations it belongs to, so its observed log_seq values are sparse. Never treat
/// a gap as a missing message. `seq` is dense, `log_seq` is not; that is the whole
/// division of labour between them.
///
/// NORMATIVE — WITHIN ONE CONVERSATION, ASCENDING `log_seq` IMPLIES ASCENDING `seq`. A
/// client discovers messages by `log_seq` and orders them by `seq`; if the two ever
/// disagree it applies a conversation out of order. This forbids the obvious
/// implementation. A `bigserial` assigns at INSERT, not at COMMIT, so a transaction
/// holding 100 can commit after one holding 101, and a reader whose cursor has already
/// passed 101 never sees 100 again — silent, permanent loss of a delivered message.
/// Assign both ordinals from a single-row counter with `UPDATE … RETURNING` inside the
/// inserting transaction, so the row lock makes assignment order and commit order the
/// same order.
///
/// 0 means “I have nothing”, so a fresh device syncs with `after=0`.
typedef LogSeq = int;
int _asLogSeq(Object? v, String p) {
  final x = _int(v, p);
  if (x < 0) _bad(p, 'LogSeq must be >= 0, got $x');
  if (x > 9007199254740991) _bad(p, 'LogSeq must be <= 9007199254740991, got $x');
  return x;
}

/// Lowercase hyphenated UUID. Lowercase is normative: these are compared as strings in
/// caches and idempotency keys, and a client that sends uppercase would defeat
/// deduplication.
typedef Uuid = String;
final RegExp _UuidPattern = RegExp(r'^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');
String _asUuid(Object? v, String p) {
  final x = _str(v, p);
  if (!_UuidPattern.hasMatch(x)) _bad(p, 'Uuid must match ^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\$, got "$x"');
  return x;
}

/// RFC 3339, UTC, exactly three fractional digits, literal Z. The pattern is enforced
/// rather than advisory because `2026-08-17T04:22:01Z` and
/// `2026-08-17T04:22:01.193000Z` both parse fine in all three languages and then sort
/// differently as strings.
typedef Timestamp = String;
final RegExp _TimestampPattern = RegExp(r'^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$');
String _asTimestamp(Object? v, String p) {
  final x = _str(v, p);
  if (!_TimestampPattern.hasMatch(x)) _bad(p, 'Timestamp must match ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{3}Z\$, got "$x"');
  return x;
}

/// D4: everything is a conversation. `direct` is two members, `group` is any number;
/// neither is a distinct entity. Adding a third member to a direct conversation
/// promotes it to `group` and is a row insert plus this field changing, never a
/// migration.
enum ConversationKind {
  direct("direct"),
  group("group"),
  ;

  const ConversationKind(this.wire);
  /// The exact string this value has on the wire. Never derive it from the
  /// Dart identifier — the two differ wherever the wire uses snake_case.
  final String wire;

  static ConversationKind fromWire(Object? v, String p) {
    for (final e in ConversationKind.values) { if (e.wire == v) return e; }
    return _bad(p, 'not a valid ConversationKind: "$v"');
  }
}

/// The server-authoritative half of the message lifecycle from IDEA-23, and the ONLY
/// states that ever cross the wire.
///
/// The full ladder the UI renders is `sending → sent → delivered → read`, plus `queued`
/// and `failed`. `sending`, `queued` and `failed` are client-local: they describe a
/// message's relationship to its own outbox, the server has no opinion on them, and a
/// client that put them in a frame would be reporting its own state as fact. Keeping
/// them out of this enum is what stops that being expressible.
///
/// Generated clients therefore get this enum for decoding and are expected to widen it
/// locally — see the `ClientDeliveryState` note in the generated output.
enum DeliveryState {
  sent("sent"),
  delivered("delivered"),
  read("read"),
  ;

  const DeliveryState(this.wire);
  /// The exact string this value has on the wire. Never derive it from the
  /// Dart identifier — the two differ wherever the wire uses snake_case.
  final String wire;

  static DeliveryState fromWire(Object? v, String p) {
    for (final e in DeliveryState.values) { if (e.wire == v) return e; }
    return _bad(p, 'not a valid DeliveryState: "$v"');
  }
}

/// Lifecycle of the async Whisper job (R3/IDEA-26) that writes `transcript_text` back
/// onto the message row.
enum TranscriptState {
  pending("pending"),
  ready("ready"),
  failed("failed"),
  ;

  const TranscriptState(this.wire);
  /// The exact string this value has on the wire. Never derive it from the
  /// Dart identifier — the two differ wherever the wire uses snake_case.
  final String wire;

  static TranscriptState fromWire(Object? v, String p) {
    for (final e in TranscriptState.values) { if (e.wire == v) return e; }
    return _bad(p, 'not a valid TranscriptState: "$v"');
  }
}

/// A duration or offset in whole milliseconds.
///
/// NO FLOATING-POINT NUMBER APPEARS ANYWHERE IN THIS PROTOCOL, and this type is why.
/// JSON has a single number type; Dart has two. A `number`-typed field holding `0`
/// decodes to Dart `double 0.0` and re-encodes as `0.0`, which is not what TypeScript
/// or Go emit for the same field — so a value that survived a round trip in two
/// languages would not survive it in the third, and the conformance vectors would fail
/// on a difference with no bug behind it. Worse, the workaround (compare numbers
/// loosely) would have hidden real precision differences too.
///
/// Milliseconds as integers are exact everywhere, sort correctly, and are what a media
/// pipeline already works in. The cost is that clients format for display, which they
/// were doing anyway — nothing renders a raw duration.
typedef DurationMs = int;
int _asDurationMs(Object? v, String p) {
  final x = _int(v, p);
  if (x < 0) _bad(p, 'DurationMs must be >= 0, got $x');
  if (x > 9007199254740991) _bad(p, 'DurationMs must be <= 9007199254740991, got $x');
  return x;
}

/// The four source types the canvas's replies section renders.
enum ReplyRefKind {
  text("text"),
  voice("voice"),
  image("image"),
  link("link"),
  ;

  const ReplyRefKind(this.wire);
  /// The exact string this value has on the wire. Never derive it from the
  /// Dart identifier — the two differ wherever the wire uses snake_case.
  final String wire;

  static ReplyRefKind fromWire(Object? v, String p) {
    for (final e in ReplyRefKind.values) { if (e.wire == v) return e; }
    return _bad(p, 'not a valid ReplyRefKind: "$v"');
  }
}

enum TypingState {
  start("start"),
  stop("stop"),
  ;

  const TypingState(this.wire);
  /// The exact string this value has on the wire. Never derive it from the
  /// Dart identifier — the two differ wherever the wire uses snake_case.
  final String wire;

  static TypingState fromWire(Object? v, String p) {
    for (final e in TypingState.values) { if (e.wire == v) return e; }
    return _bad(p, 'not a valid TypingState: "$v"');
  }
}

enum ErrorCode {
  unauthorized("unauthorized"),
  wireVersionUnsupported("wire_version_unsupported"),
  rateLimited("rate_limited"),
  notAMember("not_a_member"),
  conversationNotFound("conversation_not_found"),
  messageTooLarge("message_too_large"),
  uploadNotFound("upload_not_found"),
  internal("internal"),
  ;

  const ErrorCode(this.wire);
  /// The exact string this value has on the wire. Never derive it from the
  /// Dart identifier — the two differ wherever the wire uses snake_case.
  final String wire;

  static ErrorCode fromWire(Object? v, String p) {
    for (final e in ErrorCode.values) { if (e.wire == v) return e; }
    return _bad(p, 'not a valid ErrorCode: "$v"');
  }
}

/// Tagged union on `kind`. Decoders MUST ignore an attachment whose `kind` they do not
/// know rather than failing the whole message — a client that hard-errors on an unknown
/// attachment type cannot be shipped ahead of a server that adds one.
sealed class Attachment {
  /// The wire tag for this frame.
  String get kind;
  Map<String, dynamic> toJson();

  /// Decode a Attachment. Returns null for an unrecognised "kind", which callers MUST
  /// treat as "ignore this frame and carry on" rather than as an error — that is what
  /// lets the server ship a new frame type before every client understands it.
  static Attachment? fromJson(Object? v, [String p = "Attachment"]) {
    final o = _obj(v, p);
    switch (o["kind"]) {
      case "voice": return VoiceAttachment.fromJson(o, p);
      case "image": return ImageAttachment.fromJson(o, p);
      default: return null;
    }
  }
}

/// Anything a client may send over the socket. One JSON object per WebSocket text
/// frame; never batched, never binary.
sealed class ClientFrame {
  /// The wire tag for this frame.
  String get type;
  Map<String, dynamic> toJson();

  /// Decode a ClientFrame. Returns null for an unrecognised "type", which callers MUST
  /// treat as "ignore this frame and carry on" rather than as an error — that is what
  /// lets the server ship a new frame type before every client understands it.
  static ClientFrame? fromJson(Object? v, [String p = "ClientFrame"]) {
    final o = _obj(v, p);
    switch (o["type"]) {
      case "hello": return ClientHello.fromJson(o, p);
      case "ping": return Ping.fromJson(o, p);
      case "pong": return Pong.fromJson(o, p);
      case "send": return ClientSend.fromJson(o, p);
      case "read": return ClientRead.fromJson(o, p);
      case "typing": return ClientTyping.fromJson(o, p);
      default: return null;
    }
  }
}

/// Anything the server may send over the socket.
sealed class ServerFrame {
  /// The wire tag for this frame.
  String get type;
  Map<String, dynamic> toJson();

  /// Decode a ServerFrame. Returns null for an unrecognised "type", which callers MUST
  /// treat as "ignore this frame and carry on" rather than as an error — that is what
  /// lets the server ship a new frame type before every client understands it.
  static ServerFrame? fromJson(Object? v, [String p = "ServerFrame"]) {
    final o = _obj(v, p);
    switch (o["type"]) {
      case "ready": return ServerReady.fromJson(o, p);
      case "ping": return Ping.fromJson(o, p);
      case "pong": return Pong.fromJson(o, p);
      case "ack": return ServerAck.fromJson(o, p);
      case "message": return ServerMessageFrame.fromJson(o, p);
      case "receipt": return ServerReceipt.fromJson(o, p);
      case "typing": return ServerTyping.fromJson(o, p);
      case "error": return ServerError.fromJson(o, p);
      case "resync_required": return ServerResyncRequired.fromJson(o, p);
      default: return null;
    }
  }
}

final class User {
  const User({
    required this.id,
    required this.name,
    this.initials,
  });

  final Uuid id;

  /// Display name. The typing-indicator naming rule uses the first whitespace-separated
  /// token of this as the first name.
  final String name;

  /// Two letters for the avatar tile. Server-derived so web and Flutter cannot disagree
  /// about how to abbreviate a name — deriving this client-side is a two-implementation
  /// problem for zero benefit.
  final String? initials;

  factory User.fromJson(Object? v, [String p = "User"]) {
    final o = _obj(v, p);
    return User(
      id: o["id"] == null ? _bad('${p}.id', 'required field is missing') : _asUuid(o["id"], '${p}.id'),
      name: o["name"] == null ? _bad('${p}.name', 'required field is missing') : _str(o["name"], '${p}.name'),
      initials: o["initials"] == null ? null : _str(o["initials"], '${p}.initials'),
    );
  }

  Map<String, dynamic> toJson() => _compact({
    "id": id,
    "name": name,
    "initials": initials == null ? null : initials!,
  });
}

/// One timed span of transcript, used by search's JUMP TO and by playback highlighting.
final class TranscriptSegment {
  const TranscriptSegment({
    required this.atMs,
    required this.text,
  });

  /// Offset from the start of the clip.
  final DurationMs atMs;

  final String text;

  factory TranscriptSegment.fromJson(Object? v, [String p = "TranscriptSegment"]) {
    final o = _obj(v, p);
    return TranscriptSegment(
      atMs: o["at_ms"] == null ? _bad('${p}.at_ms', 'required field is missing') : _asDurationMs(o["at_ms"], '${p}.at_ms'),
      text: o["text"] == null ? _bad('${p}.text', 'required field is missing') : _str(o["text"], '${p}.text'),
    );
  }

  Map<String, dynamic> toJson() => _compact({
    "at_ms": atMs,
    "text": text,
  });
}

final class Transcript {
  const Transcript({
    required this.state,
    this.text,
    this.wordCount,
    this.segments,
    this.engine,
    this.language,
    this.etaSec,
  });

  final TranscriptState state;

  /// Present when state is `ready`.
  final String? text;

  /// Server-computed. The web client currently derives this from the text on screen so
  /// the count cannot lie; that stays true — a client SHOULD prefer its own count of the
  /// text it is actually rendering and treat this as a hint for collapsed state.
  final int? wordCount;

  final List<TranscriptSegment>? segments;

  /// e.g. `whisper.cpp/small.en`. Recorded because a transcript's quality is not
  /// interpretable without knowing what produced it, and R3 expects the model to be a
  /// tunable knob.
  final String? engine;

  /// BCP 47.
  final String? language;

  /// Server's own estimate of remaining work, shown while `pending`. A client must not
  /// invent one.
  final int? etaSec;

  factory Transcript.fromJson(Object? v, [String p = "Transcript"]) {
    final o = _obj(v, p);
    return Transcript(
      state: o["state"] == null ? _bad('${p}.state', 'required field is missing') : TranscriptState.fromWire(o["state"], '${p}.state'),
      text: o["text"] == null ? null : _str(o["text"], '${p}.text'),
      wordCount: o["word_count"] == null ? null : _int(o["word_count"], '${p}.word_count'),
      segments: o["segments"] == null ? null : [for (final (i, x) in _arr(o["segments"], '${p}.segments').indexed) TranscriptSegment.fromJson(x, '${p}.segments[${i}]')],
      engine: o["engine"] == null ? null : _str(o["engine"], '${p}.engine'),
      language: o["language"] == null ? null : _str(o["language"], '${p}.language'),
      etaSec: o["eta_sec"] == null ? null : _int(o["eta_sec"], '${p}.eta_sec'),
    );
  }

  Map<String, dynamic> toJson() => _compact({
    "state": state.wire,
    "text": text == null ? null : text!,
    "word_count": wordCount == null ? null : wordCount!,
    "segments": segments == null ? null : [for (final x in segments!) x.toJson()],
    "engine": engine == null ? null : engine!,
    "language": language == null ? null : language!,
    "eta_sec": etaSec == null ? null : etaSec!,
  });
}

final class VoiceAttachment implements Attachment {
  const VoiceAttachment({
    required this.url,
    required this.durationMs,
    required this.peaks,
    required this.transcript,
  });

  @override
  String get kind => "voice";

  /// Media URL. Opaque to the client; may be presigned and time-limited.
  final String url;

  final DurationMs durationMs;

  /// Amplitude peaks 0–100, computed server-side ONCE and stored with the message.
  /// Deliberate call 13 of the design canvas: the bar pattern must be identical in web
  /// and Flutter, which is only true if both render the same stored array.
  ///
  /// This is not a style preference. The canvas's own seeded generator multiplies by
  /// 1103515245, which for seeds near 2^31 exceeds 2^53 — so JavaScript rounds where
  /// Dart's 64-bit int does not, and the identical seed yields different bars. Any
  /// client-side generation reintroduces that. Clients render this array and never
  /// synthesise one.
  final List<int> peaks;

  final Transcript transcript;

  factory VoiceAttachment.fromJson(Object? v, [String p = "VoiceAttachment"]) {
    final o = _obj(v, p);
    return VoiceAttachment(
      url: o["url"] == null ? _bad('${p}.url', 'required field is missing') : _str(o["url"], '${p}.url'),
      durationMs: o["duration_ms"] == null ? _bad('${p}.duration_ms', 'required field is missing') : _asDurationMs(o["duration_ms"], '${p}.duration_ms'),
      peaks: o["peaks"] == null ? _bad('${p}.peaks', 'required field is missing') : [for (final (i, x) in _arr(o["peaks"], '${p}.peaks').indexed) _int(x, '${p}.peaks[${i}]')],
      transcript: o["transcript"] == null ? _bad('${p}.transcript', 'required field is missing') : Transcript.fromJson(o["transcript"], '${p}.transcript'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "kind": "voice",
    "url": url,
    "duration_ms": durationMs,
    "peaks": peaks,
    "transcript": transcript.toJson(),
  });
}

final class ImageAttachment implements Attachment {
  const ImageAttachment({
    required this.url,
    required this.filename,
    required this.width,
    required this.height,
    required this.bytes,
    this.placeholder,
  });

  @override
  String get kind => "image";

  final String url;

  final String filename;

  /// Stored intrinsic width. The client caps the rendered box at 340x400 from this ratio,
  /// so the row height is final before a byte of image data arrives — which is the
  /// property the blurhash placeholder exists to protect.
  final int width;

  final int height;

  final int bytes;

  /// Blurhash. Absent means render the plane flat; never means block on the image.
  final String? placeholder;

  factory ImageAttachment.fromJson(Object? v, [String p = "ImageAttachment"]) {
    final o = _obj(v, p);
    return ImageAttachment(
      url: o["url"] == null ? _bad('${p}.url', 'required field is missing') : _str(o["url"], '${p}.url'),
      filename: o["filename"] == null ? _bad('${p}.filename', 'required field is missing') : _str(o["filename"], '${p}.filename'),
      width: o["width"] == null ? _bad('${p}.width', 'required field is missing') : _int(o["width"], '${p}.width'),
      height: o["height"] == null ? _bad('${p}.height', 'required field is missing') : _int(o["height"], '${p}.height'),
      bytes: o["bytes"] == null ? _bad('${p}.bytes', 'required field is missing') : _int(o["bytes"], '${p}.bytes'),
      placeholder: o["placeholder"] == null ? null : _str(o["placeholder"], '${p}.placeholder'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "kind": "image",
    "url": url,
    "filename": filename,
    "width": width,
    "height": height,
    "bytes": bytes,
    "placeholder": placeholder == null ? null : placeholder!,
  });
}

final class ReplyRef {
  const ReplyRef({
    required this.messageId,
    required this.authorId,
    required this.kind,
    required this.preview,
    this.durationMs,
    this.url,
  });

  final Uuid messageId;

  final Uuid authorId;

  final ReplyRefKind kind;

  /// One line, rendered from the LIVE source message server-side, not frozen at send
  /// time. That is what lets a reply to a voice note back-fill its preview when the
  /// transcript lands.
  final String preview;

  final DurationMs? durationMs;

  final String? url;

  factory ReplyRef.fromJson(Object? v, [String p = "ReplyRef"]) {
    final o = _obj(v, p);
    return ReplyRef(
      messageId: o["message_id"] == null ? _bad('${p}.message_id', 'required field is missing') : _asUuid(o["message_id"], '${p}.message_id'),
      authorId: o["author_id"] == null ? _bad('${p}.author_id', 'required field is missing') : _asUuid(o["author_id"], '${p}.author_id'),
      kind: o["kind"] == null ? _bad('${p}.kind', 'required field is missing') : ReplyRefKind.fromWire(o["kind"], '${p}.kind'),
      preview: o["preview"] == null ? _bad('${p}.preview', 'required field is missing') : _str(o["preview"], '${p}.preview'),
      durationMs: o["duration_ms"] == null ? null : _asDurationMs(o["duration_ms"], '${p}.duration_ms'),
      url: o["url"] == null ? null : _str(o["url"], '${p}.url'),
    );
  }

  Map<String, dynamic> toJson() => _compact({
    "message_id": messageId,
    "author_id": authorId,
    "kind": kind.wire,
    "preview": preview,
    "duration_ms": durationMs == null ? null : durationMs!,
    "url": url == null ? null : url!,
  });
}

final class Message {
  const Message({
    required this.id,
    required this.seq,
    required this.logSeq,
    required this.conversationId,
    required this.authorId,
    required this.at,
    this.text,
    this.attachments,
    this.replyTo,
    required this.state,
    this.readBy,
    this.clientId,
    this.editedAt,
    this.deleted,
  });

  final Uuid id;

  final Seq seq;

  final LogSeq logSeq;

  final Uuid conversationId;

  final Uuid authorId;

  final Timestamp at;

  final String? text;

  final List<Attachment>? attachments;

  final ReplyRef? replyTo;

  final DeliveryState state;

  /// How many members other than the author have read it. Rooms render the fraction
  /// against `Conversation.member_count`: READ 5/7.
  final int? readBy;

  /// Echoed back to the sender only, so a client can match a broadcast message against
  /// its own outbox entry when the `ack` and the `message` frame race. Other members
  /// never receive it.
  final Uuid? clientId;

  final Timestamp? editedAt;

  /// A tombstone. The row keeps its seq — deleting a message must not renumber the
  /// conversation, or every other client's unread arithmetic breaks.
  final bool? deleted;

  factory Message.fromJson(Object? v, [String p = "Message"]) {
    final o = _obj(v, p);
    return Message(
      id: o["id"] == null ? _bad('${p}.id', 'required field is missing') : _asUuid(o["id"], '${p}.id'),
      seq: o["seq"] == null ? _bad('${p}.seq', 'required field is missing') : _asSeq(o["seq"], '${p}.seq'),
      logSeq: o["log_seq"] == null ? _bad('${p}.log_seq', 'required field is missing') : _asLogSeq(o["log_seq"], '${p}.log_seq'),
      conversationId: o["conversation_id"] == null ? _bad('${p}.conversation_id', 'required field is missing') : _asUuid(o["conversation_id"], '${p}.conversation_id'),
      authorId: o["author_id"] == null ? _bad('${p}.author_id', 'required field is missing') : _asUuid(o["author_id"], '${p}.author_id'),
      at: o["at"] == null ? _bad('${p}.at', 'required field is missing') : _asTimestamp(o["at"], '${p}.at'),
      text: o["text"] == null ? null : _str(o["text"], '${p}.text'),
      attachments: o["attachments"] == null ? null : [for (final (i, x) in _arr(o["attachments"], '${p}.attachments').indexed) Attachment.fromJson(x, '${p}.attachments[${i}]')].whereType<Attachment>().toList(),
      replyTo: o["reply_to"] == null ? null : ReplyRef.fromJson(o["reply_to"], '${p}.reply_to'),
      state: o["state"] == null ? _bad('${p}.state', 'required field is missing') : DeliveryState.fromWire(o["state"], '${p}.state'),
      readBy: o["read_by"] == null ? null : _int(o["read_by"], '${p}.read_by'),
      clientId: o["client_id"] == null ? null : _asUuid(o["client_id"], '${p}.client_id'),
      editedAt: o["edited_at"] == null ? null : _asTimestamp(o["edited_at"], '${p}.edited_at'),
      deleted: o["deleted"] == null ? null : _bool(o["deleted"], '${p}.deleted'),
    );
  }

  Map<String, dynamic> toJson() => _compact({
    "id": id,
    "seq": seq,
    "log_seq": logSeq,
    "conversation_id": conversationId,
    "author_id": authorId,
    "at": at,
    "text": text == null ? null : text!,
    "attachments": attachments == null ? null : [for (final x in attachments!) x.toJson()],
    "reply_to": replyTo == null ? null : replyTo!.toJson(),
    "state": state.wire,
    "read_by": readBy == null ? null : readBy!,
    "client_id": clientId == null ? null : clientId!,
    "edited_at": editedAt == null ? null : editedAt!,
    "deleted": deleted == null ? null : deleted!,
  });
}

final class Conversation {
  const Conversation({
    required this.id,
    required this.kind,
    required this.name,
    required this.memberCount,
    this.muted,
    this.firstUnreadSeq,
    required this.headSeq,
    this.retentionDays,
  });

  final Uuid id;

  final ConversationKind kind;

  final String name;

  final int memberCount;

  final bool? muted;

  /// The first seq the reader has not seen; absent means fully read. Both the rail's
  /// badge and the thread's "N NEW" rule derive from this. There is deliberately NO
  /// stored unread count on the wire, because a count and a marker can disagree and this
  /// one does: your own messages are never unread, so a count computed anywhere but from
  /// this marker gets it wrong. Absent-means-read also keeps a resync that delivers out
  /// of order from producing a wrong badge.
  final Seq? firstUnreadSeq;

  /// Newest seq the server holds for this conversation. What a resync counts towards, and
  /// what the numeric resync progress is a fraction of.
  final Seq headSeq;

  /// Per-conversation override of the global infinite retention. Absent means keep
  /// forever.
  final int? retentionDays;

  factory Conversation.fromJson(Object? v, [String p = "Conversation"]) {
    final o = _obj(v, p);
    return Conversation(
      id: o["id"] == null ? _bad('${p}.id', 'required field is missing') : _asUuid(o["id"], '${p}.id'),
      kind: o["kind"] == null ? _bad('${p}.kind', 'required field is missing') : ConversationKind.fromWire(o["kind"], '${p}.kind'),
      name: o["name"] == null ? _bad('${p}.name', 'required field is missing') : _str(o["name"], '${p}.name'),
      memberCount: o["member_count"] == null ? _bad('${p}.member_count', 'required field is missing') : _int(o["member_count"], '${p}.member_count'),
      muted: o["muted"] == null ? null : _bool(o["muted"], '${p}.muted'),
      firstUnreadSeq: o["first_unread_seq"] == null ? null : _asSeq(o["first_unread_seq"], '${p}.first_unread_seq'),
      headSeq: o["head_seq"] == null ? _bad('${p}.head_seq', 'required field is missing') : _asSeq(o["head_seq"], '${p}.head_seq'),
      retentionDays: o["retention_days"] == null ? null : _int(o["retention_days"], '${p}.retention_days'),
    );
  }

  Map<String, dynamic> toJson() => _compact({
    "id": id,
    "kind": kind.wire,
    "name": name,
    "member_count": memberCount,
    "muted": muted == null ? null : muted!,
    "first_unread_seq": firstUnreadSeq == null ? null : firstUnreadSeq!,
    "head_seq": headSeq,
    "retention_days": retentionDays == null ? null : retentionDays!,
  });
}

/// First frame the client sends after the socket opens. The server answers with `ready`
/// or `error`.
final class ClientHello implements ClientFrame {
  const ClientHello({
    required this.wireVersion,
    required this.deviceId,
    this.resumeFromLogSeq,
    this.clientInfo,
  });

  @override
  String get type => "hello";

  /// The version of THIS schema the client was generated from. The server refuses a
  /// version it cannot speak with an `error` frame rather than failing halfway through a
  /// session.
  final int wireVersion;

  /// Stable per install. D2 makes the device the unit of credential and of revocation, so
  /// it is the unit here too.
  final Uuid deviceId;

  /// The client's cursor. Absent means "do not stream backlog, I will call /sync myself"
  /// — which is the correct choice for a client with a large gap, since a backlog burst
  /// over the socket has no progress indication.
  final LogSeq? resumeFromLogSeq;

  /// Free-form build identifier for logs, e.g. `catenary-web/0.3.1`. Never parsed.
  final String? clientInfo;

  factory ClientHello.fromJson(Object? v, [String p = "ClientHello"]) {
    final o = _obj(v, p);
    return ClientHello(
      wireVersion: o["wire_version"] == null ? _bad('${p}.wire_version', 'required field is missing') : _int(o["wire_version"], '${p}.wire_version'),
      deviceId: o["device_id"] == null ? _bad('${p}.device_id', 'required field is missing') : _asUuid(o["device_id"], '${p}.device_id'),
      resumeFromLogSeq: o["resume_from_log_seq"] == null ? null : _asLogSeq(o["resume_from_log_seq"], '${p}.resume_from_log_seq'),
      clientInfo: o["client_info"] == null ? null : _str(o["client_info"], '${p}.client_info'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "hello",
    "wire_version": wireVersion,
    "device_id": deviceId,
    "resume_from_log_seq": resumeFromLogSeq == null ? null : resumeFromLogSeq!,
    "client_info": clientInfo == null ? null : clientInfo!,
  });
}

final class ServerReady implements ServerFrame {
  const ServerReady({
    required this.sessionId,
    required this.serverTime,
    required this.heartbeatIntervalSec,
    required this.missedPongLimit,
    required this.logSeq,
    required this.resumed,
  });

  @override
  String get type => "ready";

  final Uuid sessionId;

  /// Lets a client with a skewed clock render correct relative timestamps by offset
  /// rather than trusting its own clock.
  final Timestamp serverTime;

  /// How often the client must send `ping`. SERVER-DRIVEN ON PURPOSE (R1/IDEA-24): the
  /// 30–45 s window sits under Cloudflare's ~100 s idle timeout, and if that constant
  /// lived in two clients it would be two constants that drift, with the Flutter one
  /// discovered wrong only in the field. Here the number is deployed, not shipped.
  final int heartbeatIntervalSec;

  /// How many unanswered pings before the client severs deliberately and reconnects. R1
  /// sets this at 2. Waiting for the OS to notice a half-open socket is the failure this
  /// exists to prevent.
  final int missedPongLimit;

  /// The server's current head. A client that asked to resume can compare this against
  /// its own cursor to decide between streaming and a `/sync` call.
  final LogSeq logSeq;

  /// True when the server accepted `resume_from_log_seq` and will stream the gap. False
  /// means the client must catch up over `/sync` before trusting anything it renders.
  final bool resumed;

  factory ServerReady.fromJson(Object? v, [String p = "ServerReady"]) {
    final o = _obj(v, p);
    return ServerReady(
      sessionId: o["session_id"] == null ? _bad('${p}.session_id', 'required field is missing') : _asUuid(o["session_id"], '${p}.session_id'),
      serverTime: o["server_time"] == null ? _bad('${p}.server_time', 'required field is missing') : _asTimestamp(o["server_time"], '${p}.server_time'),
      heartbeatIntervalSec: o["heartbeat_interval_sec"] == null ? _bad('${p}.heartbeat_interval_sec', 'required field is missing') : _int(o["heartbeat_interval_sec"], '${p}.heartbeat_interval_sec'),
      missedPongLimit: o["missed_pong_limit"] == null ? _bad('${p}.missed_pong_limit', 'required field is missing') : _int(o["missed_pong_limit"], '${p}.missed_pong_limit'),
      logSeq: o["log_seq"] == null ? _bad('${p}.log_seq', 'required field is missing') : _asLogSeq(o["log_seq"], '${p}.log_seq'),
      resumed: o["resumed"] == null ? _bad('${p}.resumed', 'required field is missing') : _bool(o["resumed"], '${p}.resumed'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "ready",
    "session_id": sessionId,
    "server_time": serverTime,
    "heartbeat_interval_sec": heartbeatIntervalSec,
    "missed_pong_limit": missedPongLimit,
    "log_seq": logSeq,
    "resumed": resumed,
  });
}

/// APPLICATION-level heartbeat, sent in both directions. Deliberately not the WebSocket
/// protocol's own ping/pong control frames: those are handled inside the transport
/// stack, are not observable from application code in a browser at all, and can be
/// answered by an intermediary. R1 requires a heartbeat observable from both ends,
/// which means it has to be a data frame.
final class Ping implements ClientFrame, ServerFrame {
  const Ping({
    required this.id,
    this.at,
  });

  @override
  String get type => "ping";

  /// Echoed verbatim in the matching `pong`. Matching by id rather than by arrival order
  /// is what makes a missed pong countable — with unmatched pings you cannot tell a lost
  /// pong from a slow one.
  final String id;

  final Timestamp? at;

  factory Ping.fromJson(Object? v, [String p = "Ping"]) {
    final o = _obj(v, p);
    return Ping(
      id: o["id"] == null ? _bad('${p}.id', 'required field is missing') : _str(o["id"], '${p}.id'),
      at: o["at"] == null ? null : _asTimestamp(o["at"], '${p}.at'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "ping",
    "id": id,
    "at": at == null ? null : at!,
  });
}

final class Pong implements ClientFrame, ServerFrame {
  const Pong({
    required this.id,
    this.at,
  });

  @override
  String get type => "pong";

  /// The id of the ping being answered, verbatim.
  final String id;

  final Timestamp? at;

  factory Pong.fromJson(Object? v, [String p = "Pong"]) {
    final o = _obj(v, p);
    return Pong(
      id: o["id"] == null ? _bad('${p}.id', 'required field is missing') : _str(o["id"], '${p}.id'),
      at: o["at"] == null ? null : _asTimestamp(o["at"], '${p}.at'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "pong",
    "id": id,
    "at": at == null ? null : at!,
  });
}

final class OutboundAttachment {
  const OutboundAttachment({
    required this.kind,
    required this.uploadId,
  });

  final String kind;

  /// Handle returned by the presigned-upload flow. The client sends this, never the
  /// media: dimensions, duration, peaks, EXIF stripping and the blurhash are all computed
  /// server-side on ingest, so the client cannot report them and cannot get them wrong.
  final Uuid uploadId;

  factory OutboundAttachment.fromJson(Object? v, [String p = "OutboundAttachment"]) {
    final o = _obj(v, p);
    return OutboundAttachment(
      kind: o["kind"] == null ? _bad('${p}.kind', 'required field is missing') : _oneOf(o["kind"], const ["voice", "image"], '${p}.kind'),
      uploadId: o["upload_id"] == null ? _bad('${p}.upload_id', 'required field is missing') : _asUuid(o["upload_id"], '${p}.upload_id'),
    );
  }

  Map<String, dynamic> toJson() => _compact({
    "kind": kind,
    "upload_id": uploadId,
  });
}

final class ClientSend implements ClientFrame {
  const ClientSend({
    required this.clientId,
    required this.conversationId,
    this.text,
    this.attachments,
    this.replyToMessageId,
  });

  @override
  String get type => "send";

  /// The idempotency key, generated by the client once per logical message and REUSED
  /// verbatim on every retry. Same pattern as Switchyard. The server deduplicates on
  /// (account, client_id) and answers a duplicate with the original `ack`, so a retry
  /// after an ambiguous failure is free and safe — which is what makes an offline outbox
  /// correct rather than hopeful.
  ///
  /// A client that mints a fresh key on retry has re-implemented double-sending.
  final Uuid clientId;

  final Uuid conversationId;

  final String? text;

  final List<OutboundAttachment>? attachments;

  /// Just the id — the server builds the whole ReplyRef from the live source message, so
  /// the preview is never a stale copy taken at send time.
  final Uuid? replyToMessageId;

  factory ClientSend.fromJson(Object? v, [String p = "ClientSend"]) {
    final o = _obj(v, p);
    return ClientSend(
      clientId: o["client_id"] == null ? _bad('${p}.client_id', 'required field is missing') : _asUuid(o["client_id"], '${p}.client_id'),
      conversationId: o["conversation_id"] == null ? _bad('${p}.conversation_id', 'required field is missing') : _asUuid(o["conversation_id"], '${p}.conversation_id'),
      text: o["text"] == null ? null : _str(o["text"], '${p}.text'),
      attachments: o["attachments"] == null ? null : [for (final (i, x) in _arr(o["attachments"], '${p}.attachments').indexed) OutboundAttachment.fromJson(x, '${p}.attachments[${i}]')],
      replyToMessageId: o["reply_to_message_id"] == null ? null : _asUuid(o["reply_to_message_id"], '${p}.reply_to_message_id'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "send",
    "client_id": clientId,
    "conversation_id": conversationId,
    "text": text == null ? null : text!,
    "attachments": attachments == null ? null : [for (final x in attachments!) x.toJson()],
    "reply_to_message_id": replyToMessageId == null ? null : replyToMessageId!,
  });
}

/// The other half of the send/ack pair. Receiving this moves the outbox entry from
/// `sending` to `sent` and fixes its seq.
final class ServerAck implements ServerFrame {
  const ServerAck({
    required this.clientId,
    required this.messageId,
    required this.conversationId,
    required this.seq,
    required this.logSeq,
    required this.at,
    this.duplicate,
  });

  @override
  String get type => "ack";

  /// The key from the `send` this answers.
  final Uuid clientId;

  final Uuid messageId;

  final Uuid conversationId;

  final Seq seq;

  final LogSeq logSeq;

  /// The server's timestamp for the message. Authoritative — the client's own send time
  /// is never persisted, so two devices with different clocks cannot order a conversation
  /// differently.
  final Timestamp at;

  /// True when this ack replays an earlier send with the same `client_id`. The client
  /// treats it identically; it exists so the deduplication path is observable in logs and
  /// in R1's zero-duplication test rather than being invisibly correct.
  final bool? duplicate;

  factory ServerAck.fromJson(Object? v, [String p = "ServerAck"]) {
    final o = _obj(v, p);
    return ServerAck(
      clientId: o["client_id"] == null ? _bad('${p}.client_id', 'required field is missing') : _asUuid(o["client_id"], '${p}.client_id'),
      messageId: o["message_id"] == null ? _bad('${p}.message_id', 'required field is missing') : _asUuid(o["message_id"], '${p}.message_id'),
      conversationId: o["conversation_id"] == null ? _bad('${p}.conversation_id', 'required field is missing') : _asUuid(o["conversation_id"], '${p}.conversation_id'),
      seq: o["seq"] == null ? _bad('${p}.seq', 'required field is missing') : _asSeq(o["seq"], '${p}.seq'),
      logSeq: o["log_seq"] == null ? _bad('${p}.log_seq', 'required field is missing') : _asLogSeq(o["log_seq"], '${p}.log_seq'),
      at: o["at"] == null ? _bad('${p}.at', 'required field is missing') : _asTimestamp(o["at"], '${p}.at'),
      duplicate: o["duplicate"] == null ? null : _bool(o["duplicate"], '${p}.duplicate'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "ack",
    "client_id": clientId,
    "message_id": messageId,
    "conversation_id": conversationId,
    "seq": seq,
    "log_seq": logSeq,
    "at": at,
    "duplicate": duplicate == null ? null : duplicate!,
  });
}

/// Carries the WHOLE message, deliberately. IDEA-23 is explicit that this must not
/// degrade into a ping that makes the client go and fetch: that doubles latency on
/// every message in the steady state. The internal Postgres NOTIFY payload between
/// server instances is the thing that carries only (conversation_id, seq) under its
/// 8000-byte cap — that is a different layer and never appears on this wire.
final class ServerMessageFrame implements ServerFrame {
  const ServerMessageFrame({
    required this.message,
  });

  @override
  String get type => "message";

  final Message message;

  factory ServerMessageFrame.fromJson(Object? v, [String p = "ServerMessageFrame"]) {
    final o = _obj(v, p);
    return ServerMessageFrame(
      message: o["message"] == null ? _bad('${p}.message', 'required field is missing') : Message.fromJson(o["message"], '${p}.message'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "message",
    "message": message.toJson(),
  });
}

final class ServerReceipt implements ServerFrame {
  const ServerReceipt({
    required this.conversationId,
    required this.userId,
    required this.upToSeq,
  });

  @override
  String get type => "receipt";

  final Uuid conversationId;

  final Uuid userId;

  /// Read receipts are a high-water mark, never per-message. One frame settles an
  /// arbitrary backlog and they cannot arrive out of order in a way that matters, since a
  /// lower mark than one already held is simply discarded.
  final Seq upToSeq;

  factory ServerReceipt.fromJson(Object? v, [String p = "ServerReceipt"]) {
    final o = _obj(v, p);
    return ServerReceipt(
      conversationId: o["conversation_id"] == null ? _bad('${p}.conversation_id', 'required field is missing') : _asUuid(o["conversation_id"], '${p}.conversation_id'),
      userId: o["user_id"] == null ? _bad('${p}.user_id', 'required field is missing') : _asUuid(o["user_id"], '${p}.user_id'),
      upToSeq: o["up_to_seq"] == null ? _bad('${p}.up_to_seq', 'required field is missing') : _asSeq(o["up_to_seq"], '${p}.up_to_seq'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "receipt",
    "conversation_id": conversationId,
    "user_id": userId,
    "up_to_seq": upToSeq,
  });
}

final class ClientRead implements ClientFrame {
  const ClientRead({
    required this.conversationId,
    required this.upToSeq,
  });

  @override
  String get type => "read";

  final Uuid conversationId;

  final Seq upToSeq;

  factory ClientRead.fromJson(Object? v, [String p = "ClientRead"]) {
    final o = _obj(v, p);
    return ClientRead(
      conversationId: o["conversation_id"] == null ? _bad('${p}.conversation_id', 'required field is missing') : _asUuid(o["conversation_id"], '${p}.conversation_id'),
      upToSeq: o["up_to_seq"] == null ? _bad('${p}.up_to_seq', 'required field is missing') : _asSeq(o["up_to_seq"], '${p}.up_to_seq'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "read",
    "conversation_id": conversationId,
    "up_to_seq": upToSeq,
  });
}

final class ClientTyping implements ClientFrame {
  const ClientTyping({
    required this.conversationId,
    required this.state,
  });

  @override
  String get type => "typing";

  final Uuid conversationId;

  final TypingState state;

  factory ClientTyping.fromJson(Object? v, [String p = "ClientTyping"]) {
    final o = _obj(v, p);
    return ClientTyping(
      conversationId: o["conversation_id"] == null ? _bad('${p}.conversation_id', 'required field is missing') : _asUuid(o["conversation_id"], '${p}.conversation_id'),
      state: o["state"] == null ? _bad('${p}.state', 'required field is missing') : TypingState.fromWire(o["state"], '${p}.state'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "typing",
    "conversation_id": conversationId,
    "state": state.wire,
  });
}

final class ServerTyping implements ServerFrame {
  const ServerTyping({
    required this.conversationId,
    required this.userIds,
  });

  @override
  String get type => "typing";

  final Uuid conversationId;

  /// Who is currently typing, IN THE ORDER THEY STARTED. The order is normative because
  /// spec card F's naming rule depends on it: one person is a first name, two or three
  /// are comma-separated in start order, four or more collapse to "Several people"
  /// because past three the list churns faster than it can be read. Sorting this list in
  /// a client silently changes the rendered string, which is exactly the kind of
  /// divergence R4 exists to prevent — the rule is shared, so its input has to be too.
  final List<Uuid> userIds;

  factory ServerTyping.fromJson(Object? v, [String p = "ServerTyping"]) {
    final o = _obj(v, p);
    return ServerTyping(
      conversationId: o["conversation_id"] == null ? _bad('${p}.conversation_id', 'required field is missing') : _asUuid(o["conversation_id"], '${p}.conversation_id'),
      userIds: o["user_ids"] == null ? _bad('${p}.user_ids', 'required field is missing') : [for (final (i, x) in _arr(o["user_ids"], '${p}.user_ids').indexed) _asUuid(x, '${p}.user_ids[${i}]')],
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "typing",
    "conversation_id": conversationId,
    "user_ids": userIds,
  });
}

final class ServerError implements ServerFrame {
  const ServerError({
    required this.code,
    required this.message,
    required this.retryable,
    this.clientId,
    this.retryAfterSec,
  });

  @override
  String get type => "error";

  final ErrorCode code;

  /// Human-readable, for logs and for the inline error on a failed send. The canvas shows
  /// send failures inline on the message, never as a toast.
  final String message;

  /// Whether re-sending the identical frame could succeed. This is the server's
  /// judgement, not the client's guess: `rate_limited` is retryable, `not_a_member` is
  /// not, and a client that retried the latter forever would look exactly like a client
  /// with no error handling.
  final bool retryable;

  /// Present when the error is attributable to a specific `send`, so the right outbox
  /// entry goes to `failed` instead of all of them.
  final Uuid? clientId;

  final int? retryAfterSec;

  factory ServerError.fromJson(Object? v, [String p = "ServerError"]) {
    final o = _obj(v, p);
    return ServerError(
      code: o["code"] == null ? _bad('${p}.code', 'required field is missing') : ErrorCode.fromWire(o["code"], '${p}.code'),
      message: o["message"] == null ? _bad('${p}.message', 'required field is missing') : _str(o["message"], '${p}.message'),
      retryable: o["retryable"] == null ? _bad('${p}.retryable', 'required field is missing') : _bool(o["retryable"], '${p}.retryable'),
      clientId: o["client_id"] == null ? null : _asUuid(o["client_id"], '${p}.client_id'),
      retryAfterSec: o["retry_after_sec"] == null ? null : _int(o["retry_after_sec"], '${p}.retry_after_sec'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "error",
    "code": code.wire,
    "message": message,
    "retryable": retryable,
    "client_id": clientId == null ? null : clientId!,
    "retry_after_sec": retryAfterSec == null ? null : retryAfterSec!,
  });
}

/// The server cannot stream the requested gap and the client must catch up over `/sync`
/// instead. Naming this as its own frame rather than letting the client infer it from a
/// short stream is deliberate: silent partial resume is how a client ends up
/// confidently missing messages.
final class ServerResyncRequired implements ServerFrame {
  const ServerResyncRequired({
    required this.reason,
    required this.logSeq,
  });

  @override
  String get type => "resync_required";

  final String reason;

  /// The server's current head, so the client knows what it is syncing towards.
  final LogSeq logSeq;

  factory ServerResyncRequired.fromJson(Object? v, [String p = "ServerResyncRequired"]) {
    final o = _obj(v, p);
    return ServerResyncRequired(
      reason: o["reason"] == null ? _bad('${p}.reason', 'required field is missing') : _oneOf(o["reason"], const ["cursor_too_old", "membership_changed", "retention_purge"], '${p}.reason'),
      logSeq: o["log_seq"] == null ? _bad('${p}.log_seq', 'required field is missing') : _asLogSeq(o["log_seq"], '${p}.log_seq'),
    );
  }

  @override
  Map<String, dynamic> toJson() => _compact({
    "type": "resync_required",
    "reason": reason,
    "log_seq": logSeq,
  });
}

/// Response to `GET /sync?after=<log_seq>&limit=<n>`. The RECONNECT AND CATCH-UP path,
/// not the steady state — steady state is a `message` frame over the socket. Applying a
/// page is idempotent: every message carries its own seq and id, so replaying an
/// overlapping page cannot duplicate anything, which is what makes reconnection a query
/// rather than a guess.
final class SyncResponse {
  const SyncResponse({
    required this.logSeq,
    required this.messages,
    required this.conversations,
    required this.users,
    required this.hasMore,
    required this.serverTime,
  });

  /// The cursor to send on the NEXT call. Always the caller's new high-water mark — not
  /// the server's head, which may be further along when `has_more` is true. Deriving this
  /// client-side by maxing over `messages` is wrong the moment a page contains no
  /// messages the client can see.
  final LogSeq logSeq;

  /// Ascending by log_seq. The ordering is normative so a client can apply the page as a
  /// stream and stop anywhere without leaving a hole behind its cursor.
  final List<Message> messages;

  /// Every conversation touched by this page, plus any whose metadata changed — head_seq,
  /// first_unread_seq, membership. A client learns about a brand-new conversation here,
  /// which is why the sync cursor cannot be a per-conversation map.
  final List<Conversation> conversations;

  /// Every user referenced by this page — as a message author, a reply's author, or a
  /// member of a returned conversation.
  ///
  /// Without this there is NO path from `author_id` to a display name, and both clients
  /// would have to invent one. Messages carry ids rather than embedded authors so a
  /// rename lands everywhere at once instead of being frozen into every message ever
  /// sent; this array is the other half of that decision, and omitting it would have made
  /// the id-only design unusable rather than normalised.
  ///
  /// Send the full record whenever a user first appears on a page or their name or
  /// initials changed. Clients cache by id and treat a later record as authoritative.
  final List<User> users;

  /// Call again with the returned `log_seq`. The resync UI renders numeric progress
  /// rather than a spinner, which is only possible because `Conversation.head_seq` says
  /// what it is counting towards.
  final bool hasMore;

  final Timestamp serverTime;

  factory SyncResponse.fromJson(Object? v, [String p = "SyncResponse"]) {
    final o = _obj(v, p);
    return SyncResponse(
      logSeq: o["log_seq"] == null ? _bad('${p}.log_seq', 'required field is missing') : _asLogSeq(o["log_seq"], '${p}.log_seq'),
      messages: o["messages"] == null ? _bad('${p}.messages', 'required field is missing') : [for (final (i, x) in _arr(o["messages"], '${p}.messages').indexed) Message.fromJson(x, '${p}.messages[${i}]')],
      conversations: o["conversations"] == null ? _bad('${p}.conversations', 'required field is missing') : [for (final (i, x) in _arr(o["conversations"], '${p}.conversations').indexed) Conversation.fromJson(x, '${p}.conversations[${i}]')],
      users: o["users"] == null ? _bad('${p}.users', 'required field is missing') : [for (final (i, x) in _arr(o["users"], '${p}.users').indexed) User.fromJson(x, '${p}.users[${i}]')],
      hasMore: o["has_more"] == null ? _bad('${p}.has_more', 'required field is missing') : _bool(o["has_more"], '${p}.has_more'),
      serverTime: o["server_time"] == null ? _bad('${p}.server_time', 'required field is missing') : _asTimestamp(o["server_time"], '${p}.server_time'),
    );
  }

  Map<String, dynamic> toJson() => _compact({
    "log_seq": logSeq,
    "messages": [for (final x in messages) x.toJson()],
    "conversations": [for (final x in conversations) x.toJson()],
    "users": [for (final x in users) x.toJson()],
    "has_more": hasMore,
    "server_time": serverTime,
  });
}

/// A decode/encode pair for one named wire type.
class WireCodec {
  const WireCodec(this.decode, this.encode);
  final Object? Function(Object?) decode;
  final Map<String, dynamic> Function(Object) encode;
}

const Map<String, WireCodec> codecs = {
  "User": WireCodec(User.fromJson, _encUser),
  "TranscriptSegment": WireCodec(TranscriptSegment.fromJson, _encTranscriptSegment),
  "Transcript": WireCodec(Transcript.fromJson, _encTranscript),
  "VoiceAttachment": WireCodec(VoiceAttachment.fromJson, _encVoiceAttachment),
  "ImageAttachment": WireCodec(ImageAttachment.fromJson, _encImageAttachment),
  "Attachment": WireCodec(Attachment.fromJson, _encAttachment),
  "ReplyRef": WireCodec(ReplyRef.fromJson, _encReplyRef),
  "Message": WireCodec(Message.fromJson, _encMessage),
  "Conversation": WireCodec(Conversation.fromJson, _encConversation),
  "ClientHello": WireCodec(ClientHello.fromJson, _encClientHello),
  "ServerReady": WireCodec(ServerReady.fromJson, _encServerReady),
  "Ping": WireCodec(Ping.fromJson, _encPing),
  "Pong": WireCodec(Pong.fromJson, _encPong),
  "OutboundAttachment": WireCodec(OutboundAttachment.fromJson, _encOutboundAttachment),
  "ClientSend": WireCodec(ClientSend.fromJson, _encClientSend),
  "ServerAck": WireCodec(ServerAck.fromJson, _encServerAck),
  "ServerMessageFrame": WireCodec(ServerMessageFrame.fromJson, _encServerMessageFrame),
  "ServerReceipt": WireCodec(ServerReceipt.fromJson, _encServerReceipt),
  "ClientRead": WireCodec(ClientRead.fromJson, _encClientRead),
  "ClientTyping": WireCodec(ClientTyping.fromJson, _encClientTyping),
  "ServerTyping": WireCodec(ServerTyping.fromJson, _encServerTyping),
  "ServerError": WireCodec(ServerError.fromJson, _encServerError),
  "ServerResyncRequired": WireCodec(ServerResyncRequired.fromJson, _encServerResyncRequired),
  "ClientFrame": WireCodec(ClientFrame.fromJson, _encClientFrame),
  "ServerFrame": WireCodec(ServerFrame.fromJson, _encServerFrame),
  "SyncResponse": WireCodec(SyncResponse.fromJson, _encSyncResponse),
};

Map<String, dynamic> _encUser(Object v) => (v as User).toJson();
Map<String, dynamic> _encTranscriptSegment(Object v) => (v as TranscriptSegment).toJson();
Map<String, dynamic> _encTranscript(Object v) => (v as Transcript).toJson();
Map<String, dynamic> _encVoiceAttachment(Object v) => (v as VoiceAttachment).toJson();
Map<String, dynamic> _encImageAttachment(Object v) => (v as ImageAttachment).toJson();
Map<String, dynamic> _encAttachment(Object v) => (v as Attachment).toJson();
Map<String, dynamic> _encReplyRef(Object v) => (v as ReplyRef).toJson();
Map<String, dynamic> _encMessage(Object v) => (v as Message).toJson();
Map<String, dynamic> _encConversation(Object v) => (v as Conversation).toJson();
Map<String, dynamic> _encClientHello(Object v) => (v as ClientHello).toJson();
Map<String, dynamic> _encServerReady(Object v) => (v as ServerReady).toJson();
Map<String, dynamic> _encPing(Object v) => (v as Ping).toJson();
Map<String, dynamic> _encPong(Object v) => (v as Pong).toJson();
Map<String, dynamic> _encOutboundAttachment(Object v) => (v as OutboundAttachment).toJson();
Map<String, dynamic> _encClientSend(Object v) => (v as ClientSend).toJson();
Map<String, dynamic> _encServerAck(Object v) => (v as ServerAck).toJson();
Map<String, dynamic> _encServerMessageFrame(Object v) => (v as ServerMessageFrame).toJson();
Map<String, dynamic> _encServerReceipt(Object v) => (v as ServerReceipt).toJson();
Map<String, dynamic> _encClientRead(Object v) => (v as ClientRead).toJson();
Map<String, dynamic> _encClientTyping(Object v) => (v as ClientTyping).toJson();
Map<String, dynamic> _encServerTyping(Object v) => (v as ServerTyping).toJson();
Map<String, dynamic> _encServerError(Object v) => (v as ServerError).toJson();
Map<String, dynamic> _encServerResyncRequired(Object v) => (v as ServerResyncRequired).toJson();
Map<String, dynamic> _encClientFrame(Object v) => (v as ClientFrame).toJson();
Map<String, dynamic> _encServerFrame(Object v) => (v as ServerFrame).toJson();
Map<String, dynamic> _encSyncResponse(Object v) => (v as SyncResponse).toJson();

