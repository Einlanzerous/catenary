/// The injected seams — every source of nondeterminism the transport has comes
/// in through one of these. Mirrors web/src/transport/seams.ts, which mirrors
/// the injection fields of `Config` in internal/client/client.go (`Now`,
/// `Pause`, `Rand`, `HTTPClient`), with the socket and the lifecycle signals a
/// device produces. The interfaces are here; the `dart:io` implementations a
/// plain `dart` executable uses are in io_seams.dart, and a test passes its
/// own. Nothing here names a platform, which is what keeps Flutter out.
library;

import 'dart:async';
import 'dart:math';
import 'dart:typed_data';

/// The subset of a WebSocket the transport touches. It is handed back from
/// [WebSocketConnect] not yet open: [onOpen] fires when it is, and [onClose]
/// when it ends — with the close status the peer sent, or null for none —
/// whether or not it ever opened.
abstract interface class WebSocketLike {
  /// Throws when the socket is not open.
  void send(String data);
  void close([int? code, String? reason]);

  set onOpen(void Function()? fn);

  /// A text message is a `String`; anything else is a binary one.
  set onMessage(void Function(Object? data)? fn);
  set onClose(void Function(int? code)? fn);
}

typedef WebSocketConnect = WebSocketLike Function(Uri url, List<String> protocols);

/// Timers. The transport sleeps through nothing else.
abstract interface class Timers {
  /// `ms` may be fractional: a jittered wait is.
  Object setTimeout(void Function() fn, num ms);
  void clearTimeout(Object? handle);
}

final class SystemTimers implements Timers {
  const SystemTimers();

  @override
  Object setTimeout(void Function() fn, num ms) => Timer(Duration(microseconds: (max(0, ms) * 1000).round()), fn);

  @override
  void clearTimeout(Object? handle) => (handle as Timer?)?.cancel();
}

/// The wall clock, in ms since the epoch. NEVER a monotonic clock: CANT-31 §1
/// forbids scheduling on one, because a device that slept measures six hours
/// as a few seconds on it. Durations the transport measures (a session's time
/// `ready`, the wake detector's gap) are read off this too, and a clock that
/// jumps is exactly what the wake detector exists to notice.
typedef Clock = int Function();

int systemClock() => DateTime.now().millisecondsSinceEpoch;

/// `n` random bytes. The backoff jitter draws four; a refresh proposal 32.
typedef RandomBytes = Uint8List Function(int n);

final _secure = Random.secure();

Uint8List secureRandom(int n) => Uint8List.fromList([for (var i = 0; i < n; i++) _secure.nextInt(256)]);

/// One HTTP request, as the transport makes it. [abort] completes when the
/// caller has given up — a timeout, a stop — and an implementation tears the
/// request down then; the caller has already stopped waiting.
final class HttpExchange {
  const HttpExchange({required this.method, required this.url, this.headers = const {}, this.body, required this.abort});

  final String method;
  final Uri url;
  final Map<String, String> headers;
  final String? body;
  final Future<void> abort;
}

/// What came back. Header names are lower-case.
final class HttpAnswer {
  const HttpAnswer(this.status, this.body, [this.headers = const {}]);

  final int status;
  final String body;
  final Map<String, String> headers;
}

/// Fails when no response arrived at all.
typedef HttpFetch = Future<HttpAnswer> Function(HttpExchange request);

/// What a device tells the transport about itself. The names are the
/// reference's, which are a browser's: `visible` and `hidden` are the app
/// coming to the foreground and leaving it, `online` and `offline` the
/// network's, and `resume` a process the OS had suspended running again.
enum LifecycleEvent { online, offline, visible, hidden, pageshow, pagehide, freeze, resume }

abstract interface class Lifecycle {
  /// Returns the unsubscribe.
  void Function() subscribe(void Function(LifecycleEvent e) fn);
}

/// A lifecycle driven by hand: tests, a driver, and a host with no signals.
final class ManualLifecycle implements Lifecycle {
  final _fns = <void Function(LifecycleEvent)>{};

  @override
  void Function() subscribe(void Function(LifecycleEvent e) fn) {
    _fns.add(fn);
    return () => _fns.remove(fn);
  }

  void emit(LifecycleEvent e) {
    for (final fn in _fns.toList()) {
      fn(e);
    }
  }
}

/// Structured log lines, with the Go client's event names. NEVER GIVEN A TOKEN.
abstract interface class Logger {
  void info(String msg, [Map<String, Object?> fields = const {}]);
  void warn(String msg, [Map<String, Object?> fields = const {}]);
}

final class SilentLogger implements Logger {
  const SilentLogger();

  @override
  void info(String msg, [Map<String, Object?> fields = const {}]) {}

  @override
  void warn(String msg, [Map<String, Object?> fields = const {}]) {}
}
