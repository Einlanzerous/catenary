// The typing rule's cases, held to the web client's assertions: the five
// `typing ·` checks in web/smoke.ts, with the same names and the same cast.
// It is a rule and not a string, so it is tested as one — and the widget that
// shows it is then checked to show exactly what the rule says.

import 'dart:io';

import 'package:catenary/main.dart';
import 'package:catenary/store/typing.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/widgets/typing_row.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

const nadia = 'Nadia Okafor', ted = 'Ted Marsh', marek = 'Marek Novak', rosa = 'Rosa Diaz';

void main() {
  test('typing · one is a first name', () => expect(typingLabel([nadia]), 'Nadia'));
  test('typing · two, in the order they started', () {
    expect(typingLabel([nadia, ted]), 'Nadia, Ted');
    expect(typingLabel([ted, nadia]), 'Ted, Nadia', reason: 'the order is the order they started, not a sort');
  });
  test('typing · three still name everyone', () => expect(typingLabel([nadia, ted, marek]), 'Nadia, Ted, Marek'));
  test('typing · four or more drop names', () {
    expect(typingLabel([nadia, ted, marek, rosa]), 'Several people');
    expect(typingLabel([nadia, ted, marek, rosa, 'Ines Vale']), 'Several people');
  });
  test('typing · nobody renders nothing', () => expect(typingLabel([]), isNull));

  test('these are the web client\'s assertions, by name and by expected string', () {
    // The other party to the rule. If the web client's cases move, this fails
    // until the two are reconciled.
    final smoke = File('../web/smoke.ts').readAsStringSync();
    for (final (name, expected) in [
      ('typing · one is a first name', "'Nadia'"),
      ('typing · two, in the order they started', "'Nadia, Ted'"),
      ('typing · three still name everyone', "'Nadia, Ted, Marek'"),
      ('typing · four or more drop names', "'Several people'"),
      ('typing · nobody renders nothing', 'null'),
    ]) {
      final at = smoke.indexOf("check('$name'");
      expect(at, isNonNegative, reason: 'web/smoke.ts no longer has the check "$name"');
      expect(smoke.substring(at, smoke.indexOf(')\n', at)), contains('=== $expected'), reason: name);
    }
  });

  testWidgets('the typing row shows what the rule says, and nothing for nobody', (tester) async {
    Future<void> show(List<String> names) => tester.pumpWidget(MaterialApp(
          theme: catenaryTheme(Brightness.dark),
          home: TickerMode(enabled: false, child: Scaffold(body: TypingRow(names: names))),
        ));
    await show([nadia, ted]);
    expect(find.text('Nadia, Ted'), findsOneWidget);
    await show([nadia, ted, marek, rosa]);
    expect(find.text('Several people'), findsOneWidget);
    await show([]);
    expect(find.byType(Text), findsNothing);
    // Referenced so the app's own entrypoint stays in this test's build.
    expect(CatenaryApp, isNotNull);
  });
}
