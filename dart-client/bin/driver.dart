/// The Dart transport driver (CANT-42 row d): the Dart client's transport, run
/// as a child process and controlled over stdio, so soakrig and cmd/catenary's
/// kill-test and restore-test rigs can run it as a client beside the Go and
/// TypeScript ones and judge its journal with the same, unchanged
/// `client.Compare`. It is the twin of web/src/transport/driver/driver.ts and
/// speaks that file's protocol; THE PROTOCOL IS WRITTEN DOWN THERE, once, and
/// recorded on CANT-42. The Go half is `tsDriver` in
/// server/cmd/soakrig/tsdriver.go: one adapter, launched with two command
/// lines.
///
/// Line-delimited JSON. Each request is one line, `{"id", "cmd", "args"}`, and
/// each is answered by exactly one line carrying the same id: `{"id", "ok":
/// true, "result"}` or `{"id", "ok": false, "error": {"kind", "message",
/// "frame"}}`. Requests are handled concurrently, so a `send` awaiting its ack
/// never holds up a `status`. Nothing else is ever written to stdout; the
/// transport's own log, when asked for, goes to stderr.
///
/// NINE COMMANDS HERE: start, send, read, sever, blackhole, catchup, status,
/// snapshot and stop, each as the driver.ts header states it. `compose` and
/// `outbox` are CANT-42 row f, after the Dart outbox exists; until then they
/// are answered `UnknownCommand`, as any other unknown command is.
///
/// `Kill` IS NOT A COMMAND. It is SIGKILL on this process, sent from Go, and
/// nothing here can observe it. Closing stdin ends the process too, so a rig
/// that dies without cleaning up leaves no driver behind.
///
/// THE JOURNAL is in memory (`MemoryJournal`) and dies with the process,
/// unless the driver is launched with `--journal=<file>`: then it is the real
/// `SqliteJournal` over that file, and a driver launched again over the same
/// file — after a SIGKILL — resumes from what the dead one had committed.
/// There is no stand-in to persist, as the Node driver needs for IndexedDB:
/// SQLite's own commit is the durability point.
///
/// BOTH URLS COME FROM `baseUrl`. The transport builds its socket URL and its
/// /sync URL from the one base URL `start` gives it, so a rig that hands this
/// driver a proxy's address owns its whole network (CANT-46's partition).
/// The driver's own loopback proxy (below) sits in front of the SOCKET only,
/// for `sever` and `blackhole`, exactly as proxy.ts does for Node.
///
/// THE CREDENTIAL IS HELD (`HeldCredential`, Go's `Refresh: false`), which is
/// what the rigs run (CANT-31 criterion 39).
///
/// BUILT, NOT `dart run`. `dart run` writes `Running build hooks...` to stdout
/// ahead of the first answer, and nothing but answers may be on stdout. From
/// dart-client/:
///
///     dart build cli -t bin/driver.dart -o build/driver
///     build/driver/bundle/bin/driver [--journal=<file>]
///
/// The bundle carries the SQLite library package:sqlite3's build hook
/// provides. `--probe` opens a SQLite database in memory and exits 0: it is
/// how a rig finds out, before it hands this driver a client, that the
/// executable runs AND can load that library.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';

/// A refusal the Go half maps back onto its own error values.
final class DriverError implements Exception {
  const DriverError(this.kind, this.message, [this.frame]);

  final String kind;
  final String message;
  final Map<String, Object?>? frame;
}

/// One proxied connection: the transport's end and the server's.
final class _Pair {
  _Pair(this.down, this.up);

  final Socket down;
  final Socket up;
  var holed = false;
  var downGone = false;
  var upGone = false;
}

/// A loopback TCP pass-through between the transport and the server, so the
/// driver can do to a socket what a network does: `sever` drops every
/// connection with no close frame, and `blackhole` silences every connection
/// open now and leaves it open — a half-dead socket, which only the heartbeat
/// finds. Connections made after a black hole forward normally. The twin of
/// web/src/transport/driver/proxy.ts.
final class TcpProxy {
  TcpProxy._(this._server, this._targetHost, this._targetPort);

  final ServerSocket _server;
  final String _targetHost;
  final int _targetPort;
  final _pairs = <_Pair>{};

  int get port => _server.port;

  static Future<TcpProxy> start(String targetHost, int targetPort) async {
    final proxy = TcpProxy._(await ServerSocket.bind(InternetAddress.loopbackIPv4, 0), targetHost, targetPort);
    proxy._server.listen(proxy._accept);
    return proxy;
  }

