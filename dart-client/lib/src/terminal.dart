/// When a client must stop — mirrors web/src/transport/terminal.ts and
/// internal/client/terminal.go (CANT-123).
///
/// CANT-31's record §6 (docs/decisions/cant-31-refresh-and-terminal-reconnect.md).
/// Two terminal states that end differently and tell the person different
/// things. NEITHER ENDS ON A TIMER OR ON A NETWORK CHANGE, and NOTHING IS
/// DELETED: the credential and the journal both survive, so a false terminal is
/// recovered by a relaunch — a new Transport over the same stores — and a true
/// one loses nothing, because the server refuses that credential for ever
/// anyway. Where this file and the record disagree, fix the record first.
library;

/// `none`: running, or stopped for any other reason, however long the backoff
/// has grown. `credential`: Catenary itself refused the stored refresh token
/// (§5; entered by the credential layer). `protocol`: close `4001`, or a `1008`
/// that §4 makes permanent; it ends on relaunch.
enum TerminalKind { none, credential, protocol }

final class Terminal {
  const Terminal(this.kind, this.reason);

  static const not = Terminal(TerminalKind.none, '');

  final TerminalKind kind;

  /// The observation that put the client there. Never carries a token.
  final String reason;

  Map<String, Object?> toJson() => {'kind': kind.name, 'reason': reason};
}
