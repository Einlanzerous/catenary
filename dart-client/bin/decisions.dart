/// Dart decision-vector runner — the third, after
/// internal/client/decisions_test.go (TestTheDecisionVectors) and
/// web/decisions.ts. It reads the SAME internal/client/testdata/decisions.json
/// and must reach the SAME answer for every case; the file's note is the
/// contract.
///
/// CANT-31's rules are implemented three times, and two of them disagreeing is
/// a device that stops when it should reconnect, or one that keeps minting
/// refresh links on a dead network. Neither is a type error, so codegen cannot
/// catch it.
///
/// THIS RUNNER IMPLEMENTS NO RULE OF ITS OWN. Every kind dispatches to exactly
/// one function of `catenary_client`, and `preceding` goes through the
/// generated decoder. Its only translation is an instant to epoch
/// milliseconds; a null `access_expires_at` is the credential's own "no
/// expiry". What else is here — binding `$P<n>`, writing a scripted response,
/// reading the store back — is the harness, not a decision.
///
/// IT NAMES EVERY KIND THE FILE CARRIES AND DEFERS NONE. A kind this runner
/// does not name fails, and so does a named kind with no cases.
///
/// A runner that cannot fail proves nothing. The chain transcripts run three
/// times: clean, where every case must pass, and under `noChain` and
/// `proposeAfresh`, where at least one case must FAIL. And two mutants must
/// each fail CANT-177's close case: a `preceding` decoded strictly, and a
/// classifyClose that stops on a code it does not know.
///
/// Run: dart run bin/decisions.dart   (from the dart-client/ directory)
library;

import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';

/// Every kind in the file, in the order web/decisions.ts lists them.
const kinds = [
  'close', 'threshold', 'due', 'delay', 'stamp', 'gate', 'hold', 'refused_hold', 'chain',
  'backoff_reset', 'backoff_draw', 'backoff_jitter', 'backoff_advance', //
];

/// The kinds whose rule this package does not have yet. EMPTY, and it stays in
/// the code so that a kind added to the file before its Dart rule exists has
/// somewhere to be named rather than dropped.
const deferred = <String>{};

typedef Json = Map<String, dynamic>;

/// What a vector does that a runner refuses: a malformed case, not a
/// disagreement.
final class VectorError implements Exception {
  const VectorError(this.message);

  final String message;

  @override
  String toString() => message;
}

/// A vector field the runner does not know is an error: a misspelled key would
/// otherwise read as absent and pass.
void only(Json o, String where, List<String> keys) {
  for (final k in o.keys) {
    if (!keys.contains(k)) throw VectorError('$where: unknown field $k');
  }
}

Object? required(Json o, String k) => o.containsKey(k) ? o[k] : throw VectorError('missing field $k');

String? same(String what, Object? got, Object? want) => got == want ? null : '$what ${jsonEncode(got)}, want ${jsonEncode(want)}';

ServerError? preceding(Object? v) {
  if (v == null) return null;
  final f = ServerFrame.fromJson(v);
  if (f is! ServerError) throw VectorError('preceding: ${f == null ? 'an unknown tag' : f.type}, want an error frame');
  if (f.clientId != null) {
    throw const VectorError("preceding: an error naming a client_id is that send's answer, never the session's (a malformed vector)");
  }
  return f;
}

/// The close kind's two functions, which a teeth check replaces one at a time.
typedef CloseFns = ({ServerError? Function(Object? v) preceding, CloseVerdict Function(int? code, ServerError? preceding) classify});

const CloseFns shippedClose = (preceding: preceding, classify: _classify);

CloseVerdict _classify(int? code, ServerError? preceding) => classifyClose(code, preceding).verdict;

/// A wire timestamp to epoch ms; null is absent.
int? instant(Object? v) {
  if (v == null) return null;
  final t = DateTime.tryParse('$v');
  if (t == null) throw VectorError('not an instant: ${jsonEncode(v)}');
  return t.millisecondsSinceEpoch;
}