  Future<void> _accept(Socket down) async {
    final Socket up;
    try {
      up = await Socket.connect(_targetHost, _targetPort);
    } on SocketException {
      down.destroy();
      return;
    }
    final pair = _Pair(down, up);
    _pairs.add(pair);
    // FORWARDED BY HAND RATHER THAN piped, so a black hole can stop the bytes
    // without closing anything, and swallow the FIN a pipe would pass on.
    void forward(Socket from, Socket to, void Function() gone) {
      void ended() {
        gone();
        // A black-holed pair passes on no close either, and stays listed until
        // both legs are gone, so a later `sever` still reaches the one left
        // open.
        if (pair.holed) {
          if (pair.downGone && pair.upGone) _pairs.remove(pair);
          return;
        }
        _pairs.remove(pair);
        to.destroy();
      }

      from.listen(
        (bytes) {
          if (!pair.holed) to.add(bytes);
        },
        onDone: ended,
        onError: (Object _) => ended(),
        cancelOnError: true,
      );
      // A write to a peer that has gone is reported on `done`; the read side's
      // onDone is where the pair ends.
      from.done.ignore();
    }

    forward(down, up, () => pair.downGone = true);
    forward(up, down, () => pair.upGone = true);
  }

  /// Drops every connection. Returns how many there were.
  int sever() {
    final n = _pairs.length;
    for (final p in _pairs.toList()) {
      _pairs.remove(p);
      p.down.destroy();
      p.up.destroy();
    }
    return n;
  }

  /// Silences every connection open now. Returns how many there were.
  int blackhole() {
    var n = 0;
    for (final p in _pairs) {
      if (p.holed) continue;
      p.holed = true;
      n++;
    }
    return n;
  }

  void close() {
    _server.close().ignore();
    sever();
  }
}

/// Resolves once `pred` holds over the transport's status, or after `timeout`.
Future<void> until(Transport t, bool Function(TransportStatus s) pred, Duration timeout) {
  if (pred(t.status())) return Future.value();
  final done = Completer<void>();
  late final void Function() off;
  final timer = Timer(timeout, () {
    if (!done.isCompleted) done.complete();
  });
  off = t.subscribe((s) {
    if (pred(s) && !done.isCompleted) done.complete();
  });
  return done.future.whenComplete(() {
    timer.cancel();
    off();
  });
}

