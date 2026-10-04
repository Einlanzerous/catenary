// The thread — canvas frame 02, `Catenary Mobile.dc.html`.
//
// NO BUBBLES. A message is a line of a document: an author's run opens with
// a header — an 18px tile, the name, the time — and continuation lines drop
// it (narrow call M1: with no room for the desktop's 72px gutter, the time
// moves inline and appears on group headers only). Your own messages are a
// one-step surface lift and an accent "You", so a long thread reads as a
// document and not as a chat.
//
// Body text stays 15px (M3): density comes from 16px horizontal padding and a
// 24px line, never from smaller type. Status is a mark at the right of your
// own group's header (M2).
//
// THE HEADER READS `7 MEMBERS · TLS`, NOT E2E. The server can read these
// messages, and a badge asserting otherwise is the one claim this surface
// must not make.

import 'package:flutter/material.dart';

import 'metrics.dart';
import 'store/connection.dart';
import 'store/conversation.dart';
import 'store/status.dart';
import 'store/when.dart';
import 'tokens.dart';
import 'widgets/composer.dart';
import 'widgets/connection_banner.dart';
import 'widgets/glyph.dart';
import 'widgets/status_mark.dart';
import 'widgets/typing_row.dart';
import 'widgets/waveform.dart';

class ThreadScreen extends StatelessWidget {
  const ThreadScreen({super.key, required this.conversation, required this.connection, this.onBack, this.onRetry});

  final ConversationView conversation;
  final ConnectionView connection;
  final VoidCallback? onBack;
  final VoidCallback? onRetry;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final c = conversation;
    final rows = threadRows(c);
    final mono = TextStyle(fontFamily: fontMono, color: t.textMeta);
    return Scaffold(
      backgroundColor: t.surfaceBase,
      body: SafeArea(
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Container(
              height: 48,
              decoration: BoxDecoration(
                color: t.surfaceRail,
                border: Border(bottom: BorderSide(color: t.lineHair)),
              ),
              child: Row(
                children: [
                  // 44px wide: the back affordance is the pane's only way out.
                  GestureDetector(
                    key: const ValueKey('thread-back'),
                    behavior: HitTestBehavior.opaque,
                    onTap: onBack ?? () => Navigator.of(context).maybePop(),
                    child: SizedBox(
                      width: 44,
                      height: 48,
                      child: Center(
                        child: Text('‹', style: mono.copyWith(fontSize: 13, color: t.accentWire)),
                      ),
                    ),
                  ),
                  Expanded(
                    child: Column(
                      mainAxisAlignment: MainAxisAlignment.center,
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text(
                          c.name,
                          maxLines: 1,
                          overflow: TextOverflow.ellipsis,
                          style: TextStyle(
                            fontFamily: fontSans,
                            fontSize: 14.5,
                            height: 19 / 14.5,
                            fontWeight: FontWeight.w600,
                            color: t.textPrimary,
                          ),
                        ),
                        Text(
                          c.subtitle,
                          key: const ValueKey('thread-subtitle'),
                          style: mono.copyWith(fontSize: 9.5, height: 13 / 9.5, letterSpacing: 0.95),
                        ),
                      ],
                    ),
                  ),
                  GlyphIcon(Glyph.search, color: t.textMeta),
                  const SizedBox(width: 14),
                  GlyphIcon(Glyph.more, color: t.textMeta),
                  const SizedBox(width: CatenaryMetrics.s4),
                ],
              ),
            ),
            ConnectionBanner(connection: connection, onRetry: onRetry),
            Expanded(
              // The newest message is what a thread is opened for, so the list
              // grows from the bottom; a thread shorter than the screen still
              // starts at the top, under its header.
              child: Align(
                alignment: Alignment.topCenter,
                child: ListView(
                  reverse: true,
                  shrinkWrap: true,
                  padding: const EdgeInsets.only(top: 10),
                  children: [
                    // The typing indicator is the thread's own last row (10a).
                    TypingRow(names: c.typing),
                    for (final row in rows.reversed)
                      switch (row) {
                        DayRow(:final day) => _Rule(label: dayLabel(day), color: t.textMeta, line: t.lineHair, top: 4, bottom: 12),
                        NewRow(:final count) => _Rule(
                          key: const ValueKey('thread-new'),
                          label: '$count NEW',
                          color: t.accentWire,
                          line: t.accentRule,
                          top: 14,
                          bottom: 10,
                        ),
                        GroupRow() => MessageGroup(group: row, memberCount: c.memberCount),
                      },
                  ],
                ),
              ),
            ),
            Composer(connection: connection),
          ],
        ),
      ),
    );
  }
}

