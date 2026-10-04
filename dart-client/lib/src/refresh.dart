/// Refreshing before you need to, and also when you are refused — mirrors
/// web/src/transport/refresh.ts and internal/client/refresh.go (CANT-124,
/// CANT-126), with the call sites internal/client/hold.go (CANT-127) and
/// internal/client/refused.go (CANT-129) hang on `Client.Run`, `catchUpLoop`
/// and `fetchWith`.
///
/// CANT-31's record §1–§3 and §5 (docs/decisions/cant-31-refresh-and-terminal-reconnect.md),
/// implemented for the third time. Where this file and the record disagree, fix
/// the record first and then all three.
///
/// `RefreshingCredential` is the `CredentialSeam` the transport core calls
/// (credential.dart): the transport decides WHEN (before a dial, on Catenary's
/// own 401 on `/sync`, on a wake signal, at the dial cadence while a token is
/// refused) and this decides WHETHER and WHAT. The decisions themselves are
/// pure and live beside it — `refreshThreshold` and `refreshDue` here, the gate
/// and the delay in hold.dart, the refused wait in refused.dart — so the shared
/// decision vectors can pin them without a clock or a store.
///
/// SINGLE-FLIGHT PER CREDENTIAL, NOT PER CONTEXT: every refresh runs under the
/// lock `catenary.credential.<device_id>` (lock.dart) and re-reads the
/// persisted credential under it, so a context that waited behind another's
/// refresh finds the pair already rotated and skips. Two refreshes of one pair
/// is not a wasted round trip — outside the grace window the second is a
/// replay, and a replay revokes the device (CANT-29).
///
/// TOKENS NEVER REACH A LOG LINE OR A STATUS FIELD, and neither does a response
/// body, which is the one place a server could echo one.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:catenary_wire/catenary_wire.dart';

import 'credential.dart';
import 'credential_store.dart';
import 'faults.dart';
import 'hold.dart';
import 'io_seams.dart';
import 'lock.dart';
import 'refused.dart';
import 'seams.dart';
import 'terminal.dart';

/// The 60 s in `max(60 s, ⅓ of the served lifetime)`.
const refreshFloorMs = 60000;

/// Bounds one `POST /refresh`.
const _refreshTimeoutMs = 30000;

/// 32 CSPRNG bytes, which base64url without padding renders as the wire's
/// 43-character `Token` — exactly as the server mints its own.
const proposalBytes = 32;

/// How often one refresh may answer `fresh_proposal` with another proposal.
const maxFreshProposals = 3;

/// Caps the wait a `fresh_proposal` asks for; the lock is held across it.
const _maxRetryAfterMs = 5000;

/// Requests one refresh may send beyond one per link: the answers that send
/// the walk back up (`present_proposal`) or round again (`fresh_proposal`).
const _walkSlack = 8;

// --- the pure decisions (record §1) -------------------------------------------

/// `max(60 s, ⅓ of the served lifetime)`, in ms and NOT ROUNDED — a 100 s
/// lifetime's third is 33,333.33… ms, as Go's nanosecond duration has it. The
/// served lifetime is measured on the SERVER's clock at both ends, so a wrong
/// device clock cannot stretch it. A pair whose issue time was never learned
/// gets the floor, not a guessed lifetime.
num refreshThreshold({required num? accessIssuedAt, required num? accessExpiresAt}) {
  if (accessIssuedAt == null || accessExpiresAt == null) return refreshFloorMs;
  return max(refreshFloorMs, (accessExpiresAt - accessIssuedAt) / 3);
}

/// Whether less than the threshold remains on the access token, as of
/// `deviceNow` (the device's WALL clock, never a monotonic one) corrected by the
/// offset persisted with the credential. A pure function of durable state and
/// one clock reading, so a cold start decides as the writer would have.
bool refreshDue({required num? accessIssuedAt, required num? accessExpiresAt, required num clockOffsetMs, required num deviceNow}) {
  if (accessExpiresAt == null) return false; // no expiry learned: the reactive path is the net
  final serverNow = deviceNow + clockOffsetMs;
  return accessExpiresAt - serverNow < refreshThreshold(accessIssuedAt: accessIssuedAt, accessExpiresAt: accessExpiresAt);
}

