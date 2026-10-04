/// The lock every context respects — what replaces a Web Lock (CANT-42 ruling
/// 2 → a SQLite-held lock). Two rules need one: at most one refresh in flight
/// per credential (CANT-31 §2), and exactly one outbox drainer (CANT-36 §8).
///
/// WHAT A CONTEXT IS HERE: an isolate. Two processes on a desktop, or an app
/// and its background isolate, share files and no memory. A Dart mutex does
/// not span isolates and a POSIX file lock is per process, so neither is
/// enough. SQLite's own locking spans both: a connection inside
/// `BEGIN EXCLUSIVE` on a database file excludes every other connection to
/// that file, in another isolate or another process, and the lock is released
/// when the transaction ends, the connection closes or the process dies — a
/// Web Lock's release behaviour. So a named lock is a small dedicated database
/// file per name, held in an exclusive transaction for exactly as long as the
/// lock is.
///
/// A LOCK IS NEVER WAITED FOR ON THE EVENT LOOP. `package:sqlite3` is
/// synchronous, and a blocking wait would freeze the isolate. Acquisition is
/// one non-blocking attempt, retried on a timer.
library;

import 'dart:async';

import 'package:meta/meta.dart';
import 'package:sqlite3/sqlite3.dart';

import 'seams.dart';

/// Runs `fn` while holding the lock named `name`, and releases it when `fn`
/// completes, either way. A caller that needs the result of someone else's
/// work under the same lock re-reads what is persisted once it holds it.
typedef Lock = Future<T> Function<T>(String name, Future<T> Function() fn);

/// A mutex per name, for one isolate. Two holders sharing one of these are two
/// contexts sharing one lock — which is what a test of one process wants, and
/// all a host with nowhere to put a file can offer.
Lock inProcessLock() {
  final tails = <String, Future<void>>{};
  return <T>(String name, Future<T> Function() fn) {
    final prev = tails[name] ?? Future<void>.value();
    final run = prev.then((_) => fn());
    tails[name] = run.then<void>((_) {}, onError: (Object _) {});
    return run;
  };
}

/// How long a waiter leaves between attempts on a lock somebody else holds.
const _retryMs = 20;

/// SQLite's "somebody else holds it": `SQLITE_BUSY`, and `SQLITE_LOCKED` for a
/// holder in this process.
bool _isBusy(SqliteException e) => e.resultCode == 5 || e.resultCode == 6;

/// A named lock held in SQLite: one database file per name under [directory].
final class SqliteLocks {
  SqliteLocks(this.directory, {Timers timers = const SystemTimers()}) : _timers = timers;

  /// Where the lock files live: beside the databases they guard.
  final String directory;
  final Timers _timers;

  /// One connection per name, opened on first use and kept.
  final _connections = <String, Database>{};

  /// This isolate's own queue per name, so two waiters here do not race each
  /// other through the file.
  final _local = inProcessLock();

  /// The file a name is held in. Names are short and ours; anything outside a
  /// safe alphabet is replaced, so a name can never leave [directory].
  String fileFor(String name) {
    final safe = name.replaceAll(RegExp(r'[^A-Za-z0-9._-]'), '_');
    return '${directory.endsWith('/') ? directory : '$directory/'}$safe.lock';
  }

  /// ONE ATTEMPT, never a wait. True when this object now holds `name`; false
  /// when another connection — another isolate's, another process's, or
  /// another `SqliteLocks` — does.
  bool tryAcquire(String name) {
    final db = _connections[name] ??= _open(name);
    try {
      db.execute('BEGIN EXCLUSIVE');
      return true;
    } on SqliteException catch (e) {
      if (_isBusy(e)) return false;
      rethrow;
    }
  }

  /// Opened bare, not through db.dart: the file holds nothing and is only ever
  /// locked, and a statement that READS it — any pragma that does — would
  /// itself wait on the holder. No busy timeout, which is what makes an
  /// attempt an attempt.
  Database _open(String name) => sqlite3.open(fileFor(name))..execute('PRAGMA busy_timeout = 0');

  /// Releases a lock [tryAcquire] took.
  void release(String name) {
    final db = _connections[name];
    if (db != null && !db.autocommit) db.execute('COMMIT');
  }

  /// The [Lock]: waits its turn among this isolate's holders, then attempts
  /// the file, yielding to the event loop between attempts.
  Future<T> call<T>(String name, Future<T> Function() fn) => _local(name, () async {
        while (!tryAcquire(name)) {
          final again = Completer<void>();
          _timers.setTimeout(again.complete, _retryMs);
          await again.future;
        }
        try {
          return await fn();
        } finally {
          release(name);
        }
      });

  /// The connection that holds, or would hold, `name`.
  @visibleForTesting
  Database? connectionFor(String name) => _connections[name];

  /// Closes every connection, releasing whatever they hold.
  void close() {
    for (final db in _connections.values) {
      db.close();
    }
    _connections.clear();
  }
}
