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

Color primaryFill(WidgetTester tester) =>
    tester.widget<Container>(find.byKey(const ValueKey('composer-primary'))).color!;

void main() {
  for (final (brightness, t) in [(Brightness.dark, CatenaryTokens.dark), (Brightness.light, CatenaryTokens.light)]) {
    final theme = brightness.name;

    testWidgets('$theme · recording: the clock, CANCEL and SEND, amber and not red', (tester) async {
      var cancelled = 0, sent = 0;
      await show(
        tester,
        Composer(
          conversationName: 'Kitchen',
          connection: const ConnectionView.live(),
          recording: const Duration(seconds: 74),
          onCancelRecording: () => cancelled++,
          onSendRecording: () => sent++,
        ),
        brightness: brightness,
      );
      expect(find.text('1:14'), findsOneWidget);
      final box = tester.widget<Container>(find.byKey(const ValueKey('composer-recording')));
      expect((box.decoration! as BoxDecoration).border!.top.color, t.accentWire, reason: 'amber: red belongs to failure alone');
      expect(find.byKey(const ValueKey('composer-input')), findsNothing);
      await tester.tap(find.text('CANCEL'));
      await tester.tap(find.text('SEND'));
      expect((cancelled, sent), (1, 1));
    });

    testWidgets('$theme · offline-queued: QUEUE in the dimmer copper, the placeholder says so, ATTACH is off', (tester) async {
      var sent = 0, attached = 0, recorded = 0;
      await show(
        tester,
        Composer(
          conversationName: 'Kitchen',
          connection: const ConnectionView(kind: ConnectionKind.offline),
          onSend: () => sent++,
          onAttach: () => attached++,
          onRecord: () => recorded++,
        ),
        brightness: brightness,
      );
      expect(find.text('QUEUE'), findsOneWidget);
      expect(find.text('SEND'), findsNothing);
      expect(primaryFill(tester), t.accentQueue);
      expect(find.text('Message Kitchen — will send when reconnected'), findsOneWidget);
      await tester.tap(find.text('QUEUE'));
      await tester.tap(find.text('ATTACH'));
      await tester.tap(find.text('RECORD'));
      expect((sent, attached, recorded), (1, 0, 1), reason: 'an upload cannot be queued; a text and a recording can');
      expect(tester.widget<Text>(find.text('ATTACH')).style!.color, t.textDisabled);
    });

    testWidgets('$theme · live: SEND on the one accent', (tester) async {
      await show(tester, const Composer(conversationName: 'Kitchen', connection: ConnectionView.live()), brightness: brightness);
      expect(find.text('SEND'), findsOneWidget);
      expect(primaryFill(tester), t.accentWire);
      expect(find.text('Message Kitchen'), findsOneWidget);
    });

    testWidgets('$theme · terminal: nothing sends, and the accent is not on the button', (tester) async {
      var sent = 0, recorded = 0;
      await show(
        tester,
        Composer(
          conversationName: 'Kitchen',
          connection: const ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.credential),
          onSend: () => sent++,
          onRecord: () => recorded++,
        ),
        brightness: brightness,
      );
      expect(find.text('QUEUE'), findsNothing, reason: 'a terminal client drains nothing, so it does not offer to queue');
      expect(primaryFill(tester), t.surfaceBase);
      await tester.tap(find.text('SEND'));
      await tester.tap(find.text('RECORD'));
      expect((sent, recorded), (0, 0));
      expect(find.text('Message Kitchen — this device cannot send; see the banner'), findsOneWidget);
    });

    testWidgets('$theme · failed: the one other color, and only there', (tester) async {
      await show(tester, const StatusLabel(status: MessageStatus.failed), brightness: brightness);
      expect(tester.widget<Text>(find.text('FAILED')).style!.color, t.signalFault);
      for (final s in MessageStatus.values.where((s) => s != MessageStatus.failed)) {
        await show(tester, StatusLabel(status: s), brightness: brightness);
        expect(tester.widget<Text>(find.byType(Text)).style!.color, t.textMeta, reason: '${s.name} is not a failure');
      }
    });

    testWidgets('$theme · resyncing: counts, never a spinner, and no 0 / 0', (tester) async {
      await show(
        tester,
        const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.resyncing, synced: 1284, total: 12480, roomsPending: 2)),
        brightness: brightness,
      );
      expect(find.text('Reconnected — catching up'), findsOneWidget);
      expect(find.text('1,284 / 12,480 messages'), findsOneWidget);
      expect(find.text('2 ROOMS PENDING'), findsOneWidget);
      final bar = tester.widget<FractionallySizedBox>(find.byKey(const ValueKey('catchup-progress')));
      expect(bar.widthFactor, closeTo(1284 / 12480, 1e-9));
      expect(find.byType(CircularProgressIndicator), findsNothing);

      await show(tester, const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.resyncing)), brightness: brightness);
      expect(find.textContaining('messages'), findsNothing, reason: 'no total yet, so no number is claimed');
    });
  }

  testWidgets('reconnecting counts: the attempt and the countdown, with RETRY NOW', (tester) async {
    var retried = 0;
    await show(
      tester,
      ConnectionBanner(
        connection: const ConnectionView(kind: ConnectionKind.reconnecting, attempt: 3, retryIn: Duration(seconds: 8)),
        onRetry: () => retried++,
      ),
    );
    expect(find.text('Connection lost — reconnecting'), findsOneWidget);
    expect(find.text('attempt 3 · retry in 0:08'), findsOneWidget);
    await tester.tap(find.text('RETRY NOW'));
    expect(retried, 1);
  });

  testWidgets('offline says messages will queue', (tester) async {
    await show(tester, const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.offline)));
    expect(find.text('Offline — messages will queue'), findsOneWidget);
    expect(find.text('RECONNECT'), findsOneWidget);
  });

  testWidgets('a terminal client offers no retry; only a credential one offers RE-ENROLL', (tester) async {
    await show(
      tester,
      const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.credential)),
    );
    expect(find.textContaining('Nothing sends until then.'), findsOneWidget);
    expect(find.text('RE-ENROLL'), findsOneWidget);
    expect(find.text('RETRY NOW'), findsNothing);

    await show(
      tester,
      const ConnectionBanner(connection: ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.protocol)),
    );
    expect(find.text('PROTOCOL'), findsOneWidget);
    expect(find.text('RE-ENROLL'), findsNothing, reason: 're-enrolling cannot fix a client the server will not speak to');
    expect(find.text('RETRY NOW'), findsNothing);
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
      expect(find.byType(Composer), findsNWidgets(4));
      expect(find.text('Several people'), findsOneWidget);
      expect(find.text('FAILED'), findsOneWidget);
      expect(find.text('READ 5/7'), findsOneWidget);
    }
  });
}