bool _due(StoredCredential c, num deviceNow) =>
    refreshDue(accessIssuedAt: c.accessIssuedAt, accessExpiresAt: c.accessExpiresAt, clockOffsetMs: c.clockOffsetMs, deviceNow: deviceNow);

/// A successor secret: 32 random bytes in the wire's Token shape, NEVER DERIVED
/// FROM ANYTHING. Throws, sending and writing nothing, on a short draw.
String mintProposal(RandomBytes random) {
  final raw = random(proposalBytes);
  if (raw.length != proposalBytes) throw StateError('refresh: the generator could not fill 32 bytes');
  return base64Url.encode(raw).replaceAll('=', '');
}

// --- the layer ------------------------------------------------------------------

/// What one refresh came to. `held` is a suppressor's stand-down (CANT-127) and
/// is neither a skip nor a failure; `skipped` is another context having rotated
/// the pair already; `failed` is an attempt that settled nothing.
enum RefreshOutcome { notDue, refreshed, skipped, held, failed, terminal, killed }

enum _Need { needed, notNeeded, held }

enum _Kind { ok, unauthorized, moved, failed, killed, presentProposal, freshProposal }

/// What one step of a refresh came to: a rotated pair, a proposal, or why not.
final class _Step {
  const _Step(this.kind, {this.next, this.proposal, this.afterMs = 0});

  final _Kind kind;
  final StoredCredential? next;
  final String? proposal;
  final num afterMs;
}

final class RefreshingCredential implements CredentialSeam {
  RefreshingCredential({
    required String baseUrl,
    required CredentialStore store,
    required Lock lock,
    HttpFetch? fetch,
    Clock? now,
    Timers? timers,
    RandomBytes? random,
    Logger? logger,
    this.enabled = true,
    this.faults = Faults.none,
  })  : _store = store,
        _lock = lock,
        _fetch = fetch ?? ioHttpFetch,
        _now = now ?? systemClock,
        _timers = timers ?? const SystemTimers(),
        _random = random ?? secureRandom,
        _log = logger ?? const StderrLogger(),
        _refreshUrl = Uri.parse('${baseUrl.replaceFirst(RegExp(r'/+$'), '')}/refresh');

  final CredentialStore _store;

  /// CANT-31 §2's lock. On a device it is `SqliteLocks.call`, which every
  /// isolate and process over the same directory respects.
  final Lock _lock;
  final HttpFetch _fetch;
  final Clock _now;
  final Timers _timers;
  final RandomBytes _random;
  final Logger _log;
  final Uri _refreshUrl;

  /// Go's `Config.Refresh`. False presents the pair as held: no refresh, and
  /// the refused-token rule is inert, since a client that cannot refresh can
  /// never cure a refused token.
  final bool enabled;

  /// Never set outside a test or a rig proving an assertion can fail.
  final Faults faults;

  CredentialHost? _host;

  /// The record as last read or written. AUTHORITATIVE DECISIONS RE-READ THE
  /// STORE; this is for the synchronous questions — `refused` and `status()` —
  /// which may lag another context's write by one dial interval at most.
  StoredCredential? _rec;

  /// When CATENARY ITSELF last answered THIS context (Go's `answeredAt`). Per
  /// context and in memory on purpose: a relaunch, a second process and a
  /// background isolate are one property — a context that has heard nothing is
  /// closed.
  num? _answeredAt;

  /// The access token Catenary refused on `/sync` (CANT-129); null for none.
  String? _refusedToken;
  var _gateKnown = false;
  var _gateWasOpen = false;
  var _warnedLongChain = false;

  /// `kill -9`: nothing this context had in flight reaches the store after it.
  var _killed = false;

  var _refreshes = 0;
  var _refreshesSkipped = 0;
  var _refreshErrors = 0;
  var _refreshWalkBacks = 0;
  var _refreshesHeldUnreachable = 0;
  var _refreshesHeldBackoff = 0;

  // --- the seam ---------------------------------------------------------------

  @override
  void attach(CredentialHost host) {
    _host = host;
  }

  @override
  Future<Credential> current() async {
    final c = await _load();
    return Credential(userId: c.userId, deviceId: c.deviceId, accessToken: c.accessToken);
  }

