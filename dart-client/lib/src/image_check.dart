/// The picker's pre-check (CANT-247 ruling 1 → B): is this file one the server
/// will take? Decided from the file's first bytes and its length, never from
/// its name or a MIME type the platform guessed.
///
/// THE SERVER IS STILL THE AUTHORITY. `POST /uploads` answers 415 for bytes
/// that are not a decodable image and 413 over the cap, and the uploader maps
/// both to the same words; this check only keeps a person from composing a
/// message that is certain to fail. Its list and its cap are COPIES of the
/// server's (`internal/media/ingest.go`, `DefaultMaxImageBytes` in
/// `internal/config/config.go`), and `test/image_check_test.dart` reads the
/// shared fixtures in `testdata/media` so a copy that drifts fails a build.
library;

import 'dart:typed_data';

import 'upload.dart';

/// The server's default per-image cap (`CATENARY_MAX_IMAGE_BYTES`).
const imageCapBytes = 16 * 1024 * 1024;

/// How many leading bytes [checkImage] needs: the longest signature (WebP's
/// `RIFF....WEBP`) is twelve.
const imageSniffBytes = 12;

enum ImageVerdict { accept, notSupported, tooLarge }

/// JPEG, PNG, GIF and WebP are accepted. HEIC, AVIF, SVG and anything else are
/// not: the type is judged first, so a huge file of the wrong type says so.
ImageVerdict checkImage(Uint8List head, int length) {
  if (!_supported(head)) return ImageVerdict.notSupported;
  if (length > imageCapBytes) return ImageVerdict.tooLarge;
  return ImageVerdict.accept;
}

/// The words for a refusal, the same as the uploader's for the server's 415 and
/// 413; null for an accepted file.
String? imageRefusal(ImageVerdict v) => switch (v) {
      ImageVerdict.accept => null,
      ImageVerdict.notSupported => uploadImageType,
      ImageVerdict.tooLarge => uploadImageTooLarge,
    };

bool _starts(Uint8List b, List<int> sig, [int at = 0]) {
  if (b.length < at + sig.length) return false;
  for (var i = 0; i < sig.length; i++) {
    if (b[at + i] != sig[i]) return false;
  }
  return true;
}

bool _supported(Uint8List b) =>
    _starts(b, const [0xff, 0xd8, 0xff]) ||
    _starts(b, const [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]) ||
    _starts(b, 'GIF87a'.codeUnits) ||
    _starts(b, 'GIF89a'.codeUnits) ||
    (_starts(b, 'RIFF'.codeUnits) && _starts(b, 'WEBP'.codeUnits, 8));
