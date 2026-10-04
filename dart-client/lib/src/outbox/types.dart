/// The outbox's types and its seams — mirrors web/src/outbox/types.ts
/// (CANT-36).
///
/// `docs/decisions/cant-36-outbox.md` is the rule set. Nothing here is a wire
/// type: an `OutboxEntry` is client-local storage, and the only thing from it
/// that ever reaches a socket is the `ClientSend` the drain builds from it.
///
/// TWO BROWSER PIECES OF THE REFERENCE ARE NOT HERE. §9's `persist()` line
/// exists because a browser may evict IndexedDB; a SQLite file on a real
/// filesystem is not evicted. And §8's `BroadcastChannel` re-render is not
/// built: no target has a second context with a screen, so a context sees
/// another's writes at its next read of the store (`Outbox.refresh`).
library;

import 'dart:typed_data';

import 'package:catenary_wire/catenary_wire.dart';

import 'media.dart';

enum OutboxStatus { pending, failed }

/// Why an entry is `failed`.
sealed class OutboxError {
  const OutboxError();

  factory OutboxError.fromJson(Map<String, dynamic> o) => switch (o['kind']) {
        'server' => ServerRefusal(o['code'] as String, o['message'] as String, o['retryable'] as bool),
        'bare_1008' => const Bare1008(),
        'upload' => UploadFailure(o['message'] as String),
        _ => throw FormatException('outbox: an error of kind ${o['kind']}'),
      };

  Map<String, Object?> toJson();
}

/// The server refused the send with an `error` frame naming its `clientId`.
/// `code` is the wire's own string, kept as it came.
final class ServerRefusal extends OutboxError {
  const ServerRefusal(this.code, this.message, this.retryable);

  final String code;
  final String message;
  final bool retryable;

  @override
  Map<String, Object?> toJson() => {'kind': 'server', 'code': code, 'message': message, 'retryable': retryable};
}

/// The session closed with a bare `1008` while the entry was in flight
/// (CANT-31 §7).
final class Bare1008 extends OutboxError {
  const Bare1008();

  @override
  Map<String, Object?> toJson() => {'kind': 'bare_1008'};
}

final class UploadFailure extends OutboxError {
  const UploadFailure(this.message);

  final String message;

  @override
  Map<String, Object?> toJson() => {'kind': 'upload', 'message': message};
}

enum UploadState { pending, uploaded }

/// An attachment as composed. There is no upload queue here: the default
/// `Uploader` refuses (§10), and the queue that would move one to `uploaded`
/// is CANT-212's.
///
/// THE MEDIA IS NOT ON THIS OBJECT ONCE IT IS STORED. A draft names a `source`;
/// `compose` reads it once and the store holds the bytes beside the entry, in
/// the same transaction (CANT-201 ruling 0), under the entry's `clientId` and
/// the attachment's index. A stored attachment has no `source`, and its media
/// is read with `OutboxStore.media`, never with a listing.
final class OutboundAttachmentDraft {
  const OutboundAttachmentDraft({required this.kind, this.source, this.filename, this.durationMs, this.uploadId, this.upload = UploadState.pending});

  factory OutboundAttachmentDraft.fromJson(Map<String, dynamic> o) => OutboundAttachmentDraft(
        kind: o['kind'] as String,
        filename: o['filename'] as String?,
        durationMs: o['durationMs'] as int?,
        uploadId: o['uploadId'] as String?,
        upload: UploadState.values.byName(o['upload'] as String),
      );

  /// `voice` or `image`.
  final String kind;

  /// Where the media is copied from at compose: what the platform's recorder
  /// or picker produced, which may be a temporary file. Never stored, and
  /// absent on everything read back from the store.
  final MediaSource? source;
  final String? filename;

  /// Voice: local render only; the server measures its own.
  final int? durationMs;

  /// Set when an `Uploader` returns.
  final Uuid? uploadId;
  final UploadState upload;

  /// This attachment, uploaded under `handle`.
  OutboundAttachmentDraft uploadedAs(Uuid handle) =>
      OutboundAttachmentDraft(kind: kind, filename: filename, durationMs: durationMs, uploadId: handle, upload: UploadState.uploaded);

  /// This attachment as an entry holds it: without its `source`.
  OutboundAttachmentDraft held() => OutboundAttachmentDraft(kind: kind, filename: filename, durationMs: durationMs, uploadId: uploadId, upload: upload);

  Map<String, Object?> toJson() => {
        'kind': kind,
        if (filename != null) 'filename': filename,
        if (durationMs != null) 'durationMs': durationMs,
        if (uploadId != null) 'uploadId': uploadId,
        'upload': upload.name,
      };
}

