/// A token Catenary has refused is presented once per refresh hold — mirrors
/// web/src/transport/refused.ts and internal/client/refused.go (CANT-129).
///
/// CANT-31's record §5, the row beneath `401 on /sync`
/// (docs/decisions/cant-31-refresh-and-terminal-reconnect.md). Where this file
/// and the record disagree, fix the record first.
///
/// THE RULE, cited rather than argued: once Catenary's own 401 on `/sync` has
/// refused the held access token, the client does not dial with it, and
/// presents it in exactly one kind of request — one `/sync` each time a refresh
/// hold ends. That one request is the ENGINE, not the leak: its answer is what
/// reopens CANT-127's gate and drives the reactive refresh.
///
/// PURE, as hold.dart is: the wait is a PREDICATE over the persisted chain, its
/// stamp and one wall-clock reading, re-read at the dial cadence by the caller
/// — never a timer computed once, which record §1 forbids.
library;

import 'hold.dart';

/// The wait that bounds the one `/sync` (Go's `refusedHoldAt`): hold while the
/// pair's access token is the refused one and
/// `last_sent_at + min(15 min, 5 s × 2^(n−1))` has not arrived. It ends on any
/// of four things, every one read off state: the pair changes, the chain
/// collapses, the stamp goes absent, or the clock passes the deadline. ABSENT
/// READS AS ELAPSED, exactly as hold.dart reads it.
bool refusedHoldAt({required bool refused, required int links, required num? lastSentAt, required num now}) {
  if (!refused || links <= 0) return false;
  final sent = readStamp(lastSentAt, now);
  return sent != null && now < sent + refreshDelay(links);
}
