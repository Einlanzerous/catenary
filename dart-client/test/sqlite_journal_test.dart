/// `SqliteJournal` over a real file — the twin of
/// web/src/transport/test/idb-journal.test.ts: the binding and the migrator,
/// one transaction per write, a relaunch that resumes, the wipe, and two
/// contexts over one file. Each with the control that must make it fail.
library;

import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:sqlite3/sqlite3.dart';
import 'package:test/test.dart';

import 'support.dart';

final p1 = bootstrapPage(3, [message(1), message(2), message(3)]);

/// The second page: messages 4–6 and a room it introduces.
final c2 = uuid(102);
final p2 = page(
  6,
  messages: [message(4), message(5), message(6, conversationId: c2, seq: 1)],
  conversations: [conversation(c2)],
  users: [user(uuid(7))],
);

int count(Database db, String table) => db.select('SELECT count(*) AS n FROM $table').single['n'] as int;

/// What is in the file, read through a connection of its own.
({int? cursor, int wipes, String? generation, int messages, int conversations, int users, int counted, int credential}) stored(
  String path,
) {
  final db = sqlite3.open(path);
  try {
    final meta = db.select('SELECT cursor, wipes, generation FROM journal_meta').single;
    return (
      cursor: meta['cursor'] as int?,
      wipes: meta['wipes'] as int,
      generation: meta['generation'] as String?,
      messages: count(db, 'messages'),
      conversations: count(db, 'conversations'),
      users: count(db, 'users'),
      counted: count(db, 'counted'),
      credential: count(db, 'credential'),
    );
  } finally {
    db.close();
  }
}

const credentialRecord = '{"deviceId":"00000000-0000-4000-8000-000000000003","refreshToken":"held"}';

/// A credential row, written as the credential layer will write it: by a
/// connection that is not the journal's.
void enroll(String path) {
  final db = openCatenaryDb(path);
  db.execute('INSERT INTO credential (device_id, record) VALUES (?, ?)', [uuid(3), credentialRecord]);
  db.close();
}

String credentialRow(String path) {
  final db = sqlite3.open(path);
  try {
    return db.select('SELECT record FROM credential WHERE device_id = ?', [uuid(3)]).single['record'] as String;
  } finally {
    db.close();
  }
}

final class Killed implements Exception {}

/// Two pages, the process killed mid-way through the second when `kill` is
/// set; then the journal is closed and a new one opened over the same file, as
/// the next launch would.
Future<({SqliteJournal reopened, Object? error, int? liveCursor, int p2Held, bool wholeOrNeither})> killedMidPage({
  required bool kill,
  bool splitCursor = false,
}) async {
  final path = catenaryDbPath(tempDir());
  var pages = 0;
  final journal = SqliteJournal.open(
    path,
    SqliteJournalOptions(
      splitCursor: splitCursor,
      midWrite: (_, source) {
        if (source == AppliedSource.page && ++pages == 2 && kill) throw Killed();
      },
    ),
  );
  await journal.applyPage(p1, Faults.none);
  Object? error;
  try {
    await journal.applyPage(p2, Faults.none);
  } catch (e) {
    error = e;
  }
  final liveCursor = journal.cursor;
  journal.close();

  final reopened = SqliteJournal.open(path);
  addTearDown(reopened.close);
  final snap = reopened.snapshot();
  final ids = {for (final m in snap.messages) m.id};
  final p2Held = p2.messages.where((m) => ids.contains(m.id)).length;
  final p2Convs = p2.conversations.where((c) => reopened.holdsConversation(c.id)).length;
  final p2Users = p2.users.where((u) => reopened.holdsUser(u.id)).length;
  final wholeOrNeither = (snap.cursor == p2.logSeq && p2Held == 3 && p2Convs == 1 && p2Users == 1) ||
      (snap.cursor == p1.logSeq && p2Held == 0 && p2Convs == 0 && p2Users == 0);
  return (reopened: reopened, error: error, liveCursor: liveCursor, p2Held: p2Held, wholeOrNeither: wholeOrNeither);
}

