/// Link previews on the Dart client (CANT-324, row 5 of CANT-308): the
/// detection rule, the `POST /link-previews` call, and the composer's
/// client-local state over both. The protocol half only: nothing here knows a
/// widget, and the card is rendered by the app (CANT-325).
///
/// THE DETECTION RULE IS PINNED, AND THE TABLE IS SHARED. The URL is the first
/// token matching `https?://` plus a non-space, up to the next whitespace, with
/// trailing `.,;:!?)` trimmed; a request is made only when its host has a dot.
/// `schema/vectors/link-detection.json` is the one definition (CANT-329): this
/// file, the web client and the app all assert it, and none carries a copy.
///
/// EVERY FAILURE OF THE FETCH IS THE SAME ANSWER. A `204` is "no card", and so
/// is a `404` (the feature is off server-side), a transport failure, a body
/// that is not Catenary's and a `5xx`: [PreviewNone] carries no reason, because
/// the server deliberately gives none and a client that invented one would be
/// claiming something the server cannot keep. Only a `429` is told apart, and
/// only for the wait it names.
///
/// A PREVIEW NEVER BLOCKS A SEND. [LinkPreviewDraft.attachment] reads the
/// state and answers at once; a preview still loading contributes nothing, the
/// message goes without a card, and the late answer is discarded.
library;

import 'dart:async';
import 'dart:convert';

import 'package:catenary_wire/catenary_wire.dart';

import 'credential.dart';
import 'io_seams.dart';
import 'outbox/types.dart';
import 'seams.dart';

// ── detection ───────────────────────────────────────────────────────────────

final _urlToken = RegExp(r'https?://\S+');
const _trailing = '.,;:!?)';

/// The URL a draft would ask a preview for, or null when it has none.
String? detectLinkUrl(String text) {
  final m = _urlToken.firstMatch(text);
  if (m == null) return null;
  var url = m.group(0)!;
  while (url.isNotEmpty && _trailing.contains(url[url.length - 1])) {
    url = url.substring(0, url.length - 1);
  }
  // Nothing after the scheme once the punctuation is gone is not a link.
  final rest = url.substring(url.indexOf('://') + 3);
  return rest.isEmpty ? null : url;
}

/// Whether a detected [url] is complete enough to ask about: it parses and its
/// host has a dot and does not end in one (a half-typed `https://exa` would
/// otherwise be fetched). A courtesy only; the server decides what it fetches.
bool linkUrlIsAskable(String url) {
  final u = Uri.tryParse(url);
  if (u == null || !(u.scheme == 'http' || u.scheme == 'https')) return false;
  return u.host.contains('.') && !u.host.endsWith('.');
}

// ── the call ────────────────────────────────────────────────────────────────

sealed class PreviewResult {
  const PreviewResult();
}

/// A card, and the single-use handle that binds it to a message. The handle is
/// uploader-bound: only the account that asked can send it.
final class PreviewReady extends PreviewResult {
  const PreviewReady(this.preview, this.handle);

  final LinkAttachment preview;
  final Uuid handle;
}

/// No card, for no reason the client is told.
final class PreviewNone extends PreviewResult {
  const PreviewNone();
}

/// `429 rate_limited`: not now, and the server's own wait.
final class PreviewRateLimited extends PreviewResult {
  const PreviewRateLimited(this.retryAfterSec);

  final int? retryAfterSec;
}

final class LinkPreviewApi {
  LinkPreviewApi({
    required String baseUrl,
    required CredentialSeam credential,
    HttpFetch? fetch,
    this.timeout = const Duration(seconds: 12),
  })  : _base = baseUrl.replaceFirst(RegExp(r'/+$'), ''),
        _cred = credential,
        _fetch = fetch ?? ioHttpFetch;

  final String _base;
  final CredentialSeam _cred;
  final HttpFetch _fetch;
  final Duration timeout;

  bool _off = false;

  /// True once the server has answered 404: previews are not wired there, and
  /// no further request is made from this object. A new object (a new
  /// session) asks again.
  bool get unavailable => _off;

