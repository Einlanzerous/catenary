/// The dial backoff under CANT-35 ruling 3 → B, the wake-signal early dial,
/// and the lifecycle policy of ruling 5 → B. The twin of
/// web/src/transport/test/backoff.test.ts.
library;

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';

const tenMinutes = 10 * 60000;

const wakeEvents = [LifecycleEvent.online, LifecycleEvent.visible, LifecycleEvent.pageshow, LifecycleEvent.resume];

/// A 10-minute outage from the floor in which every dial fails.
Future<({int dials, int syncs, List<num> waits, int withheld})> outage(RandomBytes random, int? wakeEvery) async {
  final r = Rig(random: random);
  r.sync.answer = (req) => r.net.sockets.length == 1 ? bootstrapPage(req.after) : const NetworkDown();
  final waits = <num>[];
  num? seen;
  r.t.subscribe((s) {
    final at = s.nextDialAt;
    if (at != null && at != seen) waits.add(at - r.clock.now());
    seen = at;
  });
  r.t.start();
  if (wakeEvery != null) {
    // A ready that announced 35 s, then the outage.
    final s = await r.connect(ready(heartbeatIntervalSec: 35));
    r.net.onDial = (x) => x.fail();
    s.serverClose(1006);
  } else {
    r.net.onDial = (x) => x.fail();
  }
  await flush();
  final dialsAtStart = r.t.status().stats.dials;
  final syncsAtStart = r.sync.requests.length;
  var elapsed = 0;
  while (elapsed < tenMinutes) {
    final step = wakeEvery ?? tenMinutes;
    await r.clock.advance(step < tenMinutes - elapsed ? step : tenMinutes - elapsed);
    elapsed += step;
    if (wakeEvery != null && elapsed < tenMinutes) r.lifecycle.emit(LifecycleEvent.online);
  }
  await flush();
  final st = r.t.status();
  return (
    // Dials INCLUDE the first dial of the outage.
    dials: st.stats.dials - dialsAtStart + (wakeEvery == null ? 0 : 1),
    syncs: r.sync.requests.length - syncsAtStart,
    waits: waits,
    withheld: st.stats.dialsWithheld,
  );
}

