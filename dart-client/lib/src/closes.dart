/// CANT-31 §4, the close table — mirrors web/src/transport/closes.ts and
/// `classifyClose` in internal/client/terminal.go. Pure, and the part the
/// shared decision vectors point a runner at.
///
/// The table is the record's (docs/decisions/cant-31-refresh-and-terminal-reconnect.md
/// §4), and only that. Where this file and the record disagree, fix the record
/// first.
library;

import 'package:catenary_wire/catenary_wire.dart';

enum CloseVerdict {
  reconnect('reconnect'),
  reconnectAtMaximum('reconnect_at_maximum'),
  terminalProtocol('terminal_protocol');

  const CloseVerdict(this.wire);

  /// The verdict as the decision vectors and the other clients spell it.
  final String wire;
}

/// Close codes with a reading in the table. `4001` is the hub's
/// `StatusRevoked`, spelled as a number because a client does not import the
/// server; the record is what pins it.
const closeRevoked = 4001;
const closePolicyViolation = 1008;

/// The key a close is counted under in `stats.closeStatuses`, which is Go's
/// key: the code the peer sent, or `-1` when no close frame arrived at all. An
/// abnormal closure reported as `1006` and a close frame with no status
/// reported as `1005` are both "no status", and both are counted under `-1` so
/// a mixed soak report is one map.
int closeStatusKey(int? code) => code == null || code == 1005 || code == 1006 ? -1 : code;

/// Record §4. `code` is the close status the peer sent, null for none.
/// `preceding` is the LAST frame before the close, and only if it was an
/// `error` carrying no `client_id` — null otherwise, and null after ANY later
/// message event, decodable or not.
///
/// A BARE 1008 IS A CLIENT BUG: replaying the same bytes walks into the same
/// close. `error{internal}` is never terminal, whatever its `retryable` says.
/// EVERYTHING NOT LISTED RECONNECTS, including a code a later server adds — and
/// an error code this build does not know, which the generated decoder hands
/// over as the sentinel `unknown`: a client that reconnects when it should
/// have stopped wastes a dial, and one that stops when it should have
/// reconnected has logged its person out.
({CloseVerdict verdict, String reason}) classifyClose(int? code, ServerError? preceding) {
  final status = closeStatusKey(code);
  if (status == closeRevoked) {
    return (verdict: CloseVerdict.terminalProtocol, reason: 'close 4001: the credential behind the session was revoked');
  }
  if (status == closePolicyViolation) {
    if (preceding == null) {
      return (
        verdict: CloseVerdict.terminalProtocol,
        reason: 'close 1008, bare: the server could not accept what this client wrote',
      );
    }
    switch (preceding.code) {
      case ErrorCode.unauthorized || ErrorCode.wireVersionUnsupported:
        return (verdict: CloseVerdict.terminalProtocol, reason: 'close 1008 after error{${preceding.code.wire}}');
      case ErrorCode.internal:
        // `retryable` IS NOT CONSULTED. An unclassified server failure arrives
        // as `internal` with that flag biased toward true, and a permanent
        // server fault must not stop every client and keep them stopped after
        // the fix.
        return (verdict: CloseVerdict.reconnectAtMaximum, reason: '');
      default:
        break;
    }
  }
  return (verdict: CloseVerdict.reconnect, reason: '');
}
