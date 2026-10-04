// The composer: the bottom of the window, and the only place a message
// starts. Built to the narrow canvas (`Catenary Mobile.dc.html`, frame 04 A,
// calls M4, M7 and M8): ADD and the primary action are 44px squares flanking
// the field.
//
//   idle        ADD · Message · REC      record is the accent-filled one: it
//                                        is the signature feature and the
//                                        hardest thing to hit on the move
//   typing      ADD · the draft · SEND
//   recording   the clock, the levels, ✕, SEND — it REPLACES the whole row,
//               and the clock lands where the text cursor was
//   offline     ADD dimmed · "Sends when reconnected" · QUEUE, in the dimmer
//               copper
//
// IT NEVER PRETENDS A MESSAGE LEFT THE BUILDING. Off a live session the
// primary action reads QUEUE. ADD dims, because an upload cannot be queued
// safely (CANT-36), and for the same reason a recording cannot be started.
// On a terminal client nothing composed now will drain, so the composer does
// not offer to queue it and the one accent does not sit on a button that
// cannot work (Invariant 3) — that state is not on the canvas.
//
// RECORDING IS AMBER, NOT RED: red belongs to failure alone. SLIDE LEFT TO
// CANCEL is the only gesture in the system, and the ✕ stays as a visible
// fallback so the gesture is never the sole route.

import 'dart:math' as math;

import 'package:flutter/material.dart';

import '../metrics.dart';
import '../store/connection.dart';
import '../tokens.dart';
import 'glyph.dart';
import 'pulse.dart';

/// `00:17`: the recording clock, two digits each side.
String recordingClock(Duration d) {
  final s = d.inSeconds < 0 ? 0 : d.inSeconds;
  return '${(s ~/ 60).toString().padLeft(2, '0')}:${(s % 60).toString().padLeft(2, '0')}';
}

/// How far left a recording has to be dragged to cancel it.
const double slideToCancel = 64;

class Composer extends StatefulWidget {
  const Composer({
    super.key,
    required this.connection,
    this.controller,
    this.recording,
    this.onSend,
    this.onAttach,
    this.onRecord,
    this.onCancelRecording,
    this.onSendRecording,
  });

  final ConnectionView connection;
  final TextEditingController? controller;

  /// How long the recording in progress has run, or null when not recording.
  final Duration? recording;

  final VoidCallback? onSend;
  final VoidCallback? onAttach;
  final VoidCallback? onRecord;
  final VoidCallback? onCancelRecording;
  final VoidCallback? onSendRecording;

  @override
  State<Composer> createState() => _ComposerState();
}

class _ComposerState extends State<Composer> {
  TextEditingController? _own;
  double _slid = 0;

  TextEditingController get _controller => widget.controller ?? (_own ??= TextEditingController());

  @override
  void initState() {
    super.initState();
    _controller.addListener(_changed);
  }

  @override
  void didUpdateWidget(Composer old) {
    super.didUpdateWidget(old);
    if (old.controller != widget.controller) {
      (old.controller ?? _own)?.removeListener(_changed);
      _controller.addListener(_changed);
    }
  }

  @override
  void dispose() {
    _controller.removeListener(_changed);
    _own?.dispose();
    super.dispose();
  }

