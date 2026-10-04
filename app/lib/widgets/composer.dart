// The composer: the bottom of the window, and the only place a message
// starts. The twin of web/src/components/Composer.vue, with the keyboard
// hints a phone has no keys for left out.
//
// IT NEVER PRETENDS A MESSAGE LEFT THE BUILDING. Off a live session the
// primary action reads QUEUE, in the dimmer copper, and the placeholder says
// the message will send when reconnected. ATTACH dims, because an upload
// cannot be queued safely (CANT-36). On a terminal client nothing composed
// now will drain, so the composer does not offer to queue it and the one
// accent does not sit on a button that cannot work (Invariant 3).
//
// RECORDING IS AMBER, NOT RED: red belongs to failure alone.

import 'dart:math' as math;

import 'package:flutter/material.dart';

import '../metrics.dart';
import '../store/connection.dart';
import '../store/status.dart';
import '../tokens.dart';
import 'pulse.dart';

class Composer extends StatelessWidget {
  const Composer({
    super.key,
    required this.conversationName,
    required this.connection,
    this.controller,
    this.recording,
    this.onSend,
    this.onAttach,
    this.onRecord,
    this.onCancelRecording,
    this.onSendRecording,
  });

  final String conversationName;
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
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return Container(
      decoration: BoxDecoration(color: t.surfaceRail, border: Border(top: BorderSide(color: t.lineHair))),
      padding: const EdgeInsets.fromLTRB(CatenaryMetrics.s4, 10, CatenaryMetrics.s4, 12),
      child: recording != null ? _recording(t, recording!) : _field(t),
    );
  }

  Widget _recording(CatenaryTokens t, Duration elapsed) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      mainAxisSize: MainAxisSize.min,
      children: [
        Container(
          key: const ValueKey('composer-recording'),
          decoration: BoxDecoration(
            color: t.surfaceBase,
            border: Border.all(color: t.accentWire),
            borderRadius: BorderRadius.circular(CatenaryMetrics.radiusInput),
          ),
          padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
          child: Row(
            children: [
              Pulse(child: Container(width: 8, height: 8, color: t.accentWire)),
              const SizedBox(width: 14),
              Text(
                clock(elapsed),
                style: TextStyle(
                  fontFamily: fontMono,
                  fontSize: 13,
                  color: t.textPrimary,
                  fontFeatures: const [FontFeature.tabularFigures()],
                ),
              ),
              const SizedBox(width: 14),
              Expanded(child: SizedBox(height: 26, child: CustomPaint(painter: _LevelsPainter(t.accentWire)))),
              const SizedBox(width: 14),
              _Ghost('CANCEL', color: t.textMeta, onTap: onCancelRecording),
              const SizedBox(width: CatenaryMetrics.s4),
              _Primary('SEND', color: t.onAccent, fill: t.accentWire, onTap: onSendRecording),
            ],
          ),
        ),
      ],
    );
  }

  Widget _field(CatenaryTokens t) {
    final offline = connection.offline;
    final terminal = connection.isTerminal;
    final placeholder = terminal
        ? 'Message $conversationName — this device cannot send; see the banner'
        : offline
            ? 'Message $conversationName — will send when reconnected'
            : 'Message $conversationName';
    return Container(
      decoration: BoxDecoration(
        color: t.surfaceBase,
        border: Border.all(color: t.lineEdge),
        borderRadius: BorderRadius.circular(CatenaryMetrics.radiusInput),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          TextField(
            key: const ValueKey('composer-input'),
            controller: controller,
            minLines: 1,
            maxLines: 8,
            style: CatenaryType.body.style.copyWith(height: 24 / 15, color: t.textPrimary),
            decoration: InputDecoration(
              hintText: placeholder,
              hintStyle: CatenaryType.body.style.copyWith(height: 24 / 15, color: t.textSecondary),
              hintMaxLines: 2,
              border: InputBorder.none,
              enabledBorder: InputBorder.none,
              focusedBorder: InputBorder.none,
              contentPadding: const EdgeInsets.fromLTRB(14, 11, 14, 4),
            ),
          ),
          Padding(
            padding: const EdgeInsets.fromLTRB(14, 6, 12, 9),
            child: Row(
              children: [
                // Uploads can't be queued safely, so ATTACH dims when offline.
                _Ghost('ATTACH', color: offline ? t.textDisabled : t.textMeta, onTap: offline ? null : onAttach),
                const SizedBox(width: CatenaryMetrics.s4),
                _Ghost('RECORD', color: terminal ? t.textDisabled : t.textMeta, onTap: terminal ? null : onRecord),
                const Spacer(),
                // Terminal: nothing sends, so the one accent does not sit on
                // this button.
                terminal
                    ? _Primary('SEND', color: t.textDisabled, fill: t.surfaceBase, onTap: null)
                    : offline
                        ? _Primary('QUEUE', color: t.onAccent, fill: t.accentQueue, onTap: onSend)
                        : _Primary('SEND', color: t.onAccent, fill: t.accentWire, onTap: onSend),
              ],
            ),
          ),
        ],
      ),
    );
  }
}

class _Ghost extends StatelessWidget {
  const _Ghost(this.label, {required this.color, required this.onTap});

  final String label;
  final Color color;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    return GestureDetector(
      behavior: HitTestBehavior.opaque,
      onTap: onTap,
      child: Padding(
        padding: const EdgeInsets.symmetric(vertical: 5),
        child: Text(label, style: CatenaryType.label.tracked.copyWith(color: color)),
      ),
    );
  }
}

class _Primary extends StatelessWidget {
  const _Primary(this.label, {required this.color, required this.fill, required this.onTap});

  final String label;
  final Color color;
  final Color fill;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    return GestureDetector(
      behavior: HitTestBehavior.opaque,
      onTap: onTap,
      child: Container(
        key: const ValueKey('composer-primary'),
        color: fill,
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 5),
        child: Text(label, style: CatenaryType.label.tracked.copyWith(color: color)),
      ),
    );
  }
}

/// The level bars beside the recording clock. A PLACEHOLDER SHAPE, not the
/// recording's peaks and not a port of the web client's seeded generator:
/// that generator overflows 2^53 and draws different bars in Dart than in
/// JavaScript, which is why a voice note's real waveform is computed on the
/// server. This one only has to say "sound is being taken".
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
