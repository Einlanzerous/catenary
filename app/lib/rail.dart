// The conversation list — canvas frame 01, `Catenary Mobile.dc.html`. At a
// phone's width the rail is its own full-screen pane (the breakpoint is 900).
//
// One row anatomy for rooms and directs: name and stamp on the first line,
// preview and one trailing marker on the second. The accent is spent on what
// is live and on nothing else: the open row's 3px bar, an unread row's stamp
// and count, and a read receipt's two dots. A row that has gone quiet dims
// its name — never bold-for-unread (deliberate call 05).
//
// Everything a row says is derived (store/conversation.dart): the stamp from
// `now`, the count from `firstUnreadSeq`, the preview from the last message.

import 'package:flutter/material.dart';

import 'metrics.dart';
import 'store/connection.dart';
import 'store/conversation.dart';
import 'store/status.dart';
import 'store/when.dart';
import 'tokens.dart';
import 'widgets/connection_banner.dart';
import 'widgets/glyph.dart';
import 'widgets/logomark.dart';
import 'widgets/status_mark.dart';

/// The three tabs of the narrow layout (call M6).
enum RailTab { chats, search, you }

class RailScreen extends StatelessWidget {
  const RailScreen({
    super.key,
    required this.conversations,
    required this.now,
    required this.connection,
    required this.myInitials,
    this.openId,
    this.onOpen,
    this.onRetry,
    this.onReenroll,
    this.onYou,
  });

  final List<ConversationView> conversations;
  final DateTime now;
  final ConnectionView connection;
  final String myInitials;

  /// The conversation last opened, which carries the accent bar.
  final String? openId;
  final ValueChanged<ConversationView>? onOpen;

  /// The banner's two actions, passed through to it.
  final VoidCallback? onRetry;
  final VoidCallback? onReenroll;
  final VoidCallback? onYou;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final rooms = conversations.where((c) => c.kind == ConversationKind.group).toList();
    final directs = conversations.where((c) => c.kind == ConversationKind.direct).toList();
    final mono = TextStyle(fontFamily: fontMono, color: t.textMeta);
    return Scaffold(
      backgroundColor: t.surfaceRail,
      body: SafeArea(
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Container(
              height: 48,
              decoration: BoxDecoration(border: Border(bottom: BorderSide(color: t.lineHair))),
              padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s4),
              child: Row(
                children: [
                  const Logomark(),
                  const SizedBox(width: 10),
                  Text('CATENARY', style: mono.copyWith(fontSize: 12, letterSpacing: 2.4, color: t.textPrimary)),
                  const Spacer(),
                  // Drawn disabled: search is not built in this epic.
                  GlyphIcon(Glyph.search, color: t.textDisabled),
                  const SizedBox(width: CatenaryMetrics.s4),
                  Text(myInitials, style: mono.copyWith(fontSize: 11, letterSpacing: 1.32)),
                ],
              ),
            ),
            ConnectionBanner(connection: connection, onRetry: onRetry, onReenroll: onReenroll),
            Expanded(
              child: ListView(
                padding: EdgeInsets.zero,
                children: [
                  if (rooms.isNotEmpty) _SectionHeader('ROOMS', rooms.length, first: true),
                  for (final c in rooms) _row(c),
                  if (directs.isNotEmpty) _SectionHeader('DIRECT', directs.length, first: rooms.isEmpty),
                  for (final c in directs) _row(c),
                ],
              ),
            ),
            _TabBar(onYou: onYou),
          ],
        ),
      ),
    );
  }

  Widget _row(ConversationView c) => ConversationRow(
        key: ValueKey('rail-${c.id}'),
        conversation: c,
        now: now,
        open: c.id == openId,
        onTap: onOpen == null ? null : () => onOpen!(c),
      );
}

class _SectionHeader extends StatelessWidget {
  const _SectionHeader(this.title, this.count, {required this.first});

  final String title;
  final int count;
  final bool first;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return Padding(
      padding: EdgeInsets.fromLTRB(CatenaryMetrics.s4, first ? 14 : 18, CatenaryMetrics.s4, 6),
      child: Row(
        mainAxisAlignment: MainAxisAlignment.spaceBetween,
        children: [
          Text(title, style: TextStyle(fontFamily: fontMono, fontSize: 10, height: 1.2, letterSpacing: 1.6, color: t.textMeta)),
          Text(
            '$count',
            style: TextStyle(
              fontFamily: fontMono,
              fontSize: 10,
              height: 1.2,
              color: t.textDim,
              fontFeatures: const [FontFeature.tabularFigures()],
            ),
          ),
        ],
      ),
    );
  }
}

class ConversationRow extends StatelessWidget {
  const ConversationRow({super.key, required this.conversation, required this.now, this.open = false, this.onTap});

