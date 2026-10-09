// CANT-263 (under CANT-254, ruling 3 → B): each client ensures the self
// conversation once after its first completed catch-up. The decision is driven
// with fakes; the store's half runs over a real session with a scripted
// `HttpFetch` and a status the test fires by hand.

import 'dart:io';

import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/ensure_self.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential, projection, status;

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

/// A real transport whose status listeners the test can call by hand, and
/// whose `catchUp` is counted.
final class _Hand implements Transport {
  _Hand(this.inner);

  final Transport inner;
  final listeners = <void Function(TransportStatus)>[];
  var catchUps = 0;

  void emit(TransportStatus s) {
    for (final l in List.of(listeners)) {
      l(s);
    }
  }

  @override
  void catchUp() {
    catchUps++;
  }

  @override
  void Function() subscribe(void Function(TransportStatus s) fn) {
    listeners.add(fn);
    return () => listeners.remove(fn);
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

void main() {
  group('the decision', () {
    test('acts once, on the first completed catch-up, then asks the transport to fetch the row', () async {
      var calls = 0, after = 0;
      final e = EnsureSelfOnce(holdsSelf: () => false, request: () async => ++calls > 0, afterCreated: () => after++);
      e.observe(ready: false, caughtUp: false);
      e.observe(ready: true, caughtUp: false);
      expect(calls, 0, reason: 'not before the catch-up completes');
      e.observe(ready: true, caughtUp: true);
      e.observe(ready: true, caughtUp: true);
      e.observe(ready: false, caughtUp: true);
      e.observe(ready: true, caughtUp: true);
      await pumpEventQueue();
      expect(calls, 1);
      expect(after, 1);
    });

    test('makes no call when the journal already holds one, and never asks again later', () async {
      var held = true, calls = 0;
      final e = EnsureSelfOnce(holdsSelf: () => held, request: () async => ++calls > 0, afterCreated: () {});
      e.observe(ready: true, caughtUp: true);
      held = false;
      e.observe(ready: true, caughtUp: true);
      await pumpEventQueue();
      expect(calls, 0);
    });

    test('a refusal, a thrown error and an offline failure are quiet and are not retried in the session', () async {
      for (final request in <Future<bool> Function()>[() async => false, () async => throw StateError('offline')]) {
        var calls = 0, after = 0;
        final e = EnsureSelfOnce(holdsSelf: () => false, request: () { calls++; return request(); }, afterCreated: () => after++);
        e.observe(ready: true, caughtUp: true);
        await pumpEventQueue();
        e.observe(ready: true, caughtUp: true);
        await pumpEventQueue();
        expect(calls, 1);
        expect(after, 0);
      }
    });
  });

  group('the store', () {
    late Directory dir;
    late _Hand transport;
    late List<HttpExchange> sent;
    late HttpAnswer Function(HttpExchange) selfRoute;

    SessionSeams seams() => SessionSeams(
          directory: dir.path,
          lifecycle: ManualLifecycle(),
          connect: (url, protocols) => _DeadSocket(),
          fetch: (x) async {
            sent.add(x);
            return x.url.path == '/conversations/self' ? selfRoute(x) : const HttpAnswer(503, '');
          },
          transportFactory: (cfg) => transport = _Hand(createTransport(cfg)),
        );

    Future<AppStore> started({bool holdsSelf = false}) async {
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
          conversations: [
            ...p.conversations,
            if (holdsSelf)
              const wire.Conversation(id: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', kind: wire.ConversationKind.self, name: 'Notes', memberCount: 1, headSeq: 0),
          ],
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
      dir = Directory.systemTemp.createTempSync('catenary-ensure-');
      sent = [];
      selfRoute = (_) => const HttpAnswer(
            200,
            '{"id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","kind":"self","name":"Notes","member_count":1,"head_seq":0}',
          );
    });
    tearDown(() => dir.deleteSync(recursive: true));

    test('no self conversation: one POST after the first completed catch-up, then a catch-up to fetch it', () async {
      await started();
      transport.emit(status(ready: true, caughtUp: false));
      await pumpEventQueue();
      expect(posts(), isEmpty, reason: 'not while the catch-up is outstanding');
      transport.emit(status(ready: true, caughtUp: true));
      transport.emit(status(ready: true, caughtUp: true));
      await pumpEventQueue();
      expect(posts().map((x) => x.url.path), ['/conversations/self']);
      expect(transport.catchUps, 1);
    });

    test('one already held: no call at all', () async {
      await started(holdsSelf: true);
      transport.emit(status(ready: true, caughtUp: true));
      await pumpEventQueue();
      expect(posts(), isEmpty);
      expect(transport.catchUps, 0);
    });

    test('a 404 or an offline failure raises no banner and does not retry inside the launch', () async {
      selfRoute = (_) => const HttpAnswer(404, '{"code":"not_found","error":"x"}');
      final store = await started();
      transport.emit(status(ready: true, caughtUp: true));
      await pumpEventQueue();
      transport.emit(status(ready: true, caughtUp: true));
      await pumpEventQueue();
      expect(posts(), hasLength(1));
      expect(transport.catchUps, 0);
      expect(store.startFailure, isNull);
      expect(store.connection.journalError, isNull);
    });
  });
}
