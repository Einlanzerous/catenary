// The connection banner (deliberate call 09: it counts). Attempt number and
// retry countdown, not a spinner — you should be able to tell a stalled client
// from a working one without attaching a debugger. The twin of
// web/src/components/ConnectionBanner.vue, word for word where the words are
// the web client's.
//
// THE TERMINAL BRANCH IS THE HONESTY FLOOR (CANT-31 §6): nothing queued will
// drain from here, so it says so and offers no retry. A credential terminal
// offers RE-ENROLL; a protocol one does not, because re-enrolling cannot fix
// a client the server refuses to speak to at all.

import 'package:flutter/widgets.dart';

import '../metrics.dart';
import '../store/connection.dart';
import '../store/status.dart';
import '../tokens.dart';
import 'pulse.dart';

class ConnectionBanner extends StatelessWidget {
  const ConnectionBanner({super.key, required this.connection, this.onRetry, this.onReenroll});

  final ConnectionView connection;
  final VoidCallback? onRetry;
  final VoidCallback? onReenroll;

  @override
  Widget build(BuildContext context) {
    final c = connection;
    final banner = switch (c.kind) {
      ConnectionKind.live => null,
      ConnectionKind.reconnecting => _Lost(
          pulse: true,
          text: 'Connection lost — reconnecting',
          count: 'attempt ${c.attempt ?? 1} · retry in ${clock(c.retryIn ?? Duration.zero)}',
          action: 'RETRY NOW',
          onAction: onRetry,
        ),
      ConnectionKind.offline => _Lost(text: 'Offline — messages will queue', action: 'RECONNECT', onAction: onRetry),
      ConnectionKind.terminal => c.terminal == TerminalCause.protocol
          ? const _Lost(
              text: 'This app cannot talk to the server — update it, or report a bug. Nothing sends until then.',
              trailing: 'PROTOCOL',
            )
          : _Lost(
              text: 'This device is no longer signed in — re-enroll it to reconnect. Nothing sends until then.',
              action: 'RE-ENROLL',
              onAction: onReenroll,
              trailing: 'CREDENTIAL',
            ),
      ConnectionKind.resyncing => _CatchUp(connection: c),
    };
    final journal = c.journalError;
    if (banner == null && journal == null) return const SizedBox.shrink();
    return Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        ?banner,
        // Beside any of the above, never instead of it. The journal does not
        // fall back to memory when a write fails, so what is on screen may be
        // missing what that write carried; this says so until one lands.
        if (journal != null)
          _Lost(
            text: 'This device could not save messages it was sent — what is shown may be incomplete until a save succeeds.',
            trailing: journal,
          ),
      ],
    );
  }
}

class _Lost extends StatelessWidget {
  const _Lost({required this.text, this.pulse = false, this.count, this.action, this.onAction, this.trailing});

  final String text;
  final bool pulse;
  final String? count;
  final String? action;
  final VoidCallback? onAction;
  final String? trailing;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final dot = Container(width: 6, height: 6, color: t.accentWire);
    final meta = CatenaryType.meta.style.copyWith(fontSize: 10.5, color: t.accentDim);
    return Container(
      decoration: BoxDecoration(color: t.accentWash, border: Border(bottom: BorderSide(color: t.accentRule))),
      padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4, vertical: 9),
      child: Row(
        children: [
          pulse ? Pulse(period: const Duration(milliseconds: 1200), child: dot) : dot,
          const SizedBox(width: 10),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(text, style: CatenaryType.secondary.style.copyWith(fontSize: 12.5, height: 18 / 12.5, color: t.textPrimary)),
                // On a narrow screen the count sits under the sentence rather
                // than beside it: progress in this product is always numeric,
                // so it is never the thing that is dropped.
                if (count != null) Text(count!, style: meta.copyWith(fontFeatures: const [FontFeature.tabularFigures()])),
              ],
            ),
          ),
          if (action != null) ...[
            const SizedBox(width: 10),
            GestureDetector(
              behavior: HitTestBehavior.opaque,
              onTap: onAction,
              child: Padding(
                padding: const EdgeInsets.symmetric(vertical: CatenaryMetrics.s2),
                child: Text(action!, style: CatenaryType.label.tracked.copyWith(color: t.accentWire)),
              ),
            ),
          ],
          if (trailing != null) ...[
            const SizedBox(width: 10),
            Text(trailing!, style: meta.copyWith(letterSpacing: 10.5 * trackLabel)),
          ],
        ],
      ),
    );
  }
}

class _CatchUp extends StatelessWidget {
  const _CatchUp({required this.connection});

  final ConnectionView connection;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final c = connection;
    final total = c.total;
    final fraction = total == null || total == 0 ? 0.0 : ((c.synced ?? 0) / total).clamp(0.0, 1.0);
    return Container(
      decoration: BoxDecoration(color: t.accentWashSoft, border: Border(bottom: BorderSide(color: t.lineHair))),
      padding: const EdgeInsets.fromLTRB(CatenaryMetrics.s4, 10, CatenaryMetrics.s4, 11),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          Row(
            children: [
              Container(width: 6, height: 6, color: t.accentWire),
              const SizedBox(width: 12),
              // The sentence and its count stack on a phone's width, as the
              // lost banner's do: the number is never what gets dropped.
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text('Reconnected — catching up',
                        style: CatenaryType.secondary.style.copyWith(fontSize: 12.5, height: 18 / 12.5, color: t.textPrimary)),
                    // Only when there is a number to show: a `0 / 0` would be
                    // a claim.
                    if (total != null)
                      Text(
                        '${_grouped(c.synced ?? 0)} / ${_grouped(total)} messages',
                        style: CatenaryType.meta.style.copyWith(
                          fontSize: 10.5,
                          color: t.textSecondary,
                          fontFeatures: const [FontFeature.tabularFigures()],
                        ),
                      ),
                  ],
                ),
              ),
              if (c.roomsPending != null) ...[
                const SizedBox(width: 12),
                Text('${c.roomsPending} ROOMS PENDING',
                    style: CatenaryType.label.style.copyWith(letterSpacing: 1, color: t.textMeta)),
              ],
            ],
          ),
          const SizedBox(height: 10),
          SizedBox(
            height: 2,
            child: ColoredBox(
              color: t.lineEdge,
              child: FractionallySizedBox(
                key: const ValueKey('catchup-progress'),
                alignment: Alignment.centerLeft,
                widthFactor: fraction,
                child: ColoredBox(color: t.accentWire),
              ),
            ),
          ),
        ],
      ),
    );
  }
}

/// `12,480`: thousands grouped, as the web client's `toLocaleString('en-US')`.
String _grouped(int n) {
  final digits = n.toString();
  final out = StringBuffer();
  for (var i = 0; i < digits.length; i++) {
    if (i > 0 && (digits.length - i) % 3 == 0) out.write(',');
    out.write(digits[i]);
  }
  return out.toString();
}
