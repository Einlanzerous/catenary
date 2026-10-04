/// The client heartbeat — mirrors web/src/transport/heartbeat.ts and
/// `Client.heartbeat` in internal/client/client.go (CANT-23).
///
/// `ping` every `ready.heartbeat_interval_sec`, from `ready`, each with a
/// unique id; pongs matched by id; the socket severed once
/// `ready.missed_pong_limit` pings are outstanding, rather than waiting for the
/// OS to notice a half-open connection (R1 §3). NO CLIENT CONSTANT FOR EITHER
/// NUMBER: both arrive on `ready`, because two constants in two clients drift,
/// and this one would be found wrong only in the field.
///
/// AND THE WAKE DETECTOR, which Go does not have. A backgrounded app's timers
/// are throttled and a sleeping device stops them, so the gap between two
/// ticks is read off the wall clock: longer than
/// `interval × (missed_pong_limit + 1)` — the server's own backstop window,
/// after which it has already closed this session with `4000` — and the tick
/// is a wake signal instead of an ordinary ping.
library;

import 'dart:math';

import 'seams.dart';

abstract interface class HeartbeatHost {
  Timers get timers;
  Clock get now;

  /// Write a `ping` with this id.
  void ping(String id);

  /// Sever the socket: `limit` pings are outstanding.
  void sever(int outstanding);

  /// The wall clock jumped past the backstop window between two ticks.
  void wake();
  void pingsSent();
  void pongReceived(int? rttMs);
}

final class Heartbeat {
  Heartbeat(this._host);

  final HeartbeatHost _host;
  var _seq = 0;
  Object? _timer;
  var _intervalMs = 0;
  var _limit = 1;
  var _lastTick = 0;
  final _outstanding = <String, int>{};

  /// From `ready`: the two numbers it announced.
  void start(int intervalSec, int missedPongLimit) {
    stop();
    if (intervalSec <= 0) return;
    _intervalMs = intervalSec * 1000;
    _limit = max(1, missedPongLimit);
    _lastTick = _host.now();
    _schedule();
  }

  void stop() {
    if (_timer != null) _host.timers.clearTimeout(_timer);
    _timer = null;
    _outstanding.clear();
  }

  bool get running => _timer != null;

  /// A wake signal's immediate ping, so a half-dead socket is found by the
  /// next tick rather than `missed_pong_limit` ticks later. Counted as
  /// outstanding like any other.
  void pingNow() {
    if (_timer == null) return;
    _send();
  }

  void pong(String id) {
    final at = _outstanding.remove(id);
    _host.pongReceived(at == null ? null : _host.now() - at);
  }

  void _schedule() {
    _timer = _host.timers.setTimeout(_tick, _intervalMs);
  }

  void _tick() {
    final now = _host.now();
    final gap = now - _lastTick;
    _lastTick = now;
    if (gap > _intervalMs * (_limit + 1)) {
      // The session is past the server's backstop; the wake signal's own
      // ping stands in for this tick's.
      _schedule();
      _host.wake();
      return;
    }
    final n = _outstanding.length;
    if (n >= _limit) {
      stop();
      _host.sever(n);
      return;
    }
    _schedule();
    _send();
  }

  void _send() {
    final id = 'hb-${++_seq}';
    _outstanding[id] = _host.now();
    _host.pingsSent();
    _host.ping(id);
  }
}
