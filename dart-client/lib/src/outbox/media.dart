/// Where an attachment's media comes from at compose (CANT-201 ruling 0).
///
/// The app hands over what the platform's recorder or picker produced — a path
/// to a file that may be temporary — and the outbox copies it at compose: the
/// bytes are read once, here, and from then on the copy the store holds beside
/// the entry is the only one the outbox depends on. Nothing in the outbox keeps
/// a `MediaSource` after `compose` has returned.
///
/// THE WHOLE VALUE IS IN MEMORY, ONCE. `package:sqlite3` binds a BLOB as one
/// value, so a source is read whole. That is small for a voice note; for a
/// photograph it rests on a size limit CANT-48 has not set, and is the reason
/// ruling 0 names for reopening its pick.
library;

import 'dart:io';
import 'dart:typed_data';

abstract interface class MediaSource {
  /// The media, whole. Called once, by `compose`, before anything is written.
  Future<Uint8List> read();
}

/// A file the platform produced. It is read, never moved or deleted: whether a
/// temporary file is cleaned up is its owner's business.
final class MediaFile implements MediaSource {
  const MediaFile(this.path);

  final String path;

  @override
  Future<Uint8List> read() => File(path).readAsBytes();
}

/// Media already in memory: tests, and a recorder that hands over a buffer.
final class MediaBytes implements MediaSource {
  const MediaBytes(this.bytes);

  final Uint8List bytes;

  @override
  Future<Uint8List> read() async => bytes;
}
