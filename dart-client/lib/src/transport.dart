/// The transport — mirrors web/src/transport/transport.ts, which mirrors
/// internal/client/client.go: the session loop, the dial, frame dispatch, and
/// the seam the outbox adapts to.
///
/// One transport per context, on that context's isolate. Every source of
/// nondeterminism comes in through a seam (seams.dart), so the whole state
/// machine runs under a fake clock, a fake socket and a fake server.
///
/// THE RULES ARE CITED, NOT RESTATED, so this file does not become a copy that
/// drifts: CANT-24's five obligations (docs/decisions/cant-24-resume.md),
/// CANT-103's rules (docs/decisions/cant-103-conversation-introduction.md),
/// CANT-23's heartbeat (heartbeat.dart), CANT-31 §4's close table (closes.dart)
/// and §6's terminal states (terminal.dart), and CANT-35's rulings 3, 4 and 5
/// (backoff.dart, and the receipt and lifecycle arms below).
///
/// THE CREDENTIAL IS A SEAM. Every refresh hook is on `CredentialSeam`
/// (credential.dart), called where Go calls its counterpart: `HeldCredential`
/// is Go's `Refresh: false`.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:catenary_wire/catenary_wire.dart';

import 'backoff.dart';
import 'backoff.dart' as backoff show backoffMaxMs, backoffMinMs;
import 'catchup.dart';
import 'closes.dart';
import 'credential.dart';
import 'faults.dart';
import 'heartbeat.dart';
import 'io_seams.dart';
import 'journal.dart';
import 'seams.dart';
import 'sqlite_journal.dart';
import 'status.dart';
import 'terminal.dart';

const subprotocolV1 = 'catenary.v1';
const tokenSubprotocolPrefix = 'catenary.token.';

/// Go's `dialTimeout` and `syncTimeout`: a socket that has not opened, or a
/// `/sync` that has not answered, in this long is a failed attempt.
const _dialTimeoutMs = 30000;
const _syncTimeoutMs = 30000;

final class TransportConfig {
  const TransportConfig({
    required this.baseUrl,
    required this.credential,
    this.journal,
    this.clientVersion,
    this.syncLimit,
    this.connect,
    this.fetch,
    this.now,
    this.timers,
    this.random,
    this.lifecycle,
    this.logger,
    this.backoffMinMs,
    this.backoffMaxMs,
    this.faults = Faults.none,
  });

  /// The server's origin, `http://` or `https://`. `/ws` and `/sync` are
  /// appended.
  final String baseUrl;

  /// The credential layer; `HeldCredential` where the pair is presented as
  /// held (the rigs).
  final CredentialSeam credential;

  /// Default: a fresh in-memory journal. The app passes a `SqliteJournal` to
  /// resume from the stored cursor.
  final Journal? journal;

  /// Goes on the hello as `catenary-dart/<version>`. Never parsed.
  final String? clientVersion;

  /// The page size `/sync` is asked for. Absent is the server's.
  final int? syncLimit;

  /// Defaults: `dart:io`'s (io_seams.dart), an inert lifecycle, and log lines
  /// on stderr.
  final WebSocketConnect? connect;
  final HttpFetch? fetch;
  final Clock? now;
  final Timers? timers;
  final RandomBytes? random;
  final Lifecycle? lifecycle;
  final Logger? logger;

  /// The dial ramp's floor and ceiling. FOR RIGS ONLY, as Go's
  /// `Config.BackoffMin`/`BackoffMax` are: soakrig runs every cohort at
  /// 100 ms / 2 s so they storm alike.
  final num? backoffMinMs;
  final num? backoffMaxMs;

  /// Never set outside a test or a rig proving an assertion can fail.
  final Faults faults;
}

/// What the outbox needs from this layer to apply CANT-31 §7 — the fact, not
/// the policy. Emitted once per session, when it ends, and never when one
/// becomes ready: `ready` comes from `subscribe()` and only from it.
final class SessionEnd {
  const SessionEnd({
    required this.sessionGen,
    required this.opened,
    required this.readied,
    required this.closeCode,
    required this.preceding,
    required this.bare1008,
    required this.verdict,
  });

  /// Monotonically increasing per dial.
  final int sessionGen;

  /// False for a pure dial failure: no socket ever opened.
  final bool opened;
  final bool readied;

  /// The close status the peer sent; null for none, which includes a socket
  /// this client severed itself and one it closed on `stop()` or on entering a
  /// terminal state.
  final int? closeCode;

  /// CANT-31 §4's *preceded by*: the last message event before the close, if
  /// it was an `error` carrying no `client_id`.
  final ServerError? preceding;

  /// `closeCode == 1008 && preceding == null` — §7's trigger.
  final bool bare1008;
  final CloseVerdict verdict;
}

abstract interface class Transport {
  /// Idempotent. Refuses if terminal: a relaunch is a new Transport over the
  /// same stores.
  void start();

  /// A clean close (1000). Not terminal; `start()` again resumes.
  void stop();
  TransportStatus status();

  /// Called with a fresh status on every change. Returns the unsubscribe.
  void Function() subscribe(void Function(TransportStatus s) fn);

