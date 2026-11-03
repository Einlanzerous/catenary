// The picker's vocabulary, as the widgets and the store share it. Nothing here
// imports `package:catenary_client` (test/store_boundary_test.dart): the widgets
// hold a [PickedImage] and read a [PickOutcome], and the store does the
// checking and the composing.

import 'dart:typed_data';

/// Said when several files arrive at once and the whole drop is refused. The
/// web client's `ONE_IMAGE_MESSAGE` is the same words, and both are asserted
/// against testdata/media/messages.json (CANT-360). The attach zone shows it
/// when more than one file arrives at once (CANT-356).
const oneImageMessage = 'One image at a time.';

/// The accepted-types line of the attach zone (web `acceptedLine()`): the four
/// types the server's ingest takes and the size cap in MB. [imageCapMb] is
/// asserted equal to the client's `imageCapBytes` in test/attach_test.dart,
/// since the widgets cannot import the client.
const acceptedImageLabels = ['JPEG', 'PNG', 'GIF', 'WebP'];
const imageCapMb = 16;
String acceptedLine() =>
    '${acceptedImageLabels.sublist(0, acceptedImageLabels.length - 1).join(', ')} or ${acceptedImageLabels.last} \u00b7 up to $imageCapMb MB';

/// One image the person chose: where the platform put it, and what to call it.
/// The outbox copies the bytes at compose and never keeps the path.
class PickedImage {
  const PickedImage({required this.path, required this.name});

  final String path;
  final String name;
}

/// One image that arrived as bytes (pasted from the clipboard), with the name
/// to stage it under.
class PastedImage {
  const PastedImage({required this.bytes, required this.name});

  final Uint8List bytes;
  final String name;
}

/// Opens the platform's photo picker for a single image. Null when the person
/// backed out. The shipped one is store/platform.dart's.
typedef ImagePick = Future<PickedImage?> Function();

/// What picking came to. A refusal writes nothing and carries the words that
/// are shown beside the composer.
sealed class PickOutcome {
  const PickOutcome();
}

final class Picked extends PickOutcome {
  const Picked(this.image);

  final PickedImage image;
}

final class PickRefused extends PickOutcome {
  const PickRefused(this.message, {this.fileName, this.kind});

  final String message;

  /// The file's name for the zone's refusal line; null for a refusal of several.
  final String? fileName;

  /// The short type tag beside it (`TIFF`, `2 FILES`); null when there is none.
  final String? kind;
}

/// A send the outbox would not take with its picked file (the connection
/// dropped between the pick and the send, or the client went terminal). Nothing
/// was written; [message] is the outbox's own words.
final class AttachRefused implements Exception {
  const AttachRefused(this.message);

  final String message;

  @override
  String toString() => message;
}
