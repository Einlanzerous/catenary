// The picker's vocabulary, as the widgets and the store share it. Nothing here
// imports `package:catenary_client` (test/store_boundary_test.dart): the widgets
// hold a [PickedImage] and read a [PickOutcome], and the store does the
// checking and the composing.

/// One image the person chose: where the platform put it, and what to call it.
/// The outbox copies the bytes at compose and never keeps the path.
class PickedImage {
  const PickedImage({required this.path, required this.name});

  final String path;
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
  const PickRefused(this.message);

  final String message;
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
