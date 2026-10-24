/// The real `Uploader` (CANT-316, CANT-247): one `POST {origin}/uploads` per
/// attachment, the media's bytes as the raw body, and the `upload_id` out of
/// the `201`.
///
/// THE SEAM IS ITS OWN. `HttpExchange.body` is a `String?` and `ioHttpFetch`
/// writes `utf8.encode(body)`, which cannot carry a JPEG; the transport's seam
/// is left as it is and an upload goes through [UploadFetch].
///
/// CREDENTIALS GO THE WAY EVERY OTHER REST CALL'S DO: the call shape of
/// `ConversationsApi._call` — a bearer from the credential layer, and on
/// Catenary's own 401 one repair and one retry. The token is in a header and
/// nowhere else: never in a URL, a log line or one of the messages below,
/// which are shown to the person.
///
/// THE BYTES ARE THE STORE'S. The outbox hands an uploader no bytes
/// (`OutboxStore.media`), so this reads the attachment's from the store beside
/// the entry, by the entry's `clientId` and the attachment's index (CANT-247
/// ruling 3 → A).
///
/// EVERY UPLOAD SETTLES WITHIN [uploadDeadline]. The outbox keeps no deadline
/// of its own (CANT-220 ruling 1) and a future that never completes holds an
/// entry `pending` until relaunch, so the bound covers the whole call,
/// including the credential work that precedes the request.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:catenary_wire/catenary_wire.dart';

import 'credential.dart';
import 'io_seams.dart';
import 'outbox/types.dart';
import 'seams.dart';

// ── the bound (CANT-247 ruling 2 → A) ───────────────────────────────────────
//
// ONE TOTAL DEADLINE, SIZE-SCALED: 30 s plus the time the bytes take at
// 32 KiB/s (about 256 kbit/s), capped at 10 min. THESE ARE JUDGMENTS, NOT
// MEASUREMENTS: nobody has measured the group's links. They are constants and
// not config because nothing about a phone's link is per deployment, and they
// are here, once, so a later measurement changes one place.

/// The floor: what a request that moves no bytes is allowed.
const uploadBaseMs = 30000;

/// The slowest link an upload is still expected to finish over.
const uploadFloorBytesPerSecond = 32 * 1024;

/// The most any upload may take, whatever its size.
const uploadCeilingMs = 10 * 60 * 1000;

/// How long an upload of [bytes] bytes may take, in all.
int uploadDeadline(int bytes) {
  final ms = uploadBaseMs + (bytes * 1000 / uploadFloorBytesPerSecond).ceil();
  return ms > uploadCeilingMs ? uploadCeilingMs : ms;
}

// ── the words (user copy: the outbox stores `$e` as the entry's inline error) ─

const uploadDidNotFinish = "The upload didn't finish. Check the connection and try again.";
const uploadSignedOut = "You're signed out, so this can't be sent. Sign in again.";
const uploadUnavailable = "Uploads aren't available on this server yet.";
const uploadServerTrouble = 'The server had trouble with that upload. Try again in a moment.';
const uploadImageTooLarge = 'That image is too large to send.';
const uploadImageType = "That file type isn't supported. Use a JPEG, PNG, GIF or WebP image.";
const uploadVoiceTooLarge = 'That recording is too long to send.';
const uploadVoiceType = "That recording's format isn't supported.";
const uploadRefusedGeneric = 'The server refused that upload.';
const uploadMediaMissing = "That attachment's file is no longer held, so it can't be sent. Delete this message and attach it again.";

final _uuid = RegExp(r'^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$');

final class HttpUploader implements Uploader {
  HttpUploader({
    required String baseUrl,
    required CredentialSeam credential,
    required OutboxStore store,
    UploadFetch? fetch,
    Timers timers = const SystemTimers(),
  })  : _base = baseUrl.replaceFirst(RegExp(r'/+$'), ''),
        _cred = credential,
        _store = store,
        _fetch = fetch ?? ioUploadFetch,
        _timers = timers;

