// "Names are the contract, values are per-theme." The contract's other party
// is the web client, so this test reads ITS table — web/src/styles/tokens.css
// — and holds lib/tokens.dart and lib/metrics.dart to it: the same names, the
// same value under each name in each theme, and nothing on either side the
// other lacks. A token added, renamed or re-valued in one runtime fails here
// until the other follows.

import 'dart:io';

import 'package:catenary/metrics.dart';
import 'package:catenary/tokens.dart';
import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';

/// The custom properties of the first rule whose selector matches, by name.
Map<String, String> block(String css, RegExp selector) {
  final rule = RegExp('${selector.pattern}\\s*\\{(.*?)\\n\\}', dotAll: true).firstMatch(css);
  if (rule == null) fail('tokens.css has no rule matching ${selector.pattern}');
  return {
    for (final m in RegExp(r'--([a-z0-9-]+):\s*([^;]+);').allMatches(rule.group(1)!)) m.group(1)!: m.group(2)!.trim(),
  };
}

/// A CSS color as the Color Flutter paints: `#rrggbb`, or `rgba(r, g, b, a)`
/// with the alpha rounded to a byte, as a browser rounds it.
Color cssColor(String name, String value) {
  final hex = RegExp(r'^#([0-9a-fA-F]{6})$').firstMatch(value);
  if (hex != null) return Color(0xFF000000 | int.parse(hex.group(1)!, radix: 16));
  final rgba = RegExp(r'^rgba\((\d+),\s*(\d+),\s*(\d+),\s*([0-9.]+)\)$').firstMatch(value);
  if (rgba != null) {
    final a = (double.parse(rgba.group(4)!) * 255).round();
    return Color.fromARGB(a, int.parse(rgba.group(1)!), int.parse(rgba.group(2)!), int.parse(rgba.group(3)!));
  }
  fail('--$name: $value is neither #rrggbb nor rgba(); teach this test the form');
}

void main() {
  final css = File('../web/src/styles/tokens.css').readAsStringSync();
  final dark = block(css, RegExp(r":root,\s*:root\[data-theme='dark'\]"))..remove('color-scheme');
  final light = block(css, RegExp(r":root\[data-theme='light'\]"))..remove('color-scheme');
  final fixed = block(css, RegExp(r'\n:root'));

  test('tokens.css is the table this test thinks it is', () {
    expect(dark.keys, containsAll(['surface-base', 'accent-wire', 'signal-fault']));
    expect(light.keys.toSet(), dark.keys.toSet(), reason: 'no token exists in one theme only');
    expect(fixed.keys, containsAll(['type-body', 's4', 'radius-input']));
  });

  for (final (theme, table, tokens) in [
    ('dark', dark, CatenaryTokens.dark),
    ('light', light, CatenaryTokens.light),
  ]) {
    test('$theme: every color token is named as the web client names it', () {
      expect(tokens.byName.keys.toSet(), table.keys.toSet());
    });

    test('$theme: every color token has the web client\'s value', () {
      for (final e in table.entries) {
        expect(tokens.byName[e.key], cssColor(e.key, e.value), reason: '--${e.key}: ${e.value}');
      }
    });
  }

  test('the one accent is one color per theme, and the two themes differ', () {
    expect(CatenaryTokens.dark.accentWire, const Color(0xFFE0913F));
    expect(CatenaryTokens.light.accentWire, const Color(0xFFA8611A));
  });

  test('the type scale is the web client\'s: weight, size, line height and family', () {
    final types = {for (final e in fixed.entries) if (e.key.startsWith('type-')) e.key: e.value};
    expect(CatenaryType.byName.keys.toSet(), types.keys.toSet());
    for (final e in types.entries) {
      final m = RegExp(r'^(\d+) ([0-9.]+)px/([0-9.]+)px var\(--font-(sans|mono)\)$').firstMatch(e.value);
      if (m == null) fail('--${e.key}: ${e.value} is not `weight size/line var(--font-*)`');
      final t = CatenaryType.byName[e.key]!;
      expect(
        (t.weight, t.size, t.lineHeight, t.family),
        (int.parse(m.group(1)!), double.parse(m.group(2)!), double.parse(m.group(3)!), m.group(4) == 'sans' ? fontSans : fontMono),
        reason: '--${e.key}: ${e.value}',
      );
    }
    expect(fixed['font-sans'], startsWith("'$fontSans'"));
    expect(fixed['font-mono'], startsWith("'$fontMono'"));
    expect(fixed['track-label'], '${trackLabel}em');
    expect(fixed['track-tight'], '${trackTight}em');
  });

  test('space, layout and the one radius are the web client\'s', () {
    final lengths = {
      for (final e in fixed.entries)
        if (RegExp(r'^[0-9.]+px$').hasMatch(e.value)) e.key: double.parse(e.value.replaceAll('px', '')),
    };
    expect(CatenaryMetrics.byName, lengths);
    // The only fixed tokens with no pixel value, each accounted for elsewhere.
    final other = fixed.keys.toSet().difference(lengths.keys.toSet()).where((k) => !k.startsWith('type-')).toSet();
    expect(other, {'font-sans', 'font-mono', 'track-label', 'track-tight', 'measure'});
  });
}
