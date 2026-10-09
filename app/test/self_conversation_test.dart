// A conversation with only yourself (CANT-261, under CANT-254): the `self`
// wire kind has its own arm in the store, the rail pins it first in DIRECT as
// Notes, the header says JUST YOU over a derived transport word, the preview
// is the bare body, and the receipt is drawn as the server serves it.

import 'package:catenary/rail.dart';
import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
import 'package:catenary/store/status.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

const me = '11111111-1111-4111-8111-111111111111';
const nadia = '33333333-3333-4333-8333-333333333333';
const notesId = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const roomId = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const directId = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';

final t0 = DateTime.utc(2026, 10, 4, 12);

ThreadMessage msg(int seq, String text, {bool mine = true, MessageStatus status = MessageStatus.sent, VoiceNote? voice}) => ThreadMessage(
      id: '0000000$seq-0000-4000-8000-000000000000',
      seq: seq,
      authorId: mine ? me : nadia,
      authorName: mine ? 'Hollis Brandt' : 'Nadia Okonkwo',
      at: t0.add(Duration(minutes: seq)),
      mine: mine,
      text: text,
      status: status,
      voice: voice,
    );

ConversationView view(String id, ConversationKind kind, String name, List<ThreadMessage> messages, {int members = 1, bool secure = true}) =>
    ConversationView(id: id, kind: kind, name: name, memberCount: members, messages: messages, secure: secure);

void phone(WidgetTester tester) {
  tester.view.physicalSize = const Size(390, 844);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
}

Widget app(Widget home) => MaterialApp(theme: catenaryTheme(Brightness.dark), home: TickerMode(enabled: false, child: home));

void main() {
  final notes = view(notesId, ConversationKind.self, 'Notes', [msg(1, 'buy oat milk')]);
  // A direct with a newer message than Notes.
  final dm = view(directId, ConversationKind.direct, 'Nadia Okonkwo', [msg(9, 'later', mine: false)], members: 2);
  final room = view(roomId, ConversationKind.group, 'Kitchen Table', [msg(3, 'hi', mine: false)], members: 3);

  group('the store', () {
    test('the wire self kind gets its own arm, and unknown is still drawn as a group', () {
      final p = Projection(
        conversations: const [
          wire.Conversation(id: notesId, kind: wire.ConversationKind.self, name: 'Notes', memberCount: 1, headSeq: 0),
          wire.Conversation(id: roomId, kind: wire.ConversationKind.unknown, name: 'Future', memberCount: 4, headSeq: 0),
        ],
        users: const {me: wire.User(id: me, name: 'Hollis Brandt')},
        messages: const [],
      );
      final views = conversationViews(projection: p, outbox: const [], me: me, secure: true);
      final byId = {for (final v in views) v.id: v};
      expect(byId[notesId]!.kind, ConversationKind.self);
      expect(byId[notesId]!.name, 'Notes');
      expect(byId[roomId]!.kind, ConversationKind.group);
      expect(byId[roomId]!.subtitle, '4 MEMBERS · TLS');
    });
  });

  group('the header', () {
    test('JUST YOU, its last word derived from the origin, and never E2E', () {
      expect(notes.subtitle, 'JUST YOU · TLS');
      final plain = view(notesId, ConversationKind.self, 'Notes', const [], secure: false);
      expect(plain.subtitle, 'JUST YOU · CLEARTEXT');
      expect(plain.subtitle.contains('E2E'), isFalse);
    });

    testWidgets('the thread draws the title and the chip', (tester) async {
      phone(tester);
      await tester.pumpWidget(app(ThreadScreen(conversation: notes, connection: const ConnectionView.live())));
      expect(find.text('Notes'), findsWidgets);
      expect(find.text('JUST YOU · TLS'), findsOneWidget);
    });
  });

  group('the rail', () {
    testWidgets('Notes is first in DIRECT, with the others in order, even under a newer direct', (tester) async {
      phone(tester);
      await tester.pumpWidget(app(RailScreen(
        conversations: [dm, room, notes],
        now: t0.add(const Duration(hours: 1)),
        connection: const ConnectionView.live(),
        myInitials: 'HB',
      )));
      expect(find.text('DIRECT'), findsOneWidget);
      expect(find.text('ROOMS'), findsOneWidget);
      double y(String id) => tester.getTopLeft(find.byKey(ValueKey('rail-$id'))).dy;
      expect(y(roomId), lessThan(y(notesId)));
      expect(y(notesId), lessThan(y(directId)));
      expect(find.text('Notes'), findsOneWidget);
      // DIRECT counts both of its rows.
      expect(find.text('2'), findsOneWidget);
    });
  });

  group('the preview', () {
    test('the bare body, with no You: prefix', () {
      expect(preview(notes), 'buy oat milk');
      expect(preview(view(roomId, ConversationKind.group, 'K', [msg(1, 'hi')], members: 3)), 'You: hi');
    });

    test('a pending transcript and an unsent voice note still read as before', () {
      const note = VoiceNote(duration: Duration(seconds: 12), peaks: [], status: TranscriptStatus.pending);
      final pending = view(notesId, ConversationKind.self, 'Notes', [msg(1, '', voice: note)]);
      expect(preview(pending), 'transcript pending · 0:12');
      final unsent = view(notesId, ConversationKind.self, 'Notes', [msg(1, '', voice: note, status: MessageStatus.queued)]);
      expect(preview(unsent), 'voice note · 0:12');
    });
  });

  group('the receipt', () {
    test('is drawn as served: SENT with one member, never READ, and no unread count for your own', () {
      expect(statusLabel(MessageStatus.sent, readBy: 1, memberCount: 1), 'SENT');
      expect(statusLabel(MessageStatus.queued, memberCount: 1), 'QUEUED');
      expect(statusLabel(MessageStatus.failed, memberCount: 1), 'FAILED');
      final own = ConversationView(
        id: notesId,
        kind: ConversationKind.self,
        name: 'Notes',
        memberCount: 1,
        firstUnreadSeq: 1,
        messages: [msg(1, 'a'), msg(2, 'b'), msg(3, 'c')],
      );
      expect(own.newCount, 0);
    });
  });
}