  /// Completes with the `ack`. Never mints or alters `client_id`. Fails with
  /// `NotConnected`, `SessionEnded`, `SendRefused` or `SendInFlight`.
  Future<ServerAck> send(ClientSend f);
  void read(ClientRead f);
  void typing(ClientTyping f);

  /// A trigger (obligation 3; obligation 5's hook). Withheld while a token is
  /// refused.
  void catchUp();

  /// Resets the ramp to its floor and dials now. No effect while terminal or
  /// while a token is refused.
  void retryNow();

  /// The explicit §1 check. Never held.
  Future<void> refreshIfDue();
  void Function() onSessionEnd(void Function(SessionEnd e) fn);
  void Function() onApply(void Function(Applied e) fn);
  JournalSnapshot snapshot();

  /// Inbound `typing` frames, carried and not decided about.
  void Function() onTyping(void Function(ServerTyping f) fn);
}

/// `send()` with no open socket. Nothing was written.
final class NotConnected implements Exception {
  const NotConnected();

  @override
  String toString() => 'NotConnected: no socket is open';
}

/// The socket closed before the send was answered. THE OUTCOME IS UNKNOWN:
/// retrying with the SAME `client_id` is safe.
final class SessionEnded implements Exception {
  const SessionEnded();

  @override
  String toString() => 'SessionEnded: the session ended before the send was answered';
}

/// The server refused this send with an `error` frame naming its `client_id`.
final class SendRefused implements Exception {
  const SendRefused(this.frame);

  final ServerError frame;

  @override
  String toString() => 'SendRefused: ${frame.code.wire}: ${frame.message}';
}

/// A send with this `client_id` is already awaiting its answer.
final class SendInFlight implements Exception {
  const SendInFlight();

  @override
  String toString() => 'SendInFlight: a send with this client_id is already awaiting its answer';
}

Transport createTransport(TransportConfig cfg) => _SocketTransport(cfg);

enum _DialWaitResult { elapsed, early, retryNow, stopped }

/// A session's end, with what the session loop needs beyond the public fact.
final class _DialEnd extends SessionEnd {
  const _DialEnd({
    required super.sessionGen,
    required super.opened,
    required super.readied,
    required super.closeCode,
    required super.preceding,
    required super.bare1008,
    required super.verdict,
    required this.readiedForMs,
    required this.intervalSec,
    required this.reason,
  });

  /// How long it stayed `ready`; null if it never was.
  final int? readiedForMs;
  final int? intervalSec;

  /// Why a terminal verdict is terminal.
  final String reason;
}

final class _Session {
  _Session(this.ws, this.gen, this.finish);

  final WebSocketLike ws;
  final int gen;
  final void Function(_DialEnd end) finish;
  var opened = false;
  var readied = false;
  int? readyAt;
  int? intervalSec;
  ServerError? preceding;
  var ended = false;
  Object? dialTimer;
}

final class _DialWait {
  _DialWait(this.atMax);

  final bool atMax;
  late final void Function(_DialWaitResult r) resolve;
}

final class _SyncAborted implements Exception {
  const _SyncAborted();
}

final class _SocketTransport implements Transport, HeartbeatHost, CatchUpHost, CredentialHost {
  _SocketTransport(TransportConfig cfg)
      : _cred = cfg.credential,
        _journal = cfg.journal ?? MemoryJournal(),
        _connect = cfg.connect ?? ioWebSocket,
        _fetch = cfg.fetch ?? ioHttpFetch,
        now = cfg.now ?? systemClock,
        timers = cfg.timers ?? const SystemTimers(),
        _random = cfg.random ?? secureRandom,
        _lifecycle = cfg.lifecycle ?? ManualLifecycle(),
        _log = cfg.logger ?? const StderrLogger(),
        faults = cfg.faults,
        _clientInfo = 'catenary-dart/${cfg.clientVersion ?? 'dev'}',
        _syncLimit = cfg.syncLimit {
    final base = Uri.parse(cfg.baseUrl.replaceFirst(RegExp(r'/+$'), ''));
    if (base.scheme != 'http' && base.scheme != 'https') {
      throw ArgumentError('transport: baseUrl scheme ${base.scheme}, want http or https');
    }
    _httpBase = base;
    _wsUrl = base.replace(scheme: base.scheme == 'https' ? 'wss' : 'ws', path: '${base.path}/ws');
    final lo = cfg.backoffMinMs;
    final hi = cfg.backoffMaxMs;
    backoffMinMs = lo != null && lo > 0 ? lo : backoff.backoffMinMs;
    backoffMaxMs = max(hi != null && hi > 0 ? hi : backoff.backoffMaxMs, backoffMinMs);
    _heartbeat = Heartbeat(this);
    _catchup = CatchUp(this);
    _cred.attach(this);
  }

  final CredentialSeam _cred;
  final Journal _journal;
  final WebSocketConnect _connect;
  final HttpFetch _fetch;
  @override
  final Clock now;
  @override
  final Timers timers;
  final RandomBytes _random;
  final Lifecycle _lifecycle;
  final Logger _log;
  @override
  final Faults faults;
  final String _clientInfo;
  final int? _syncLimit;
  late final Uri _httpBase;
  late final Uri _wsUrl;
  @override
  late final num backoffMinMs;
  @override
  late final num backoffMaxMs;
  late final Heartbeat _heartbeat;
  late final CatchUp _catchup;

