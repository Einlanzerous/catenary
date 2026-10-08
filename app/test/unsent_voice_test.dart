// An unsent voice note draws nothing under its player (CANT-252, CANT-249's
// app half): no TRANSCRIBING, no pulse, no skeleton. The row's status mark
// already says everything that is true of it.

import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show conversations, item, me, room, users;

OutboxItem voiceItem(String id, OutboxState state) {
  final e = item(id, state, text: '').entry;
  return OutboxItem(
    entry: OutboxEntry(
      clientId: e.clientId,
      accountId: e.accountId,
      conversationId: e.conversationId,
      order: e.order,
      composedAt: e.composedAt,
      status: e.status,
      lastError: e.lastError,
      attachments: const [OutboundAttachmentDraft(kind: 'voice', durationMs: 12000)],
    ),
    state: state,
    ack: null,
    retrying: false,
  );
}

void main() {
  for (final state in [OutboxState.queued, OutboxState.sending, OutboxState.failed]) {
    testWidgets('a ${state.name} voice note has its player, its status mark and no strip', (tester) async {
      final thread = conversationViews(
        projection: Projection(conversations: conversations, users: users, messages: const []),
        outbox: [voiceItem('v-${state.name}', state)],
        me: me,
        secure: true,
      ).firstWhere((c) => c.id == room);
      tester.view.physicalSize = const Size(390, 1200);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: ThreadScreen(conversation: thread, connection: const ConnectionView.live()),
      ));
      expect(find.byKey(const ValueKey('voice-play')), findsOneWidget);
      expect(find.textContaining('TRANSCRIBING'), findsNothing);
      expect(find.byKey(const ValueKey('transcript-pending')), findsNothing);
      expect(find.byKey(const ValueKey('transcript-pulse')), findsNothing);
      expect(find.byKey(const ValueKey('transcript-skeleton-1')), findsNothing);
      expect(find.byKey(const ValueKey('group-status')), findsOneWidget);
    });
  }
}
