// Enrollment always ends in a text (CANT-222, sub-task CANT-238): the form's
// button cannot stay disabled, an `Error` is an outcome and not a throw, a
// session that cannot start after a successful enrollment is the failed-start
// screen (ruling 2), and a journal wipe that failed is paid before any
// session runs.

import 'dart:io';

import 'package:catenary/enroll.dart';
import 'package:catenary/main.dart';
import 'package:catenary/rail.dart';
import 'package:catenary/start_failed.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/enrollment.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/theme.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential, projection;
import 'enroll_test.dart' show Server, token;

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

/// A store that reads as empty and whose write throws an `Error`, not an
/// `Exception`: a bug on this device after the server's 200.
final class _BrokenStore implements CredentialStore {
  @override
  Future<StoredCredential?> read() async => null;

  @override
  Future<T> update<T>(CredentialWrite<T> Function(StoredCredential? held) fn) async => throw StateError('a bug in the store');
}

void main() {
  late Directory dir;
  late Server server;
  late int built;
  var transportFails = false;

  SessionSeams seams() => SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) => _DeadSocket(),
        fetch: server.call,
        transportFactory: (cfg) {
          if (transportFails) throw StateError('no transport today');
          built++;
          return createTransport(cfg);
        },
      );

  setUp(() {
    dir = Directory.systemTemp.createTempSync('catenary-enroll-failures-');
    server = Server();
    built = 0;
    transportFails = false;
  });
  tearDown(() => dir.deleteSync(recursive: true));

  Future<void> show(WidgetTester tester, Widget app) async {
    tester.view.physicalSize = const Size(390, 844);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.reset);
    await tester.pumpWidget(TickerMode(enabled: false, child: app));
  }

  group('the form', () {
    testWidgets('the ENROLL button cannot stay disabled: a submit that throws is a text and the button back', (tester) async {
      await show(
        tester,
        MaterialApp(
          theme: catenaryTheme(Brightness.dark),
          home: EnrollScreen(deviceName: 'Android phone', onSubmit: (_, _, _) async => throw StateError('a bug in the store')),
        ),
      );
      await tester.enterText(find.byKey(const Key('enroll-address')), 'chat.example.com');
      await tester.enterText(find.byKey(const Key('enroll-token')), token);
      await tester.pump();
      await tester.tap(find.byKey(const Key('enroll-submit')));
      await tester.pump();

      expect(
        tester.widget<Text>(find.byKey(const Key('enroll-error'))).data,
        'This device hit an error while enrolling. Try again — if the token is then refused, it was used, and you need a fresh one from whoever invited you.',
      );
      expect(find.text('ENROLL DEVICE'), findsOneWidget);
      expect(find.text('ENROLLING…'), findsNothing);
      expect(tester.widget<FilledButton>(find.byKey(const Key('enroll-submit'))).onPressed, isNotNull);
    });
  });

  group('an enrollment that goes wrong on this device', () {
    test('a credential store that cannot be opened is a text, and no token is sent', () async {
      File('${dir.path}/$journalFileName').writeAsStringSync('this file is not a database\n');
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isFalse);
      expect(store.startFailure, isNull, reason: 'with no server file nothing was opened');

      final outcome = await store.enroll(typedAddress: 'chat.example.com', token: token, deviceName: 'Android phone', release: true);
      expect(outcome, isA<EnrollFailed>());
      expect((outcome as EnrollFailed).text, 'This device could not open its own storage, so the token was not sent.');
      expect(server.requests, isEmpty);
      expect(readAddress(dir.path), isNull);
    });

    test('an Error after the server\'s 200 reads as not stored', () async {
      final outcome = await enrollDeviceAt(
        directory: dir.path,
        store: _BrokenStore(),
        typedAddress: 'chat.example.com',
        token: token,
        deviceName: 'Android phone',
        release: true,
        fetch: server.call,
      );
      expect(server.enrolls, 1, reason: 'the token was spent');
      expect(outcome, isA<EnrollFailed>());
      expect(
        (outcome as EnrollFailed).text,
        'The server accepted that token, but this device could not save the credential. The token is now used — ask whoever invited you for a fresh one.',
      );
    });

    test('an Error before the server\'s 200 reads as a device fault', () async {
      final credentials = openCredentialStore(dir.path);
      addTearDown(credentials.close);
      final outcome = await enrollDeviceAt(
        directory: dir.path,
        store: credentials,
        typedAddress: 'chat.example.com',
        token: token,
        deviceName: 'Android phone',
        release: true,
        fetch: (x) async => x.url.path == '/enroll' ? throw StateError('a bug in the client') : server.call(x),
      );
      expect(outcome, isA<EnrollFailed>());
      expect((outcome as EnrollFailed).text, enrollDeviceFaultText);
      expect(await credentials.read(), isNull);
      expect(readAddress(dir.path), isNull, reason: 'a failed attempt leaves no address');
    });
  });

  group('[ruling 2 → option 0] a session that cannot start after a successful enrollment', () {
    testWidgets('is the failed-start screen, and the outcome is still Enrolled', (tester) async {
      transportFails = true;
      final store = AppStore(seams());
      expect(await tester.runAsync(store.start), isFalse);
      final outcome = await tester.runAsync(
        () => store.enroll(typedAddress: 'chat.example.com', token: token, deviceName: 'Android phone', release: true),
      );
      expect(outcome, isA<Enrolled>());
      expect(server.enrolls, 1);
      expect(store.startFailure, isNotNull);
      expect(store.startFailure!.wipeOwed, isFalse);
      expect(store.enrolled, isFalse);

      await show(tester, CatenaryApp(initialMode: ThemeMode.dark, store: store));
      expect(find.byType(StartFailedScreen), findsOneWidget);
      expect(find.textContaining('has been set up for Catenary'), findsOneWidget);
      expect(find.byType(EnrollScreen), findsNothing);

      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => store.dispose());
    });
  });

  group('a journal wipe that failed', () {
    var wipeFails = true;
    var wipes = 0;

    Future<void> wipe(String directory) async {
      wipes++;
      if (wipeFails) throw const FileSystemException('disk full');
      await wipeJournalFile(directory);
    }

    /// A device enrolled at chat.example.com with the canvas in its journal.
    Future<void> enrolled() async {
      final credentials = openCredentialStore(dir.path);
      await enrollCredential(credentials, inProcessLock(), credential());
      credentials.close();
      writeAddress(dir.path, 'https://chat.example.com');
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

    int journalConversations() {
      final journal = SqliteJournal.open('${dir.path}/$journalFileName');
      try {
        return journal.snapshot().conversations.length;
      } finally {
        journal.close();
      }
    }

    setUp(() {
      wipeFails = true;
      wipes = 0;
    });

    test('is paid before any session runs', () async {
      await enrolled();
      final store = AppStore(seams(), systemClock, wipe);
      addTearDown(store.dispose);
      expect(await store.start(), isTrue);
      expect(store.conversations, hasLength(2));
      expect(built, 1);

      // The token is accepted, the pair is replaced, and the wipe throws.
      final outcome = await store.reenroll(token: token, deviceName: 'Android phone', release: true);
      expect(outcome, isA<Enrolled>());
      expect(store.startFailure, isNotNull);
      expect(store.startFailure!.wipeOwed, isTrue);
      expect(store.startFailure!.name, 'FileSystemException');
      expect(store.enrolled, isFalse);
      expect(store.conversations, isEmpty);
      expect(built, 1, reason: 'no transport was constructed over the old journal');
      expect(journalConversations(), 2, reason: 'the journal is as the previous sign-in left it');

      // Still failing: still owed, and still no transport.
      expect(await store.start(), isFalse);
      expect(store.startFailure!.wipeOwed, isTrue);
      expect(store.enrolled, isFalse);
      expect(built, 1);
      expect(wipes, 2);

      // The wipe lands, and only then does a session start.
      wipeFails = false;
      expect(await store.start(), isTrue);
      expect(store.startFailure, isNull);
      expect(store.enrolled, isTrue);
      expect(store.conversations, isEmpty);
      expect(built, 2);
      expect(wipes, 3);

      // Paid once: a later start does not wipe again.
      expect(await store.start(), isTrue);
      expect(wipes, 3);
    });

    testWidgets('is drawn as such', (tester) async {
      await tester.runAsync(enrolled);
      final store = AppStore(seams(), systemClock, wipe);
      expect(await tester.runAsync(store.start), isTrue);
      final outcome = await tester.runAsync(() => store.reenroll(token: token, deviceName: 'Android phone', release: true));
      expect(outcome, isA<Enrolled>());

      await show(tester, CatenaryApp(initialMode: ThemeMode.dark, store: store));
      expect(find.text("This device's data could not be opened"), findsOneWidget);
      expect(
        find.text(
            'This device was re-enrolled, but Catenary could not clear the messages the previous sign-in left here, so it has not opened them. Anything waiting to send is untouched.'),
        findsOneWidget,
      );
      expect(find.byType(RailScreen), findsNothing);
      expect(find.byType(EnrollScreen), findsNothing);

      await tester.pumpWidget(const SizedBox());
      await tester.runAsync(() async => store.dispose());
    });
  });
}
