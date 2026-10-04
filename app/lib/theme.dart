// The Material theme, derived from the tokens and from nothing else.
//
// Material is the widget toolkit here, not the design: every color it would
// pick for itself is pointed at a token, every shape it would round is
// squared, and the ripple it would draw is turned off. Radius is 0 except 2px
// on inputs. There are no bubbles and no elevation; structure is a hairline.

import 'package:flutter/material.dart';

import 'metrics.dart';
import 'tokens.dart';

ThemeData catenaryTheme(Brightness brightness) {
  final t = brightness == Brightness.dark ? CatenaryTokens.dark : CatenaryTokens.light;
  const square = RoundedRectangleBorder();
  final edge = OutlineInputBorder(
    borderRadius: BorderRadius.circular(CatenaryMetrics.radiusInput),
    borderSide: BorderSide(color: t.lineEdge),
  );
  return ThemeData(
    useMaterial3: true,
    brightness: brightness,
    extensions: [t],
    fontFamily: fontSans,
    scaffoldBackgroundColor: t.surfaceBase,
    canvasColor: t.surfaceBase,
    dividerColor: t.lineHair,
    splashFactory: NoSplash.splashFactory,
    highlightColor: Colors.transparent,
    hoverColor: t.surfaceRaised,
    colorScheme: ColorScheme(
      brightness: brightness,
      primary: t.accentWire,
      onPrimary: t.onAccent,
      secondary: t.accentWire,
      onSecondary: t.onAccent,
      error: t.signalFault,
      onError: t.onAccent,
      surface: t.surfaceBase,
      onSurface: t.textPrimary,
      outline: t.lineEdge,
      outlineVariant: t.lineHair,
      // No tint: a lifted surface is a token, never a blend Material computes.
      surfaceTint: Colors.transparent,
    ),
    textTheme: TextTheme(
      displaySmall: CatenaryType.display.style.copyWith(color: t.textPrimary),
      titleMedium: CatenaryType.title.style.copyWith(color: t.textPrimary),
      bodyMedium: CatenaryType.body.style.copyWith(color: t.textPrimary),
      bodySmall: CatenaryType.secondary.style.copyWith(color: t.textSecondary),
      labelSmall: CatenaryType.label.tracked.copyWith(color: t.textMeta),
    ),
    appBarTheme: AppBarTheme(
      backgroundColor: t.surfaceRail,
      foregroundColor: t.textPrimary,
      elevation: 0,
      scrolledUnderElevation: 0,
      shape: Border(bottom: BorderSide(color: t.lineHair)),
    ),
    cardTheme: CardThemeData(color: t.surfaceLift, elevation: 0, margin: EdgeInsets.zero, shape: square),
    dialogTheme: DialogThemeData(backgroundColor: t.surfaceRail, elevation: 0, shape: square),
    dividerTheme: DividerThemeData(color: t.lineHair, thickness: 1, space: 1),
    textButtonTheme: TextButtonThemeData(
      style: TextButton.styleFrom(foregroundColor: t.accentWire, shape: square),
    ),
    filledButtonTheme: FilledButtonThemeData(
      style: FilledButton.styleFrom(backgroundColor: t.accentWire, foregroundColor: t.onAccent, shape: square),
    ),
    inputDecorationTheme: InputDecorationTheme(
      isDense: true,
      contentPadding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s3, vertical: CatenaryMetrics.s3),
      hintStyle: CatenaryType.body.style.copyWith(color: t.textPlaceholder),
      border: edge,
      enabledBorder: edge,
      focusedBorder: edge.copyWith(borderSide: BorderSide(color: t.accentWire)),
    ),
    textSelectionTheme: TextSelectionThemeData(
      cursorColor: t.accentWire,
      selectionColor: t.accentWash,
      selectionHandleColor: t.accentWire,
    ),
  );
}
