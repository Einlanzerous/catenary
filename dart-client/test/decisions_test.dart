/// The decision-vector runner's own contract (bin/decisions.dart): it passes
/// the shared file, names every kind in it, defers by name and never by
/// omission, and fails on each thing the file's RUNNER CONTRACT says it must.
library;

import 'dart:convert';
import 'dart:io';

import 'package:test/test.dart';

import '../bin/decisions.dart';

Json vectors() => jsonDecode(File('../internal/client/testdata/decisions.json').readAsStringSync()) as Json;

/// The file with `edit` applied to a deep copy of its cases.
Json edited(void Function(List<Json> cases) edit) {
  final doc = jsonDecode(jsonEncode(vectors())) as Json;
  edit((doc['cases'] as List).cast<Json>());
  return doc;
}

Json firstOf(List<Json> cases, String kind) => cases.firstWhere((c) => c['kind'] == kind);

void main() {
  test('the shared file passes: every close and backoff_* case, and the rest deferred by name', () {
    final doc = vectors();
    final cases = (doc['cases'] as List).cast<Json>();
    final o = runDecisions(doc);
    expect(o.failures, isEmpty);
    const run = ['close', 'backoff_reset', 'backoff_draw', 'backoff_jitter', 'backoff_advance'];
    expect(o.ran, cases.where((c) => run.contains(c['kind'])).length);
    expect(o.ran, greaterThan(30));
    expect(o.ran + o.skipped, cases.length, reason: 'every case is run or deferred; none is dropped');
    expect(o.deferredKinds, ['threshold', 'due', 'delay', 'stamp', 'gate', 'hold', 'refused_hold', 'chain']);
    expect(summary(o), matches(RegExp(r'^all green — \d+ decision vectors \+ 15 runner checks; \d+ deferred by name: threshold, due, delay, stamp, gate, hold, refused_hold, chain$')));
  });

  test('it names every kind present in the file, and no other', () {
    final present = {for (final c in (vectors()['cases'] as List).cast<Json>()) c['kind'] as String};
    expect(kinds.toSet(), present);
    expect(deferred.difference(kinds.toSet()), isEmpty);
    expect(kinds.toSet().difference(deferred), {'close', 'backoff_reset', 'backoff_draw', 'backoff_jitter', 'backoff_advance'});
  });

  test('it fails on a kind it does not name', () {
    final o = runDecisions(edited((cases) => firstOf(cases, 'close')['kind'] = 'a_kind_from_the_future'));
    expect(o.failures, contains(contains('unknown kind a_kind_from_the_future')));
  });

  test('it fails on a named kind with no cases, a deferred one included', () {
    for (final kind in ['backoff_draw', 'chain']) {
      final o = runDecisions(edited((cases) => cases.removeWhere((c) => c['kind'] == kind)));
      expect(o.failures, contains(startsWith('kind $kind has cases')), reason: kind);
    }
  });

  test('it fails on an unknown field in `in` and in `want`', () {
    for (final side in ['in', 'want']) {
      for (final kind in ['close', 'backoff_reset', 'backoff_draw', 'backoff_jitter', 'backoff_advance']) {
        final o = runDecisions(edited((cases) => (firstOf(cases, kind)[side] as Json)['misspelled'] = 1));
        expect(o.failures, contains(contains('$side: unknown field misspelled')), reason: '$kind, $side');
      }
    }
  });

  test('it fails on a wrong answer, a missing field, a duplicate name and a case with no why', () {
    var o = runDecisions(edited((cases) => (firstOf(cases, 'backoff_advance')['want'] as Json)['next_ms'] = 1));
    expect(o.failures.single, contains('next_ms'));
    o = runDecisions(edited((cases) => (firstOf(cases, 'close')['in'] as Json).remove('preceding')));
    expect(o.failures.single, contains('missing field preceding'));
    o = runDecisions(edited((cases) => cases[1]['name'] = cases[0]['name']));
    expect(o.failures, contains(contains('two cases have this name')));
    o = runDecisions(edited((cases) => cases[0]['why'] = ' '));
    expect(o.failures, contains(contains('no why')));
  });

  test('preceding goes through the generated decoder: a frame it refuses fails the case', () {
    var o = runDecisions(edited((cases) {
      final c = cases.firstWhere((c) => c['kind'] == 'close' && (c['in'] as Json)['preceding'] != null);
      ((c['in'] as Json)['preceding'] as Json).remove('retryable');
    }));
    expect(o.failures.single, contains('retryable'));
    o = runDecisions(edited((cases) {
      final c = cases.firstWhere((c) => c['kind'] == 'close' && (c['in'] as Json)['preceding'] != null);
      ((c['in'] as Json)['preceding'] as Json)['client_id'] = '00000000-0000-4000-8000-000000000001';
    }));
    expect(o.failures.single, contains('a malformed vector'));
  });

  test('the two close mutants each fail CANT-177\'s case, and the runner fails when that case is gone', () {
    final lines = <String>[];
    final o = runDecisions(vectors(), report: lines.add);
    expect(o.failures, isEmpty);
    expect(lines.where((l) => l.startsWith('ok  ') && l.contains('fails $laterCode')), hasLength(2));
    final gone = runDecisions(edited((cases) => cases.removeWhere((c) => c['name'] == laterCode)));
    expect(gone.failures.where((f) => f.contains('no case has that name')), hasLength(2));
  });
}