  var _started = false;

  /// Bumped by every start and stop, so a loop from an earlier run sees it is
  /// over at its next await.
  var _runId = 0;

  /// The aborts of the `/sync` requests in flight, which a stop pulls.
  final _inFlight = <void Function()>{};
  void Function()? _unlisten;
  var _terminal = Terminal.not;

  _Session? _session;

  /// The socket `send` writes to: published only once the hello is on it.
  WebSocketLike? _conn;
  var _ready = false;

  /// A dial is in progress and has not reached `ready` (Go's `connecting`).
  var _connecting = false;
  String? _sessionId;
  int? _intervalSec;
  int? _missedPongLimit;

  /// From the most recent `ready` in this transport's life; null before any.
  int? _learnedIntervalSec;
  var _sessionGen = 0;
  Uuid? _selfUserId;

  @override
  final Stats stats = Stats();
  var _attempt = 0;
  num? _nextDialAt;
  _DialWait? _dialWait;
  int? _lastEarlyDialAt;
  int? _lastPolicyAt;

  /// Journal writes, one at a time, in arrival order.
  Future<void> _writes = Future.value();
  var _pendingWrites = 0;

  /// The last journal write's failure, until a write succeeds.
  JournalError? _journalError;

  /// Bumped when a wipe is decided — by this transport (obligation 4), or by
  /// another context, which this one learns as a `JournalStale` refusal
  /// (CANT-199): a page requested in an earlier epoch is
  /// dropped, not applied over the empty store (obligation 4).
  var _epoch = 0;
  var _wipes = 0;

  final _waiters = <Uuid, Completer<ServerAck>>{};
  final _statusFns = <void Function(TransportStatus)>{};
  final _endFns = <void Function(SessionEnd)>{};
  final _applyFns = <void Function(Applied)>{};
  final _typingFns = <void Function(ServerTyping)>{};

  // --- the public surface ----------------------------------------------------

  @override
  void start() {
    if (_started) return;
    if (_terminal.kind != TerminalKind.none) {
      _log.warn('start refused: this transport is terminal; a relaunch is a new transport', {'kind': _terminal.kind.name});
      return;
    }
    _started = true;
    final run = ++_runId;
    _unlisten = _lifecycle.subscribe(_onLifecycle);
    unawaited(_catchup.run(() => _alive(run)));
    unawaited(_runLoop(run));
    notify();
  }

  @override
  void stop() {
    if (!_started) return;
    _halt();
    notify();
  }

  @override
  TransportStatus status() {
    final c = _cred.status();
    final s = Stats.copy(stats)
      ..refreshes = c.refreshes
      ..refreshesSkipped = c.refreshesSkipped
      ..refreshErrors = c.refreshErrors
      ..refreshWalkBacks = c.refreshWalkBacks
      ..chainLength = c.chainLength
      ..refreshesHeldUnreachable = c.refreshesHeldUnreachable
      ..refreshesHeldBackoff = c.refreshesHeldBackoff;
    return TransportStatus(
      terminal: _terminal,
      refreshHold: c.refreshHold,
      tokenRefused: _cred.refused,
      nextRefreshAt: c.nextRefreshAt,
      connected: _conn != null,
      ready: _ready,
      sessionId: _sessionId,
      heartbeatIntervalSec: _intervalSec,
      missedPongLimit: _missedPongLimit,
      caughtUp: _catchup.caughtUp,
      cursor: _journal.cursor,
      attempt: _attempt,
      nextDialAt: _nextDialAt,
      stats: s,
      messages: _journal.messageCount,
      wipes: _wipes,
      headSeqTotal: _journal.headSeqTotal,
      journalError: _journalError,
    );
  }

  @override
  void Function() subscribe(void Function(TransportStatus s) fn) {
    _statusFns.add(fn);
    return () => _statusFns.remove(fn);
  }

  @override
  Future<ServerAck> send(ClientSend f) {
    final conn = _conn;
    if (conn == null) return Future.error(const NotConnected());
    if (_waiters.containsKey(f.clientId)) return Future.error(const SendInFlight());
    final waiter = Completer<ServerAck>();
    _waiters[f.clientId] = waiter;
    try {
      // THE FRAME GOES AS THE CALLER BUILT IT: a retry is the same frame,
      // client_id included.
      conn.send(jsonEncode(f.toJson()));
    } catch (_) {
      _waiters.remove(f.clientId);
      return Future.error(const NotConnected());
    }
    return waiter.future;
  }

  @override
  void read(ClientRead f) => _write(f.toJson());

  @override
  void typing(ClientTyping f) => _write(f.toJson());

  @override
  void catchUp() {
    if (_withheldSync()) return;
    _catchup.trigger();
  }

  @override
  void retryNow() {
    if (_terminal.kind != TerminalKind.none || !_started) return;
    if (_cred.refused) {
      stats.dialsWithheld++;
      notify();
      return;
    }
    _dialWait?.resolve(_DialWaitResult.retryNow);
  }

  @override
  Future<void> refreshIfDue() => _cred.refreshIfDue();

  @override
  void Function() onSessionEnd(void Function(SessionEnd e) fn) {
    _endFns.add(fn);
    return () => _endFns.remove(fn);
  }