Future<void> main(List<String> argv) async {
  String? journalFile;
  for (final a in argv) {
    if (a.startsWith('--journal=')) {
      journalFile = a.substring('--journal='.length);
    } else if (a == '--probe') {
      // Through the journal's own open, so it is the real binding and the
      // real migrations that are proven, and nothing touches a disk.
      SqliteJournal.open(':memory:').close();
      exit(0);
    } else {
      stderr.writeln('driver: unknown argument $a');
      exit(2);
    }
  }

  final StagedJournal journal = journalFile == null ? MemoryJournal() : SqliteJournal.open(journalFile);
  Transport? transport;
  TcpProxy? proxy;
  var proxyTarget = '';

  Transport need() => transport ?? (throw const DriverError('NotStarted', 'driver: no transport is running; send start first'));

  final handlers = <String, Future<Object?> Function(Map<String, Object?> args)>{
    'start': (args) async {
      if (transport != null) throw const DriverError('AlreadyStarted', 'driver: a transport is already running');
      final baseUrl = args['baseUrl']! as String;
      final base = Uri.parse(baseUrl);
      final targetPort = base.hasPort ? base.port : (base.scheme == 'https' ? 443 : 80);
      final target = '${base.host}:$targetPort';
      var p = proxy;
      if (p == null) {
        p = proxy = await TcpProxy.start(base.host, targetPort);
        proxyTarget = target;
      } else if (proxyTarget != target) {
        throw DriverError('BadArgs', 'driver: this driver proxies $proxyTarget, not $target');
      }
      final port = p.port;
      final credential = args['credential']! as Map<String, Object?>;
      final faults = (args['faults'] as Map<String, Object?>?) ?? const {};
      final backoffMin = args['backoffMinMs'] as num?;
      final backoffMax = args['backoffMaxMs'] as num?;
      final t = createTransport(TransportConfig(
        baseUrl: baseUrl,
        credential: HeldCredential(Credential(
          userId: credential['userId']! as String,
          deviceId: credential['deviceId']! as String,
          accessToken: credential['accessToken']! as String,
        )),
        journal: journal,
        clientVersion: (args['clientVersion'] as String?) ?? 'soakrig-driver',
        // THE SEAM THE PROXY SITS BEHIND. The transport builds its URL from
        // `baseUrl` as it would on a device; only the host it connects to
        // moves.
        connect: (url, protocols) =>
            ioWebSocket(url.replace(host: InternetAddress.loopbackIPv4.address, port: port), protocols),
        lifecycle: ManualLifecycle(),
        logger: args['log'] == true ? const StderrLogger() : const SilentLogger(),
        backoffMinMs: backoffMin == null || backoffMin <= 0 ? null : backoffMin,
        backoffMaxMs: backoffMax == null || backoffMax <= 0 ? null : backoffMax,
        faults: Faults.named([
          for (final e in faults.entries)
            if (e.value == true) e.key,
        ]),
      ));
      transport = t;
      t.start();
      return const <String, Object?>{};
    },
    'send': (args) async {
      final t = need();
      final frame = ClientSend.fromJson(args['frame']);
      try {
        return {'ack': (await t.send(frame)).toJson()};
      } on SendRefused catch (e) {
        throw DriverError('SendRefused', e.toString(), e.frame.toJson());
      } on NotConnected catch (e) {
        throw DriverError('NotConnected', e.toString());
      } on SessionEnded catch (e) {
        throw DriverError('SessionEnded', e.toString());
      } on SendInFlight catch (e) {
        throw DriverError('SendInFlight', e.toString());
      }
    },
    'read': (args) async {
      final frame = ClientRead.fromJson(args['frame']);
      // Not `need()`: before `start` there is no session either, and the Go
      // half reads one refusal for both, as `(*Client).Read` gives one. THE
      // REFUSAL IS THE DRIVER'S: `Transport.read` drops a frame silently when
      // there is no connection.
      final t = transport;
      if (t == null || !t.status().ready) {
        throw const DriverError('NotConnected', 'driver: read without a ready session');
      }
      t.read(frame);
      return const <String, Object?>{};
    },
    'sever': (_) async {
      final severed = proxy?.sever() ?? 0;
      // ANSWERED ONCE THE TRANSPORT HAS SEEN IT, so a rig that severs and then
      // waits for `ready` cannot read the old session's `ready` and move on
      // before the drop has even arrived. Bounded: with no socket carried
      // there is nothing to wait for.
      final t = transport;
      if (severed > 0 && t != null) await until(t, (s) => !s.connected, const Duration(seconds: 5));
      return {'severed': severed};
    },
    'blackhole': (_) async => {'holed': proxy?.blackhole() ?? 0},
    'catchup': (_) async {
      need().catchUp();
      return const <String, Object?>{};
    },
    'status': (_) async => {'status': need().status().toJson(), 'journalWipes': journal.wipes},
    'snapshot': (_) async {
      final s = journal.snapshot();
      return {
        'cursor': s.cursor,
        'messages': [for (final m in s.messages) m.toJson()],
        'conversations': [for (final c in s.conversations) c.toJson()],
        'users': [for (final u in s.users) u.toJson()],
        'counted': journal.counted,
        'wipes': journal.wipes,
      };
    },
    'stop': (_) async {
      transport?.stop();
      transport = null;
      return const <String, Object?>{};
    },
  };

  void answer(Object? id, Map<String, Object?> body) => stdout.writeln(jsonEncode({'id': id, ...body}));

  Future<void> handle(String line) async {
    if (line.trim().isEmpty) return;
    final Map<String, Object?> req;
    try {
      req = jsonDecode(line) as Map<String, Object?>;
    } on Object {
      stderr.writeln(jsonEncode({'level': 'warn', 'msg': 'driver: a request that is not JSON; ignored'}));
      return;
    }
    final id = req['id'];
    final cmd = req['cmd'];
    final h = handlers[cmd];
    if (h == null) {
      answer(id, {
        'ok': false,
        'error': {'kind': 'UnknownCommand', 'message': 'driver: unknown command $cmd'},
      });
      return;
    }
    try {
      final result = await h((req['args'] as Map<String, Object?>?) ?? const {});
      answer(id, {'ok': true, 'result': result});
    } on DriverError catch (e) {
      answer(id, {
        'ok': false,
        'error': {'kind': e.kind, 'message': e.message, if (e.frame != null) 'frame': e.frame},
      });
    } on Object catch (e) {
      answer(id, {
        'ok': false,
        'error': {'kind': 'Error', 'message': e.toString()},
      });
    }
  }

  await for (final line in stdin.transform(utf8.decoder).transform(const LineSplitter())) {
    unawaited(handle(line));
  }
  transport?.stop();
  proxy?.close();
  await stdout.flush();
  exit(0);
}
