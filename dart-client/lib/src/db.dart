/// The versioned open of the client's SQLite files — the counterpart of
/// web/src/transport/db.ts, whose database is IndexedDB.
///
/// `package:sqlite3`, hand-written SQL and an in-process migrator: the Go
/// stack's "no ORM, no external migration tool" rule, extended to the Dart
/// client by CANT-42. A file carries its schema version in
/// `PRAGMA user_version` and is brought forward by the numbered steps below.
///
/// A MIGRATION IS A STEP APPENDED TO ITS LIST, never an edit to one already
/// shipped: the file's version is the list's length, and an open runs exactly
/// the steps between the stored version and that one, in order, inside one
/// transaction — so a file is at a whole version or it is at the one before.
///
/// DURABILITY IS STATED, NOT DEFAULTED. Every connection opened here runs
/// `PRAGMA synchronous = FULL`, and a write is reported complete only after
/// its `COMMIT` returns (CANT-24 obligation 1, persist before render).
library;

import 'dart:ffi';
import 'dart:io';

import 'package:ffi/ffi.dart';
import 'package:sqlite3/sqlite3.dart';

/// The journal and the credential (CANT-42 ruling 1 → two files). The outbox
/// is in a file of its own, which nothing on this side names.
const catenaryDbFile = 'catenary.db';

/// The path of [catenaryDbFile] in `directory`, which is the platform's to
/// supply: the app's private data directory, or a rig's temporary one.
String catenaryDbPath(String directory) => directory.endsWith('/') ? '$directory$catenaryDbFile' : '$directory/$catenaryDbFile';

/// `catenary.db`'s migrations.
///
/// Wire records are stored as the generated codec's JSON (`record`) beside the
/// columns the store queries by, so a field added to the schema needs no
/// migration here.
const catenaryMigrations = <String>[
  // 1 · the journal and the credential.
  //
  // `journal_meta` is one row: the cursor, the wipe count, and the generation
  // a wipe stamps (sqlite_journal.dart). `counted` is the evidence log, keyed
  // by its position. `credential` is one row per device, and no journal
  // statement reaches it: a wipe (obligation 4) clears the journal's tables
  // and not this one.
  '''
  CREATE TABLE journal_meta (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    cursor INTEGER,
    wipes INTEGER NOT NULL,
    generation TEXT
  ) STRICT;
  INSERT INTO journal_meta (id, cursor, wipes, generation) VALUES (1, NULL, 0, NULL);

  CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    log_seq INTEGER NOT NULL,
    record TEXT NOT NULL
  ) STRICT;
  CREATE INDEX messages_by_conversation ON messages (conversation_id, seq);

  CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    record TEXT NOT NULL
  ) STRICT;

  CREATE TABLE users (
    id TEXT PRIMARY KEY,
    record TEXT NOT NULL
  ) STRICT;

  CREATE TABLE counted (
    n INTEGER PRIMARY KEY,
    id TEXT NOT NULL
  ) STRICT;

  CREATE TABLE credential (
    device_id TEXT PRIMARY KEY,
    record TEXT NOT NULL
  ) STRICT;
  ''',
];

/// A file whose `user_version` is above what this build knows: it was written
/// by a newer one, and reading it with older statements is a guess.
final class DatabaseTooNew implements Exception {
  const DatabaseTooNew(this.path, this.stored, this.known);

  final String path;
  final int stored;
  final int known;

  @override
  String toString() => 'DatabaseTooNew: $path is at schema version $stored and this build knows $known';
}

/// How long a write waits on another connection's write before it fails with
/// `SQLITE_BUSY`. `package:sqlite3` is synchronous, so the wait is on the
/// calling isolate; the writes it waits behind are single transactions.
const _busyTimeoutMs = 2000;

/// Opens the file at [path], creating it if absent, and runs each missing step
/// of [migrations] once.
Database openMigrated(String path, List<String> migrations) {
  _createPrivate(path);
  final db = sqlite3.open(path);
  try {
    db.execute('PRAGMA busy_timeout = $_busyTimeoutMs');
    db.execute('PRAGMA synchronous = FULL');
    if (db.userVersion != migrations.length) _migrate(db, path, migrations);
    return db;
  } catch (_) {
    db.close();
    rethrow;
  }
}

/// `chmod(2)`, from the C library every process on these platforms already
/// has loaded. `dart:io` can read a file's mode and cannot set one.
final int Function(Pointer<Utf8> path, int mode) _chmod =
    DynamicLibrary.process().lookupFunction<Int32 Function(Pointer<Utf8>, Uint32), int Function(Pointer<Utf8>, int)>('chmod');

/// A file this code creates is created EMPTY and made `0600` before SQLite
/// writes a byte to it, so what it comes to hold — the credential, in
/// `catenary.db` (CANT-42 ruling 3) — was never readable by anybody but its
/// owner. SQLite gives a database's journal the database's own mode. A file
/// that already exists keeps the mode it has. Windows has no such mode, and
/// its per-user data directory is the boundary there.
void _createPrivate(String path) {
  if (Platform.isWindows || path == ':memory:' || path.isEmpty) return;
  final file = File(path);
  if (file.existsSync()) return;
  try {
    file.createSync(exclusive: true);
  } on PathExistsException {
    return; // another context created it between the check and here
  }
  final native = path.toNativeUtf8();
  try {
    if (_chmod(native, 0x180) != 0) {
      // Removed, not left: the next open would find it, take it for a file
      // that already existed, and write into it at whatever mode it has.
      file.deleteSync();
      throw FileSystemException('could not make the database file private (chmod 0600)', path);
    }
  } finally {
    malloc.free(native);
  }
}

void _migrate(Database db, String path, List<String> migrations) {
  // IMMEDIATE, and the version read again inside it: two contexts opening a
  // fresh file at once run the steps once between them.
  db.execute('BEGIN IMMEDIATE');
  try {
    final stored = db.userVersion;
    if (stored > migrations.length) throw DatabaseTooNew(path, stored, migrations.length);
    for (var v = stored; v < migrations.length; v++) {
      db.execute(migrations[v]);
    }
    db.userVersion = migrations.length;
    db.execute('COMMIT');
  } catch (_) {
    // Guarded: after a full disk or an I/O error SQLite has already rolled
    // back, and a second ROLLBACK would throw over the error that matters.
    if (!db.autocommit) db.execute('ROLLBACK');
    rethrow;
  }
}

/// Opens `catenary.db` at [path] at the current version.
Database openCatenaryDb(String path, {List<String> migrations = catenaryMigrations}) => openMigrated(path, migrations);
