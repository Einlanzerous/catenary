/// Source-shape rules, checked against the files themselves, and each proved
/// against a planted line every run: a rule nobody has seen refuse something is
/// a claim about the rule.
///
/// 1. NO FLUTTER. Nothing under `lib/` or `bin/` imports `package:flutter`,
///    `dart:ui` or a Flutter plugin, and nothing the package resolves needs the
///    Flutter SDK — which is what lets a plain `dart` executable host it
///    (CANT-46 ruling 0).
/// 2. TWO FILES (CANT-42 ruling 1). Journal and transport code never names the
///    outbox's file, and outbox code never names the journal's.
/// 3. NO MONOTONIC CLOCK (CANT-31 §1), and NO HEARTBEAT NUMBER that did not
///    arrive on `ready` (CANT-23): the reference's own two greps, over this
///    package's source.
/// 4. A STALE REFUSAL MOVES THE EPOCH (CANT-199). Every branch of the transport
///    that sees `JournalStale` increments the epoch, and the transport's
///    journal write calls are the three that were looked at: a fourth has to
///    be classified here before it lands.
library;

import 'dart:convert';
import 'dart:io';

import 'package:test/test.dart';

final _directive = RegExp(r'''^\s*(?:import|export)\s+['"]([^'"]+)['"]''', multiLine: true);

/// The URIs a Dart source imports or exports.
List<String> directives(String source) => [for (final m in _directive.allMatches(source)) m.group(1)!];

/// Every Dart file under `lib/` and `bin/`, by path relative to the package.
Map<String, String> sources() {
  final found = <String, String>{};
  for (final root in ['lib', 'bin']) {
    final dir = Directory(root);
    if (!dir.existsSync()) continue;
    for (final f in dir.listSync(recursive: true).whereType<File>()) {
      if (f.path.endsWith('.dart')) found[f.path] = f.readAsStringSync();
    }
  }
  return found;
}

/// Whether the package at `root` needs the Flutter SDK: its pubspec depends on
/// it or constrains it, which is what makes a package a Flutter plugin.
bool needsFlutter(String pubspec) =>
    RegExp(r'^\s*sdk:\s*flutter\s*$', multiLine: true).hasMatch(pubspec) || RegExp(r'^  flutter:', multiLine: true).hasMatch(pubspec);

/// The package roots `dart pub get` resolved, by name.
Map<String, String> resolvedPackages() {
  final config = jsonDecode(File('.dart_tool/package_config.json').readAsStringSync()) as Map<String, dynamic>;
  return {
    for (final p in (config['packages'] as List).cast<Map<String, dynamic>>())
      p['name'] as String: Uri.parse('.dart_tool/').resolve(p['rootUri'] as String).toFilePath(),
  };
}

/// Why `source` breaks the no-Flutter rule, or an empty list.
List<String> flutterImports(String path, String source, bool Function(String package) isFlutterPackage) => [
      for (final uri in directives(source))
        if (uri == 'dart:ui' || uri.startsWith('dart:ui/'))
          '$path imports $uri'
        else if (uri.startsWith('package:') && isFlutterPackage(uri.substring('package:'.length).split('/').first))
          '$path imports $uri, which needs the Flutter SDK',
    ];

/// The outbox's file, spelled here in two halves so this file is not itself a
/// place the journal's side names it.
const outboxFile = 'catenary-' 'outbox';

/// The outbox's own sources, which are the only ones that may name its file.
bool isOutboxSource(String path) => path.split('/').any((part) => part.startsWith('outbox'));

/// The rule is the LIBRARY's. A driver under `bin/` hosts the journal and the
/// outbox side by side and is neither.
bool isLibrary(String path) => path.startsWith('lib/');

List<String> outboxFileNamed(String path, String source) =>
    isLibrary(path) && !isOutboxSource(path) && source.contains(outboxFile) ? ['$path names the outbox\'s file'] : [];

/// The journal's file, and the only ways to open it: its name, db.dart's
/// names for it, and the two modules that hold a connection to it.
final journalFile = RegExp(r'catenary\.db|catenaryDb|openCatenaryDb|catenaryMigrations|sqlite_journal\.dart|credential_store\.dart');

