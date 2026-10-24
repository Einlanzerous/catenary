// The session (lib/store/session.dart), with no widget involved: a temporary
// directory for the files, a recording transport factory for "constructs a
// transport", and a socket that never opens, so nothing leaves the process.

import 'dart:io';
import 'dart:typed_data';

import 'package:catenary/store/address.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' show ClientRead, ClientSend, ClientTyping, ServerAck, ServerTyping;
import 'package:flutter_test/flutter_test.dart';

/// A real transport that says it is ready, so the outbox offers an attachment
/// to its uploader at once. Nothing else about it is changed.
final class _Ready implements Transport {
  _Ready(this.inner);

  final Transport inner;

  @override
  TransportStatus status() {
    final s = inner.status();
    return TransportStatus(
      terminal: s.terminal,
      refreshHold: s.refreshHold,
      tokenRefused: s.tokenRefused,
      nextRefreshAt: s.nextRefreshAt,
      connected: true,
      ready: true,
      sessionId: 's-1',
      heartbeatIntervalSec: s.heartbeatIntervalSec,
      missedPongLimit: s.missedPongLimit,
      caughtUp: s.caughtUp,
      cursor: s.cursor,
      attempt: s.attempt,
      nextDialAt: s.nextDialAt,
      stats: s.stats,
      messages: s.messages,
      wipes: s.wipes,
      headSeqTotal: s.headSeqTotal,
      journalError: s.journalError,
    );
  }

  @override
  void start() => inner.start();
  @override
  void stop() => inner.stop();
  @override
  void Function() subscribe(void Function(TransportStatus s) fn) => inner.subscribe(fn);
  @override
  Future<ServerAck> send(ClientSend f) => inner.send(f);
  @override
  void read(ClientRead f) => inner.read(f);
  @override
  void typing(ClientTyping f) => inner.typing(f);
  @override
  void catchUp() => inner.catchUp();
  @override
  void retryNow() => inner.retryNow();
  @override
  Future<void> refreshIfDue() => inner.refreshIfDue();
  @override
  void Function() onSessionEnd(void Function(SessionEnd e) fn) => inner.onSessionEnd(fn);
  @override
  void Function() onApply(void Function(Applied e) fn) => inner.onApply(fn);
  @override
  JournalSnapshot snapshot() => inner.snapshot();
  @override
  void Function() onTyping(void Function(ServerTyping f) fn) => inner.onTyping(fn);
}

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

  // CANT-316. Forgetting `uploader:` on Outbox.open compiles, and every
  // attachment then fails with the RefusingUploader's words.
  test('the outbox the session builds holds the real uploader: an attachment is POSTed to /uploads with the stored token and its own bytes', () async {
    await storeCredential();
    writeAddress(dir.path, 'https://chat.example.com');
    final requests = <UploadExchange>[];
    final s = SessionSeams(
      directory: dir.path,
      lifecycle: ManualLifecycle(),
      connect: (url, protocols) => _DeadSocket(),
      fetch: (_) async => const HttpAnswer(503, ''),
      uploadFetch: (x) async {
        requests.add(x);
        return const HttpAnswer(201, '{"upload_id":"33333333-3333-4333-8333-333333333333"}');
      },
      transportFactory: (cfg) => _Ready(createTransport(cfg)),
    );
    final session = ((await startSession(s)) as SessionRunning).session;
    addTearDown(session.end);

    final bytes = Uint8List.fromList([0xff, 0xd8, 0xff, 0x80, 0x81]);
    final entry = await session.outbox.compose(OutboxDraft(
      conversationId: '44444444-4444-4444-8444-444444444444',
      attachments: [OutboundAttachmentDraft(kind: 'image', source: MediaBytes(bytes), filename: 'a.jpg')],
    ));
    for (var i = 0; i < 50 && requests.isEmpty; i++) {
      await Future<void>.delayed(const Duration(milliseconds: 20));
    }

    expect(requests, hasLength(1), reason: 'the uploader was reached');
    expect(requests.single.url.toString(), 'https://chat.example.com/uploads?kind=image&filename=a.jpg');
    expect(requests.single.headers['Authorization'], 'Bearer access');
    expect(requests.single.body, bytes);
    final item = session.outbox.view().firstWhere((i) => i.entry.clientId == entry.clientId);
    expect(errorText(item.entry.lastError), isNot(RefusingUploader.message));
  });
}
