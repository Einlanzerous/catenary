// The four pictographs the canvas uses — search, more, play, and the cross —
// drawn rather than typed. The canvas writes them as characters (⌕ ⋯ ▶ ✕),
// and IBM Plex has none of them: typed, each would be whatever the device's
// fallback font happens to hold, or an empty box. Drawn, they are the same
// 1px strokes on every device.

import 'package:flutter/widgets.dart';

enum Glyph { search, more, play, cross }

class GlyphIcon extends StatelessWidget {
  const GlyphIcon(this.glyph, {super.key, required this.color, this.size = 11});

  final Glyph glyph;
  final Color color;
  final double size;

  @override
  Widget build(BuildContext context) => SizedBox.square(dimension: size, child: CustomPaint(painter: _GlyphPainter(glyph, color)));
}

class _GlyphPainter extends CustomPainter {
  const _GlyphPainter(this.glyph, this.color);

  final Glyph glyph;
  final Color color;

  @override
  void paint(Canvas canvas, Size size) {
    final s = size.width;
    final stroke = Paint()
      ..color = color
      ..style = PaintingStyle.stroke
      ..strokeWidth = 1.2;
    final fill = Paint()..color = color;
    switch (glyph) {
      case Glyph.search:
        canvas.drawCircle(Offset(s * 0.42, s * 0.42), s * 0.3, stroke);
        canvas.drawLine(Offset(s * 0.64, s * 0.64), Offset(s * 0.95, s * 0.95), stroke);
      case Glyph.more:
        for (final x in [0.14, 0.5, 0.86]) {
          canvas.drawRect(Rect.fromCenter(center: Offset(s * x, s * 0.5), width: 2, height: 2), fill);
        }
      case Glyph.play:
        canvas.drawPath(
          Path()
            ..moveTo(s * 0.12, 0)
            ..lineTo(s * 0.95, s * 0.5)
            ..lineTo(s * 0.12, s)
            ..close(),
          fill,
        );
      case Glyph.cross:
        canvas.drawLine(Offset(s * 0.1, s * 0.1), Offset(s * 0.9, s * 0.9), stroke);
        canvas.drawLine(Offset(s * 0.9, s * 0.1), Offset(s * 0.1, s * 0.9), stroke);
    }
  }

  @override
  bool shouldRepaint(_GlyphPainter old) => old.glyph != glyph || old.color != color;
}