/// One pure case: null when it agrees, else what disagreed.
String? pure(Json c, [CloseFns fns = shippedClose]) {
  final i = c['in'] as Json;
  final w = c['want'] as Json;
  switch (c['kind']) {
    case 'close':
      only(i, 'in', ['status', 'preceding']);
      only(w, 'want', ['verdict']);
      return same('verdict', fns.classify(required(i, 'status') as int?, fns.preceding(required(i, 'preceding'))).wire, required(w, 'verdict'));
    case 'threshold':
      only(i, 'in', ['access_issued_at', 'access_expires_at']);
      only(w, 'want', ['threshold_ms']);
      return same(
        'threshold_ms',
        refreshThreshold(accessIssuedAt: instant(required(i, 'access_issued_at')), accessExpiresAt: instant(required(i, 'access_expires_at'))),
        required(w, 'threshold_ms'),
      );
    case 'due':
      only(i, 'in', ['access_issued_at', 'access_expires_at', 'clock_offset_ms', 'device_now']);
      only(w, 'want', ['due']);
      return same(
        'due',
        refreshDue(
          accessIssuedAt: instant(required(i, 'access_issued_at')),
          accessExpiresAt: instant(required(i, 'access_expires_at')),
          clockOffsetMs: required(i, 'clock_offset_ms') as num,
          deviceNow: instant(required(i, 'device_now'))!,
        ),
        required(w, 'due'),
      );
    case 'delay':
      only(i, 'in', ['links']);
      only(w, 'want', ['delay_ms']);
      return same('delay_ms', refreshDelay(required(i, 'links') as int), required(w, 'delay_ms'));
    case 'stamp':
      only(i, 'in', ['last_sent', 'now']);
      only(w, 'want', ['stamp']);
      return same('stamp', readStamp(instant(required(i, 'last_sent')), instant(required(i, 'now'))!), instant(required(w, 'stamp')));
    case 'gate':
      only(i, 'in', ['answered_at', 'stamp']);
      only(w, 'want', ['open']);
      return same('open', gateOpen(instant(required(i, 'answered_at')), instant(required(i, 'stamp'))), required(w, 'open'));
    case 'hold':
      only(i, 'in', ['links', 'answered_at', 'last_sent', 'now']);
      only(w, 'want', ['hold']);
      return same(
        'hold',
        refreshHoldAt(
          links: required(i, 'links') as int,
          answeredAt: instant(required(i, 'answered_at')),
          lastSentAt: instant(required(i, 'last_sent')),
          now: instant(required(i, 'now'))!,
        ).name,
        required(w, 'hold'),
      );
    case 'refused_hold':
      only(i, 'in', ['refused', 'links', 'last_sent', 'now']);
      only(w, 'want', ['hold']);
      return same(
        'hold',
        refusedHoldAt(
          refused: required(i, 'refused') as bool,
          links: required(i, 'links') as int,
          lastSentAt: instant(required(i, 'last_sent')),
          now: instant(required(i, 'now'))!,
        ),
        required(w, 'hold'),
      );
    // The dial backoff's pure pieces (CANT-170).
    case 'backoff_reset':
      only(i, 'in', ['readied_for_ms', 'heartbeat_interval_sec']);
      only(w, 'want', ['resets']);
      return same('resets', resetsRamp(required(i, 'readied_for_ms') as num?, required(i, 'heartbeat_interval_sec') as num?), required(w, 'resets'));
    case 'backoff_draw':
      only(i, 'in', ['bytes']);
      only(w, 'want', ['unit']);
      final hex = '${required(i, 'bytes')}';
      if (!RegExp(r'^[0-9a-f]{8}$').hasMatch(hex)) throw VectorError('in.bytes ${jsonEncode(hex)} is not four bytes of hex');
      final bytes = [for (var k = 0; k < 8; k += 2) int.parse(hex.substring(k, k + 2), radix: 16)];
      return same('unit', unitFromBytes(bytes), required(w, 'unit'));
    case 'backoff_jitter':
      only(i, 'in', ['nominal_ms', 'unit']);
      only(w, 'want', ['wait_ms']);
      return same('wait_ms', jitteredWait(required(i, 'nominal_ms') as num, required(i, 'unit') as num), required(w, 'wait_ms'));
    case 'backoff_advance':
      only(i, 'in', ['nominal_ms', 'max_ms']);
      only(w, 'want', ['next_ms']);
      return same('next_ms', advance(required(i, 'nominal_ms') as num, required(i, 'max_ms') as num), required(w, 'next_ms'));
  }
  return 'unknown kind ${c['kind']}';
}

// --- the chain transcripts ------------------------------------------------------

/// `$P<n>` binds ON FIRST APPEARANCE to what the client presented, and every
/// later appearance must equal it. Anything else is a literal fixture.
final class Symbols {
  final _bound = <String, String>{};

