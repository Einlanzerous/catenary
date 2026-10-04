// CANT-200 ruling 2 → the UI isolate, and what keeps a later move off it
// cheap. Two source-shape rules, checked against the files themselves, and
// each proved against planted source every run: a rule nobody has seen refuse
// something is a claim about the rule.
//
// 1. THE BOUNDARY. Nothing under lib/ outside lib/store/ imports
//    `package:catenary_client`. The widgets read the store's own view models,
//    so moving the transport, the journal and the outbox to a background
//    isolate changes lib/store/ and no widget.
// 2. COMMANDS ARE FUTURES. A public method of the store that sends, reads,
//    types, retries or discards returns a `Future`: across an isolate port
//    every one of them is a message and an answer, and a caller written
//    against a synchronous one would have to change.
//
// The plants are source text this test supplies, not files: lib/enroll.dart is
// CANT-208's and does not exist yet, and a rule about it can be shown today
// only over text.

import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

final _directive = RegExp(r'''^\s*(?:import|export)\s+['"]([^'"]+)['"]''', multiLine: true);

/// The URIs a Dart source imports or exports.
List<String> directives(String source) => [for (final m in _directive.allMatches(source)) m.group(1)!];

/// Every Dart file under lib/, by path relative to the package, `/`-separated.
Map<String, String> sources() => {
      for (final f in Directory('lib').listSync(recursive: true).whereType<File>())
        if (f.path.endsWith('.dart')) f.path.replaceAll(r'\', '/'): f.readAsStringSync(),
    };

/// Rule 1: each file outside lib/store/ that imports the client package.
List<String> boundaryBreaks(Map<String, String> sources) => [
      for (final MapEntry(key: path, value: source) in sources.entries)
        if (!path.startsWith('lib/store/'))
          for (final uri in directives(source))
            if (uri.startsWith('package:catenary_client')) '$path imports $uri',
    ];

/// The store: the file the widgets' `ChangeNotifier` lives in.
const storePath = 'lib/store/app_store.dart';

// A member declared at class depth with a return type before its name. A
// constructor has no return type and a getter no parameter list, so neither
// matches.
final _method = RegExp(r'^  (?:static\s+)?([A-Za-z_][\w<>?,. ]*?)\s+(\w+)(?:<[^>]*>)?\(', multiLine: true);

/// The store's public methods, by name, with the return type each declares.
Map<String, String> storeMethods(String source) => {
      for (final m in _method.allMatches(source))
        if (!m.group(2)!.startsWith('_') && m.group(1) != 'return') m.group(2)!: m.group(1)!,
    };

/// A command is known by its verb: `send` or `compose`, `read` or `markRead`,
/// `type` or `typing`, `retry`, `discard` or `delete`, alone or leading a
/// longer name (`retryNow`). `compose` and `delete` are the outbox's and the
/// thread's own words for a send and a discard, so a method named for either
/// is held to the rule too.
final _command = RegExp(r'^(?:send|compose|read|markRead|type|typing|retry|discard|delete)(?:[A-Z]\w*)?$');

/// Rule 2: each command of the store that does not return a `Future`.
List<String> commandBreaks(String store) => [
      for (final MapEntry(key: name, value: type) in storeMethods(store).entries)
        if (_command.hasMatch(name) && !type.startsWith('Future<')) '$storePath: $name returns $type, not a Future',
    ];

void main() {
  final real = sources();

  test('the scan reads the tree: the files this plan names, and the store\'s own methods', () {
    expect(
      real.keys,
      containsAll([
        'lib/main.dart',
        'lib/rail.dart',
        'lib/thread.dart',
        storePath,
        'lib/store/session.dart',
        'lib/store/address.dart',
        'lib/store/platform.dart',
      ]),
    );
    final methods = storeMethods(real[storePath]!);
    expect(methods['start'], 'Future<bool>');
    expect(methods['dispose'], 'void');
    expect(methods.keys, isNot(contains('_show')), reason: 'private methods are not the store\'s surface');
    expect(directives(real[storePath]!), contains('package:catenary_client/catenary_client.dart'),
        reason: 'the store is where the client package is imported');
  });

  test('nothing under lib/ outside lib/store/ imports package:catenary_client', () {
    expect(boundaryBreaks(real), isEmpty);
  });

  test('the boundary fails when an import is planted in lib/enroll.dart', () {
    final planted = {
      ...real,
      'lib/enroll.dart': "import 'package:flutter/material.dart';\nimport 'package:catenary_client/catenary_client.dart';\n",
    };
    expect(boundaryBreaks(planted), ['lib/enroll.dart imports package:catenary_client/catenary_client.dart']);
    // A deep import and an export are the same breach, in a file that exists.
    expect(boundaryBreaks({...real, 'lib/thread.dart': "${real['lib/thread.dart']}\nexport 'package:catenary_client/src/status.dart';\n"}),
        ['lib/thread.dart imports package:catenary_client/src/status.dart']);
    // The same import under lib/store/ is where it belongs.
    expect(boundaryBreaks({...real, 'lib/store/enrollment.dart': "import 'package:catenary_client/catenary_client.dart';\n"}), isEmpty);
  });

  test('every store method that sends, reads, types, retries or discards returns a Future', () {
    expect(commandBreaks(real[storePath]!), isEmpty);
  });

  test('the rule fails on a planted command that does not return a Future, verb by verb', () {
    final store = real[storePath]!;
    const seam = '  /// Stops listening and ends the session';
    expect(store, contains(seam), reason: 'the plant has somewhere to land');
    String plant(String member) => store.replaceFirst(seam, '$member\n\n$seam');

    for (final (member, name, type) in [
      ('  void send(String conversationId, String text) {}', 'send', 'void'),
      ('  void compose(String conversationId, String text) {}', 'compose', 'void'),
      ('  void delete(String clientId) {}', 'delete', 'void'),
      ('  void read(String conversationId) {}', 'read', 'void'),
      ('  void markRead(String conversationId) {}', 'markRead', 'void'),
      ('  void typing(String conversationId, bool on) {}', 'typing', 'void'),
      ('  bool retry(String clientId) => true;', 'retry', 'bool'),
      ('  void retryNow() {}', 'retryNow', 'void'),
      ('  static bool discard(String clientId) => true;', 'discard', 'bool'),
    ]) {
      expect(commandBreaks(plant(member)), ['$storePath: $name returns $type, not a Future'], reason: member);
    }
    // The same commands as futures pass, and so does a method that is no
    // command at all.
    expect(commandBreaks(plant('  Future<void> send(String conversationId, String text) async {}')), isEmpty);
    expect(commandBreaks(plant('  Future<bool> discard(String clientId) async => true;')), isEmpty);
    expect(commandBreaks(plant('  void readjust() {}')), isEmpty);
  });
}
