// Delivery state at a phone's width (narrow call M2): status words become
// marks. THE ONLY PLACE GLYPHS ARE PERMITTED — `•` sent, `••` delivered, `••`
// in the accent for read. FAILED stays a word, in the fault color, because it
// needs a tap target next to it. A long-press gives the full word, which is
// `statusLabel` in store/status.dart.
//
// `queued` and `sending` are the outbox's own states, which the canvas's
// marks do not cover; they stay words too, in meta grey, so a message that
// has not left the device never looks like one that has.

import 'package:flutter/widgets.dart';

import '../metrics.dart';
import '../store/status.dart';
import '../tokens.dart';

class StatusMark extends StatelessWidget {
  const StatusMark({super.key, required this.status, this.retrying = false});

  final MessageStatus status;
  final bool retrying;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final (text, color, word) = switch (status) {
      MessageStatus.sent => ('•', t.textMeta, false),
      MessageStatus.delivered => ('••', t.textMeta, false),
      MessageStatus.read => ('••', t.accentWire, false),
      MessageStatus.failed => ('FAILED', t.signalFault, true),
      MessageStatus.queued || MessageStatus.sending => (statusLabel(status, retrying: retrying), t.textMeta, true),
    };
    return Text(
      text,
      semanticsLabel: statusLabel(status, retrying: retrying),
      style: word
          ? TextStyle(fontFamily: fontMono, fontSize: 9.5, height: 14 / 9.5, letterSpacing: 0.95, color: color)
          : TextStyle(fontFamily: fontMono, fontSize: 11, height: 14 / 11, color: color),
    );
  }
}
