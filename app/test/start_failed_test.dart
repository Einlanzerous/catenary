// A start that fails is a screen (CANT-222, sub-task CANT-237): the store
// reports it and does not throw, the app draws the failed-start screen and
// never the enrollment form, and nothing on the device is deleted.
//
// Over real files in a temporary directory, and a socket that never opens.
// "Not a database" is a file of plain text where SQLite expects its header.

import 'dart:io';

import 'package:catenary/enroll.dart';
import 'package:catenary/main.dart';
import 'package:catenary/rail.dart';
import 'package:catenary/start_failed.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/theme.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential;

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

const _garbage = 'this file is not a database, and has never been one\n';

void main() {
  late Directory dir;

  SessionSeams seams() => SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) => _DeadSocket(),
        fetch: (_) async => const HttpAnswer(503, ''),
      );

  File journalFile() => File('${dir.path}/$journalFileName');
  File outboxFile() => File('${dir.path}/$outboxFileName');
  File serverFile() => File('${dir.path}/server');

  /// A `server` file and a `catenary.db` that is not a database.
  void unreadableJournal() {
    writeAddress(dir.path, 'https://chat.example.com');
    journalFile().writeAsStringSync(_garbage);
  }

  /// An enrolled device whose `catenary-outbox.db` is not a database.
  Future<void> unreadableOutbox() async {
    final credentials = openCredentialStore(dir.path);
    await enrollCredential(credentials, inProcessLock(), credential());
    credentials.close();
    writeAddress(dir.path, 'https://chat.example.com');
    outboxFile().writeAsStringSync(_garbage);
  }

  setUp(() => dir = Directory.systemTemp.createTempSync('catenary-start-failed-'));
  tearDown(() => dir.deleteSync(recursive: true));

  Future<void> show(WidgetTester tester, Widget app) async {
    tester.view.physicalSize = const Size(390, 844);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.reset);
    await tester.pumpWidget(TickerMode(enabled: false, child: app));
  }

  /// Lets a start that a tap began run its real file work, then draws.
  Future<void> settle(WidgetTester tester) async {
    for (var i = 0; i < 5; i++) {
      await tester.runAsync(() => Future<void>.delayed(const Duration(milliseconds: 20)));
      await tester.pump();
    }
  }

  group('the store', () {
    test('a failed start is reported, not thrown', () async {
      unreadableJournal();
      final store = AppStore(seams());
      addTearDown(store.dispose);
      var notified = 0;
      store.addListener(() => notified++);
      expect(await store.start(), isFalse);
      expect(store.startFailure, isNotNull);
      expect(store.startFailure!.name, 'SqliteException');
      expect(store.startFailure!.wipeOwed, isFalse);
      expect(store.enrolled, isFalse);
      expect(store.conversations, isEmpty);
      expect(notified, 1);
    });

    test('an unreadable outbox file is a failed start too, and nothing is deleted', () async {
      await unreadableOutbox();
      final before = outboxFile().readAsBytesSync();
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isFalse);
      expect(store.startFailure, isNotNull);
      expect(store.enrolled, isFalse);
      expect(outboxFile().readAsBytesSync(), before);
      expect(journalFile().existsSync(), isTrue);
      expect(serverFile().existsSync(), isTrue);
    });

    test('a start that succeeds clears the failure', () async {
      await unreadableOutbox();
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isFalse);
      // The cause removed by the test, never by the app.
      outboxFile().deleteSync();
      expect(await store.start(), isTrue);
      expect(store.startFailure, isNull);
      expect(store.enrolled, isTrue);
    });

    test('a device that is not enrolled has no failure', () async {
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isFalse);
      expect(store.startFailure, isNull);
    });
  });

  group('the app', () {
    for (final (name, mode) in [('dark', ThemeMode.dark), ('light', ThemeMode.light)]) {
      testWidgets('draws the failed-start screen and not the enrollment form, $name', (tester) async {
        unreadableJournal();
        final store = AppStore(seams());
        expect(await tester.runAsync(store.start), isFalse);
        await show(tester, CatenaryApp(initialMode: mode, store: store));

        expect(find.text("This device's data could not be opened"), findsOneWidget);
        expect(
          find.text(
              'This device has been set up for Catenary, but Catenary could not open what it keeps here: your messages, and anything waiting to send. Catenary has not deleted anything.'),
          findsOneWidget,
        );
        expect(find.byType(EnrollScreen), findsNothing);
        expect(find.byType(RailScreen), findsNothing);

        await tester.pumpWidget(const SizedBox());
        await tester.runAsync(() async => store.dispose());
      });
    }

    testWidgets('[ruling 0 → option 0] shows the error\'s type name and nothing else of it', (tester) async {
      unreadableJournal();
      final store = AppStore(seams());
      expect(await tester.runAsync(store.start), isFalse);
      await show(tester, CatenaryApp(initialMode: ThemeMode.dark, store: store));

      expect(tester.widget<Text>(find.byKey(const Key('start-failed-name'))).data, 'SqliteException');
      for (final text in tester.widgetList<Text>(find.byType(Text))) {
        expect(text.data, isNot(contains(dir.path)));
        expect(text.data, isNot(contains('not a database')));
      }

      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => store.dispose());
    });

    testWidgets('[ruling 1 → option 0] TRY AGAIN starts the session once the cause is gone, and deletes nothing when it is not', (tester) async {
      await tester.runAsync(unreadableOutbox);
      final before = outboxFile().readAsBytesSync();
      final store = AppStore(seams());
      expect(await tester.runAsync(store.start), isFalse);
      await show(tester, CatenaryApp(initialMode: ThemeMode.dark, store: store));
      final retry = find.byKey(const Key('start-failed-retry'));
      expect(find.text('TRY AGAIN'), findsOneWidget);

      // Still unreadable: the same screen, the button back, the file as it was.
      await tester.tap(retry);
      await settle(tester);
      expect(find.byType(StartFailedScreen), findsOneWidget);
      expect(find.text('TRY AGAIN'), findsOneWidget);
      expect(tester.widget<FilledButton>(retry).onPressed, isNotNull);
      expect(outboxFile().readAsBytesSync(), before);
      expect(journalFile().existsSync(), isTrue);

      // The test takes the file away; the app opens a fresh one.
      outboxFile().deleteSync();
      await tester.tap(retry);
      await settle(tester);
      expect(find.byType(StartFailedScreen), findsNothing);
      expect(find.byType(RailScreen), findsOneWidget);
      expect(find.byType(EnrollScreen), findsNothing);

      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => store.dispose());
    });

    testWidgets('the button is disabled and says so while an attempt runs', (tester) async {
      await show(
        tester,
        MaterialApp(
          theme: catenaryTheme(Brightness.dark),
          home: StartFailedScreen(
            cause: StartFailedCause.stores,
            name: 'SqliteException',
            onRetry: () => Future<void>.delayed(const Duration(seconds: 1)),
          ),
        ),
      );
      await tester.tap(find.byKey(const Key('start-failed-retry')));
      await tester.pump();
      expect(find.text('TRYING…'), findsOneWidget);
      expect(tester.widget<FilledButton>(find.byKey(const Key('start-failed-retry'))).onPressed, isNull);
      await tester.pump(const Duration(seconds: 1));
      expect(find.text('TRY AGAIN'), findsOneWidget);
    });
  });

  group('a directory the platform cannot name', () {
    testWidgets('still draws a frame, and TRY AGAIN opens the store once it can', (tester) async {
      var fail = true;
      late AppStore opened;
      Future<AppStore> open() async {
        if (fail) throw const FileSystemException('no support directory');
        return opened = AppStore(seams());
      }

      final app = await tester.runAsync(() => bootApp(open));
      await show(tester, app!);
      expect(find.text("This device's data could not be opened"), findsOneWidget);
      expect(
        find.text(
            'Catenary could not reach its own storage on this device, so it cannot tell whether this device is enrolled. Catenary has not deleted anything.'),
        findsOneWidget,
      );
      expect(tester.widget<Text>(find.byKey(const Key('start-failed-name'))).data, 'FileSystemException');
      expect(find.byType(EnrollScreen), findsNothing);

      // The directory is there now, and holds no enrollment.
      fail = false;
      await tester.tap(find.byKey(const Key('start-failed-retry')));
      await settle(tester);
      expect(find.byType(StartFailedScreen), findsNothing);
      expect(find.byType(EnrollScreen), findsOneWidget);

      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => opened.dispose());
    });

    testWidgets('a store that opens and cannot start is the stores screen, through the same function', (tester) async {
      unreadableJournal();
      late AppStore opened;
      final app = await tester.runAsync(() => bootApp(() async => opened = AppStore(seams())));
      await show(tester, app!);
      expect(find.byKey(const Key('start-failed-text')), findsOneWidget);
      expect(find.textContaining('has been set up for Catenary'), findsOneWidget);
      expect(find.byType(EnrollScreen), findsNothing);

      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => opened.dispose());
    });
  });
}
