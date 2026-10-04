// What the app knows about its connection, as the views read it. The twin of
// `state.connection` in the web client (web/src/client-types.ts): the banner
// and the composer are functions of this and of nothing else.
//
// THESE STATES ARE THE CLIENT'S OWN. None of them is on the wire — the server
// has no opinion on whether a device is offline — so nothing here is a wire
// type, and nothing here claims more than the client can keep: a terminal
// client says nothing will send, and offers no retry that cannot work.

import 'package:flutter/foundation.dart';

enum ConnectionKind {
  /// A ready session.
  live,

  /// The session dropped and the client is redialing on its backoff.
  reconnecting,

  /// The device has no network. Messages composed now are queued.
  offline,

  /// A session is back and the journal is catching up.
  resyncing,

  /// The client will not reconnect (CANT-31 §6).
  terminal,
}

/// Why a terminal client stopped: a credential the server no longer accepts,
/// or a protocol this build cannot speak.
enum TerminalCause { credential, protocol }

@immutable
class ConnectionView {
  const ConnectionView({
    required this.kind,
    this.attempt,
    this.retryIn,
    this.synced,
    this.total,
    this.roomsPending,
    this.terminal,
    this.journalError,
  });

  const ConnectionView.live() : this(kind: ConnectionKind.live);

  final ConnectionKind kind;

  /// Reconnecting: which dial this is, and how long until it.
  final int? attempt;
  final Duration? retryIn;

  /// Resyncing: messages held of the total the server announced. Null until
  /// there is a number to show — a `0 / 0` would be a claim.
  final int? synced;
  final int? total;
  final int? roomsPending;

  final TerminalCause? terminal;

  /// The name of the error the journal's last write failed with, shown beside
  /// any other banner until a write lands.
  final String? journalError;

  /// The app never pretends a message left the building.
  bool get offline => kind != ConnectionKind.live;

  /// Nothing composed now will drain until a relaunch or a re-enrollment, so
  /// the composer does not offer to queue it.
  bool get isTerminal => kind == ConnectionKind.terminal;
}
