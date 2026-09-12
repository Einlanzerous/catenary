/// Dart conformance runner — IDEA-27 (R4).
///
/// Reads the SAME schema/vectors/vectors.json as web/conformance.ts and
/// server/cmd/conformance, and must produce the SAME answer for every case.
///
/// This is the half of R4 that actually matters. Generating Dart and
/// TypeScript from one schema guarantees the two clients agree about the
/// SHAPE of a message. It does not guarantee they agree about whether an
/// absent optional round-trips as absent, whether an unknown attachment kind
/// costs you the attachment or the whole message, or whether a malformed
/// timestamp is refused. Those are the differences that show up in the field
/// as ghost messages and disagreeing unread counts, and not one of them is a
/// compile error in either language.
///
/// THIS IS A CLIENT-SIDE RUNNER (CANT-74). `tolerate` is the one expectation
/// whose answer differs by side: an enum value this schema version does not
/// know decodes here to the sentinel `unknown` and round-trips as `encoded`,
/// exactly as `roundtrip` does. The Go runner is the server side and treats
/// the same case as `reject`. Which side a runner is on is stated here, once,
/// and the vector file stays one word per case.
///
/// Run: dart run bin/conformance.dart   (from the dart/ directory)
library;

import 'dart:convert';
import 'dart:io';

import 'package:catenary_wire/catenary_wire.dart';

/// Recursively key-sorted JSON, so key ORDER is not asserted and all else is.
Object? _canonicalise(Object? v) {
  if (v is List) return v.map(_canonicalise).toList();
  if (v is Map) {
    final keys = v.keys.map((k) => k as String).toList()..sort();
    return {for (final k in keys) k: _canonicalise(v[k])};
  }
  return v;
}

String canonical(Object? v) => jsonEncode(_canonicalise(v));

void main(List<String> args) {
  final path = args.isNotEmpty ? args.first : '../schema/vectors/vectors.json';
  final file = File(path);
  if (!file.existsSync()) {
    stderr.writeln('conformance: cannot find $path');
    exit(2);
  }

  final doc = jsonDecode(file.readAsStringSync()) as Map<String, dynamic>;
  final cases = (doc['cases'] as List).cast<Map<String, dynamic>>();

  final failures = <String>[];
  void check(String name, bool ok, [String detail = '']) {
    if (!ok) failures.add(name);
    final suffix = detail.isEmpty ? '' : '  ($detail)';
    stdout.writeln('${ok ? 'ok  ' : 'FAIL'}  $name$suffix');
  }

  for (final c in cases) {
    final name = c['name'] as String;
    final kind = c['kind'] as String;
    final expect = c['expect'] as String;
    final input = c['json'];

    final codec = codecs[kind];
    if (codec == null) {
      check(name, false, 'no codec for kind $kind');
      continue;
    }

    if (expect == 'reject') {
      var threw = false;
      var how = '';
      try {
        codec.decode(input);
      } on WireFormatException catch (e) {
        threw = true;
        how = '${e.path}: ${e.message}';
      } catch (e) {
        // Any decode failure counts; the vector asserts refusal, not which
        // exception type carries it.
        threw = true;
        how = e.toString();
      }
      check(name, threw,
          threw ? (how.length > 90 ? how.substring(0, 90) : how) : 'decoded a frame the schema forbids');
      continue;
    }

    Object? decoded;
    try {
      decoded = codec.decode(input);
    } catch (e) {
      check(name, false, 'threw: $e');
      continue;
    }

    if (expect == 'ignore') {
      check(name, decoded == null, decoded == null ? 'ignored' : 'decoded an unknown tag');
      continue;
    }

    if (decoded == null) {
      check(name, false, 'decoder returned null for a known tag');
      continue;
    }

    // `roundtrip` and, on this side, `tolerate`: decode, re-encode, compare.
    final want = canonical(c.containsKey('encoded') ? c['encoded'] : input);
    final got = canonical(codec.encode(decoded));
    check(name, want == got, want == got ? '' : '\n    want $want\n    got  $got');
  }

  // CANT-74 criterion 11: the unknown-value report fires ONCE per (enum, raw)
  // per process. Measured with a value no vector uses, so the loop above has
  // not already spent it, and the same frame decoded twice yields one line.
  {
    final seen = <String>[];
    onUnknownWireValue = seen.add;
    final frame = {'type': 'resync_required', 'reason': 'warn_once_probe', 'log_seq': 1};
    codecs['ServerFrame']!.decode(frame);
    codecs['ServerFrame']!.decode(frame);
    final ok = seen.length == 1 && seen.first.contains('ResyncReason: unknown value "warn_once_probe"');
    check('unknown_value_is_reported_once_per_enum_and_value', ok,
        ok ? seen.first : '${seen.length} report(s): ${seen.join(' | ')}');
  }

  // The probe above is one check beyond the vector file, so the total says
  // so: a failing probe must not read as a failing vector.
  const runnerChecks = 1;
  stdout.writeln(failures.isEmpty
      ? '\nall green — ${cases.length} vectors + $runnerChecks runner check'
      : '\n${failures.length} of ${cases.length + runnerChecks} FAILED');
  exit(failures.isEmpty ? 0 : 1);
}
