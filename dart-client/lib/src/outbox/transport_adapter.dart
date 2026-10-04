/// The outbox's `OutboxTransport` over the transport — mirrors
/// web/src/outbox/transport-adapter.ts (CANT-36 row 3).
///
/// THIS IS THE OUTBOX'S ADAPTER, NOT THE TRANSPORT'S: the transport implements
/// none of `OutboxTransport` and owns no queue. Each outbox event is sourced
/// from exactly one transport call, and the adapter decides nothing the
/// transport has already decided:
///
///   SessionReady     `subscribe()`: a `TransportStatus` whose `ready` has gone
///                    true. `status().ready` is read once, at construction, for
///                    the value before the first change.
///   SessionClosed    `onSessionEnd`: `SessionEnd.bare1008` is the transport's
///                    classification of CANT-31 §4's close, consumed as given
///                    and never re-derived from a close code here.
///   SendAcked /      what `send()` completes with. `SendRefused` carries the
///   SendErrored      server's `error` frame; every other failure means the
///                    frame was not answered, and the `SessionClosed` that
///                    follows is what the outbox acts on.
///   RecordHeld       `onApply`: every record an `Applied` carries — a live
///                    `message` frame or a `/sync` page — and, for a listener
///                    that attaches after some were applied, the records
///                    `snapshot()` already holds.
///   Bootstrapped     `onApply` with `wiped` (CANT-24 obligation 4).
///
/// Only a record carrying a `clientId` can settle an entry: the server echoes
/// it to its author alone, so every other record is passed over here.
library;

import 'package:catenary_wire/catenary_wire.dart';

import '../journal.dart';
import '../seams.dart';
import '../status.dart';
import '../terminal.dart';
import '../transport.dart';
import '../closes.dart';
import 'types.dart';

final class TransportOutbox implements OutboxTransport {
  TransportOutbox(this._transport, [this._log = const SilentLogger()])
      // Read once. From here on `ready` moves only with what `subscribe()`
      // delivers.
      : _ready = _transport.status().ready {
    _detach = [
      _transport.subscribe(_onStatus),
      _transport.onSessionEnd((e) => _emit(SessionClosed(
            bare1008: e.bare1008,
            // The transport's verdict on this close, or a terminal state it
            // had already entered. Informational: the outbox deletes nothing
            // on either (CANT-31 §6).
            terminal: e.verdict == CloseVerdict.terminalProtocol || _transport.status().terminal.kind != TerminalKind.none,
          ))),
      _transport.onApply(_onApply),
    ];
  }

  final Transport _transport;
  final Logger _log;
  bool _ready;
  final _listeners = <void Function(OutboxTransportEvent)>{};
  late final List<void Function()> _detach;

  @override
  bool get isReady => _ready;

  @override
  void sendFrame(ClientSend frame) {
    _transport.send(frame).then(
      (ack) => _emit(SendAcked(ack)),
      onError: (Object err) {
        if (err is SendRefused) {
          _emit(SendErrored(err.frame));
          return;
        }
        // `SessionEnded`: the outcome is unknown, and the session's close is
        // on its way — the outbox keeps the entry pending and resends it under
        // the same clientId. `NotConnected`: nothing was written, which
        // happens only between a session's end and its close event, so the
        // same event returns the entry to the drain. `SendInFlight`: the
        // outbox never writes a clientId twice on one session. None is a
        // refusal, and none may fail an entry.
        _log.info('outbox send unanswered', {'client_id': frame.clientId, 'reason': '${err.runtimeType}'});
      },
    );
  }

  /// A listener attaching late is handed every record already held with a
  /// clientId, so an entry whose record landed before the outbox loaded
  /// settles at once rather than being resent.
  @override
  void Function() subscribe(void Function(OutboxTransportEvent event) listener) {
    _listeners.add(listener);
    for (final m in _transport.snapshot().messages) {
      if (m.clientId == null) continue;
      if (!_listeners.contains(listener)) break;
      listener(RecordHeld(m.clientId));
    }
    return () => _listeners.remove(listener);
  }

  /// Detaches from the transport. Does not stop it, which the caller owns.
  void close() {
    for (final d in _detach) {
      d();
    }
    _listeners.clear();
  }

  void _onStatus(TransportStatus s) {
    final was = _ready;
    _ready = s.ready;
    if (s.ready && !was) _emit(const SessionReady());
  }

  void _onApply(Applied a) {
    if (a.wiped) _emit(const Bootstrapped());
    for (final m in a.messages) {
      if (m.clientId != null) _emit(RecordHeld(m.clientId));
    }
  }

  void _emit(OutboxTransportEvent event) {
    for (final l in _listeners.toList()) {
      l(event);
    }
  }
}

/// A session that never exists: it is never ready, so every entry composed is
/// kept durably, reads QUEUED, and is never shown SENT by an ack nobody sent.
/// For whatever needs an outbox with no transport at all.
final class NullTransport implements OutboxTransport {
  const NullTransport();

  @override
  bool get isReady => false;

  @override
  void sendFrame(ClientSend frame) => throw StateError('NullTransport has no session');

  @override
  void Function() subscribe(void Function(OutboxTransportEvent event) listener) => () {};
}
