// CANT-200's cleartext rule, at the manifest. A development server on the LAN
// is plain http, so the debug and profile builds allow cleartext; a release
// build must not, and the merged release manifest is the main one alone. So:
// `usesCleartextTraffic` is in the debug and profile manifests and nowhere in
// the main one. The other half of the rule, that a release build refuses every
// http:// address, is address_test.dart's.

import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

String manifest(String variant) => File('android/app/src/$variant/AndroidManifest.xml').readAsStringSync();

/// Whether a manifest's source mentions the attribute at all, whatever its
/// value: `="false"` in the main manifest is still a line someone flips.
bool mentionsCleartext(String source) => source.contains('usesCleartextTraffic');

final _allowed = RegExp(r'''<application\b[^>]*\bandroid:usesCleartextTraffic\s*=\s*"true"''');

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
}
