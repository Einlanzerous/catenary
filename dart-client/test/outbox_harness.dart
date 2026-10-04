/// The outbox's test server and context — the twin of
/// web/src/outbox/transports.ts's `ScriptedTransport` and the harness at the
/// top of web/outbox.test.ts.
///
/// `ScriptedTransport` is the tests' server. Its dedup follows the server's
/// stated semantics — unique per (author, client_id), a replay answered with
/// the ORIGINAL ids and `duplicate: true` — and it answers only when told to,
/// so a test decides exactly when an ack, a refusal, a record or a close
/// arrives. It is not the oracle; the faults are what keep a test honest
/// against it.
library;

import 'dart:async';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';

/// One author's view of the server's messages table.
final class ScriptedServer {
  final received = <ClientSend>[];
  final stored = <Uuid, ServerAck>{};
  var _logSeq = 100;
  final _seqs = <Uuid, int>{};

  ServerAck commit(Uuid clientId, Uuid conversationId) {
    final held = stored[clientId];
    if (held != null) {
      return ServerAck(clientId: held.clientId, messageId: held.messageId, conversationId: held.conversationId, seq: held.seq, logSeq: held.logSeq, at: held.at, duplicate: true);
    }
    final seq = (_seqs[conversationId] ?? 40) + 1;
    _seqs[conversationId] = seq;
    final logSeq = ++_logSeq;
    return stored[clientId] = ServerAck(
      clientId: clientId,
      messageId: uuid(900000 + logSeq),
      conversationId: conversationId,
      seq: seq,
      logSeq: logSeq,
      // An hour after anything a test composes, so the ack's time is never the
      // entry's own.
      at: DateTime.utc(2026, 9, 29, 13, 0, logSeq - 100).toIso8601String(),
    );
  }
}

final class ScriptedTransport implements OutboxTransport {
  ScriptedTransport([ScriptedServer? server]) : server = server ?? ScriptedServer();

  /// The "server" — shared between transports that stand for one account's
  /// sessions in two contexts.
  final ScriptedServer server;

  /// Every frame written on this transport, across sessions, in order.
  final frames = <ClientSend>[];

  /// Called synchronously as each frame is written.
  void Function(ClientSend frame)? onFrame;

  var _ready = false;
  final _listeners = <void Function(OutboxTransportEvent)>{};

  @override
  bool get isReady => _ready;

  @override
  void sendFrame(ClientSend frame) {
    if (!_ready) throw StateError('ScriptedTransport: frame written with no ready session');
    frames.add(frame);
    server.received.add(frame);
    onFrame?.call(frame);
  }

  @override
  void Function() subscribe(void Function(OutboxTransportEvent event) listener) {
    _listeners.add(listener);
    return () => _listeners.remove(listener);
  }

  void emit(OutboxTransportEvent event) {
    for (final l in _listeners.toList()) {
      l(event);
    }
  }

  void open() {
    _ready = true;
    emit(const SessionReady());
  }

  void close({bool bare1008 = false, bool terminal = false}) {
    _ready = false;
    emit(SessionClosed(bare1008: bare1008, terminal: terminal));
  }

  /// Commit (or replay) a send and answer it with the server's ack.
  ServerAck ack(Uuid clientId, [Uuid? conversationId]) {
    final ack = server.commit(clientId, conversationId ?? server.received.lastWhere((f) => f.clientId == clientId).conversationId);
    emit(SendAcked(ack));
    return ack;
  }

  void refuse(Uuid clientId, ErrorCode code, String message, {required bool retryable, int? retryAfterSec}) =>
      emit(SendErrored(ServerError(code: code, message: message, retryable: retryable, clientId: clientId, retryAfterSec: retryAfterSec)));

  /// The record for a committed send arrives — a `message` frame or `/sync`.
  void deliver(Uuid clientId) => emit(RecordHeld(clientId));

  List<ClientSend> framesFor(Uuid clientId) => frames.where((f) => f.clientId == clientId).toList();
}

/// Fails a future that does not settle in time — a store whose commit never
/// comes must fail a criterion, not hang the run. Real time, not the fake
/// clock's.
Future<T> within<T>(String what, Future<T> p, [Duration limit = const Duration(milliseconds: 1500)]) =>
    p.timeout(limit, onTimeout: () => throw TimeoutException('$what: did not settle in ${limit.inMilliseconds}ms'));

/// One context: an outbox over a store and a scripted session.
final class Ctx {
  Ctx._(this.store, this.transport, this.clock);

  final OutboxStore store;
  final ScriptedTransport transport;
  final FakeClock clock;
  late final Outbox outbox;

  OutboxItem? item(Uuid clientId) => outbox.view().where((i) => i.entry.clientId == clientId).firstOrNull;

  void close() => outbox.close();

  Future<OutboxEntry> composeText([String text = 'hello', Uuid? conversationId]) =>
      within('compose', outbox.compose(OutboxDraft(conversationId: conversationId ?? conv, text: text)));

  Future<OutboxEntry?> stored(Uuid clientId) async => (await store.list()).where((e) => e.clientId == clientId).firstOrNull;
}

/// The account every context composes as unless it says otherwise.
final account = me;

Future<Ctx> context({
  OutboxStore? store,
  ScriptedTransport? transport,
  OutboxFaults faults = OutboxFaults.none,
  Object? accountId = _unset,
  DrainLock? lock,
  FakeClock? clock,
  Uuid Function()? mintId,
  void Function(List<OutboxItem> items)? onView,
  // Long, so a criterion that advances the clock by minutes is not a thousand
  // re-reads. The criterion about the re-read sets it.
  num rereadMs = 3600000,
}) async {
  final ctx = Ctx._(store ?? MemoryOutboxStore(), transport ?? ScriptedTransport(), clock ?? FakeClock());
  ctx.outbox = await Outbox.open(
    store: ctx.store,
    transport: ctx.transport,
    accountId: identical(accountId, _unset) ? account : accountId as Uuid?,
    lock: lock,
    now: ctx.clock.now,
    timers: ctx.clock,
    random: () => 0.5,
    mintId: mintId,
    faults: faults,
    onChange: onView,
    rereadMs: rereadMs,
  );
  addTearDown(ctx.close);
  return ctx;
}

const _unset = Object();
