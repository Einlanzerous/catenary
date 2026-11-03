// The in-composer attach zone (M-ATT-02 and M-ATT-03 of the mobile canvas; web
// ATT-02/04). It is the first row inside the composer, at the composer's width
// and in the chip rows' column, and it grows upward because the composer row
// stays pinned at the bottom. No drag overlay on touch, no copper, no motion.
//
//   prompt        Choose an image, or paste one.   (the paste half only where
//                 the keyboard can insert one)
//   CHOOSE FILE   44px target, focused on open
//   accepted      JPEG, PNG, GIF or WebP · up to 16 MB
//   refusal       one status line under a hairline: the file's name and type,
//                 then the words. Text only; it replaces the last one.

import 'package:flutter/material.dart';

import '../store/attach.dart';
import '../metrics.dart';
import '../tokens.dart';

class AttachZone extends StatelessWidget {
  const AttachZone({
    super.key,
    required this.chooseFocus,
    required this.onChoose,
    this.canPaste = false,
    this.refusal,
  });

  final FocusNode chooseFocus;
  final VoidCallback onChoose;

  /// Whether the platform can paste an image here; the prompt says so only then.
  final bool canPaste;
  final PickRefused? refusal;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final r = refusal;
    return Semantics(
      container: true,
      label: 'Attach an image',
      child: Container(
        key: const ValueKey('attach-zone'),
        padding: const EdgeInsets.all(14),
        decoration: BoxDecoration(color: t.surfaceRail, border: Border.all(color: t.lineHair)),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(
              canPaste ? 'Choose an image, or paste one.' : 'Choose an image.',
              style: TextStyle(fontFamily: fontSans, fontSize: 13.5, height: 20 / 13.5, color: t.textPrimary),
            ),
            const SizedBox(height: 12),
            Focus(
              focusNode: chooseFocus,
              child: Builder(
                builder: (context) {
                  final focused = Focus.of(context).hasFocus;
                  return GestureDetector(
                    key: const ValueKey('attach-choose'),
                    behavior: HitTestBehavior.opaque,
                    onTap: onChoose,
                    child: Container(
                      height: 44,
                      padding: const EdgeInsets.symmetric(horizontal: 16),
                      alignment: Alignment.center,
                      decoration: BoxDecoration(
                        color: t.surfaceBase,
                        border: Border.all(color: t.lineHair),
                        // The focus ring: 2px, offset 2px, outside the border.
                      ),
                      foregroundDecoration: focused ? _Ring(t.textPrimary) : null,
                      child: Text(
                        'CHOOSE FILE',
                        style: TextStyle(fontFamily: fontMono, fontSize: 10, letterSpacing: 1.4, color: t.textPrimary),
                      ),
                    ),
                  );
                },
              ),
            ),
            const SizedBox(height: 12),
            Text(
              acceptedLine(),
              key: const ValueKey('attach-accepted'),
              style: TextStyle(fontFamily: fontMono, fontSize: 10.5, height: 14 / 10.5, color: t.textSecondary),
            ),
            if (r != null) ...[
              const SizedBox(height: 12),
              Container(
                key: const ValueKey('attach-refusal'),
                width: double.infinity,
                padding: const EdgeInsets.only(top: 10),
                decoration: BoxDecoration(border: Border(top: BorderSide(color: t.lineHair))),
                child: Semantics(
                  liveRegion: true,
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      if (r.fileName != null || r.kind != null)
                        Row(
                          children: [
                            if (r.fileName != null)
                              Expanded(
                                child: Text(
                                  r.fileName!,
                                  maxLines: 1,
                                  overflow: TextOverflow.ellipsis,
                                  style: TextStyle(fontFamily: fontMono, fontSize: 10.5, color: t.textSecondary),
                                ),
                              )
                            else
                              const Spacer(),
                            if (r.kind != null) ...[
                              const SizedBox(width: 10),
                              Text(r.kind!, style: TextStyle(fontFamily: fontMono, fontSize: 10.5, color: t.textSecondary)),
                            ],
                          ],
                        ),
                      const SizedBox(height: 3),
                      Text(
                        r.message,
                        key: const ValueKey('attach-refusal-text'),
                        style: TextStyle(fontFamily: fontSans, fontSize: 13.5, height: 20 / 13.5, color: t.textPrimary),
                      ),
                    ],
                  ),
                ),
              ),
            ],
          ],
        ),
      ),
    );
  }
}

/// A 2px outline drawn 2px outside the box it decorates.
class _Ring extends Decoration {
  const _Ring(this.color);

  final Color color;

  @override
  BoxPainter createBoxPainter([VoidCallback? onChanged]) => _RingPainter(color);
}

class _RingPainter extends BoxPainter {
  _RingPainter(this.color);

  final Color color;

  @override
  void paint(Canvas canvas, Offset offset, ImageConfiguration configuration) {
    final size = configuration.size;
    if (size == null) return;
    final rect = (offset & size).inflate(3);
    canvas.drawRect(rect, Paint()..color = color..style = PaintingStyle.stroke..strokeWidth = 2);
  }
}