/// A labelled hairline: the date separator, and the "N NEW" rule.
class _Rule extends StatelessWidget {
  const _Rule({super.key, required this.label, required this.color, required this.line, required this.top, required this.bottom});

  final String label;
  final Color color;
  final Color line;
  final double top;
  final double bottom;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: EdgeInsets.fromLTRB(CatenaryMetrics.s4, top, CatenaryMetrics.s4, bottom),
      child: Row(
        children: [
          Text(
            label,
            style: TextStyle(fontFamily: fontMono, fontSize: 10, height: 1.2, letterSpacing: 1.2, color: color),
          ),
          const SizedBox(width: 12),
          Expanded(child: Container(height: 1, color: line)),
        ],
      ),
    );
  }
}

/// One author's run of messages.
class MessageGroup extends StatelessWidget {
  const MessageGroup({super.key, required this.group, required this.memberCount});

  final GroupRow group;
  final int memberCount;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final first = group.first;
    final mine = first.mine;
    // The mark is the group's newest message's: it is the one still moving.
    final newest = group.messages.last;
    final failed = mine && newest.status == MessageStatus.failed;
    return Container(
      key: ValueKey('group-${first.id}'),
      margin: EdgeInsets.only(top: mine ? 8 : 0, bottom: mine ? 6 : 0),
      decoration: BoxDecoration(
        // Own messages: a one-step surface lift, never a bubble. A failed
        // send carries the fault color on its leading edge.
        color: mine ? t.surfaceLift : null,
        border: failed ? Border(left: BorderSide(color: t.signalFault, width: 2)) : null,
      ),
      padding: EdgeInsets.fromLTRB(failed ? 14 : 16, mine ? 12 : 2, 16, mine ? 8 : 2),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              Expanded(
                child: Row(
                  children: [
                    Container(
                      width: 18,
                      height: 18,
                      alignment: Alignment.center,
                      color: mine ? t.accentWire : t.surfaceAvatar,
                      child: Text(
                        initials(first.authorName),
                        style: TextStyle(fontFamily: fontMono, fontSize: 8.5, height: 1, color: mine ? t.onAccent : t.textBright),
                      ),
                    ),
                    const SizedBox(width: 8),
                    Flexible(
                      child: Text(
                        mine ? 'You' : first.authorName,
                        maxLines: 1,
                        overflow: TextOverflow.ellipsis,
                        style: TextStyle(
                          fontFamily: fontSans,
                          fontSize: 13,
                          height: 18 / 13,
                          fontWeight: FontWeight.w600,
                          color: mine ? t.accentWire : t.textPrimary,
                        ),
                      ),
                    ),
                    const SizedBox(width: 8),
                    Text(
                      clockTime(first.at),
                      style: TextStyle(
                        fontFamily: fontMono,
                        fontSize: 10,
                        height: 14 / 10,
                        color: t.textDim,
                        fontFeatures: const [FontFeature.tabularFigures()],
                      ),
                    ),
                  ],
                ),
              ),
              if (mine) StatusMark(key: const ValueKey('group-status'), status: newest.status),
            ],
          ),
          for (final m in group.messages) ..._body(t, m),
          if (failed) _FailedActions(reason: newest.failure),
        ],
      ),
    );
  }

  List<Widget> _body(CatenaryTokens t, ThreadMessage m) {
    final text = m.text;
    final voice = m.voice;
    final image = m.image;
    return [
      if (voice != null)
        Padding(
          padding: const EdgeInsets.only(top: 8),
          child: VoiceNoteBlock(voice: voice),
        ),
      if (text != null && text.isNotEmpty)
        Padding(
          padding: EdgeInsets.only(top: voice != null ? 6 : 3),
          child: Text(
            text,
            style: TextStyle(fontFamily: fontSans, fontSize: 15, height: 24 / 15, color: t.textPrimary),
          ),
        ),
      if (image != null)
        Padding(
          padding: const EdgeInsets.only(top: 8, bottom: 4),
          child: ImageBlock(image: image),
        ),
    ];
  }
}