List<String> journalFileNamed(String path, String source) =>
    isLibrary(path) && isOutboxSource(path) && journalFile.hasMatch(source) ? ['$path names the journal\'s file'] : [];

/// Source with comments removed, so prose citing a number is not code.
String code(String source) => source.replaceAll(RegExp(r'/\*[\s\S]*?\*/'), '').replaceAll(RegExp(r'//.*$', multiLine: true), '');

/// A monotonic clock: a device that slept measures six hours as a few seconds
/// on one, so nothing is scheduled or measured on it.
final monotonic = RegExp(r'\bStopwatch\b|Timeline\.now|\.elapsed(Ticks|Micro|Milli)');

final heartbeatDefault = RegExp(r'\b(35|105)\b');
final heartbeatAssigned = RegExp(r'(heartbeat|interval|pong|ping|missed)\w*\s*[:=]\s*[1-9]\d', caseSensitive: false);

/// The `Journal` members the transport calls that write nothing.
const journalReads = {'cursor', 'snapshot', 'holdsConversation', 'holdsUser', 'messageCount', 'headSeqTotal'};

/// Every other `_journal.` call site in the transport, by member: its writes.
/// `applyLive` is two sites, the `skipStaleCatchUp` control's and the guarded
/// one. A `wipe` skips the generation check, so it is never refused as stale.
const journalWrites = {'applyLive': 2, 'applyPage': 1, 'wipe': 1};

Map<String, int> journalWriteCalls(String transport) {
  final found = <String, int>{};
  for (final m in RegExp(r'\b_journal\s*\.\s*([A-Za-z_]\w*)').allMatches(code(transport))) {
    final member = m.group(1)!;
    if (!journalReads.contains(member)) found[member] = (found[member] ?? 0) + 1;
  }
  return found;
}

/// The block whose `{` is at or after `from`, braces included.
String blockAt(String source, int from) {
  final open = source.indexOf('{', from);
  var depth = 0;
  for (var i = open; i < source.length; i++) {
    if (source[i] == '{') depth++;
    if (source[i] == '}' && --depth == 0) return source.substring(open, i + 1);
  }
  throw StateError('unbalanced braces after offset $from');
}

/// Every handler in `transport` that sees `JournalStale`: an `on JournalStale`
/// clause, or a bare `catch` whose body names it.
List<String> staleBranches(String transport) {
  final c = code(transport);
  return [
    for (final m in RegExp(r'\bon\s+JournalStale\b[^{]*\{|\bcatch\s*\([^)]*\)\s*\{').allMatches(c))
      if (m.group(0)!.contains('JournalStale') || blockAt(c, m.end - 1).contains('JournalStale')) m.group(0)! + blockAt(c, m.end - 1),
  ];
}

/// Why `transport` breaks rule 4, or an empty list.
List<String> staleEpochRule(String transport) {
  final calls = journalWriteCalls(transport);
  final branches = staleBranches(transport);
  return [
    for (final name in {...calls.keys, ...journalWrites.keys})
      if (calls[name] != journalWrites[name])
        '_journal.$name is called at ${calls[name] ?? 0} site(s), and ${journalWrites[name] ?? 0} were classified: say whether it can be refused as stale',
    if (branches.length != 2) '${branches.length} branches see JournalStale, and two were looked at: the live write\'s and the page\'s',
    for (final b in branches)
      if (!RegExp(r'\b_epoch\+\+').hasMatch(b)) 'a branch that sees JournalStale does not increment the epoch: ${b.split('\n').first.trim()}',
  ];
}