  @override
  void Function() onApply(void Function(Applied e) fn) {
    _applyFns.add(fn);
    return () => _applyFns.remove(fn);
  }

  @override
  void Function() onTyping(void Function(ServerTyping f) fn) {
    _typingFns.add(fn);
    return () => _typingFns.remove(fn);
  }

  @override
  JournalSnapshot snapshot() => _journal.snapshot();

  // --- the session loop (Client.Run) ------------------------------------------

  bool _alive(int run) => _started && run == _runId && _terminal.kind == TerminalKind.none;

  Future<void> _runLoop(int run) async {
    var backoff = backoffMinMs;
    while (_alive(run)) {
      // CANT-129: no dial with a token Catenary has refused. Polled at the dial
      // cadence, counted, not logged per poll.
      if (await _cred.withholdDial()) {
        if (!_alive(run)) break;
        stats.dialsWithheld++;
        notify();
        if (await _dialWaitFor(backoffMaxMs, true) == _DialWaitResult.stopped) break;
        continue;
      }
      // The proactive refresh, bounded by CANT-127. Held: a no-op.
      await _cred.beforeDial();
      if (!_alive(run)) break;

      stats.dials++;
      _attempt++;
      _connecting = true;
      // A DIAL IS A TRIGGER, PULLED BEFORE THE UPGRADE, so the /sync goes out
      // beside it rather than a round trip later on `ready`.
      _catchup.trigger();
      notify();

      final end = await _dial(run);
      _emitSessionEnd(end);
      if (!_alive(run)) break;

      if (end.verdict == CloseVerdict.terminalProtocol) {
        terminal(Terminal(TerminalKind.protocol, end.reason));
        break;
      }

      // RULING 3 → B. The ramp resets only after a session that stayed ready
      // for a heartbeat interval; `error{internal}` waits the maximum, and no
      // wake signal shortens that wait.
      final stable = resetsRamp(end.readiedForMs, end.intervalSec);
      if (stable) {
        backoff = backoffMinMs;
        _attempt = 0;
      }
      final atMax = end.verdict == CloseVerdict.reconnectAtMaximum;
      if (atMax) backoff = backoffMaxMs;
      final wait = jitteredWait(backoff, unitFromBytes(_random(4)));
      _log.info('session ended; reconnecting', {
        'close': end.closeCode ?? -1,
        'opened': end.opened,
        'backoff_ms': wait.round(),
      });
      final r = await _dialWaitFor(wait, atMax);
      if (r == _DialWaitResult.stopped) break;
      if (r == _DialWaitResult.retryNow) {
        backoff = backoffMinMs;
        _attempt = 0;
      } else if (!stable) {
        // An early dial takes the pending dial's place and advances the ramp
        // as any failed dial does: it never resets it.
        backoff = advance(backoff, backoffMaxMs);
      }
    }
  }

  /// Waits for the next dial: the backoff, a wake signal's early dial, or
  /// `retryNow()`.
  Future<_DialWaitResult> _dialWaitFor(num ms, bool atMax) {
    final done = Completer<_DialWaitResult>();
    final w = _DialWait(atMax);
    Object? handle;
    w.resolve = (r) {
      if (!identical(_dialWait, w)) return;
      _dialWait = null;
      _nextDialAt = null;
      timers.clearTimeout(handle);
      done.complete(r);
      notify();
    };
    handle = timers.setTimeout(() => w.resolve(_DialWaitResult.elapsed), ms);
    _dialWait = w;
    _nextDialAt = now() + ms;
    notify();
    return done.future;
  }

  /// One socket, from dial to close (Go's `session`).
  Future<_DialEnd> _dial(int run) {
    final done = Completer<_DialEnd>();
    void failed(String? why) {
      _connecting = false;
      if (why != null) {
        stats.dialErrors++;
        stats.lastClose = why;
      }
      done.complete(_DialEnd(
        sessionGen: ++_sessionGen,
        opened: false,
        readied: false,
        closeCode: null,
        preceding: null,
        bare1008: false,
        verdict: CloseVerdict.reconnect,
        readiedForMs: null,
        intervalSec: null,
        reason: '',
      ));
    }

    _cred.current().then(
          (cred) => _alive(run) ? _openSocket(cred, done.complete, failed) : failed(null),
          onError: (Object _) => failed('dial: no credential'),
        );
    return done.future;
  }

  void _openSocket(Credential cred, void Function(_DialEnd) resolve, void Function(String) failed) {
    _selfUserId = cred.userId;
    final WebSocketLike ws;
    try {
      ws = _connect(_wsUrl, [subprotocolV1, tokenSubprotocolPrefix + cred.accessToken]);
    } catch (_) {
      // Not the error's text: a connector may quote its protocols, and one of
      // them carries the token.
      return failed('dial: the socket could not be constructed');
    }
    final s = _Session(ws, ++_sessionGen, resolve);
    _session = s;
    s.dialTimer = timers.setTimeout(() {
      if (!s.opened) _abandon(s);
    }, _dialTimeoutMs);

    ws.onOpen = () {
      if (s.ended) return;
      s.opened = true;
      timers.clearTimeout(s.dialTimer);
      // THE HELLO IS WRITTEN BEFORE THE SOCKET IS PUBLISHED. The server closes
      // a session whose first frame is not a hello with a bare 1008, and
      // `send` writes to whatever `_conn` holds — so until the hello is on the
      // wire, a send gets NotConnected rather than severing the session.
      final hello = ClientHello(
        wireVersion: wireVersion,
        deviceId: cred.deviceId,
        resumeFromLogSeq: _journal.cursor,
        clientInfo: _clientInfo,
      );
      try {
        ws.send(jsonEncode(hello.toJson()));
      } catch (_) {
        _abandon(s);
        return;
      }
      _conn = ws;
      notify();
    };
    ws.onMessage = (data) {
      if (!s.ended) _onMessage(s, data);
    };
    ws.onClose = (code) => _endSession(s, code);
  }

