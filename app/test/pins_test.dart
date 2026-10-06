// The picks CANT-203 ruled on, held by tests so a later ticket cannot move one
// without a test saying so: the path filter of .github/workflows/app.yml
// (ruling 2) and the bundled typeface (ruling 3). And one CANT-220 ruled on:
// the app runs the sqlite3 that dart-client's tests run (its ruling 3).

import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

/// The `paths:` entries under `pull_request:` and `push:` of app.yml, read by
/// line: the app has no `yaml` dependency and the file's shape is fixed.
Map<String, List<String>> workflowPaths() {
  final lines = File('../.github/workflows/app.yml').readAsLinesSync();
  final out = <String, List<String>>{};
  String? event;
  var inPaths = false;
  for (final line in lines) {
    final ev = RegExp(r'^  (\w+):\s*$').firstMatch(line);
    if (ev != null) {
      event = ev.group(1);
      inPaths = false;
      continue;
    }
    if (RegExp(r'^\S').hasMatch(line)) event = null;
    if (event == null) continue;
    if (RegExp(r'^    paths:\s*$').hasMatch(line)) {
      inPaths = true;
      out[event] = [];
      continue;
    }
    final item = RegExp(r'^      - "([^"]+)"\s*$').firstMatch(line);
    if (inPaths && item != null) {
      out[event]!.add(item.group(1)!);
    } else if (inPaths && RegExp(r'^    \S').hasMatch(line)) {
      inPaths = false;
    }
  }
  return out;
}

/// The version a pubspec.lock resolved [package] to.
String lockedVersion(File lock, String package) {
  final lines = lock.readAsLinesSync();
  final at = lines.indexOf('  $package:');
  expect(at, isNonNegative, reason: '${lock.path} resolves $package');
  for (final line in lines.skip(at + 1)) {
    if (!line.startsWith('    ')) break;
    final v = RegExp(r'^    version: "([^"]+)"$').firstMatch(line);
    if (v != null) return v.group(1)!;
  }
  fail('${lock.path}: no version under $package');
}

/// A GitHub path glob against a repo-relative file: `dir/**` matches anything
/// beneath `dir/`; anything else is an exact path.
bool matches(String glob, String path) {
  if (glob.endsWith('/**')) return path.startsWith(glob.substring(0, glob.length - 2));
  return glob == path;
}

void main() {
  test('app.yml path filter covers every file outside app/ that app/test reads', () {
    // Collected by scanning app/test sources for File( calls whose argument is
    // a string literal beginning with the parent directory. A path built at run
    // time (an interpolation, a join) slips past this scan, so keep cross-tree
    // reads literal.
    final reads = <String>{};
    final literal = RegExp(r'''File\(\s*['"]\.\./([^'"$]+)['"]''');
    for (final f in Directory('test').listSync(recursive: true).whereType<File>().where((f) => f.path.endsWith('.dart'))) {
      for (final m in literal.allMatches(f.readAsStringSync())) {
        reads.add(m.group(1)!);
      }
    }
    expect(reads, isNotEmpty, reason: 'the scan found nothing; it has rotted');
    expect(reads, containsAll(['web/src/styles/tokens.css', 'web/smoke.ts', 'server/spec/testdata/read-fraction.json']));

    final paths = workflowPaths();
    expect(paths.keys, containsAll(['pull_request', 'push']), reason: 'parsed both path lists');
    final missing = [
      for (final event in ['pull_request', 'push'])
        for (final read in reads)
          if (!paths[event]!.any((g) => matches(g, read))) '$event: $read',
    ];
    expect(missing, isEmpty, reason: 'read by app/test but matched by no path in app.yml');
  });

  test('the typeface is bundled: both families at 400, 500, 600, files present, no google_fonts', () {
    final pubspec = File('pubspec.yaml').readAsStringSync();
    for (final family in ['IBM Plex Sans', 'IBM Plex Mono']) {
      final start = pubspec.indexOf('- family: $family');
      expect(start, isNonNegative, reason: '$family declared');
      final next = pubspec.indexOf('- family:', start + 1);
      final block = pubspec.substring(start, next < 0 ? pubspec.length : next);
      final assets = RegExp(r'asset:\s*(\S+)\s+weight:\s*(\d+)').allMatches(block).toList();
      expect({for (final a in assets) a.group(2)}, {'400', '500', '600'}, reason: '$family weights');
      for (final a in assets) {
        expect(File(a.group(1)!).existsSync(), isTrue, reason: '${a.group(1)} exists');
      }
    }
    for (final f in Directory('lib').listSync(recursive: true).whereType<File>().where((f) => f.path.endsWith('.dart'))) {
      expect(f.readAsStringSync().contains('package:google_fonts'), isFalse, reason: '${f.path} imports google_fonts');
    }
    expect(pubspec.contains('google_fonts'), isFalse);
  });

  test('the app and dart-client resolve one sqlite3, and the app overrides nothing', () {
    // dart-client's suite is what exercises the journal, the credential store
    // and the outbox over real SQLite files. A version it does not run is a
    // version nothing tested, so the two lockfiles name the same one.
    expect(lockedVersion(File('pubspec.lock'), 'sqlite3'), lockedVersion(File('../dart-client/pubspec.lock'), 'sqlite3'));
    expect(File('pubspec.yaml').readAsStringSync().contains('dependency_overrides'), isFalse);
  });
}
