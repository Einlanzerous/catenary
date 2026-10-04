/// The credential layer over its real store and its real lock: the row in
/// `catenary.db` (CANT-42 ruling 3), single-flight refresh across two contexts
/// (CANT-31 §2, ruling 2), and the hooks the transport calls. The pure
/// decisions and the chain are the shared decision vectors'
/// (bin/decisions.dart); this is what they do not reach.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:sqlite3/sqlite3.dart';
import 'package:test/test.dart';

import 'harness.dart';

const firstRefresh = 'refresh_token_FIXTURE_first_enrollment_____';
const minute = 60000;

/// A 43-character wire `Token`, distinct per `n`.
String accessTokenNo(int n) => 'access_token_FIXTURE_rotation_${'$n'.padLeft(13, '0')}';

String wireTime(num ms) => DateTime.fromMillisecondsSinceEpoch(ms.floor(), isUtc: true).toIso8601String();

/// The pair an enrollment at `now` minted: fifteen minutes of access, served
/// by a server whose clock agrees with the device's.
StoredCredential enrolled(num now, {String accessToken = token, String? deviceId}) => StoredCredential(
      userId: me,
      deviceId: deviceId ?? device,
      accessToken: accessToken,
      accessExpiresAt: now + 15 * minute,
      refreshToken: firstRefresh,
      refreshExpiresAt: now + 30 * 24 * 60 * minute,
      accessIssuedAt: now,
    );

/// A `/refresh` endpoint that rotates as the server does: the answer's refresh
/// token is the proposal, and a token already rotated away is refused. `gate`
/// holds every answer open until it completes.
final class FakeRefresh {
  FakeRefresh(this.clock);

  final FakeClock clock;
  final requests = <({String token, String proposal})>[];
  var current = firstRefresh;
  var rotations = 0;
  Future<void>? gate;

  /// Replaces the default answer for one request; null answers as a server.
  FutureOr<HttpAnswer> Function()? override;

  Future<HttpAnswer> fetch(HttpExchange x) async {
    expect(x.method, 'POST');
    expect('${x.url}', '${Rig.baseUrl}/refresh');
    final req = RefreshRequest.fromJson(jsonDecode(x.body!));
    requests.add((token: req.refreshToken, proposal: req.proposedRefreshToken!));
    await gate;
    final o = override;
    if (o != null) return o();
    if (req.refreshToken != current) return const HttpAnswer(401, '{"code":"unauthorized"}');
    current = req.proposedRefreshToken!;
    final now = clock.now();
    return HttpAnswer(
      200,
      jsonEncode(RefreshResponse(
        accessToken: accessTokenNo(++rotations),
        accessExpiresAt: wireTime(now + 15 * minute),
        refreshToken: current,
        refreshExpiresAt: wireTime(now + 30 * 24 * 60 * minute),
      ).toJson()),
      {'date': HttpDate.format(DateTime.fromMillisecondsSinceEpoch(now, isUtc: true))},
    );
  }
}

/// One context: its own connection to the credential, its own lock
/// connections, its own layer — over the directory every context shares.
final class Context {
  Context(String dir, this.refresh, FakeClock clock, {Faults faults = Faults.none, ListLogger? logger})
      : store = SqliteCredentialStore.open(catenaryDbPath(dir)),
        locks = SqliteLocks(dir, timers: clock) {
    addTearDown(store.close);
    addTearDown(locks.close);
    layer = RefreshingCredential(
      baseUrl: Rig.baseUrl,
      store: store,
      lock: locks.call,
      fetch: refresh.fetch,
      now: clock.now,
      timers: clock,
      logger: logger ?? ListLogger(),
      faults: faults,
    );
  }

  final SqliteCredentialStore store;
  final SqliteLocks locks;
  final FakeRefresh refresh;
  late final RefreshingCredential layer;
}

/// What is in the file, read through a connection of its own.
StoredCredential? storedIn(String dir) {
  final db = sqlite3.open(catenaryDbPath(dir));
  try {
    final rows = db.select('SELECT record FROM credential');
    return rows.isEmpty ? null : StoredCredential.fromJson(jsonDecode(rows.single['record'] as String) as Map<String, dynamic>);
  } finally {
    db.close();
  }
}

