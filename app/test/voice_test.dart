// The recorder behind REC (CANT-318, CANT-247 ruling 0 → B). Two things are
// held. The recorder is built: it records, cancels, and composes a `voice`
// attachment from the file as recorded, with its own `durationMs` and no
// peaks. And it is gated off: while `voiceSendEnabled` is false REC is drawn
// disabled and no recording and no entry can come from the screen, because a
// voice note cannot be sent until CANT-56 gives the server peaks to resolve it
// with.

import 'dart:async';
import 'dart:io';
import 'dart:typed_data';

import 'package:catenary/main.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/attach.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/store/voice.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary/tokens.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show credential, me, projection, room;

final class _DeadSocket implements WebSocketLike {
  @override
  void Function()? onOpen;
  @override
  void Function(Object? data)? onMessage;
  @override
  void Function(int? code)? onClose;

  @override
  void send(String data) => throw StateError('never open');

  @override
  void close([int? code, String? reason]) {}
}

/// A recorder that writes a real file, so the store's copy-and-delete is real.
final class _FakeRecorder implements VoiceRecorder {
  _FakeRecorder(this.dir, {this.permitted = true});

  final Directory dir;
  bool permitted;
  var starts = 0, stops = 0, cancels = 0;
  final bytes = Uint8List.fromList([0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70, 0x4d, 0x34, 0x41, 0x20, 0xff, 0xfe, 0x80]);
  File? file;

  @override
  Future<bool> start() async {
    starts++;
    if (!permitted) return false;
    file = File('${dir.path}/take-$starts.m4a')..writeAsBytesSync(bytes);
    return true;
  }

  @override
  Future<RecordedVoice?> stop() async {
    stops++;
    final f = file;
    return f == null ? null : RecordedVoice(path: f.path, durationMs: 4200);
  }

  @override
  Future<void> cancel() async {
    cancels++;
    file?.deleteSync();
    file = null;
  }

  @override
  Future<void> dispose() async {}
}

