// Catenary's design tokens, in a second runtime.
//
// "Names are the contract, values are per-theme." The table is
// web/src/styles/tokens.css, the web client's, and this file is that table
// lifted into Dart: every themed token there exists here under the same name,
// with the same value in each theme, and nothing here exists that is not
// there. test/tokens_test.dart reads tokens.css and holds this file to it, so
// a token added, renamed or re-valued on one side fails the build on the
// other.
//
// Dark is primary; light is derived and changes nothing but the values — no
// token exists in one theme only. One copper accent, `accent-wire`, carries
// unread, active, playing and read, and nothing else competes for it. A raw
// Color in a widget is a bug: a widget reads `CatenaryTokens.of(context)`.

import 'package:flutter/widgets.dart';
import 'package:flutter/material.dart' show Theme, ThemeExtension;

@immutable
class CatenaryTokens extends ThemeExtension<CatenaryTokens> {
  const CatenaryTokens({
    required this.surfaceBase,
    required this.surfaceRail,
    required this.surfaceLift,
    required this.surfaceRaised,
    required this.surfaceAvatar,
    required this.lineHair,
    required this.lineEdge,
    required this.lineInner,
    required this.lineFaint,
    required this.lineAccentDim,
    required this.textPrimary,
    required this.textBright,
    required this.textSecondary,
    required this.textMeta,
    required this.textDim,
    required this.textFaint,
    required this.textDisabled,
    required this.textPlaceholder,
    required this.textQuote,
    required this.accentWire,
    required this.accentWash,
    required this.accentWashSoft,
    required this.accentRule,
    required this.accentUnderline,
    required this.accentDim,
    required this.accentQueue,
    required this.onAccent,
    required this.accentMark,
    required this.signalFault,
    required this.waveRest,
    required this.waveRestDim,
  });

  /// `--surface-base` — app canvas
  final Color surfaceBase;
  /// `--surface-rail` — sidebar, headers
  final Color surfaceRail;
  /// `--surface-lift` — one-step lift: own message, attachment body
  final Color surfaceLift;
  /// `--surface-raised` — hover, active row, skeleton fill
  final Color surfaceRaised;
  /// `--surface-avatar` — avatar tile for other people
  final Color surfaceAvatar;
  /// `--line-hair` — 1px structural separators
  final Color lineHair;
  /// `--line-edge` — input and attachment borders
  final Color lineEdge;
  /// `--line-inner` — separators inside an attachment block
  final Color lineInner;
  /// `--line-faint` — list-row separators
  final Color lineFaint;
  /// `--line-accent-dim` — border on an accented chip
  final Color lineAccentDim;
  /// `--text-primary` — message body, names
  final Color textPrimary;
  /// `--text-bright` — avatar glyph, unread preview
  final Color textBright;
  /// `--text-secondary` — previews, transcript
  final Color textSecondary;
  /// `--text-meta` — timestamps, status labels
  final Color textMeta;
  /// `--text-dim` — gutter time, keycaps
  final Color textDim;
  /// `--text-faint` — continuation-row time
  final Color textFaint;
  /// `--text-disabled` — action unavailable
  final Color textDisabled;
  /// `--text-placeholder` — placeholder captions
  final Color textPlaceholder;
  /// `--text-quote` — reply-stub sender
  final Color textQuote;
  /// `--accent-wire` — unread, active, playing, read
  final Color accentWire;
  /// `--accent-wash` — banners, highlight
  final Color accentWash;
  /// `--accent-wash-soft` — arrival wash
  final Color accentWashSoft;
  /// `--accent-rule` — the "N NEW" divider
  final Color accentRule;
  /// `--accent-underline`
  final Color accentUnderline;
  /// `--accent-dim` — transcribing, pending, retry countdown
  final Color accentDim;
  /// `--accent-queue` — offline QUEUE button
  final Color accentQueue;
  /// `--on-accent` — text on an accent fill
  final Color onAccent;
  /// `--accent-mark` — the logomark tile; the brand, not a state. Nothing else uses it
  final Color accentMark;
  /// `--signal-fault` — failed send only — never decorative
  final Color signalFault;
  /// `--wave-rest` — unplayed bars
  final Color waveRest;
  /// `--wave-rest-dim` — unplayed bars, subordinate contexts
  final Color waveRestDim;

  static CatenaryTokens of(BuildContext context) => Theme.of(context).extension<CatenaryTokens>()!;