/// Two contexts over one file, each with its own mirror, through a restore
/// that shrinks the log from head 10 to head 6. A wipes and bootstraps to 6;
/// B, whose `ready` arrives later, wipes again — clearing A's pages — and
/// lands only its first page (to 5) before it is closed. Then a live frame
/// reaches A. The next launch must resume from a cursor whose every message is
/// held.
Future<({Object? live, int? aCursor, int cursor, List<int> uncovered})> twoContextsAcrossAWipe({bool ignoreGeneration = false}) async {
  final path = catenaryDbPath(tempDir());
  final opts = SqliteJournalOptions(ignoreGeneration: ignoreGeneration);
  final seed = SqliteJournal.open(path);
  await seed.applyPage(bootstrapPage(10, [for (var n = 1; n <= 10; n++) message(n)]), Faults.none);
  seed.close();

  final a = SqliteJournal.open(path, opts);
  final b = SqliteJournal.open(path, opts);
  final after = [for (var n = 1; n <= 7; n++) message(n, id: uuid(5000 + n))];
  await a.wipe();
  await a.applyPage(bootstrapPage(6, after.sublist(0, 6)), Faults.none);
  await b.wipe();
  await b.applyPage(bootstrapPage(5, after.sublist(0, 5)), Faults.none);
  b.close();
  Object? live;
  try {
    await a.applyLive(LiveWrite(messages: [after[6]]), Faults.none);
  } catch (e) {
    live = e;
  }
  final aCursor = a.cursor;
  a.close();

  final next = SqliteJournal.open(path);
  final held = {for (final m in next.snapshot().messages) m.id};
  final cursor = next.cursor ?? 0;
  final uncovered = [
    for (final m in after)
      if (m.logSeq <= cursor && !held.contains(m.id)) m.logSeq,
  ];
  next.close();
  return (live: live, aCursor: aCursor, cursor: cursor, uncovered: uncovered);
}

