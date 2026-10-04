/// The `dart:io` seams against a real loopback server: a transport with no
/// fake under it dials, says hello, catches up over HTTP, sends, and reads the
/// close status the server sent. The fakes prove the rules; this proves the
/// defaults a plain `dart` executable runs are wired to them.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';

Future<void> until(bool Function() done, String what) async {
  final deadline = DateTime.now().add(const Duration(seconds: 10));
  while (!done()) {
    if (DateTime.now().isAfter(deadline)) fail('timed out waiting for $what');
    await Future<void>.delayed(const Duration(milliseconds: 5));
  }
}

void main() {
  test('a transport over dart:io dials /ws with both subprotocols, syncs with a bearer, and reads a close status', () async {
    final sockets = <WebSocket>[];
    final hellos = <Json>[];
    final offered = <List<String>?>[];
    final syncs = <({String? authorization, String after})>[];
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    addTearDown(() => server.close(force: true));
    server.listen((req) async {
      if (req.uri.path == '/ws') {
        offered.add(req.headers['sec-websocket-protocol']);
        final ws = await WebSocketTransformer.upgrade(req, protocolSelector: (protocols) => protocols.first);
        sockets.add(ws);
        ws.listen((data) {
          final f = jsonDecode(data as String) as Json;
          if (f['type'] == 'hello') {
            hellos.add(f);
            ws.add(jsonEncode(ready(heartbeatIntervalSec: 30).toJson()));
          } else if (f['type'] == 'send') {
            ws.add(jsonEncode(ServerAck(
              clientId: f['client_id'] as String,
              messageId: uuid(6001),
              conversationId: conv,
              seq: 4,
              logSeq: 4,
              at: at,
            ).toJson()));
          }
        });
        return;
      }
      syncs.add((authorization: req.headers.value('authorization'), after: req.uri.queryParameters['after']!));
      req.response
        ..headers.contentType = ContentType.json
        ..write(jsonEncode(bootstrapPage(3, [message(1), message(2), message(3)]).toJson()));
      await req.response.close();
    });

    final logger = ListLogger();
    final t = createTransport(TransportConfig(
      baseUrl: 'http://127.0.0.1:${server.port}',
      credential: HeldCredential(Credential(userId: me, deviceId: device, accessToken: token)),
      logger: logger,
    ));
    addTearDown(t.stop);
    final ends = <SessionEnd>[];
    t.onSessionEnd(ends.add);
    t.start();
    await until(() => t.status().ready && t.status().caughtUp, 'ready and caught up');

    expect(offered.single!.join(', '), 'catenary.v1, catenary.token.$token');
    expect(hellos.single['device_id'], device);
    expect(syncs.first, (authorization: 'Bearer $token', after: '0'));
    expect(t.status().cursor, 3);
    expect(t.status().messages, 3);

    final ack = await t.send(ClientSend(clientId: uuid(5001), conversationId: conv, text: 'over a real socket'));
    expect(ack.messageId, uuid(6001));

    await sockets.single.close(4001, 'revoked');
    await until(() => ends.isNotEmpty, 'the session end');
    expect(ends.single.closeCode, 4001);
    expect(t.status().terminal.kind, TerminalKind.protocol);
  });

  test('a dial nothing answers is a dial error, and a /sync nothing answers is a failed catch-up', () async {
    // A port that was bound and is now closed: nothing listens on it.
    final gone = await ServerSocket.bind(InternetAddress.loopbackIPv4, 0);
    final port = gone.port;
    await gone.close();
    final t = createTransport(TransportConfig(
      baseUrl: 'http://127.0.0.1:$port',
      credential: HeldCredential(Credential(userId: me, deviceId: device, accessToken: token)),
      logger: const SilentLogger(),
    ));
    addTearDown(t.stop);
    t.start();
    await until(() => t.status().stats.dialErrors > 0 && t.status().stats.syncErrors > 0, 'both to fail');
    expect(t.status().connected, isFalse);
    expect(t.status().stats.closeStatuses, isEmpty, reason: 'a dial that never opened is not a close');
    expect(t.status().nextDialAt, isNotNull, reason: 'and it is waiting to dial again');
  });

  test('ioHttpFetch carries the method, headers and body out and the status, headers and body back', () async {
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    addTearDown(() => server.close(force: true));
    server.listen((req) async {
      final body = await utf8.decoder.bind(req).join();
      req.response
        ..statusCode = 418
        ..headers.set('X-Echo', '${req.method} ${req.headers.value('x-sent')}')
        ..write('got: $body');
      await req.response.close();
    });
    final answer = await ioHttpFetch(HttpExchange(
      method: 'POST',
      url: Uri.parse('http://127.0.0.1:${server.port}/refresh'),
      headers: {'X-Sent': 'a header'},
      body: '{"a":1}',
      abort: Completer<void>().future,
    ));
    expect(answer.status, 418);
    expect(answer.body, 'got: {"a":1}');
    expect(answer.headers['x-echo'], 'POST a header');
  });
}

typedef Json = Map<String, dynamic>;
