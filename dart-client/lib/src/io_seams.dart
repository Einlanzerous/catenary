/// The seams' `dart:io` implementations — what a plain `dart` executable, and
/// an app with nothing better to offer, runs the transport over. The
/// counterpart of the browser defaults in web/src/transport/seams.ts; `dart:io`
/// is not Flutter, and runs under either.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'seams.dart';

/// A `dart:io` WebSocket, dialed as it is constructed.
WebSocketLike ioWebSocket(Uri url, List<String> protocols) => _IoWebSocket(url, protocols);

final class _IoWebSocket implements WebSocketLike {
  _IoWebSocket(Uri url, List<String> protocols) {
    WebSocket.connect(url.toString(), protocols: protocols).then(_opened, onError: (Object _) => _closed(null));
  }

  WebSocket? _ws;
  var _closing = false;
  var _closed_ = false;

  @override
  void Function()? onOpen;

  @override
  void Function(Object? data)? onMessage;

  @override
  void Function(int? code)? onClose;

  void _opened(WebSocket ws) {
    if (_closing) {
      // Closed before it opened: the caller has already been told.
      ws.close().ignore();
      return;
    }
    _ws = ws;
    ws.listen(
      (data) => onMessage?.call(data),
      onDone: () => _closed(ws.closeCode),
      onError: (Object _) => _closed(ws.closeCode),
      cancelOnError: true,
    );
    onOpen?.call();
  }

  void _closed(int? code) {
    if (_closed_) return;
    _closed_ = true;
    onClose?.call(code);
  }

  @override
  void send(String data) {
    final ws = _ws;
    if (ws == null || ws.readyState != WebSocket.open) throw StateError('the socket is not open');
    ws.add(data);
  }

  @override
  void close([int? code, String? reason]) {
    _closing = true;
    _ws?.close(code, reason).ignore();
  }
}

HttpClient? _sharedClient;

/// One request through a shared `dart:io` `HttpClient`.
Future<HttpAnswer> ioHttpFetch(HttpExchange exchange) async {
  final client = _sharedClient ??= HttpClient();
  var aborted = false;
  HttpClientRequest? request;
  unawaited(exchange.abort.then((_) {
    aborted = true;
    request?.abort();
  }));
  final req = request = await client.openUrl(exchange.method, exchange.url);
  if (aborted) {
    req.abort();
    throw const HttpException('aborted before the request was sent');
  }
  exchange.headers.forEach(req.headers.set);
  final body = exchange.body;
  if (body != null) req.add(utf8.encode(body));
  final res = await req.close();
  final text = await utf8.decoder.bind(res).join();
  final headers = <String, String>{};
  res.headers.forEach((name, values) => headers[name.toLowerCase()] = values.join(', '));
  return HttpAnswer(res.statusCode, text, headers);
}

/// Structured log lines on stderr, one JSON object each. Never stdout: a
/// driver's stdout is its protocol.
final class StderrLogger implements Logger {
  const StderrLogger();

  @override
  void info(String msg, [Map<String, Object?> fields = const {}]) => _line('info', msg, fields);

  @override
  void warn(String msg, [Map<String, Object?> fields = const {}]) => _line('warn', msg, fields);

  void _line(String level, String msg, Map<String, Object?> fields) {
    stderr.writeln(jsonEncode({'level': level, 'msg': 'catenary transport: $msg', ...fields}));
  }
}
