// A real transport that reports a ready session, so the outbox offers work to
// its uploader and accepts a picked file at once. Nothing else is changed.

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' show ClientRead, ClientSend, ClientTyping, ServerAck, ServerTyping;

final class ReadyTransport implements Transport {
  ReadyTransport(this.inner, {this.terminal});

  final Transport inner;

  /// When set, the status says the transport is terminal as well as ready.
  final Terminal? terminal;

  @override
  TransportStatus status() {
    final s = inner.status();
    return TransportStatus(
      terminal: terminal ?? s.terminal,
      refreshHold: s.refreshHold,
      tokenRefused: s.tokenRefused,
      nextRefreshAt: s.nextRefreshAt,
      connected: true,
      ready: true,
      sessionId: 's-1',
      heartbeatIntervalSec: s.heartbeatIntervalSec,
      missedPongLimit: s.missedPongLimit,
      caughtUp: s.caughtUp,
      cursor: s.cursor,
      attempt: s.attempt,
      nextDialAt: s.nextDialAt,
      stats: s.stats,
      messages: s.messages,
      wipes: s.wipes,
      headSeqTotal: s.headSeqTotal,
      journalError: s.journalError,
    );
  }

  @override
  void start() => inner.start();
  @override
  void stop() => inner.stop();
  @override
  void Function() subscribe(void Function(TransportStatus s) fn) => inner.subscribe(fn);
  @override
  Future<ServerAck> send(ClientSend f) => inner.send(f);
  @override
  void read(ClientRead f) => inner.read(f);
  @override
  void typing(ClientTyping f) => inner.typing(f);
  @override
  void catchUp() => inner.catchUp();
  @override
  void retryNow() => inner.retryNow();
  @override
  Future<void> refreshIfDue() => inner.refreshIfDue();
  @override
  void Function() onSessionEnd(void Function(SessionEnd e) fn) => inner.onSessionEnd(fn);
  @override
  void Function() onApply(void Function(Applied e) fn) => inner.onApply(fn);
  @override
  JournalSnapshot snapshot() => inner.snapshot();
  @override
  void Function() onTyping(void Function(ServerTyping f) fn) => inner.onTyping(fn);
}