  /// Every token under the name tokens.css gives it, without the `--`. The
  /// names are the contract with the web client; the Dart field names are
  /// only how a widget spells them.
  Map<String, Color> get byName => {
        'surface-base': surfaceBase,
        'surface-rail': surfaceRail,
        'surface-lift': surfaceLift,
        'surface-raised': surfaceRaised,
        'surface-avatar': surfaceAvatar,
        'line-hair': lineHair,
        'line-edge': lineEdge,
        'line-inner': lineInner,
        'line-faint': lineFaint,
        'line-accent-dim': lineAccentDim,
        'text-primary': textPrimary,
        'text-bright': textBright,
        'text-secondary': textSecondary,
        'text-meta': textMeta,
        'text-dim': textDim,
        'text-faint': textFaint,
        'text-disabled': textDisabled,
        'text-placeholder': textPlaceholder,
        'text-quote': textQuote,
        'accent-wire': accentWire,
        'accent-wash': accentWash,
        'accent-wash-soft': accentWashSoft,
        'accent-rule': accentRule,
        'accent-underline': accentUnderline,
        'accent-dim': accentDim,
        'accent-queue': accentQueue,
        'on-accent': onAccent,
        'accent-mark': accentMark,
        'signal-fault': signalFault,
        'wave-rest': waveRest,
        'wave-rest-dim': waveRestDim,
      };

  static const CatenaryTokens dark = CatenaryTokens(
    surfaceBase: Color(0xFF0B0D0E),
    surfaceRail: Color(0xFF0E1112),
    surfaceLift: Color(0xFF0F1213),
    surfaceRaised: Color(0xFF171B1C),
    surfaceAvatar: Color(0xFF2C3335),
    lineHair: Color(0xFF1E2325),
    lineEdge: Color(0xFF23292B),
    lineInner: Color(0xFF1A1F21),
    lineFaint: Color(0xFF14181A),
    lineAccentDim: Color(0xFF3A3227),
    textPrimary: Color(0xFFE8ECEA),
    textBright: Color(0xFFC3C9C7),
    textSecondary: Color(0xFF98A2A0),
    textMeta: Color(0xFF626D6C),
    textDim: Color(0xFF47504F),
    textFaint: Color(0xFF2F3739),
    textDisabled: Color(0xFF3C4544),
    textPlaceholder: Color(0xFF4D5654),
    textQuote: Color(0xFF7E8785),
    accentWire: Color(0xFFE0913F),
    accentWash: Color(0x24E0913F),
    accentWashSoft: Color(0x1AE0913F),
    accentRule: Color(0x8CE0913F),
    accentUnderline: Color(0x8CE0913F),
    accentDim: Color(0xFFC99458),
    accentQueue: Color(0xFF8C6533),
    onAccent: Color(0xFF0B0D0E),
    accentMark: Color(0xFFE5A03C),
    signalFault: Color(0xFFD1594A),
    waveRest: Color(0xFF39413F),
    waveRestDim: Color(0xFF2F3739),
  );

  static const CatenaryTokens light = CatenaryTokens(
    surfaceBase: Color(0xFFF7F6F3),
    surfaceRail: Color(0xFFEFEDE8),
    surfaceLift: Color(0xFFEDEAE4),
    surfaceRaised: Color(0xFFE4E1DA),
    surfaceAvatar: Color(0xFFD2CEC4),
    lineHair: Color(0xFFD9D5CC),
    lineEdge: Color(0xFFCFCABF),
    lineInner: Color(0xFFE0DCD3),
    lineFaint: Color(0xFFE8E5DD),
    lineAccentDim: Color(0xFFDCC49E),
    textPrimary: Color(0xFF16191A),
    textBright: Color(0xFF2C302F),
    textSecondary: Color(0xFF5C6360),
    textMeta: Color(0xFF8A908C),
    textDim: Color(0xFFA2A7A2),
    textFaint: Color(0xFFC2C5BE),
    textDisabled: Color(0xFFB5B9B2),
    textPlaceholder: Color(0xFFA9ADA6),
    textQuote: Color(0xFF767B76),
    accentWire: Color(0xFFA8611A),
    accentWash: Color(0x24A8611A),
    accentWashSoft: Color(0x1AA8611A),
    accentRule: Color(0x8CA8611A),
    accentUnderline: Color(0x8CA8611A),
    accentDim: Color(0xFF8A5518),
    accentQueue: Color(0xFFC08A4E),
    onAccent: Color(0xFFF7F6F3),
    accentMark: Color(0xFFC4761F),
    signalFault: Color(0xFFA63A2C),
    waveRest: Color(0xFFBFC3BC),
    waveRestDim: Color(0xFFCBCEC7),
  );

  @override
  CatenaryTokens copyWith() => this;

  /// A theme change is a swap, not a tween: the two tables are two fixed sets
  /// of values, and a color halfway between them is in neither.
  @override
  CatenaryTokens lerp(ThemeExtension<CatenaryTokens>? other, double t) =>
      other is CatenaryTokens && t >= 0.5 ? other : this;
}
