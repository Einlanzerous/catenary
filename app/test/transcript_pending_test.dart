// The pending transcript's strip (CANT-229), held to the narrow canvas's frame
// 04 B: the pulse, TRANSCRIBING with the server's estimate when there is one,
// and two skeleton lines that reserve the collapsed transcript's height.

import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary/tokens.dart';
import 'package:catenary/widgets/pulse.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show conversations, me, message, nadia, room, users;

/// Long enough to wrap past two lines at 390 wide, so the collapsed transcript
/// is at its full two-line height.
const _long =
    'Okay so I talked to Ted about the delivery and the short version is Thursday still works but they want us to confirm the count by tomorrow noon and I said that should be fine.';

/// The room holding one voice note of Nadia's with this transcript.
ConversationView noteWith(wire.Transcript transcript) {
  final p = Projection(conversations: conversations, users: users, messages: [
    message('00000001-0000-4000-8000-000000000000', room, 1, nadia, '2026-10-04T12:00:00.000Z', text: null, attachments: [
      wire.VoiceAttachment(url: '/media/v', durationMs: 72000, peaks: const [9, 50, 100], transcript: transcript),
    ]),
  ]);
  return conversationViews(projection: p, outbox: const [], me: me, secure: true).firstWhere((c) => c.id == room);
}

ConversationView pending({int? etaSec}) => noteWith(wire.Transcript(state: wire.TranscriptState.pending, etaSec: etaSec));

Future<void> showThread(WidgetTester tester, ConversationView thread, {Brightness brightness = Brightness.dark, bool motion = false}) {
  tester.view.physicalSize = const Size(390, 1200);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  return tester.pumpWidget(MaterialApp(
    theme: catenaryTheme(brightness),
    home: TickerMode(enabled: motion, child: ThreadScreen(conversation: thread, connection: const ConnectionView.live())),
  ));
}

Color colorOf(WidgetTester tester, Finder container) =>
    tester.widget<Container>(find.descendant(of: container, matching: find.byType(Container)).last).color!;

void main() {
  group('the view model', () {
    test('carries the wire\'s estimate while pending, and none otherwise', () {
      expect(pending(etaSec: 20).messages.single.voice!.eta, const Duration(seconds: 20));
      expect(pending().messages.single.voice!.eta, isNull);
      expect(pending(etaSec: 0).messages.single.voice!.eta, isNull, reason: 'a zero is no estimate, as the web has it');
      final ready = noteWith(const wire.Transcript(state: wire.TranscriptState.ready, text: 'two words', etaSec: 20));
      expect(ready.messages.single.voice!.eta, isNull);
    });
  });

  group('the strip', () {
    testWidgets('says TRANSCRIBING with the estimate when the wire carries one, and without it when it does not', (tester) async {
      await showThread(tester, pending(etaSec: 20));
      expect(find.text('TRANSCRIBING · ~20 S'), findsOneWidget);

      await showThread(tester, pending());
      expect(find.text('TRANSCRIBING'), findsOneWidget);
      expect(find.textContaining('~'), findsNothing);
    });

    for (final (name, brightness, tokens) in [
      ('dark', Brightness.dark, CatenaryTokens.dark),
      ('light', Brightness.light, CatenaryTokens.light),
    ]) {
      testWidgets('draws the pulse and both skeleton lines in the $name theme\'s tokens', (tester) async {
        await showThread(tester, pending(etaSec: 20), brightness: brightness);

        final pulse = find.byKey(const ValueKey('transcript-pulse'));
        expect(tester.getSize(pulse), const Size(5, 5));
        expect(tester.widget<Container>(pulse).color, tokens.accentWire);
        expect(find.ancestor(of: pulse, matching: find.byType(Pulse)), findsOneWidget);
        expect(tester.widget<Text>(find.text('TRANSCRIBING · ~20 S')).style!.color, tokens.accentDim);

        // The strip is the block's width less its 12px padding either side.
        final inner = tester.getSize(find.byKey(const ValueKey('transcript-pending'))).width;
        for (final (key, fraction) in [('transcript-skeleton-1', 0.88), ('transcript-skeleton-2', 0.58)]) {
          final bar = find.descendant(of: find.byKey(ValueKey(key)), matching: find.byType(Container));
          expect(tester.getSize(bar).height, 9);
          expect(tester.getSize(bar).width, closeTo(inner * fraction, 0.01));
          expect(tester.widget<Container>(bar).color, tokens.surfaceRaised);
          expect(find.ancestor(of: bar, matching: find.byType(Pulse)), findsOneWidget);
        }
      });
    }

    testWidgets('pulses and shimmers when motion is on', (tester) async {
      await showThread(tester, pending(), motion: true);
      double opacity(String key) => tester
          .widget<FadeTransition>(find.ancestor(of: find.byKey(ValueKey(key)), matching: find.byType(FadeTransition)).first)
          .opacity
          .value;
      double barOpacity(String key) => tester
          .widget<FadeTransition>(find.descendant(of: find.byKey(ValueKey(key)), matching: find.byType(FadeTransition)))
          .opacity
          .value;
      // The first frame starts each cycle; the clock runs from the next.
      await tester.pump();
      expect(opacity('transcript-pulse'), 1);
      expect(barOpacity('transcript-skeleton-1'), closeTo(0.35, 1e-9));
      // Half a period on, each is at its turn.
      await tester.pump(const Duration(milliseconds: 650));
      expect(opacity('transcript-pulse'), closeTo(0.25, 1e-9));
      await tester.pump(const Duration(milliseconds: 150));
      expect(barOpacity('transcript-skeleton-1'), closeTo(0.85, 1e-9));
      expect(barOpacity('transcript-skeleton-2'), isNot(closeTo(0.85, 1e-3)), reason: 'the second line follows the first by 0.3s');
      await tester.pumpWidget(const SizedBox());
    });

    testWidgets('a two-line transcript landing does not change the note\'s height', (tester) async {
      await showThread(tester, pending(etaSec: 20));
      final before = tester.getSize(find.byType(VoiceNoteBlock));

      await showThread(tester, noteWith(const wire.Transcript(state: wire.TranscriptState.ready, text: _long)));
      expect(find.byKey(const ValueKey('transcript-pending')), findsNothing);
      expect(find.textContaining('EXPAND'), findsOneWidget);
      expect(tester.getSize(find.byType(VoiceNoteBlock)), before);
    });

    testWidgets('a failed or unknown transcript draws no pulse and no skeleton', (tester) async {
      for (final state in [wire.TranscriptState.failed, wire.TranscriptState.unknown]) {
        await showThread(tester, noteWith(wire.Transcript(state: state)));
        expect(find.byKey(const ValueKey('transcript-pulse')), findsNothing);
        expect(find.byKey(const ValueKey('transcript-skeleton-1')), findsNothing);
      }
    });
  });
}
