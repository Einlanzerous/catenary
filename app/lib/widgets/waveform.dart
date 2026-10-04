// A voice note's waveform: 2px bars from the peaks the server computed, the
// played ones in the accent. The bars are data — this draws them and derives
// nothing.

import 'package:flutter/widgets.dart';

class Waveform extends StatelessWidget {
  const Waveform({super.key, required this.peaks, required this.rest, required this.on, this.played = 0, this.height = 30});

  /// 0–100 per bar.
  final List<int> peaks;
  final Color rest;
  final Color on;

  /// The fraction already played, 0–1.
  final double played;
  final double height;

  @override
  Widget build(BuildContext context) {
    return SizedBox(height: height, child: ClipRect(child: CustomPaint(painter: _BarsPainter(peaks, rest, on, played))));
  }
}

class _BarsPainter extends CustomPainter {
  const _BarsPainter(this.peaks, this.rest, this.on, this.played);

  final List<int> peaks;
  final Color rest;
  final Color on;
  final double played;

  @override
  void paint(Canvas canvas, Size size) {
    const w = 2.0, gap = 2.0;
    for (var i = 0; i < peaks.length; i++) {
      final x = i * (w + gap);
      if (x + w > size.width) break;
      final h = size.height * peaks[i].clamp(0, 100) / 100;
      canvas.drawRect(
        Rect.fromLTWH(x, (size.height - h) / 2, w, h),
        Paint()..color = i / peaks.length < played ? on : rest,
      );
    }
  }

  @override
  bool shouldRepaint(_BarsPainter old) => old.peaks != peaks || old.played != played || old.rest != rest || old.on != on;
}
