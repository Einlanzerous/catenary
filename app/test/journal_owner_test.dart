// A journal is one account's (CANT-230, sub-task CANT-245). `startSession`
// claims the journal for the held credential's account before it builds a
// transport, so a re-enrollment that died between writing its credential and
// wiping the journal heals on the next launch.
//
// Over real files in a temporary directory, and a socket that never opens.
// The interrupted re-enrollment is staged exactly as it is left on disk: the
// pair replaced through `reenrollCredential`, and no wipe after.

import 'dart:io';

import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential, me, nadia, projection, room;

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
  late int built;

  SessionSeams seams() => SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) => _DeadSocket(),
        fetch: (_) async => const HttpAnswer(503, ''),
        transportFactory: (cfg) {
          built++;
          return createTransport(cfg);
        },
      );

  String journalPath() => '${dir.path}/$journalFileName';

  /// A device enrolled as `me`, as its launches left it: the canvas in its
  /// journal with a cursor, the journal claimed by `me`, and one unsent
  /// message in the outbox.
  Future<void> enrolledAsMe() async {
    final credentials = openCredentialStore(dir.path);
    await enrollCredential(credentials, inProcessLock(), credential());
    credentials.close();
    writeAddress(dir.path, 'https://chat.example.com');
    final journal = SqliteJournal.open(journalPath());
    final p = projection();
    await journal.applyPage(
      wire.SyncResponse(
        logSeq: 200,
        messages: p.messages.where((m) => m.state != wire.DeliveryState.unknown).toList(),
        conversations: p.conversations,
        users: p.users.values.toList(),
        hasMore: false,
        serverTime: '2026-10-04T12:05:00.000Z',
      ),
      Faults.none,
    );
    journal.close();
    // A launch: the session start claims the journal for `me`.
    final session = ((await startSession(seams())) as SessionRunning).session;
    expect(session.journal.owner, me);
    await session.outbox.compose(OutboxDraft(conversationId: room, text: 'unsent'));
    session.end();
    built = 0;
  }

  /// What a re-enrollment leaves when the process dies before its wipe: the
  /// pair is [userId]'s, a new device, and the journal is as it was.
  Future<void> reenrolledWithoutTheWipe(String userId) async {
    final credentials = openCredentialStore(dir.path);
    await reenrollCredential(
      credentials,
      inProcessLock(),
      StoredCredential(
        userId: userId,
        deviceId: '99999999-9999-4999-8999-999999999999',
        accessToken: 'a' * 43,
        accessExpiresAt: 4102444800000,
        refreshToken: 'r' * 43,
        refreshExpiresAt: 4102444800000,
      ),
    );
    credentials.close();
  }

  Future<int> outboxCount() async {
    final s = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
    try {
      return (await s.list()).length;
    } finally {
      s.close();
    }
  }

  setUp(() {
    dir = Directory.systemTemp.createTempSync('catenary-journal-owner-');
    built = 0;
  });
  tearDown(() => dir.deleteSync(recursive: true));

  test('the app does not open another account\'s journal: the claim wipes it before a transport is built', () async {
    await enrolledAsMe();
    expect(await outboxCount(), 1);
    await reenrolledWithoutTheWipe(nadia);

    final session = ((await startSession(seams())) as SessionRunning).session;
    addTearDown(session.end);
    expect(session.credential.userId, nadia);
    final snapshot = session.journal.snapshot();
    expect(snapshot.conversations, isEmpty);
    expect(snapshot.messages, isEmpty);
    expect(snapshot.cursor, isNull);
    expect(session.journal.owner, nadia);
    expect(built, 1);
    expect(await outboxCount(), 1, reason: 'the outbox is keyed by account and never touched');
  });

  test('and the store shows the new account an empty rail, not the previous account\'s', () async {
    await enrolledAsMe();
    await reenrolledWithoutTheWipe(nadia);
    final store = AppStore(seams());
    addTearDown(store.dispose);
    expect(await store.start(), isTrue);
    expect(store.conversations, isEmpty);
  });

  test('the app keeps the same account\'s journal: a new device of that account resumes from its cursor', () async {
    await enrolledAsMe();
    await reenrolledWithoutTheWipe(me);

    final session = ((await startSession(seams())) as SessionRunning).session;
    addTearDown(session.end);
    final snapshot = session.journal.snapshot();
    expect(snapshot.conversations, hasLength(2));
    expect(snapshot.cursor, 200);
    expect(session.journal.owner, me);
  });

  test('a claim that cannot be written is a failed start: no transport is built over a journal that is someone else\'s', () async {
    await enrolledAsMe();
    await reenrolledWithoutTheWipe(nadia);
    // Another context holds a write transaction open on catenary.db, so the
    // claim's own cannot begin within the busy timeout. Reads still can,
    // which is everything `startSession` does before the claim.
    final other = SqliteJournal.open(journalPath());
    other.database.execute('BEGIN IMMEDIATE');
    addTearDown(() {
      other.database.execute('ROLLBACK');
      other.close();
    });

    final store = AppStore(seams());
    addTearDown(store.dispose);
    expect(await store.start(), isFalse);
    expect(store.startFailure, isNotNull);
    expect(store.startFailure!.name, 'SqliteException');
    expect(store.enrolled, isFalse);
    expect(built, 0, reason: 'transportFactory was not called');

    // And the journal is still the previous account's, unopened.
    expect(other.owner, me);
    expect(other.snapshot().conversations, hasLength(2));
  });
}