  String? match(String what, String want, String got) {
    if (!want.startsWith(r'$')) return got == want ? null : '$what ${jsonEncode(got)}, want ${jsonEncode(want)}';
    final b = _bound[want];
    if (b != null && b != got) return '$what ${jsonEncode(got)}, want $want = ${jsonEncode(b)}';
    _bound[want] = got;
    return null;
  }

  /// A symbol in a response or in `want`: nothing the client has not presented
  /// can be answered or held.
  String resolve(String v) {
    if (!v.startsWith(r'$')) return v;
    return _bound[v] ?? (throw VectorError('symbol $v is used before the client presented it'));
  }

  Object? resolveJson(Object? v) => switch (v) {
        String() => resolve(v),
        List() => [for (final x in v) resolveJson(x)],
        Map() => {for (final e in v.entries) e.key: resolveJson(e.value)},
        _ => v,
      };
}

const _device = '00000000-0000-4000-8000-000000000156';
const _user = '00000000-0000-4000-8000-000000000035';

List<ChainLink> _links(Object? v) => [for (final l in (v as List).cast<Json>()) ChainLink(l['token'] as String, l['proposal'] as String)];

Future<String?> chain(Json c, Faults faults) async {
  final i = c['in'] as Json;
  final w = c['want'] as Json;
  only(i, 'in', ['now', 'start', 'calls']);
  only(w, 'want', ['refresh_token', 'chain', 'stamped', 'terminal']);
  final now = instant(required(i, 'now'))!;
  final start = required(i, 'start') as Json;
  only(start, 'in.start', ['refresh_token', 'chain', 'last_sent']);
  final calls = (required(i, 'calls') as List).cast<Json>();
  final startChain = _links(required(start, 'chain'));
  if (startChain.isNotEmpty && startChain.first.token != start['refresh_token']) {
    throw const VectorError("start.chain[0].token must be start.refresh_token (the store's invariant)");
  }

  // A PAIR EXPIRED AN HOUR BEFORE `now`, so the refresh is due whatever the
  // floor, on a clock that does not move.
  final store = MemoryCredentialStore(StoredCredential(
    userId: _user,
    deviceId: _device,
    accessToken: 'access_token_FIXTURE_before_rotation_______',
    accessExpiresAt: now - 3600000,
    refreshToken: required(start, 'refresh_token') as String,
    refreshExpiresAt: now + 86400000,
    chain: startChain,
    lastSentAt: instant(required(start, 'last_sent')),
  ));
  final sym = Symbols();
  var steps = <Json>[];
  var next = 0;
  String? mismatch;

  // The scripted /refresh: no model of a server, only the next step's check
  // and its bytes. The first disagreement is kept, and every request after it
  // gets a 500 — an unknown outcome, which ends the attempt.
  Future<HttpAnswer> fetch(HttpExchange x) async {
    HttpAnswer fail(String why) {
      mismatch ??= why;
      return const HttpAnswer(500, 'decision vector mismatch');
    }

    if (mismatch != null) return fail(mismatch!);
    if (x.url.path != '/refresh') return fail('a request to ${x.url.path}; only /refresh is scripted');
    if (x.method != 'POST') return fail('a ${x.method} to /refresh');
    if (next >= steps.length) return fail('request ${next + 1} arrived, and this call scripts ${steps.length}');
    final n = next + 1;
    final st = steps[next++];

    // THE GENERATED DECODER, so a proposal that is not a wire Token fails here.
    final String token;
    final String proposal;
    try {
      final req = RefreshRequest.fromJson(jsonDecode(x.body!));
      final proposed = req.proposedRefreshToken;
      if (proposed == null) return fail('step $n: the request carries no proposal');
      token = req.refreshToken;
      proposal = proposed;
    } catch (e) {
      return fail('step $n: the request does not decode as a wire RefreshRequest: $e');
    }
    final expect = st['expect'] as Json;
    final bad = sym.match('presented token', expect['token'] as String, token) ?? sym.match('proposal', expect['proposal'] as String, proposal);
    if (bad != null) return fail('step $n: $bad');
    // PERSISTED BEFORE SENT, at every request: the link and its stamp are in
    // the store before the request arrives.
    final held = await store.read();
    if (held == null || !held.chain.any((l) => l.token == token && l.proposal == proposal)) {
      return fail('step $n: the store does not hold the link being presented; it holds ${jsonEncode(held?.chain)}');
    }
    if (held.lastSentAt == null) return fail('step $n: the store holds no last_sent stamp for the request in flight');

    final r = st['respond'] as Json;
    only(r, 'step $n: respond', ['status', 'json', 'text', 'headers', 'drop']);
    final headers = (r['headers'] as Json?) ?? const {};
    for (final h in headers.entries) {
      if (h.key != 'Retry-After' || h.value != '0') return fail('step $n: the only header a vector may set is Retry-After: 0');
    }
    if (r['drop'] == true) {
      if (r.length != 1) return fail('step $n: a drop carries nothing else');
      throw const VectorDrop(); // the connection closed with no response
    }
    if (r.containsKey('json') == r.containsKey('text')) return fail('step $n: respond is exactly one of json, text or drop');
    final String body;
    try {
      body = r.containsKey('json') ? jsonEncode(sym.resolveJson(r['json'])) : r['text'] as String;
    } catch (e) {
      return fail('step $n: respond: $e');
    }
    return HttpAnswer(r['status'] as int, body, {
      'content-type': r.containsKey('json') ? 'application/json' : 'text/plain; charset=utf-8',
      for (final h in headers.entries) h.key.toLowerCase(): '${h.value}',
    });
  }

  final terminals = <Terminal>[];
  final cred = RefreshingCredential(
    baseUrl: 'http://decisions.invalid',
    store: store,
    lock: inProcessLock(),
    fetch: fetch,
    now: () => now,
    logger: const SilentLogger(),
    faults: faults,
  )..attach(_Terminals(terminals));

  for (final (k, call) in calls.indexed) {
    steps = (call['steps'] as List).cast<Json>();
    next = 0;
    await cred.refreshIfDue();
    if (mismatch != null) return 'call ${k + 1}: $mismatch';
    if (next != steps.length) return 'call ${k + 1}: $next requests arrived, and it scripts ${steps.length}';
  }

  // THE END STATE, READ FROM STATE: no counter is asserted.
  final rec = await store.read();
  if (rec == null) return 'the store holds no credential';
  final wantToken = sym.resolve(required(w, 'refresh_token') as String);
  if (rec.refreshToken != wantToken) return 'the store holds refresh token ${jsonEncode(rec.refreshToken)}, want ${jsonEncode(wantToken)}';
  final wantChain = jsonEncode([for (final l in _links(required(w, 'chain'))) ChainLink(sym.resolve(l.token), sym.resolve(l.proposal))]);
  if (jsonEncode(rec.chain) != wantChain) return 'the chain is ${jsonEncode(rec.chain)}, want $wantChain';
  return same('stamped', rec.lastSentAt != null, required(w, 'stamped')) ??
      same('credential terminal', terminals.any((t) => t.kind == TerminalKind.credential), required(w, 'terminal'));
}

