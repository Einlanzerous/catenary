/// What bounds the chain — mirrors web/src/transport/hold.ts and
/// internal/client/hold.go (CANT-127).
///
/// CANT-31's record §3, the bound (docs/decisions/cant-31-refresh-and-terminal-reconnect.md).
/// Where this file and the record disagree, fix the record first.
///
/// THE DECISIONS ARE PURE: every function here is a function of durable state
/// (the chain's length and its `last_sent_at` stamp), this context's latest
/// Catenary answer, and ONE wall-clock reading, all passed in. No clock and no
/// I/O inside, so the shared decision vectors can pin them against Go's and
/// TypeScript's, and a cold start decides exactly as the context that wrote
/// the credential would have. Times are wall-clock milliseconds since the
/// epoch; durations are milliseconds and are NEVER ROUNDED.
library;

import 'dart:math';

import 'credential.dart';

/// The 5 s in `min(cap, 5 s × 2^(n−1))`: one link's wait, which is also the
/// dial backoff's ceiling, so a chain of one costs no more than the dial rate.
const refreshBackoffBaseMs = 5000;

/// The 15 minutes CANT-127 ruling 2 picked.
const refreshBackoffCapMs = 15 * 60000;

/// The length at which a chain is worth one WARN.
const chainWarnLength = 64;

/// `min(15 min, 5 s × 2^(n−1))` for a chain of `links`, counted from the send;
/// 0 for a settled credential. Doubled rather than exponentiated, as Go does,
/// so a long chain cannot overflow on its way to a cap it passed at eight links.
int refreshDelay(int links) {
  if (links <= 0) return 0;
  var d = refreshBackoffBaseMs;
  for (var i = 1; i < links; i++) {
    if (d >= refreshBackoffCapMs) break;
    d *= 2;
  }
  return min(d, refreshBackoffCapMs);
}

/// `last_sent_at` as the suppressors must read it (Go's `readStamp`): ONE RULE
/// FOR ABSENT. A missing stamp and one in the future — a clock set backwards —
/// are both null, and null reads the same way everywhere: the delay has
/// elapsed, and any Catenary answer opens the gate.
num? readStamp(num? lastSentAt, num now) => lastSentAt == null || lastSentAt > now ? null : lastSentAt;

/// Record §3's gate (Go's `gateOpen`), as a comparison rather than a signal.
/// `stamp` is already read through `readStamp`. A context with no answer yet is
/// CLOSED either way; an absent stamp is opened by any answer; otherwise LATER
/// THAN IS STRICT, and an answer at the same instant as the send is closed.
bool gateOpen(num? answeredAt, num? stamp) {
  if (answeredAt == null) return false;
  if (stamp == null) return true;
  return answeredAt > stamp;
}

/// The two suppressors, in order (Go's `refreshHoldAt`): a settled credential
/// is never held; the gate holds an unsettled one Catenary has not answered
/// since the last send; the delay holds one whose `last_sent_at + delay` has
/// not arrived. `lastSentAt` is the persisted stamp, raw; `answeredAt` is when
/// Catenary last answered THIS context, null for never.
RefreshHold refreshHoldAt({required int links, required num? lastSentAt, required num? answeredAt, required num now}) {
  if (links <= 0) return RefreshHold.none;
  final sent = readStamp(lastSentAt, now);
  if (!gateOpen(answeredAt, sent)) return RefreshHold.unreachable;
  if (sent != null && now < sent + refreshDelay(links)) return RefreshHold.backoff;
  return RefreshHold.none;
}

/// `Status.NextRefreshAt`: `last_sent_at + min(15 min, 5 s × 2^(n−1))`, the
/// time the backoff next allows an automatic refresh — and so also the time
/// CANT-129's one `/sync` goes out. Null for an empty chain or an absent stamp.
/// It is not "when the next refresh happens": while the GATE holds, that has no
/// answer.
num? nextRefreshAt(int links, num? lastSentAt, num now) {
  if (links <= 0) return null;
  final sent = readStamp(lastSentAt, now);
  return sent == null ? null : sent + refreshDelay(links);
}