  /// Ends a session this client gave up on — a heartbeat sever, a dial
  /// timeout — with no close frame received: Go's `CloseNow`.
  void _abandon(_Session s) {
    try {
      s.ws.close();
    } catch (_) {
      // Closing a socket that never opened may throw; it is gone either way.
    }
    _endSession(s, null);
  }

  /// `stopping`: this client closed it on purpose (`stop()`, terminal), so a
  /// socket that never opened is not a dial error.
  void _endSession(_Session s, int? rawCode, {bool stopping = false}) {
    if (s.ended) return;
    s.ended = true;
    timers.clearTimeout(s.dialTimer);
    s.ws
      ..onOpen = null
      ..onMessage = null
      ..onClose = null;
    if (identical(_session, s)) _session = null;
    if (identical(_conn, s.ws)) _conn = null;
    _ready = false;
    _connecting = false;
    _heartbeat.stop();

    final waiters = _waiters.values.toList();
    _waiters.clear();
    for (final w in waiters) {
      w.completeError(const SessionEnded());
    }

    // A close this client made itself is not one the peer sent: it is
    // neither counted nor classified, and its SessionEnd carries no code.
    final key = closeStatusKey(stopping ? null : rawCode);
    final closeCode = key == -1 ? null : key;
    var verdict = CloseVerdict.reconnect;
    var reason = '';
    if (stopping) {
      stats.lastClose = 'closed by this client';
    } else if (s.opened) {
      stats.closeStatuses[key] = (stats.closeStatuses[key] ?? 0) + 1;
      stats.lastClose = closeCode == null ? 'closed with no close frame' : 'close $closeCode';
      (:verdict, :reason) = classifyClose(closeCode, s.preceding);
      if (faults.neverTerminal) {
        verdict = CloseVerdict.reconnect;
      } else if (faults.alwaysTerminal) {
        verdict = CloseVerdict.terminalProtocol;
        reason = 'fault: every close is terminal';
      }
    } else {
      // A pure dial failure: never a close status, and never a verdict to
      // read.
      stats.dialErrors++;
      stats.lastClose = 'dial failed: the socket never opened';
    }
    final readyAt = s.readyAt;
    s.finish(_DialEnd(
      sessionGen: s.gen,
      opened: s.opened,
      readied: s.readied,
      closeCode: closeCode,
      preceding: s.preceding,
      bare1008: closeCode == closePolicyViolation && s.preceding == null,
      verdict: verdict,
      readiedForMs: readyAt == null ? null : now() - readyAt,
      intervalSec: s.intervalSec,
      reason: reason,
    ));
    notify();
  }

  void _emitSessionEnd(_DialEnd end) {
    final e = SessionEnd(
      sessionGen: end.sessionGen,
      opened: end.opened,
      readied: end.readied,
      closeCode: end.closeCode,
      preceding: end.preceding,
      bare1008: end.bare1008,
      verdict: end.verdict,
    );
    for (final fn in _endFns.toList()) {
      try {
        fn(e);
      } catch (err) {
        _log.warn('a session-end listener threw', {'error': '$err'});
      }
    }
  }

  // --- frames ----------------------------------------------------------------

  void _onMessage(_Session s, Object? data) {
    // EVERY MESSAGE EVENT CLEARS *PRECEDED BY*, BEFORE ANY DECODING (CANT-31
    // §4): a frame the decoder returns null for, one it throws on, text that
    // is not JSON, a binary message. Only a session-level `error` sets it
    // again.
    s.preceding = null;
    if (data is! String) {
      stats.undecodable++;
      _log.warn('server sent a binary message; ignored');
      notify();
      return;
    }
    final ServerFrame? f;
    try {
      f = ServerFrame.fromJson(jsonDecode(data));
    } catch (e) {
      stats.undecodable++;
      _log.warn('server frame the generated decoder refuses', {'error': '$e'});
      notify();
      return;
    }
    // CATENARY ANSWERED THIS CONTEXT (CANT-127's gate): a frame the generated
    // decoder accepted, an unknown tag included. One it refused is not.
    _cred.answered();
    switch (f) {
      case null:
        return; // an unknown tag: ignored, as every decoder is told to
      case ServerReady():
        _onReady(s, f);
      case Ping(:final id):
        _write(Pong(id: id).toJson());
      case Pong(:final id):
        _heartbeat.pong(id);
      case ServerAck():
        _waiters.remove(f.clientId)?.complete(f);
      case ServerError(:final clientId):
        if (clientId != null) {
          _waiters.remove(clientId)?.completeError(SendRefused(f));
        } else {
          s.preceding = f;
          _log.warn('server error', {'code': f.code.wire, 'message': f.message});
        }
      case ServerMessageFrame(:final message):
        _onLiveMessage(message);
      case ServerConversationFrame(:final conversation):
        // CANT-103: applied idempotently by id; moves no cursor.
        _enqueue(() async {
          final a = await _applyLiveWrite(LiveWrite(conversations: [conversation]));
          if (a != null) _emitApply(a);
        });
      case ServerUserFrame(:final user):
        _enqueue(() async {
          final a = await _applyLiveWrite(LiveWrite(users: [user]));
          if (a != null) _emitApply(a);
        });
      case ServerReceipt():
        _onReceipt(f);
      case ServerTyping():
        for (final fn in _typingFns.toList()) {
          fn(f);
        }
      case ServerResyncRequired():
        // A trigger (obligation 3), and nothing else: the cursor is the last
        // page's high water, so a catch-up from it covers the gap.
        stats.resyncs++;
        _catchup.trigger();
    }
    notify();
  }