/// `respond: {drop: true}`: the request got no response at all.
final class VectorDrop implements Exception {
  const VectorDrop();
}

/// The host a chain transcript attaches: it records the terminals entered,
/// which is the only place a credential terminal is announced.
final class _Terminals implements CredentialHost {
  const _Terminals(this._entered);

  final List<Terminal> _entered;

  @override
  void terminal(Terminal t) => _entered.add(t);

  @override
  void notify() {}
}

Future<String?> run(Json c, {Faults faults = Faults.none, CloseFns fns = shippedClose}) async {
  try {
    return c['kind'] == 'chain' ? await chain(c, faults) : pure(c, fns);
  } catch (e) {
    return 'threw: $e';
  }
}

/// The case CANT-177 added: a 1008 after an error code a later server adds.
const laterCode = 'close_1008_after_an_error_code_a_later_server_adds';

/// The outcome of one run over a vector file.
typedef Outcome = ({List<String> failures, int ran, int skipped, List<String> deferredKinds, int checks});

/// Runs every case of `doc`. `report` gets one line per check.
Future<Outcome> runDecisions(Json doc, {void Function(String line)? report}) async {
  final cases = (doc['cases'] as List).cast<Json>();
  final failures = <String>[];
  var checks = 0;
  void check(String name, bool ok, [String detail = '']) {
    if (!ok) failures.add(detail.isEmpty ? name : '$name — $detail');
    report?.call('${ok ? 'ok  ' : 'FAIL'}  $name${detail.isEmpty ? '' : '  ($detail)'}');
  }

  final names = <String>{};
  var ran = 0;
  var skipped = 0;
  for (final c in cases) {
    final name = '${c['name']}';
    final kind = '${c['kind']}';
    if (!names.add(name)) check(name, false, 'two cases have this name');
    if ('${c['why'] ?? ''}'.trim().isEmpty) check(name, false, 'no why');
    if (!kinds.contains(kind)) {
      check(name, false, 'unknown kind $kind');
      continue;
    }
    if (deferred.contains(kind)) {
      skipped++;
      continue;
    }
    final err = await run(c);
    ran++;
    check(name, err == null, err ?? '');
  }

  // Every listed kind has cases: a kind nobody wrote a vector for pins nothing.
  for (final k in kinds) {
    final n = cases.where((c) => c['kind'] == k).length;
    checks++;
    check('kind $k has cases', n > 0, '$n${deferred.contains(k) ? ', deferred' : ''}');
  }

  // TEETH: the chain transcripts must catch a client that forgets its
  // proposals, and one that mints a new proposal where it must reuse the
  // original.
  final chains = cases.where((c) => c['kind'] == 'chain').toList();
  for (final (name, faults) in const [('faults.noChain', Faults(noChain: true)), ('faults.proposeAfresh', Faults(proposeAfresh: true))]) {
    var caught = 0;
    for (final c in chains) {
      if (await run(c, faults: faults) != null) caught++;
    }
    checks++;
    check('$name fails at least one chain transcript', caught > 0, '$caught of ${chains.length}');
  }

  // TEETH for CANT-177's case, the Go runner's two mutants: `preceding` decoded
  // as the server decodes it, which refuses an error code this wire version
  // does not define, and a classifyClose that stops on such a code. Each must
  // fail that case by name.
  ServerError? strictPreceding(Object? v) {
    final f = preceding(v);
    if (f != null && f.code == ErrorCode.unknown) {
      throw const VectorError('preceding: the strict decoder refuses an error code this wire version does not define');
    }
    return f;
  }

  CloseVerdict stopsOnUnknown(int? code, ServerError? p) =>
      code == closePolicyViolation && p != null && p.code == ErrorCode.unknown ? CloseVerdict.terminalProtocol : _classify(code, p);

  final mutants = <String, CloseFns>{
    "preceding decoded by the server's strict decoder": (preceding: strictPreceding, classify: _classify),
    'classifyClose stops on a code it does not know': (preceding: preceding, classify: stopsOnUnknown),
  };
  for (final MapEntry(key: name, value: fns) in mutants.entries) {
    checks++;
    final c = cases.where((c) => c['name'] == laterCode).firstOrNull;
    if (c == null) {
      check('$name fails $laterCode', false, 'no case has that name');
      continue;
    }
    final err = await run(c, fns: fns);
    check('$name fails $laterCode', err != null, err ?? 'it passed');
  }

  return (
    failures: failures,
    ran: ran,
    skipped: skipped,
    deferredKinds: [for (final k in kinds) if (deferred.contains(k)) k],
    checks: checks,
  );
}

/// The last line: the count it ran, and every kind it deferred, by name.
String summary(Outcome o) {
  if (o.failures.isNotEmpty) return '${o.failures.length} of ${o.ran + o.checks} FAILED';
  final tail = o.deferredKinds.isEmpty ? 'none deferred' : '${o.skipped} deferred by name: ${o.deferredKinds.join(', ')}';
  return 'all green — ${o.ran} decision vectors + ${o.checks} runner checks; $tail';
}

Future<void> main(List<String> args) async {
  final path = args.isNotEmpty ? args.first : '../internal/client/testdata/decisions.json';
  final file = File(path);
  if (!file.existsSync()) {
    stderr.writeln('decisions: cannot find $path');
    exit(2);
  }
  // A tolerated unknown enum value is a warning the generated decoder prints.
  // One vector produces one on purpose — the CANT-177 case — and any other
  // that did must not be lost in the noise.
  onUnknownWireValue = (m) => stdout.writeln('warn  $m');
  final outcome = await runDecisions(jsonDecode(file.readAsStringSync()) as Json, report: stdout.writeln);
  stdout.writeln('\n${summary(outcome)}');
  exit(outcome.failures.isEmpty ? 0 : 1);
}
