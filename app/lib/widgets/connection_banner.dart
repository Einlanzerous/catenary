// The connection banner (deliberate call 09: it counts). Attempt number and
// retry countdown, not a spinner — you should be able to tell a stalled client
// from a working one without attaching a debugger.
//
// AS THE NARROW CANVAS DRAWS IT (`Catenary Mobile.dc.html`, frames 01 and
// 04): one 40px row — a 6px dot, a short sentence, the count beside it, the
// action at the right. `Reconnecting · attempt 3 · 0:08 · RETRY`, `Offline —
// 2 queued · RETRY`, and `Catching up · 412 / 1,180` over a 2px track. The
// web client's longer sentences do not fit 390px and are not used here.
//
// THE TERMINAL BRANCH IS THE HONESTY FLOOR (CANT-31 §6) and is not on the
// canvas: nothing queued will drain from here, so it says so and offers no
// retry. A credential terminal offers RE-ENROLL; a protocol one does not,
// because re-enrolling cannot fix a client the server refuses to speak to.

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
    final queued = c.queued ?? 0;
    final banner = switch (c.kind) {
      ConnectionKind.live => null,
      ConnectionKind.reconnecting => _Lost(
          pulse: true,
          text: 'Reconnecting',
          count: 'attempt ${c.attempt ?? 1} · ${clock(c.retryIn ?? Duration.zero)}',
          action: 'RETRY',
          onAction: onRetry,
        ),
      ConnectionKind.offline => _Lost(
          pulse: true,
          // Only a number that is true: with nothing waiting it says what will
          // happen rather than "0 queued".
          text: queued > 0 ? 'Offline — $queued queued' : 'Offline — messages will queue',
          action: 'RETRY',
          onAction: onRetry,
        ),
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
    final mono = TextStyle(fontFamily: fontMono, height: 14 / 10.5, fontFeatures: const [FontFeature.tabularFigures()]);
    return Container(
      constraints: const BoxConstraints(minHeight: 40),
      decoration: BoxDecoration(color: t.accentWash, border: Border(bottom: BorderSide(color: t.accentRule))),
      padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4),
      child: Row(
        children: [
          pulse ? Pulse(period: const Duration(milliseconds: 1200), child: dot) : dot,
          const SizedBox(width: 9),
          Flexible(
            child: Padding(
              padding: const EdgeInsets.symmetric(vertical: 9),
              child: Text(text, style: TextStyle(fontFamily: fontSans, fontSize: 12.5, height: 18 / 12.5, color: t.textPrimary)),
            ),
          ),
          if (count != null) ...[
            const SizedBox(width: 9),
            Text(count!, style: mono.copyWith(fontSize: 10.5, color: t.accentDim)),
          ],
          const Spacer(),
          if (action != null)
            // A 44px-tall target for a 10px word (TAP TARGETS · 44 MIN).
            GestureDetector(
              behavior: HitTestBehavior.opaque,
              onTap: onAction,
              child: ConstrainedBox(
                constraints: const BoxConstraints(minHeight: 40, minWidth: 44),
                child: Align(
                  alignment: Alignment.centerRight,
                  widthFactor: 1,
                  child: Text(action!, style: mono.copyWith(fontSize: 10, letterSpacing: 1.2, color: t.accentWire)),
                ),
              ),
            ),
          if (trailing != null) ...[
            const SizedBox(width: 10),
            Text(trailing!, style: mono.copyWith(fontSize: 10, letterSpacing: 1.2, color: t.accentDim)),
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
      decoration: BoxDecoration(color: t.accentWashSoft, border: Border(bottom: BorderSide(color: t.lineEdge))),
      padding: const EdgeInsets.fromLTRB(CatenaryMetrics.s4, 10, CatenaryMetrics.s4, 10),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          Row(
            children: [
              Container(width: 6, height: 6, color: t.accentWire),
              const SizedBox(width: 9),
              Text('Catching up', style: TextStyle(fontFamily: fontSans, fontSize: 12.5, height: 18 / 12.5, color: t.textPrimary)),
              const Spacer(),
              // Progress in this product is always numeric — and only when
              // there is a number to show: a `0 / 0` would be a claim.
              if (total != null)
                Text(
                  '${_grouped(c.synced ?? 0)} / ${_grouped(total)}',
                  style: TextStyle(
                    fontFamily: fontMono,
                    fontSize: 10,
                    height: 14 / 10,
                    color: t.textSecondary,
                    fontFeatures: const [FontFeature.tabularFigures()],
                  ),
                ),
            ],
          ),
          const SizedBox(height: 9),
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

/// `1,180`: thousands grouped.
String _grouped(int n) {
  final digits = n.toString();
  final out = StringBuffer();
  for (var i = 0; i < digits.length; i++) {
    if (i > 0 && (digits.length - i) % 3 == 0) out.write(',');
    out.write(digits[i]);
  }
  return out.toString();
}
