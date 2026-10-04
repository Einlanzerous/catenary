/// The dial, frames before `ready`, the heartbeat, the seam the outbox adapts
/// to, and the status. The twin of web/src/transport/test/session.test.ts,
/// which mirrors internal/client/client_test.go.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';

ClientSend sendFrame(int n) => ClientSend(clientId: uuid(5000 + n), conversationId: conv, text: 'hi $n');

void main() {
  test('the dial: subprotocols, the hello, and a /sync before ready on every dial', () async {
    final r = Rig();
    final first = Completer<SyncResponse>();
    r.sync.answer = (req) => r.sync.requests.length == 1 ? first.future : bootstrapPage(req.after < 7 ? 7 : req.after);
    r.t.start();
    await flush();
    final s = r.net.last;
    expect('${s.url}', 'ws://catenary.test/ws');
    expect(s.protocols, ['catenary.v1', 'catenary.token.$token']);
    expect(r.sync.requests, hasLength(1), reason: 'the /sync went out beside the upgrade, before any ready');
    expect(r.sync.requests.single.authorization, 'Bearer $token');
    expect(r.sync.requests.single.limit, isNull);
    expect(r.t.status().stats.syncsBeforeReady, 1);

    // No cursor held yet: the hello carries none.
    s.open();
    expect(jsonDecode(s.written.single), {
      'type': 'hello',
      'wire_version': wireVersion,
      'device_id': device,
      'client_info': 'catenary-dart/0.0.0-test',
    });
    s.frame(ready());
    first.complete(bootstrapPage(7));
    await flush();
    expect(r.t.status().cursor, 7);

    // The second dial resumes from the cursor, and pulls its own /sync before ready.
    final before = r.t.status().stats.syncsBeforeReady;
    s.serverClose(1001);
    await r.clock.advance(250);
    final s2 = r.net.last;
    expect(s2, isNot(same(s)));
    expect(r.t.status().stats.syncsBeforeReady, before + 1, reason: 'syncsBeforeReady rises per dial');
    s2.open();
    expect(s2.written_<ClientHello>().single.resumeFromLogSeq, 7);
  });

  test('both URLs are built from the one baseUrl, a path prefix included, and the page size is asked for', () async {
    final sync = FakeSync();
    final net = FakeNet();
    final urls = <Uri>[];
    final t = createTransport(TransportConfig(
      baseUrl: 'https://catenary.test:8443/behind/a/proxy/',
      credential: HeldCredential(Credential(userId: me, deviceId: device, accessToken: token)),
      connect: net.connect,
      fetch: (x) {
        urls.add(x.url);
        return sync.fetch(x);
      },
      timers: FakeClock(),
      logger: const SilentLogger(),
      syncLimit: 50,
    ));
    addTearDown(t.stop);
    t.start();
    await flush();
    expect('${net.last.url}', 'wss://catenary.test:8443/behind/a/proxy/ws');
    expect('${urls.single}', 'https://catenary.test:8443/behind/a/proxy/sync?after=0&limit=50');
    expect(() => createTransport(TransportConfig(baseUrl: 'ftp://catenary.test', credential: HeldCredential(Credential(userId: me, deviceId: device, accessToken: token)))), throwsArgumentError);
  });

  test('CANT-103 rule 5 · frames before ready are applied like any live frame and close nothing', () async {
    final r = Rig();
    r.sync.answer = (req) => req.after == 0 ? bootstrapPage(3, [message(1), message(2), message(3)]) : bootstrapPage(req.after);
    r.t.start();
    await flush();
    final s = r.net.last..open();
    await flush();
    expect(r.t.status().cursor, 3);
    // Every frame type, before ready.
    s.frame(messageFrame(message(4)));
    s.frame(ServerConversationFrame(conversation: conversation(uuid(101))));
    s.frame(ServerUserFrame(user: user(uuid(201))));
    s.frame(ServerReceipt(conversationId: conv, userId: other, upToSeq: 2));
    s.frame(ServerTyping(conversationId: conv, userIds: [other]));
    s.frame(const Ping(id: 'server-1'));
    s.frame(const Pong(id: 'nobody-asked'));
    s.frame(const ServerResyncRequired(reason: ResyncReason.cursorTooOld, logSeq: 4));
    s.frame(ServerAck(clientId: uuid(1), messageId: uuid(2), conversationId: conv, seq: 9, logSeq: 9, at: at));
    s.frame(ServerError(code: ErrorCode.rateLimited, message: 'slow', retryable: true, clientId: uuid(1)));
    s.raw(jsonEncode({'type': 'something_new'}));
    await flush();
    expect(r.t.snapshot().messages.any((m) => m.id == message(4).id), isTrue, reason: 'the pre-ready message is held');
    expect(r.t.status().cursor, 3, reason: 'rule 6: and it moved no cursor');
    expect(s.closedByClient, isNull, reason: 'nothing before ready closed the socket');
    expect(r.t.status().connected, isTrue);
    expect(r.t.status().terminal.kind, TerminalKind.none);
    s.frame(ready());
    await flush();
    expect(r.t.status().ready, isTrue);
  });

  test('the heartbeat: from ready, at the announced interval, matched by id, severed at the limit', () async {
    final r = Rig();
    r.sync.answer = (_) => bootstrapPage();
    r.t.start();
    await flush();
    final s = r.net.last..open();
    await r.clock.advance(100000);
    expect(s.written_<Ping>(), isEmpty, reason: 'no ping before ready');

    // An interval that is not the default, so nothing here can be a constant.
    s.frame(ready(heartbeatIntervalSec: 12, missedPongLimit: 3));
    await flush();
    await r.clock.advance(12000);
    var pings = s.written_<Ping>();
    expect(pings, hasLength(1), reason: 'one ping at the first interval');
    s.frame(Pong(id: pings.single.id));
    await flush();
    expect(r.t.status().stats.pongsReceived, 1);

    // Unanswered from here: pings 2, 3 and 4 go out, and the tick after the
    // third outstanding one severs.
    await r.clock.advance(12000 * 3);
    pings = s.written_<Ping>();
    expect(pings, hasLength(4));
    expect({for (final p in pings) p.id}, hasLength(4), reason: 'every id is unique');
    expect(r.t.status().stats.heartbeatSevers, 0);
    await r.clock.advance(12000);
    expect(r.t.status().stats.heartbeatSevers, 1, reason: 'severed once missed_pong_limit pings were outstanding');
    expect(r.t.status().connected, isFalse);
    expect(r.t.status().stats.closeStatuses[-1], 1, reason: 'a sever is a close with no close frame');
    expect(r.t.status().stats.pingsSent, 4);
  });

  test('a server ping is answered with pong{id}', () async {
    final r = Rig();
    r.sync.answer = (_) => bootstrapPage();
    r.t.start();
    final s = await r.connect();
    s.frame(const Ping(id: 'from-the-server'));
    await flush();
    expect([for (final p in s.written_<Pong>()) p.toJson()], [
      {'type': 'pong', 'id': 'from-the-server'},
    ]);
  });

  test('send: its four refusals, before ready included, and client_id never touched', () async {
    final r = Rig();
    r.sync.answer = (_) => bootstrapPage();
    r.t.start();
    await flush();
    await expectLater(r.t.send(sendFrame(1)), throwsA(isA<NotConnected>()), reason: 'no socket yet');

    final s = r.net.last..open();
    // After the socket opens and BEFORE ready: written, and resolves on its ack.
    final p1 = r.t.send(sendFrame(1));
    await expectLater(r.t.send(sendFrame(1)), throwsA(isA<SendInFlight>()), reason: 'the same client_id while it awaits its answer');
    expect([for (final f in s.written_<ClientSend>()) f.toJson()], [sendFrame(1).toJson()], reason: 'the frame as the caller built it');
    final ack = ServerAck(clientId: sendFrame(1).clientId, messageId: uuid(6001), conversationId: conv, seq: 4, logSeq: 40, at: at);
    s.frame(ack);
    expect((await p1).toJson(), ack.toJson());

    s.frame(ready());
    final p2 = r.t.send(sendFrame(2));
    s.frame(ServerError(code: ErrorCode.notAMember, message: 'no', retryable: false, clientId: sendFrame(2).clientId));
    await expectLater(p2, throwsA(isA<SendRefused>().having((e) => e.frame.code, 'code', ErrorCode.notAMember)));

    final p3 = r.t.send(sendFrame(3));
    s.serverClose(1006);
    await expectLater(p3, throwsA(isA<SessionEnded>()), reason: 'the socket closed first: outcome unknown, retry with the same client_id');
  });

  test('read and typing are written as built, and dropped with no socket', () async {
    final r = Rig();
    r.sync.answer = (_) => bootstrapPage();
    r.t.read(ClientRead(conversationId: conv, upToSeq: 3));
    r.t.start();
    final s = await r.connect();
    r.t.read(ClientRead(conversationId: conv, upToSeq: 3));
    r.t.typing(ClientTyping(conversationId: conv, state: TypingState.start));
    expect(s.written_<ClientRead>().single.upToSeq, 3);
    expect(s.written_<ClientTyping>().single.state, TypingState.start);
    final typing = <ServerTyping>[];
    r.t.onTyping(typing.add);
    s.frame(ServerTyping(conversationId: conv, userIds: [other]));
    expect(typing.single.userIds, [other]);
  });

  test('SessionEnd: bare1008 exactly when the 1008 was bare', () async {
    Future<List<SessionEnd>> run(bool preceded) async {
      final r = Rig();
      r.sync.answer = (_) => bootstrapPage();
      final ends = <SessionEnd>[];
      r.t.onSessionEnd(ends.add);
      r.t.start();
      final s = await r.connect();
      if (preceded) s.frame(const ServerError(code: ErrorCode.rateLimited, message: 'slow down', retryable: true));
      s.serverClose(1008);
      await flush();
      return ends;
    }

    final bare = (await run(false)).single;
    expect((bare.opened, bare.readied, bare.closeCode, bare.bare1008, bare.preceding), (true, true, 1008, true, null));
    final framed = (await run(true)).single;
    expect(framed.bare1008, isFalse);
    expect(framed.preceding?.code, ErrorCode.rateLimited);
    expect(framed.verdict, CloseVerdict.reconnect);
  });

  test('ready is announced by subscribe(), and a session end only by onSessionEnd', () async {
    final r = Rig();
    r.sync.answer = (_) => bootstrapPage();
    final readies = <bool>[];
    final ends = <SessionEnd>[];
    r.t.subscribe((s) {
      if (readies.isEmpty || readies.last != s.ready) readies.add(s.ready);
    });
    r.t.onSessionEnd(ends.add);
    r.t.start();
    await flush();
    expect(readies, [false]);
    final s = await r.connect();
    expect(readies, [false, true]);
    expect(ends, isEmpty, reason: 'becoming ready is not a session end');
    s.serverClose(1001);
    await flush();
    expect(readies, [false, true, false]);
    expect(ends, hasLength(1));
    final gen = ends[0].sessionGen;
    await r.clock.advance(250);
    r.net.last.fail();
    await flush();
    expect(ends, hasLength(2));
    expect(ends[1].sessionGen, greaterThan(gen), reason: 'sessionGen rises per dial');
    expect(ends[1].opened, isFalse, reason: 'a pure dial failure');
  });

  test('stats carry Go\'s client.Stats names, camelCased', () {
    final go = File('../internal/client/client.go').readAsStringSync();
    final start = go.indexOf('type Stats struct {');
    final body = go.substring(start, go.indexOf('\n}\n', start));
    final goNames = [
      for (final m in RegExp(r'^\t([A-Z][A-Za-z]*(?:, [A-Z][A-Za-z]*)*)\s+[\w.\[\]]+', multiLine: true).allMatches(body)) ...m.group(1)!.split(', '),
    ];
    expect(goNames.length, greaterThan(20), reason: 'parsed ${goNames.length} fields from client.go');
    const renamed = {'LastRTT': 'lastRttMs'};
    final want = [for (final n in goNames) renamed[n] ?? n[0].toLowerCase() + n.substring(1)]..sort();
    expect(Stats().toJson().keys.toList()..sort(), want);
  });

  test('the status serializes under the reference\'s field names', () {
    final ts = File('../web/src/transport/status.ts').readAsStringSync();
    final start = ts.indexOf('export interface TransportStatus {');
    final body = ts.substring(start, ts.indexOf('\n}\n', start));
    final want = [for (final m in RegExp(r'^  ([a-zA-Z]+): ', multiLine: true).allMatches(body)) m.group(1)!]..sort();
    expect(want.length, greaterThan(15));
    final r = Rig();
    final json = r.t.status().toJson();
    expect(json.keys.toList()..sort(), want);
    expect(jsonDecode(jsonEncode(json)), isA<Map<String, dynamic>>(), reason: 'and is encodable as it stands');
  });

  test('no token appears in any status field or log line', () async {
    final r = Rig();
    var n = 0;
    r.sync.answer = (req) {
      n++;
      if (n == 2) return const HttpAnswer(401, '{"code":"unauthorized"}');
      if (n == 3) return const HttpAnswer(502, 'bad gateway');
      return bootstrapPage(req.after);
    };
    final statuses = <TransportStatus>[];
    r.t.subscribe(statuses.add);
    r.t.start();
    final s = await r.connect();
    s.frame(const ServerError(code: ErrorCode.internal, message: 'boom', retryable: true));
    s.raw('not json');
    s.raw(Uint8List(2));
    s.serverClose(1008);
    await r.clock.advance(6000);
    r.net.last.fail();
    await r.clock.advance(6000);
    final s3 = await r.connect();
    s3.serverClose(4001);
    await flush();
    expect(r.logger.lines.length, greaterThan(3), reason: 'something was logged');
    final everything = jsonEncode([for (final st in statuses) st.toJson()]) +
        jsonEncode([for (final l in r.logger.lines) [l.level, l.msg, l.fields]]) +
        jsonEncode(r.t.status().toJson());
    expect(everything, isNot(contains(token)), reason: 'the access token appears nowhere');
    expect(everything, isNot(contains(token.substring(0, 20))), reason: 'nor any prefix of it');
  });

  test('closeStatuses keys an abnormal closure under -1, and a dial failure not at all', () async {
    final r = Rig();
    r.sync.answer = (_) => bootstrapPage();
    r.t.start();
    final s = await r.connect();
    s.serverClose(1006);
    await r.clock.advance(250);
    r.net.last.fail();
    await r.clock.advance(1000);
    final s3 = await r.connect();
    s3.serverClose(1001);
    await flush();
    final st = r.t.status();
    expect(st.stats.closeStatuses, {-1: 1, 1001: 1});
    expect(st.stats.toJson()['closeStatuses'], {'-1': 1, '1001': 1});
    expect(st.stats.dialErrors, 1);
  });

  test('a close this client made on stop() is not counted as one the peer sent', () async {
    final r = Rig();
    r.sync.answer = (_) => bootstrapPage();
    final ends = <SessionEnd>[];
    r.t.onSessionEnd(ends.add);
    r.t.start();
    await r.connect();
    r.t.stop();
    await flush();
    expect(r.t.status().stats.closeStatuses, isEmpty);
    expect(ends, hasLength(1), reason: 'the outbox still learns the session ended');
    expect(ends.single.closeCode, isNull);
    expect(ends.single.bare1008, isFalse);
    expect(r.net.last.closedByClient?.code, 1000, reason: 'a clean close on the wire');
  });

  test('a /sync that never answers is abandoned at the timeout, and at a stop', () async {
    final r = Rig();
    final aborted = <int>[];
    var n = 0;
    final t = createTransport(TransportConfig(
      baseUrl: 'http://catenary.test',
      credential: HeldCredential(r.credential),
      connect: r.net.connect,
      fetch: (x) {
        final k = ++n;
        unawaited(x.abort.then((_) => aborted.add(k)));
        return Completer<HttpAnswer>().future;
      },
      now: r.clock.now,
      timers: r.clock,
      random: minDraw,
      logger: r.logger,
    ));
    addTearDown(t.stop);
    t.start();
    await r.connect();
    expect(n, 1);
    await r.clock.advance(29999);
    expect(aborted, isEmpty);
    await r.clock.advance(1);
    expect(aborted, [1], reason: 'the request was told to stop at the timeout');
    expect(t.status().stats.syncErrors, 1);
    // With a ready session the catch-up retries on its backoff; a stop aborts
    // that request too.
    await r.clock.advance(250);
    expect(n, 2);
    t.stop();
    await flush();
    expect(aborted, [1, 2]);
  });
}
