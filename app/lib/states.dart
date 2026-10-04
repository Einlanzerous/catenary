// Every state the composer, the connection banner, the status column and the
// typing row can be in, on one screen (CANT-44). It is to those widgets what
// the token specimen is to the tokens: how each state is looked at on a
// device, in either theme, without waiting for a network to misbehave.
//
// The values are fixtures, and say so. Nothing here reads a transport.

import 'package:flutter/material.dart';

import 'metrics.dart';
import 'store/connection.dart';
import 'store/status.dart';
import 'tokens.dart';
import 'widgets/composer.dart';
import 'widgets/connection_banner.dart';
import 'widgets/status_label.dart';
import 'widgets/typing_row.dart';

/// The connection states the ticket names, and the two around them.
const connectionFixtures = <(String, ConnectionView)>[
  ('RECONNECTING', ConnectionView(kind: ConnectionKind.reconnecting, attempt: 3, retryIn: Duration(seconds: 8))),
  ('OFFLINE', ConnectionView(kind: ConnectionKind.offline)),
  ('RESYNCING', ConnectionView(kind: ConnectionKind.resyncing, synced: 1284, total: 12480, roomsPending: 2)),
  ('TERMINAL · CREDENTIAL', ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.credential)),
  ('TERMINAL · PROTOCOL', ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.protocol)),
];

/// The typing rule's cases, with the web client's own cast (web/smoke.ts).
const typingFixtures = <List<String>>[
  ['Nadia Okafor'],
  ['Nadia Okafor', 'Ted Marsh'],
  ['Nadia Okafor', 'Ted Marsh', 'Marek Novak'],
  ['Nadia Okafor', 'Ted Marsh', 'Marek Novak', 'Rosa Diaz'],
];

class StatesScreen extends StatelessWidget {
  const StatesScreen({super.key});

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    Widget section(String title) => Padding(
          padding: const EdgeInsets.fromLTRB(CatenaryMetrics.s4, CatenaryMetrics.s6, CatenaryMetrics.s4, CatenaryMetrics.s2),
          child: Text(title, style: CatenaryType.label.tracked.copyWith(color: t.textMeta)),
        );
    Widget status(MessageStatus s, {int? readBy, int memberCount = 0, bool retrying = false}) => Container(
          decoration: BoxDecoration(border: Border(bottom: BorderSide(color: t.lineFaint))),
          padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4, vertical: CatenaryMetrics.s2),
          child: Row(
            children: [
              Expanded(child: Text('sent it to your inbox instead', style: CatenaryType.body.style.copyWith(color: t.textPrimary))),
              StatusLabel(status: s, readBy: readBy, memberCount: memberCount, retrying: retrying),
            ],
          ),
        );
    return Scaffold(
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.only(bottom: CatenaryMetrics.s12),
          children: [
            Container(
              decoration: BoxDecoration(color: t.surfaceRail, border: Border(bottom: BorderSide(color: t.lineHair))),
              padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4, vertical: CatenaryMetrics.s3),
              child: Row(
                children: [
                  GestureDetector(
                    behavior: HitTestBehavior.opaque,
                    onTap: () => Navigator.of(context).maybePop(),
                    child: Text('← TOKENS', style: CatenaryType.meta.tracked.copyWith(color: t.accentWire)),
                  ),
                  const SizedBox(width: CatenaryMetrics.s4),
                  Text('States', style: CatenaryType.title.style.copyWith(color: t.textPrimary)),
                ],
              ),
            ),
            section('CONNECTION'),
            for (final (_, c) in connectionFixtures) ConnectionBanner(connection: c),
            section('STATUS'),
            status(MessageStatus.queued),
            status(MessageStatus.sending),
            status(MessageStatus.queued, retrying: true),
            status(MessageStatus.failed),
            status(MessageStatus.sent),
            status(MessageStatus.delivered),
            status(MessageStatus.read, readBy: 5, memberCount: 7),
            section('TYPING'),
            for (final names in typingFixtures) TypingRow(names: names),
            section('COMPOSER · LIVE'),
            const Composer(conversationName: 'Kitchen', connection: ConnectionView.live()),
            section('COMPOSER · OFFLINE, QUEUED'),
            const Composer(conversationName: 'Kitchen', connection: ConnectionView(kind: ConnectionKind.offline)),
            section('COMPOSER · RECORDING'),
            const Composer(
              conversationName: 'Kitchen',
              connection: ConnectionView.live(),
              recording: Duration(seconds: 14),
            ),
            section('COMPOSER · TERMINAL'),
            const Composer(
              conversationName: 'Kitchen',
              connection: ConnectionView(kind: ConnectionKind.terminal, terminal: TerminalCause.credential),
            ),
          ],
        ),
      ),
    );
  }
}
