// The token specimen: every token under its contract name, in the theme on
// screen, with the type scale and the one rounded thing. It is what the
// skeleton shows until the rail and thread exist (CANT-43), and afterwards it
// is how a token change is looked at on a device.

import 'package:flutter/material.dart';

import 'metrics.dart';
import 'states.dart';
import 'tokens.dart';

class SpecimenScreen extends StatelessWidget {
  const SpecimenScreen({super.key, required this.mode, required this.onMode});

  final ThemeMode mode;
  final ValueChanged<ThemeMode> onMode;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final dark = Theme.of(context).brightness == Brightness.dark;
    return Scaffold(
      body: SafeArea(
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            _Header(dark: dark, mode: mode, onMode: onMode),
            Expanded(
              child: ListView(
                padding: const EdgeInsets.only(bottom: CatenaryMetrics.s12),
                children: [
                  const _Section('TYPE'),
                  for (final e in CatenaryType.byName.entries) _TypeRow(name: e.key, token: e.value),
                  const _Section('COLOR'),
                  for (final e in t.byName.entries) _ColorRow(name: e.key, color: e.value),
                  const _Section('INPUT · RADIUS 2'),
                  Padding(
                    padding: const EdgeInsets.all(CatenaryMetrics.s4),
                    child: TextField(
                      style: CatenaryType.body.style.copyWith(color: t.textPrimary),
                      decoration: const InputDecoration(hintText: 'Message'),
                    ),
                  ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }
}

class _Header extends StatelessWidget {
  const _Header({required this.dark, required this.mode, required this.onMode});

  final bool dark;
  final ThemeMode mode;
  final ValueChanged<ThemeMode> onMode;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return Container(
      decoration: BoxDecoration(color: t.surfaceRail, border: Border(bottom: BorderSide(color: t.lineHair))),
      padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4, vertical: CatenaryMetrics.s3),
      child: Row(
        children: [
          Container(width: 8, height: 8, color: t.accentWire),
          const SizedBox(width: CatenaryMetrics.s3),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text('Catenary', style: CatenaryType.title.style.copyWith(color: t.textPrimary)),
                Text(
                  'TOKENS · ${dark ? 'DARK' : 'LIGHT'}',
                  key: const ValueKey('theme-label'),
                  style: CatenaryType.label.tracked.copyWith(color: t.textMeta),
                ),
              ],
            ),
          ),
          TextButton(
            key: const ValueKey('open-states'),
            onPressed: () => Navigator.of(context).push(MaterialPageRoute<void>(builder: (_) => const StatesScreen())),
            child: Text('STATES', style: CatenaryType.meta.tracked),
          ),
          TextButton(
            key: const ValueKey('theme-toggle'),
            onPressed: () => onMode(dark ? ThemeMode.light : ThemeMode.dark),
            child: Text(dark ? 'LIGHT' : 'DARK', style: CatenaryType.meta.tracked),
          ),
        ],
      ),
    );
  }
}

class _Section extends StatelessWidget {
  const _Section(this.title);

  final String title;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return Padding(
      padding: const EdgeInsets.fromLTRB(CatenaryMetrics.s4, CatenaryMetrics.s6, CatenaryMetrics.s4, CatenaryMetrics.s2),
      child: Text(title, style: CatenaryType.label.tracked.copyWith(color: t.textMeta)),
    );
  }
}

class _TypeRow extends StatelessWidget {
  const _TypeRow({required this.name, required this.token});

  final String name;
  final TypeToken token;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return Container(
      decoration: BoxDecoration(border: Border(bottom: BorderSide(color: t.lineFaint))),
      padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4, vertical: CatenaryMetrics.s2),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('The wire carries it', style: token.style.copyWith(color: t.textPrimary)),
          Text(
            '$name · ${token.weight} ${_px(token.size)}/${_px(token.lineHeight)}',
            style: CatenaryType.meta.style.copyWith(color: t.textMeta),
          ),
        ],
      ),
    );
  }
}

class _ColorRow extends StatelessWidget {
  const _ColorRow({required this.name, required this.color});

  final String name;
  final Color color;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return Container(
      key: ValueKey('token-$name'),
      decoration: BoxDecoration(border: Border(bottom: BorderSide(color: t.lineFaint))),
      padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4, vertical: CatenaryMetrics.s2),
      child: Row(
        children: [
          Container(
            width: CatenaryMetrics.s8,
            height: CatenaryMetrics.s6,
            decoration: BoxDecoration(color: color, border: Border.all(color: t.lineEdge)),
          ),
          const SizedBox(width: CatenaryMetrics.s3),
          Expanded(child: Text(name, style: CatenaryType.secondary.style.copyWith(color: t.textPrimary))),
          Text(_hex(color), style: CatenaryType.meta.style.copyWith(color: t.textMeta)),
        ],
      ),
    );
  }
}

String _px(double v) => v == v.roundToDouble() ? v.toStringAsFixed(0) : v.toString();

/// `#RRGGBB`, with the alpha as a percentage when the token has one.
String _hex(Color c) {
  final argb = c.toARGB32();
  final rgb = (argb & 0xFFFFFF).toRadixString(16).padLeft(6, '0').toUpperCase();
  final alpha = argb >>> 24;
  return alpha == 0xFF ? '#$rgb' : '#$rgb · ${(alpha / 255 * 100).round()}%';
}