void main() {
  late Directory dir;
  late Directory scratch;

  SessionSeams seams() => SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) => _DeadSocket(),
        fetch: (_) async => const HttpAnswer(503, ''),
        uploadFetch: (_) => Completer<HttpAnswer>().future,
      );

  Future<void> enroll() async {
    final credentials = openCredentialStore(dir.path);
    await enrollCredential(credentials, inProcessLock(), credential());
    credentials.close();
    writeAddress(dir.path, 'http://192.168.1.20:4012');
    final journal = SqliteJournal.open('${dir.path}/$journalFileName');
    final p = projection();
    await journal.applyPage(
      wire.SyncResponse(
        logSeq: 200,
        messages: p.messages.where((m) => m.state != wire.DeliveryState.unknown).toList(),
        conversations: p.conversations,
        users: p.users.values.toList(),
        hasMore: false,
        serverTime: '2026-10-04T12:05:00.000Z',
      ),
      Faults.none,
    );
    journal.close();
  }

  Future<List<OutboxEntry>> held() async {
    final s = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
    try {
      return await s.list();
    } finally {
      s.close();
    }
  }

  setUp(() {
    dir = Directory.systemTemp.createTempSync('catenary-voice-');
    scratch = Directory.systemTemp.createTempSync('catenary-voice-rec-');
  });
  tearDown(() {
    dir.deleteSync(recursive: true);
    scratch.deleteSync(recursive: true);
  });

  test('the gate is off: this is the line CANT-56 flips, and flipping it is meant to fail here', () {
    expect(voiceSendEnabled, isFalse);
  });

  group('the gate, through the store', () {
    test('off (the shipped value): REC is not offered, and nothing starts or composes even if called', () async {
      await enroll();
      final rec = _FakeRecorder(scratch);
      final store = AppStore(seams(), systemClock, wipeJournalFile, null, rec);
      addTearDown(store.dispose);
      await store.start();
      expect(store.canRecord, isFalse);
      expect(await store.startRecording(), isA<RecordRefused>());
      await store.sendRecording(room);
      expect(rec.starts, 0);
      expect(rec.stops, 0);
      expect(await held(), isEmpty);
    });

    test('on, with a recorder: REC is offered; with no recorder it is not', () async {
      await enroll();
      final store = AppStore(seams(), systemClock, wipeJournalFile, null, _FakeRecorder(scratch), true);
      addTearDown(store.dispose);
      expect(store.canRecord, isTrue);
      final bare = AppStore(seams(), systemClock, wipeJournalFile, null, null, true);
      addTearDown(bare.dispose);
      expect(bare.canRecord, isFalse);
    });
  });

  group('the recorder, with the gate on', () {
    Future<(AppStore, _FakeRecorder)> started({bool permitted = true}) async {
      await enroll();
      final rec = _FakeRecorder(scratch, permitted: permitted);
      final store = AppStore(seams(), systemClock, wipeJournalFile, null, rec, true);
      addTearDown(store.dispose);
      await store.start();
      return (store, rec);
    }

    test('a refused microphone is an inline message, nothing is recording, and nothing is composed', () async {
      final (store, rec) = await started(permitted: false);
      final r = await store.startRecording();
      expect((r as RecordRefused).message, microphoneDenied);
      expect(rec.starts, 1);
      expect(await held(), isEmpty);
    });

    test('SEND composes one voice attachment: the file as recorded, durationMs from the recorder, no peaks anywhere, offline included', () async {
      final (store, rec) = await started();
      expect(await store.startRecording(), isA<RecordingStarted>());
      final path = rec.file!.path;
      await store.sendRecording(room);

      final entries = await held();
      expect(entries, hasLength(1));
      final a = entries.single.attachments.single;
      expect((a.kind, a.durationMs), ('voice', 4200));
      expect(a.filename, isNull);
      final store2 = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
      addTearDown(store2.close);
      expect(await store2.media(entries.single.clientId, 0), rec.bytes, reason: 'submitted as recorded');
      expect(entries.single.toJson().toString().contains('peaks'), isFalse, reason: 'no client computes or carries peaks (Invariant 3)');
      expect(File(path).existsSync(), isFalse, reason: 'the outbox holds its own copy, so the temporary file is gone');
    });

    test('cancel keeps nothing: the recorder lets go and no entry is composed', () async {
      final (store, rec) = await started();
      await store.startRecording();
      await store.cancelRecording();
      expect(rec.cancels, 1);
      expect(await held(), isEmpty);
      await store.sendRecording(room);
      expect(await held(), isEmpty, reason: 'a send with nothing recording composes nothing');
    });
  });

  group('the screen', () {
    const live = ConnectionView.live();

    Future<void> pumpThread(WidgetTester tester, Widget thread) {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      return tester.pumpWidget(MaterialApp(theme: catenaryTheme(Brightness.dark), home: TickerMode(enabled: false, child: thread)));
    }

    ThreadScreen thread({Future<RecordStart> Function()? start, Future<void> Function()? cancel, Future<void> Function()? send}) => ThreadScreen(
          conversation: conversationViews(projection: projection(), outbox: const [], me: me, secure: true).first,
          connection: live,
          onSend: (_) async {},
          onStartRecording: start,
          onCancelRecording: cancel,
          onSendRecording: send,
        );

    testWidgets('with nothing wired (the gate off) REC is drawn disabled and a tap starts nothing', (tester) async {
      var starts = 0;
      await pumpThread(tester, thread(cancel: () async {}, send: () async => starts++));
      final t = CatenaryTokens.of(tester.element(find.byType(ThreadScreen)));
      expect(tester.widget<Text>(find.text('REC')).style!.color, t.textDisabled);
      await tester.tap(find.text('REC'));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-recording')), findsNothing);
      expect(starts, 0);
    });

    testWidgets('wired: REC starts a recording; the clock shows; cancel ends it without sending', (tester) async {
      var cancels = 0, sends = 0;
      await pumpThread(tester, thread(start: () async => const RecordingStarted(), cancel: () async => cancels++, send: () async => sends++));
      await tester.tap(find.text('REC'));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-recording')), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('composer-cancel-recording')));
      await tester.pump();
      expect((cancels, sends), (1, 0));
      expect(find.byKey(const ValueKey('composer-recording')), findsNothing);
    });

    testWidgets('wired: SEND on a recording hands it over once and returns to idle', (tester) async {
      var sends = 0;
      await pumpThread(tester, thread(start: () async => const RecordingStarted(), cancel: () async {}, send: () async => sends++));
      await tester.tap(find.text('REC'));
      await tester.pump();
      await tester.tap(find.text('SEND'));
      await tester.pump();
      expect(sends, 1);
      expect(find.byKey(const ValueKey('composer-recording')), findsNothing);
    });

    testWidgets('a refused microphone shows its words inline and stays idle', (tester) async {
      await pumpThread(tester, thread(start: () async => const RecordRefused(microphoneDenied), cancel: () async {}, send: () async {}));
      await tester.tap(find.text('REC'));
      await tester.pump();
      expect(find.text(microphoneDenied), findsOneWidget);
      expect(find.byKey(const ValueKey('composer-recording')), findsNothing);
    });

    testWidgets('a send the outbox refuses is shown inline and the recording is over', (tester) async {
      await pumpThread(
        tester,
        thread(start: () async => const RecordingStarted(), cancel: () async {}, send: () async => throw const AttachRefused(ComposeRefused.terminal)),
      );
      await tester.tap(find.text('REC'));
      await tester.pump();
      await tester.tap(find.text('SEND'));
      await tester.pump();
      expect(find.text(ComposeRefused.terminal), findsOneWidget);
      expect(find.byKey(const ValueKey('composer-recording')), findsNothing);
    });

    for (final on in [false, true]) {
      testWidgets('the app, gate ${on ? 'on' : 'off'}: REC is ${on ? 'live and starts the recorder' : 'disabled and starts nothing'}', (tester) async {
        tester.view.physicalSize = const Size(390, 844);
        tester.view.devicePixelRatio = 1;
        addTearDown(tester.view.reset);
        await tester.runAsync(enroll);
        final rec = _FakeRecorder(scratch);
        final store = on ? AppStore(seams(), systemClock, wipeJournalFile, null, rec, true) : AppStore(seams(), systemClock, wipeJournalFile, null, rec);
        addTearDown(store.dispose);
        expect(await tester.runAsync(store.start), isTrue);
        await tester.pumpWidget(CatenaryApp(initialMode: ThemeMode.dark, store: store));
        await tester.tap(find.byKey(const ValueKey('rail-$room')));
        await tester.pump();
        await tester.pump(const Duration(seconds: 1));

        final t = CatenaryTokens.of(tester.element(find.byType(ThreadScreen)));
        expect(tester.widget<Text>(find.text('REC')).style!.color, on ? t.onAccent : t.textDisabled);
        await tester.runAsync(() async {
          await tester.tap(find.text('REC'));
          await Future<void>.delayed(const Duration(milliseconds: 50));
        });
        await tester.pump();
        expect(rec.starts, on ? 1 : 0);
        expect(find.byKey(const ValueKey('composer-recording')), on ? findsOneWidget : findsNothing);
        expect(await tester.runAsync(held), isEmpty, reason: 'no entry is composed by starting');
      });
    }
  });
}
