// CANT-208 — the enrollment screen and RE-ENROLL (CANT-200 rulings 0 and 3).
//
// Three layers, each with no more than it needs: the link parser over strings;
// the screen over a recorded `onSubmit`; and the store over a real directory
// and a scripted server, which is where "wipes the journal" and "leaves the
// outbox" are asserted, since both are facts about files.

import 'dart:convert';
import 'dart:io';

import 'package:catenary/enroll.dart';
import 'package:catenary/main.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/enrollment.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/theme.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential, me, nadia, projection, room;

const token = 'AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdE';
final newAccess = 'a' * 43;
final newRefresh = 'r' * 43;

/// A scripted server: `/healthz` and `/enroll`, recording what was asked.
final class Server {
  Server({this.health = 200, this.enrollStatus = 200, this.userId = me});

  int health;
  int enrollStatus;
  String userId;
  final requests = <String>[];
  final bodies = <Map<String, dynamic>>[];

  Future<HttpAnswer> call(HttpExchange x) async {
    requests.add('${x.method} ${x.url.path}');
    if (x.url.path == '/healthz') return HttpAnswer(health, '{}');
    if (x.url.path == '/enroll') {
      bodies.add(jsonDecode(x.body!) as Map<String, dynamic>);
      if (enrollStatus != 200) return HttpAnswer(enrollStatus, '{}');
      return HttpAnswer(
        200,
        jsonEncode({
          'user_id': userId,
          'device_id': '99999999-9999-4999-8999-999999999999',
          'access_token': newAccess,
          'access_expires_at': '2100-01-01T00:00:00.000Z',
          'refresh_token': newRefresh,
          'refresh_expires_at': '2100-01-01T00:00:00.000Z',
        }),
      );
    }
    return const HttpAnswer(404, '');
  }