  void _onReady(_Session s, ServerReady r) {
    s.readied = true;
    s.readyAt = now();
    s.intervalSec = r.heartbeatIntervalSec;
    _ready = true;
    _connecting = false;
    _sessionId = r.sessionId;
    _intervalSec = r.heartbeatIntervalSec;
    _missedPongLimit = r.missedPongLimit;
    _learnedIntervalSec = r.heartbeatIntervalSec;
    stats.readys++;
    serverWireVersion = r.wireVersion;

    // OBLIGATION 4, in the journal's own write order so it is checked against
    // the cursor every earlier write left. The server's log is behind the
    // cursor: a restore, or the wrong server. The epoch moves as the wipe is
    // decided, so a page requested from the old cursor is dropped rather than
    // landing on the empty store with none of the messages below its high
    // water.
    _enqueue(() async {
      final cursor = _journal.cursor;
      if (cursor == null || r.logSeq >= cursor) return;
      stats.discards++;
      if (faults.skipWipe) {
        _catchup.restartFromZero();
        return;
      }
      _epoch++;
      final a = await _journal.wipe();
      _wipes++;
      _emitApply(a);
    });
    // Every `ready` is a trigger: `resumed: true` is treated as false, which
    // is always correct.
    _catchup.trigger();
    _heartbeat.start(r.heartbeatIntervalSec, r.missedPongLimit);
    _log.info('ready', {
      'session_id': r.sessionId,
      'head': r.logSeq,
      'heartbeat_interval_sec': r.heartbeatIntervalSec,
      'missed_pong_limit': r.missedPongLimit,
    });
  }

  /// A `message` frame (obligation 2, CANT-103 rules 1 and 4): applied by id,
  /// moving no cursor — unless it names a conversation or an author the
  /// journal does not hold, when it is discarded, never buffered and never a
  /// placeholder, and pulls a catch-up that is guaranteed to return it.
  void _onLiveMessage(Message m) {
    _enqueue(() async {
      if (!_journal.holdsConversation(m.conversationId) || !_journal.holdsUser(m.authorId)) {
        stats.introductionDiscards++;
        _catchup.trigger();
        return;
      }
      final a = await _applyLiveWrite(LiveWrite(messages: [m]));
      if (a == null) return;
      stats.liveFrames++;
      _emitApply(a);
    });
  }

  /// CANT-175. A live write `SqliteJournal` refuses with `JournalStale` —
  /// another context wiped the shared journal since this one's mirror was last
  /// read — has already reloaded that mirror: what it carried is not lost,
  /// only unseen here. So this pulls a catch-up, the same trigger CANT-103's
  /// introduction discard above uses, rather than letting it fall through to
  /// `_enqueue`'s generic handling, which would only surface it as
  /// `journalError` and leave it for whatever trigger happens along next.
  /// `MemoryJournal` never throws this, so every other caller is unaffected.
  /// `skipStaleCatchUp` is the negative control: the write is left to fail.
  ///
  /// CANT-199. The refusal also MOVES THE EPOCH, before the trigger: it is a
  /// wipe this transport did not decide, and a `/sync` page already in flight
  /// was asked from the cursor the wipe destroyed. Without the move that page
  /// passes `applyPage`'s epoch check, lands on the reloaded, empty store —
  /// whose generation now matches what is stored, so nothing refuses it — and
  /// leaves the cursor above messages the store no longer holds. With it the
  /// page is dropped as "a wipe intervened" and the pass starts again from the
  /// stored cursor. `staleKeepsEpoch` is the negative control.
  Future<Applied?> _applyLiveWrite(LiveWrite write) async {
    if (faults.skipStaleCatchUp) return _journal.applyLive(write, faults);
    try {
      return await _journal.applyLive(write, faults);
    } on JournalStale {
      if (!faults.staleKeepsEpoch) _epoch++;
      _catchup.trigger();
      return null;
    }
  }

