// A ready transcript with no words in it (CANT-236 ruling 0, pick A): the
// job finished and found nothing to write down, so the strip reads NO SPEECH
// in the failed strip's place and style, with nothing to expand.

import 'package:catenary/thread.dart';
import 'package:catenary/tokens.dart';
import 'package:catenary_wire/catenary_wire.dart' as wire;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'transcript_pending_test.dart' show noteWith, showThread;

void main() {
  test('the label is the ruled one', () {
    expect(noSpeechLabel, 'NO SPEECH');
  });

  for (final (name, text) in [('the empty string', ''), ('only whitespace', '  \n')]) {
    testWidgets('a ready transcript of $name draws NO SPEECH and nothing to expand', (tester) async {
      await showThread(tester, noteWith(wire.Transcript(state: wire.TranscriptState.ready, text: text)));

      expect(find.text('NO SPEECH'), findsOneWidget);
      expect(find.textContaining('EXPAND'), findsNothing);
      expect(find.textContaining('0 W'), findsNothing);
      expect(find.text('TRANSCRIPT'), findsNothing);
      expect(find.byKey(const ValueKey('transcript-toggle')), findsNothing);
      expect(find.text('NO TRANSCRIPT'), findsNothing, reason: 'the job did not fail');
      expect(find.text('TRANSCRIBING'), findsNothing);

      // No transcript body: the label is the only text under the player's row.
      final strip = find.ancestor(of: find.text('NO SPEECH'), matching: find.byType(Container)).first;
      expect(find.descendant(of: strip, matching: find.byType(Text)), findsOneWidget);
    });
  }

  testWidgets('it is drawn as the failed strip is: same style, same height', (tester) async {
    await showThread(tester, noteWith(const wire.Transcript(state: wire.TranscriptState.failed)));
    final failed = tester.widget<Text>(find.text('NO TRANSCRIPT')).style;
    final failedSize = tester.getSize(find.byType(VoiceNoteBlock));

    await showThread(tester, noteWith(const wire.Transcript(state: wire.TranscriptState.ready, text: '')));
    final silent = tester.widget<Text>(find.text('NO SPEECH')).style;
    expect(silent, failed);
    expect(silent!.color, CatenaryTokens.dark.textMeta, reason: 'no accent: nothing here is tappable');
    expect(tester.getSize(find.byType(VoiceNoteBlock)), failedSize);
  });

  testWidgets('one word is a transcript again', (tester) async {
    await showThread(tester, noteWith(const wire.Transcript(state: wire.TranscriptState.ready, text: ' hm ')));
    expect(find.text('EXPAND · 1 W'), findsOneWidget);
    expect(find.text('NO SPEECH'), findsNothing);
  });
}