/// Two contexts that find a refresh due at the same moment.
Future<({int posts, List<String> presented, int skipped, int refreshed, bool committedFirst})> twoContexts(Faults faults) async {
  final dir = tempDir();
  final clock = FakeClock();
  final refresh = FakeRefresh(clock);
  final a = Context(dir, refresh, clock, faults: faults);
  final b = Context(dir, refresh, clock, faults: faults);
  await enrollCredential(a.store, a.locks.call, enrolled(clock.now()));
  clock.jump(11 * minute); // inside the last third of fifteen minutes

  // The answer is held open, so both contexts have decided before either has
  // an answer.
  final open = Completer<void>();
  refresh.gate = open.future;
  final both = [a.layer.attemptIfDue(), b.layer.attemptIfDue()];
  await clock.advance(100);
  open.complete();
  await clock.advance(100);
  final outcomes = await Future.wait(both);

  // THE ROTATED PAIR IS COMMITTED BEFORE IT IS PRESENTED ANYWHERE: whatever a
  // context presents now is what an independent connection reads from the file.
  final onDisk = storedIn(dir)!.accessToken;
  final presented = [(await a.layer.current()).accessToken, (await b.layer.current()).accessToken];
  return (
    posts: refresh.requests.length,
    presented: presented,
    skipped: outcomes.where((o) => o == RefreshOutcome.skipped).length,
    refreshed: outcomes.where((o) => o == RefreshOutcome.refreshed).length,
    committedFirst: presented.every((t) => t == onDisk),
  );
}

