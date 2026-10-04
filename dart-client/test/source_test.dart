/// Source-shape rules, checked against the files themselves, and each proved
/// against a planted line every run: a rule nobody has seen refuse something is
/// a claim about the rule.
///
/// 1. NO FLUTTER. Nothing under `lib/` or `bin/` imports `package:flutter`,
///    `dart:ui` or a Flutter plugin, and nothing the package resolves needs the
///    Flutter SDK — which is what lets a plain `dart` executable host it
///    (CANT-46 ruling 0).
/// 2. TWO FILES (CANT-42 ruling 1). Journal and transport code never names the
///    outbox's file. The outbox's side of this rule lands with the outbox.
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

List<String> outboxFileNamed(String path, String source) =>
    !isOutboxSource(path) && source.contains(outboxFile) ? ['$path names the outbox\'s file'] : [];

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
  });
}
