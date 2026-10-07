/// The durable journal — the SQLite twin of web/src/transport/idb-journal.ts.
/// It owns CANT-24 obligation 1's DURABLE half: "the cursor is written durably
/// before any message it covers is shown or counted".
///
/// WHERE: `catenary.db`, through `openCatenaryDb` (db.dart). Messages,
/// conversations and users as wire JSON keyed by `id`, the cursor, the wipe
/// count and the generation in `journal_meta`, and the evidence log in
/// `counted`. The credential shares the file and never this code's statements:
/// a wipe (obligation 4) clears the journal's tables and not `credential`.
///
/// WHOSE IT IS (CANT-230): `journal_meta.owner`, the `user_id` the journal was
/// written for. A wipe leaves it. `claim` compares it with the account about
/// to use the journal, inside one transaction, and wipes a journal that is
/// another account's before anything is built over it.
///
/// ONE TRANSACTION PER WRITE. A page's messages, conversations, users, counted
/// ids and cursor go in one `BEGIN IMMEDIATE … COMMIT`, and the in-memory
/// mirror — which is what `snapshot()`, `cursor` and the `Applied` the
/// transport emits read — is updated only after `COMMIT` has returned. A
/// transaction that fails (a full disk, a file another context holds past the
/// busy timeout) lands none of it, and the write's future fails with its
/// error. It does not fall back to memory: a journal that silently stops being
/// durable is the one failure this exists to rule out.
///
/// A RELAUNCH RESUMES: `SqliteJournal.open` reads the whole journal back, and
/// a transport over it dials with `resume_from_log_seq` equal to the stored
/// cursor and asks `/sync` from it.
///
/// READS ARE SYNCHRONOUS, from the mirror, which is what this journal's last
/// completed transaction left.
///
/// TWO CONTEXTS ARE TWO WRITERS — a second process on a desktop, or E7's
/// background isolate — each with its own mirror, over one file. Records from
/// either are the server's and upsert harmlessly; what one can break for the
/// other is a WIPE, which clears messages the other's mirror — and so its
/// cursor — still covers. So a wipe stamps the journal with a fresh
/// `generation`, and every other write reads the stored generation inside its
/// own transaction first: a journal wiped under this context refuses the write
/// (`JournalStale`), reloads its mirror from what is stored, and the
/// transport's catch-up asks again from the stored cursor. The cursor written
/// is never below the stored one, so two contexts never move it backward
/// (obligation 2).
library;

import 'dart:convert';
import 'dart:math';

import 'package:catenary_wire/catenary_wire.dart';
import 'package:meta/meta.dart';
import 'package:sqlite3/sqlite3.dart';

import 'db.dart';
import 'journal.dart';

/// A write refused because another context wiped the journal since this one's
/// mirror was read; the mirror has been reloaded from the file.
final class JournalStale implements Exception {
  const JournalStale(this.source);

  final AppliedSource source;

  @override
  String toString() =>
      'JournalStale: the ${source.name} write was refused — the journal was wiped under this context, and it has reloaded';
}

final class SqliteJournalOptions {
  const SqliteJournalOptions({
    this.migrations = catenaryMigrations,
    this.midWrite,
    this.splitCursor = false,
    this.ignoreGeneration = false,
  });

  /// Default: `catenaryMigrations`. A test passes a longer list to exercise a
  /// bump.
  final List<String> migrations;

  /// TEST SEAM. Called once every statement of a write has run and before its
  /// `COMMIT` — the moment a process killed mid-page dies at. A test throws
  /// here, and the write lands nothing. A claim that writes fires it too, with
  /// `AppliedSource.wipe`, whether or not it wiped.
  final void Function(Database db, AppliedSource source)? midWrite;

  /// NEGATIVE CONTROL for the one-transaction rule, never set outside a test:
  /// the cursor goes in a second transaction after the records, so a death
  /// between the two keeps a page's messages without the cursor that covers
  /// them. `midWrite` fires in the second.
  final bool splitCursor;

  /// NEGATIVE CONTROL for the two-context rule, never set outside a test:
  /// writes skip the stored-generation check, so a context writes its own
  /// mirror's cursor over a journal another context wiped.
  final bool ignoreGeneration;
}

enum _Part { all, records, cursor }

