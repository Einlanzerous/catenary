// The chip a composer holds above the field (CANT-325, section 1 of
// `Catenary Link Preview.dc.html`, narrow = 1E): the link chip and the image
// chip are ONE component with two tags, stacked, sharing a 58px tag column.
//
//   [ LINK  | host                         | ✕ ]   44 high, the ✕ a 44×44 target
//   [       | title, or FETCHING PREVIEW   |   ]
//   [ IMAGE | IMG_4471.HEIC                | ✕ ]
//
// The host is parsed from the pasted URL locally, so it shows the instant the
// chip appears; only the title waits. FETCHING PREVIEW carries a 1px hairline
// sweeping the bottom edge, linear, 1.4 s. SEND stays live while it runs:
// sent while fetching, the message goes out as plain text and never gains a
// card. The ground is surface.base on the surface.rail tray: always one step
// off what it sits on.

import 'package:flutter/material.dart';

import '../metrics.dart';
import '../tokens.dart';
import 'glyph.dart';

/// What the link chip says: the pasted URL's host at once, and the page's
/// title when the fetch has one.
class HeldLink {
  const HeldLink({required this.host, this.title, this.fetching = false});

  final String host;
  final String? title;
  final bool fetching;
}

class HeldChip extends StatelessWidget {
  const HeldChip({
    super.key,
    required this.tag,
    required this.primary,
    this.secondary,
    this.fetching = false,
    required this.onRemove,
    this.removeKey,
  });

  /// `LINK` or `IMAGE`.
  final String tag;

  /// The host, or the file name.
  final String primary;

  /// The title; null for the image chip, or a link chip whose title has not
  /// arrived and is not fetching.
  final String? secondary;
  final bool fetching;
  final VoidCallback? onRemove;
  final Key? removeKey;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final line = BorderSide(color: t.lineHair);
    final host = TextStyle(fontFamily: fontMono, fontSize: 10.5, height: 14 / 10.5, color: t.textSecondary);
    final Widget lines;
    if (fetching) {
      lines = Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.start, children: [
        Text(primary, maxLines: 1, overflow: TextOverflow.ellipsis, style: host),
        Text(
          'FETCHING PREVIEW',
          style: TextStyle(fontFamily: fontMono, fontSize: 9.5, height: 18 / 9.5, letterSpacing: 9.5 * 0.12, color: t.textMeta),
        ),
      ]);
    } else if (secondary != null) {
      lines = Column(mainAxisSize: MainAxisSize.min, crossAxisAlignment: CrossAxisAlignment.start, children: [
        Text(primary, maxLines: 1, overflow: TextOverflow.ellipsis, style: host),
        Text(
          secondary!,
          maxLines: 1,
          overflow: TextOverflow.ellipsis,
          style: TextStyle(fontFamily: fontSans, fontSize: 13, height: 18 / 13, color: t.textPrimary),
        ),
      ]);
    } else {
      lines = Text(primary, maxLines: 1, overflow: TextOverflow.ellipsis, style: host);
    }
    return Container(
      height: 44,
      clipBehavior: Clip.hardEdge,
      // The border is painted over the box and takes no room: the ✕ is the full
      // 44×44 target the canvas calls for, not 42.
      decoration: BoxDecoration(color: t.surfaceBase),
      foregroundDecoration: BoxDecoration(border: Border.fromBorderSide(line)),
      child: Stack(
        children: [
          Row(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              Container(
                width: 58,
                alignment: Alignment.center,
                decoration: BoxDecoration(border: Border(right: line)),
                child: Text(tag, style: TextStyle(fontFamily: fontMono, fontSize: 9.5, letterSpacing: 9.5 * 0.14, color: t.textSecondary)),
              ),
              Expanded(
                child: Padding(padding: const EdgeInsets.symmetric(horizontal: 10), child: Align(alignment: Alignment.centerLeft, child: lines)),
              ),
              GestureDetector(
                key: removeKey,
                behavior: HitTestBehavior.opaque,
                onTap: onRemove,
                child: Container(
                  width: 44,
                  alignment: Alignment.center,
                  decoration: BoxDecoration(border: Border(left: line)),
                  child: Semantics(label: 'Remove', button: true, child: GlyphIcon(Glyph.cross, color: t.textSecondary, size: 9)),
                ),
              ),
            ],
          ),
          if (fetching) Positioned(left: 0, right: 0, bottom: 0, height: 1, child: _Sweep(color: t.textMeta)),
        ],
      ),
    );
  }
}

/// The fetch sweep, `catSweep`: a hairline a quarter of the chip wide, from
/// off the left edge to off the right, linear, 1.4 s, repeating. Colour and
/// position only; nothing changes the chip's height.
class _Sweep extends StatefulWidget {
  const _Sweep({required this.color});

  final Color color;

  @override
  State<_Sweep> createState() => _SweepState();
}

class _SweepState extends State<_Sweep> with SingleTickerProviderStateMixin {
  late final AnimationController _c = AnimationController(vsync: this, duration: const Duration(milliseconds: 1400))..repeat();

  @override
  void dispose() {
    _c.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => ClipRect(
        child: FractionallySizedBox(
          widthFactor: 0.24,
          alignment: Alignment.centerLeft,
          child: AnimatedBuilder(
            animation: _c,
            builder: (context, child) =>
                FractionalTranslation(translation: Offset(-1 + 5.2 * _c.value, 0), child: child),
            child: ColoredBox(key: const ValueKey('held-chip-sweep'), color: widget.color),
          ),
        ),
      );
}