void main() {
  group('the credential at rest', () {
    test('it is a row in catenary.db behind CredentialStore, and on Linux the file is created 0600', () async {
      final dir = tempDir();
      final path = catenaryDbPath(dir);
      final CredentialStore store = SqliteCredentialStore.open(path);
      addTearDown((store as SqliteCredentialStore).close);
      expect(await store.read(), isNull);
      final cred = enrolled(epoch).withChain(const [ChainLink(firstRefresh, 'a-proposal')], epoch + 5);
      await store.update<void>((held) => (write: cred, result: null));

      expect(File(path).existsSync(), isTrue);
      expect(Directory(dir).listSync().map((f) => f.uri.pathSegments.last), ['catenary.db'], reason: 'the credential has no file of its own');
      expect(jsonEncode(storedIn(dir)), jsonEncode(cred), reason: 'an independent connection reads the whole record back');
      expect(jsonEncode(await store.read()), jsonEncode(cred));
      if (Platform.isLinux) {
        expect((File(path).statSync().mode & 0x1ff).toRadixString(8), '600', reason: 'readable and writable by its owner, and by nobody else');
      }
    }, testOn: 'vm');

    test('a file that already exists keeps the mode it has', () {
      final dir = tempDir();
      final path = catenaryDbPath(dir);
      File(path).createSync();
      final before = File(path).statSync().mode & 0x1ff;
      SqliteCredentialStore.open(path).close();
      expect(File(path).statSync().mode & 0x1ff, before);
    });

    test('a write that fails lands nothing, and an update reads what another context wrote', () async {
      final dir = tempDir();
      final a = SqliteCredentialStore.open(catenaryDbPath(dir));
      final b = SqliteCredentialStore.open(catenaryDbPath(dir));
      addTearDown(a.close);
      addTearDown(b.close);
      await a.update<void>((held) => (write: enrolled(epoch), result: null));
      await expectLater(b.update<void>((held) => throw StateError('decided against it')), throwsStateError);
      expect((await b.read())!.accessToken, token);
      final seen = await b.update<String?>((held) => (write: enrolled(epoch, accessToken: accessTokenNo(1)), result: held?.accessToken));
      expect(seen, token, reason: 'the function ran against what A stored');
      expect((await a.read())!.accessToken, accessTokenNo(1), reason: 'and A reads what B stored');
    });

    test('enroll seeds it once; re-enroll replaces the device\'s row, chain and stamp and all', () async {
      final dir = tempDir();
      final store = SqliteCredentialStore.open(catenaryDbPath(dir));
      final locks = SqliteLocks(dir);
      addTearDown(store.close);
      addTearDown(locks.close);
      await expectLater(reenrollCredential(store, locks.call, enrolled(epoch)), throwsA(isA<NoCredential>()));
      await enrollCredential(store, locks.call, enrolled(epoch).withChain(const [ChainLink('t', 'p')], epoch));
      expect((await store.read())!.chain, isEmpty, reason: 'a first enrollment has sent nothing');
      await expectLater(enrollCredential(store, locks.call, enrolled(epoch, accessToken: accessTokenNo(9))), throwsA(isA<CredentialHeld>()));
      expect((await store.read())!.accessToken, token, reason: 'the held pair was not overwritten');

      await reenrollCredential(store, locks.call, enrolled(epoch, accessToken: accessTokenNo(2), deviceId: uuid(4)));
      final held = storedIn(dir)!;
      expect((held.deviceId, held.accessToken, held.chain.length, held.lastSentAt), (uuid(4), accessTokenNo(2), 0, null));
      await expectLater(enrollCredential(store, locks.call, StoredCredential(userId: me, deviceId: '', accessToken: 'a', accessExpiresAt: 0, refreshToken: 'r', refreshExpiresAt: 0)), throwsArgumentError);
    });

    test('a journal wipe leaves the credential the store wrote exactly as it was', () async {
      final dir = tempDir();
      final store = SqliteCredentialStore.open(catenaryDbPath(dir));
      final journal = SqliteJournal.open(catenaryDbPath(dir));
      addTearDown(store.close);
      addTearDown(journal.close);
      await store.update<void>((held) => (write: enrolled(epoch), result: null));
      await journal.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), Faults.none);
      final before = jsonEncode(storedIn(dir));
      await journal.wipe();
      expect(jsonEncode(storedIn(dir)), before);
      expect(journal.messageCount, 0);
    });

    test('credentialFromEnroll reads the Date for the offset and the issue time, and does without one', () {
      final e = EnrollResponse.fromJson({
        'user_id': me,
        'device_id': device,
        'access_token': token.substring(0, 43),
        'access_expires_at': '2026-09-29T12:15:00.000Z',
        'refresh_token': firstRefresh,
        'refresh_expires_at': '2026-10-29T12:00:00.000Z',
      });
      // The server's clock reads noon; the device's is an hour fast.
      final served = credentialFromEnroll(e, 'Tue, 29 Sep 2026 12:00:00 GMT', epoch + 60 * minute);
      expect((served.accessIssuedAt, served.clockOffsetMs, served.accessExpiresAt), (epoch, -60 * minute, epoch + 15 * minute));
      final undated = credentialFromEnroll(e, null, epoch);
      expect((undated.accessIssuedAt, undated.clockOffsetMs), (null, 0));
      expect(credentialFromEnroll(e, 'not a date', epoch).accessIssuedAt, isNull);
    });
  });

  group('single-flight', () {
    test('two contexts that find a refresh due at the same moment issue exactly one POST /refresh, and both present the rotated pair', () async {
      final r = await twoContexts(Faults.none);
      expect(r.posts, 1, reason: 'one refresh between them');
      expect((r.refreshed, r.skipped), (1, 1), reason: 'the second waited, re-read the persisted credential under the lock, and skipped');
      expect(r.presented, [accessTokenNo(1), accessTokenNo(1)], reason: 'and presents the pair the first rotated to');
      expect(r.committedFirst, isTrue, reason: 'what is presented is what is on disk');
    });

    test('negative control refreshUnlocked: two refreshes of one pair, and the second is a replay', () async {
      final r = await twoContexts(const Faults(refreshUnlocked: true));
      expect(r.posts, greaterThan(1));
    });

    test('the rotated pair is on disk before the request that presents it leaves', () async {
      final dir = tempDir();
      final clock = FakeClock();
      final refresh = FakeRefresh(clock);
      late final Context ctx;
      final r = Rig(clock: clock, seam: (r) => (ctx = Context(dir, refresh, clock, logger: r.logger)).layer);
      await enrollCredential(ctx.store, ctx.locks.call, enrolled(clock.now()));
      clock.jump(11 * minute);
      // Every dial and every /sync: what it presents, and what the file held
      // at that moment.
      final seen = <({String presented, String onDisk})>[];
      r.sync.answer = (req) {
        seen.add((presented: req.authorization!.substring('Bearer '.length), onDisk: storedIn(dir)!.accessToken));
        return bootstrapPage(req.after);
      };
      r.net.onDial = (s) => seen.add((presented: s.protocols.last.substring('catenary.token.'.length), onDisk: storedIn(dir)!.accessToken));
      r.t.start();
      await r.connect();
      expect(refresh.requests, hasLength(1), reason: 'the proactive refresh, before the dial');
      expect(seen, isNotEmpty);
      expect({for (final s in seen) s.presented}, {accessTokenNo(1)}, reason: 'nothing presented the expiring token');
      expect(seen.every((s) => s.presented == s.onDisk), isTrue);
      expect(r.t.status().stats.refreshes, 1);
      expect(r.t.status().stats.chainLength, 0, reason: 'an answer collapses the chain');
    });
  });

  group('the hooks the transport calls', () {
    /// A transport over a refreshing credential enrolled at the clock's now.
    Future<({Rig r, Context ctx, FakeRefresh refresh, String dir})> rigged({Faults faults = Faults.none}) async {
      final dir = tempDir();
      final clock = FakeClock();
      final refresh = FakeRefresh(clock);
      late final Context ctx;
      final r = Rig(clock: clock, faults: faults, seam: (r) => (ctx = Context(dir, refresh, clock, faults: faults, logger: r.logger)).layer);
      await enrollCredential(ctx.store, ctx.locks.call, enrolled(clock.now()));
      return (r: r, ctx: ctx, refresh: refresh, dir: dir);
    }

    test('a pair that is not due is presented as it is, and nothing is refreshed', () async {
      final x = await rigged();
      x.r.sync.answer = (req) => bootstrapPage(req.after);
      x.r.t.start();
      await x.r.connect();
      expect(x.refresh.requests, isEmpty);
      expect(x.r.net.last.protocols.last, 'catenary.token.$token');
      expect(x.r.t.status().refreshHold, RefreshHold.none);
    });

    test('Catenary\'s own 401 on /sync: one refresh, then ONE retry with the rotated pair', () async {
      final x = await rigged();
      final bearers = <String?>[];
      x.r.sync.answer = (req) {
        bearers.add(req.authorization);
        return req.authorization == 'Bearer $token' ? const HttpAnswer(401, '{"code":"unauthorized"}') : bootstrapPage(3, [message(1), message(2), message(3)]);
      };
      x.r.t.start();
      await flush();
      expect(bearers, ['Bearer $token', 'Bearer ${accessTokenNo(1)}'], reason: 'the refused request, and its one retry');
      expect(x.refresh.requests, hasLength(1));
      expect(x.r.t.status().cursor, 3);
      expect(x.r.t.status().tokenRefused, isFalse, reason: 'the pair changed: the refused token is behind us');
      expect(storedIn(x.dir)!.accessToken, accessTokenNo(1));
    });

    test('a hop\'s 401 is not Catenary answering: nothing is refreshed and nothing is marked', () async {
      final x = await rigged();
      x.r.sync.answer = (_) => const HttpAnswer(401, 'access: session expired\n');
      x.r.t.start();
      await flush();
      expect(x.refresh.requests, isEmpty);
      expect(x.r.t.status().tokenRefused, isFalse);
      expect(x.r.t.status().stats.syncErrors, 1);
    });

    /// Catenary refuses the access token on /sync, and the refresh that would
    /// cure it gets no response: the token stays refused.
    Future<({int dialsAfter, int withheld, bool refused, RefreshHold hold, int chain})> refusedAndUncured(Faults faults) async {
      final x = await rigged(faults: faults);
      x.r.sync.answer = (_) => const HttpAnswer(401, '{"code":"unauthorized"}');
      x.refresh.override = () => throw const NetworkDown();
      x.r.net.onDial = (s) => s.fail();
      x.r.t.start();
      await flush();
      final dials = x.r.t.status().stats.dials;
      await x.r.clock.advance(60000);
      final st = x.r.t.status();
      return (dialsAfter: st.stats.dials - dials, withheld: st.stats.dialsWithheld, refused: st.tokenRefused, hold: st.refreshHold, chain: st.stats.chainLength);
    }

    test('a refused token is not dialed with: the dial is withheld and counted until the pair changes', () async {
      final r = await refusedAndUncured(Faults.none);
      expect(r.refused, isTrue);
      expect(r.dialsAfter, 0, reason: 'no dial in a minute with the refused token');
      expect(r.withheld, greaterThan(5), reason: 'polled at the dial cadence, and counted');
      expect(r.chain, greaterThan(0), reason: 'the unanswered refresh left its link');
    });

    test('negative control presentRefusedToken: it dials on regardless', () async {
      final r = await refusedAndUncured(const Faults(presentRefusedToken: true));
      expect(r.dialsAfter, greaterThan(5));
      expect(r.withheld, 0);
    });

    test('Catenary\'s own 401 to the stored refresh token is the credential terminal, and deletes nothing', () async {
      final x = await rigged();
      x.r.clock.jump(11 * minute);
      x.refresh.override = () => const HttpAnswer(401, '{"code":"unauthorized"}');
      x.r.sync.answer = (req) => bootstrapPage(req.after);
      x.r.t.start();
      await flush();
      final st = x.r.t.status();
      expect(st.terminal.kind, TerminalKind.credential);
      expect(st.terminal.reason, isNot(contains(firstRefresh)));
      expect(st.stats.dials, 0, reason: 'terminal before the dial it was about to make');
      expect(storedIn(x.dir)!.refreshToken, firstRefresh, reason: 'the credential is still there');
      await x.r.clock.advance(10 * minute);
      expect(x.r.t.status().stats.dials, 0, reason: 'and no timer ends it');

      final never = await rigged(faults: const Faults(neverTerminal: true));
      never.r.clock.jump(11 * minute);
      never.refresh.override = () => const HttpAnswer(401, '{"code":"unauthorized"}');
      never.r.sync.answer = (req) => bootstrapPage(req.after);
      never.r.t.start();
      await flush();
      expect(never.r.t.status().terminal.kind, TerminalKind.none, reason: 'the control: neverTerminal dials on');
    });

    test('an unanswered refresh is held, not retried: unreachable until Catenary answers, then the backoff', () async {
      final x = await rigged();
      x.r.clock.jump(11 * minute);
      x.refresh.override = () => throw const NetworkDown();
      expect(await x.ctx.layer.refreshDueWhenAllowed(), RefreshOutcome.failed);
      expect(x.refresh.requests, hasLength(1));
      expect(storedIn(x.dir)!.chain, hasLength(1), reason: 'the link was persisted before the request left');
      expect(storedIn(x.dir)!.lastSentAt, x.r.clock.now());

      // Nothing has answered this context since the send: the gate is closed.
      expect(await x.ctx.layer.refreshDueWhenAllowed(), RefreshOutcome.held);
      expect(x.ctx.layer.status().refreshHold, RefreshHold.unreachable);
      expect(x.ctx.layer.status().refreshesHeldUnreachable, 1);
      expect(x.ctx.layer.status().refreshErrors, 1, reason: 'a refresh nobody made did not fail');
      expect(x.refresh.requests, hasLength(1));

      // Catenary answers; the gate opens, and the delay of one link holds.
      x.r.clock.jump(1000);
      x.ctx.layer.answered();
      expect(await x.ctx.layer.refreshDueWhenAllowed(), RefreshOutcome.held);
      expect(x.ctx.layer.status().refreshHold, RefreshHold.backoff);
      expect(x.ctx.layer.status().nextRefreshAt, storedIn(x.dir)!.lastSentAt! + 5000);

      // Past it, the walk starts from the chain's newest and steps back.
      x.r.clock.jump(4000);
      x.refresh.override = null;
      expect(await x.ctx.layer.refreshDueWhenAllowed(), RefreshOutcome.refreshed);
      expect(x.refresh.requests.map((q) => q.token), [firstRefresh, x.refresh.requests[0].proposal, firstRefresh]);
      expect(x.refresh.requests[2].proposal, x.refresh.requests[0].proposal, reason: 'the older token goes back with the proposal it was first sent with');
      expect(x.ctx.layer.status().refreshWalkBacks, 1);
      expect(storedIn(x.dir)!.chain, isEmpty);

      // The explicit check is never held, whatever the suppressors say.
      x.r.clock.jump(11 * minute);
      x.refresh.override = () => throw const NetworkDown();
      expect(await x.ctx.layer.attemptIfDue(), RefreshOutcome.failed);
      expect(await x.ctx.layer.attemptIfDue(), RefreshOutcome.failed, reason: 'asked again at once, and it went');
      expect(storedIn(x.dir)!.chain, hasLength(2));
    });

    test('after a kill nothing in flight reaches the store', () async {
      final x = await rigged();
      x.r.clock.jump(11 * minute);
      final open = Completer<void>();
      x.refresh.gate = open.future;
      final refreshing = x.ctx.layer.attemptIfDue();
      await flush();
      final mid = jsonEncode(storedIn(x.dir));
      x.ctx.layer.kill();
      open.complete();
      expect(await refreshing, RefreshOutcome.killed);
      expect(jsonEncode(storedIn(x.dir)), mid, reason: 'the answer that arrived after the kill was not written');
      expect(storedIn(x.dir)!.chain, hasLength(1), reason: 'and the link it sent with is what a relaunch walks');
    });

    test('no token reaches a log line or a status field', () async {
      final x = await rigged();
      x.r.clock.jump(11 * minute);
      x.r.sync.answer = (req) => req.authorization == 'Bearer ${accessTokenNo(1)}' ? const HttpAnswer(401, '{"code":"unauthorized"}') : bootstrapPage(req.after);
      final statuses = <TransportStatus>[];
      x.r.t.subscribe(statuses.add);
      x.r.t.start();
      await x.r.connect();
      await x.r.clock.advance(60000);
      expect(x.r.logger.lines, isNotEmpty);
      final everything = jsonEncode([for (final st in statuses) st.toJson()]) + jsonEncode([for (final l in x.r.logger.lines) [l.msg, l.fields]]);
      for (final secret in [token, firstRefresh, accessTokenNo(1), accessTokenNo(2), ...x.refresh.requests.map((q) => q.proposal)]) {
        expect(everything, isNot(contains(secret)));
        expect(everything, isNot(contains(secret.substring(0, 20))));
      }
    });
  });
}
