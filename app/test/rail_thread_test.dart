// CANT-43 — the rail and the thread against `Catenary Mobile.dc.html`, frames
// 01 and 02, in both themes; and the one thing derived rather than copied:
// every stamp is relative to today, never pinned to a fixture's date.
//
// The canvas's moment is Sunday 16 August at 14:16. The fixtures are built
// from `now`, so at that moment the rail reads as the canvas does — and a
// week later it reads differently, row by row, which is the point.

import 'package:catenary/fixtures.dart';
import 'package:catenary/main.dart';
import 'package:catenary/rail.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
import 'package:catenary/store/status.dart';
import 'package:catenary/store/when.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary/tokens.dart';
import 'package:catenary/widgets/status_mark.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

/// The canvas's moment.
final canvasNow = DateTime(2026, 8, 16, 14, 16);

/// A phone: the canvas's 390 × 844. [height] is raised where a test reads a
/// whole thread at once: the canvas draws a photo as a 100px placeholder, and
/// the app gives it its real 3:2 (call M10), so the frame's content is taller
/// here than its 844.
void phone(WidgetTester tester, {double height = 844}) {
  tester.view.physicalSize = Size(390, height);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
}

Future<void> show(WidgetTester tester, Widget screen, Brightness brightness, {double height = 844}) {
  phone(tester, height: height);
  return tester.pumpWidget(MaterialApp(
    theme: catenaryTheme(brightness),
    home: TickerMode(enabled: false, child: screen),
  ));
}

Widget rail(DateTime now, {DateTime? builtAt, String? openId = 'kitchen', ConnectionView connection = const ConnectionView.live()}) =>
    RailScreen(
      conversations: fixtureConversations(builtAt ?? now),
      now: now,
      connection: connection,
      myInitials: 'HB',
      openId: openId,
    );

/// The stamp a rail row shows.
String stampOf(WidgetTester tester, String id) => tester
    .widget<Text>(find.descendant(of: find.byKey(ValueKey('rail-$id')), matching: find.byKey(const ValueKey('rail-stamp'))))
    .data!;

Text nameOf(WidgetTester tester, String id, String name) =>
    tester.widget<Text>(find.descendant(of: find.byKey(ValueKey('rail-$id')), matching: find.text(name)));

