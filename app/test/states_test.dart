// The connection states render (CANT-44): recording, offline-queued, failed
// and resyncing — and the ones around them — each checked for what it says
// and for what it refuses to offer, in both themes.

import 'dart:convert';
import 'dart:io';

import 'package:catenary/states.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/status.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/tokens.dart';
import 'package:catenary/widgets/composer.dart';
import 'package:catenary/widgets/connection_banner.dart';
import 'package:catenary/widgets/status_label.dart';
import 'package:catenary/widgets/status_mark.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

/// Pumps one widget under a theme, with tickers muted so a pulse does not
/// keep the test waiting.
Future<void> show(WidgetTester tester, Widget child, {Brightness brightness = Brightness.dark}) {
  return tester.pumpWidget(MaterialApp(
    theme: catenaryTheme(brightness),
    home: TickerMode(enabled: false, child: Scaffold(body: child)),
  ));
}

/// The fill of the 44px square at the right of the field.
Color? primaryFill(WidgetTester tester) => (tester
        .widget<Container>(find.descendant(of: find.byKey(const ValueKey('composer-primary')), matching: find.byType(Container)))
        .decoration! as BoxDecoration)
    .color;

void main() {
  for (final (brightness, t) in [(Brightness.dark, CatenaryTokens.dark), (Brightness.light, CatenaryTokens.light)]) {
    final theme = brightness.name;

    testWidgets('$theme · recording: the clock, ✕ and SEND, amber and not red; a slide left cancels', (tester) async {
      var cancelled = 0, sent = 0;
      await show(
        tester,
        Composer(
          connection: const ConnectionView.live(),
          recording: const Duration(seconds: 17),
          onCancelRecording: () => cancelled++,
          onSendRecording: () => sent++,
        ),
        brightness: brightness,
      );
      expect(find.text('00:17'), findsOneWidget);
      expect(find.text('slide left to cancel · lift to keep recording'), findsOneWidget);
      final box = tester.widget<Container>(find.byKey(const ValueKey('composer-recording')));
      expect((box.decoration! as BoxDecoration).border!.top.color, t.accentWire, reason: 'amber: red belongs to failure alone');
      // It replaces the whole composer row (M7).
      expect(find.byKey(const ValueKey('composer-input')), findsNothing);
      expect(find.text('ADD'), findsNothing);
      // The row's own SEND is the canvas's smaller square: 9px at .08em.
      final send = tester.widget<Text>(find.text('SEND')).style!;
      expect(send.fontSize, 9.0);
      expect(send.letterSpacing, closeTo(0.72, 1e-9));

      await tester.tap(find.text('SEND'));
      // The ✕ is the visible fallback, so the gesture is never the sole route.
      await tester.tap(find.byKey(const ValueKey('composer-cancel-recording')));
      expect((cancelled, sent), (1, 1));
      // A slide short of the threshold keeps recording; one past it cancels.
      await tester.drag(find.text('00:17'), const Offset(-(slideToCancel - 24), 0));
      expect(cancelled, 1);
      await tester.drag(find.text('00:17'), const Offset(-(slideToCancel + 24), 0));
      expect(cancelled, 2);
    });

    testWidgets('$theme · idle is ADD · Message · REC, and a draft turns REC into SEND', (tester) async {
      var sent = 0, recorded = 0;
      final draft = TextEditingController();
      await show(
        tester,
        Composer(connection: const ConnectionView.live(), controller: draft, onSend: () => sent++, onRecord: () => recorded++),
        brightness: brightness,
      );
      expect(find.text('Message'), findsOneWidget);
      expect(find.text('REC'), findsOneWidget);
      expect(primaryFill(tester), t.accentWire, reason: 'record is the accent-filled one');
      expect(tester.getSize(find.byKey(const ValueKey('composer-primary'))), const Size(44, 44));
      await tester.tap(find.text('REC'));
      expect(recorded, 1);

      draft.text = 'Eight works';
      await tester.pump();
      expect(find.text('REC'), findsNothing);
      await tester.tap(find.text('SEND'));
      expect(sent, 1);
    });

    // CANT-220 ruling 7: with nothing wired to record, REC is drawn disabled.
    // The accent says "this works", and it does not sit on a square that
    // does nothing — which is every build until recording exists.
    testWidgets('$theme · REC with no recorder is drawn disabled, live and offline, and filled with the accent once there is one', (tester) async {
      for (final connection in [const ConnectionView.live(), const ConnectionView(kind: ConnectionKind.offline)]) {
        await show(tester, Composer(connection: connection), brightness: brightness);
        expect(find.text('REC'), findsOneWidget, reason: connection.kind.name);
        expect(tester.widget<Text>(find.text('REC')).style!.color, t.textDisabled, reason: connection.kind.name);
        expect(primaryFill(tester), t.surfaceBase, reason: 'no accent fill, ${connection.kind.name}');
        expect(primaryFill(tester), isNot(t.accentWire));

        await show(tester, Composer(connection: connection, onRecord: () {}), brightness: brightness);
        expect(primaryFill(tester), t.accentWire, reason: connection.kind.name);
        expect(tester.widget<Text>(find.text('REC')).style!.color, t.onAccent);
      }
    });

    testWidgets('$theme · offline-queued: REC with no draft and QUEUE in the dimmer copper with one, the hint says so, ADD is off', (tester) async {
      var sent = 0, attached = 0, recorded = 0;
      final draft = TextEditingController();
      await show(
        tester,
        Composer(
          connection: const ConnectionView(kind: ConnectionKind.offline),
          controller: draft,
          onSend: () => sent++,
          onAttach: () => attached++,
          onRecord: () => recorded++,
        ),
        brightness: brightness,
      );
      // An empty draft: the outbox holds a recording offline (CANT-201 rulings
      // 1 and 2), so the square is REC, as on a live session.
      expect(find.text('REC'), findsOneWidget, reason: 'a recording is held with its media and sent on reconnect');
      expect(find.text('QUEUE'), findsNothing, reason: 'there is nothing to queue yet');
      expect(find.text('SEND'), findsNothing);
      expect(primaryFill(tester), t.accentWire);
      expect(find.text('Sends when reconnected'), findsOneWidget);
      await tester.tap(find.text('REC'));
      expect((recorded, sent), (1, 0));
      await tester.tap(find.text('ADD'));
      expect(attached, 0);
      expect(tester.widget<Text>(find.text('ADD')).style!.color, t.textDisabled);

      // A draft: QUEUE, and never SEND.
      draft.text = 'Eight works';
      await tester.pump();
      expect(find.text('QUEUE'), findsOneWidget);
      expect(find.text('REC'), findsNothing);
      expect(find.text('SEND'), findsNothing);
      expect(primaryFill(tester), t.accentQueue);
      // The canvas's own type for this square: 9px at .08em, where ADD beside
      // it is 9.5px at .1em (frame 04 A, OFFLINE — QUEUED).
      final queue = tester.widget<Text>(find.text('QUEUE')).style!;
      expect(queue.fontSize, 9.0);
      expect(queue.letterSpacing, closeTo(0.72, 1e-9));
      final add = tester.widget<Text>(find.text('ADD')).style!;
      expect(add.fontSize, 9.5);
      expect(add.letterSpacing, closeTo(0.95, 1e-9));
      await tester.tap(find.text('ADD'));
      expect(attached, 0, reason: 'ADD is off with a draft too');
      await tester.tap(find.text('QUEUE'));
      expect((recorded, sent), (1, 1));
    });

    testWidgets('$theme · terminal: nothing sends, and the accent is not on the button', (tester) async {
      var sent = 0;
      final draft = TextEditingController(text: 'Eight works');
      await show(
        tester,
        Composer(
          connection: const ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.credential),
          controller: draft,
          onSend: () => sent++,
        ),
        brightness: brightness,
      );
      expect(find.text('QUEUE'), findsNothing, reason: 'a terminal client drains nothing, so it does not offer to queue');
      expect(primaryFill(tester), t.surfaceBase);
      await tester.tap(find.text('SEND'));
      expect(sent, 0);
    });

    testWidgets('$theme · failed: the one other color, and only there', (tester) async {
      await show(tester, const StatusMark(status: MessageStatus.failed), brightness: brightness);
      expect(tester.widget<Text>(find.text('FAILED')).style!.color, t.signalFault);
      for (final s in MessageStatus.values.where((s) => s != MessageStatus.failed)) {
        await show(tester, StatusMark(status: s), brightness: brightness);
        expect(tester.widget<Text>(find.byType(Text)).style!.color, isNot(t.signalFault), reason: '${s.name} is not a failure');
        await show(tester, StatusLabel(status: s), brightness: brightness);
        expect(tester.widget<Text>(find.byType(Text)).style!.color, t.textMeta);
      }
    });

    testWidgets('$theme · status words become marks: • sent, •• delivered, •• read in the accent', (tester) async {
      for (final (status, text, color) in [
        (MessageStatus.sent, '•', t.textMeta),
        (MessageStatus.delivered, '••', t.textMeta),
        (MessageStatus.read, '••', t.accentWire),
        (MessageStatus.queued, 'QUEUED', t.textMeta),
        (MessageStatus.sending, 'SENDING', t.textMeta),
      ]) {
        await show(tester, StatusMark(status: status), brightness: brightness);
        final mark = tester.widget<Text>(find.byType(Text));
        expect((mark.data, mark.style!.color), (text, color), reason: status.name);
      }
    });

    // CANT-203 ruling 0, as built: queued and sending are words, and a send
    // held under backoff reads RETRYING. Sent, delivered and read are marks.
    testWidgets('$theme · queued, sending and retrying are words in meta grey; the rest are marks', (tester) async {
      for (final (status, retrying, word) in [
        (MessageStatus.queued, false, 'QUEUED'),
        (MessageStatus.sending, false, 'SENDING'),
        (MessageStatus.sending, true, 'RETRYING'),
      ]) {
        await show(tester, StatusMark(status: status, retrying: retrying), brightness: brightness);
        final mark = tester.widget<Text>(find.byType(Text));
        expect((mark.data, mark.style!.color), (word, t.textMeta), reason: '${status.name} retrying=$retrying');
      }
      for (final status in [MessageStatus.sent, MessageStatus.delivered, MessageStatus.read]) {
        await show(tester, StatusMark(status: status), brightness: brightness);
        final mark = tester.widget<Text>(find.byType(Text));
        expect(mark.data, anyOf('•', '••'), reason: '${status.name} is a mark');
        expect(mark.semanticsLabel, statusLabel(status), reason: '${status.name}: the word is the semantics label');
      }
    });

    testWidgets('$theme · resyncing: counts, never a spinner, and no 0 / 0', (tester) async {
      await show(
        tester,
        const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.resyncing, synced: 412, total: 1180)),
        brightness: brightness,
      );
      expect(find.text('Catching up'), findsOneWidget);
      expect(find.text('412 / 1,180'), findsOneWidget);
      final bar = tester.widget<FractionallySizedBox>(find.byKey(const ValueKey('catchup-progress')));
      expect(bar.widthFactor, closeTo(412 / 1180, 1e-9));
      expect(find.byType(CircularProgressIndicator), findsNothing);

      await show(tester, const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.resyncing)), brightness: brightness);
      expect(find.textContaining('/'), findsNothing, reason: 'no total yet, so no number is claimed');
    });
  }

  testWidgets('reconnecting counts: the attempt and the countdown, with RETRY', (tester) async {
    var retried = 0;
    await show(
      tester,
      ConnectionBanner(
        connection: const ConnectionView(kind: ConnectionKind.reconnecting, attempt: 3, retryIn: Duration(seconds: 8)),
        onRetry: () => retried++,
      ),
    );
    expect(find.text('Reconnecting'), findsOneWidget);
    expect(find.text('attempt 3 · 0:08'), findsOneWidget);
    await tester.tap(find.text('RETRY'));
    expect(retried, 1);
  });

  testWidgets('offline says how many are queued, and never "0 queued"', (tester) async {
    await show(tester, const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.offline, queued: 2)));
    expect(find.text('Offline — 2 queued'), findsOneWidget);
    expect(find.text('RETRY'), findsOneWidget);
    await show(tester, const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.offline)));
    expect(find.text('Offline — messages will queue'), findsOneWidget);
  });

  testWidgets('a terminal client offers no retry; only a credential one offers RE-ENROLL', (tester) async {
    await show(
      tester,
      const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.credential)),
    );
    expect(find.textContaining('Nothing sends until then.'), findsOneWidget);
    expect(find.text('RE-ENROLL'), findsOneWidget);
    expect(find.text('RETRY'), findsNothing);

    await show(
      tester,
      const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.protocol)),
    );
    expect(find.text('PROTOCOL'), findsOneWidget);
    expect(find.text('RE-ENROLL'), findsNothing, reason: 're-enrolling cannot fix a client the server will not speak to');
    expect(find.text('RETRY'), findsNothing);
  });

  testWidgets('a live connection shows no banner, and a journal error shows beside any other', (tester) async {
    await show(tester, const ConnectionBanner(connection: ConnectionView.live()));
    expect(find.byType(Text), findsNothing);
    await show(
      tester,
      const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.offline, journalError: 'SqliteException')),
    );
    expect(find.text('Offline — messages will queue'), findsOneWidget);
    expect(find.text('SqliteException'), findsOneWidget);
  });

  test('the status words, and RETRYING past the third refusal', () {
    expect(statusLabel(MessageStatus.queued), 'QUEUED');
    expect(statusLabel(MessageStatus.sending), 'SENDING');
    expect(statusLabel(MessageStatus.queued, retrying: true), 'RETRYING');
    expect(statusLabel(MessageStatus.sending, retrying: true), 'RETRYING');
    expect(statusLabel(MessageStatus.sent), 'SENT');
    expect(statusLabel(MessageStatus.delivered), 'DELIVERED');
    expect(statusLabel(MessageStatus.failed), 'FAILED');
  });

  // The third reader of server/spec/testdata/read-fraction.json, after the Go
  // harness and web/smoke.ts: held numerator, fresh denominator, clamped.
  test('READ n/m is clamped to the room: the shared read-fraction fixture', () {
    final fixture = jsonDecode(File('../server/spec/testdata/read-fraction.json').readAsStringSync()) as Map<String, Object?>;
    final rows = (fixture['rows']! as List).cast<Map<String, Object?>>();
    expect(rows, hasLength(5));
    for (final row in rows) {
      final held = row['held']! as Map<String, Object?>;
      expect(
        statusLabel(MessageStatus.read, readBy: held['read_by']! as int, memberCount: row['fresh_member_count']! as int),
        'READ ${row['clamped']}',
        reason: row['id']! as String,
      );
    }
    // And the row the clamp exists for would have rendered a number no serve
    // ever said.
    expect(rows.first['unclamped'], '7/6');
    expect(statusLabel(MessageStatus.read, readBy: 7, memberCount: 6), 'READ 6/6');
  });

  test('a two-member room renders bare READ, never a fraction', () {
    expect(statusLabel(MessageStatus.read, readBy: 1, memberCount: 2), 'READ');
    expect(statusLabel(MessageStatus.read, memberCount: 7), 'READ');
  });

  testWidgets('the states screen shows every fixture, in both themes', (tester) async {
    for (final brightness in Brightness.values) {
      tester.view.physicalSize = const Size(820, 6000);
      tester.view.devicePixelRatio = 2;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(brightness),
        home: const TickerMode(enabled: false, child: StatesScreen()),
      ));
      expect(find.byType(ConnectionBanner), findsNWidgets(connectionFixtures.length));
      expect(find.byType(Composer), findsNWidgets(5));
      expect(find.text('Several people'), findsOneWidget);
      expect(find.text('FAILED'), findsNWidgets(2), reason: 'the mark and the word are both FAILED');
      expect(find.text('READ 5/7'), findsOneWidget);
    }
  });
}