  /// `POST /link-previews` for [url].
  Future<PreviewResult> fetch(String url) async {
    if (_off) return const PreviewNone();
    var retried = false;
    while (true) {
      final Credential cred;
      final HttpAnswer res;
      try {
        cred = await _cred.current();
        final cancel = Completer<void>();
        final timer = Timer(timeout, () {
          if (!cancel.isCompleted) cancel.complete();
        });
        try {
          res = await Future.any([
            _fetch(HttpExchange(
              method: 'POST',
              url: Uri.parse('$_base/link-previews'),
              headers: {'Authorization': 'Bearer ${cred.accessToken}', 'Content-Type': 'application/json'},
              body: jsonEncode(LinkPreviewRequest(url: url).toJson()),
              abort: cancel.future,
            )),
            cancel.future.then<HttpAnswer>((_) => throw TimeoutException('link preview: no response')),
          ]);
        } finally {
          timer.cancel();
        }
      } catch (_) {
        return const PreviewNone();
      }
      if (isCatenaryUnauthorized(res.status, res.body)) {
        // The token may simply have expired: one repair, one retry. Past that
        // the credential layer owns the signed-out state, not this call.
        try {
          if (retried || !await _cred.onSyncUnauthorized(cred)) return const PreviewNone();
        } catch (_) {
          return const PreviewNone();
        }
        retried = true;
        continue;
      }
      switch (res.status) {
        case 200:
          try {
            final r = LinkPreviewResponse.fromJson(jsonDecode(res.body));
            return PreviewReady(r.preview, r.uploadId);
          } catch (_) {
            return const PreviewNone();
          }
        case 429:
          int? wait;
          try {
            wait = ServerError.fromJson(jsonDecode(res.body)).retryAfterSec;
          } catch (_) {}
          return PreviewRateLimited(wait);
        case 404:
          _off = true;
          return const PreviewNone();
      }
      return const PreviewNone();
    }
  }
}

// ── the composer's state ────────────────────────────────────────────────────

/// Client-local, never on the wire: the same category as `sending`, `queued`
/// and `failed`.
enum LinkPreviewState { idle, loading, ready, none, dismissed }

/// One draft's preview. Feed it the draft's text; read [attachment] when the
/// message is sent.
final class LinkPreviewDraft {
  LinkPreviewDraft({required LinkPreviewApi api, Timers timers = const SystemTimers(), this.debounceMs = 600, void Function()? onChange})
      : _api = api,
        _timers = timers,
        _onChange = onChange;

  final LinkPreviewApi _api;
  final Timers _timers;
  final int debounceMs;
  final void Function()? _onChange;

  LinkPreviewState _state = LinkPreviewState.idle;
  PreviewReady? _ready;
  String? _url;
  Object? _timer;
  var _gen = 0;

  LinkPreviewState get state => _state;

  /// The card to show, only while [state] is [LinkPreviewState.ready].
  LinkAttachment? get preview => _state == LinkPreviewState.ready ? _ready?.preview : null;

  /// Set after a `429`, for a caller that wants to say when it will try again.
  int? retryAfterSec;

  /// The draft's text changed. A request is made only once a complete URL has
  /// been idle for [debounceMs], and never for a dismissed draft.
  void textChanged(String text) {
    if (_state == LinkPreviewState.dismissed) return;
    final url = detectLinkUrl(text);
    if (url == _url && _state != LinkPreviewState.idle) return;
    _cancel();
    _url = url;
    _ready = null;
    _set(LinkPreviewState.idle);
    if (url == null || !linkUrlIsAskable(url)) return;
    final gen = _gen;
    _timer = _timers.setTimeout(() {
      _timer = null;
      if (gen == _gen) unawaited(_run(url, gen));
    }, debounceMs);
  }

  Future<void> _run(String url, int gen) async {
    retryAfterSec = null;
    _set(LinkPreviewState.loading);
    final r = await _api.fetch(url);
    if (gen != _gen) return;
    switch (r) {
      case PreviewReady():
        _ready = r;
        _set(LinkPreviewState.ready);
      case PreviewRateLimited(:final retryAfterSec):
        this.retryAfterSec = retryAfterSec;
        _set(LinkPreviewState.none);
      case PreviewNone():
        _set(LinkPreviewState.none);
    }
  }

  /// The person closed the card. The draft is not asked again, and what was
  /// fetched is dropped.
  void dismiss() {
    _cancel();
    _ready = null;
    _set(LinkPreviewState.dismissed);
  }

  /// The draft was sent or cleared: back to idle, and a new draft may ask.
  void reset() {
    _cancel();
    _url = null;
    _ready = null;
    retryAfterSec = null;
    _set(LinkPreviewState.idle);
  }

  /// The attachment a send carries RIGHT NOW. Never waits: a preview that is
  /// loading, absent or dismissed is null, and the message goes without a card.
  OutboundAttachmentDraft? attachment() {
    final r = _state == LinkPreviewState.ready ? _ready : null;
    return r == null ? null : OutboundAttachmentDraft.link(r.handle);
  }

  void _cancel() {
    _gen++;
    if (_timer != null) _timers.clearTimeout(_timer);
    _timer = null;
  }

  void _set(LinkPreviewState s) {
    _state = s;
    _onChange?.call();
  }
}
