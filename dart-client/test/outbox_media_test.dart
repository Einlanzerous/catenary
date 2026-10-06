/// An unsent attachment's media, and the offline rule (CANT-201, sub-task
/// CANT-211).
///
/// Three rulings are held here. Ruling 0: the media is a BLOB in
/// `catenary-outbox.db`, written with its entry and deleted with it. Ruling 1:
/// with no ready session a recording is held and a picked file is refused, and
/// a held recording's upload waits for a ready session. And the rule the wait
/// needs to be worth anything: an entry reloaded after a relaunch is offered to
/// the uploader on the first `ready`, once, by the drain lock's holder.
///
/// NONE OF THIS IS IN THE FAULT TABLE. outbox_criteria_test.dart's table is the
/// reference's, row for row, and the reference's attachment criteria (13 and
/// 14) are CANT-162's and not written. So these are plain tests, and where one
/// claims "in the same transaction" it plants a failure in the second write and
/// checks the first did not survive it.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:sqlite3/sqlite3.dart' show SqliteException;
import 'package:test/test.dart';

import 'harness.dart';
import 'outbox_harness.dart';

/// Not text, not short, and not the same at every index: a store that kept a
/// prefix, or a string, would not give these back.
Uint8List recording([int seed = 7]) => Uint8List.fromList([for (var i = 0; i < 4096; i++) (i * 31 + seed) & 0xff]);

OutboxDraft voiceDraft(Uint8List bytes, {String? text}) =>
    OutboxDraft(conversationId: conv, text: text, attachments: [OutboundAttachmentDraft(kind: 'voice', source: MediaBytes(bytes), durationMs: 1700)]);

OutboxDraft imageDraft(Uint8List bytes) =>
    OutboxDraft(conversationId: conv, attachments: [OutboundAttachmentDraft(kind: 'image', source: MediaBytes(bytes), filename: 'a.png')]);

/// The tests' uploader: it records every call, and answers each with what
/// `answer` returns — a handle, a future that never completes, or a throw.
final class ScriptedUploader implements Uploader {
  ScriptedUploader(this.answer, {this.name = '', List<String>? calls}) : calls = calls ?? [];

  final Future<Uuid> Function(OutboxEntry entry, OutboundAttachmentDraft attachment) answer;

  /// Which context this uploader belongs to, where two share one `calls`.
  final String name;

  /// One line per call: the context's name and the entry's `clientId`.
  final List<String> calls;

  @override
  Future<Uuid> upload(OutboxEntry entry, OutboundAttachmentDraft attachment) {
    calls.add('$name${entry.clientId}');
    return answer(entry, attachment);
  }
}

Future<Uuid> never(OutboxEntry _, OutboundAttachmentDraft __) => Completer<Uuid>().future;

Future<Uuid> wouldThrow(OutboxEntry _, OutboundAttachmentDraft __) => Future.error(const UploadRefused('no connection to upload on'));

int mediaRows(SqliteOutboxStore store, [Uuid? clientId]) => clientId == null
    ? store.database.select('SELECT count(*) AS n FROM outbox_media').single['n'] as int
    : store.database.select('SELECT count(*) AS n FROM outbox_media WHERE client_id = ?', [clientId]).single['n'] as int;

SqliteOutboxStore openStore(String dir) {
  final store = SqliteOutboxStore.open(outboxDbPath(dir));
  addTearDown(store.close);
  return store;
}

/// Both implementations of the store, each fresh.
final stores = <String, OutboxStore Function()>{
  'SqliteOutboxStore': () => openStore(tempDir()),
  'MemoryOutboxStore': MemoryOutboxStore.new,
};

