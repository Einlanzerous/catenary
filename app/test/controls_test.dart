// CANT-209: the controls. The composer's send reaches `Outbox.compose`, RETRY
// and DELETE on a failed message reach `Outbox.retry` and `Outbox.discard`
// with that message's `client_id`, and the banner's RETRY reaches
// `Transport.retryNow`; the play button, ADD, REC, search and the thread's menu
// are drawn disabled and have nothing to tap.
//
// The store's half runs over a real session in a temporary directory, a
// seeded journal and outbox file and a socket that never opens, so the outbox
// it reaches is the real one. The widget half drives the screens through the
// callbacks the app wires.

import 'dart:io';

import 'package:catenary/main.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/store/status.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary/tokens.dart';
import 'package:catenary/widgets/connection_banner.dart';
import 'package:catenary/widgets/glyph.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential, item, me, projection, room, conversations;

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

/// A transport that counts `retryNow` and forwards everything to a real one.
final class _CountingTransport implements Transport {
  _CountingTransport(this.inner);

  final Transport inner;
  var retries = 0;

  @override
  void retryNow() {
    retries++;
    inner.retryNow();
  }

  @override
  void start() => inner.start();
  @override
  void stop() => inner.stop();
  @override
  TransportStatus status() => inner.status();
  @override
  void Function() subscribe(void Function(TransportStatus s) fn) => inner.subscribe(fn);
  @override
  Future<wire.ServerAck> send(wire.ClientSend f) => inner.send(f);
  @override
  void read(wire.ClientRead f) => inner.read(f);
  @override
  void typing(wire.ClientTyping f) => inner.typing(f);
  @override
  void catchUp() => inner.catchUp();
  @override
  Future<void> refreshIfDue() => inner.refreshIfDue();
  @override
  void Function() onSessionEnd(void Function(SessionEnd e) fn) => inner.onSessionEnd(fn);
  @override
  void Function() onApply(void Function(Applied e) fn) => inner.onApply(fn);
  @override
  JournalSnapshot snapshot() => inner.snapshot();
  @override
  void Function() onTyping(void Function(wire.ServerTyping f) fn) => inner.onTyping(fn);
}

