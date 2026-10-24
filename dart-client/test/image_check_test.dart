/// The picker's pre-check against the one shared fixture set (CANT-312,
/// CANT-317): the same verdicts the Go ingest test asserts against the real
/// server, so a client list or cap that drifts from the server's fails here.
library;

import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:catenary_client/catenary_client.dart';
import 'package:test/test.dart';

final _dir = Directory('../testdata/media');
final _manifest = jsonDecode(File('${_dir.path}/manifest.json').readAsStringSync()) as Map<String, dynamic>;

ImageVerdict verdictOf(Uint8List bytes) => checkImage(Uint8List.sublistView(bytes, 0, bytes.length < imageSniffBytes ? bytes.length : imageSniffBytes), bytes.length);

void main() {
  test('the cap is the manifest\'s, which the Go test holds to DefaultMaxImageBytes', () {
    expect(imageCapBytes, _manifest['imageCapBytes']);
  });

  for (final f in (_manifest['files'] as List).cast<Map<String, dynamic>>()) {
    final name = f['name'] as String;
    final accept = f['verdict'] == 'accept';
    test('$name is ${accept ? 'accepted' : 'refused (${f['reason']})'}, as the server says (${f['status']})', () {
      final bytes = File('${_dir.path}/$name').readAsBytesSync();
      expect(verdictOf(bytes), accept ? ImageVerdict.accept : ImageVerdict.notSupported);
    });
  }

  test('the oversize case, built here as the manifest says, is refused for size and not for type', () {
    final o = _manifest['oversize'] as Map<String, dynamic>;
    final header = [for (var i = 0; i < (o['headerHex'] as String).length; i += 2) int.parse((o['headerHex'] as String).substring(i, i + 2), radix: 16)];
    expect(o['totalBytes'], 'imageCapBytes + 1');
    final total = (_manifest['imageCapBytes'] as int) + 1;
    final bytes = Uint8List(total)..setRange(0, header.length, header);
    expect(bytes.sublist(header.length).every((b) => b == o['padByte']), isTrue);
    expect(verdictOf(bytes), ImageVerdict.tooLarge);
    // One byte under is fine.
    expect(checkImage(Uint8List.sublistView(bytes, 0, imageSniffBytes), imageCapBytes), ImageVerdict.accept);
  });

  test('a huge file of the wrong type is refused for its type', () {
    expect(checkImage(Uint8List.fromList('<svg xmlns'.codeUnits), imageCapBytes * 2), ImageVerdict.notSupported);
  });

  test('the extension is never evidence: text named .jpg is refused, an empty file too', () {
    expect(checkImage(Uint8List.fromList('hello jpeg'.codeUnits), 10), ImageVerdict.notSupported);
    expect(checkImage(Uint8List(0), 0), ImageVerdict.notSupported);
  });

  test('the refusals say what the uploader says for the server\'s 415 and 413', () {
    expect(imageRefusal(ImageVerdict.notSupported), uploadImageType);
    expect(imageRefusal(ImageVerdict.tooLarge), uploadImageTooLarge);
    expect(imageRefusal(ImageVerdict.accept), isNull);
  });
}
