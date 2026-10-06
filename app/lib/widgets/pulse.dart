// The motions the design allows itself: a pulse, a skeleton's shimmer (the
// same fade between two other opacities) and a hairline sweep. None is a
// spinner. The twins of `catPulse`, `catShimmer` and `catSweep` in the web
// client's base.css.

import 'package:flutter/widgets.dart';

/// Fades its child to a quarter and back: 1 → 0.25 → 1, ease in and out.
class Pulse extends StatefulWidget {
  const Pulse({super.key, required this.child, this.period = const Duration(milliseconds: 1400), this.delay = Duration.zero})
      : from = 1,
        to = 0.25;

  /// A skeleton line's shimmer: 0.35 → 0.85 → 0.35 over 1.6s.
  const Pulse.shimmer({super.key, required this.child, this.delay = Duration.zero})
      : period = const Duration(milliseconds: 1600),
        from = 0.35,
        to = 0.85;

  final Widget child;
  final Duration period;

  /// The opacity the cycle starts and ends at, and the one it turns at.
  final double from;
  final double to;

  /// How far into the cycle this one starts, so three dots can follow each
  /// other.
  final Duration delay;

  @override
  State<Pulse> createState() => _PulseState();
}

class _PulseState extends State<Pulse> with SingleTickerProviderStateMixin {
  late final AnimationController _c = AnimationController(vsync: this, duration: widget.period);

  @override
  void initState() {
    super.initState();
    // With no delay the first cycle starts at its own end and is over at
    // once, with nothing left to hear it complete: that pulse repeats from
    // the start. A delayed one runs out the rest of its cycle first.
    final from = 1 - (widget.delay.inMicroseconds / widget.period.inMicroseconds) % 1;
    if (from == 1) {
      _c.repeat();
    } else {
      _c.forward(from: from).whenComplete(() {
        if (mounted) _c.repeat();
      });
    }
  }

  @override
  void dispose() {
    _c.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return FadeTransition(
      opacity: _c.drive(TweenSequence([
        TweenSequenceItem(tween: Tween(begin: widget.from, end: widget.to).chain(CurveTween(curve: Curves.easeInOut)), weight: 1),
        TweenSequenceItem(tween: Tween(begin: widget.to, end: widget.from).chain(CurveTween(curve: Curves.easeInOut)), weight: 1),
      ])),
      child: widget.child,
    );
  }
}

/// A 2px track with a bar a third of its width crossing it, left to right.
class Sweep extends StatefulWidget {
  const Sweep({super.key, required this.track, required this.bar});

  final Color track;
  final Color bar;

  @override
  State<Sweep> createState() => _SweepState();
}

class _SweepState extends State<Sweep> with SingleTickerProviderStateMixin {
  late final AnimationController _c = AnimationController(vsync: this, duration: const Duration(milliseconds: 1100))..repeat();

  @override
  void dispose() {
    _c.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return SizedBox(
      height: 2,
      child: ClipRect(
        child: ColoredBox(
          color: widget.track,
          child: AnimatedBuilder(
            animation: _c,
            builder: (context, _) => FractionallySizedBox(
              widthFactor: 0.3,
              // -100% to 320% of the bar's own width, as the CSS has it.
              alignment: Alignment(-1 + 2 * ((-1 + 4.2 * _c.value) * 0.3 / 0.7), 0),
              child: ColoredBox(color: widget.bar, child: const SizedBox.expand()),
            ),
          ),
        ),
      ),
    );
  }
}