  /// A live `receipt`. RULING 4 → B: one naming this person pulls a catch-up,
  /// and `first_unread_seq` moves only when a page lands. One naming anybody
  /// else is a no-op for the store: `read_by` arrives on the re-emitted
  /// `message` frames (CANT-92). Nothing is inferred from the ABSENCE of a
  /// receipt — a device on another instance learns a mark only at `/sync`.
  void _onReceipt(ServerReceipt r) {
    if (_selfUserId != null && r.userId == _selfUserId) _catchup.trigger();
    _enqueue(() async => _emitApply(Applied(source: AppliedSource.live, cursor: _journal.cursor, receipts: [r])));
  }

  void _write(Map<String, dynamic> frame) {
    final conn = _conn;
    if (conn == null) return;
    try {
      conn.send(jsonEncode(frame));
    } catch (_) {
      // The close event that follows is where this session ends.
    }
  }

  // --- /sync -----------------------------------------------------------------

  @override
  Future<SyncResponse> fetchPage(int after) async {
    final used = await _cred.current();
    var page = await _syncOnce(after, used);
    if (page == null) {
      // Catenary's own 401: the reactive refresh, then ONE retry — not a loop.
      if (await _cred.onSyncUnauthorized(used)) page = await _syncOnce(after, await _cred.current());
      if (page == null) throw StateError('sync: 401 unauthorized');
    }
    return page;
  }

  /// One `GET /sync`; null for Catenary's own 401.
  Future<SyncResponse?> _syncOnce(int after, Credential cred) async {
    if (_connecting) {
      stats.syncsBeforeReady++;
      notify();
    }
    final limit = _syncLimit;
    final url = _httpBase.replace(
      path: '${_httpBase.path}/sync',
      queryParameters: {'after': '$after', if (limit != null && limit > 0) 'limit': '$limit'},
    );
    final cancel = Completer<void>();
    void abort() {
      if (!cancel.isCompleted) cancel.complete();
    }

    _inFlight.add(abort);
    final timer = timers.setTimeout(abort, _syncTimeoutMs);
    try {
      final res = await Future.any([
        _fetch(HttpExchange(method: 'GET', url: url, headers: {'Authorization': 'Bearer ${cred.accessToken}'}, abort: cancel.future)),
        cancel.future.then<HttpAnswer>((_) => throw const _SyncAborted()),
      ]);
      if (isCatenaryUnauthorized(res.status, res.body)) {
        // CATENARY'S OWN 401 IS CATENARY ANSWERING. A hop's 401 carries no
        // such body and falls through to the status branch, answering nothing.
        _cred.answered();
        return null;
      }
      if (res.status != 200) throw StateError('sync: HTTP ${res.status}');
      final page = SyncResponse.fromJson(jsonDecode(res.body));
      // A PAGE THE GENERATED DECODER ACCEPTED, not merely a 200: a captive
      // portal's interception page is a 200 and is not an answer from Catenary.
      _cred.answered();
      return page;
    } finally {
      timers.clearTimeout(timer);
      _inFlight.remove(abort);
    }
  }

  // --- the journal's write queue ---------------------------------------------

  Future<void> _enqueue(Future<void> Function() op) {
    _pendingWrites++;
    final p = _writes.then((_) => op()).then((_) {
      _journalError = null;
    }, onError: (Object e) {
      // SURFACED, NOT SWALLOWED: a write that did not land (a full disk, a
      // file another context holds) is in status until one does.
      final failure = JournalError('${e.runtimeType}', '$e');
      _journalError = failure;
      _log.warn('journal write failed', {'error': failure.message, 'name': failure.name});
    }).whenComplete(() {
      _pendingWrites--;
      notify();
    });
    _writes = p;
    return p;
  }

  @override
  Future<({int? cursor, int epoch})> settled() async {
    while (_pendingWrites > 0) {
      await _writes;
    }
    return (cursor: _journal.cursor, epoch: _epoch);
  }

  @override
  Future<bool> applyPage(SyncResponse page, int epoch) async {
    var applied = false;
    Object? failure;
    StackTrace? trace;
    await _enqueue(() async {
      if (epoch != _epoch) return;
      try {
        _emitApply(await _journal.applyPage(page, faults));
      } catch (e, st) {
        // NOT LOAD-BEARING, and kept so the rule is one rule (CANT-199): every
        // branch that sees `JournalStale` moves the epoch, which is what
        // source_test.dart checks. Nothing can observe this one. The rethrow
        // below fails the pass, only one pass runs at a time so no other page
        // is in flight to drop, and the retry reads the epoch again from
        // `settled()`.
        if (e is JournalStale && !faults.staleKeepsEpoch) _epoch++;
        failure = e;
        trace = st;
        rethrow;
      }
      applied = true;
    });
    // A page that did not land is a failed catch-up, retried on its backoff —
    // never "a wipe intervened", which would ask again at once, forever. A
    // page refused as stale is that too: the mirror has reloaded, and the
    // retry asks from the stored cursor.
    if (failure != null) Error.throwWithStackTrace(failure!, trace!);
    return applied;
  }

  /// Only ever called after the write it describes has landed.
  void _emitApply(Applied a) {
    for (final fn in _applyFns.toList()) {
      try {
        fn(a);
      } catch (err) {
        _log.warn('an apply listener threw', {'error': '$err'});
      }
    }
  }

  // --- lifecycle -------------------------------------------------------------