final class SqliteJournal extends StagedJournal {
  SqliteJournal._(this._db, _Loaded loaded, this._opts)
      : _generation = loaded.generation,
        super(loaded.state);

  final Database _db;
  final SqliteJournalOptions _opts;

  /// The wipe generation this mirror was read at, or last wrote.
  String? _generation;

  /// Opens `catenary.db` at [path] (running any missing migration) and reads
  /// the journal back. What it holds is what the last committed transaction
  /// left.
  static SqliteJournal open(String path, [SqliteJournalOptions opts = const SqliteJournalOptions()]) {
    final db = openCatenaryDb(path, migrations: opts.migrations);
    try {
      return SqliteJournal._(db, _load(db), opts);
    } catch (_) {
      db.close();
      rethrow;
    }
  }

  /// This journal's one connection, the one every write goes through.
  @visibleForTesting
  Database get database => _db;

  /// Closes the connection. A write after this fails.
  void close() => _db.close();

  @override
  Future<void> commit(JournalState next, JournalDelta delta) async {
    final generation = delta.wiped ? _newGeneration() : _generation;
    try {
      if (_opts.splitCursor && !delta.wiped) {
        _write(delta, _Part.records, generation);
        _write(delta, _Part.cursor, generation);
      } else {
        // A wipe's count is the STORED count plus one, read in the wipe's own
        // transaction: two contexts that each wipe once have wiped twice. The
        // owner is the stored one too, which a wipe does not write.
        final (:wipes, :owner) = _write(delta, _Part.all, generation);
        if (delta.wiped) {
          next.wipes = wipes;
          next.owner = owner;
        }
      }
    } on JournalStale {
      final loaded = _load(_db);
      state = loaded.state;
      _generation = loaded.generation;
      rethrow;
    }
    _generation = generation;
  }

  @override
  Future<Applied?> claim(Uuid accountId) async {
    // Read first, outside any write transaction: the journal's own account
    // claiming it again, which is every ordinary launch, writes nothing and
    // waits on nobody's lock.
    if (_storedOwner() == accountId) {
      state.owner = accountId;
      return null;
    }
    final bool wiped;
    _db.execute('BEGIN IMMEDIATE');
    try {
      // And again inside it, so nothing another context commits lands between
      // the check and the write.
      final held = _storedOwner();
      if (held == accountId) {
        _db.execute('COMMIT');
        state.owner = accountId;
        return null;
      }
      wiped = held != null;
      if (wiped) {
        // ANOTHER ACCOUNT'S: a wipe in every respect — the same tables, the
        // count, a fresh generation, so a second context's next write is
        // refused as stale — and the new owner with it.
        _db.execute('DELETE FROM messages');
        _db.execute('DELETE FROM conversations');
        _db.execute('DELETE FROM users');
        _db.execute('DELETE FROM counted');
        _db.execute(
          'UPDATE journal_meta SET cursor = NULL, wipes = wipes + 1, generation = ?, owner = ? WHERE id = 1',
          [_newGeneration(), accountId],
        );
      } else {
        // NOBODY'S: a journal from before there was an owner. Kept, and
        // adopted (CANT-230 ruling 1).
        _db.execute('UPDATE journal_meta SET owner = ? WHERE id = 1', [accountId]);
      }
      _opts.midWrite?.call(_db, AppliedSource.wipe);
      _db.execute('COMMIT');
    } catch (_) {
      if (!_db.autocommit) _db.execute('ROLLBACK');
      rethrow;
    }
    // The mirror is whatever is stored now: the claim wrote from the file,
    // not from this context's copy of it.
    final loaded = _load(_db);
    state = loaded.state;
    _generation = loaded.generation;
    return wiped ? const Applied(source: AppliedSource.wipe, cursor: null, wiped: true) : null;
  }

  String? _storedOwner() => _db.select('SELECT owner FROM journal_meta WHERE id = 1').single['owner'] as String?;