void main() {
  group('the binding and the migrator', () {
    test('opens a file-backed database, with synchronous = FULL on its writing connection', () async {
      final path = catenaryDbPath(tempDir());
      expect(path, endsWith('/catenary.db'));
      final j = SqliteJournal.open(path);
      addTearDown(j.close);
      expect(File(path).existsSync(), isTrue);
      expect(j.database.select('PRAGMA synchronous').single.values.single, 2, reason: '2 is FULL');
      await j.applyPage(p1, Faults.none);
      expect(File(path).lengthSync(), greaterThan(0));
      expect(stored(path).messages, 3, reason: 'a second connection reads what the first committed to the file');
    });

    // CANT-202 ruling 1: both files use SQLite's rollback journal. Asked of the connection each open function returned, not of a second
    // connection to the file: synchronous is per connection, and a second connection would report its own.
    test('both files run the rollback journal and synchronous = FULL, on the connection each open function returned', () {
      final dir = tempDir();
      final journal = openCatenaryDb(catenaryDbPath(dir));
      addTearDown(journal.close);
      final outbox = SqliteOutboxStore.open(outboxDbPath(dir));
      addTearDown(outbox.close);
      for (final (name, db) in [('catenary.db', journal), ('catenary-outbox.db', outbox.database)]) {
        expect(db.select('PRAGMA journal_mode').single.values.single, 'delete', reason: '$name: the rollback journal');
        expect(db.select('PRAGMA synchronous').single.values.single, 2, reason: '$name: 2 is FULL');
      }
    });

    test('brings a fresh file to the current user_version, with the journal and credential tables', () {
      final path = catenaryDbPath(tempDir());
      final j = SqliteJournal.open(path);
      addTearDown(j.close);
      expect(j.database.userVersion, catenaryMigrations.length);
      final tables = {
        for (final row in j.database.select("SELECT name FROM sqlite_schema WHERE type = 'table'")) row['name'] as String,
      };
      expect(tables, containsAll(['journal_meta', 'messages', 'conversations', 'users', 'counted', 'credential']));
      expect(tables, isNot(contains('outbox')), reason: 'the outbox is in a file of its own');
      expect(stored(path), (cursor: null, wipes: 0, generation: null, messages: 0, conversations: 0, users: 0, counted: 0, credential: 0));
    });

    test('a later build runs only the steps the file has not had, and keeps what it holds', () async {
      final path = catenaryDbPath(tempDir());
      final first = SqliteJournal.open(path);
      await first.applyPage(p1, Faults.none);
      first.close();

      const longer = [...catenaryMigrations, 'CREATE TABLE added_later (id INTEGER PRIMARY KEY) STRICT;'];
      final second = SqliteJournal.open(path, const SqliteJournalOptions(migrations: longer));
      addTearDown(second.close);
      expect(second.database.userVersion, longer.length);
      expect(count(second.database, 'added_later'), 0);
      expect(second.cursor, 3);
      expect(second.messageCount, 3);
    });

    test('a step that fails leaves the file at the version before it', () {
      final path = catenaryDbPath(tempDir());
      SqliteJournal.open(path).close();
      const broken = [...catenaryMigrations, 'CREATE TABLE half (id INTEGER PRIMARY KEY) STRICT; CREATE TABLE half (id INTEGER);'];
      expect(() => SqliteJournal.open(path, const SqliteJournalOptions(migrations: broken)), throwsA(isA<SqliteException>()));
      final db = sqlite3.open(path);
      addTearDown(db.close);
      expect(db.userVersion, catenaryMigrations.length);
      expect(db.select("SELECT name FROM sqlite_schema WHERE name = 'half'"), isEmpty);
    });

    test('a file from a newer build is refused, not read with older statements', () {
      final path = catenaryDbPath(tempDir());
      const longer = [...catenaryMigrations, 'CREATE TABLE added_later (id INTEGER PRIMARY KEY) STRICT;'];
      SqliteJournal.open(path, const SqliteJournalOptions(migrations: longer)).close();
      expect(() => SqliteJournal.open(path), throwsA(isA<DatabaseTooNew>()));
    });
  });

  group('one transaction per write', () {
    test('a page killed mid-write leaves the whole page and its cursor, or neither', () async {
      final killed = await killedMidPage(kill: true);
      expect(killed.error, isA<Killed>(), reason: 'the write failed with the error that ended it');
      expect(killed.liveCursor, 3, reason: 'and the live journal never showed the page');
      expect(killed.reopened.cursor, 3, reason: 'reopened: page 1\'s cursor');
      expect(killed.reopened.messageCount, 3, reason: 'page 1 whole');
      expect(killed.p2Held, 0, reason: 'and none of page 2');
      expect(killed.wholeOrNeither, isTrue);
      expect(killed.reopened.counted, [for (final m in p1.messages) m.id], reason: 'the evidence log is page 1\'s, once each');

      final whole = await killedMidPage(kill: false);
      expect(whole.reopened.cursor, 6);
      expect(whole.p2Held, 3);
      expect(whole.wholeOrNeither, isTrue);
    });

    test('negative control splitCursor: the cursor in its own transaction keeps a page without it', () async {
      final killed = await killedMidPage(kill: true, splitCursor: true);
      expect(killed.reopened.cursor, 3, reason: 'the cursor did not land');
      expect(killed.p2Held, 3, reason: 'but page 2\'s messages did');
      expect(killed.wholeOrNeither, isFalse, reason: 'the property fails');
    });

    test('a relaunch reads back everything the last commit left, and counts nothing twice', () async {
      final path = catenaryDbPath(tempDir());
      final first = SqliteJournal.open(path);
      await first.applyPage(p1, Faults.none);
      await first.applyPage(p2, Faults.none);
      await first.applyLive(LiveWrite(messages: [message(7)]), Faults.none);
      final before = wireShape(first.snapshot());
      first.close();

      final second = SqliteJournal.open(path);
      expect(wireShape(second.snapshot()), before);
      expect(second.cursor, 6);
      expect(second.counted, [for (var n = 1; n <= 7; n++) message(n).id]);
      // The page that re-carries the live message, after the relaunch.
      await second.applyPage(page(7, messages: [message(7)]), Faults.none);
      second.close();

      final third = SqliteJournal.open(path);
      addTearDown(third.close);
      expect(third.cursor, 7);
      expect(third.messageCount, 7);
      expect(third.counted.toSet(), hasLength(third.counted.length), reason: 'nothing counted twice across the relaunch');
      expect(third.counted, hasLength(7));
    });

    test('a stored record the wire schema refuses is refused at open, not rendered', () {
      final path = catenaryDbPath(tempDir());
      final db = openCatenaryDb(path);
      db.execute(
        'INSERT INTO messages (id, conversation_id, seq, log_seq, record) VALUES (?, ?, 1, 1, ?)',
        [uuid(1), conv, '{"id":"${uuid(1)}","text":"no seq, no log_seq"}'],
      );
      db.close();
      expect(() => SqliteJournal.open(path), throwsA(isA<WireFormatException>()));
    });
  });

  group('obligation 4', () {
    test('a wipe clears the stored journal, counts itself, stamps a fresh generation, and never touches the credential', () async {
      final path = catenaryDbPath(tempDir());
      enroll(path);
      final journal = SqliteJournal.open(path);
      await journal.applyPage(p1, Faults.none);
      expect(stored(path), (cursor: 3, wipes: 0, generation: null, messages: 3, conversations: 1, users: 2, counted: 3, credential: 1));

      await journal.wipe();
      final once = stored(path);
      expect(once.generation, isNotNull);
      expect(once, (cursor: null, wipes: 1, generation: once.generation, messages: 0, conversations: 0, users: 0, counted: 0, credential: 1));
      expect(credentialRow(path), credentialRecord, reason: 'the credential row is byte for byte what it was');

      await journal.wipe();
      final twice = stored(path);
      expect(twice.wipes, 2);
      expect(twice.generation, isNot(once.generation), reason: 'each wipe stamps its own');
      journal.close();

      final reopened = SqliteJournal.open(path);
      addTearDown(reopened.close);
      expect(wireShape(reopened.snapshot()), {'cursor': null, 'messages': [], 'conversations': [], 'users': []});
      expect(reopened.counted, isEmpty);
      expect(reopened.wipes, 2, reason: 'the wipes are counted, durably');
    });

    test('a wipe is one transaction: killed mid-wipe, nothing it does has landed', () async {
      final path = catenaryDbPath(tempDir());
      final journal = SqliteJournal.open(
        path,
        SqliteJournalOptions(
          midWrite: (_, source) {
            if (source == AppliedSource.wipe) throw Killed();
          },
        ),
      );
      addTearDown(journal.close);
      await journal.applyPage(p1, Faults.none);
      await expectLater(journal.wipe(), throwsA(isA<Killed>()));
      expect(stored(path), (cursor: 3, wipes: 0, generation: null, messages: 3, conversations: 1, users: 2, counted: 3, credential: 0));
      expect(journal.cursor, 3, reason: 'and the mirror is what it was');
      expect(journal.wipes, 0);
    });

    test('two contexts that each wipe once have wiped twice', () async {
      final path = catenaryDbPath(tempDir());
      final a = SqliteJournal.open(path);
      final b = SqliteJournal.open(path);
      addTearDown(a.close);
      addTearDown(b.close);
      await a.wipe();
      await b.wipe();
      expect(stored(path).wipes, 2);
      expect(b.wipes, 2);
    });
  });

  group('whose journal it is (CANT-230)', () {
    final a = uuid(1);
    final b = uuid(2);

    String? owner(String path) {
      final db = sqlite3.open(path);
      try {
        return db.select('SELECT owner FROM journal_meta').single['owner'] as String?;
      } finally {
        db.close();
      }
    }

    test('the journal records its owner: written, read back by the next launch, and a repeat claim writes nothing', () async {
      final path = catenaryDbPath(tempDir());
      final journal = SqliteJournal.open(path);
      expect(journal.owner, isNull);
      expect(await journal.claim(a), isNull);
      expect(owner(path), a);
      await journal.applyPage(p1, Faults.none);
      journal.close();

      var writes = 0;
      final reopened = SqliteJournal.open(path, SqliteJournalOptions(midWrite: (_, _) => writes++));
      addTearDown(reopened.close);
      expect(reopened.owner, a);
      final before = stored(path);
      final snapshot = wireShape(reopened.snapshot());
      expect(await reopened.claim(a), isNull);
      expect(writes, 0, reason: 'its own account claiming it again writes nothing');
      expect(stored(path), before, reason: 'the cursor, the wipe count and the generation are what they were');
      expect(wireShape(reopened.snapshot()), snapshot);
      expect(reopened.cursor, 3);
    });

    test('a journal owned by another account is wiped by the claim, and the credential beside it is not touched', () async {
      final path = catenaryDbPath(tempDir());
      enroll(path);
      final journal = SqliteJournal.open(path);
      addTearDown(journal.close);
      await journal.claim(a);
      await journal.applyPage(p1, Faults.none);
      await journal.applyPage(p2, Faults.none);
      final before = stored(path);
      expect((before.cursor, before.messages, before.conversations, before.users), (6, 6, 2, 3));

      final applied = await journal.claim(b);
      expect(applied!.wiped, isTrue);
      final after = stored(path);
      expect(after.generation, isNotNull);
      expect(after.generation, isNot(before.generation));
      expect(after, (cursor: null, wipes: before.wipes + 1, generation: after.generation, messages: 0, conversations: 0, users: 0, counted: 0, credential: 1));
      expect(owner(path), b);
      expect(credentialRow(path), credentialRecord, reason: 'the credential row is byte for byte what it was');
      expect(wireShape(journal.snapshot()), {'cursor': null, 'messages': [], 'conversations': [], 'users': []});
      expect(journal.owner, b);
      expect(journal.wipes, before.wipes + 1);
    });

    test('the claim is one transaction: killed before its commit, nothing it does has landed', () async {
      final path = catenaryDbPath(tempDir());
      final first = SqliteJournal.open(path);
      await first.claim(a);
      await first.applyPage(p1, Faults.none);
      first.close();
      final before = stored(path);

      final journal = SqliteJournal.open(
        path,
        SqliteJournalOptions(
          midWrite: (_, source) {
            if (source == AppliedSource.wipe) throw Killed();
          },
        ),
      );
      await expectLater(journal.claim(b), throwsA(isA<Killed>()));
      expect(journal.owner, a, reason: 'and the mirror is what it was');
      expect(journal.cursor, 3);
      journal.close();

      expect(stored(path), before);
      expect(owner(path), a);
      final reopened = SqliteJournal.open(path);
      addTearDown(reopened.close);
      expect(reopened.owner, a);
      expect(reopened.cursor, 3);
      expect(reopened.messageCount, 3);
    });

    test('a wipe keeps the owner, in the file and in the mirror', () async {
      final path = catenaryDbPath(tempDir());
      final journal = SqliteJournal.open(path);
      addTearDown(journal.close);
      await journal.claim(a);
      await journal.applyPage(p1, Faults.none);
      await journal.wipe();
      expect(owner(path), a);
      expect(journal.owner, a);
    });

    test('a claim\'s wipe is seen by a second context: its next page write is refused as stale, and it reads back what is stored', () async {
      final path = catenaryDbPath(tempDir());
      final first = SqliteJournal.open(path);
      final second = SqliteJournal.open(path);
      addTearDown(first.close);
      addTearDown(second.close);
      await first.claim(a);
      await first.applyPage(p1, Faults.none);
      await second.applyPage(p1, Faults.none);
      expect(second.cursor, 3);

      await first.claim(b);
      await expectLater(second.applyPage(p2, Faults.none), throwsA(isA<JournalStale>()));
      expect(second.cursor, isNull, reason: 'it holds what the claim left');
      expect(second.messageCount, 0);
      expect(second.owner, b);
      expect(stored(path).messages, 0, reason: 'the refused page landed nothing');
    });

    test('an existing journal file is brought forward: version 2, its records and cursor intact, and no owner', () async {
      final path = catenaryDbPath(tempDir());
      // A file as the build before this one left it: the first step only.
      final old = openCatenaryDb(path, migrations: [catenaryMigrations.first]);
      expect(old.userVersion, 1);
      expect(old.select("SELECT name FROM pragma_table_info('journal_meta')").map((r) => r['name']), isNot(contains('owner')));
      final m = message(1);
      old.execute(
        'INSERT INTO messages (id, conversation_id, seq, log_seq, record) VALUES (?, ?, ?, ?, ?)',
        [m.id, m.conversationId, m.seq, m.logSeq, jsonEncode(m.toJson())],
      );
      old.execute('UPDATE journal_meta SET cursor = 1');
      old.close();

      final journal = SqliteJournal.open(path);
      addTearDown(journal.close);
      expect(journal.database.userVersion, 2);
      expect(catenaryMigrations, hasLength(2));
      expect(journal.owner, isNull);
      expect(journal.cursor, 1);
      expect(journal.snapshot().messages.single.id, m.id);

      // [ruling 1 → option 0] and its first claim adopts it.
      expect(await journal.claim(a), isNull);
      expect(owner(path), a);
      expect(journal.cursor, 1);
      expect(journal.messageCount, 1);
    });
  });

  group('two contexts', () {
    /// A and B hold the same journal; A wipes and bootstraps again from a
    /// shorter log. `write` is B's next write.
    Future<void> refusedAfterAWipe(Future<Applied> Function(SqliteJournal b) write, AppliedSource source) async {
      final path = catenaryDbPath(tempDir());
      final a = SqliteJournal.open(path);
      final b = SqliteJournal.open(path);
      addTearDown(a.close);
      addTearDown(b.close);
      await a.applyPage(bootstrapPage(10, [for (var n = 1; n <= 10; n++) message(n)]), Faults.none);
      await b.applyPage(bootstrapPage(10, [for (var n = 1; n <= 10; n++) message(n)]), Faults.none);
      await a.wipe();
      await a.applyPage(bootstrapPage(2, [message(1, id: uuid(5001)), message(2, id: uuid(5002))]), Faults.none);
      expect(b.cursor, 10, reason: 'B\'s mirror still covers what A wiped');
      final before = stored(path);

      await expectLater(write(b), throwsA(isA<JournalStale>().having((e) => e.source, 'source', source)));
      expect(stored(path), before, reason: 'the refused write left the stored cursor and records untouched');
      expect(wireShape(b.snapshot()), wireShape(a.snapshot()), reason: 'and B has read back what is stored');
      expect(b.cursor, 2);
      expect(b.counted, a.counted);
      expect(b.wipes, 1);

      // Reloaded, B is a writer of the journal as it now is.
      await b.applyPage(page(3, messages: [message(3, id: uuid(5003))]), Faults.none);
      expect(stored(path).cursor, 3);
    }

    test('after one wipes, the other\'s next live write is refused, and it reads back what is stored', () async {
      await refusedAfterAWipe((b) => b.applyLive(LiveWrite(messages: [message(11)]), Faults.none), AppliedSource.live);
    });

    test('after one wipes, the other\'s next page write is refused, and it reads back what is stored', () async {
      await refusedAfterAWipe((b) => b.applyPage(page(12, messages: [message(11), message(12)]), Faults.none), AppliedSource.page);
    });

    test('a write over a journal another context wiped is refused, and no cursor is left above a message the file lacks', () async {
      final r = await twoContextsAcrossAWipe();
      expect(r.live, isA<JournalStale>(), reason: 'A\'s live write is refused');
      expect(r.aCursor, 5, reason: 'and A\'s mirror is now what is stored');
      expect(r.cursor, 5);
      expect(r.uncovered, isEmpty, reason: 'every message at or below the stored cursor is held');
    });

    test('negative control ignoreGeneration leaves a cursor above messages the file no longer holds', () async {
      final r = await twoContextsAcrossAWipe(ignoreGeneration: true);
      expect(r.live, isNull, reason: 'the write went through');
      expect(r.cursor, 6);
      expect(r.uncovered, [6], reason: 'message 6 is below the cursor and will never be fetched again');
    });

    test('a context behind the stored cursor never moves it backward', () async {
      final path = catenaryDbPath(tempDir());
      final a = SqliteJournal.open(path);
      final b = SqliteJournal.open(path);
      await a.applyPage(bootstrapPage(6, [message(1), message(6)]), Faults.none);
      expect(stored(path).cursor, 6);
      await b.applyPage(bootstrapPage(3, [message(1)]), Faults.none);
      expect(stored(path).cursor, 6, reason: 'B\'s page, bounded at 3, did not lower it');
      await b.applyLive(LiveWrite(messages: [message(4)]), Faults.none);
      expect(stored(path).cursor, 6, reason: 'nor did its live write');
      await a.applyPage(page(8), Faults.none);
      expect(stored(path).cursor, 8);
      a.close();
      b.close();
      final next = SqliteJournal.open(path);
      addTearDown(next.close);
      expect(next.cursor, 8);
      expect(next.messageCount, 3);
    });
  });
}