/// Stored in `catenary-outbox.db`, table `outbox`, as this JSON beside the
/// columns the store queries by.
final class OutboxEntry {
  OutboxEntry({
    required this.clientId,
    required this.accountId,
    required this.conversationId,
    required this.order,
    required this.composedAt,
    this.text,
    this.replyToMessageId,
    this.replyPreview,
    this.attachments = const [],
    this.status = OutboxStatus.pending,
    this.attempts = 0,
    this.internalRetries = 0,
    this.reuploads = 0,
    this.notBefore,
    this.lastError,
  });

  /// A stored record. A field this shape does not know is refused.
  factory OutboxEntry.fromJson(Map<String, dynamic> o) {
    final unknown = o.keys.toSet().difference(storedKeys);
    if (unknown.isNotEmpty) throw FormatException('outbox: stored field ${unknown.join(', ')} is not in the entry shape');
    if (o['v'] != 1) throw FormatException('outbox: an entry of version ${o['v']}');
    final replyPreview = o['replyPreview'];
    final lastError = o['lastError'];
    // `pending` or `failed`, and nothing else is ever written (§3). A record
    // that says otherwise — which only the `persistSending` fault produces —
    // is carried as it was stored, so the check that reads it can see it.
    final stored = o['status'] as String;
    final status = OutboxStatus.values.where((s) => s.name == stored).firstOrNull;
    return OutboxEntry(
      clientId: o['clientId'] as String,
      accountId: o['accountId'] as String,
      conversationId: o['conversationId'] as String,
      order: o['order'] as int,
      composedAt: o['composedAt'] as String,
      text: o['text'] as String?,
      replyToMessageId: o['replyToMessageId'] as String?,
      replyPreview: replyPreview == null ? null : ReplyRef.fromJson(replyPreview),
      attachments: [for (final a in (o['attachments'] as List?) ?? const []) OutboundAttachmentDraft.fromJson(a as Map<String, dynamic>)],
      status: status ?? OutboxStatus.pending,
      attempts: o['attempts'] as int,
      internalRetries: o['internalRetries'] as int,
      reuploads: o['reuploads'] as int,
      notBefore: o['notBefore'] as String?,
      lastError: lastError == null ? null : OutboxError.fromJson(lastError as Map<String, dynamic>),
    )..storedStatusOverride = status == null ? stored : null;
  }

  /// Every key a stored record may carry.
  static const storedKeys = {
    'v', 'clientId', 'accountId', 'conversationId', 'order', 'composedAt', 'text', 'replyToMessageId', //
    'replyPreview', 'attachments', 'status', 'attempts', 'internalRetries', 'reuploads', 'notBefore', 'lastError',
  };

  /// THE wire idempotency key. Minted once at compose; never re-minted by
  /// retry, reload, reconnect or re-upload.
  Uuid clientId;

  /// The author it is sent as. Other accounts' entries are never sent.
  final Uuid accountId;

  /// What the send names; `ack.conversationId` wins once acked.
  final Uuid conversationId;

  /// Per-device, drawn inside the insert's own transaction. Drain order. NOT
  /// the device clock.
  int order;

  /// Device clock. Local display and ordering only — never sent, never the
  /// message's time.
  final String composedAt;
  final String? text;

  /// The only reply field sent.
  final Uuid? replyToMessageId;

  /// Display-only. Never sent.
  final ReplyRef? replyPreview;

  /// Replaced, never mutated in place: a copy shares the list.
  List<OutboundAttachmentDraft> attachments;

  /// `sending` versus `queued` is DERIVED, never stored, and there is no
  /// stored ack.
  OutboxStatus status;

  /// Frames written for this entry.
  int attempts;

  /// Retryable refusals received — one per refusal, not per context.
  int internalRetries;
  int reuploads;

  /// Earliest resend under backoff / `retryAfterSec`, wall clock.
  String? notBefore;
  OutboxError? lastError;

  /// The status as stored. Only the `persistSending` fault ever makes it
  /// anything but `status`'s own name.
  String? storedStatusOverride;

  OutboxEntry copy() => OutboxEntry(
        clientId: clientId,
        accountId: accountId,
        conversationId: conversationId,
        order: order,
        composedAt: composedAt,
        text: text,
        replyToMessageId: replyToMessageId,
        replyPreview: replyPreview,
        attachments: attachments,
        status: status,
        attempts: attempts,
        internalRetries: internalRetries,
        reuploads: reuploads,
        notBefore: notBefore,
        lastError: lastError,
      )..storedStatusOverride = storedStatusOverride;