  /// Go's `refusedDial`: NO TIME IN THIS ONE. While the access token is refused
  /// the client does not dial at all; the request a hold's end allows is a
  /// `/sync`, never an upgrade.
  @override
  Future<bool> withholdDial() async {
    if (await _reload() == null) return false;
    return !faults.presentRefusedToken && _refusedNow(true);
  }

  /// Go's `refusedHold`, re-read from the store on every poll.
  @override
  Future<bool> withholdSync() async {
    final rec = await _reload();
    if (rec == null || faults.presentRefusedToken || !_refusedNow(true)) return false;
    // THE WITHDRAWN RULE: never again, and so never at all — the control that
    // is watched stalling.
    if (faults.neverPresentRefusedToken) return true;
    return refusedHoldAt(refused: true, links: rec.chain.length, lastSentAt: rec.lastSentAt, now: _now());
  }

  @override
  bool get refused => _refusedNow(false);

  /// Go's `Run` calling `refreshDueWhenAllowed`: the proactive refresh, bounded
  /// by CANT-127. A held attempt is expected and not logged; a failed one is
  /// logged once, and the dial goes ahead with the held pair.
  @override
  Future<void> beforeDial() async {
    if (await refreshDueWhenAllowed() == RefreshOutcome.failed) _log.info('proactive refresh failed; dialing with the held pair');
  }

  @override
  Future<void> refreshIfDue() async {
    await attemptIfDue();
  }

  /// Go's `fetchWith` marking the token and `fetch` calling `refreshAfter401`:
  /// one refresh, then ONE retry — true when the retry should go out. A held
  /// refresh hands back the 401 it already had, without the retry.
  @override
  Future<bool> onSyncUnauthorized(Credential presented) async {
    if (!enabled) return false;
    _markRefused(presented.accessToken);
    final outcome = await refreshAfter401(presented.accessToken);
    return outcome == RefreshOutcome.refreshed || outcome == RefreshOutcome.skipped;
  }

  /// Go's `markAnswered`. Opening the gate is not a trigger: nothing is sent.
  @override
  void answered() {
    final now = _now();
    final last = _answeredAt;
    if (last == null || now > last) _answeredAt = now;
    _holdNow(true);
  }

  @override
  CredentialStatus status() {
    final rec = _rec;
    final links = rec?.chain.length ?? 0;
    return CredentialStatus(
      refreshHold: _holdNow(false),
      nextRefreshAt: nextRefreshAt(links, rec?.lastSentAt, _now()),
      refreshes: _refreshes,
      refreshesSkipped: _refreshesSkipped,
      refreshErrors: _refreshErrors,
      refreshWalkBacks: _refreshWalkBacks,
      chainLength: links,
      refreshesHeldUnreachable: _refreshesHeldUnreachable,
      refreshesHeldBackoff: _refreshesHeldBackoff,
    );
  }

  // --- the entry points ---------------------------------------------------------

  /// Go's `RefreshIfDue`: the EXPLICIT §1 check, and never held (CANT-127). It
  /// is an unconditional "do it now" when due, and costs one link if it settles
  /// nothing — what is bounded is the attempts made on the client's own
  /// initiative, because those repeat without anybody deciding to.
  Future<RefreshOutcome> attemptIfDue() async {
    final rec = await _reload();
    if (!enabled || rec == null || !_due(rec, _now())) return RefreshOutcome.notDue;
    return _refresh((held) => _due(held, _now()) ? _Need.needed : _Need.notNeeded);
  }

  /// Go's `refreshDueWhenAllowed`: the same decision, behind the suppressors.
  Future<RefreshOutcome> refreshDueWhenAllowed() async {
    final rec = await _reload();
    if (!enabled || rec == null || !_due(rec, _now())) return RefreshOutcome.notDue;
    return _refreshWhenAllowed((held) => _due(held, _now()) ? _Need.needed : _Need.notNeeded);
  }

  /// Go's `refreshAfter401`: refreshes unless some other context already
  /// replaced the pair that carried `usedAccessToken`. Not due-gated: a token
  /// the device's clock thinks is live still reaches its refresh.
  Future<RefreshOutcome> refreshAfter401(String usedAccessToken) =>
      _refreshWhenAllowed((held) => held.accessToken == usedAccessToken ? _Need.needed : _Need.notNeeded);