void main() {
  group('a stamp is relative to today', () {
    final now = DateTime(2026, 8, 16, 14, 16);
    test('today is a clock', () => expect(railStamp(DateTime(2026, 8, 16, 9, 41), now), '09:41'));
    test('this week is a weekday', () {
      expect(railStamp(DateTime(2026, 8, 15, 23, 50), now), 'SAT');
      expect(railStamp(DateTime(2026, 8, 10, 12), now), 'MON', reason: 'six days ago is still a weekday');
    });
    test('a week or more is a date', () {
      expect(railStamp(DateTime(2026, 8, 9, 12), now), '9 AUG', reason: 'seven days ago: the same weekday as today would be ambiguous');
      expect(railStamp(DateTime(2026, 3, 2, 8), now), '2 MAR');
    });
    test('it is calendar days, not 24-hour spans', () {
      // Ten minutes before midnight is a weekday ten minutes after it.
      expect(railStamp(DateTime(2026, 8, 15, 23, 50), DateTime(2026, 8, 16, 0, 10)), 'SAT');
      expect(railStamp(DateTime(2026, 8, 16, 0, 5), DateTime(2026, 8, 16, 23, 55)), '00:05');
    });
    test('a day with a clock change is still one day', () {
      // Across a spring-forward and a fall-back, wherever this runs: seven
      // calendar days is a date and six is a weekday.
      for (final end in [DateTime(2026, 3, 9, 12), DateTime(2026, 3, 30, 12), DateTime(2026, 11, 2, 12), DateTime(2026, 10, 26, 12)]) {
        expect(calendarDaysBetween(end.subtract(const Duration(days: 7, hours: -1)), end), anyOf(6, 7));
        expect(calendarDaysBetween(DateTime(end.year, end.month, end.day - 7, 12), end), 7);
        expect(calendarDaysBetween(DateTime(end.year, end.month, end.day - 6, 12), end), 6);
      }
    });
  });

  for (final (brightness, t) in [(Brightness.dark, CatenaryTokens.dark), (Brightness.light, CatenaryTokens.light)]) {
    final theme = brightness.name;

    testWidgets('$theme · the rail at the canvas\'s moment reads as frame 01', (tester) async {
      await show(tester, rail(canvasNow), brightness);
      expect(tester.takeException(), isNull, reason: 'nothing overflows at 390');

      expect(find.text('CATENARY'), findsOneWidget);
      expect(find.text('ROOMS'), findsOneWidget);
      expect(find.text('DIRECT'), findsOneWidget);
      // The stamps: clocks today, weekdays this week.
      expect(
        {for (final id in ['coop', 'dinner', 'shed', 'ilse', 'marek', 'ted', 'nadia']) id: stampOf(tester, id)},
        {'coop': '13:58', 'dinner': '11:20', 'shed': 'MON', 'ilse': '14:04', 'marek': '12:47', 'ted': 'TUE', 'nadia': '09:41'},
      );
      // The previews, each said the way its kind says it.
      for (final line in [
        'Ted: the delivery window moved to Thursday afternoon, I will confirm with the depot',
        'You: bringing the big pot',
        'Marek: photo',
        'transcript pending · 1:12',
        'You: sent it to your inbox instead',
        'You: voice note · 0:22',
        'no rush on any of it, truly',
      ]) {
        expect(find.text(line), findsOneWidget, reason: line);
      }
      // The trailing markers.
      expect(find.text('12'), findsOneWidget);
      expect(find.text('MUTED'), findsOneWidget);
      expect(find.text('FAILED'), findsOneWidget);
      final marks = tester.widgetList<StatusMark>(find.byType(StatusMark)).map((m) => m.status).toList();
      expect(marks, [MessageStatus.read, MessageStatus.sent, MessageStatus.failed], reason: 'Sunday Dinner, Marek, Ted');

      // The accent is spent on what is live: the open row's bar, an unread
      // row's stamp and count — and on nothing else in a row.
      final open = tester.widget<Container>(
        find.descendant(of: find.byKey(const ValueKey('rail-kitchen')), matching: find.byType(Container)).first,
      );
      expect(open.color, t.surfaceRaised);
      final unreadStamp = tester.widget<Text>(
        find.descendant(of: find.byKey(const ValueKey('rail-coop')), matching: find.byKey(const ValueKey('rail-stamp'))),
      );
      expect(unreadStamp.style!.color, t.accentWire);
      final readStamp = tester.widget<Text>(
        find.descendant(of: find.byKey(const ValueKey('rail-dinner')), matching: find.byKey(const ValueKey('rail-stamp'))),
      );
      expect(readStamp.style!.color, t.textMeta);

      // Quiet rows dim their name; nothing is bold-for-unread.
      expect(nameOf(tester, 'shed', 'Shed Projects').style!.color, t.textSecondary);
      expect(nameOf(tester, 'ted', 'Ted Almasy').style!.color, t.textSecondary);
      expect(nameOf(tester, 'nadia', 'Nadia Okonkwo').style!.color, t.textSecondary);
      for (final (id, name) in [('kitchen', 'Kitchen Table'), ('coop', 'Bergen Hill Co-op'), ('dinner', 'Sunday Dinner'), ('marek', 'Marek Dubois')]) {
        final text = nameOf(tester, id, name);
        expect((text.style!.color, text.style!.fontWeight), (t.textPrimary, FontWeight.w600), reason: name);
      }

      // Three tabs; SEARCH is drawn and inert.
      expect(tester.widget<Text>(find.text('CHATS')).style!.color, t.accentWire);
      expect(tester.widget<Text>(find.text('SEARCH')).style!.color, t.textDisabled);
    });

    testWidgets('$theme · the thread at the canvas\'s moment reads as frame 02', (tester) async {
      final kitchen = fixtureConversations(canvasNow).first;
      await show(tester, ThreadScreen(conversation: kitchen, connection: const ConnectionView.live()), brightness, height: 1200);
      expect(tester.takeException(), isNull, reason: 'nothing overflows at 390');

      expect(find.text('Kitchen Table'), findsOneWidget);
      // TLS, and never E2E: the server can read these.
      expect(tester.widget<Text>(find.byKey(const ValueKey('thread-subtitle'))).data, '7 MEMBERS · TLS');
      expect(find.textContaining('E2E'), findsNothing);
      expect(find.textContaining('ENCRYPTED'), findsNothing);

      expect(find.text('16 AUG'), findsOneWidget);
      expect(find.text('3 NEW'), findsOneWidget);
      // M1: the time is on group headers only. Six messages, four groups.
      for (final time in ['13:41', '13:52', '14:12', '14:15']) {
        expect(find.text(time), findsOneWidget, reason: time);
      }
      expect(find.byType(MessageGroup), findsNWidgets(4));

      // No bubbles: an own group is a one-step surface lift and an accent
      // "You"; anybody else's is the canvas itself.
      final groups = tester.widgetList<MessageGroup>(find.byType(MessageGroup)).toList();
      for (final g in groups) {
        final box = tester.widget<Container>(find.byKey(ValueKey('group-${g.group.first.id}')));
        final decoration = box.decoration! as BoxDecoration;
        expect(decoration.color, g.group.first.mine ? t.surfaceLift : null);
        expect(decoration.borderRadius, isNull);
        expect(decoration.boxShadow, isNull);
      }
      final you = tester.widgetList<Text>(find.text('You')).toList();
      expect(you, hasLength(2));
      expect(you.every((w) => w.style!.color == t.accentWire), isTrue);
      expect(find.text('Hollis Brandt'), findsNothing);

      // M2: status is a mark on your own group's header — read in the accent,
      // delivered in meta grey.
      final marks = tester.widgetList<StatusMark>(find.byKey(const ValueKey('group-status'))).map((m) => m.status).toSet();
      expect(marks, {MessageStatus.read, MessageStatus.delivered});

      // M3: body text stays 15px.
      final body = tester.widget<Text>(find.text('I can be there at eight to sign for it.'));
      expect((body.style!.fontSize, body.style!.height), (15, 24 / 15));

      // The transcript's count is the count of the words on screen.
      final transcript = kitchen.messages[2].voice!.transcript!;
      expect(find.text('EXPAND · ${countWords(transcript)} W'), findsOneWidget);
      expect(tester.widget<Text>(find.text(transcript)).maxLines, 2);
      await tester.tap(find.byKey(const ValueKey('transcript-toggle')));
      await tester.pump();
      expect(find.text('COLLAPSE'), findsOneWidget);
      expect(tester.widget<Text>(find.text(transcript)).maxLines, isNull);

      expect(find.text('IMG_4471.HEIC'), findsOneWidget);
      expect(find.text('3024×2016'), findsOneWidget);
      // Typing is the thread's last row, a first name.
      expect(find.text('Nadia'), findsOneWidget);
      expect(find.text('REC'), findsOneWidget);
    });
  }

  testWidgets('timestamps are relative to today, not pinned to the fixtures\' dates', (tester) async {
    // The same conversations, read two days later and then nine days later.
    // Two days on is Tuesday the 18th: Sunday is a weekday, and the Monday and
    // Tuesday before it are eight and seven days back.
    // A list of pinned strings would show the canvas's clocks forever.
    await show(tester, rail(canvasNow.add(const Duration(days: 2)), builtAt: canvasNow), Brightness.dark);
    expect(
      {for (final id in ['coop', 'dinner', 'shed', 'ted', 'nadia']) id: stampOf(tester, id)},
      {'coop': 'SUN', 'dinner': 'SUN', 'shed': '10 AUG', 'ted': '11 AUG', 'nadia': 'SUN'},
      reason: 'today\'s clocks are now a weekday, and last week\'s weekdays are now dates',
    );
    await show(tester, rail(canvasNow.add(const Duration(days: 9)), builtAt: canvasNow), Brightness.dark);
    expect(
      {for (final id in ['coop', 'dinner', 'shed', 'ted', 'nadia']) id: stampOf(tester, id)},
      {'coop': '16 AUG', 'dinner': '16 AUG', 'shed': '10 AUG', 'ted': '11 AUG', 'nadia': '16 AUG'},
    );
    expect(find.textContaining(RegExp(r'^\d\d:\d\d$')), findsNothing, reason: 'nothing here is from today any more');

    // And the fixtures themselves are not pinned: built on another day, the
    // rail reads the same way as on the canvas's.
    final later = DateTime(2027, 2, 3, 14, 16);
    await show(tester, rail(later), Brightness.dark);
    expect(stampOf(tester, 'coop'), '13:58');
    expect(stampOf(tester, 'shed'), 'MON');
    expect(stampOf(tester, 'ted'), 'TUE');
  });

  test('the badge and the "N NEW" rule are one number, and neither counts your own messages', () {
    final kitchen = fixtureConversations(canvasNow).first;
    expect(kitchen.firstUnreadSeq, 3);
    final atOrPast = kitchen.messages.where((m) => m.seq >= 3).toList();
    expect(atOrPast, hasLength(4), reason: 'four messages at or past the marker…');
    expect(atOrPast.where((m) => m.mine), hasLength(1), reason: '…one of them yours');
    expect(kitchen.newCount, 3);
    expect(railMark(kitchen).count, 3);
    expect(threadRows(kitchen).whereType<NewRow>().single.count, 3);

    // Nothing unread: no rule, no badge.
    final dinner = fixtureConversations(canvasNow)[2];
    expect(dinner.newCount, 0);
    expect(threadRows(dinner).whereType<NewRow>(), isEmpty);
    expect(railMark(dinner).kind, RailMarkKind.status);
  });

  test('the "N NEW" rule sits above the first message that is new to you, and splits a group there', () {
    final rows = threadRows(fixtureConversations(canvasNow).first);
    expect(rows.map((r) => r.runtimeType).toList(), [DayRow, GroupRow, GroupRow, NewRow, GroupRow, GroupRow]);
    expect((rows[4] as GroupRow).messages, hasLength(3), reason: 'Ilse\'s three, under one header');
  });

  test('a preview is said the way its conversation\'s kind says it', () {
    final all = {for (final c in fixtureConversations(canvasNow)) c.id: preview(c)};
    expect(all['coop'], startsWith('Ted: '), reason: 'a room names who');
    expect(all['nadia'], 'no rush on any of it, truly', reason: 'a direct needs no name: the row is already the person');
    expect(all['shed'], 'Marek: photo');
    expect(all['ilse'], 'transcript pending · 1:12');
    expect(all['ted'], 'You: voice note · 0:22');
  });

  testWidgets('the app opens on the rail; a row opens its thread, and back returns', (tester) async {
    phone(tester);
    await tester.pumpWidget(TickerMode(enabled: false, child: CatenaryApp(initialMode: ThemeMode.dark, clock: () => canvasNow)));
    expect(find.byType(RailScreen), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('rail-coop')));
    await tester.pumpAndSettle();
    expect(find.byType(ThreadScreen), findsOneWidget);
    expect(find.text('31 MEMBERS · TLS'), findsOneWidget);
    expect(find.text('12 NEW'), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('thread-back')));
    await tester.pumpAndSettle();
    expect(find.byType(ThreadScreen), findsNothing);
    // The row just opened carries the bar and no marker.
    final row = tester.widget<ConversationRow>(find.byKey(const ValueKey('rail-coop')));
    expect(row.open, isTrue);
  });

  testWidgets('the rail shows the connection banner as frame 01 does', (tester) async {
    await show(
      tester,
      rail(canvasNow, connection: const ConnectionView(kind: ConnectionKind.reconnecting, attempt: 3, retryIn: Duration(seconds: 8))),
      Brightness.dark,
    );
    expect(find.text('Reconnecting'), findsOneWidget);
    expect(find.text('attempt 3 · 0:08'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });
}
