/// The decision-vector runner's own contract (bin/decisions.dart): it passes
/// the shared file, names every kind in it, defers nothing, and fails on each
/// thing the file's RUNNER CONTRACT says it must.
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

/// The first step of a chain case's first call.
Json firstStep(Json c) => ((((c['in'] as Json)['calls'] as List).first as Json)['steps'] as List).first as Json;

void main() {
  test('the shared file passes: every case of every kind, and nothing is deferred', () async {
    final doc = vectors();
    final cases = (doc['cases'] as List).cast<Json>();
    final lines = <String>[];
    final o = await runDecisions(doc, report: lines.add);
    expect(o.failures, isEmpty);
    expect(o.ran, cases.length, reason: 'every case is run');
    expect(o.ran, greaterThan(100));
    expect(o.skipped, 0);
    expect(o.deferredKinds, isEmpty);
    expect(deferred, isEmpty);
    expect(summary(o), matches(RegExp(r'^all green — \d+ decision vectors \+ 17 runner checks; none deferred$')));
    expect(lines.where((l) => l.startsWith('FAIL')), isEmpty);
  });

  test('it names every kind present in the file, and no other', () {
    final present = {for (final c in (vectors()['cases'] as List).cast<Json>()) c['kind'] as String};
    expect(kinds.toSet(), present);
    expect(kinds, hasLength(13));
  });

  test('it fails on a kind it does not name', () async {
    final o = await runDecisions(edited((cases) => firstOf(cases, 'close')['kind'] = 'a_kind_from_the_future'));
    expect(o.failures, contains(contains('unknown kind a_kind_from_the_future')));
  });

  test('it fails on a named kind with no cases', () async {
    for (final kind in kinds) {
      final o = await runDecisions(edited((cases) => cases.removeWhere((c) => c['kind'] == kind)));
      expect(o.failures, contains(startsWith('kind $kind has cases')), reason: kind);
    }
  });

  test('it fails on an unknown field in `in` and in `want`, for every kind', () async {
    for (final side in ['in', 'want']) {
      for (final kind in kinds) {
        final o = await runDecisions(edited((cases) => (firstOf(cases, kind)[side] as Json)['misspelled'] = 1));
        expect(o.failures, contains(contains('$side: unknown field misspelled')), reason: '$kind, $side');
      }
    }
  });

  test('it fails on a wrong answer, a missing field, a duplicate name and a case with no why', () async {
    var o = await runDecisions(edited((cases) => (firstOf(cases, 'backoff_advance')['want'] as Json)['next_ms'] = 1));
    expect(o.failures.single, contains('next_ms'));
    o = await runDecisions(edited((cases) => (firstOf(cases, 'delay')['want'] as Json)['delay_ms'] = 1));
    expect(o.failures.single, contains('delay_ms'));
    o = await runDecisions(edited((cases) => (firstOf(cases, 'close')['in'] as Json).remove('preceding')));
    expect(o.failures.single, contains('missing field preceding'));
    o = await runDecisions(edited((cases) => cases[1]['name'] = cases[0]['name']));
    expect(o.failures, contains(contains('two cases have this name')));
    o = await runDecisions(edited((cases) => cases[0]['why'] = ' '));
    expect(o.failures, contains(contains('no why')));
  });

  test('preceding goes through the generated decoder: a frame it refuses fails the case', () async {
    var o = await runDecisions(edited((cases) {
      final c = cases.firstWhere((c) => c['kind'] == 'close' && (c['in'] as Json)['preceding'] != null);
      ((c['in'] as Json)['preceding'] as Json).remove('retryable');
    }));
    expect(o.failures.single, contains('retryable'));
    o = await runDecisions(edited((cases) {
      final c = cases.firstWhere((c) => c['kind'] == 'close' && (c['in'] as Json)['preceding'] != null);
      ((c['in'] as Json)['preceding'] as Json)['client_id'] = '00000000-0000-4000-8000-000000000001';
    }));
    expect(o.failures.single, contains('a malformed vector'));
  });

  test('a chain transcript fails on what was presented, on a request it does not script, and on the end state', () async {
    // The first step expects a token the client does not hold.
    var o = await runDecisions(edited((cases) => (firstStep(firstOf(cases, 'chain'))['expect'] as Json)['token'] = 'refresh_token_FIXTURE_some_other_token_____'));
    expect(o.failures.where((f) => f.contains('presented token')), isNotEmpty);
    // A call that scripts no steps, where the client sends one.
    o = await runDecisions(edited((cases) => ((((firstOf(cases, 'chain')['in'] as Json)['calls'] as List).first as Json)['steps'] as List).clear()));
    expect(o.failures.where((f) => f.contains('this call scripts 0')), isNotEmpty);
    // The end state names a token the store does not hold.
    o = await runDecisions(edited((cases) => (firstOf(cases, 'chain')['want'] as Json)['refresh_token'] = 'refresh_token_FIXTURE_never_held___________'));
    expect(o.failures.where((f) => f.contains('the store holds refresh token')), isNotEmpty);
    // A header a vector may not set.
    o = await runDecisions(edited((cases) => (firstStep(firstOf(cases, 'chain'))['respond'] as Json)['headers'] = {'Date': 'now'}));
    expect(o.failures, isNotEmpty);
  });

  test('the controls have teeth: noChain and proposeAfresh each fail a chain transcript, and both close mutants fail CANT-177\'s case', () async {
    final lines = <String>[];
    final o = await runDecisions(vectors(), report: lines.add);
    expect(o.failures, isEmpty);
    final caught = RegExp(r'^ok    (faults\.\w+) fails at least one chain transcript  \((\d+) of (\d+)\)$');
    final teeth = {for (final m in lines.map(caught.firstMatch).nonNulls) m.group(1)!: int.parse(m.group(2)!)};
    expect(teeth.keys, ['faults.noChain', 'faults.proposeAfresh']);
    expect(teeth.values, everyElement(greaterThan(0)));
    expect(lines.where((l) => l.startsWith('ok  ') && l.contains('fails $laterCode')), hasLength(2));

    // And the runner fails when a control could not have bitten: with the
    // chain transcripts gone a fault catches nothing, and with CANT-177's case
    // gone a mutant has nothing to fail.
    final noChains = await runDecisions(edited((cases) => cases.removeWhere((c) => c['kind'] == 'chain')));
    expect(noChains.failures.where((f) => f.contains('fails at least one chain transcript')), hasLength(2));
    final gone = await runDecisions(edited((cases) => cases.removeWhere((c) => c['name'] == laterCode)));
    expect(gone.failures.where((f) => f.contains('no case has that name')), hasLength(2));
  });
}
