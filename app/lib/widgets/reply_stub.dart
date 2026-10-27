// The quoted stub and the armed reply bar for a message that is a link
// (CANT-325, section 3 of `Catenary Link Preview.dc.html`, narrow = 3C).
//
// The app does not have replies yet, so these two are built to the canvas and
// fed by fixtures, ready for the ticket that gives a reply its model. They
// take plain strings, not a message.
//
//   stub   2px line.hair rule · SENDER · ↗ LINK · host · title (one line)
//   bar    2px accent.wire rule (the existing "active" use) · REPLYING TO
//          SENDER · ↗ LINK · host · title · ✕ (44×44) on surface.rail
//
// An empty preview ends the stub at the host. There is no "no preview"
// filler: the host alone is honest and always true, since it comes from the
// URL. `↗ LINK` is text.meta, not copper: the accent says "active" here and
// nothing else (canvas flag 4).

import 'package:flutter/material.dart';

import '../metrics.dart';
import '../store/conversation.dart';
import '../tokens.dart';
import 'glyph.dart';

/// `↗ LINK`, typed: IBM Plex Mono has the arrow.
class _LinkTag extends StatelessWidget {
  const _LinkTag({required this.color});

  final Color color;

  @override
  Widget build(BuildContext context) => Text(
        '\u2197 LINK',
        style: TextStyle(fontFamily: fontMono, fontSize: 9.5, letterSpacing: 9.5 * 0.06, color: color),
      );
}

class QuotedLinkStub extends StatelessWidget {
  const QuotedLinkStub({super.key, required this.sender, required this.link});

  /// The quoted message's author, as the canvas abbreviates it: a first name.
  final String sender;
  final LinkPreviewView link;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final title = link.isEmpty ? null : link.cleanTitle;
    return Container(
      key: const ValueKey('stub-rule'),
      // The 2px rule runs the stub's height; the text starts 9 after it.
      decoration: BoxDecoration(border: Border(left: BorderSide(color: t.lineHair, width: 2))),
      padding: const EdgeInsets.only(left: 9),
      child: Row(crossAxisAlignment: CrossAxisAlignment.baseline, textBaseline: TextBaseline.alphabetic, children: [
        Text(
          sender.toUpperCase(),
          style: TextStyle(fontFamily: fontMono, fontSize: 9.5, letterSpacing: 9.5 * 0.06, color: t.textSecondary),
        ),
        const SizedBox(width: 7),
        _LinkTag(color: t.textMeta),
        const SizedBox(width: 7),
        Flexible(
          flex: title == null ? 1 : 0,
          child: ConstrainedBox(
            constraints: BoxConstraints(maxWidth: title == null ? double.infinity : 140),
            child: Text(
              link.host,
              key: const ValueKey('stub-host'),
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: TextStyle(fontFamily: fontMono, fontSize: 10, color: t.textSecondary),
            ),
          ),
        ),
        if (title != null) ...[
          const SizedBox(width: 7),
          Expanded(
            child: Text(
              title,
              key: const ValueKey('stub-title'),
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: TextStyle(fontFamily: fontSans, fontSize: 12, height: 16 / 12, color: t.textMeta),
            ),
          ),
        ],
      ]),
    );
  }
}

class ArmedLinkBar extends StatelessWidget {
  const ArmedLinkBar({super.key, required this.sender, required this.link, required this.onClear});

  final String sender;
  final LinkPreviewView link;
  final VoidCallback onClear;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final title = link.isEmpty ? null : link.cleanTitle;
    return Container(
      decoration: BoxDecoration(color: t.surfaceRail, border: Border(bottom: BorderSide(color: t.lineHair))),
      padding: const EdgeInsets.only(left: 12),
      child: Row(children: [
        Container(width: 2, height: 32, color: t.accentWire),
        const SizedBox(width: 9),
        Expanded(
          child: Padding(
            padding: const EdgeInsets.symmetric(vertical: 7),
            child: Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.start, children: [
              Text.rich(
                TextSpan(children: [
                  TextSpan(text: 'REPLYING TO ${sender.toUpperCase()}', style: TextStyle(color: t.accentWire)),
                  TextSpan(text: ' · \u2197 LINK', style: TextStyle(color: t.textMeta)),
                ]),
                key: const ValueKey('bar-label'),
                style: TextStyle(fontFamily: fontMono, fontSize: 9.5, letterSpacing: 9.5 * 0.1),
              ),
              const SizedBox(height: 2),
              Row(crossAxisAlignment: CrossAxisAlignment.baseline, textBaseline: TextBaseline.alphabetic, children: [
                Flexible(
                  flex: title == null ? 1 : 0,
                  child: ConstrainedBox(
                    constraints: BoxConstraints(maxWidth: title == null ? double.infinity : 190),
                    child: Text(
                      link.host,
                      key: const ValueKey('bar-host'),
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: TextStyle(fontFamily: fontMono, fontSize: 10.5, height: 16 / 10.5, color: t.textSecondary),
                    ),
                  ),
                ),
                if (title != null) ...[
                  const SizedBox(width: 7),
                  Expanded(
                    child: Text(
                      title,
                      key: const ValueKey('bar-title'),
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: TextStyle(fontFamily: fontSans, fontSize: 12, height: 16 / 12, color: t.textMeta),
                    ),
                  ),
                ],
              ]),
            ]),
          ),
        ),
        GestureDetector(
          key: const ValueKey('bar-clear'),
          behavior: HitTestBehavior.opaque,
          onTap: onClear,
          child: SizedBox(width: 44, height: 44, child: Center(child: GlyphIcon(Glyph.cross, color: t.textSecondary, size: 9))),
        ),
      ]),
    );
  }
}
