/// The three REST calls that start a conversation (CANT-253's app half,
/// CANT-273): `GET /users` for the roster, `POST /conversations/direct` for a
/// direct and `POST /conversations` for a group.
///
/// THE REQUEST BODIES ARE THE GENERATED ENCODERS' AND THE ANSWERS ARE THE
/// GENERATED DECODERS' (`DirectConversationRequest`, `CreateGroupRequest`,
/// `RosterResponse`, `Conversation`); nothing here is a second definition of
/// a wire type. A 200 or 201 whose body does not decode is not an answer from
/// Catenary (a captive portal's page is a 200) and is [StartUnreachable].
///
/// NOTHING IS APPLIED TO THE JOURNAL HERE. A conversation the server made
/// reaches this device the way every other does, through `/sync`; the caller
/// triggers `Transport.catchUp` and reads the journal. The returned
/// `Conversation` is only how the caller learns the id to wait for.
library;

import 'dart:async';
import 'dart:convert';

import 'package:catenary_wire/catenary_wire.dart';

import 'credential.dart';
import 'io_seams.dart';
import 'seams.dart';

/// What a start call came to.
sealed class StartResult<T> {
  const StartResult();
}

/// The server answered and stored what was asked. [created] is false for a
/// find that found (200 on a direct, a replayed `request_id`).
final class Started<T> extends StartResult<T> {
  const Started(this.value, {required this.created});

  final T value;
  final bool created;
}

/// The server answered and refused. [code] is the wire's `code` when the body
/// carried one (`conversation_not_found`), and null otherwise.
final class StartRefused<T> extends StartResult<T> {
  const StartRefused(this.status, this.code);

  final int status;
  final String? code;
}

/// Nothing usable came back: no response, a timeout, a body that is not a
/// Catenary answer, or a credential that the server refused even after the
/// refresh. Retryable, and nothing is known about whether it was made.
final class StartUnreachable<T> extends StartResult<T> {
  const StartUnreachable();
}

final class ConversationsApi {
  ConversationsApi({
    required String baseUrl,
    required CredentialSeam credential,
    HttpFetch? fetch,
    this.timeout = const Duration(seconds: 15),
  })  : _base = baseUrl.replaceFirst(RegExp(r'/+$'), ''),
        _cred = credential,
        _fetch = fetch ?? ioHttpFetch;

  final String _base;
  final CredentialSeam _cred;
  final HttpFetch _fetch;
  final Duration timeout;

  /// `GET /users`: every active person other than the caller.
  Future<StartResult<List<RosterEntry>>> roster() =>
      _call('GET', '/users', null, (status, json) => status == 200 ? Started(RosterResponse.fromJson(json).users, created: false) : null);

  /// `POST /conversations/direct`: the direct with [handle], found or made.
  Future<StartResult<Conversation>> startDirect(String handle) => _call(
        'POST',
        '/conversations/direct',
        DirectConversationRequest(handle: handle).toJson(),
        (status, json) => status == 200 || status == 201 ? Started(Conversation.fromJson(json), created: status == 201) : null,
      );

  /// `POST /conversations`: a group of the caller and [memberHandles].
  /// [requestId] makes a replay safe (CANT-253 ruling 2 → option 1): the same
  /// id from the same caller returns the first room with 200.
  Future<StartResult<Conversation>> createGroup(String name, List<String> memberHandles, {String? requestId}) => _call(
        'POST',
        '/conversations',
        CreateGroupRequest(name: name, memberHandles: memberHandles, requestId: requestId).toJson(),
        (status, json) => status == 200 || status == 201 ? Started(Conversation.fromJson(json), created: status == 201) : null,
      );

  Future<StartResult<T>> _call<T>(
    String method,
    String path,
    Map<String, dynamic>? body,
    StartResult<T>? Function(int status, Object? json) accept,
  ) async {
    final fetch = _fetch;
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
            fetch(HttpExchange(
              method: method,
              url: Uri.parse('$_base$path'),
              headers: {
                'Authorization': 'Bearer ${cred.accessToken}',
                if (body != null) 'Content-Type': 'application/json',
              },
              body: body == null ? null : jsonEncode(body),
              abort: cancel.future,
            )),
            cancel.future.then<HttpAnswer>((_) => throw TimeoutException('conversations: no response')),
          ]);
        } finally {
          timer.cancel();
        }
      } catch (_) {
        return StartUnreachable<T>();
      }
      if (isCatenaryUnauthorized(res.status, res.body)) {
        // The token may simply have expired: one refresh, one retry.
        if (retried || !await _cred.onSyncUnauthorized(cred)) return StartUnreachable<T>();
        retried = true;
        continue;
      }
      Object? json;
      try {
        json = jsonDecode(res.body);
      } on FormatException {
        json = null;
      }
      if (res.status >= 200 && res.status < 300) {
        try {
          return accept(res.status, json) ?? StartUnreachable<T>();
        } catch (_) {
          return StartUnreachable<T>();
        }
      }
      // A hop's error page is not Catenary refusing: only a JSON object body
      // is read for a code, and a 5xx is never a refusal of this request.
      if (res.status >= 500 || json is! Map) return StartUnreachable<T>();
      final code = json['code'];
      return StartRefused<T>(res.status, code is String ? code : null);
    }
  }
}