  final ConversationView conversation;
  final DateTime now;
  final bool open;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final c = conversation;
    final last = c.last;
    final mark = railMark(c, open: open);
    final unread = mark.kind == RailMarkKind.count;
    final quiet = isQuiet(c, now, open: open);
    return GestureDetector(
      behavior: HitTestBehavior.opaque,
      onTap: onTap,
      child: Container(
        color: open ? t.surfaceRaised : null,
        child: IntrinsicHeight(
          child: Row(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              Container(width: 3, color: open ? t.accentWire : null),
              Expanded(
                child: Padding(
                  padding: const EdgeInsets.fromLTRB(15, 11, 16, 12),
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Row(
                        crossAxisAlignment: CrossAxisAlignment.baseline,
                        textBaseline: TextBaseline.alphabetic,
                        children: [
                          Expanded(
                            child: Text(
                              c.name,
                              maxLines: 1,
                              overflow: TextOverflow.ellipsis,
                              style: TextStyle(
                                fontFamily: fontSans,
                                fontSize: 14,
                                height: 19 / 14,
                                fontWeight: FontWeight.w600,
                                color: quiet ? t.textSecondary : t.textPrimary,
                              ),
                            ),
                          ),
                          const SizedBox(width: 10),
                          if (last != null)
                            Text(
                              railStamp(last.at, now),
                              key: const ValueKey('rail-stamp'),
                              style: TextStyle(
                                fontFamily: fontMono,
                                fontSize: 10.5,
                                height: 14 / 10.5,
                                color: unread ? t.accentWire : t.textMeta,
                                fontFeatures: const [FontFeature.tabularFigures()],
                              ),
                            ),
                        ],
                      ),
                      const SizedBox(height: 3),
                      Row(
                        crossAxisAlignment: CrossAxisAlignment.baseline,
                        textBaseline: TextBaseline.alphabetic,
                        children: [
                          Expanded(
                            child: Text(
                              preview(c),
                              maxLines: 1,
                              overflow: TextOverflow.ellipsis,
                              style: TextStyle(
                                fontFamily: fontSans,
                                fontSize: 13,
                                height: 18 / 13,
                                color: unread
                                    ? t.textBright
                                    : quiet
                                        ? t.textMeta
                                        : t.textSecondary,
                              ),
                            ),
                          ),
                          if (mark.kind != RailMarkKind.none) ...[const SizedBox(width: 10), _Marker(mark)],
                        ],
                      ),
                    ],
                  ),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}

class _Marker extends StatelessWidget {
  const _Marker(this.mark);

  final RailMark mark;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    switch (mark.kind) {
      case RailMarkKind.count:
        return Container(
          key: const ValueKey('rail-unread'),
          color: t.accentWire,
          padding: const EdgeInsets.symmetric(horizontal: 5, vertical: 1),
          child: Text(
            '${mark.count}',
            style: TextStyle(
              fontFamily: fontMono,
              fontSize: 10,
              height: 13 / 10,
              fontWeight: FontWeight.w500,
              color: t.onAccent,
              fontFeatures: const [FontFeature.tabularFigures()],
            ),
          ),
        );
      case RailMarkKind.muted:
        return Text('MUTED', style: TextStyle(fontFamily: fontMono, fontSize: 9.5, height: 14 / 9.5, letterSpacing: 0.95, color: t.textDim));
      case RailMarkKind.status:
        return StatusMark(status: mark.status ?? MessageStatus.sent, retrying: mark.retrying);
      case RailMarkKind.none:
        return const SizedBox.shrink();
    }
  }
}

/// Call M6: search and profile have nowhere else to live at this width.
/// SEARCH is drawn and inert — search is not built in this epic, and a tab
/// that looked live would be a claim.
class _TabBar extends StatelessWidget {
  const _TabBar({this.onYou});

  final VoidCallback? onYou;

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    Widget tab(String label, {bool active = false, bool built = true, VoidCallback? onTap}) => Expanded(
          child: GestureDetector(
            behavior: HitTestBehavior.opaque,
            onTap: onTap,
            child: Column(
              mainAxisAlignment: MainAxisAlignment.center,
              children: [
                Text(
                  label,
                  style: TextStyle(
                    fontFamily: fontMono,
                    fontSize: 10,
                    height: 1.2,
                    letterSpacing: 1.4,
                    color: active
                        ? t.accentWire
                        : built
                            ? t.textMeta
                            : t.textDisabled,
                  ),
                ),
                const SizedBox(height: 4),
                Container(width: 18, height: 2, color: active ? t.accentWire : null),
              ],
            ),
          ),
        );
    return Container(
      height: 56,
      decoration: BoxDecoration(border: Border(top: BorderSide(color: t.lineHair))),
      padding: const EdgeInsets.symmetric(horizontal: CatenaryMetrics.s2),
      child: Row(
        children: [
          tab('CHATS', active: true),
          tab('SEARCH', built: false),
          tab('YOU', onTap: onYou),
        ],
      ),
    );
  }
}