/// RETRY and DELETE on a failed send, 44px tall (M11): the one place the
/// narrow layout spends more vertical space than desktop, because a mis-tap
/// here deletes a message.
class _FailedActions extends StatelessWidget {
  const _FailedActions({this.reason});

  final String? reason;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final label = TextStyle(fontFamily: fontMono, fontSize: 10, letterSpacing: 1.2);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (reason != null)
          Padding(
            padding: const EdgeInsets.only(top: 4),
            child: Text(
              reason!,
              style: TextStyle(fontFamily: fontMono, fontSize: 10, height: 14 / 10, color: t.signalFault),
            ),
          ),
        const SizedBox(height: 9),
        Row(
          children: [
            Container(
              height: 44,
              alignment: Alignment.center,
              color: t.signalFault,
              padding: const EdgeInsets.symmetric(horizontal: 14),
              child: Text('RETRY', style: label.copyWith(color: t.onAccent)),
            ),
            const SizedBox(width: 8),
            Container(
              height: 44,
              alignment: Alignment.center,
              decoration: BoxDecoration(border: Border.all(color: t.surfaceAvatar)),
              padding: const EdgeInsets.symmetric(horizontal: 14),
              child: Text('DELETE', style: label.copyWith(color: t.textSecondary)),
            ),
          ],
        ),
      ],
    );
  }
}

/// A voice note: play, the server's waveform, the length — and under a
/// hairline, the transcript, clamped to two lines until it is expanded (M9).
/// `EXPAND · 96 W` is counted from the transcript's own text, so it cannot
/// lie.
class VoiceNoteBlock extends StatefulWidget {
  const VoiceNoteBlock({super.key, required this.voice});

  final VoiceNote voice;

  @override
  State<VoiceNoteBlock> createState() => _VoiceNoteBlockState();
}

