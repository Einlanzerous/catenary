// CANT-370: a cold start reopens the conversation that was open, the app half
// of CANT-369. The store's half runs over a real session in a temporary
// directory; the widget half pumps the app over that store, as a cold start
// does, and reads the very first frame.

import 'dart:io';

import 'package:catenary/main.dart';
import 'package:catenary/rail.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/open_conversation.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/thread.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential, direct, me, projection, room;

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

/// Past a route transition. Not `pumpAndSettle`: the reconnecting banner
/// animates for as long as the socket stays dead, which here is forever, and
/// tickers are left running so that a route really does come and go.
Future<void> settle(WidgetTester tester) async {
  await tester.pump();
  await tester.pump(const Duration(milliseconds: 500));
}

void main() {
  late Directory dir;

  SessionSeams seams() => SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) => _DeadSocket(),
        fetch: (x) async => const HttpAnswer(503, ''),
      );

  /// An enrolled device whose journal holds the room and the direct.
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
  }

  setUp(() => dir = Directory.systemTemp.createTempSync('catenary-restore-'));
  tearDown(() => dir.deleteSync(recursive: true));

  group('the store remembers the open conversation per account', () {
    Future<AppStore> started() async {
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isTrue);
      return store;
    }

    test('what one run records open, the next run reopens, once', () async {
      await enroll();
      final first = await started();
      expect(first.takeRestore(), isNull, reason: 'nothing was open before');
      first.rememberOpen(direct);

      final second = await started();
      expect(second.takeRestore(), direct);
      expect(second.takeRestore(), isNull, reason: 'a restore is offered once');
    });

    test('Back to the rail records none open', () async {
      await enroll();
      final store = await started();
      store.rememberOpen(room);
      store.rememberOpen(null);
      expect(File('${dir.path}/$openConversationFileName').existsSync(), isFalse);
      expect((await started()).takeRestore(), isNull);
    });

    test('an id the list does not hold is not reopened', () async {
      await enroll();
      writeOpenConversation(dir.path, me, 'cccccccc-cccc-4ccc-8ccc-cccccccccccc');
      expect((await started()).takeRestore(), isNull);
    });

    test('another account\'s open conversation is not this one\'s', () async {
      await enroll();
      writeOpenConversation(dir.path, '99999999-9999-4999-8999-999999999999', room);
      expect((await started()).takeRestore(), isNull);
    });

    test('a malformed file is none open', () async {
      await enroll();
      File('${dir.path}/$openConversationFileName').writeAsStringSync('not a record');
      expect((await started()).takeRestore(), isNull);
    });
  });

  group('a cold start', () {
    Future<AppStore> boot(WidgetTester tester) async {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.runAsync(enroll);
      final store = AppStore(seams());
      expect(await tester.runAsync(store.start), isTrue);
      return store;
    }

    Future<void> end(WidgetTester tester, AppStore store) async {
      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => store.dispose());
    }

    testWidgets('reopens the thread with no frame of the rail, over the rail, and Back reaches it', (tester) async {
      final store = await boot(tester);
      writeOpenConversation(dir.path, me, direct);
      // A store reads the file at start, as a cold start's does.
      expect(await tester.runAsync(store.start), isTrue);

      await tester.pumpWidget(CatenaryApp(initialMode: ThemeMode.dark, store: store));
      // THE FIRST FRAME: the thread, and no rail anywhere, onstage or off.
      expect(find.byType(ThreadScreen), findsOneWidget);
      expect(tester.widget<ThreadScreen>(find.byType(ThreadScreen)).conversation.id, direct);
      expect(find.byType(RailScreen, skipOffstage: false), findsNothing, reason: 'the rail must not flash before the restore');

      // The second frame: the thread is a route over the rail, already in place.
      await tester.pump();
      expect(find.byType(ThreadScreen), findsOneWidget);
      expect(tester.widget<ThreadScreen>(find.byType(ThreadScreen)).conversation.id, direct);
      expect(find.byType(RailScreen), findsNothing, reason: 'the rail is under the thread');
      expect(find.byType(RailScreen, skipOffstage: false), findsOneWidget);
      expect(readOpenConversation(dir.path, me), direct);

      // Back: the rail, and none open for the next start.
      tester.state<NavigatorState>(find.byType(Navigator)).pop();
      await settle(tester);
      expect(find.byType(RailScreen), findsOneWidget);
      expect(find.byType(ThreadScreen), findsNothing);
      expect(readOpenConversation(dir.path, me), isNull);

      await end(tester, store);
    });

    testWidgets('with an id the list does not hold lands on the rail', (tester) async {
      final store = await boot(tester);
      writeOpenConversation(dir.path, me, 'cccccccc-cccc-4ccc-8ccc-cccccccccccc');
      expect(await tester.runAsync(store.start), isTrue);

      await tester.pumpWidget(CatenaryApp(initialMode: ThemeMode.dark, store: store));
      expect(find.byType(RailScreen), findsOneWidget);
      expect(find.byType(ThreadScreen, skipOffstage: false), findsNothing);
      await settle(tester);
      expect(find.byType(ThreadScreen, skipOffstage: false), findsNothing);

      await end(tester, store);
    });

    testWidgets('opening a thread from the rail is what the next start reopens', (tester) async {
      final store = await boot(tester);
      await tester.pumpWidget(CatenaryApp(initialMode: ThemeMode.dark, store: store));
      expect(find.byType(RailScreen), findsOneWidget);

      await tester.tap(find.byKey(const ValueKey('rail-$room')));
      await settle(tester);
      expect(find.byType(ThreadScreen), findsOneWidget);
      expect(readOpenConversation(dir.path, me), room);

      await end(tester, store);
    });
  });
}