void main() {
  group('media is held with its entry', () {
    test('an attachment entry composed with media, in a store closed and reopened on the same path, is listed with its attachment pending and yields the same bytes', () async {
      final dir = tempDir();
      final bytes = recording();
      final uploader = ScriptedUploader(never);
      final written = Completer<Uuid>();
      final store = SqliteOutboxStore.open(outboxDbPath(dir));
      final transport = ScriptedTransport();
      final ctx = await context(
        store: store,
        transport: transport,
        uploader: uploader,
        onView: (items) {
          if (items.isNotEmpty && !written.isCompleted) written.complete(items.single.entry.clientId);
        },
      );
      transport.open();
      await flush();

      // Not awaited: compose awaits an upload that never answers.
      unawaited(ctx.outbox.compose(voiceDraft(bytes, text: 'a voice note')));
      final clientId = await within('the write, through onChange', written.future);
      await flush();
      expect(uploader.calls, [clientId], reason: 'the upload is in flight and will never answer');
      ctx.close();
      store.close();

      final reopened = openStore(dir);
      final entry = (await reopened.list()).single;
      expect(entry.clientId, clientId);
      expect(entry.status, OutboxStatus.pending);
      expect([for (final a in entry.attachments) (a.kind, a.upload, a.uploadId, a.durationMs)], [('voice', UploadState.pending, null, 1700)]);
      expect(await reopened.media(clientId, 0), bytes, reason: 'the same bytes that were composed');
      expect(await reopened.media(clientId, 1), isNull, reason: 'and nothing at an index it has no attachment for');
    });

    for (final MapEntry(key: name, value: open) in stores.entries) {
      test('when an entry settles or is discarded, its media is gone — $name', () async {
        final store = open();
        final transport = ScriptedTransport();
        var refuse = false;
        final uploader = ScriptedUploader((e, a) async => refuse ? throw const UploadRefused('would not go') : uuid(5000));
        final ctx = await context(store: store, transport: transport, uploader: uploader);
        transport.open();
        await flush();

        // Settled: uploaded, sent, and met by its record.
        final settled = await ctx.outbox.compose(voiceDraft(recording(1)));
        await flush();
        expect(transport.framesFor(settled.clientId), hasLength(1));
        expect(await store.media(settled.clientId, 0), recording(1), reason: 'held while the entry is, uploaded or not: a stale handle is re-uploaded from it');
        transport.deliver(settled.clientId);
        await flush();
        expect(await ctx.stored(settled.clientId), isNull);
        expect(await store.media(settled.clientId, 0), isNull, reason: 'the store holds no media for a settled entry');

        // Discarded: failed, then DELETE.
        refuse = true;
        final discarded = await ctx.outbox.compose(voiceDraft(recording(2)));
        await flush();
        expect(ctx.item(discarded.clientId)!.state, OutboxState.failed);
        expect(await store.media(discarded.clientId, 0), recording(2), reason: 'a failed entry keeps its media for its RETRY');
        expect(await ctx.outbox.discard(discarded.clientId), isTrue);
        expect(await ctx.stored(discarded.clientId), isNull);
        expect(await store.media(discarded.clientId, 0), isNull, reason: 'the store holds no media for a discarded entry');
        if (store is SqliteOutboxStore) expect(mediaRows(store), 0, reason: 'and the table is empty');
      });

      test('what the store hands back is a copy of what was composed, and each attachment has its own — $name', () async {
        final store = open();
        final ctx = await context(store: store, uploader: ScriptedUploader(never));
        final first = recording(1), second = recording(2);
        final entry = await ctx.outbox.compose(OutboxDraft(conversationId: conv, attachments: [
          OutboundAttachmentDraft(kind: 'voice', source: MediaBytes(first)),
          OutboundAttachmentDraft(kind: 'voice', source: MediaBytes(second)),
        ]));
        final want = recording(1);
        first[0] ^= 0xff;
        expect(await store.media(entry.clientId, 0), want, reason: 'the source changing afterwards changes nothing stored');
        expect(await store.media(entry.clientId, 1), second);
        expect([for (final a in entry.attachments) a.source], [null, null], reason: 'an entry never carries a source');
        expect([for (final a in (await store.list()).single.attachments) a.source], [null, null]);
      });
    }

    test('a source file is copied at compose: the entry\'s media outlives the file', () async {
      final dir = tempDir();
      final bytes = recording();
      final file = File('$dir/recorder-temp.m4a')..writeAsBytesSync(bytes);
      final store = openStore(dir);
      final ctx = await context(store: store);
      final entry = await ctx.outbox.compose(OutboxDraft(conversationId: conv, attachments: [OutboundAttachmentDraft(kind: 'voice', source: MediaFile(file.path))]));
      file.deleteSync();
      expect(await store.media(entry.clientId, 0), bytes);
    });

    test('an attachment that names no source is refused, and nothing is written', () async {
      final store = openStore(tempDir());
      final transport = ScriptedTransport();
      final ctx = await context(store: store, transport: transport);
      transport.open();
      await flush();
      await expectLater(
        ctx.outbox.compose(OutboxDraft(conversationId: conv, attachments: const [OutboundAttachmentDraft(kind: 'voice')])),
        throwsA(isA<ComposeRefused>().having((e) => e.message, 'message', ComposeRefused.noMedia)),
      );
      expect(await store.list(), isEmpty);
      expect(mediaRows(store), 0);
    });
  });

  group('the outbox file holds the media [ruling 0: a BLOB in catenary-outbox.db]', () {
    test('the file is at the version after the appended step, and a file at the previous version opens and migrates with its entries intact', () async {
      final dir = tempDir();
      expect(outboxMigrations.length, greaterThanOrEqualTo(2));
      expect(outboxMigrations[0], isNot(contains('outbox_media')), reason: 'the first step is as it shipped');
      expect(outboxMigrations[1], contains('CREATE TABLE outbox_media'), reason: 'the media table is a step appended after it');

      // A file as the build before this one left it.
      final before = openMigrated(outboxDbPath(dir), outboxMigrations.sublist(0, 1));
      expect(before.userVersion, 1);
      final held = [
        OutboxEntry(clientId: uuid(1), accountId: me, conversationId: conv, order: 1, composedAt: at, text: 'queued before the upgrade'),
        OutboxEntry(clientId: uuid(2), accountId: me, conversationId: conv, order: 2, composedAt: at, text: 'failed before the upgrade', status: OutboxStatus.failed, lastError: const Bare1008(), attempts: 1),
      ];
      for (final e in held) {
        before.execute('INSERT INTO outbox (client_id, account_id, "order", record) VALUES (?, ?, ?, ?)', [e.clientId, e.accountId, e.order, jsonEncode(e.toJson())]);
      }
      before.close();

      final store = openStore(dir);
      expect(store.database.userVersion, outboxMigrations.length);
      final tables = {for (final r in store.database.select("SELECT name FROM sqlite_schema WHERE type = 'table'")) r['name']};
      expect(tables, {'outbox', 'outbox_media'});
      expect([for (final e in await store.list()) jsonEncode(e.toJson())], [for (final e in held) jsonEncode(e.toJson())], reason: 'every entry, as it was stored');
      expect(mediaRows(store), 0);
    });

    test('removing an entry through the store leaves no outbox_media row for its client_id, in the same transaction', () async {
      final store = openStore(tempDir());
      final ctx = await context(store: store);
      final kept = await ctx.outbox.compose(voiceDraft(recording(1)));
      final removed = await ctx.outbox.compose(voiceDraft(recording(2)));
      expect((mediaRows(store, kept.clientId), mediaRows(store, removed.clientId)), (1, 1));

      // The media delete is made to fail. Were it a second transaction, the
      // entry's delete would already have committed.
      store.database.execute("CREATE TEMP TRIGGER planted BEFORE DELETE ON outbox_media BEGIN SELECT RAISE(ABORT, 'planted'); END");
      await expectLater(store.delete(removed.clientId), throwsA(isA<SqliteException>()));
      expect([for (final e in await store.list()) e.clientId], [kept.clientId, removed.clientId], reason: 'the entry\'s delete was taken back with its media\'s');
      expect(await store.media(removed.clientId, 0), recording(2));
      store.database.execute('DROP TRIGGER planted');

      await store.delete(removed.clientId);
      expect([for (final e in await store.list()) e.clientId], [kept.clientId]);
      expect(mediaRows(store, removed.clientId), 0);
      expect(await store.media(kept.clientId, 0), recording(1), reason: 'and no other entry\'s media went with it');
    });

    test('an entry and its media are written in one transaction: a media write that fails leaves no entry', () async {
      final store = openStore(tempDir());
      final ctx = await context(store: store);
      store.database.execute("CREATE TEMP TRIGGER planted BEFORE INSERT ON outbox_media BEGIN SELECT RAISE(ABORT, 'planted'); END");
      await expectLater(ctx.outbox.compose(voiceDraft(recording())), throwsA(isA<SqliteException>()));
      expect(await store.list(), isEmpty, reason: 'no entry without its media');
      expect(ctx.outbox.view(), isEmpty, reason: 'and nothing rendered');
      store.database.execute('DROP TRIGGER planted');
      final entry = await ctx.outbox.compose(voiceDraft(recording()));
      expect(entry.order, 1, reason: 'the refused write drew no order');
    });

    test('listing does not depend on media: with an entry\'s outbox_media rows deleted, list still returns the entry with its attachment', () async {
      final store = openStore(tempDir());
      final ctx = await context(store: store);
      final entry = await ctx.outbox.compose(voiceDraft(recording(), text: 'still listed'));
      store.database.execute('DELETE FROM outbox_media WHERE client_id = ?', [entry.clientId]);
      final listed = (await store.list()).single;
      expect((listed.clientId, listed.text), (entry.clientId, 'still listed'));
      expect([for (final a in listed.attachments) (a.kind, a.upload)], [('voice', UploadState.pending)]);
      expect(await store.media(entry.clientId, 0), isNull);
      // And a fresh outbox loads it.
      final again = await context(store: store);
      expect(again.item(entry.clientId)?.state, OutboxState.queued);
    });
  });

  group('the offline rule [ruling 1: a recording is held, a picked file is refused]', () {
    for (final MapEntry(key: name, value: open) in stores.entries) {
      test('with the session not ready and not terminal, compose stores a voice draft with its media, and refuses an image draft and writes nothing — $name', () async {
        final store = open();
        final views = <int>[];
        final ctx = await context(store: store, uploader: ScriptedUploader(wouldThrow), onView: (items) => views.add(items.length));
        final bytes = recording();

        final voice = await within('compose', ctx.outbox.compose(voiceDraft(bytes)));
        expect((await ctx.stored(voice.clientId))!.status, OutboxStatus.pending);
        expect(await store.media(voice.clientId, 0), bytes);

        views.clear();
        await expectLater(
          ctx.outbox.compose(imageDraft(recording(9))),
          throwsA(isA<ComposeRefused>().having((e) => e.message, 'message', ComposeRefused.pickedFileOffline)),
        );
        // A recording and a picked file in one draft is a picked file.
        await expectLater(
          ctx.outbox.compose(OutboxDraft(conversationId: conv, attachments: [
            OutboundAttachmentDraft(kind: 'voice', source: MediaBytes(bytes)),
            OutboundAttachmentDraft(kind: 'image', source: MediaBytes(bytes)),
          ])),
          throwsA(isA<ComposeRefused>()),
        );
        expect([for (final e in await store.list()) e.clientId], [voice.clientId], reason: 'the refusals wrote no entry');
        if (store is SqliteOutboxStore) expect(mediaRows(store), 1, reason: 'and no media');
        expect(views, isEmpty, reason: 'and rendered nothing');
        // Text, in the same state, is queued as it always was.
        final text = await ctx.composeText('queued offline');
        expect(ctx.item(text.clientId)?.state, OutboxState.queued);
      });
    }

    test('a picked file is taken on a ready session, as before', () async {
      final transport = ScriptedTransport();
      final ctx = await context(transport: transport, uploader: ScriptedUploader((e, a) async => uuid(5001)));
      transport.open();
      await flush();
      final entry = await ctx.outbox.compose(imageDraft(recording()));
      await flush();
      expect(transport.framesFor(entry.clientId).single.toJson()['attachments'], [
        {'kind': 'image', 'upload_id': uuid(5001)},
      ]);
    });

    test('with the session not ready and an uploader that would throw, a voice draft makes zero upload calls and is pending, not failed; on ready the uploader is called once for it', () async {
      final transport = ScriptedTransport();
      final clock = FakeClock();
      final uploader = ScriptedUploader(wouldThrow);
      final ctx = await context(transport: transport, clock: clock, uploader: uploader, rereadMs: holderRereadMs);

      final entry = await within('compose', ctx.outbox.compose(voiceDraft(recording())));
      await clock.advance(holderRereadMs * 3);
      await ctx.outbox.refresh();
      expect(uploader.calls, isEmpty, reason: 'no upload is attempted with no ready session');
      expect((await ctx.stored(entry.clientId))!.status, OutboxStatus.pending, reason: 'pending, not failed');
      expect(ctx.item(entry.clientId)!.state, OutboxState.queued);

      transport.open();
      await flush();
      expect(uploader.calls, [entry.clientId], reason: 'called once for it when the session becomes ready');
      // This uploader throws, so the entry now fails, with its RETRY — and the
      // holder's re-reads do not offer a failed entry again.
      expect(ctx.item(entry.clientId)!.state, OutboxState.failed);
      await clock.advance(holderRereadMs * 3);
      expect(uploader.calls, [entry.clientId]);
    });

    test('a recording held offline uploads and sends once a session is ready, and one in flight is not offered twice', () async {
      final transport = ScriptedTransport();
      final clock = FakeClock();
      final answer = Completer<Uuid>();
      final uploader = ScriptedUploader((e, a) => answer.future);
      final ctx = await context(transport: transport, clock: clock, uploader: uploader, rereadMs: holderRereadMs);
      final entry = await within('compose', ctx.outbox.compose(voiceDraft(recording())));

      transport.open();
      await flush();
      // The upload is in flight across re-reads, a drop and a second ready.
      await clock.advance(holderRereadMs * 3);
      transport.close();
      await flush();
      transport.open();
      await clock.advance(holderRereadMs * 3);
      expect(uploader.calls, [entry.clientId], reason: 'offered once, however often the holder reads');
      expect(transport.frames, isEmpty, reason: 'and nothing is sent without its handle');

      answer.complete(uuid(5002));
      await flush();
      expect(transport.framesFor(entry.clientId).single.toJson()['attachments'], [
        {'kind': 'voice', 'upload_id': uuid(5002)},
      ]);
      expect(uploader.calls, [entry.clientId]);
    });

    test('RETRY with no ready session makes no upload call: the entry waits for one, pending', () async {
      final transport = ScriptedTransport();
      var refuse = true;
      final uploader = ScriptedUploader((e, a) async => refuse ? throw const UploadRefused('would not go') : uuid(5003));
      final ctx = await context(transport: transport, uploader: uploader);
      transport.open();
      await flush();
      final entry = await ctx.outbox.compose(voiceDraft(recording()));
      expect(ctx.item(entry.clientId)!.state, OutboxState.failed);
      transport.close();
      await flush();

      refuse = false;
      await within('retry', ctx.outbox.retry(entry.clientId));
      expect(uploader.calls, hasLength(1), reason: 'no call while not ready');
      expect((await ctx.stored(entry.clientId))!.status, OutboxStatus.pending);
      transport.open();
      await flush();
      expect(uploader.calls, hasLength(2));
      expect(transport.framesFor(entry.clientId), hasLength(1));
    });

    test('a terminal client takes no attachment, and still queues text', () async {
      final store = MemoryOutboxStore();
      final transport = ScriptedTransport();
      final ctx = await context(store: store, transport: transport, uploader: ScriptedUploader(never));
      transport.open();
      await flush();
      transport.close(terminal: true);
      await flush();
      await expectLater(
        ctx.outbox.compose(voiceDraft(recording())),
        throwsA(isA<ComposeRefused>().having((e) => e.message, 'message', ComposeRefused.terminal)),
      );
      expect(await store.list(), isEmpty);
      await ctx.composeText('kept for a re-enrollment');
      expect(await store.list(), hasLength(1));
      // A close that is not terminal is the offline rule again.
      transport.open();
      await flush();
      transport.close();
      await flush();
      await within('compose', ctx.outbox.compose(voiceDraft(recording())));
      expect(await store.list(), hasLength(2));
    });
  });

  group('the choices CANT-211 made alone, as ruled [CANT-220 rulings 0, 1 and 2]', () {
    test('ruling 2: a transport that is terminal and has emitted no close takes no attachment, and still stores text', () async {
      final store = MemoryOutboxStore();
      final transport = ScriptedTransport()..terminal = true;
      final uploader = ScriptedUploader(never);
      final ctx = await context(store: store, transport: transport, uploader: uploader);
      await expectLater(
        ctx.outbox.compose(voiceDraft(recording())),
        throwsA(isA<ComposeRefused>().having((e) => e.message, 'message', ComposeRefused.terminal)),
      );
      expect(await store.list(), isEmpty, reason: 'a refusal writes nothing');
      expect(uploader.calls, isEmpty);
      await ctx.composeText('kept for a re-enrollment');
      expect(await store.list(), hasLength(1));
    });

    test('ruling 1: an uploader that never answers is called once across a close and the next ready, and its entry stays pending', () async {
      final transport = ScriptedTransport();
      final clock = FakeClock();
      final uploader = ScriptedUploader(never);
      final ctx = await context(transport: transport, clock: clock, uploader: uploader, rereadMs: holderRereadMs);
      transport.open();
      await flush();
      // Not awaited: `compose` completes with the upload, and this one never does.
      unawaited(ctx.outbox.compose(voiceDraft(recording())));
      await flush();
      final entry = (await ctx.store.list()).single;
      expect(uploader.calls, [entry.clientId]);

      transport.close();
      await flush();
      transport.open();
      await clock.advance(holderRereadMs * 3);
      expect(uploader.calls, [entry.clientId], reason: 'the outbox keeps no deadline and does not offer it again');
      expect((await ctx.stored(entry.clientId))!.status, OutboxStatus.pending);
      expect(transport.frames, isEmpty);
    });

    test('ruling 0: RETRY in a ready context that does not hold the lock calls no uploader there; the holder uploads it', () async {
      final dir = tempDir();
      final clock = FakeClock();
      final server = ScriptedServer();
      final tA = ScriptedTransport(server);
      final tB = ScriptedTransport(server);
      SqliteDrainLock drainLock() {
        final locks = SqliteLocks(dir, timers: clock);
        addTearDown(locks.close);
        return SqliteDrainLock(locks, timers: clock);
      }

      final calls = <String>[];
      var refuse = true;
      final a = await context(
        store: openStore(dir),
        transport: tA,
        clock: clock,
        lock: drainLock(),
        rereadMs: holderRereadMs,
        uploader: ScriptedUploader((e, x) async => refuse ? throw const UploadRefused('would not go') : uuid(5007), name: 'A ', calls: calls),
      );
      final b = await context(store: openStore(dir), transport: tB, clock: clock, lock: drainLock(), rereadMs: holderRereadMs, uploader: ScriptedUploader((e, x) async => uuid(5008), name: 'B ', calls: calls));
      tA.open();
      await flush();
      tB.open();
      await clock.advance(200);
      expect((a.outbox.isHolder, b.outbox.isHolder), (true, false));

      final entry = await within('compose', b.outbox.compose(voiceDraft(recording())));
      await clock.advance(holderRereadMs * 2);
      expect(calls, ['A ${entry.clientId}'], reason: 'the holder offered it, and its uploader refused');
      await b.outbox.refresh();
      expect(b.item(entry.clientId)!.state, OutboxState.failed);

      refuse = false;
      await within('retry', b.outbox.retry(entry.clientId));
      expect(calls, ['A ${entry.clientId}'], reason: 'ready is not enough: the context without the lock does not upload on RETRY');
      expect((await b.stored(entry.clientId))!.status, OutboxStatus.pending);

      await clock.advance(holderRereadMs * 2);
      expect(calls, ['A ${entry.clientId}', 'A ${entry.clientId}']);
      expect(tA.framesFor(entry.clientId), hasLength(1));
      expect(tB.frames, isEmpty);
    });
  });

  group('an offline recording survives a relaunch', () {
    // Each context has its own connection to the store and its own to the
    // lock, as in criterion 15; the directory is what they share.
    test('an entry reloaded from the store with an attachment pending is offered to the uploader exactly once on the first ready, and by the lock\'s holder only', () async {
      final dir = tempDir();
      final bytes = recording();

      // Recorded with no session, then the app goes away.
      final firstStore = SqliteOutboxStore.open(outboxDbPath(dir));
      final first = await context(store: firstStore, uploader: ScriptedUploader(wouldThrow));
      final entry = await within('compose', first.outbox.compose(voiceDraft(bytes)));
      first.close();
      firstStore.close();

      // The relaunch: two contexts over the one directory.
      final clock = FakeClock();
      final server = ScriptedServer();
      final tA = ScriptedTransport(server);
      final tB = ScriptedTransport(server);
      SqliteDrainLock drainLock() {
        final locks = SqliteLocks(dir, timers: clock);
        addTearDown(locks.close);
        return SqliteDrainLock(locks, timers: clock);
      }

      final calls = <String>[];
      final answer = Completer<Uuid>();
      final storeA = openStore(dir);
      final a = await context(store: storeA, transport: tA, clock: clock, lock: drainLock(), rereadMs: holderRereadMs, uploader: ScriptedUploader((e, x) => answer.future, name: 'A ', calls: calls));
      final b = await context(store: openStore(dir), transport: tB, clock: clock, lock: drainLock(), rereadMs: holderRereadMs, uploader: ScriptedUploader((e, x) => answer.future, name: 'B ', calls: calls));
      for (final ctx in [a, b]) {
        final reloaded = ctx.item(entry.clientId)!;
        expect(reloaded.state, OutboxState.queued);
        expect(reloaded.entry.attachments.single.upload, UploadState.pending);
      }
      await clock.advance(holderRereadMs * 2);
      expect(calls, isEmpty, reason: 'nothing is offered before a session is ready');

      tA.open();
      await flush();
      tB.open();
      await clock.advance(200);
      expect((a.outbox.isHolder, b.outbox.isHolder), (true, false));
      expect(calls, ['A ${entry.clientId}'], reason: 'offered on the first ready, by the holder');

      // B is ready too, reads the same store, and is not the holder.
      await b.outbox.refresh();
      await clock.advance(holderRereadMs * 3);
      expect(calls, ['A ${entry.clientId}'], reason: 'exactly once: not again at a re-read, and never by the context without the lock');

      answer.complete(uuid(5004));
      await clock.advance(holderRereadMs);
      expect(tA.framesFor(entry.clientId).single.toJson()['attachments'], [
        {'kind': 'voice', 'upload_id': uuid(5004)},
      ]);
      expect(tB.frames, isEmpty);
      expect(calls, ['A ${entry.clientId}']);
      expect(await storeA.media(entry.clientId, 0), bytes, reason: 'the media it was recorded with is what the relaunch held');
    });

    test('a recording composed in a ready context that does not hold the lock is uploaded by the holder, once', () async {
      final dir = tempDir();
      final clock = FakeClock();
      final server = ScriptedServer();
      final tA = ScriptedTransport(server);
      final tB = ScriptedTransport(server);
      SqliteDrainLock drainLock() {
        final locks = SqliteLocks(dir, timers: clock);
        addTearDown(locks.close);
        return SqliteDrainLock(locks, timers: clock);
      }

      final calls = <String>[];
      final a = await context(store: openStore(dir), transport: tA, clock: clock, lock: drainLock(), rereadMs: holderRereadMs, uploader: ScriptedUploader((e, x) async => uuid(5005), name: 'A ', calls: calls));
      final b = await context(store: openStore(dir), transport: tB, clock: clock, lock: drainLock(), rereadMs: holderRereadMs, uploader: ScriptedUploader((e, x) async => uuid(5006), name: 'B ', calls: calls));
      tA.open();
      await flush();
      tB.open();
      await clock.advance(200);
      expect((a.outbox.isHolder, b.outbox.isHolder), (true, false));

      final entry = await within('compose', b.outbox.compose(voiceDraft(recording())));
      expect(calls, isEmpty, reason: 'the context without the lock does not upload');
      await clock.advance(holderRereadMs * 2);
      expect(calls, ['A ${entry.clientId}']);
      expect(tA.framesFor(entry.clientId), hasLength(1));
      expect(tB.frames, isEmpty);
    });
  });
}
