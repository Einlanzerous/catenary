// The store (lib/store/app_store.dart): the banner's derivation held to the
// web client's, the two status vocabularies held apart, the subtitle's
// transport word, and the store itself over a real session in a temporary
// directory — a seeded SQLite journal and a socket that never opens, so
// nothing leaves the process.

import 'dart:io';

import 'package:catenary/main.dart';
import 'package:catenary/rail.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/store/status.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

const me = '11111111-1111-4111-8111-111111111111';
const nadia = '33333333-3333-4333-8333-333333333333';
const ilse = '44444444-4444-4444-8444-444444444444';
const room = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const direct = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';

// Message ids: the journal stores records as their wire encoding, which holds
// an id to the UUID shape.
const m1 = '00000001-0000-4000-8000-000000000000';
const m2 = '00000002-0000-4000-8000-000000000000';
const m3 = '00000003-0000-4000-8000-000000000000';
const m4 = '00000004-0000-4000-8000-000000000000';
const m5 = '00000005-0000-4000-8000-000000000000';
const m9 = '00000009-0000-4000-8000-000000000000';

/// The outbox's three, which the wire cannot express.
const outboxOnly = {MessageStatus.queued, MessageStatus.sending, MessageStatus.failed};

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

const now = 1791000000000;

TransportStatus status({
  Terminal terminal = Terminal.not,
  bool ready = false,
  bool caughtUp = false,
  int attempt = 0,
  num? nextDialAt,
  int messages = 0,
  int headSeqTotal = 0,
  JournalError? journalError,
}) =>
    TransportStatus(
      terminal: terminal,
      refreshHold: RefreshHold.none,
      tokenRefused: false,
      nextRefreshAt: null,
      connected: ready,
      ready: ready,
      sessionId: ready ? 's-1' : null,
      heartbeatIntervalSec: null,
      missedPongLimit: null,
      caughtUp: caughtUp,
      cursor: null,
      attempt: attempt,
      nextDialAt: nextDialAt,
      stats: Stats(),
      messages: messages,
      wipes: 0,
      headSeqTotal: headSeqTotal,
      journalError: journalError,
    );

/// One row of the table: a status, what the device says about its network,
/// and what `connectionInfo` in web/src/transport/status.ts returns for it.
/// [state] is the reference's `state` string, and the rest are the fields it
/// sets beside it on that branch.
typedef Row = ({
  String name,
  TransportStatus Function(JournalError? journalError) status,
  bool? online,
  String state,
  TerminalCause? terminal,
  int? synced,
  int? total,
  int? attempt,
  int? retryInSec,
});