  void _changed() => setState(() {});

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final recording = widget.recording;
    return Container(
      decoration: BoxDecoration(color: t.surfaceRail, border: Border(top: BorderSide(color: t.lineHair))),
      padding: const EdgeInsets.fromLTRB(12, 10, 12, 14),
      child: recording != null ? _recording(t, recording) : _row(t),
    );
  }

  Widget _recording(CatenaryTokens t, Duration elapsed) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      mainAxisSize: MainAxisSize.min,
      children: [
        GestureDetector(
          onHorizontalDragUpdate: (d) => setState(() => _slid = math.max(0, _slid - d.delta.dx)),
          onHorizontalDragEnd: (_) {
            final cancel = _slid >= slideToCancel;
            setState(() => _slid = 0);
            if (cancel) widget.onCancelRecording?.call();
          },
          child: Container(
            key: const ValueKey('composer-recording'),
            decoration: BoxDecoration(
              color: t.surfaceBase,
              border: Border.all(color: t.accentWire),
              borderRadius: BorderRadius.circular(CatenaryMetrics.radiusInput),
            ),
            padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 8),
            child: Row(
              children: [
                Pulse(child: Container(width: 8, height: 8, color: t.accentWire)),
                const SizedBox(width: 10),
                Text(
                  recordingClock(elapsed),
                  style: TextStyle(
                    fontFamily: fontMono,
                    fontSize: 13,
                    height: 18 / 13,
                    color: t.textPrimary,
                    fontFeatures: const [FontFeature.tabularFigures()],
                  ),
                ),
                const SizedBox(width: 10),
                Expanded(child: SizedBox(height: 26, child: CustomPaint(painter: _LevelsPainter(t.accentWire)))),
                const SizedBox(width: 10),
                GestureDetector(
                  key: const ValueKey('composer-cancel-recording'),
                  behavior: HitTestBehavior.opaque,
                  onTap: widget.onCancelRecording,
                  child: SizedBox(
                    width: 32,
                    height: 36,
                    child: Center(child: GlyphIcon(Glyph.cross, color: t.textSecondary, size: 9)),
                  ),
                ),
                const SizedBox(width: 4),
                _Square('SEND', size: 36, fontSize: 9, color: t.onAccent, fill: t.accentWire, onTap: widget.onSendRecording),
              ],
            ),
          ),
        ),
        const SizedBox(height: 6),
        Text(
          'slide left to cancel · lift to keep recording',
          style: TextStyle(fontFamily: fontMono, fontSize: 9.5, height: 12 / 9.5, color: t.textMeta),
        ),
      ],
    );
  }

  Widget _row(CatenaryTokens t) {
    final offline = widget.connection.offline;
    final terminal = widget.connection.isTerminal;
    final drafted = _controller.text.trim().isNotEmpty;
    final hint = terminal
        ? 'This device cannot send'
        : offline
            ? 'Sends when reconnected'
            : 'Message';
    final Widget primary;
    if (terminal) {
      // Nothing sends, so the one accent does not sit on this button.
      primary = _Square('SEND', color: t.textDisabled, fill: t.surfaceBase, border: t.lineInner, onTap: null);
    } else if (offline) {
      primary = _Square('QUEUE', fontSize: 9, color: t.onAccent, fill: t.accentQueue, onTap: drafted ? widget.onSend : null);
    } else if (drafted) {
      primary = _Square('SEND', color: t.onAccent, fill: t.accentWire, onTap: widget.onSend);
    } else {
      primary = _Square('REC', color: t.onAccent, fill: t.accentWire, onTap: widget.onRecord);
    }
    return Row(
      crossAxisAlignment: CrossAxisAlignment.end,
      children: [
        // Uploads can't be queued safely, so ADD dims when offline.
        _Square(
          'ADD',
          color: offline ? t.textDisabled : t.textMeta,
          border: offline ? t.lineInner : t.lineEdge,
          onTap: offline ? null : widget.onAttach,
        ),
        const SizedBox(width: 10),
        Expanded(
          child: Container(
            constraints: const BoxConstraints(minHeight: 44),
            alignment: Alignment.centerLeft,
            decoration: BoxDecoration(
              color: t.surfaceBase,
              border: Border.all(color: t.lineEdge),
              borderRadius: BorderRadius.circular(CatenaryMetrics.radiusInput),
            ),
            child: TextField(
              key: const ValueKey('composer-input'),
              controller: _controller,
              minLines: 1,
              maxLines: 6,
              cursorColor: t.accentWire,
              cursorWidth: 1,
              style: TextStyle(fontFamily: fontSans, fontSize: 15, height: 24 / 15, color: t.textPrimary),
              decoration: InputDecoration(
                isCollapsed: true,
                hintText: hint,
                hintStyle: TextStyle(
                  fontFamily: fontSans,
                  fontSize: offline ? 14 : 15,
                  height: 24 / (offline ? 14 : 15),
                  color: offline ? t.textSecondary : t.textMeta,
                ),
                border: InputBorder.none,
                enabledBorder: InputBorder.none,
                focusedBorder: InputBorder.none,
                contentPadding: const EdgeInsets.symmetric(horizontal: 12, vertical: 9),
              ),
            ),
          ),
        ),
        const SizedBox(width: 10),
        KeyedSubtree(key: const ValueKey('composer-primary'), child: primary),
      ],
    );
  }
}

/// A square button with a mono label: the 44px targets flanking the field.
class _Square extends StatelessWidget {
  const _Square(this.label, {required this.color, required this.onTap, this.fill, this.border, this.size = 44, this.fontSize = 9.5});

  final String label;
  final Color color;
  final Color? fill;
  final Color? border;
  final double size;
  final double fontSize;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    return GestureDetector(
      behavior: HitTestBehavior.opaque,
      onTap: onTap,
      child: Container(
        width: size,
        height: size,
        alignment: Alignment.center,
        decoration: BoxDecoration(color: fill, border: border == null ? null : Border.all(color: border!)),
        child: Text(label, style: TextStyle(fontFamily: fontMono, fontSize: fontSize, letterSpacing: fontSize * 0.1, color: color)),
      ),
    );
  }
}

/// The level bars beside the recording clock. A PLACEHOLDER SHAPE, not the
/// recording's peaks: a voice note's real waveform is computed on the server
/// and arrives on the wire. This one only has to say "sound is being taken".
class _LevelsPainter extends CustomPainter {
  const _LevelsPainter(this.color);

  final Color color;

  @override
  void paint(Canvas canvas, Size size) {
    final paint = Paint()..color = color;
    const barWidth = 2.0, gap = 2.0;
    final bars = (size.width / (barWidth + gap)).floor();
    for (var i = 0; i < bars; i++) {
      final level = 0.25 + 0.75 * (0.5 + 0.5 * math.sin(i * 0.9) * math.cos(i * 0.23)).abs();
      final h = math.max(2.0, size.height * level);
      canvas.drawRect(Rect.fromLTWH(i * (barWidth + gap), (size.height - h) / 2, barWidth, h), paint);
    }
  }

  @override
  bool shouldRepaint(_LevelsPainter old) => old.color != color;
}
