// Both themes render: the app is pumped in each, and what is on screen is
// painted from that theme's table and from nothing else.

import 'package:catenary/main.dart';
import 'package:catenary/metrics.dart';
import 'package:catenary/tokens.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  for (final (mode, tokens, label) in [
    (ThemeMode.dark, CatenaryTokens.dark, 'TOKENS · DARK'),
    (ThemeMode.light, CatenaryTokens.light, 'TOKENS · LIGHT'),
  ]) {
    testWidgets('$label renders from its own table', (tester) async {
      await tester.pumpWidget(CatenaryApp(initialMode: mode));
      expect(find.text(label), findsOneWidget);

      final context = tester.element(find.byType(Scaffold));
      expect(CatenaryTokens.of(context), same(tokens));
      expect(Theme.of(context).scaffoldBackgroundColor, tokens.surfaceBase);

      // The first swatch on screen is `surface-base`, painted in this theme's
      // value, and its row is ruled with this theme's hairline.
      final row = tester.widget<Container>(find.byKey(const ValueKey('token-surface-base')));
      expect((row.decoration! as BoxDecoration).border!.bottom.color, tokens.lineFaint);

      // IBM Plex, and no other family, in the title.
      final title = tester.widget<Text>(find.text('Catenary'));
      expect(title.style!.fontFamily, fontSans);
      expect(title.style!.color, tokens.textPrimary);
    });
  }

  testWidgets('the toggle swaps one table for the other', (tester) async {
    await tester.pumpWidget(const CatenaryApp(initialMode: ThemeMode.dark));
    await tester.tap(find.byKey(const ValueKey('theme-toggle')));
    await tester.pumpAndSettle();
    expect(find.text('TOKENS · LIGHT'), findsOneWidget);
    expect(CatenaryTokens.of(tester.element(find.byType(Scaffold))), same(CatenaryTokens.light));
  });

  testWidgets('radius is 0 except 2px on inputs', (tester) async {
    await tester.pumpWidget(const CatenaryApp(initialMode: ThemeMode.dark));
    final theme = Theme.of(tester.element(find.byType(Scaffold)));
    final input = theme.inputDecorationTheme.border! as OutlineInputBorder;
    expect(input.borderRadius, BorderRadius.circular(2));
    expect(theme.cardTheme.shape, const RoundedRectangleBorder());
    expect(theme.dialogTheme.shape, const RoundedRectangleBorder());
  });
}
