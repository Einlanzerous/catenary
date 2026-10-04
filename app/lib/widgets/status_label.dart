// A message's status, as words, in a fixed-width column so nothing reflows as
// the state advances. The twin of web/src/components/StatusLabel.vue; the
// wording is `statusLabel` in store/status.dart.

import 'package:flutter/widgets.dart';

import '../metrics.dart';
import '../store/status.dart';
import '../tokens.dart';
import 'pulse.dart';

class StatusLabel extends StatelessWidget {
  const StatusLabel({super.key, required this.status, this.readBy, this.memberCount = 0, this.retrying = false});

  final MessageStatus status;
  final int? readBy;
  final int memberCount;
  final bool retrying;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final style = CatenaryType.label.style.copyWith(
      fontSize: 9.5,
      letterSpacing: 9.5 * 0.12,
      // Red belongs to failure alone. READ stays meta grey, as the canvas's
      // thread screens render it.
      color: status == MessageStatus.failed ? t.signalFault : t.textMeta,
    );
    return SizedBox(
      width: CatenaryMetrics.statusW,
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.end,
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            statusLabel(status, readBy: readBy, memberCount: memberCount, retrying: retrying),
            textAlign: TextAlign.right,
            style: style,
          ),
          // Hairline sweep, no spinner.
          if (status == MessageStatus.sending)
            Padding(
              padding: const EdgeInsets.only(top: 6),
              child: Sweep(track: t.lineEdge, bar: t.accentWire),
            ),
        ],
      ),
    );
  }
}