void main() {
  final packages = resolvedPackages();
  bool isFlutterPackage(String name) {
    if (name == 'flutter' || name.startsWith('flutter_')) return true;
    final root = packages[name];
    if (root == null) return false;
    final pubspec = File('${root}pubspec.yaml');
    return pubspec.existsSync() && needsFlutter(pubspec.readAsStringSync());
  }

  test('there are sources to check', () {
    expect(sources().keys, contains('lib/catenary_client.dart'));
    expect(sources().keys.where((p) => p.startsWith('lib/src/')), isNotEmpty);
  });

  test('no file under lib/ or bin/ imports package:flutter, dart:ui or a Flutter plugin', () {
    expect([for (final MapEntry(:key, :value) in sources().entries) ...flutterImports(key, value, isFlutterPackage)], isEmpty);
  });

  test('nothing the package resolves needs the Flutter SDK', () {
    expect(packages.keys, contains('sqlite3'));
    expect(packages.keys.where(isFlutterPackage), isEmpty);
    expect(needsFlutter(File('pubspec.yaml').readAsStringSync()), isFalse);
  });

  test('the no-Flutter rule refuses each thing it names, planted', () {
    const plugin = 'name: some_plugin\ndependencies:\n  flutter:\n    sdk: flutter\n';
    const constrained = 'name: other\nenvironment:\n  sdk: ^3.0.0\n  flutter: ">=3.0.0"\n';
    expect(needsFlutter(plugin), isTrue);
    expect(needsFlutter(constrained), isTrue);
    expect(needsFlutter('name: plain\nenvironment:\n  sdk: ^3.0.0\ndependencies:\n  path: ^1.9.0\n'), isFalse);

    bool planted(String name) => name == 'flutter' || name == 'some_plugin';
    expect(flutterImports('lib/a.dart', "import 'package:flutter/widgets.dart';\n", planted), hasLength(1));
    expect(flutterImports('lib/a.dart', "import 'dart:ui';\n", planted), hasLength(1));
    expect(flutterImports('lib/a.dart', '  import "dart:ui" as ui;\n', planted), hasLength(1));
    expect(flutterImports('lib/a.dart', "export 'package:some_plugin/some_plugin.dart';\n", planted), hasLength(1));
    expect(flutterImports('lib/a.dart', "import 'dart:io';\nimport 'package:sqlite3/sqlite3.dart';\n", planted), isEmpty);
  });

  test('journal and transport code never names the outbox\'s file', () {
    expect([for (final MapEntry(:key, :value) in sources().entries) ...outboxFileNamed(key, value)], isEmpty);
  });

  test('the two-file rule refuses the outbox\'s file named from the journal\'s side, planted', () {
    expect(outboxFileNamed('lib/src/db.dart', "const other = '$outboxFile.db';"), hasLength(1));
    expect(outboxFileNamed('lib/src/transport.dart', "open('$outboxFile.db')"), hasLength(1));
    expect(outboxFileNamed('lib/src/outbox/store.dart', "const outboxDbFile = '$outboxFile.db';"), isEmpty);
    expect(outboxFileNamed('lib/src/outbox_store.dart', "const outboxDbFile = '$outboxFile.db';"), isEmpty);
    expect(outboxFileNamed('lib/src/db.dart', "const catenaryDbFile = 'catenary.db';"), isEmpty);
    expect(outboxFileNamed('bin/driver.dart', "open('$outboxFile.db')"), isEmpty, reason: 'a driver hosts both and is neither');
  });

  test('outbox code never names the journal\'s file, or opens it', () {
    final outbox = sources().keys.where((p) => isLibrary(p) && isOutboxSource(p)).toList();
    expect(outbox, containsAll(['lib/src/outbox/store.dart', 'lib/src/outbox/outbox.dart', 'lib/src/outbox/transport_adapter.dart']), reason: 'there is outbox code to check');
    expect([for (final MapEntry(:key, :value) in sources().entries) ...journalFileNamed(key, value)], isEmpty);
  });

  test('the two-file rule refuses the journal\'s file named from the outbox\'s side, planted', () {
    expect(journalFileNamed('lib/src/outbox/store.dart', "sqlite3.open('\$dir/catenary.db')"), hasLength(1));
    expect(journalFileNamed('lib/src/outbox/store.dart', 'openCatenaryDb(path)'), hasLength(1));
    expect(journalFileNamed('lib/src/outbox/store.dart', 'catenaryDbPath(dir)'), hasLength(1));
    expect(journalFileNamed('lib/src/outbox/outbox.dart', "import '../sqlite_journal.dart';"), hasLength(1));
    expect(journalFileNamed('lib/src/outbox/outbox.dart', "import '../credential_store.dart';"), hasLength(1));
    expect(journalFileNamed('lib/src/outbox/store.dart', "import '../db.dart';\nopenMigrated(path, outboxMigrations)"), isEmpty, reason: 'the versioned open is shared; the file is not');
    expect(journalFileNamed('lib/src/sqlite_journal.dart', 'openCatenaryDb(path)'), isEmpty);
  });

  test('no monotonic clock anywhere under lib/', () {
    for (final MapEntry(:key, :value) in sources().entries) {
      expect(monotonic.hasMatch(code(value)), isFalse, reason: key);
    }
    expect(monotonic.hasMatch(code('final w = Stopwatch()..start(); // timing')), isTrue, reason: 'planted');
    expect(monotonic.hasMatch(code('// a Stopwatch would be wrong here')), isFalse, reason: 'prose is not code');
  });

  test('no numeric heartbeat constant under lib/', () {
    for (final MapEntry(:key, :value) in sources().entries) {
      final c = code(value);
      expect(heartbeatDefault.hasMatch(c), isFalse, reason: '$key: neither default appears as a number');
      expect(heartbeatAssigned.hasMatch(c), isFalse, reason: '$key: nothing heartbeat-named is assigned a number');
    }
    expect(heartbeatDefault.hasMatch(code('const interval = Duration(seconds: 35);')), isTrue, reason: 'planted');
    expect(heartbeatAssigned.hasMatch(code('var missedPongLimit = 20;')), isTrue, reason: 'planted');
  });

  test('CANT-199 · every branch that sees JournalStale increments the epoch, and the journal writes are the three classified', () {
    final transport = File('lib/src/transport.dart').readAsStringSync();
    expect(journalWriteCalls(transport), journalWrites);
    expect(staleBranches(transport), hasLength(2));
    expect(staleEpochRule(transport), isEmpty);
  });

  test('CANT-199 · the stale-epoch rule refuses each thing it names, planted', () {
    final transport = File('lib/src/transport.dart').readAsStringSync();

    // The increment taken out of each branch in turn.
    const live = 'if (!faults.staleKeepsEpoch) _epoch++;';
    const paged = 'if (e is JournalStale && !faults.staleKeepsEpoch) _epoch++;';
    for (final (line, without) in [(live, ''), (paged, 'if (e is JournalStale) _log.warn(\'stale\');')]) {
      final planted = transport.replaceFirst(line, without);
      expect(planted, isNot(transport), reason: 'the plant landed: $line');
      expect(staleEpochRule(planted).join('\n'), contains('does not increment the epoch'), reason: line);
    }

    // The increment in a comment is not an increment.
    final commented = transport.replaceFirst(live, '// $live');
    expect(staleEpochRule(commented).join('\n'), contains('does not increment the epoch'));

    // A third branch, and a third write path with no branch at all.
    const third = '''
  Future<void> _third() async {
    try {
      await _journal.applyPage(p, faults);
    } on JournalStale catch (_) {
      _catchup.trigger();
    }
  }
''';
    final withThird = staleEpochRule('$transport$third').join('\n');
    expect(withThird, contains('3 branches see JournalStale'));
    expect(withThird, contains('does not increment the epoch'));
    expect(withThird, contains('_journal.applyPage is called at 2 site(s)'));
    expect(staleEpochRule('$transport\nvoid _mark() { _journal.applyReceipt(r, faults); }\n').join('\n'), contains('_journal.applyReceipt is called at 1 site(s), and 0 were classified'));
    expect(staleEpochRule('$transport\nvoid _again() { _journal.wipe(); }\n').join('\n'), contains('_journal.wipe is called at 2 site(s)'));

    // And a transport with no stale handling at all does not pass by matching nothing.
    expect(staleEpochRule('void f() { _journal.cursor; }'), isNotEmpty);
  });
}