class _VoiceNoteBlockState extends State<VoiceNoteBlock> {
  var _expanded = false;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final v = widget.voice;
    final transcript = v.transcript;
    final small = TextStyle(fontFamily: fontMono, fontSize: 9, height: 12 / 9);
    return Container(
      decoration: BoxDecoration(
        color: t.surfaceLift,
        border: Border.all(color: t.lineEdge),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Padding(
            padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
            child: Row(
              children: [
                Container(
                  width: 32,
                  height: 32,
                  alignment: Alignment.center,
                  color: t.accentWire,
                  child: GlyphIcon(Glyph.play, color: t.onAccent, size: 10),
                ),
                const SizedBox(width: 10),
                Expanded(
                  child: Waveform(peaks: v.peaks, rest: transcript == null ? t.waveRestDim : t.waveRest, on: t.accentWire),
                ),
                const SizedBox(width: 10),
                Text(
                  clock(v.duration),
                  style: TextStyle(
                    fontFamily: fontMono,
                    fontSize: 10.5,
                    height: 14 / 10.5,
                    color: t.textSecondary,
                    fontFeatures: const [FontFeature.tabularFigures()],
                  ),
                ),
              ],
            ),
          ),
          Container(
            decoration: BoxDecoration(
              border: Border(top: BorderSide(color: t.lineInner)),
            ),
            padding: const EdgeInsets.fromLTRB(12, 9, 12, 11),
            child: transcript == null
                ? Text('TRANSCRIBING', style: small.copyWith(letterSpacing: 1.26, color: t.accentDim))
                : Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      GestureDetector(
                        key: const ValueKey('transcript-toggle'),
                        behavior: HitTestBehavior.opaque,
                        onTap: () => setState(() => _expanded = !_expanded),
                        child: Row(
                          children: [
                            Text('TRANSCRIPT', style: small.copyWith(letterSpacing: 1.26, color: t.textMeta)),
                            const SizedBox(width: 8),
                            Expanded(child: Container(height: 1, color: t.lineInner)),
                            const SizedBox(width: 8),
                            Text(
                              _expanded ? 'COLLAPSE' : 'EXPAND · ${countWords(transcript)} W',
                              style: small.copyWith(letterSpacing: 0.9, color: t.accentWire),
                            ),
                          ],
                        ),
                      ),
                      const SizedBox(height: 6),
                      Text(
                        transcript,
                        maxLines: _expanded ? null : 2,
                        overflow: _expanded ? TextOverflow.visible : TextOverflow.ellipsis,
                        style: TextStyle(fontFamily: fontSans, fontSize: 13, height: 19 / 13, color: t.textSecondary),
                      ),
                    ],
                  ),
          ),
        ],
      ),
    );
  }
}

/// An image, edge to edge within the 16px padding, capped at 320 tall (M10) —
/// a portrait photo should not eat a whole screen of scroll. Until its bytes
/// are loaded this is its placeholder, at the photo's own aspect ratio so
/// nothing moves when they arrive, over the filename and pixel size.
class ImageBlock extends StatelessWidget {
  const ImageBlock({super.key, required this.image});

  final ImageAttachment image;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final footer = TextStyle(fontFamily: fontMono, fontSize: 9, height: 12 / 9, color: t.textMeta);
    return Container(
      decoration: BoxDecoration(
        color: t.surfaceLift,
        border: Border.all(color: t.lineEdge),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          ConstrainedBox(
            constraints: const BoxConstraints(maxHeight: 320),
            child: AspectRatio(
              aspectRatio: image.width / image.height,
              child: ClipRect(
                child: CustomPaint(
                  painter: _HatchPainter(t.surfaceRaised, t.surfaceLift),
                  child: Center(
                    child: Text(
                      'PHOTO',
                      style: TextStyle(fontFamily: fontMono, fontSize: 10, letterSpacing: 1.2, color: t.textPlaceholder),
                    ),
                  ),
                ),
              ),
            ),
          ),
          Container(
            decoration: BoxDecoration(
              border: Border(top: BorderSide(color: t.lineInner)),
            ),
            padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 5),
            child: Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                Text(image.filename, style: footer),
                Text('${image.width}×${image.height}', style: footer.copyWith(fontFeatures: const [FontFeature.tabularFigures()])),
              ],
            ),
          ),
        ],
      ),
    );
  }
}

/// 8px diagonal bands in two surface tokens: the canvas's placeholder fill.
class _HatchPainter extends CustomPainter {
  const _HatchPainter(this.a, this.b);

  final Color a;
  final Color b;

  @override
  void paint(Canvas canvas, Size size) {
    canvas.drawRect(Offset.zero & size, Paint()..color = b);
    final band = Paint()
      ..color = a
      ..strokeWidth = 8 * 0.7071
      ..style = PaintingStyle.stroke;
    const step = 16.0 * 1.4142;
    for (var x = -size.height; x < size.width + size.height; x += step) {
      canvas.drawLine(Offset(x, size.height), Offset(x + size.height, 0), band);
    }
  }

  @override
  bool shouldRepaint(_HatchPainter old) => old.a != a || old.b != b;
}
