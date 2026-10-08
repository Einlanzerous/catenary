// CANT-273 (CANT-253's app half): the rail starts a direct and then a group
// against the real transport's REST seam. The store's half runs over a real
// session in a temporary directory with a scripted `HttpFetch`; the widget half
// drives the screen through the callbacks the app wires.

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:catenary/fixtures.dart';
import 'package:catenary/new_conversation.dart';
import 'package:catenary/rail.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/store/start.dart';
import 'package:catenary/theme.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential, direct, ilse, nadia, projection, room;

const _unheld = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const _requestPattern = r'^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$';

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

/// Counts `catchUp` and forwards everything else to a real transport.
final class _CatchUpCounting implements Transport {
  _CatchUpCounting(this.inner);

  final Transport inner;
  var catchUps = 0;

  @override
  void catchUp() {
    catchUps++;
    inner.catchUp();
  }

  @override
  void retryNow() => inner.retryNow();
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

wire.Conversation _conv(String id, wire.ConversationKind kind, String name, int members) =>
    wire.Conversation(id: id, kind: kind, name: name, memberCount: members, headSeq: 0);

void main() {
  group('the store starts conversations over the REST seam', () {
    late Directory dir;
    late _CatchUpCounting transport;
    late List<HttpExchange> sent;
    late Map<String, HttpAnswer Function(HttpExchange)> routes;

    SessionSeams seams() => SessionSeams(
          directory: dir.path,
          lifecycle: ManualLifecycle(),
          connect: (url, protocols) => _DeadSocket(),
          fetch: (x) async {
            sent.add(x);
            final route = routes[x.url.path];
            return route == null ? const HttpAnswer(503, '') : route(x);
          },
          transportFactory: (cfg) => transport = _CatchUpCounting(createTransport(cfg)),
        );

    Future<AppStore> started() async {
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
      final store = AppStore(seams());
      addTearDown(store.dispose);
      expect(await store.start(), isTrue);
      return store;
    }

    List<HttpExchange> posts() => [for (final x in sent) if (x.method == 'POST') x];

    setUp(() {
      dir = Directory.systemTemp.createTempSync('catenary-start-');
      sent = [];
      routes = {};
    });
    tearDown(() => dir.deleteSync(recursive: true));

    test('the roster is GET /users, as people with the handles a start names them by', () async {
      routes['/users'] = (_) => HttpAnswer(
            200,
            jsonEncode(wire.RosterResponse(users: [
              wire.RosterEntry(id: nadia, name: 'Nadia Okonkwo', initials: 'NO', handle: 'nadia'),
              wire.RosterEntry(id: ilse, name: 'Ilse Marchetti', handle: 'ilse'),
            ]).toJson()),
          );
      final store = await started();
      final people = (await store.roster())!;
      expect(people.map((p) => p.handle), ['nadia', 'ilse']);
      expect(people.map((p) => p.tile), ['NO', 'IM'], reason: 'the server\'s letters when it sent them, derived otherwise');
      routes.remove('/users');
      expect(await store.roster(), isNull, reason: 'a roster that could not be had is not an empty one');
    });

    test('a direct posts the handle; a conversation the journal already holds is opened with no catch-up', () async {
      routes['/conversations/direct'] = (_) => HttpAnswer(200, jsonEncode(_conv(direct, wire.ConversationKind.direct, 'Nadia O.', 2).toJson()));
      final store = await started();
      final outcome = await store.startDirect('nadia');
      expect(outcome, isA<StartDone>());
      expect((outcome as StartDone).conversationId, direct);
      expect(outcome.held, isTrue);
      expect(jsonDecode(posts().single.body!), {'handle': 'nadia'});
      expect(transport.catchUps, 0);
    });

    test('a conversation the journal does not hold yet triggers a catch-up and is reported as not held when /sync does not serve it', () async {
      routes['/conversations/direct'] = (_) => HttpAnswer(201, jsonEncode(_conv(_unheld, wire.ConversationKind.direct, 'Ilse', 2).toJson()));
      final store = await started();
      store.startWait = const Duration(milliseconds: 50);
      final outcome = await store.startDirect('ilse') as StartDone;
      expect(outcome.conversationId, _unheld);
      expect(outcome.held, isFalse);
      expect(transport.catchUps, greaterThan(0));
      expect(store.conversation(_unheld), isNull, reason: 'the returned record is never placed in the journal by hand');
    });

    test('a group posts the trimmed name, the handles and the form\'s request_id', () async {
      routes['/conversations'] = (_) => HttpAnswer(201, jsonEncode(_conv(room, wire.ConversationKind.group, 'Kitchen', 3).toJson()));
      final store = await started();
      final id = newRequestId();
      expect(id, matches(RegExp(_requestPattern)));
      final outcome = await store.startGroup('  Kitchen  ', ['nadia', 'ilse'], requestId: id);
      expect((outcome as StartDone).conversationId, room);
      expect(jsonDecode(posts().single.body!), {'name': 'Kitchen', 'member_handles': ['nadia', 'ilse'], 'request_id': id});
    });

    test('a refusal and a dead network are different outcomes', () async {
      routes['/conversations/direct'] = (_) => const HttpAnswer(404, '{"code":"conversation_not_found"}');
      final store = await started();
      final refused = await store.startDirect('nobody');
      expect(refused, isA<StartRefusedBy>());
      expect((refused as StartRefusedBy).code, 'conversation_not_found');
      routes.remove('/conversations/direct');
      expect(await store.startDirect('nadia'), isA<StartUnreachableNow>());
    });
  });

  group('the new-conversation screen', () {
    const nadiaP = RosterPerson(id: nadia, name: 'Nadia Okonkwo', handle: 'nadia', initials: 'NO');
    const ilseP = RosterPerson(id: ilse, name: 'Ilse Marchetti', handle: 'ilse');
    const meP = RosterPerson(id: '55555555-5555-4555-8555-555555555555', name: 'Third Person', handle: 'third');

    Future<void> pump(
      WidgetTester tester, {
      Future<List<RosterPerson>?> Function()? roster,
      Future<StartOutcome> Function(RosterPerson)? onDirect,
      Future<StartOutcome> Function(String, List<RosterPerson>, String)? onGroup,
      void Function(String, bool)? onDone,
    }) {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      return tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: NewConversationScreen(
          loadRoster: roster ?? () async => [nadiaP, ilseP, meP],
          onDirect: onDirect ?? (_) async => const StartDone(direct, held: true),
          onGroup: onGroup ?? (_, _, _) async => const StartDone(room, held: true),
          onDone: onDone ?? (_, _) {},
        ),
      ));
    }

    Future<void> pick(WidgetTester tester, String id) async {
      await tester.tap(find.byKey(ValueKey('person-$id')));
      await tester.pump();
    }

    bool submitEnabled(WidgetTester tester) => tester.widget<FilledButton>(find.byKey(const ValueKey('new-submit'))).onPressed != null;

    testWidgets('picking one person requests a direct with their handle, and the conversation is opened', (tester) async {
      final asked = <String>[];
      final done = <(String, bool)>[];
      await pump(tester, onDirect: (p) async {
        asked.add(p.handle);
        return const StartDone(direct, held: true);
      }, onDone: (id, held) => done.add((id, held)));
      await tester.pumpAndSettle();
      expect(find.text('Nadia Okonkwo'), findsOneWidget);
      expect(submitEnabled(tester), isFalse, reason: 'nobody picked');
      expect(find.byKey(const ValueKey('new-room-name')), findsNothing);
      await pick(tester, nadia);
      expect(find.text('START DIRECT'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('new-submit')));
      await tester.pumpAndSettle();
      expect(asked, ['nadia']);
      expect(done, [(direct, true)]);
    });

    testWidgets('picking two reveals a name field, needs the name, and requests a named group', (tester) async {
      final asked = <(String, List<String>, String)>[];
      await pump(tester, onGroup: (name, people, requestId) async {
        asked.add((name, [for (final p in people) p.handle], requestId));
        return const StartDone(room, held: true);
      });
      await tester.pumpAndSettle();
      await pick(tester, nadia);
      await pick(tester, ilse);
      expect(find.byKey(const ValueKey('new-room-name')), findsOneWidget);
      expect(find.text('CREATE ROOM · 3 MEMBERS'), findsOneWidget, reason: 'the two picked and you');
      expect(submitEnabled(tester), isFalse, reason: 'a room needs a name');
      await tester.enterText(find.byKey(const ValueKey('new-room-name')), '  Kitchen Table ');
      await tester.pump();
      expect(submitEnabled(tester), isTrue);
      await tester.tap(find.byKey(const ValueKey('new-submit')));
      await tester.pumpAndSettle();
      expect(asked.single.$1, 'Kitchen Table');
      expect(asked.single.$2, ['nadia', 'ilse']);
      expect(asked.single.$3, matches(RegExp(_requestPattern)));
    });

    testWidgets('the control is disabled while a start is in flight', (tester) async {
      final gate = Completer<StartOutcome>();
      var calls = 0;
      await pump(tester, onDirect: (_) {
        calls++;
        return gate.future;
      });
      await tester.pumpAndSettle();
      await pick(tester, nadia);
      await tester.tap(find.byKey(const ValueKey('new-submit')));
      await tester.pump();
      expect(find.text('STARTING…'), findsOneWidget);
      expect(submitEnabled(tester), isFalse);
      await pick(tester, ilse);
      expect(find.byKey(const ValueKey('new-room-name')), findsNothing, reason: 'the picks do not move under a request');
      gate.complete(const StartDone(direct, held: true));
      await tester.pumpAndSettle();
      expect(calls, 1);
    });

    testWidgets('a refusal and an unreachable answer are visible, retryable and say different things', (tester) async {
      var answer = const StartRefusedBy('conversation_not_found') as StartOutcome;
      await pump(tester, onDirect: (_) async => answer);
      await tester.pumpAndSettle();
      await pick(tester, nadia);
      await tester.tap(find.byKey(const ValueKey('new-submit')));
      await tester.pumpAndSettle();
      expect(find.textContaining('could not find one of them'), findsOneWidget);
      expect(submitEnabled(tester), isTrue, reason: 'retryable');
      answer = const StartUnreachableNow();
      await tester.tap(find.byKey(const ValueKey('new-submit')));
      await tester.pumpAndSettle();
      expect(find.textContaining('Could not reach the server'), findsOneWidget);
      expect(find.textContaining('could not find one of them'), findsNothing);
    });

    testWidgets('a retry of the same group form is a replay and an edited one is a new room', (tester) async {
      final ids = <String>[];
      var answer = const StartUnreachableNow() as StartOutcome;
      await pump(tester, onGroup: (_, _, requestId) async {
        ids.add(requestId);
        return answer;
      });
      await tester.pumpAndSettle();
      await pick(tester, nadia);
      await pick(tester, ilse);
      await tester.enterText(find.byKey(const ValueKey('new-room-name')), 'Kitchen');
      await tester.pump();
      await tester.tap(find.byKey(const ValueKey('new-submit')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('new-submit')));
      await tester.pumpAndSettle();
      expect(ids[1], ids[0]);
      await tester.enterText(find.byKey(const ValueKey('new-room-name')), 'Kitchen Two');
      await tester.pump();
      answer = const StartDone(room, held: true);
      await tester.tap(find.byKey(const ValueKey('new-submit')));
      await tester.pumpAndSettle();
      expect(ids[2], isNot(ids[0]));
    });

    testWidgets('a roster that fails to load says so and loads again on RETRY; an empty one says nobody is there', (tester) async {
      var ok = false;
      await pump(tester, roster: () async => ok ? const <RosterPerson>[] : null);
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('new-roster-failed')), findsOneWidget);
      ok = true;
      await tester.tap(find.byKey(const ValueKey('new-roster-retry')));
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('new-empty')), findsOneWidget);
      expect(find.byKey(const ValueKey('new-submit')), findsNothing);
    });
  });

  group('the rail\'s control', () {
    testWidgets('is drawn when the rail can start a conversation and taps through; the fixture rail has none', (tester) async {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      var tapped = 0;
      Widget rail({VoidCallback? onNew}) => MaterialApp(
            theme: catenaryTheme(Brightness.dark),
            home: TickerMode(
              enabled: false,
              child: RailScreen(
                conversations: fixtureConversations(DateTime(2026, 8, 16, 14, 16)),
                now: DateTime(2026, 8, 16, 14, 16),
                connection: const ConnectionView.live(),
                myInitials: 'HB',
                onNew: onNew,
              ),
            ),
          );
      await tester.pumpWidget(rail());
      expect(find.byKey(const ValueKey('rail-new')), findsNothing);
      await tester.pumpWidget(rail(onNew: () => tapped++));
      await tester.tap(find.byKey(const ValueKey('rail-new')));
      expect(tapped, 1);
    });
  });
}