  void _onLifecycle(LifecycleEvent e) {
    switch (e) {
      case LifecycleEvent.online || LifecycleEvent.visible || LifecycleEvent.pageshow:
        _policyCatchUp();
        unawaited(_wake());
      case LifecycleEvent.resume:
        unawaited(_wake());
      case LifecycleEvent.offline || LifecycleEvent.hidden || LifecycleEvent.pagehide || LifecycleEvent.freeze:
        // Nothing to do: a suspended process's socket is found dead by the
        // wake detector when it runs again.
        break;
    }
  }

  /// RULING 5 → B: visible, online and pageshow each pull a trigger, at most
  /// once per `heartbeat_interval_sec` learned from the most recent `ready`.
  /// INERT BEFORE THE FIRST `ready`, and whenever no session is ready: the dial
  /// pulls its own trigger, so there is nothing for this to add. Withheld while
  /// a token is refused, like every `/sync` source.
  void _policyCatchUp() {
    final interval = _learnedIntervalSec;
    if (!_started || !_ready || interval == null) return;
    final at = now();
    final last = _lastPolicyAt;
    if (last != null && at - last < interval * 1000) return;
    if (_withheldSync()) return;
    _lastPolicyAt = at;
    _catchup.trigger();
  }

  @override
  void wake() => unawaited(_wake());

  /// A wake signal: `online`, visible, `pageshow`, `resume`, or the heartbeat's
  /// wall-clock jump. The explicit §1 refresh check first (never held); then an
  /// immediate ping when a socket is open, so a half-dead one is found at the
  /// next tick; or, when none is, ONE EARLY DIAL. The early dial does not reset
  /// the ramp, is limited to one per learned `heartbeat_interval_sec`, is not
  /// made before any `ready` (the backoff alone governs, and its worst cost is
  /// one ceiling wait), never shortens an `error{internal}` maximum wait, and
  /// is withheld while a token is refused. NO WAKE SIGNAL ENDS A TERMINAL STATE.
  Future<void> _wake() async {
    if (!_started || _terminal.kind != TerminalKind.none) return;
    final run = _runId;
    await _cred.refreshIfDue();
    if (!_alive(run)) return;
    if (_conn != null) {
      _heartbeat.pingNow();
      return;
    }
    // CANT-129 FIRST: a wake signal is a dial source, and while the token is
    // refused it is withheld and COUNTED, whichever other rule would also have
    // stood it down — the refused wait's own poll included, which waits at the
    // maximum and is never shortened by a wake signal.
    if (_cred.refused) {
      stats.dialsWithheld++;
      notify();
      return;
    }
    final w = _dialWait;
    final interval = _learnedIntervalSec;
    if (w == null || interval == null || w.atMax) return;
    final at = now();
    final last = _lastEarlyDialAt;
    if (last != null && at - last < interval * 1000) {
      stats.dialsWithheld++;
      notify();
      return;
    }
    _lastEarlyDialAt = at;
    w.resolve(_DialWaitResult.early);
  }

  bool _withheldSync() {
    if (!_cred.refused) return false;
    stats.syncsWithheld++;
    notify();
    return true;
  }

  // --- the heartbeat's and the catch-up's host ---------------------------------

  @override
  void ping(String id) => _write(Ping(id: id).toJson());

  @override
  void sever(int outstanding) {
    stats.heartbeatSevers++;
    _log.warn('severing: pings unanswered', {'outstanding': outstanding, 'missed_pong_limit': _missedPongLimit});
    final s = _session;
    if (s != null) _abandon(s);
  }

  @override
  void pingsSent() {
    stats.pingsSent++;
    notify();
  }

  @override
  void pongReceived(int? rttMs) {
    stats.pongsReceived++;
    if (rttMs != null) stats.lastRttMs = rttMs;
    notify();
  }

  @override
  bool sessionReady() => _ready;

  @override
  bool tokenRefused() => _cred.refused;

  @override
  Future<bool> withholdSync() => _cred.withholdSync();

  // --- terminal, stop, notify --------------------------------------------------

  /// The first terminal wins. Nothing is deleted: not the credential, not the
  /// journal.
  @override
  void terminal(Terminal t) {
    if (_terminal.kind != TerminalKind.none || t.kind == TerminalKind.none) return;
    _terminal = t;
    _log.warn('terminal: this client will not reconnect', {'kind': t.kind.name, 'reason': t.reason});
    _halt();
    notify();
  }

  void _halt() {
    _started = false;
    _runId++;
    for (final abort in _inFlight.toList()) {
      abort();
    }
    _unlisten?.call();
    _unlisten = null;
    _dialWait?.resolve(_DialWaitResult.stopped);
    _heartbeat.stop();
    final s = _session;
    if (s != null) {
      try {
        s.ws.close(1000, 'bye');
      } catch (_) {
        // Already closing.
      }
      _endSession(s, 1000, stopping: true);
    }
    _catchup.wake();
  }

  @override
  void notify() {
    if (_statusFns.isEmpty) return;
    final s = status();
    for (final fn in _statusFns.toList()) {
      try {
        fn(s);
      } catch (err) {
        _log.warn('a status listener threw', {'error': '$err'});
      }
    }
  }
}