  Map<String, Object?> toJson() => {
        'v': 1,
        'clientId': clientId,
        'accountId': accountId,
        'conversationId': conversationId,
        'order': order,
        'composedAt': composedAt,
        if (text != null) 'text': text,
        if (replyToMessageId != null) 'replyToMessageId': replyToMessageId,
        if (replyPreview != null) 'replyPreview': replyPreview!.toJson(),
        if (attachments.isNotEmpty) 'attachments': [for (final a in attachments) a.toJson()],
        'status': storedStatusOverride ?? status.name,
        'attempts': attempts,
        'internalRetries': internalRetries,
        'reuploads': reuploads,
        if (notBefore != null) 'notBefore': notBefore,
        if (lastError != null) 'lastError': lastError!.toJson(),
      };
}

/// What `compose` is given; everything else on an entry is the outbox's.
final class OutboxDraft {
  const OutboxDraft({required this.conversationId, this.text, this.replyToMessageId, this.replyPreview, this.attachments = const []});

  final Uuid conversationId;
  final String? text;
  final Uuid? replyToMessageId;
  final ReplyRef? replyPreview;
  final List<OutboundAttachmentDraft> attachments;
}

// ── seam 1: persistence ─────────────────────────────────────────────────────

/// Every write completes only after its transaction has committed. `add`
/// allocates `order` inside the same transaction as the insert, so two contexts
/// composing at once cannot draw the same value.
///
/// AN ENTRY AND ITS MEDIA ARE NEVER STORED APART (CANT-201 ruling 0). `media`
/// is one item per attachment, by index, null where the attachment has none to
/// hold; it is written in the entry's own transaction, and `delete` removes it
/// in the transaction that removes the entry.
abstract interface class OutboxStore {
  /// Allocate `order` (highest for the account, plus one) and insert, with the
  /// entry's media, in one transaction. `entry.order` is ignored.
  Future<OutboxEntry> add(OutboxEntry entry, [List<Uint8List?> media = const []]);

  /// Put as given. Nothing in the outbox calls this for a new entry — the
  /// `orderOutsideTxn` fault does, and seeding does. Media given replaces what
  /// is held at its index; media not given is left as it is.
  Future<void> put(OutboxEntry entry, [List<Uint8List?> media = const []]);

  /// Read-modify-write in one transaction. Does nothing, and completes with
  /// null, when the entry is gone — so a late counter update can never
  /// resurrect an entry a record has already settled.
  Future<OutboxEntry?> update(Uuid clientId, void Function(OutboxEntry e) mutate);

  /// Removes the entry and every item of its media, in one transaction.
  Future<void> delete(Uuid clientId);

  /// Every entry, every account, in `order`. READS NO MEDIA: the lock's holder
  /// lists on a timer, and a listing that carried the bytes would read every
  /// unsent recording each time it fired.
  Future<List<OutboxEntry>> list();

  /// The media held for attachment `index` of the entry, as it was composed,
  /// or null when none is held — the entry is gone, or never had any there.
  Future<Uint8List?> media(Uuid clientId, int index);
  void close();
}

// ── seam 2: the transport ───────────────────────────────────────────────────

/// What the outbox needs from a session. `TransportOutbox` adapts the
/// transport onto it: `ready` from `subscribe()`, `closed` from `onSessionEnd`
/// — `bare1008` is the transport's classification, consumed here and never
/// re-derived — `ack` and `error` from what `send` completes with, and
/// `message` from `onApply`.
///
/// `sendFrame` returns nothing: an answer, if any, arrives as an event.
abstract interface class OutboxTransport {
  bool get isReady;
  void sendFrame(ClientSend frame);

  /// Returns the unsubscribe.
  void Function() subscribe(void Function(OutboxTransportEvent event) listener);
}

sealed class OutboxTransportEvent {
  const OutboxTransportEvent();
}

final class SessionReady extends OutboxTransportEvent {
  const SessionReady();
}

final class SendAcked extends OutboxTransportEvent {
  const SendAcked(this.ack);

  final ServerAck ack;
}

final class SendErrored extends OutboxTransportEvent {
  const SendErrored(this.error);

  final ServerError error;
}

/// A record held — a `message` frame or a `/sync` record. Only its `clientId`
/// is read.
final class RecordHeld extends OutboxTransportEvent {
  const RecordHeld(this.clientId);

  final Uuid? clientId;
}