  /// `kill -9` (Go's `Client.Kill`): nothing in flight reaches the store after
  /// this, and nothing new is proposed. A relaunch is a new layer over the same
  /// store.
  void kill() {
    _killed = true;
  }

  // --- CANT-127: the suppressors ------------------------------------------------

  /// The ONE gated entry point (Go's `refreshWhenAllowed`). The hold is checked
  /// twice: here, before the lock, as the cheap skip — a held attempt must not
  /// queue on a lock it has no business taking — and under it, as the
  /// authority, because another context may have attempted while this one
  /// waited, lengthening the chain and moving the stamp.
  Future<RefreshOutcome> _refreshWhenAllowed(_Need Function(StoredCredential held) stillNeeded) async {
    final h = _holdNow(true);
    if (h != RefreshHold.none) {
      _countHeld(h);
      return RefreshOutcome.held;
    }
    return _refresh((held) {
      final again = _holdNow(true, held);
      if (again != RefreshHold.none) {
        _countHeld(again);
        return _Need.held;
      }
      return stillNeeded(held);
    });
  }

  /// The hold, from `fresh` (the cache unless a fresher read is passed), with
  /// the gate's transitions logged when `note` — status asks with note false,
  /// because being asked how you are must not write a line.
  RefreshHold _holdNow(bool note, [StoredCredential? fresh]) {
    final rec = fresh ?? _rec;
    final links = rec?.chain.length ?? 0;
    if (rec == null || links == 0) {
      if (note) _gateKnown = _gateWasOpen = false;
      return RefreshHold.none;
    }
    if (note) _warnLongChain(links);
    if (faults.unbounded) return RefreshHold.none;
    final now = _now();
    if (note) _noteGate(gateOpen(_answeredAt, readStamp(rec.lastSentAt, now)), links);
    return refreshHoldAt(links: links, lastSentAt: rec.lastSentAt, answeredAt: _answeredAt, now: now);
  }

  /// One INFO when this context's gate closes and one when it opens — not one
  /// per held attempt, which on a dead network would be ~720 an hour.
  void _noteGate(bool open, int links) {
    final known = _gateKnown;
    final was = _gateWasOpen;
    _gateKnown = true;
    _gateWasOpen = open;
    if (known && was == open) return;
    if (open) {
      _log.info('refresh gate open: Catenary has answered this context since the last send', {'chain_length': links});
    } else {
      _log.info('refresh gate closed: no answer from Catenary since the last send', {'chain_length': links});
    }
  }

  void _warnLongChain(int links) {
    if (links <= chainWarnLength || _warnedLongChain) return;
    _warnedLongChain = true;
    _log.warn('the refresh chain is long: the path is failing, not the device', {'chain_length': links, 'above': chainWarnLength});
  }

  /// A refresh nobody made did not fail: counted under its suppressor, NEVER in
  /// `refreshErrors`.
  void _countHeld(RefreshHold h) {
    if (h == RefreshHold.unreachable) {
      _refreshesHeldUnreachable++;
    } else if (h == RefreshHold.backoff) {
      _refreshesHeldBackoff++;
    }
    _host?.notify();
  }

  // --- CANT-129: the refused token ------------------------------------------------

  /// Go's `markRefused`: the ONE site that marks a token, keyed on the token
  /// the request carried, so the pair changing is what ends it. One INFO the
  /// first time a token is marked.
  void _markRefused(String token) {
    if (!enabled || token.isEmpty) return;
    final first = _refusedToken != token;
    _refusedToken = token;
    if (!first) return;
    _log.info(
      'access token refused by Catenary on /sync; until the pair changes it is not dialed with, and it is presented at most once per refresh hold',
      {'chain_length': _rec?.chain.length ?? 0},
    );
    _host?.notify();
  }