  /// One transaction. Returns the wipe count and the owner it left stored.
  ({int wipes, String? owner}) _write(JournalDelta d, _Part part, String? generation) {
    _db.execute('BEGIN IMMEDIATE');
    try {
      // THE STORED GENERATION AND CURSOR, read inside this transaction, so
      // nothing another context commits can land between the check and the
      // write.
      final meta = _db.select('SELECT cursor, wipes, generation, owner FROM journal_meta WHERE id = 1').single;
      final storedCursor = meta['cursor'] as int?;
      var wipes = meta['wipes'] as int;
      if (!d.wiped && !_opts.ignoreGeneration && meta['generation'] != _generation) throw JournalStale(d.source);
      final written = d.cursor;
      final cursor = d.wiped || storedCursor == null || (written != null && written > storedCursor) ? written : storedCursor;

      if (part != _Part.cursor) {
        if (d.wiped) {
          // OBLIGATION 4's wipe: the journal's tables, and never `credential`.
          _db.execute('DELETE FROM messages');
          _db.execute('DELETE FROM conversations');
          _db.execute('DELETE FROM users');
          _db.execute('DELETE FROM counted');
          wipes++;
          _db.execute('UPDATE journal_meta SET wipes = ?, generation = ? WHERE id = 1', [wipes, generation]);
        }
        _upsert(
          'INSERT OR REPLACE INTO messages (id, conversation_id, seq, log_seq, record) VALUES (?, ?, ?, ?, ?)',
          [for (final m in d.messages) [m.id, m.conversationId, m.seq, m.logSeq, jsonEncode(m.toJson())]],
        );
        _upsert(
          'INSERT OR REPLACE INTO conversations (id, record) VALUES (?, ?)',
          [for (final c in d.conversations) [c.id, jsonEncode(c.toJson())]],
        );
        _upsert(
          'INSERT OR REPLACE INTO users (id, record) VALUES (?, ?)',
          [for (final u in d.users) [u.id, jsonEncode(u.toJson())]],
        );
        // By position, as the reference's `put` is: the log is one writer's
        // evidence, and a second writer's entries replace rather than collide.
        _upsert(
          'INSERT OR REPLACE INTO counted (n, id) VALUES (?, ?)',
          [for (final (i, id) in d.counted.indexed) [d.countedFrom + i, id]],
        );
      }
      if (part != _Part.records) {
        _db.execute('UPDATE journal_meta SET cursor = ? WHERE id = 1', [cursor]);
        _opts.midWrite?.call(_db, d.source);
      }
      _db.execute('COMMIT');
      return (wipes: wipes, owner: meta['owner'] as String?);
    } catch (_) {
      if (!_db.autocommit) _db.execute('ROLLBACK');
      rethrow;
    }
  }

  void _upsert(String sql, List<List<Object?>> rows) {
    if (rows.isEmpty) return;
    final statement = _db.prepare(sql);
    try {
      for (final row in rows) {
        statement.execute(row);
      }
    } finally {
      statement.close();
    }
  }
}

final class _Loaded {
  const _Loaded(this.state, this.generation);

  final JournalState state;
  final String? generation;
}

/// Reads every journal table in one read transaction, decoded through the
/// generated codecs: a record the wire schema would refuse is refused here
/// too, loudly, not rendered.
_Loaded _load(Database db) {
  db.execute('BEGIN');
  try {
    final meta = db.select('SELECT cursor, wipes, generation, owner FROM journal_meta WHERE id = 1').single;
    final s = JournalState.empty(meta['wipes'] as int)
      ..cursor = meta['cursor'] as int?
      ..owner = meta['owner'] as String?;
    for (final row in db.select('SELECT record FROM messages')) {
      final m = Message.fromJson(jsonDecode(row['record'] as String));
      s.messages[m.id] = m;
    }
    for (final row in db.select('SELECT record FROM conversations')) {
      final c = Conversation.fromJson(jsonDecode(row['record'] as String));
      s.conversations[c.id] = c;
    }
    for (final row in db.select('SELECT record FROM users')) {
      final u = User.fromJson(jsonDecode(row['record'] as String));
      s.users[u.id] = u;
    }
    for (final row in db.select('SELECT id FROM counted ORDER BY n')) {
      s.counted.add(row['id'] as String);
    }
    return _Loaded(s, meta['generation'] as String?);
  } finally {
    db.execute('COMMIT');
  }
}

final _random = Random.secure();

/// 128 random bits. Only ever compared for equality with the stored one.
String _newGeneration() => [for (var i = 0; i < 16; i++) _random.nextInt(256).toRadixString(16).padLeft(2, '0')].join();
