// The live store's link path (CANT-363): the composer's chip asks the real
// `POST /link-previews` through the session's credential, and a send that
// carries the preview it returned goes through the ONE compose path, as an
// outbox entry holding a `link` attachment bound to the server's handle.

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:flutter_test/flutter_test.dart';

import 'ready_transport.dart';

const me = '11111111-1111-4111-8111-111111111111';
const conv = '44444444-4444-4444-8444-444444444444';
const handle = '55555555-5555-4555-8555-555555555555';
const url = 'https://riverline.example.org/depot';

final class _DeadSocket implements WebSocketLike {
  @override
  void Function()? onOpen;
  @override
  void Function(Object? data)? onMessage;
  @override
  void Function(int? code)? onClose;
  @override
  void send(String data) => throw StateError('never open');
  @override
  void close([int? code, String? reason]) {}
}

void main() {
  late Directory dir;
  late List<HttpExchange> asked;

  setUp(() {
    dir = Directory.systemTemp.createTempSync('catenary-link-');
    asked = [];
  });
  tearDown(() => dir.deleteSync(recursive: true));

  Future<AppStore> store(int status, String body) async {
    final credentials = openCredentialStore(dir.path);
    await enrollCredential(
      credentials,
      inProcessLock(),
      const StoredCredential(
        userId: me,
        deviceId: '22222222-2222-4222-8222-222222222222',
        accessToken: 'access',
        accessExpiresAt: 4102444800000,
        refreshToken: 'refresh',
        refreshExpiresAt: 4102444800000,
      ),
    );
    credentials.close();
    writeAddress(dir.path, 'https://chat.example.com');
    final s = AppStore(
      SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (u, p) => _DeadSocket(),
        fetch: (x) async {
          if (x.url.path == '/link-previews') {
            asked.add(x);
            return HttpAnswer(status, body);
          }
          return const HttpAnswer(503, '');
        },
        uploadFetch: (_) => Completer<HttpAnswer>().future,
        transportFactory: (cfg) => ReadyTransport(createTransport(cfg)),
      ),
      systemClock,
      wipeJournalFile,
      null,
    );
    addTearDown(s.dispose);
    await s.start();
    return s;
  }

  final ok = jsonEncode({
    'upload_id': handle,
    'preview': {'kind': 'link', 'url': url, 'title': 'Depot hours', 'description': 'Open 6 to 6.'},
  });

  test('the chip asks the real endpoint with the bearer, and a send with that preview is one entry holding a link bound to the handle', () async {
    final s = await store(200, ok);
    final p = await s.previewLink(url);
    expect(asked.single.method, 'POST');
    expect(asked.single.headers['Authorization'], 'Bearer access');
    expect(jsonDecode(asked.single.body!), {'url': url});
    expect(p!.title, 'Depot hours');

    await s.send(conv, 'hours: $url', link: p);
    final entries = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
    addTearDown(entries.close);
    final held = await entries.list();
    expect(held, hasLength(1));
    expect(held.single.text, 'hours: $url');
    expect(held.single.attachments.single.kind, 'link');
    expect(held.single.attachments.single.uploadId, handle);
  });

  test('a handle is single-use: sending the same preview again goes as plain text', () async {
    final s = await store(200, ok);
    final p = await s.previewLink(url);
    await s.send(conv, 'one $url', link: p);
    await s.send(conv, 'two $url', link: p);
    final entries = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
    addTearDown(entries.close);
    final held = await entries.list();
    expect(held, hasLength(2));
    expect(held.where((e) => e.attachments.isNotEmpty), hasLength(1));
  });

  for (final (status, body) in [(204, ''), (404, '{}'), (502, 'bad gateway')]) {
    test('a $status is no card, and the draft sends as text', () async {
      final s = await store(status, body);
      expect(await s.previewLink(url), isNull);
    });
  }
}
