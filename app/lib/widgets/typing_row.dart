// The typing indicator: the thread's own last row, in the message column, not
// a strip under the composer (deliberate call 10a). The area you type in
// stays the bottom of the window, and the indicator appears exactly where the
// message will. Who it names is `typingLabel` in store/typing.dart.

import 'package:flutter/widgets.dart';

import '../metrics.dart';
import '../store/typing.dart';
import '../tokens.dart';
import 'pulse.dart';

class TypingRow extends StatelessWidget {
  const TypingRow({super.key, required this.names});

  /// The display names of the people typing, in the order each started.
  final List<String> names;

  @override
  Widget build(BuildContext context) {
    final label = typingLabel(names);
    if (label == null) return const SizedBox.shrink();
    final t = CatenaryTokens.of(context);
    return Padding(
      padding: const EdgeInsets.fromLTRB(CatenaryMetrics.s4, 10, CatenaryMetrics.s4, 2),
      child: Row(
        children: [
          Pulse(child: Container(width: 7, height: 7, color: t.accentWire)),
          const SizedBox(width: 9),
          Flexible(
            child: Text(
              label,
              overflow: TextOverflow.ellipsis,
              style: CatenaryType.secondary.style.copyWith(color: t.textSecondary),
            ),
          ),
          const SizedBox(width: 9),
          for (var i = 0; i < 3; i++) ...[
            if (i > 0) const SizedBox(width: 3),
            Pulse(
              period: const Duration(milliseconds: 1200),
              delay: Duration(milliseconds: 200 * i),
              child: Container(width: 3, height: 3, color: t.textMeta),
            ),
          ],
        ],
      ),
    );
  }
}