void main() {
  late Directory dir;
  late _CountingTransport counting;

  SessionSeams seams() => SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) => _DeadSocket(),
        fetch: (_) async => const HttpAnswer(503, ''),
        transportFactory: (cfg) => counting = _CountingTransport(createTransport(cfg)),
      );

  /// An enrolled directory: the canvas's journal, and an outbox holding one
  /// failed entry, as a previous launch would have left them.
  Future<void> enroll() async {
    final credentials = openCredentialStore(dir.path);
    await enrollCredential(credentials, inProcessLock(), credential());
    credentials.close();
    writeAddress(dir.path, 'http://192.168.1.20:4012');
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
    final outbox = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
    await outbox.add(item('c-failed', OutboxState.failed, error: const ServerRefusal('message_too_large', 'message too large', false)).entry);
    outbox.close();
  }

  setUp(() {
    dir = Directory.systemTemp.createTempSync('catenary-controls-');
  });
  tearDown(() => dir.deleteSync(recursive: true));

  group('the store\'s commands reach the outbox and the transport', () {
    test('send is Outbox.compose: an entry in that conversation, queued while no session is ready', () async {
      await enroll();
      final store = AppStore(seams());
      addTearDown(store.dispose);
      await store.start();
      await store.send(room, '  see you at six  ');
      final thread = store.conversation(room)!;
      final sent = thread.messages.where((m) => m.text == 'see you at six').single;
      expect(sent.status, MessageStatus.queued);
      expect(sent.mine, isTrue);
      expect(thread.messages.last.id, sent.id, reason: 'laid after the log, as the outbox\'s tail');
      expect(store.connection.kind, ConnectionKind.reconnecting);
    });

    test('a blank draft is not a message, and a device that is not enrolled refuses', () async {
      await enroll();
      final store = AppStore(seams());
      addTearDown(store.dispose);
      await store.start();
      final before = store.conversation(room)!.messages.length;
      await store.send(room, '   ');
      expect(store.conversation(room)!.messages, hasLength(before));

      final empty = Directory.systemTemp.createTempSync('catenary-controls-empty-');
      addTearDown(() => empty.deleteSync(recursive: true));
      final bare = AppStore(SessionSeams(directory: empty.path, lifecycle: ManualLifecycle()));
      addTearDown(bare.dispose);
      await bare.start();
      await expectLater(bare.send(room, 'hi'), throwsStateError);
    });

    test('retry is Outbox.retry: the failed entry goes back to queued under the same client_id', () async {
      await enroll();
      final store = AppStore(seams());
      addTearDown(store.dispose);
      await store.start();
      expect(store.conversation(room)!.messages.last.status, MessageStatus.failed);
      await store.retry('c-failed');
      final retried = store.conversation(room)!.messages.last;
      expect(retried.id, 'c-failed');
      expect(retried.status, MessageStatus.queued);
      expect(retried.failure, isNull);
    });

    test('discard is Outbox.discard: the failed entry goes, and an entry that has not failed stays', () async {
      await enroll();
      final store = AppStore(seams());
      addTearDown(store.dispose);
      await store.start();
      await store.send(room, 'still waiting');
      final waiting = store.conversation(room)!.messages.last.id;
      expect(await store.discard(waiting), isFalse, reason: 'DELETE is offered on a failed send only');
      expect(await store.discard('c-failed'), isTrue);
      final ids = [for (final m in store.conversation(room)!.messages) m.id];
      expect(ids, isNot(contains('c-failed')));
      expect(ids, contains(waiting));
    });

    test('retryNow is Transport.retryNow', () async {
      await enroll();
      final store = AppStore(seams());
      addTearDown(store.dispose);
      await store.start();
      expect(counting.retries, 0);
      await store.retryNow();
      expect(counting.retries, 1);
    });
  });

  group('the screens call them', () {
    Future<void> pump(WidgetTester tester, Widget screen) {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      return tester.pumpWidget(MaterialApp(theme: catenaryTheme(Brightness.dark), home: TickerMode(enabled: false, child: screen)));
    }

    const live = ConnectionView.live();
    const offline = ConnectionView(kind: ConnectionKind.offline, queued: 0);

    testWidgets('the composer\'s send hands the draft over, and clears it once the call completes', (tester) async {
      final sent = <String>[];
      final thread = conversationViews(projection: projection(), outbox: const [], me: me, secure: true).first;
      await pump(tester, ThreadScreen(conversation: thread, connection: live, onSend: (text) async => sent.add(text)));
      await tester.enterText(find.byKey(const ValueKey('composer-input')), 'on my way');
      await tester.pump();
      await tester.tap(find.text('SEND'));
      await tester.pump();
      expect(sent, ['on my way']);
      expect(tester.widget<TextField>(find.byKey(const ValueKey('composer-input'))).controller!.text, isEmpty);
    });

    testWidgets('offline the same control reads QUEUE and hands the draft over the same way', (tester) async {
      final sent = <String>[];
      final thread = conversationViews(projection: projection(), outbox: const [], me: me, secure: true).first;
      await pump(tester, ThreadScreen(conversation: thread, connection: offline, onSend: (text) async => sent.add(text)));
      await tester.enterText(find.byKey(const ValueKey('composer-input')), 'later');
      await tester.pump();
      await tester.tap(find.text('QUEUE'));
      expect(sent, ['later']);
    });

    testWidgets('a send that throws keeps the draft where it was typed', (tester) async {
      final thread = conversationViews(projection: projection(), outbox: const [], me: me, secure: true).first;
      await pump(tester, ThreadScreen(conversation: thread, connection: live, onSend: (_) async => throw StateError('no')));
      await tester.enterText(find.byKey(const ValueKey('composer-input')), 'keep me');
      await tester.pump();
      await tester.tap(find.text('SEND'));
      await tester.pump();
      expect(tester.widget<TextField>(find.byKey(const ValueKey('composer-input'))).controller!.text, 'keep me');
    });

    testWidgets('RETRY and DELETE on a failed message hand over that message\'s client_id', (tester) async {
      final retried = <String>[], discarded = <String>[];
      final outbox = [
        item('c-other', OutboxState.queued, order: 1),
        item('c-failed', OutboxState.failed, order: 2, error: const ServerRefusal('message_too_large', 'message too large', false)),
      ];
      final thread = conversationViews(projection: projection(), outbox: outbox, me: me, secure: true).firstWhere((c) => c.id == room);
      await pump(
        tester,
        ThreadScreen(
          conversation: thread,
          connection: live,
          onRetryMessage: (id) async => retried.add(id),
          onDiscardMessage: (id) async => discarded.add(id),
        ),
      );
      await tester.tap(find.byKey(const ValueKey('failed-retry')));
      expect(retried, ['c-failed']);
      expect(discarded, isEmpty);
      await tester.tap(find.byKey(const ValueKey('failed-delete')));
      expect(retried, ['c-failed']);
      expect(discarded, ['c-failed']);
    });

    testWidgets('the app: the banner\'s RETRY is Transport.retryNow, and a thread\'s send is an outbox entry', (tester) async {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.runAsync(enroll);
      final store = AppStore(seams());
      expect(await tester.runAsync(store.start), isTrue);
      // Tickers on, so the route transition runs; pulses never settle, so time is stepped by hand.
      await tester.pumpWidget(CatenaryApp(initialMode: ThemeMode.dark, store: store));

      await tester.tap(find.text('RETRY'));
      expect(counting.retries, 1);

      await tester.tap(find.byKey(const ValueKey('rail-$room')));
      await tester.pump();
      await tester.pump(const Duration(seconds: 1));
      await tester.tap(find.descendant(
        of: find.descendant(of: find.byType(ThreadScreen), matching: find.byType(ConnectionBanner)),
        matching: find.text('RETRY'),
      ));
      expect(counting.retries, 2, reason: 'the thread\'s banner is the same control');

      await tester.enterText(find.byKey(const ValueKey('composer-input')), 'from the app');
      await tester.pump();
      await tester.runAsync(() async {
        // Reconnecting is not a live session, so the control reads QUEUE.
        await tester.tap(find.text('QUEUE'));
        await Future<void>.delayed(const Duration(milliseconds: 200));
      });
      await tester.pump();
      expect(store.conversation(room)!.messages.where((m) => m.text == 'from the app').single.status, MessageStatus.queued);

      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => store.dispose());
    });

    testWidgets('ADD, REC, the play button, search and the menu are drawn disabled and have nothing to tap', (tester) async {
      const voice = wire.VoiceAttachment(
        url: '/media/v',
        durationMs: 12000,
        peaks: [1, 4, 9, 4, 1],
        transcript: wire.Transcript(state: wire.TranscriptState.ready, text: 'two words'),
      );
      final p = projection();
      final withVoice = Projection(
        conversations: conversations,
        users: p.users,
        messages: [
          for (final m in p.messages)
            if (m.id == p.messages.first.id) _withVoice(m, voice) else m,
        ],
      );
      final thread = conversationViews(projection: withVoice, outbox: const [], me: me, secure: true).firstWhere((c) => c.id == room);
      // Wired for everything the app wires, so a control that stays disabled
      // does so with its neighbours live.
      await pump(
        tester,
        ThreadScreen(
          conversation: thread,
          connection: live,
          onSend: (_) async {},
          onRetryMessage: (_) async {},
          onDiscardMessage: (_) async {},
        ),
      );
      final t = CatenaryTokens.of(tester.element(find.byType(ThreadScreen)));
      Color colorOf(String label) => tester.widget<Text>(find.text(label)).style!.color!;

      expect(colorOf('ADD'), t.textDisabled);
      expect(colorOf('REC'), t.textDisabled);
      for (final label in ['ADD', 'REC']) {
        final taps = tester.widgetList<GestureDetector>(find.ancestor(of: find.text(label), matching: find.byType(GestureDetector)));
        expect(taps.map((g) => g.onTap), everyElement(isNull), reason: '$label has no tap handler');
      }

      final play = find.byKey(const ValueKey('voice-play'));
      expect(play, findsOneWidget);
      expect(tester.widget<GlyphIcon>(find.descendant(of: play, matching: find.byType(GlyphIcon))).color, t.textDisabled);
      expect(find.ancestor(of: play, matching: find.byType(GestureDetector)), findsNothing);
      expect(find.ancestor(of: play, matching: find.byType(InkWell)), findsNothing);

      for (final glyph in [Glyph.search, Glyph.more]) {
        final icon = find.byWidgetPredicate((w) => w is GlyphIcon && w.glyph == glyph);
        expect(icon, findsOneWidget);
        expect(tester.widget<GlyphIcon>(icon).color, t.textDisabled, reason: '$glyph');
        expect(find.ancestor(of: icon, matching: find.byType(GestureDetector)), findsNothing, reason: '$glyph has no tap handler');
      }
    });
  });
}

wire.Message _withVoice(wire.Message m, wire.VoiceAttachment voice) => wire.Message(
      id: m.id,
      seq: m.seq,
      logSeq: m.logSeq,
      conversationId: m.conversationId,
      authorId: m.authorId,
      at: m.at,
      text: m.text,
      state: m.state,
      attachments: [voice],
    );
