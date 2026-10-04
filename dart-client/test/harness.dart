/// The fakes the transport's tests run against: a clock and its timers, a
/// socket, a `/sync` server, a lifecycle, a log. The twin of
/// web/src/transport/test/harness.ts.
///
/// Every frame a fake sends is wire JSON built by the generated ENCODERS, and
/// every frame the transport writes is read back through the generated
/// DECODERS, so the tests exercise the same codecs the app does.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'support.dart';

export 'support.dart';

/// Drains every pending microtask and event (and anything they chain).
Future<void> flush() => pumpEventQueue();

final device = uuid(3);
const token = 'SECRET-access-token-that-must-never-be-logged-0001';

/// 2026-09-29T12:00:00Z, where every fake clock starts.
final epoch = DateTime.utc(2026, 9, 29, 12).millisecondsSinceEpoch;

/// A clock whose timers run only when told to. `now()` is the wall clock.
final class FakeClock implements Timers {
  num t = epoch;
  var _seq = 0;
  final _timers = <int, ({num due, void Function() fn})>{};

  int now() => t.floor();

  @override
  Object setTimeout(void Function() fn, num ms) {
    final id = ++_seq;
    _timers[id] = (due: t + (ms < 0 ? 0 : ms), fn: fn);
    return id;
  }

  @override
  void clearTimeout(Object? handle) => _timers.remove(handle);

  int get pending => _timers.length;

  /// Moves the clock `ms` forward, running every timer that falls due in
  /// order, with the event queue drained after each.
  Future<void> advance(num ms) async {
    final target = t + ms;
    await flush();
    for (;;) {
      MapEntry<int, ({num due, void Function() fn})>? next;
      for (final e in _timers.entries) {
        if (e.value.due <= target && (next == null || e.value.due < next.value.due)) next = e;
      }
      if (next == null) break;
      _timers.remove(next.key);
      if (next.value.due > t) t = next.value.due;
      next.value.fn();
      await flush();
    }
    t = target;
    await flush();
  }

  /// The wall clock jumps — a device slept and woke — with no timer running.
  void jump(num ms) {
    t += ms;
  }
}

final class FakeSocket implements WebSocketLike {
  FakeSocket(this.url, this.protocols);

  final Uri url;
  final List<String> protocols;
  final written = <String>[];

  /// Set once the client closes it: the code it closed with, or null for none.
  ({int? code})? closedByClient;
  var isOpen = false;

  @override
  void Function()? onOpen;

  @override
  void Function(Object? data)? onMessage;

  @override
  void Function(int? code)? onClose;

  @override
  void send(String data) {
    if (!isOpen) throw StateError('not open');
    written.add(data);
  }

  @override
  void close([int? code, String? reason]) {
    closedByClient = (code: code);
    isOpen = false;
  }

  /// What the client wrote, through the generated decoder.
  List<ClientFrame> frames() => [for (final w in written) ClientFrame.fromJson(jsonDecode(w))].nonNulls.toList();

  List<T> written_<T extends ClientFrame>() => frames().whereType<T>().toList();

  void open() {
    isOpen = true;
    onOpen?.call();
  }

  /// A server frame, encoded by the generated encoder.
  void frame(ServerFrame f) => raw(jsonEncode(f.toJson()));

  void raw(Object? data) => onMessage?.call(data);

  /// The server closes with `code`; 1006 is an abnormal closure.
  void serverClose(int code) {
    isOpen = false;
    onClose?.call(code);
  }

  /// A dial that never opens.
  void fail() => onClose?.call(null);
}

final class FakeNet {
  final sockets = <FakeSocket>[];

  /// Runs on each new socket; the default leaves it to the test.
  void Function(FakeSocket s) onDial = (_) {};

  WebSocketLike connect(Uri url, List<String> protocols) {
    final s = FakeSocket(url, protocols);
    sockets.add(s);
    scheduleMicrotask(() => onDial(s));
    return s;
  }

  FakeSocket get last => sockets.last;
}

