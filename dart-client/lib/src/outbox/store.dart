/// The outbox's two stores — mirrors web/src/outbox/idb-store.ts and
/// memory-store.ts (CANT-36 §2).
///
/// FILE `catenary-outbox.db`, OWNED BY THE OUTBOX ALONE (CANT-42 ruling 1 →
/// two files). It is not a table in the journal's file: obligation 4's
/// discard-and-bootstrap must not be able to reach an unsent message, and the
/// two stores share no migration lineage. This file never opens the journal's
/// database, and nothing there opens this one. Every future migration of the
/// outbox's file belongs here.
///
/// DURABLE BEFORE IT IS REPORTED. Every connection runs `synchronous = FULL`
/// (the versioned open's rule), and every operation completes only after its
/// `COMMIT` has returned: a row the app shows QUEUED has to survive a power cut
/// just after compose.
///
/// `order` IS DRAWN INSIDE THE INSERT'S OWN TRANSACTION — `BEGIN IMMEDIATE`,
/// read the account's highest `order`, insert at that plus one, `COMMIT` — and
/// never from `AUTOINCREMENT` or a rowid, which would allocate outside the rule
/// §2 states.
library;

import 'dart:async';
import 'dart:convert';

import 'package:catenary_wire/catenary_wire.dart';
import 'package:meta/meta.dart';
import 'package:sqlite3/sqlite3.dart';

import '../db.dart';
import 'types.dart';

const outboxDbFile = 'catenary-outbox.db';

/// The path of [outboxDbFile] in `directory`.
String outboxDbPath(String directory) => directory.endsWith('/') ? '$directory$outboxDbFile' : '$directory/$outboxDbFile';

/// The outbox file's migrations. The entry is stored as its own JSON beside
/// the columns the store queries by.
const outboxMigrations = <String>[
  // 1 · the outbox. Unique on (account_id, "order"): two entries of one
  // account can never share an `order`, so a collision is a refused write
  // rather than a silent tie in the drain.
  '''
  CREATE TABLE outbox (
    client_id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    "order" INTEGER NOT NULL,
    record TEXT NOT NULL
  ) STRICT;
  CREATE UNIQUE INDEX outbox_by_account_order ON outbox (account_id, "order");
  ''',
];

int byOrder(OutboxEntry a, OutboxEntry b) => a.accountId == b.accountId ? a.order.compareTo(b.order) : a.accountId.compareTo(b.accountId);

/// The in-memory store: tests, and a context with nowhere durable to put it.
/// It copies on the way in and out, so the outbox can never mutate a stored
/// record by holding a reference to it. It is not durable, and nothing that
/// must survive a relaunch may use it.
final class MemoryOutboxStore implements OutboxStore {
  MemoryOutboxStore([List<OutboxEntry> seed = const []]) {
    for (final e in seed) {
      _rows[e.clientId] = e.copy();
    }
  }

  final _rows = <Uuid, OutboxEntry>{};

  @override
  Future<OutboxEntry> add(OutboxEntry entry) async {
    // Synchronous from read to write, which is this store's whole transaction.
    var highest = 0;
    for (final e in _rows.values) {
      if (e.accountId == entry.accountId && e.order > highest) highest = e.order;
    }
    final stored = entry.copy()..order = highest + 1;
    _rows[stored.clientId] = stored;
    return stored.copy();
  }

  @override
  Future<void> put(OutboxEntry entry) async {
    _rows[entry.clientId] = entry.copy();
  }

  @override
  Future<OutboxEntry?> update(Uuid clientId, void Function(OutboxEntry e) mutate) async {
    final held = _rows[clientId];
    if (held == null) return null;
    final next = held.copy();
    mutate(next);
    _rows[clientId] = next;
    return next.copy();
  }

  @override
  Future<void> delete(Uuid clientId) async {
    _rows.remove(clientId);
  }

  @override
  Future<List<OutboxEntry>> list() async => [for (final e in _rows.values) e.copy()]..sort(byOrder);

  @override
  void close() {}
}

/// The durable store: `catenary-outbox.db`, one connection of its own.
final class SqliteOutboxStore implements OutboxStore {
  SqliteOutboxStore._(this._db, this._faults);

  /// Opens the outbox's file at [path], running any missing migration.
  static SqliteOutboxStore open(String path, [StoreFaults faults = StoreFaults.none]) {
    final db = openMigrated(path, outboxMigrations);
    if (faults.relaxedDurability) db.execute('PRAGMA synchronous = NORMAL');
    return SqliteOutboxStore._(db, faults);
  }

  final Database _db;
  final StoreFaults _faults;

  /// This store's one connection, the one every write goes through.
  @visibleForTesting
  Database get database => _db;

  /// One write transaction. Under `neverCompletes` the statements run and the
  /// transaction is rolled back, and the future it returns never completes.
  Future<T> _write<T>(T Function() statements) {
    _db.execute('BEGIN IMMEDIATE');
    final T result;
    try {
      result = statements();
      if (_faults.neverCompletes) {
        _db.execute('ROLLBACK');
        return Completer<T>().future;
      }
      _db.execute('COMMIT');
    } catch (e, st) {
      if (!_db.autocommit) _db.execute('ROLLBACK');
      return Future.error(e, st);
    }
    return Future.value(result);
  }

  /// By `client_id`, and only by it: an `order` another entry of the account
  /// already holds is a refused write, never a row replaced.
  void _insert(OutboxEntry e) => _db.execute(
        'INSERT INTO outbox (client_id, account_id, "order", record) VALUES (?, ?, ?, ?) '
        'ON CONFLICT (client_id) DO UPDATE SET account_id = excluded.account_id, "order" = excluded."order", record = excluded.record',
        [e.clientId, e.accountId, e.order, jsonEncode(e.toJson())],
      );

  OutboxEntry _decode(Row row) => OutboxEntry.fromJson(jsonDecode(row['record'] as String) as Map<String, dynamic>);

  @override
  Future<OutboxEntry> add(OutboxEntry entry) => _write(() {
        // THE ACCOUNT'S HIGHEST ORDER, read inside this transaction, so nothing
        // another context commits can land between the read and the insert.
        final highest = _db.select('SELECT max("order") AS highest FROM outbox WHERE account_id = ?', [entry.accountId]).single['highest'] as int?;
        final stored = entry.copy()..order = (highest ?? 0) + 1;
        // INSERT, not REPLACE: a clientId already stored is a refused write.
        _db.execute(
          'INSERT INTO outbox (client_id, account_id, "order", record) VALUES (?, ?, ?, ?)',
          [stored.clientId, stored.accountId, stored.order, jsonEncode(stored.toJson())],
        );
        return stored;
      });

  @override
  Future<void> put(OutboxEntry entry) => _write(() => _insert(entry));

  @override
  Future<OutboxEntry?> update(Uuid clientId, void Function(OutboxEntry e) mutate) => _write(() {
        final rows = _db.select('SELECT record FROM outbox WHERE client_id = ?', [clientId]);
        if (rows.isEmpty) return null;
        final held = _decode(rows.single);
        mutate(held);
        _insert(held);
        return held;
      });

  @override
  Future<void> delete(Uuid clientId) => _write(() => _db.execute('DELETE FROM outbox WHERE client_id = ?', [clientId]));

  @override
  Future<List<OutboxEntry>> list() async => [for (final row in _db.select('SELECT record FROM outbox ORDER BY account_id, "order"')) _decode(row)];

  @override
  void close() => _db.close();
}