  /// Go's `refusedNow`: the pair the client would present NOW carries the
  /// refused token. With `note`, also where the mark is retired, once.
  bool _refusedNow(bool note) {
    final token = _rec?.accessToken ?? '';
    final marked = _refusedToken;
    if (marked != null && marked != token) {
      if (!note) return false;
      _refusedToken = null;
      _log.info('the pair changed; the refused access token is behind us');
      _host?.notify();
      return false;
    }
    return marked != null && marked == token;
  }

  // --- record §2: single-flight -------------------------------------------------

  /// Go's `refresh`. `stillNeeded` is asked UNDER the lock about the credential
  /// as it is persisted now, and answers three ways, because a bool cannot say
  /// HELD. PERSIST BEFORE USE: the rotated pair is in the store before the lock
  /// is released and before anything presents it.
  Future<RefreshOutcome> _refresh(_Need Function(StoredCredential held) stillNeeded) async {
    Future<RefreshOutcome> run() async {
      final held = await _load();
      switch (stillNeeded(held)) {
        case _Need.notNeeded:
          _refreshesSkipped++;
          _host?.notify();
          return RefreshOutcome.skipped;
        case _Need.held:
          return RefreshOutcome.held;
        case _Need.needed:
          break;
      }

      final ex = await _exchange(held);
      switch (ex.kind) {
        case _Kind.moved:
          _refreshesSkipped++;
          _host?.notify();
          return RefreshOutcome.skipped;
        case _Kind.unauthorized:
          return _refusedRefresh(held);
        case _Kind.ok:
          final rotated = await _rotate(ex.next!);
          if (rotated != RefreshOutcome.refreshed) return rotated;
          _refreshes++;
          _host?.notify();
          return RefreshOutcome.refreshed;
        case _Kind.killed:
          _refreshErrors++;
          _host?.notify();
          return RefreshOutcome.killed;
        case _Kind.failed || _Kind.presentProposal || _Kind.freshProposal:
          _refreshErrors++;
          _host?.notify();
          if (faults.alwaysTerminal) return _terminal('fault: every unsettled refresh is terminal');
          return RefreshOutcome.failed;
      }
    }

    try {
      final deviceId = (_rec ?? await _load()).deviceId;
      return await (faults.refreshUnlocked ? run() : _lock(credentialLockName(deviceId), run));
    } catch (_) {
      // The store or the lock failed under us: an attempt that settled nothing.
      _refreshErrors++;
      _host?.notify();
      return RefreshOutcome.failed;
    }
  }

  /// Record §5's `/refresh` row (Go's `refused`): Catenary's OWN 401 to the
  /// oldest token is terminal only when THE STORED CREDENTIAL IS STILL THE ONE
  /// PRESENTED. A context that does not share the lock may have rotated the
  /// pair while this request was in flight; then the refused token is merely
  /// spent, and the caller uses what is there now.
  Future<RefreshOutcome> _refusedRefresh(StoredCredential presented) async {
    final now = await _load();
    if (now.refreshToken != presented.refreshToken) {
      _refreshesSkipped++;
      _host?.notify();
      return RefreshOutcome.skipped;
    }
    _refreshErrors++;
    if (faults.neverTerminal) {
      _host?.notify();
      return RefreshOutcome.failed;
    }
    return _terminal('POST /refresh: Catenary refused the stored refresh token');
  }

  /// The credential terminal. NOTHING IS DELETED — not the credential, not the
  /// journal — and the transport's first terminal wins.
  RefreshOutcome _terminal(String reason) {
    _host?.terminal(Terminal(TerminalKind.credential, reason));
    _host?.notify();
    return RefreshOutcome.terminal;
  }

  // --- record §3: the chain -----------------------------------------------------

