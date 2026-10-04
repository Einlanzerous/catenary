// The session (lib/store/session.dart), with no widget involved: a temporary
// directory for the files, a recording transport factory for "constructs a
// transport", and a socket that never opens, so nothing leaves the process.

import 'dart:io';

import 'package:catenary/store/address.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:flutter_test/flutter_test.dart';

const me = '11111111-1111-4111-8111-111111111111';

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

StoredCredential credential() => const StoredCredential(
      userId: me,
      deviceId: '22222222-2222-4222-8222-222222222222',
      accessToken: 'access',
      accessExpiresAt: 4102444800000,
      refreshToken: 'refresh',
      refreshExpiresAt: 4102444800000,
    );

void main() {
  late Directory dir;
  late List<TransportConfig> built;
  late List<String> dials;

  SessionSeams seams() => SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) {
          dials.add(url.toString());
          return _DeadSocket();
        },
        fetch: (_) async => const HttpAnswer(503, ''),
        transportFactory: (cfg) {
          built.add(cfg);
          return createTransport(cfg);
        },
      );

  Future<void> storeCredential() async {
    final store = openCredentialStore(dir.path);
    await enrollCredential(store, inProcessLock(), credential());
    store.close();
  }

  setUp(() {
    dir = Directory.systemTemp.createTempSync('catenary-session-');
    built = [];
    dials = [];
  });
  tearDown(() => dir.deleteSync(recursive: true));

  test('an empty directory is not enrolled, constructs no transport and opens no file', () async {
    final r = await startSession(seams());
    expect(r, isA<NotEnrolled>());
    expect(built, isEmpty);
    expect(dials, isEmpty);
    expect(dir.listSync(), isEmpty, reason: 'a device that never enrolled has no database');
  });

  test('a credential and no address file is not enrolled and constructs no transport', () async {
    await storeCredential();
    expect(await startSession(seams()), isA<NotEnrolled>());
    expect(built, isEmpty);
    expect(dials, isEmpty);
  });

  test('an address file and no credential is not enrolled and constructs no transport', () async {
    writeAddress(dir.path, 'https://chat.example.com');
    expect(await startSession(seams()), isA<NotEnrolled>());
    expect(built, isEmpty);
    expect(dials, isEmpty);
  });

  test('an empty address file counts as none', () async {
    await storeCredential();
    File('${dir.path}/$addressFileName').writeAsStringSync('\n');
    expect(await startSession(seams()), isA<NotEnrolled>());
    expect(built, isEmpty);
  });

  test('both constructs one transport, for that address and that account, and starts it', () async {
    await storeCredential();
    writeAddress(dir.path, 'https://chat.example.com');
    final r = await startSession(seams());
    expect(r, isA<SessionRunning>());
    final s = (r as SessionRunning).session;
    addTearDown(s.end);

    expect(built, hasLength(1));
    expect(built.single.baseUrl, 'https://chat.example.com');
    expect(s.address, 'https://chat.example.com');
    expect(s.credential.userId, me);
    expect(s.transport, isNotNull);
    expect(s.outbox, isNotNull);
    await pumpEventQueue();
    expect(dials.single, startsWith('wss://chat.example.com/ws'), reason: 'start() dialed the stored address');
    expect(File('${dir.path}/$outboxFileName').existsSync(), isTrue, reason: 'the outbox is its own file');
  });

  test('a failure while building closes what was opened, so a retry starts clean', () async {
    await storeCredential();
    writeAddress(dir.path, 'https://chat.example.com');
    var fail = true;
    final s = SessionSeams(
      directory: dir.path,
      lifecycle: ManualLifecycle(),
      connect: (url, protocols) => _DeadSocket(),
      fetch: (_) async => const HttpAnswer(503, ''),
      transportFactory: (cfg) => fail ? throw StateError('no transport today') : createTransport(cfg),
    );
    await expectLater(startSession(s), throwsStateError);
    fail = false;
    final r = await startSession(s);
    expect(r, isA<SessionRunning>());
    (r as SessionRunning).session.end();
  });

  test('a session started to replace another ends it first', () async {
    await storeCredential();
    writeAddress(dir.path, 'https://chat.example.com');
    final first = ((await startSession(seams())) as SessionRunning).session;
    final second = ((await startSession(seams(), replacing: first)) as SessionRunning).session;
    addTearDown(second.end);
    expect(built, hasLength(2));
    first.end(); // idempotent: the replacement already ended it
  });

  test('the same session is a restart after a credential terminal: end, then start again', () async {
    await storeCredential();
    writeAddress(dir.path, 'https://chat.example.com');
    final first = ((await startSession(seams())) as SessionRunning).session..end();
    final again = await startSession(seams(), replacing: first);
    expect(again, isA<SessionRunning>());
    (again as SessionRunning).session.end();
  });
}