/// The session is no longer ready. `terminal` is CANT-31 §6's terminal state,
/// which never deletes the local store.
final class SessionClosed extends OutboxTransportEvent {
  const SessionClosed({required this.bare1008, this.terminal = false});

  final bool bare1008;
  final bool terminal;
}

/// CANT-24 obligation 4's discard-and-bootstrap. It wipes server-derived
/// state, and the outbox is not server-derived.
final class Bootstrapped extends OutboxTransportEvent {
  const Bootstrapped();
}

// ── the multi-context piece (§8) ────────────────────────────────────────────

/// `catenary.outbox`. `onGranted` runs once the lock is held, and the returned
/// function releases it — or withdraws the request if it has not been granted
/// yet.
abstract interface class DrainLock {
  void Function() request(void Function() onGranted);
}

// ── attachments (§10) ───────────────────────────────────────────────────────

/// `compose` would not take the draft, and wrote nothing. The message is for
/// the person who composed it.
final class ComposeRefused implements Exception {
  const ComposeRefused(this.message);

  /// CANT-201 ruling 1: a picked file is not composed without a ready session.
  static const pickedFileOffline = "A file can't be attached while offline";

  /// A terminal client drains nothing, so it takes no new attachment to hold.
  static const terminal = 'This device cannot send';

  /// An entry is reported composed only with its media held (ruling 0).
  static const noMedia = 'An attachment needs its media';

  final String message;

  @override
  String toString() => message;
}

/// Uploads one attachment and completes with its handle.
abstract interface class Uploader {
  Future<Uuid> upload(OutboxEntry entry, OutboundAttachmentDraft attachment);
}

final class UploadRefused implements Exception {
  const UploadRefused(this.message);

  final String message;

  @override
  String toString() => message;
}

/// §10's default until a real uploader exists: it refuses at once with a clear
/// message, and an attachment entry goes to `failed` with that inline error —
/// never a loop.
final class RefusingUploader implements Uploader {
  const RefusingUploader();

  static const message = "Attachments can't be sent yet";

  @override
  Future<Uuid> upload(OutboxEntry entry, OutboundAttachmentDraft attachment) => Future.error(const UploadRefused(message));
}

// ── the render projection's input ───────────────────────────────────────────

/// `queued` / `sending` / `sent` are derived; `failed` is the stored status.
/// None is on the wire.
enum OutboxState { queued, sending, sent, failed }

final class OutboxItem {
  const OutboxItem({required this.entry, required this.state, this.ack, required this.retrying});

  final OutboxEntry entry;
  final OutboxState state;

  /// Held in memory only, this session.
  final ServerAck? ack;

  /// §6: `pending` with three or more retryable refusals received.
  final bool retrying;
}

/// Test-only negative controls (the reference's rollout table, under its
/// names). Each one breaks exactly one rule, and the criterion that rule serves
/// must FAIL with it on. Never set outside the criteria suite.
final class OutboxFaults {
  const OutboxFaults({
    this.sendBeforePersist = false,
    this.remintOnRetry = false,
    this.persistSending = false,
    this.settlePendingOnly = false,
    this.misclassifyRetryable = false,
    this.misjudgeRetryBudget = false,
    this.retryNonRetryable = false,
    this.resendAfterBare1008 = false,
    this.resendFailedWithoutRetry = false,
    this.deleteOnTerminal = false,
    this.crossAccountLeak = false,
    this.orderOutsideTxn = false,
    this.everyTabDrains = false,
    this.lockWithoutReady = false,
  });

  static const none = OutboxFaults();

  final bool sendBeforePersist;
  final bool remintOnRetry;
  final bool persistSending;
  final bool settlePendingOnly;
  final bool misclassifyRetryable;
  final bool misjudgeRetryBudget;
  final bool retryNonRetryable;
  final bool resendAfterBare1008;
  final bool resendFailedWithoutRetry;
  final bool deleteOnTerminal;
  final bool crossAccountLeak;
  final bool orderOutsideTxn;

  /// A context drains without the lock.
  final bool everyTabDrains;

  /// A context takes the lock while its session is not `ready`.
  final bool lockWithoutReady;
}

/// The store's own two faults, which live below the `Outbox`.
final class StoreFaults {
  const StoreFaults({this.neverCompletes = false, this.relaxedDurability = false});

  static const none = StoreFaults();

  /// The insert succeeds and the transaction is then rolled back, so the write
  /// never completes.
  final bool neverCompletes;

  /// A connection opened without `synchronous = FULL`.
  final bool relaxedDurability;
}
