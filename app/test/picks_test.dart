// The choices CANT-205…CANT-211 made alone, as CANT-220's plan ruled on them,
// each held by a test so a later ticket cannot move one without a test saying
// so. Ruling 4 (an acked entry reads SENT) is held in app_store_test.dart, and
// ruling 7 (REC with no recorder) in states_test.dart.
//
//   ruling 5   a failed transcript says NO TRANSCRIPT, never TRANSCRIBING
//   ruling 6   a wire value this build does not know claims the least it can
//   ruling 8   a credential the server issued and this device could not store
//              is not "could not reach that server"
//   and RETRYING reaches the thread (CANT-36 ruling 3 C), which no ruling
//   was needed for: the word was settled and the thread did not carry it.

import 'dart:io';

import 'package:catenary/store/address.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
import 'package:catenary/store/enrollment.dart';
import 'package:catenary/store/status.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show conversations, item, me, message, nadia, projection, room, users;
import 'enroll_test.dart' show Server, token;

/// The room holding one voice note of Nadia's, 1:12 long, whose transcript is
/// in [state].
ConversationView noteIn(wire.TranscriptState state, {String? text}) {
  final p = Projection(conversations: conversations, users: users, messages: [
    message('00000001-0000-4000-8000-000000000000', room, 1, nadia, '2026-10-04T12:00:00.000Z', text: null, attachments: [
      wire.VoiceAttachment(url: '/media/v', durationMs: 72000, peaks: const [9, 50, 100], transcript: wire.Transcript(state: state, text: text)),
    ]),
  ]);
  return conversationViews(projection: p, outbox: const [], me: me, secure: true).firstWhere((c) => c.id == room);
}

Future<void> showThread(WidgetTester tester, ConversationView thread) {
  tester.view.physicalSize = const Size(390, 1200);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  return tester.pumpWidget(MaterialApp(
    theme: catenaryTheme(Brightness.dark),
    home: TickerMode(enabled: false, child: ThreadScreen(conversation: thread, connection: const ConnectionView.live())),
  ));
}

/// A store that reads as empty and cannot be written: a full disk, a file
/// that will not open.
final class _UnwritableStore implements CredentialStore {
  @override
  Future<StoredCredential?> read() async => null;

  @override
  Future<T> update<T>(CredentialWrite<T> Function(StoredCredential? held) fn) async => throw const FileSystemException('disk full');
}

