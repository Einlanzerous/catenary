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
/// generated decoder.
///
/// IT NAMES EVERY KIND THE FILE CARRIES, and runs the ones whose code this
/// package has: `close` and the four `backoff_*`. The other eight are the
/// credential layer's, and until it lands they are DEFERRED BY NAME — reported
/// in the last line, and never "unknown". A kind this runner does not name
/// fails, and so does a named kind with no cases.
///
/// A runner that cannot fail proves nothing, so two mutants must each fail
/// CANT-177's close case: a `preceding` decoded strictly, and a classifyClose
/// that stops on a code it does not know.
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

/// The kinds whose rule this package does not have yet.
const deferred = {'threshold', 'due', 'delay', 'stamp', 'gate', 'hold', 'refused_hold', 'chain'};

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

/// One case: null when it agrees, else what disagreed.
String? pure(Json c, [CloseFns fns = shippedClose]) {
  final i = c['in'] as Json;
  final w = c['want'] as Json;
  switch (c['kind']) {
    case 'close':
      only(i, 'in', ['status', 'preceding']);
      only(w, 'want', ['verdict']);
      return same('verdict', fns.classify(required(i, 'status') as int?, fns.preceding(required(i, 'preceding'))).wire, required(w, 'verdict'));
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

String? run(Json c, [CloseFns fns = shippedClose]) {
  try {
    return pure(c, fns);
  } catch (e) {
    return 'threw: $e';
  }
}

/// The case CANT-177 added: a 1008 after an error code a later server adds.
const laterCode = 'close_1008_after_an_error_code_a_later_server_adds';

/// The outcome of one run over a vector file.
typedef Outcome = ({List<String> failures, int ran, int skipped, List<String> deferredKinds, int checks});

/// Runs every case of `doc`. `report` gets one line per check.
Outcome runDecisions(Json doc, {void Function(String line)? report}) {
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
    final err = run(c);
    ran++;
    check(name, err == null, err ?? '');
  }

  // Every listed kind has cases: a kind nobody wrote a vector for pins nothing.
  for (final k in kinds) {
    final n = cases.where((c) => c['kind'] == k).length;
    checks++;
    check('kind $k has cases', n > 0, '$n${deferred.contains(k) ? ', deferred' : ''}');
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
    final err = run(c, fns);
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

void main(List<String> args) {
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
  final outcome = runDecisions(jsonDecode(file.readAsStringSync()) as Json, report: stdout.writeln);
  stdout.writeln('\n${summary(outcome)}');
  exit(outcome.failures.isEmpty ? 0 : 1);
}