  final String _base;
  final CredentialSeam _cred;
  final OutboxStore _store;
  final UploadFetch _fetch;
  final Timers _timers;

  @override
  Future<Uuid> upload(OutboxEntry entry, OutboundAttachmentDraft attachment) async {
    final index = entry.attachments.indexWhere((a) => identical(a, attachment));
    if (index < 0) throw const UploadRefused(uploadMediaMissing);
    final Uint8List? bytes;
    try {
      bytes = await _store.media(entry.clientId, index);
    } catch (_) {
      throw const UploadRefused(uploadMediaMissing);
    }
    if (bytes == null) throw const UploadRefused(uploadMediaMissing);

    final abort = Completer<void>();
    final done = Completer<Uuid>();
    final timer = _timers.setTimeout(() {
      if (!abort.isCompleted) abort.complete();
      if (!done.isCompleted) done.completeError(const UploadRefused(uploadDidNotFinish));
    }, uploadDeadline(bytes.length));
    _run(attachment, bytes, abort.future).then((id) {
      if (!done.isCompleted) done.complete(id);
    }, onError: (Object e) {
      if (!done.isCompleted) done.completeError(e);
    });
    try {
      return await done.future;
    } finally {
      _timers.clearTimeout(timer);
    }
  }

  Future<Uuid> _run(OutboundAttachmentDraft a, Uint8List bytes, Future<void> abort) async {
    final query = 'kind=${Uri.encodeQueryComponent(a.kind)}${a.filename == null ? '' : '&filename=${Uri.encodeQueryComponent(a.filename!)}'}';
    final url = Uri.parse('$_base/uploads?$query');
    var retried = false;
    while (true) {
      final Credential cred;
      final HttpAnswer res;
      try {
        await _cred.refreshIfDue();
        cred = await _cred.current();
        res = await _fetch(UploadExchange(
          url: url,
          headers: {'Authorization': 'Bearer ${cred.accessToken}', 'Content-Type': 'application/octet-stream'},
          body: bytes,
          abort: abort,
        ));
      } catch (_) {
        // Deliberately not `$e`: nothing from the network or the credential
        // layer goes into words the person reads.
        throw const UploadRefused(uploadDidNotFinish);
      }
      if (isCatenaryUnauthorized(res.status, res.body)) {
        // The token may simply have expired: one repair, one retry.
        bool repaired;
        try {
          repaired = !retried && await _cred.onSyncUnauthorized(cred);
        } catch (_) {
          throw const UploadRefused(uploadDidNotFinish);
        }
        if (!repaired) throw const UploadRefused(uploadSignedOut);
        retried = true;
        continue;
      }
      return _answer(a.kind, res);
    }
  }

  Uuid _answer(String kind, HttpAnswer res) {
    final image = kind == 'image';
    switch (res.status) {
      case 201:
        try {
          final json = jsonDecode(res.body);
          final id = json is Map ? json['upload_id'] : null;
          if (id is String && _uuid.hasMatch(id)) return id;
        } on FormatException {
          // not Catenary's
        }
        throw const UploadRefused(uploadDidNotFinish);
      case 404:
        throw const UploadRefused(uploadUnavailable);
      case 413:
        throw UploadRefused(image ? uploadImageTooLarge : uploadVoiceTooLarge);
      case 415:
        throw UploadRefused(image ? uploadImageType : uploadVoiceType);
    }
    if (res.status >= 500) throw const UploadRefused(uploadServerTrouble);
    if (res.status >= 400 && _isCatenaryBody(res.body)) throw const UploadRefused(uploadRefusedGeneric);
    // A hop's error page, a captive portal's 200: not Catenary answering.
    throw const UploadRefused(uploadDidNotFinish);
  }

  static bool _isCatenaryBody(String body) {
    try {
      return jsonDecode(body) is Map;
    } on FormatException {
      return false;
    }
  }
}