void main() {
  group('[ruling 5] a failed transcript', () {
    test('is not a pending one in the view model, and the rail does not say it is coming', () {
      final failed = noteIn(wire.TranscriptState.failed);
      final pending = noteIn(wire.TranscriptState.pending);
      expect(failed.messages.single.voice!.status, TranscriptStatus.failed);
      expect(pending.messages.single.voice!.status, TranscriptStatus.pending);
      expect(failed.messages.single.voice!.transcript, isNull);
      expect(preview(failed), 'Nadia: voice note · 1:12');
      expect(preview(pending), 'Nadia: transcript pending · 1:12');
    });

    testWidgets('draws NO TRANSCRIPT and not TRANSCRIBING', (tester) async {
      await showThread(tester, noteIn(wire.TranscriptState.failed));
      expect(find.text('NO TRANSCRIPT'), findsOneWidget);
      expect(find.text('TRANSCRIBING'), findsNothing);

      await showThread(tester, noteIn(wire.TranscriptState.pending));
      expect(find.text('TRANSCRIBING'), findsOneWidget);
      expect(find.text('NO TRANSCRIPT'), findsNothing);
    });

    testWidgets('and a ready one is still its text, with the word count', (tester) async {
      await showThread(tester, noteIn(wire.TranscriptState.ready, text: 'two words'));
      expect(find.text('two words'), findsOneWidget);
      expect(find.text('EXPAND · 2 W'), findsOneWidget);
    });
  });

  group('[ruling 6] a wire value this build does not know', () {
    test('an unknown delivery state is sent: the least a record the server holds can be', () {
      expect(deliveryStatus(wire.DeliveryState.unknown), MessageStatus.sent);
    });

    test('a conversation of an unknown kind is listed with the rooms', () {
      const odd = wire.Conversation(id: room, kind: wire.ConversationKind.unknown, name: 'Something new', memberCount: 4, headSeq: 0);
      final view = conversationViews(
        projection: const Projection(conversations: [odd], users: users),
        outbox: const [],
        me: me,
        secure: true,
      ).single;
      expect(view.kind, ConversationKind.group);
      expect(view.name, 'Something new');
    });

    test('a transcript in an unknown state is unknown in the view model, not pending', () {
      final note = noteIn(wire.TranscriptState.unknown);
      expect(note.messages.single.voice!.status, TranscriptStatus.unknown);
      expect(preview(note), 'Nadia: voice note · 1:12');
    });

    testWidgets('and its note is drawn with no transcript strip at all', (tester) async {
      await showThread(tester, noteIn(wire.TranscriptState.unknown));
      expect(find.text('1:12'), findsOneWidget, reason: 'the note itself is drawn');
      expect(find.text('TRANSCRIBING'), findsNothing);
      expect(find.text('NO TRANSCRIPT'), findsNothing);
      expect(find.text('TRANSCRIPT'), findsNothing);
    });
  });

  group('RETRYING reaches the thread [CANT-36 ruling 3 C]', () {
    ConversationView withTail(bool retrying) => conversationViews(
          projection: projection(),
          outbox: [item('c-held', OutboxState.queued, retrying: retrying)],
          me: me,
          secure: true,
        ).firstWhere((c) => c.id == room);

    test('the view model carries the outbox\'s flag, and the rail\'s mark does too', () {
      expect(withTail(true).messages.last.retrying, isTrue);
      expect(withTail(false).messages.last.retrying, isFalse);
      expect(railMark(withTail(true)).kind, RailMarkKind.count, reason: 'the unread reply outranks the mark');
      final read = ConversationView(id: room, kind: ConversationKind.group, name: 'r', memberCount: 3, messages: withTail(true).messages, unacked: 1);
      expect((railMark(read).status, railMark(read).retrying), (MessageStatus.queued, true));
    });

    testWidgets('a queued entry past its third retryable refusal reads RETRYING, and one that is not reads QUEUED', (tester) async {
      await showThread(tester, withTail(true));
      expect(find.text('RETRYING'), findsOneWidget);
      expect(find.text('QUEUED'), findsNothing);

      await showThread(tester, withTail(false));
      expect(find.text('RETRYING'), findsNothing);
      expect(find.text('QUEUED'), findsOneWidget);
    });
  });

  group('[ruling 8] the server accepted the enrollment and the device could not store the credential', () {
    late Directory dir;
    late Server server;

    setUp(() {
      dir = Directory.systemTemp.createTempSync('catenary-picks-');
      server = Server();
    });
    tearDown(() => dir.deleteSync(recursive: true));

    Future<EnrollOutcome> attempt(CredentialStore store) => enrollDeviceAt(
          directory: dir.path,
          store: store,
          typedAddress: 'chat.example.com',
          token: token,
          deviceName: 'Android phone',
          release: true,
          fetch: server.call,
        );

    test('says this device could not save it and that a fresh token is needed, not that the server was unreachable', () async {
      final outcome = await attempt(_UnwritableStore()) as EnrollFailed;
      expect(server.enrolls, 1, reason: 'the token was spent');
      expect(outcome.text, contains('could not save the credential'));
      expect(outcome.text, contains('fresh one'));
      expect(outcome.text.toLowerCase(), isNot(contains('reach')));
      expect(readAddress(dir.path), isNull, reason: 'no address was stored before, and none is left');
    });

    test('and the address that was stored before the attempt is put back', () async {
      writeAddress(dir.path, 'https://old.example.com');
      final outcome = await attempt(_UnwritableStore());
      expect(outcome, isA<EnrollFailed>());
      expect(readAddress(dir.path), 'https://old.example.com');
    });

    test('a failed probe is still the unreachable text, and spends no token', () async {
      server.health = 503;
      final outcome = await attempt(_UnwritableStore()) as EnrollFailed;
      expect(outcome.text, startsWith('Could not reach that server'));
      expect(server.enrolls, 0);
    });
  });
}