  int get enrolls => requests.where((r) => r.endsWith('/enroll')).length;
}

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
  late Server server;

  SessionSeams seams() => SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) => _DeadSocket(),
        fetch: server.call,
      );

  setUp(() {
    dir = Directory.systemTemp.createTempSync('catenary-enroll-');
    server = Server();
  });
  tearDown(() => dir.deleteSync(recursive: true));

  group('[ruling 0 → option 1] the link', () {
    test('https://<host>/#enroll=<token> is an origin and a token', () {
      final l = parseEnrollLink('https://chat.example.com/#enroll=$token')!;
      expect((l.origin, l.token), ('https://chat.example.com', token));
    });

    test('a trailing slash, a port, no path and surrounding whitespace', () {
      expect(parseEnrollLink('https://chat.example.com/#enroll=$token/'), isNull, reason: 'the token is exactly 43 characters');
      expect(parseEnrollLink('  https://chat.example.com:8443/#enroll=$token \n')!.origin, 'https://chat.example.com:8443');
      expect(parseEnrollLink('https://chat.example.com#enroll=$token')!.origin, 'https://chat.example.com');
      expect(parseEnrollLink('https://chat.example.com//#enroll=$token'), isNull, reason: 'a path other than / is not the shape');
      expect(parseEnrollLink('http://192.168.1.20:4012/#enroll=$token')!.origin, 'http://192.168.1.20:4012');
    });

    test('a fragment whose token does not match the 43-character pattern is not a link', () {
      for (final bad in [
        'https://chat.example.com/#enroll=short',
        'https://chat.example.com/#enroll=${token}x',
        'https://chat.example.com/#enroll=${token.substring(1)}!',
        'https://chat.example.com/#enroll=',
        'https://chat.example.com/#token=$token',
        'https://chat.example.com/?enroll=$token',
        'chat.example.com',
        token,
        '',
      ]) {
        expect(parseEnrollLink(bad), isNull, reason: bad);
      }
    });
  });

  group('the screen', () {
    Future<List<(String, String, String)>> pump(WidgetTester tester, {String? locked, EnrollOutcome? answer, String name = 'Android phone'}) async {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      final submitted = <(String, String, String)>[];
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: EnrollScreen(
          deviceName: name,
          lockedAddress: locked,
          onCancel: locked == null ? null : () {},
          onSubmit: (a, t, n) async {
            submitted.add((a, t, n));
            return answer ?? const EnrollFailed('refused');
          },
        ),
      ));
      return submitted;
    }

    String field(WidgetTester tester, String key) => tester.widget<TextField>(find.byKey(Key(key))).controller!.text;

    testWidgets('[ruling 0 → option 1] a link pasted into the token field fills the address and the token', (tester) async {
      await pump(tester);
      expect(field(tester, 'enroll-address'), isEmpty, reason: 'ruling 1: no build carries an address');
      await tester.enterText(find.byKey(const Key('enroll-token')), ' https://chat.example.com:8443/#enroll=$token ');
      expect(field(tester, 'enroll-address'), 'https://chat.example.com:8443');
      expect(field(tester, 'enroll-token'), token);
    });

    testWidgets('[ruling 0 → option 1] a link pasted into the address field fills both too', (tester) async {
      await pump(tester);
      await tester.enterText(find.byKey(const Key('enroll-address')), 'https://chat.example.com/#enroll=$token');
      expect(field(tester, 'enroll-address'), 'https://chat.example.com');
      expect(field(tester, 'enroll-token'), token);
    });

    testWidgets('[ruling 0 → option 1] a string that is not a link is left where it was pasted', (tester) async {
      await pump(tester);
      await tester.enterText(find.byKey(const Key('enroll-token')), 'https://chat.example.com/#enroll=short');
      expect(field(tester, 'enroll-token'), 'https://chat.example.com/#enroll=short');
      expect(field(tester, 'enroll-address'), isEmpty);
      await tester.enterText(find.byKey(const Key('enroll-address')), 'chat.example.com');
      expect(field(tester, 'enroll-address'), 'chat.example.com');
    });

    testWidgets('the name passed to enrollment is the field\'s trimmed content, the address and token too', (tester) async {
      final submitted = await pump(tester);
      expect(field(tester, 'enroll-name'), 'Android phone');
      await tester.enterText(find.byKey(const Key('enroll-address')), '  chat.example.com ');
      await tester.enterText(find.byKey(const Key('enroll-token')), '  $token ');
      await tester.enterText(find.byKey(const Key('enroll-name')), '  Rosa\'s Pixel  ');
      await tester.tap(find.byKey(const Key('enroll-submit')));
      await tester.pump();
      expect(submitted, [('chat.example.com', token, 'Rosa\'s Pixel')]);
      expect(find.byKey(const Key('enroll-error')), findsOneWidget);
      expect(find.text('refused'), findsOneWidget);
    });

    test('the device name starts as Android phone or Linux desktop by platform', () {
      expect(defaultDeviceName('android'), 'Android phone');
      expect(defaultDeviceName('linux'), 'Linux desktop');
    });

    testWidgets('[ruling 3 → option 0] on a re-enrollment the address is shown and not editable', (tester) async {
      await pump(tester, locked: 'https://chat.example.com');
      expect(field(tester, 'enroll-address'), 'https://chat.example.com');
      expect(tester.widget<TextField>(find.byKey(const Key('enroll-address'))).enabled, isFalse);
      expect(find.text('Re-enroll this device'), findsOneWidget);
      expect(find.byKey(const Key('enroll-cancel')), findsOneWidget);
    });
  });

  group('the first screen of an app that is not enrolled', () {
    testWidgets('is the enrollment screen, and a could-not-reach result is shown as text on it', (tester) async {
      server.health = 503;
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      final store = AppStore(seams());
      await tester.runAsync(store.start);
      await tester.pumpWidget(TickerMode(enabled: false, child: CatenaryApp(initialMode: ThemeMode.dark, store: store)));
      expect(find.byType(EnrollScreen), findsOneWidget);
      expect(find.byKey(const ValueKey('rail-kitchen')), findsNothing, reason: 'no fixture is shown');

      await tester.enterText(find.byKey(const Key('enroll-address')), 'chat.example.com');
      await tester.enterText(find.byKey(const Key('enroll-token')), token);
      await tester.pump();
      await tester.tap(find.byKey(const Key('enroll-submit')));
      await tester.runAsync(() => Future<void>.delayed(const Duration(milliseconds: 100)));
      await tester.pump();
      expect(find.textContaining('Could not reach that server'), findsOneWidget);
      expect(server.enrolls, 0, reason: 'a failed probe spends no token');
      expect(readAddress(dir.path), isNull);

      await tester.pumpWidget(const SizedBox());
      store.dispose();
    });
  });

  group('the store', () {
    /// A device enrolled as `me` at [address] with the canvas in its journal and
    /// one unsent message in its outbox, as a previous launch would leave it.
    Future<void> enrolled(String address) async {
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
      final started = await startSession(seams());
      final session = (started as SessionRunning).session;
      await session.outbox.compose(OutboxDraft(conversationId: room, text: 'unsent'));
      session.end();
    }

    Future<int> outboxCount() async {
      final s = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
      try {
        return (await s.list()).length;
      } finally {
        s.close();
      }
    }

    Future<void> terminal(AppStore store) async => expect(await store.start(), isTrue);

    for (final (label, userId) in [('the same user_id', me), ('a different user_id', nadia)]) {
      test('[ruling 3 → option 0] re-enrolling as $label replaces the pair, wipes the journal, starts a new transport and keeps the outbox', () async {
        await enrolled('https://chat.example.com');
        final before = await outboxCount();
        expect(before, 1);
        server.userId = userId;
        final store = AppStore(seams());
        addTearDown(store.dispose);
        await terminal(store);
        expect(store.conversations, hasLength(2));
        expect(store.address, 'https://chat.example.com');

        final outcome = await store.reenroll(token: token, deviceName: 'Rosa\'s Pixel', release: true);
        expect(outcome, isA<Enrolled>());
        expect((outcome as Enrolled).credential.userId, userId);

        // The pair is the new one (reenrollCredential), under the same address.
        final credentials = openCredentialStore(dir.path);
        final held = await credentials.read();
        credentials.close();
        expect((held!.deviceId, held.accessToken, held.userId), ('99999999-9999-4999-8999-999999999999', newAccess, userId));
        expect(readAddress(dir.path), 'https://chat.example.com');
        expect(server.bodies.single, containsPair('device_name', 'Rosa\'s Pixel'));

        // The journal holds no conversation and no cursor; a new transport runs.
        expect(store.enrolled, isTrue);
        expect(store.conversations, isEmpty);
        final fresh = SqliteJournal.open('${dir.path}/$journalFileName');
        final snapshot = fresh.snapshot();
        fresh.close();
        expect(snapshot.conversations, isEmpty);
        expect(snapshot.messages, isEmpty);
        expect(snapshot.cursor, isNull);

        expect(await outboxCount(), before, reason: 'the outbox is keyed by account and never touched');
      });
    }

    test('a refused re-enrollment changes nothing: the pair, the address and the journal stay', () async {
      await enrolled('https://chat.example.com');
      server.enrollStatus = 401;
      final store = AppStore(seams());
      addTearDown(store.dispose);
      await terminal(store);
      final outcome = await store.reenroll(token: token, deviceName: 'x', release: true);
      expect(outcome, isA<EnrollFailed>());
      expect((outcome as EnrollFailed).text, startsWith('That enrollment token was not accepted'));
      expect(store.conversations, hasLength(2));
      final credentials = openCredentialStore(dir.path);
      expect((await credentials.read())!.accessToken, 'access');
      credentials.close();
    });

    test('a first enrollment writes the address, stores the pair and starts a session', () async {
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isFalse);
      final outcome = await store.enroll(typedAddress: 'chat.example.com', token: token, deviceName: 'Linux desktop', release: true);
      expect(outcome, isA<Enrolled>());
      expect(store.enrolled, isTrue);
      expect(readAddress(dir.path), 'https://chat.example.com');
      expect(server.requests.take(2), ['GET /healthz', 'POST /enroll']);
      expect(server.bodies.single, {'enrollment_token': token, 'device_name': 'Linux desktop'});
    });

    test('the web\'s two refusal texts, and a failed attempt leaves no address behind', () async {
      final store = AppStore(seams());
      addTearDown(store.dispose);
      server.enrollStatus = 400;
      var o = await store.enroll(typedAddress: 'chat.example.com', token: 'x', deviceName: 'n', release: true) as EnrollFailed;
      expect(o.text, 'That does not look like a valid enrollment token or device name.');
      server.enrollStatus = 401;
      o = await store.enroll(typedAddress: 'chat.example.com', token: token, deviceName: 'n', release: true) as EnrollFailed;
      expect(o.text, startsWith('That enrollment token was not accepted'));
      expect(readAddress(dir.path), isNull);
      expect(store.enrolled, isFalse);
    });

    test('a release build refuses http:// before anything is sent', () async {
      final store = AppStore(seams());
      addTearDown(store.dispose);
      final o = await store.enroll(typedAddress: 'http://192.168.1.20:4012', token: token, deviceName: 'n', release: true);
      expect(o, isA<EnrollFailed>());
      expect(server.requests, isEmpty);
    });
  });
}