void main() {
  test('the pure pieces: the jitter band and the stability rule', () {
    expect(unitFromBytes(minDraw(4)), 0);
    expect(unitFromBytes(maxDraw(4)), 1);
    expect(jitteredWait(5000, 0), 4000, reason: 'the minimum draw is 0.8d');
    expect(jitteredWait(5000, 1), 5000, reason: 'the maximum is d: never above the ceiling');
    expect(resetsRamp(null, 35), isFalse, reason: 'never ready: no reset');
    expect(resetsRamp(34999, 35), isFalse, reason: 'ready for less than an interval: no reset');
    expect(resetsRamp(35000, 35), isTrue);
  });

  test('a 10-minute outage stays within 156 dials at the minimum draw, one /sync per dial at most', () async {
    final o = await outage(minDraw, null);
    printOnFailure('minimum draw: ${o.dials} dials, ${o.syncs} /sync requests in ten minutes');
    expect(o.dials, lessThanOrEqualTo(156));
    expect(o.dials, greaterThanOrEqualTo(150), reason: 'the bound is tight, not vacuous');
    expect(o.syncs, lessThanOrEqualTo(o.dials));
    // The ramp: 250, 500, 1000, 2000, 4000, then the 5 s ceiling, each drawn at 0.8×.
    expect(o.waits.take(7), [200, 400, 800, 1600, 3200, 4000, 4000]);
  });

  test('and within it at the maximum draw', () async {
    final o = await outage(maxDraw, null);
    expect(o.dials, lessThanOrEqualTo(156));
    expect(o.syncs, lessThanOrEqualTo(o.dials));
    expect(o.waits.take(6), [250, 500, 1000, 2000, 4000, 5000]);
  });

  test('with a wake signal every 10 s, within 174, and no early dial resets the ramp', () async {
    final o = await outage(minDraw, 10000);
    expect(o.dials, lessThanOrEqualTo(174));
    expect(o.withheld, greaterThan(0), reason: 'wake signals inside the interval were withheld and counted');
    // Once the ramp reaches the ceiling it stays there: an early dial never put
    // it back to the floor.
    final afterRamp = o.waits.skip(6).toList();
    expect(afterRamp.length, greaterThan(100));
    expect(afterRamp.every((w) => w >= 4000), isTrue);
  });

  test('a wake signal during an error{internal} maximum wait does not shorten it', () async {
    final r = Rig();
    r.sync.answer = (req) => bootstrapPage(req.after);
    r.t.start();
    final s = await r.connect();
    s.frame(const ServerError(code: ErrorCode.internal, message: 'boom', retryable: false));
    s.serverClose(1008);
    await flush();
    final dials = r.t.status().stats.dials;
    final wait = r.t.status().nextDialAt! - r.clock.now();
    expect(wait, greaterThanOrEqualTo(4000));
    wakeEvents.forEach(r.lifecycle.emit);
    await r.clock.advance(wait - 1);
    expect(r.t.status().stats.dials, dials, reason: 'nothing dialed before the maximum wait ran out');
    await r.clock.advance(1);
    expect(r.t.status().stats.dials, dials + 1);
  });

  test('a wake signal before any ready makes no early dial', () async {
    final r = Rig();
    r.net.onDial = (s) => s.fail();
    r.sync.answer = (_) => const NetworkDown();
    r.t.start();
    await r.clock.advance(20000);
    final dials = r.t.status().stats.dials;
    r.lifecycle.emit(LifecycleEvent.online);
    r.lifecycle.emit(LifecycleEvent.resume);
    await flush();
    expect(r.t.status().stats.dials, dials);
  });

  test('a path that answers ready and drops at once stays within the outage bound', () async {
    final r = Rig();
    r.sync.answer = (req) => page(req.after);
    r.net.onDial = (s) {
      s.open();
      s.frame(ready());
      s.serverClose(1006);
    };
    r.t.start();
    await r.clock.advance(tenMinutes);
    final dials = r.t.status().stats.dials;
    expect(dials, lessThanOrEqualTo(156), reason: 'reset-on-any-ready would make ~2400');
    expect(r.t.status().stats.readys, dials);
  });

  test('ruling 3 → B: a session ready for a whole interval resets the ramp, a shorter one does not', () async {
    final r = Rig();
    r.sync.answer = (req) => bootstrapPage(req.after);
    r.t.start();
    // Grow the ramp: five failed dials.
    r.net.onDial = (s) => s.fail();
    await r.clock.advance(1);
    await r.clock.advance(6500);
    final grown = r.t.status().nextDialAt! - r.clock.now();
    r.net.onDial = (_) {};
    await r.clock.advance(grown);
    // Ready, and dropped well inside one interval: the ramp carries on.
    var s = await r.connect(ready(heartbeatIntervalSec: 35));
    await r.clock.advance(10000);
    s.serverClose(1006);
    await flush();
    final afterShort = r.t.status().nextDialAt! - r.clock.now();
    expect(afterShort, greaterThanOrEqualTo(3200), reason: 'a short session left the ramp where it was');
    await r.clock.advance(afterShort);
    // Ready for longer than one interval: the ramp resets to its floor.
    s = await r.connect(ready(heartbeatIntervalSec: 35));
    await r.clock.advance(36000);
    s.serverClose(1006);
    await flush();
    final afterStable = r.t.status().nextDialAt! - r.clock.now();
    expect(afterStable, lessThanOrEqualTo(250), reason: 'a stable session reset the ramp');
  });

  test('retryNow() resets the ramp and dials now', () async {
    final r = Rig();
    r.net.onDial = (s) => s.fail();
    r.sync.answer = (_) => const NetworkDown();
    r.t.start();
    await r.clock.advance(30000);
    final dials = r.t.status().stats.dials;
    expect(r.t.status().nextDialAt! - r.clock.now(), greaterThanOrEqualTo(1));
    r.t.retryNow();
    await flush();
    expect(r.t.status().stats.dials, dials + 1, reason: 'dialed at once');
    expect(r.t.status().nextDialAt! - r.clock.now(), lessThanOrEqualTo(250), reason: 'from the floor');
    expect(r.t.status().attempt, 1);
  });

  test('the rig\'s floor and ceiling replace the defaults', () async {
    final r = Rig(backoffMinMs: 100, backoffMaxMs: 2000, random: maxDraw);
    r.net.onDial = (s) => s.fail();
    r.sync.answer = (_) => const NetworkDown();
    final waits = <num>[];
    num? seen;
    r.t.subscribe((s) {
      final at = s.nextDialAt;
      if (at != null && at != seen) waits.add(at - r.clock.now());
      seen = at;
    });
    r.t.start();
    await r.clock.advance(20000);
    expect(waits.take(7), [100, 200, 400, 800, 1600, 2000, 2000]);
  });

  // --- the lifecycle -----------------------------------------------------------

  test('each wake signal pings at once when a socket is open', () async {
    for (final e in <LifecycleEvent?>[...wakeEvents, null]) {
      final r = Rig();
      r.sync.answer = (req) => bootstrapPage(req.after);
      r.t.start();
      final s = await r.connect(ready(heartbeatIntervalSec: 35, missedPongLimit: 2));
      expect(s.written_<Ping>(), isEmpty);
      if (e == null) {
        // The device slept for longer than the server's backstop window.
        r.clock.jump(35000 * 3 + 1000);
        await r.clock.advance(0);
      } else {
        r.lifecycle.emit(e);
        await flush();
      }
      final name = e?.name ?? 'the clock jump';
      expect(s.written_<Ping>(), hasLength(1), reason: '$name: one immediate ping');
      expect(r.t.status().stats.dials, 1, reason: '$name: and no dial');
    }
  });

  test('each wake signal makes one early dial when no socket is open, rate-limited per interval', () async {
    for (final e in wakeEvents) {
      final r = Rig();
      r.sync.answer = (req) => bootstrapPage(req.after);
      r.t.start();
      final s = await r.connect(ready(heartbeatIntervalSec: 35));
      r.net.onDial = (x) => x.fail();
      s.serverClose(1006);
      await r.clock.advance(20000); // the ramp is at its ceiling
      final dials = r.t.status().stats.dials;
      expect(r.t.status().nextDialAt! - r.clock.now(), greaterThan(100), reason: '${e.name}: a dial is pending');
      for (var i = 0; i < 10; i++) {
        r.lifecycle.emit(e);
      }
      await flush();
      expect(r.t.status().stats.dials, dials + 1, reason: '${e.name}: ten signals within one interval make exactly one early dial');
      expect(r.t.status().nextDialAt! - r.clock.now(), greaterThanOrEqualTo(4000), reason: '${e.name}: and the ramp was not reset');
    }
  });

  test('no wake signal changes a terminal state', () async {
    final r = Rig();
    r.sync.answer = (req) => bootstrapPage(req.after);
    r.t.start();
    final s = await r.connect();
    s.serverClose(4001);
    await flush();
    wakeEvents.forEach(r.lifecycle.emit);
    r.clock.jump(600000);
    await r.clock.advance(60000);
    expect(r.t.status().terminal.kind, TerminalKind.protocol);
    expect(r.t.status().stats.dials, 1);
  });

  test('ruling 5 → B: visible, online and pageshow pull a catch-up once per interval, inert before ready', () async {
    final r = Rig();
    r.sync.answer = (req) => bootstrapPage(req.after);
    r.t.start();
    await flush();
    // Before the first ready: the dial pulled its own /sync, and the policy adds nothing.
    final beforeReady = r.sync.requests.length;
    for (final e in [LifecycleEvent.visible, LifecycleEvent.online, LifecycleEvent.pageshow]) {
      r.lifecycle.emit(e);
    }
    await flush();
    expect(r.sync.requests, hasLength(beforeReady), reason: 'inert before the first ready');

    await r.connect(ready(heartbeatIntervalSec: 35));
    var n = r.sync.requests.length;
    r.lifecycle.emit(LifecycleEvent.visible);
    await flush();
    expect(r.sync.requests, hasLength(n + 1), reason: 'becoming visible pulls one catch-up');
    n = r.sync.requests.length;
    r.lifecycle.emit(LifecycleEvent.online);
    r.lifecycle.emit(LifecycleEvent.pageshow);
    r.lifecycle.emit(LifecycleEvent.visible);
    await flush();
    expect(r.sync.requests, hasLength(n), reason: 'at most once per heartbeat interval');
    await r.clock.advance(35000);
    // Keep the socket alive through the tick.
    for (final p in r.net.last.written_<Ping>()) {
      r.net.last.frame(Pong(id: p.id));
    }
    n = r.sync.requests.length;
    r.lifecycle.emit(LifecycleEvent.pageshow);
    await flush();
    expect(r.sync.requests, hasLength(n + 1), reason: 'and again once the interval has passed');
    r.lifecycle.emit(LifecycleEvent.resume);
    await flush();
    expect(r.sync.requests, hasLength(n + 1), reason: 'resume is a wake signal, not a catch-up trigger');
  });
}
