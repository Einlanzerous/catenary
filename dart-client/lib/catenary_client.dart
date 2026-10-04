/// The Catenary client's protocol half (CANT-42): a module-for-module twin of
/// web/src/transport and web/src/outbox, over real SQLite files.
///
/// NO FLUTTER. Nothing under this package imports `package:flutter`, `dart:ui`
/// or a Flutter plugin, so a plain `dart` executable can host it as a driver
/// (CANT-46 ruling 0). Everything a platform has to supply reaches it through
/// a seam. Wire types are `catenary_wire`'s generated ones and are never
/// written by hand here.
library;

export 'src/backoff.dart';
export 'src/closes.dart';
export 'src/credential.dart';
export 'src/db.dart';
export 'src/faults.dart';
export 'src/io_seams.dart';
export 'src/journal.dart';
export 'src/seams.dart';
export 'src/sqlite_journal.dart';
export 'src/status.dart';
export 'src/terminal.dart';
export 'src/transport.dart';
