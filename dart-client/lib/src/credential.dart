/// The credential seam — mirrors web/src/transport/credential.ts, which
/// mirrors the credential half of internal/client/journal.go (`Credential`)
/// and the hooks refresh.go, hold.go and refused.go hang on `Client.Run`,
/// `catchUpLoop` and `fetchWith`.
///
/// `HeldCredential` presents the credential as held, as Go's
/// `Config.Refresh: false` does: it never refreshes, never refuses, never
/// withholds — what the rigs run. The refreshing implementation (CANT-31
/// §1–§6, CANT-127's hold and CANT-129's refused wait, over a durable store)
/// is the credential layer's. The transport calls every hook below at the
/// point Go calls its counterpart.
library;

import 'dart:async';
import 'dart:convert';

import 'package:catenary_wire/catenary_wire.dart';

import 'terminal.dart';

/// What the transport presents. Re-read at every use and never cached, so a
/// rotation by this transport or another context is what the next dial and the
/// next `/sync` carry (Go's `Client.credential`).
final class Credential {
  const Credential({required this.userId, required this.deviceId, required this.accessToken});

  /// Who this device speaks as — `EnrollResponse.user_id`. The transport needs
  /// it for exactly one rule: a `receipt` naming it is this person's own.
  final Uuid userId;

  /// `ClientHello.device_id`. A rotation never changes it.
  final Uuid deviceId;

  /// Rides the upgrade as `catenary.token.<token>` and `/sync` as a bearer.
  /// NEVER LOGGED, and never in a status field.
  final String accessToken;
}

/// CANT-127's `RefreshHold`: why no automatic refresh is being made.
enum RefreshHold { none, unreachable, backoff }

/// What the credential layer reports into `TransportStatus`. Stats use Go's
/// `Stats` names, camelCased.
final class CredentialStatus {
  const CredentialStatus({
    this.refreshHold = RefreshHold.none,
    this.nextRefreshAt,
    this.refreshes = 0,
    this.refreshesSkipped = 0,
    this.refreshErrors = 0,
    this.refreshWalkBacks = 0,
    this.chainLength = 0,
    this.refreshesHeldUnreachable = 0,
    this.refreshesHeldBackoff = 0,
  });

  final RefreshHold refreshHold;

  /// `last_sent_at + min(15 min, 5 s × 2^(n−1))`, wall-clock ms; null for an
  /// empty chain or an absent stamp.
  final num? nextRefreshAt;
  final int refreshes;
  final int refreshesSkipped;
  final int refreshErrors;
  final int refreshWalkBacks;
  final int chainLength;
  final int refreshesHeldUnreachable;
  final int refreshesHeldBackoff;
}

/// What the transport lends the credential layer.
abstract interface class CredentialHost {
  /// Enter a terminal state (§5's credential terminal). The first wins.
  void terminal(Terminal t);

  /// Something the status reports changed.
  void notify();
}

/// The seam the credential layer fills in. Every method is called where Go
/// calls its counterpart; none may throw — a failure is the credential layer's
/// to count and log, and the transport carries on as Go's `Run` does after a
/// failed proactive refresh.
abstract interface class CredentialSeam {
  void attach(CredentialHost host);

  /// The pair to present now.
  Future<Credential> current();

  /// `waitWhileRefused(withheldDial)`'s predicate: true withholds this dial.
  /// Polled at the dial cadence (the backoff ceiling), never a timer.
  Future<bool> withholdDial();

  /// `waitWhileRefused(withheldSync)`'s predicate: true withholds this `/sync`.
  Future<bool> withholdSync();

  /// CANT-129: Catenary's own 401 on `/sync` has refused the token held now.
  /// Read synchronously for the status and for every trigger the refused wait
  /// suppresses (wake signals, the policy catch-up, `catchUp()`, `retryNow()`).
  bool get refused;

  /// The proactive refresh before a dial (`refreshDueWhenAllowed`): bounded by
  /// CANT-127's suppressors.
  Future<void> beforeDial();

  /// The explicit §1 check (`RefreshIfDue`): never held. Also run on every wake
  /// signal before the dial it may make.
  Future<void> refreshIfDue();

  /// Catenary's own 401 on `/sync` for `presented` (`markRefused` plus
  /// `refreshAfter401`). Completes true when the request should be retried
  /// ONCE with the pair `current()` now returns.
  Future<bool> onSyncUnauthorized(Credential presented);

  /// Catenary answered this context — a decoded frame, a decoded `/sync` page,
  /// or its own 401 (`markAnswered`, CANT-127's gate).
  void answered();
  CredentialStatus status();
}

/// The credential presented as held: no refresh, no refusal, no hold. Go's
/// `Refresh: false`, which is also what the rigs run.
final class HeldCredential implements CredentialSeam {
  HeldCredential(Credential credential) : _read = (() => credential);

  /// Re-read at every use: a rig that rotates the pair itself hands this the
  /// read.
  HeldCredential.reading(this._read);

  final FutureOr<Credential> Function() _read;

  @override
  void attach(CredentialHost host) {}

  @override
  Future<Credential> current() async => _read();

  @override
  Future<bool> withholdDial() async => false;

  @override
  Future<bool> withholdSync() async => false;

  @override
  bool get refused => false;

  @override
  Future<void> beforeDial() async {}

  @override
  Future<void> refreshIfDue() async {}

  @override
  Future<bool> onSyncUnauthorized(Credential presented) async => false;

  @override
  void answered() {}

  @override
  CredentialStatus status() => const CredentialStatus();
}

/// Catenary's own 401: the status AND the body `{"code":"unauthorized"}`. A
/// 401 from a hop in front carries neither body nor meaning (CANT-31 §5).
bool isCatenaryUnauthorized(int status, String body) {
  if (status != 401) return false;
  try {
    final v = jsonDecode(body);
    return v is Map && v['code'] == 'unauthorized';
  } on FormatException {
    return false;
  }
}