  /// Go's `exchange`: WHEN THE OUTCOME OF A REFRESH IS UNKNOWN, WALK THE CHAIN.
  /// NEWEST FIRST — the chain's last proposal, with a new link of its own —
  /// then ONE STEP BACK PER CATENARY 401, reusing each token's original
  /// proposal. A 401 that is not Catenary's never steps the walk;
  /// `present_proposal` moves it forward; `fresh_proposal` rewrites the link,
  /// bounded. What bounds how OFTEN this is asked is hold.dart's, not this.
  Future<_Step> _exchange(StoredCredential held) async {
    final chain = held.chain;
    var token = held.refreshToken;
    if (chain.isNotEmpty && !faults.noChain) token = chain.last.proposal;
    var replace = false;
    var collisions = 0;
    for (var steps = chain.length + _walkSlack; steps > 0; steps--) {
      final p = await _propose(token, replace);
      if (p.kind != _Kind.ok) return p;
      replace = false;

      final res = await _postRefresh(held, token, p.proposal!);
      switch (res.kind) {
        case _Kind.ok:
          // THE RESPONSE'S refresh_token, NEVER THE PROPOSAL ON FAITH.
          return res;
        case _Kind.unauthorized:
          final older = await _older(token);
          if (older == null) return res;
          _refreshWalkBacks++;
          token = older;
        case _Kind.presentProposal:
          // STOP PRESENTING THAT TOKEN: its proposal is the newest token now.
          token = p.proposal!;
        case _Kind.freshProposal:
          if (++collisions > maxFreshProposals) return const _Step(_Kind.failed);
          await _pause(min(res.afterMs, _maxRetryAfterMs));
          replace = true;
        case _Kind.failed || _Kind.moved || _Kind.killed:
          return res;
      }
    }
    // Too many answers that settled nothing.
    return const _Step(_Kind.failed);
  }

  /// Go's `Journal.propose` with CANT-127's stamp riding on the mint: PERSIST
  /// BEFORE SEND. A token already in the chain gets the proposal it was first
  /// presented with and writes nothing; otherwise it must be the chain's
  /// newest, and one link is appended with `last_sent_at` in the same write.
  /// `moved` is another context having rotated the credential since it was
  /// read.
  Future<_Step> _propose(String token, bool replace) async {
    if (faults.noChain) {
      try {
        return _Step(_Kind.ok, proposal: mintProposal(_random));
      } catch (_) {
        return const _Step(_Kind.failed);
      }
    }
    final rep = replace || faults.proposeAfresh;
    StoredCredential? written;
    final out = await _store.update<_Step>((held) {
      if (_killed) return (write: null, result: const _Step(_Kind.killed));
      if (held == null) return (write: null, result: const _Step(_Kind.failed));
      CredentialWrite<_Step> link(int i) {
        final String proposal;
        try {
          proposal = mintProposal(_random);
        } catch (_) {
          // NOTHING IS WRITTEN AND NOTHING IS STAMPED: no request, no send.
          return (write: null, result: const _Step(_Kind.failed));
        }
        final next = held.withChain([...held.chain.take(i), ChainLink(token, proposal)], _now());
        written = next;
        return (write: next, result: _Step(_Kind.ok, proposal: proposal));
      }

      final at = held.chain.indexWhere((l) => l.token == token);
      if (at >= 0) return rep ? link(at) : (write: null, result: _Step(_Kind.ok, proposal: held.chain[at].proposal));
      final newest = held.chain.isNotEmpty ? held.chain.last.proposal : held.refreshToken;
      if (token != newest) return (write: null, result: const _Step(_Kind.moved));
      return link(held.chain.length);
    });
    if (written != null) _rec = written;
    return out;
  }

  /// The token presented BEFORE `token`; null for the oldest, or one not in the
  /// chain.
  Future<String?> _older(String token) async {
    final rec = await _load();
    final i = rec.chain.indexWhere((l) => l.token == token);
    return i > 0 ? rec.chain[i - 1].token : null;
  }

  /// Go's `rotate` + `Journal.Rotate`: AN ANSWER COLLAPSES THE CHAIN, and the
  /// stamp goes with it. Refused after a kill, and for a different device.
  Future<RefreshOutcome> _rotate(StoredCredential next) async {
    StoredCredential? written;
    final out = await _store.update<RefreshOutcome>((held) {
      if (_killed) return (write: null, result: RefreshOutcome.killed);
      if (held == null || held.deviceId != next.deviceId || next.accessToken.isEmpty || next.refreshToken.isEmpty) {
        return (write: null, result: RefreshOutcome.failed);
      }
      final rotated = StoredCredential(
        userId: held.userId,
        deviceId: next.deviceId,
        accessToken: next.accessToken,
        accessExpiresAt: next.accessExpiresAt,
        refreshToken: next.refreshToken,
        refreshExpiresAt: next.refreshExpiresAt,
        accessIssuedAt: next.accessIssuedAt,
        clockOffsetMs: next.clockOffsetMs,
      );
      written = rotated;
      return (write: rotated, result: RefreshOutcome.refreshed);
    });
    if (written != null) _rec = written;
    return out;
  }

