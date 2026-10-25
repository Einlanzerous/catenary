// CANT-200's cleartext rule, at the manifest. A development server on the LAN
// is plain http, so the debug and profile builds allow cleartext; a release
// build must not, and the merged release manifest is the main one alone. So:
// `usesCleartextTraffic` is in the debug and profile manifests and nowhere in
// the main one. The other half of the rule, that a release build refuses every
// http:// address, is address_test.dart's.

import 'dart:io';

import 'package:catenary/store/voice.dart';
import 'package:flutter_test/flutter_test.dart';

String manifest(String variant) => File('android/app/src/$variant/AndroidManifest.xml').readAsStringSync();

/// Whether a manifest's source mentions the attribute at all, whatever its
/// value: `="false"` in the main manifest is still a line someone flips.
bool mentionsCleartext(String source) => source.contains('usesCleartextTraffic');

final _allowed = RegExp(r'''<application\b[^>]*\bandroid:usesCleartextTraffic\s*=\s*"true"''');

final _stripsRecordAudio = RegExp(
  r'<uses-permission\b(?=[^>]*\bandroid:name\s*=\s*"android\.permission\.RECORD_AUDIO")(?=[^>]*\btools:node\s*=\s*"remove")[^>]*>',
);

/// CANT-327's rule: the main manifest strips `RECORD_AUDIO` exactly while the
/// voice gate is off. Off requires the remove directive; on forbids it.
bool manifestMatchesGate(String source, {required bool voiceEnabled}) =>
    _stripsRecordAudio.hasMatch(source) == !voiceEnabled;

void main() {
  test('the main manifest does not mention usesCleartextTraffic', () {
    expect(mentionsCleartext(manifest('main')), isFalse,
        reason: 'a release build is built from the main manifest alone, and it must not allow cleartext');
  });

  test('the rule refuses a main manifest that carries it', () {
    final planted = manifest('main').replaceFirst('<application', '<application android:usesCleartextTraffic="true"');
    expect(planted, isNot(manifest('main')));
    expect(mentionsCleartext(planted), isTrue);
  });

  for (final variant in ['debug', 'profile']) {
    test('the $variant manifest allows cleartext on its application', () {
      expect(_allowed.hasMatch(manifest(variant)), isTrue);
    });
  }

  group('manifestMatchesGate', () {
    const directive =
        '<uses-permission android:name="android.permission.RECORD_AUDIO" tools:node="remove"/>';
    final withDirective = manifest('main');
    final without = withDirective.replaceFirst(directive, '');

    test('the real main manifest matches the real gate', () {
      expect(
        manifestMatchesGate(manifest('main'), voiceEnabled: voiceSendEnabled),
        isTrue,
        reason:
            'voiceSendEnabled and the RECORD_AUDIO remove line move together (CANT-327)',
      );
    });

    test('the planted sources cover all four cases', () {
      expect(withDirective, contains(directive));
      expect(without, isNot(contains('RECORD_AUDIO')));
      expect(manifestMatchesGate(without, voiceEnabled: true), isTrue);
      expect(manifestMatchesGate(withDirective, voiceEnabled: true), isFalse);
      expect(manifestMatchesGate(withDirective, voiceEnabled: false), isTrue);
      expect(manifestMatchesGate(without, voiceEnabled: false), isFalse);
    });

    test('a bare declaration without the remove node is not a strip', () {
      final bare = withDirective.replaceFirst(
        directive,
        directive.replaceFirst(' tools:node="remove"', ''),
      );
      expect(manifestMatchesGate(bare, voiceEnabled: false), isFalse);
    });
  });
}