final List<Row> table = [
  (
    name: 'terminal credential, and it is terminal even on a device that is offline',
    status: (e) => status(terminal: const Terminal(TerminalKind.credential, 'refresh refused'), journalError: e),
    online: false,
    state: 'terminal',
    terminal: TerminalCause.credential,
    synced: null,
    total: null,
    attempt: null,
    retryInSec: null,
  ),
  (
    name: 'terminal protocol, though the session is ready',
    status: (e) => status(terminal: const Terminal(TerminalKind.protocol, 'close 4001'), ready: true, caughtUp: true, journalError: e),
    online: true,
    state: 'terminal',
    terminal: TerminalCause.protocol,
    synced: null,
    total: null,
    attempt: null,
    retryInSec: null,
  ),
  (
    name: 'offline, whatever the session says',
    status: (e) => status(ready: true, caughtUp: true, journalError: e),
    online: false,
    state: 'offline',
    terminal: null,
    synced: null,
    total: null,
    attempt: null,
    retryInSec: null,
  ),
  (
    name: 'ready and caught up',
    status: (e) => status(ready: true, caughtUp: true, messages: 40, headSeqTotal: 40, journalError: e),
    online: true,
    state: 'live',
    terminal: null,
    synced: null,
    total: null,
    attempt: null,
    retryInSec: null,
  ),
  (
    name: 'ready with headSeqTotal 0: resyncing with no numbers',
    status: (e) => status(ready: true, messages: 0, headSeqTotal: 0, journalError: e),
    online: null,
    state: 'resyncing',
    terminal: null,
    synced: null,
    total: null,
    attempt: null,
    retryInSec: null,
  ),
  (
    name: 'ready with a total',
    status: (e) => status(ready: true, messages: 412, headSeqTotal: 1180, journalError: e),
    online: true,
    state: 'resyncing',
    terminal: null,
    synced: 412,
    total: 1180,
    attempt: null,
    retryInSec: null,
  ),
  (
    name: 'ready with more held than the total: synced clamped to total',
    status: (e) => status(ready: true, messages: 1300, headSeqTotal: 1180, journalError: e),
    online: true,
    state: 'resyncing',
    terminal: null,
    synced: 1180,
    total: 1180,
    attempt: null,
    retryInSec: null,
  ),
  (
    name: 'not ready, before the first dial: attempt at least 1, no wait',
    status: (e) => status(attempt: 0, nextDialAt: null, journalError: e),
    online: null,
    state: 'reconnecting',
    terminal: null,
    synced: null,
    total: null,
    attempt: 1,
    retryInSec: 0,
  ),
  (
    name: 'not ready, a dial pending: the wait rounded up to a second',
    status: (e) => status(attempt: 3, nextDialAt: now + 7200, journalError: e),
    online: true,
    state: 'reconnecting',
    terminal: null,
    synced: null,
    total: null,
    attempt: 3,
    retryInSec: 8,
  ),
  (
    name: 'not ready, a dial overdue: retry seconds never negative',
    status: (e) => status(attempt: 5, nextDialAt: now - 30000, journalError: e),
    online: true,
    state: 'reconnecting',
    terminal: null,
    synced: null,
    total: null,
    attempt: 5,
    retryInSec: 0,
  ),
];

wire.Message message(
  String id,
  String conversationId,
  int seq,
  String authorId,
  String at, {
  String? text = 'hello',
  wire.DeliveryState state = wire.DeliveryState.sent,
  int? readBy,
  String? clientId,
  List<wire.Attachment>? attachments,
}) =>
    wire.Message(
      id: id,
      seq: seq,
      logSeq: seq + (conversationId == room ? 0 : 100),
      conversationId: conversationId,
      authorId: authorId,
      at: at,
      text: text,
      state: state,
      readBy: readBy,
      clientId: clientId,
      attachments: attachments,
    );

OutboxItem item(
  String clientId,
  OutboxState state, {
  String conversationId = room,
  int order = 1,
  String composedAt = '2026-10-04T12:30:00.000Z',
  String text = 'on my way',
  wire.ServerAck? ack,
  OutboxError? error,
  bool retrying = false,
}) =>
    OutboxItem(
      entry: OutboxEntry(
        clientId: clientId,
        accountId: me,
        conversationId: conversationId,
        order: order,
        composedAt: composedAt,
        text: text,
        status: state == OutboxState.failed ? OutboxStatus.failed : OutboxStatus.pending,
        lastError: error,
      ),
      state: state,
      ack: ack,
      retrying: retrying,
    );

const users = {
  me: wire.User(id: me, name: 'Hollis Brandt', initials: 'HB'),
  nadia: wire.User(id: nadia, name: 'Nadia Okonkwo'),
  ilse: wire.User(id: ilse, name: 'Ilse Marchetti'),
};

const conversations = [
  wire.Conversation(id: room, kind: wire.ConversationKind.group, name: 'Kitchen Table', memberCount: 3, headSeq: 4, firstUnreadSeq: 4),
  // The record's own name is stale; the other member's `User` is the title.
  wire.Conversation(id: direct, kind: wire.ConversationKind.direct, name: 'Nadia O.', otherMemberId: nadia, memberCount: 2, headSeq: 1),
];

/// A journal holding one message in each of the wire's delivery states,
/// yours and other people's.
Projection projection() => Projection(
      conversations: conversations,
      users: users,
      messages: [
        message(m1, room, 1, me, '2026-10-04T12:00:00.000Z', state: wire.DeliveryState.sent),
        message(m2, room, 2, me, '2026-10-04T12:01:00.000Z', state: wire.DeliveryState.delivered),
        message(m3, room, 3, me, '2026-10-04T12:02:00.000Z', state: wire.DeliveryState.read, readBy: 2),
        message(m4, room, 4, nadia, '2026-10-04T12:03:00.000Z', state: wire.DeliveryState.delivered),
        message(m5, direct, 1, nadia, '2026-10-04T09:00:00.000Z', state: wire.DeliveryState.unknown),
      ],
    );

