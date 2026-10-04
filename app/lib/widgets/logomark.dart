// The logomark (canvas 07, form 1C): a square tile in `accent-mark` with a
// catenary span cut out of it — two posts, a deck, and the wire hanging
// between them. The brand, not a state; nothing else uses that color.

import 'package:flutter/widgets.dart';

import '../tokens.dart';

class Logomark extends StatelessWidget {
  const Logomark({super.key, this.size = 18});

  final double size;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return SizedBox.square(dimension: size, child: CustomPaint(painter: _MarkPainter(t.accentMark)));
  }
}

class _MarkPainter extends CustomPainter {
  const _MarkPainter(this.tile);

  final Color tile;

  @override
  void paint(Canvas canvas, Size size) {
    // The canvas's own 48-unit viewBox.
    final k = size.width / 48;
    canvas.saveLayer(Offset.zero & size, Paint());
    canvas.drawRect(Offset.zero & size, Paint()..color = tile);
    final cut = Paint()..blendMode = BlendMode.clear;
    canvas.drawRect(Rect.fromLTWH(10 * k, 12 * k, 3.4 * k, 28 * k), cut);
    canvas.drawRect(Rect.fromLTWH(34.6 * k, 12 * k, 3.4 * k, 28 * k), cut);
    canvas.drawRect(Rect.fromLTWH(5 * k, 31 * k, 38 * k, 2.4 * k), cut);
    canvas.drawPath(
      Path()
        ..moveTo(12 * k, 15 * k)
        ..quadraticBezierTo(24 * k, 27 * k, 36 * k, 15 * k),
      Paint()
        ..blendMode = BlendMode.clear
        ..style = PaintingStyle.stroke
        ..strokeWidth = 3 * k,
    );
    canvas.restore();
  }

  @override
  bool shouldRepaint(_MarkPainter old) => old.tile != tile;
}
