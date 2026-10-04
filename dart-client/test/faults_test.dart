/// Every fault named in web/src/transport/faults.ts exists here under the same
/// name: the lanes set them by name, so a switch this side lacks — or spells
/// differently — is a control that silently controls nothing.
library;

import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:test/test.dart';

/// The field names of `export interface Faults { … }` in faults.ts.
List<String> typescriptFaults(String source) {
  final start = source.indexOf('export interface Faults {');
  if (start < 0) throw StateError('faults.ts declares no `export interface Faults`');
  final body = source.substring(start, source.indexOf('\n}\n', start));
  return [for (final m in RegExp(r'^  ([a-zA-Z]+): boolean$', multiLine: true).allMatches(body)) m.group(1)!];
}

void main() {
  final reference = File('../web/src/transport/faults.ts').readAsStringSync();

  test('the Dart list is faults.ts\'s list, name for name and in its order', () {
    final want = typescriptFaults(reference);
    expect(want.length, greaterThanOrEqualTo(15), reason: 'parsed ${want.length} switches from faults.ts');
    expect(Faults.names, want);
  });

  test('the comparison fails when the lists differ: a switch added to faults.ts, planted', () {
    final planted = reference.replaceFirst('  alwaysTerminal: boolean\n', '  alwaysTerminal: boolean\n  aSwitchFromALaterTicket: boolean\n');
    expect(planted, isNot(reference), reason: 'the plant landed');
    expect(typescriptFaults(planted), isNot(Faults.names));
    expect(typescriptFaults(planted), contains('aSwitchFromALaterTicket'));
  });

  test('every name is a switch: set alone it is on, and nothing else is', () {
    expect(Faults.none.toMap().keys, Faults.names);
    expect(Faults.none.toMap().values, everyElement(isFalse), reason: 'the all-false value is a correct client');
    for (final name in Faults.names) {
      final on = Faults.named([name]).toMap();
      expect(on[name], isTrue, reason: name);
      expect(on.values.where((v) => v), hasLength(1), reason: '$name sets only itself');
    }
    expect(Faults.named(Faults.names).toMap().values, everyElement(isTrue));
  });

  test('a name this list does not know is refused, not ignored', () {
    expect(() => Faults.named(['dedupeByLogSeq', 'dedupeByLogseq']), throwsArgumentError);
  });

  test('the constructor\'s switches are the named ones', () {
    const f = Faults(skipStaleCatchUp: true, keepHeldConversation: true);
    expect([for (final e in f.toMap().entries) if (e.value) e.key], ['skipStaleCatchUp', 'keepHeldConversation']);
    expect(f.skipStaleCatchUp && f.keepHeldConversation && !f.skipWipe, isTrue);
  });
}
