// The tokens that are not colors: type, space, layout and the one radius.
// The same table as tokens.dart — web/src/styles/tokens.css — and held to it
// by the same test. These do not change with the theme.

import 'package:flutter/widgets.dart';

const String fontSans = 'IBM Plex Sans';
const String fontMono = 'IBM Plex Mono';

/// `--track-label` and `--track-tight`, in em.
const double trackLabel = 0.14;
const double trackTight = -0.02;

/// One `--type-*` token: CSS's `weight size/line-height family`.
@immutable
class TypeToken {
  const TypeToken(this.weight, this.size, this.lineHeight, this.family);

  final int weight;
  final double size;
  final double lineHeight;
  final String family;

  /// The style, colorless: the color is a token too and the caller names it.
  TextStyle get style => TextStyle(
        fontFamily: family,
        fontWeight: FontWeight.values[weight ~/ 100 - 1],
        fontSize: size,
        height: lineHeight / size,
        // The app is a reading surface: line boxes are the token's, not the
        // font's ascent and descent, as they are in CSS.
        leadingDistribution: TextLeadingDistribution.even,
      );

  /// The style with `--track-label`, for the mono caps labels.
  TextStyle get tracked => style.copyWith(letterSpacing: size * trackLabel);
}

/// The type scale, under the names tokens.css gives it.
abstract final class CatenaryType {
  static const display = TypeToken(500, 28, 34, fontSans);
  static const title = TypeToken(500, 18, 24, fontSans);
  static const body = TypeToken(400, 15, 25, fontSans);
  static const sender = TypeToken(600, 13.5, 18, fontSans);
  static const secondary = TypeToken(400, 13, 20, fontSans);
  static const meta = TypeToken(400, 11, 14, fontMono);
  static const label = TypeToken(400, 10, 12, fontMono);

  static const Map<String, TypeToken> byName = {
    'type-display': display,
    'type-title': title,
    'type-body': body,
    'type-sender': sender,
    'type-secondary': secondary,
    'type-meta': meta,
    'type-label': label,
  };
}

/// Space, on a 4pt base, and the layout constants.
abstract final class CatenaryMetrics {
  static const double s1 = 4;
  static const double s2 = 8;
  static const double s3 = 12;
  static const double s4 = 16;
  static const double s6 = 24;
  static const double s8 = 32;
  static const double s12 = 48;

  static const double railW = 304;
  static const double gutterW = 72;
  static const double statusW = 96;
  static const double rowY = 3;
  static const double groupGap = 16;

  /// Radius is 0 everywhere except inputs.
  static const double radiusInput = 2;

  /// Below this the rail becomes its own pane.
  static const double narrow = 900;

  /// Every length above under its tokens.css name, in logical pixels.
  /// `--measure` is `74ch` and has no pixel value; a text column measures
  /// itself.
  static const Map<String, double> byName = {
    's1': s1,
    's2': s2,
    's3': s3,
    's4': s4,
    's6': s6,
    's8': s8,
    's12': s12,
    'rail-w': railW,
    'gutter-w': gutterW,
    'status-w': statusW,
    'row-y': rowY,
    'group-gap': groupGap,
    'radius-input': radiusInput,
    'narrow': narrow,
  };
}