typedef SyncRequest = ({int after, String? limit, String? authorization});

/// A `/sync` endpoint. `answer` decides each response — a `SyncResponse`, an
/// `HttpAnswer`, or an `Exception` for a request that got no response — and
/// `requests` records them.
final class FakeSync {
  final requests = <SyncRequest>[];
  FutureOr<Object> Function(SyncRequest req) answer = (req) => page(req.after);

  Future<HttpAnswer> fetch(HttpExchange x) async {
    final req = (
      after: int.parse(x.url.queryParameters['after']!),
      limit: x.url.queryParameters['limit'],
      authorization: x.headers['Authorization'],
    );
    requests.add(req);
    final a = await answer(req);
    return switch (a) {
      SyncResponse() => HttpAnswer(200, jsonEncode(a.toJson())),
      HttpAnswer() => a,
      Exception() => throw a,
      _ => throw StateError('FakeSync: an answer of type ${a.runtimeType}'),
    };
  }
}

final class NetworkDown implements Exception {
  const NetworkDown();
}

typedef LogLine = ({String level, String msg, Map<String, Object?> fields});

final class ListLogger implements Logger {
  final lines = <LogLine>[];

  @override
  void info(String msg, [Map<String, Object?> fields = const {}]) => lines.add((level: 'info', msg: msg, fields: fields));

  @override
  void warn(String msg, [Map<String, Object?> fields = const {}]) => lines.add((level: 'warn', msg: msg, fields: fields));
}

/// The minimum jitter draw: the worst case for dial counts.
Uint8List minDraw(int n) => Uint8List(n);

/// The maximum jitter draw.
Uint8List maxDraw(int n) => Uint8List(n)..fillRange(0, n, 0xff);

ServerReady ready({int heartbeatIntervalSec = 35, int missedPongLimit = 2, int logSeq = 1000}) => ServerReady(
      sessionId: uuid(9000),
      serverTime: at,
      heartbeatIntervalSec: heartbeatIntervalSec,
      missedPongLimit: missedPongLimit,
      // A head above anything a test holds: a `ready` below the cursor is
      // obligation 4's case, and a test asks for it by name.
      logSeq: logSeq,
      resumed: false,
    );

ServerMessageFrame messageFrame(Message m) => ServerMessageFrame(message: m);

final class Rig {
  Rig({
    Faults faults = Faults.none,
    StagedJournal? journal,
    RandomBytes random = minDraw,
    num? backoffMinMs,
    num? backoffMaxMs,
    int? syncLimit,
    FakeClock? clock,
    CredentialSeam Function(Rig r)? seam,
  })  : journal = journal ?? MemoryJournal(),
        clock = clock ?? FakeClock() {
    t = createTransport(TransportConfig(
      baseUrl: baseUrl,
      credential: seam?.call(this) ?? HeldCredential(credential),
      journal: this.journal,
      clientVersion: '0.0.0-test',
      connect: net.connect,
      fetch: sync.fetch,
      now: this.clock.now,
      timers: this.clock,
      random: random,
      lifecycle: lifecycle,
      logger: logger,
      faults: faults,
      backoffMinMs: backoffMinMs,
      backoffMaxMs: backoffMaxMs,
      syncLimit: syncLimit,
    ));
    addTearDown(t.stop);
  }

  static const baseUrl = 'http://catenary.test';

  late final Transport t;
  final FakeClock clock;
  final net = FakeNet();
  final sync = FakeSync();
  final lifecycle = ManualLifecycle();
  final StagedJournal journal;
  final logger = ListLogger();
  final credential = Credential(userId: me, deviceId: device, accessToken: token);

  /// Opens the newest socket and answers its hello with `ready`.
  Future<FakeSocket> connect([ServerReady? r]) async {
    await flush();
    final s = net.last;
    s.open();
    s.frame(r ?? ready());
    await flush();
    return s;
  }

  /// A relaunch: a new transport over the same journal and credential.
  Rig again() => Rig(journal: journal);
}
