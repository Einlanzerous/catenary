// Renders the app to PNGs, one per theme, for a pull request to show: the
// estate's rule is that a change to something visible carries a picture.
// Not part of `flutter test` (it is outside test/); run it by name:
//
//     flutter test tool/render_test.dart --update-goldens
//
// and the images land in build/render/. It loads the bundled IBM Plex files,
// because a widget test otherwise paints every glyph as a box.

import 'dart:io';

import 'package:catenary/main.dart';
import 'package:catenary/metrics.dart';
import 'package:catenary/states.dart';
import 'package:catenary/theme.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

Future<void> loadFonts() async {
  for (final (family, files) in [
    (fontSans, ['IBMPlexSans-Regular', 'IBMPlexSans-Medium', 'IBMPlexSans-SemiBold']),
    (fontMono, ['IBMPlexMono-Regular', 'IBMPlexMono-Medium', 'IBMPlexMono-SemiBold']),
  ]) {
    final loader = FontLoader(family);
    for (final f in files) {
      final bytes = File('assets/fonts/$f.ttf').readAsBytesSync();
      loader.addFont(Future.value(ByteData.sublistView(bytes)));
    }
    await loader.load();
  }
}

void main() {
  for (final (mode, name) in [(ThemeMode.dark, 'dark'), (ThemeMode.light, 'light')]) {
    testWidgets('render the specimen, $name', (tester) async {
      await loadFonts();
      // A Pixel 9 Pro's logical size, at 2x so the text is legible in a PR.
      tester.view.physicalSize = const Size(820, 1830);
      tester.view.devicePixelRatio = 2;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(CatenaryApp(initialMode: mode));
      await tester.pumpAndSettle();
      await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/specimen-$name.png'));
    });

    // Every connection, status, typing and composer state (CANT-44), with the
    // tickers muted: a pulse caught mid-fade is not what the state looks like.
    testWidgets('render the states, $name', (tester) async {
      await loadFonts();
      tester.view.physicalSize = const Size(820, 2560);
      tester.view.devicePixelRatio = 2;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        debugShowCheckedModeBanner: false,
        theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
        home: const TickerMode(enabled: false, child: StatesScreen()),
      ));
      await tester.pumpAndSettle();
      await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/states-$name.png'));
    });
  }
}