StoredCredential credential() => const StoredCredential(
      userId: me,
      deviceId: '22222222-2222-4222-8222-222222222222',
      accessToken: 'access',
      accessExpiresAt: 4102444800000,
      refreshToken: 'refresh',
      refreshExpiresAt: 4102444800000,
    );

void main() {
  group('connectionInfo', () {
    for (final row in table) {
      test('${row.name}: ${row.state}, as web/src/transport/status.ts has it', () {
        final view = connectionInfo(row.status(null), now: now, online: row.online);
        // The app's kinds are named as the reference's states are.
        expect(view.kind.name, row.state);
        expect(view.terminal, row.terminal);
        expect(view.synced, row.synced);
        expect(view.total, row.total);
        expect(view.attempt, row.attempt);
        expect(view.retryIn, row.retryInSec == null ? null : Duration(seconds: row.retryInSec!));
        expect(view.journalError, isNull, reason: 'a status with no journal error yields none');
      });

      test('${row.name}: a journal error rides beside it, by name', () {
        final failed = connectionInfo(row.status(const JournalError('SqliteException', 'disk I/O error')), now: now, online: row.online);
        expect(failed.journalError, 'SqliteException');
        expect(failed.kind.name, row.state, reason: 'beside the state, never instead of it');
      });
    }

    test('the table covers every state the reference returns', () {
      expect({for (final row in table) row.state}, {for (final k in ConnectionKind.values) k.name});
      expect({for (final row in table) row.terminal}..remove(null), TerminalCause.values.toSet());
    });
  });

  group('MessageStatus', () {
    final outbox = [
      item('c-queued', OutboxState.queued, order: 1),
      item('c-sending', OutboxState.sending, order: 2),
      item('c-failed', OutboxState.failed, order: 3, error: const ServerRefusal('message_too_large', 'message too large', false)),
    ];

    test('no delivery state the wire has becomes queued, sending or failed', () {
      for (final state in wire.DeliveryState.values) {
        expect(outboxOnly, isNot(contains(deliveryStatus(state))), reason: state.wire);
      }
      expect(deliveryStatus(wire.DeliveryState.sent), MessageStatus.sent);
      expect(deliveryStatus(wire.DeliveryState.delivered), MessageStatus.delivered);
      expect(deliveryStatus(wire.DeliveryState.read), MessageStatus.read);
    });

    test('a journal message is sent, delivered or read, beside an outbox holding every one of its own states', () {
      final p = projection();
      final views = conversationViews(projection: p, outbox: outbox, me: me, secure: true);
      final shown = {for (final c in views) for (final m in c.messages) m.id: m};
      for (final m in p.messages) {
        expect(outboxOnly, isNot(contains(shown[m.id]!.status)), reason: '${m.id} came from the journal');
      }
      expect([for (final m in p.messages) shown[m.id]!.status], [
        MessageStatus.sent,
        MessageStatus.delivered,
        MessageStatus.read,
        MessageStatus.delivered,
        MessageStatus.sent,
      ]);
      expect(shown[m3]!.readBy, 2);
    });

    test('queued, sending and failed come from the outbox, each from its own state', () {
      final views = conversationViews(projection: projection(), outbox: outbox, me: me, secure: true);
      final shown = {for (final m in views.firstWhere((c) => c.id == room).messages) m.id: m};
      expect(shown['c-queued']!.status, MessageStatus.queued);
      expect(shown['c-sending']!.status, MessageStatus.sending);
      expect(shown['c-failed']!.status, MessageStatus.failed);
      expect(shown['c-failed']!.failure, 'message too large');
      expect(shown.values.where((m) => outboxOnly.contains(m.status)).map((m) => m.id), ['c-queued', 'c-sending', 'c-failed'],
          reason: 'and nothing else in the thread carries one');
      for (final state in OutboxState.values.where((s) => s != OutboxState.sent)) {
        expect(outboxOnly, contains(outboxStatus(state)));
      }
    });

    test('the tail follows the log in the outbox\'s order, and every tail row is yours', () {
      final thread = conversationViews(projection: projection(), outbox: outbox, me: me, secure: true).firstWhere((c) => c.id == room);
      expect([for (final m in thread.messages) m.id], [m1, m2, m3, m4, 'c-queued', 'c-sending', 'c-failed']);
      expect(thread.messages.skip(4).every((m) => m.mine), isTrue);
      expect(thread.newCount, 1, reason: 'your own unsent messages are never new');
      expect(preview(thread), 'You: on my way');
    });

    test('an acked entry sits at the ack\'s seq as sent, and is not shown beside its own record', () {
      const ack = wire.ServerAck(clientId: 'c-acked', messageId: m9, conversationId: room, seq: 5, logSeq: 9, at: '2026-10-04T12:31:00.000Z');
      final acked = [item('c-acked', OutboxState.sent, ack: ack)];
      final before = conversationViews(projection: projection(), outbox: acked, me: me, secure: true).firstWhere((c) => c.id == room);
      expect(before.messages.last.id, 'c-acked');
      expect(before.messages.last.seq, 5);
      expect(before.messages.last.status, MessageStatus.sent);

      final p = projection();
      final landed = Projection(
        conversations: p.conversations,
        users: p.users,
        messages: [...p.messages, message(m9, room, 5, me, '2026-10-04T12:31:00.000Z', text: 'on my way', clientId: 'c-acked')],
      );
      final after = conversationViews(projection: landed, outbox: acked, me: me, secure: true).firstWhere((c) => c.id == room);
      expect([for (final m in after.messages) m.id], [m1, m2, m3, m4, m9]);
    });
  });

  group('the subtitle\'s transport word', () {
    List<String> subtitles(String address) => [
          for (final c in conversationViews(projection: projection(), outbox: const [], me: me, secure: addressIsSecure(address))) c.subtitle,
        ];

    test('an https address ends every subtitle in TLS', () {
      final all = subtitles('https://chat.example.com');
      expect(all, unorderedEquals(['3 MEMBERS · TLS', 'DIRECT · TLS']));
      expect(all.every((s) => s.split(' ').last == 'TLS'), isTrue);
    });

    test('an http address ends every subtitle in CLEARTEXT, and none contains TLS', () {
      final all = subtitles('http://192.168.1.20:4012');
      expect(all, unorderedEquals(['3 MEMBERS · CLEARTEXT', 'DIRECT · CLEARTEXT']));
      expect(all.every((s) => s.split(' ').last == 'CLEARTEXT'), isTrue);
      expect(all.where((s) => s.contains('TLS')), isEmpty);
    });
  });

  group('the projection, as the rail and the thread read it', () {
    test('rooms and directs are most recent first, and a direct is titled by the other member\'s live name', () {
      final views = conversationViews(projection: projection(), outbox: const [], me: me, secure: true);
      expect([for (final c in views) c.id], [room, direct]);
      expect(views.last.kind, ConversationKind.direct);
      expect(views.last.name, 'Nadia Okonkwo');
      expect(views.first.memberCount, 3);
      expect(views.first.messages.first.authorName, 'Hollis Brandt');
      expect(views.first.messages.first.mine, isTrue);
      expect(views.first.messages.last.mine, isFalse);

      // A send composed offline moves its conversation to the top at once.
      final moved = conversationViews(
        projection: projection(),
        outbox: [item('c-1', OutboxState.queued, conversationId: direct, composedAt: '2026-10-04T13:00:00.000Z')],
        me: me,
        secure: true,
      );
      expect([for (final c in moved) c.id], [direct, room]);
    });

    test('typing names are the server\'s list, in the order each started', () {
      final views = conversationViews(
        projection: projection(),
        outbox: const [],
        me: me,
        secure: true,
        typing: const {
          room: [ilse, nadia],
        },
      );
      expect(views.firstWhere((c) => c.id == room).typing, ['Ilse Marchetti', 'Nadia Okonkwo']);
      expect(views.firstWhere((c) => c.id == direct).typing, isEmpty);
    });

    test('a voice note carries the server\'s peaks and its transcript; an image its stored size', () {
      final p = Projection(conversations: conversations, users: users, messages: [
        message('m-v', room, 1, nadia, '2026-10-04T12:00:00.000Z', text: null, attachments: const [
          wire.VoiceAttachment(url: '/media/v', durationMs: 38000, peaks: [9, 50, 100], transcript: wire.Transcript(state: wire.TranscriptState.ready, text: 'two words')),
        ]),
        message('m-p', room, 2, nadia, '2026-10-04T12:01:00.000Z', text: null, attachments: const [
          wire.VoiceAttachment(url: '/media/p', durationMs: 72000, peaks: [12], transcript: wire.Transcript(state: wire.TranscriptState.pending)),
        ]),
        message('m-i', room, 3, nadia, '2026-10-04T12:02:00.000Z', text: null, attachments: const [
          wire.ImageAttachment(url: '/media/i', filename: 'IMG_4471.HEIC', width: 3024, height: 2016, bytes: 1),
        ]),
      ]);
      final thread = conversationViews(projection: p, outbox: const [], me: me, secure: true).firstWhere((c) => c.id == room);
      expect(thread.messages[0].voice!.peaks, [9, 50, 100]);
      expect(thread.messages[0].voice!.duration, const Duration(seconds: 38));
      expect(thread.messages[0].voice!.transcript, 'two words');
      expect(thread.messages[1].voice!.transcript, isNull);
      expect(thread.messages[2].image!.filename, 'IMG_4471.HEIC');
      expect((thread.messages[2].image!.width, thread.messages[2].image!.height), (3024, 2016));
      expect(roomsPending(p), 2, reason: 'the room holds 3 of 4 and the direct 0 of 1');
    });
  });

  group('AppStore over a session', () {
    late Directory dir;
    late ManualLifecycle lifecycle;

    SessionSeams seams() => SessionSeams(
          directory: dir.path,
          lifecycle: lifecycle,
          connect: (url, protocols) => _DeadSocket(),
          fetch: (_) async => const HttpAnswer(503, ''),
        );

    /// Enrolls the directory against [address] and leaves the canvas's
    /// records in its journal, as a previous launch would have.
    Future<void> enroll(String address) async {
      final credentials = openCredentialStore(dir.path);
      await enrollCredential(credentials, inProcessLock(), credential());
      credentials.close();
      writeAddress(dir.path, address);
      final journal = SqliteJournal.open('${dir.path}/$journalFileName');
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
    }

    setUp(() {
      dir = Directory.systemTemp.createTempSync('catenary-store-');
      lifecycle = ManualLifecycle();
    });
    tearDown(() => dir.deleteSync(recursive: true));

    test('a device that is not enrolled starts nothing and holds nothing', () async {
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isFalse);
      expect(store.enrolled, isFalse);
      expect(store.conversations, isEmpty);
      expect(store.myInitials, '');
      expect(dir.listSync(), isEmpty);
    });

    test('an enrolled device shows what its journal holds, before any socket opens', () async {
      await enroll('http://192.168.1.20:4012');
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isTrue);
      expect(store.enrolled, isTrue);
      expect([for (final c in store.conversations) c.name], ['Kitchen Table', 'Nadia Okonkwo']);
      expect(store.myInitials, 'HB');
      expect(store.conversation(room)!.subtitle, '3 MEMBERS · CLEARTEXT');
      expect(store.conversation(room)!.newCount, 1);
      expect(store.conversation('nowhere'), isNull);
      expect(store.connection.kind, ConnectionKind.reconnecting, reason: 'the socket never opens');
      expect(store.connection.attempt, greaterThanOrEqualTo(1));
    });

    test('an https address reads TLS through the same store', () async {
      await enroll('https://chat.example.com');
      final store = AppStore(seams());
      addTearDown(store.dispose);
      await store.start();
      expect(store.conversation(room)!.subtitle, '3 MEMBERS · TLS');
      expect(store.conversation(direct)!.subtitle, 'DIRECT · TLS');
    });

    test('the network going away is offline with what is queued, and coming back is not', () async {
      await enroll('https://chat.example.com');
      final store = AppStore(seams());
      addTearDown(store.dispose);
      await store.start();
      var notified = 0;
      store.addListener(() => notified++);

      lifecycle.emit(LifecycleEvent.offline);
      expect(store.connection.kind, ConnectionKind.offline);
      expect(store.connection.queued, 0);
      expect(notified, greaterThan(0));

      lifecycle.emit(LifecycleEvent.online);
      expect(store.connection.kind, ConnectionKind.reconnecting);
      expect(store.connection.queued, isNull);
    });

    test('a start after a start replaces the session and keeps what the journal holds', () async {
      await enroll('https://chat.example.com');
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isTrue);
      expect(await store.start(), isTrue);
      expect(store.conversations, hasLength(2));
    });

    testWidgets('the app reads the store on an enrolled device, and the fixtures on one that is not', (tester) async {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);

      // Not enrolled: the canvas, until CANT-208 lands the enrollment screen.
      final empty = AppStore(seams());
      expect(await tester.runAsync(empty.start), isFalse);
      await tester.pumpWidget(TickerMode(enabled: false, child: CatenaryApp(initialMode: ThemeMode.dark, store: empty)));
      expect(find.byKey(const ValueKey('rail-kitchen')), findsOneWidget);
      await tester.runAsync(() async => empty.dispose());

      await tester.runAsync(() => enroll('http://192.168.1.20:4012'));
      final store = AppStore(seams());
      expect(await tester.runAsync(store.start), isTrue);
      await tester.pumpWidget(TickerMode(
        enabled: false,
        child: CatenaryApp(key: const ValueKey('enrolled'), initialMode: ThemeMode.dark, store: store),
      ));
      expect(find.byKey(const ValueKey('rail-kitchen')), findsNothing, reason: 'no fixture is on screen');
      expect(find.byKey(const ValueKey('rail-$room')), findsOneWidget);
      expect(tester.widget<RailScreen>(find.byType(RailScreen)).myInitials, 'HB');
      expect(find.text('Reconnecting'), findsOneWidget, reason: 'the banner is the store\'s connection');

      await tester.tap(find.byKey(const ValueKey('rail-$room')));
      await tester.pumpAndSettle();
      expect(find.byType(ThreadScreen), findsOneWidget);
      expect(tester.widget<Text>(find.byKey(const ValueKey('thread-subtitle'))).data, '3 MEMBERS · CLEARTEXT');
      expect(find.text('1 NEW'), findsOneWidget);

      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => store.dispose());
    });
  });

  group('the banner\'s actions are passed through', () {
    Future<void> pump(WidgetTester tester, Widget screen) {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      return tester.pumpWidget(MaterialApp(theme: catenaryTheme(Brightness.dark), home: TickerMode(enabled: false, child: screen)));
    }

    const reconnecting = ConnectionView(kind: ConnectionKind.reconnecting, attempt: 2, retryIn: Duration(seconds: 4));
    const terminal = ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.credential);
    final thread = conversationViews(projection: projection(), outbox: const [], me: me, secure: true).first;

    testWidgets('the rail hands RETRY and RE-ENROLL to its callbacks', (tester) async {
      var retried = 0, reenrolled = 0;
      Widget rail(ConnectionView connection) => RailScreen(
            conversations: const [],
            now: DateTime(2026, 10, 4, 14),
            connection: connection,
            myInitials: 'HB',
            onRetry: () => retried++,
            onReenroll: () => reenrolled++,
          );
      await pump(tester, rail(reconnecting));
      await tester.tap(find.text('RETRY'));
      await pump(tester, rail(terminal));
      await tester.tap(find.text('RE-ENROLL'));
      expect((retried, reenrolled), (1, 1));
    });

    testWidgets('the thread hands RETRY and RE-ENROLL to its callbacks', (tester) async {
      var retried = 0, reenrolled = 0;
      Widget screen(ConnectionView connection) =>
          ThreadScreen(conversation: thread, connection: connection, onRetry: () => retried++, onReenroll: () => reenrolled++);
      await pump(tester, screen(reconnecting));
      await tester.tap(find.text('RETRY'));
      await pump(tester, screen(terminal));
      await tester.tap(find.text('RE-ENROLL'));
      expect((retried, reenrolled), (1, 1));
    });
  });
}