  /// One `POST /refresh`: `token`, presented with `proposal`. `held` is the
  /// confirmed pair, for what a rotation keeps — the device, and the clock
  /// offset when the response carries no `Date`.
  Future<_Step> _postRefresh(StoredCredential held, String token, String proposal) async {
    final cancel = Completer<void>();
    final timer = _timers.setTimeout(() {
      if (!cancel.isCompleted) cancel.complete();
    }, _refreshTimeoutMs);
    final HttpAnswer res;
    try {
      res = await Future.any([
        _fetch(HttpExchange(
          method: 'POST',
          url: _refreshUrl,
          headers: const {'Content-Type': 'application/json'},
          body: jsonEncode(RefreshRequest(refreshToken: token, proposedRefreshToken: proposal).toJson()),
          abort: cancel.future,
        )),
        cancel.future.then<HttpAnswer>((_) => throw TimeoutException('refresh: no response')),
      ]);
    } catch (_) {
      return const _Step(_Kind.failed);
    } finally {
      _timers.clearTimeout(timer);
    }
    final arrived = _now();
    if (isCatenaryUnauthorized(res.status, res.body)) return const _Step(_Kind.unauthorized);
    if (res.status == 503) {
      // `retry` SAYS WHICH, and the two ask for opposite things (record §5). A
      // 503 without one this client recognises is an unknown outcome.
      Object? retry;
      try {
        final v = jsonDecode(res.body);
        retry = v is Map ? v['retry'] : null;
      } on FormatException {
        retry = null;
      }
      if (retry == 'present_proposal') return const _Step(_Kind.presentProposal);
      if (retry == 'fresh_proposal') {
        final secs = int.tryParse(res.headers['retry-after'] ?? '');
        return _Step(_Kind.freshProposal, afterMs: secs != null && secs > 0 ? secs * 1000 : 0);
      }
    }
    if (res.status != 200) return const _Step(_Kind.failed);
    final RefreshResponse out;
    final int accessExpiresAt;
    final int refreshExpiresAt;
    try {
      out = RefreshResponse.fromJson(jsonDecode(res.body));
      accessExpiresAt = parseWireTime(out.accessExpiresAt, 'access_expires_at');
      refreshExpiresAt = parseWireTime(out.refreshExpiresAt, 'refresh_expires_at');
    } catch (_) {
      // The response did not decode. Not its text: a body is the one place a
      // server could echo a token.
      return const _Step(_Kind.failed);
    }
    // A RESPONSE WITH NO USABLE Date KEEPS THE OFFSET IT HAD: the offset
    // describes the device's clock, not one pair, and a stale correction is a
    // better guess than none. The issue time is then the corrected arrival.
    final date = parseHttpDate(res.headers['date']);
    return _Step(
      _Kind.ok,
      next: StoredCredential(
        userId: held.userId,
        deviceId: held.deviceId,
        accessToken: out.accessToken,
        accessExpiresAt: accessExpiresAt,
        refreshToken: out.refreshToken,
        refreshExpiresAt: refreshExpiresAt,
        accessIssuedAt: date ?? arrived + held.clockOffsetMs,
        clockOffsetMs: date == null ? held.clockOffsetMs : date - arrived,
      ),
    );
  }

  // --- the store ------------------------------------------------------------------

  /// The persisted credential, re-read, and cached for the synchronous
  /// questions.
  Future<StoredCredential> _load() async {
    final rec = await _store.read();
    if (rec == null) throw const NoCredential();
    return _rec = rec;
  }

  /// `_load` for the polls, where no credential is an answer rather than an
  /// error.
  Future<StoredCredential?> _reload() async {
    try {
      return await _load();
    } catch (_) {
      return null;
    }
  }

  Future<void> _pause(num ms) {
    if (ms <= 0) return Future.value();
    final done = Completer<void>();
    _timers.setTimeout(done.complete, ms);
    return done.future;
  }
}
