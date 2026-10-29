// The link preview card in a thread row (CANT-325), built to the narrow
// canvas: `Catenary Link Preview.dc.html`, section 2C and the BUILD SPEC.
//
//   host + ↗      mono 10.5/14, text.secondary. The host is derived from the
//                 URL (store/link.dart), never from og:site_name
//   title         sans 14/20, 600, text.primary, two lines
//   description   sans 13/19, text.secondary, two lines
//   image         above the text, in a box whose height is known BEFORE PAINT:
//                 the card's width times the stored height over width,
//                 clamped 2:1 to 1:1 and capped at 240. The blurhash fills it
//                 first and the image crossfades over it in 160 ms, linear.
//                 No dimensions or no hash stored: the card is text-only.
//                 Never a guessed ratio. A failed image leaves the blurhash;
//                 nothing collapses and nothing reflows.
//
// The whole card is one target. There is no hover on touch; the pressed state
// is surface.raised with a line-strong border, and the focus ring is a 2px
// text.primary outline at offset 2 (CANT-332: the ring is text.primary and no
// token is added for it). Motion is colour only, 80 ms linear.
//
// Nothing on the card vouches for the page: no padlock, no check. ↗ only says
// "opens elsewhere". The full URL is in the long-press sheet.
//
// The redirect row: when the server says the fetch left the pasted site
// (`final_host`), the host line reads `youtu.be → youtube.com`, the arrow in
// text.meta and both hosts through the one display rule. Absent, the line is
// the pasted host alone. Same-site is the server's call, never the client's.

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../metrics.dart';
import '../store/conversation.dart';
import '../store/link.dart';
import '../tokens.dart';
import 'blurhash.dart';
import 'glyph.dart';

/// The widest a card gets; in the narrow column it is the column.
const double linkCardMaxWidth = 440;

class LinkCard extends StatefulWidget {
  const LinkCard({
    super.key,
    required this.link,
    required this.ground,
    this.image,
    this.onOpen,
  });

  final LinkPreviewView link;

  /// The card's ground: one step off the row it sits on, so surface.rail on
  /// another person's row and surface.base on your own.
  final Color ground;

  /// The card image's bytes, once something can fetch them. Until it loads,
  /// and if it never does, the blurhash stays.
  final ImageProvider? image;

  /// Opens the URL in the system browser.
  final void Function(String url)? onOpen;

  @override
  State<LinkCard> createState() => _LinkCardState();
}

class _LinkCardState extends State<LinkCard> {
  bool _pressed = false;
  bool _focused = false;

  void _open() {
    if (isWebUrl(widget.link.url)) widget.onOpen?.call(widget.link.url);
  }

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    final link = widget.link;
    final title = link.cleanTitle;
    final description = link.cleanDescription;
    final hostStyle = TextStyle(fontFamily: fontMono, fontSize: 10.5, height: 14 / 10.5, color: t.textSecondary);
    final card = LayoutBuilder(
      builder: (context, box) {
        final width = box.maxWidth.isFinite ? box.maxWidth : linkCardMaxWidth;
        return AnimatedContainer(
          key: const ValueKey('link-card-surface'),
          duration: const Duration(milliseconds: 80),
          curve: Curves.linear,
          decoration: BoxDecoration(
            color: _pressed ? t.surfaceRaised : widget.ground,
            border: Border.all(color: _pressed ? t.lineStrong : t.lineHair),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            mainAxisSize: MainAxisSize.min,
            children: [
              if (link.hasImage) _imageBox(t, width),
              Padding(
                padding: const EdgeInsets.fromLTRB(12, 10, 12, 12),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    Row(
                      children: [
                        Expanded(
                          child: link.shownFinalHost == null
                              ? Text(link.host, key: const ValueKey('link-card-host'), maxLines: 1, overflow: TextOverflow.ellipsis, style: hostStyle)
                              : Text.rich(
                                  TextSpan(children: [
                                    TextSpan(text: link.host),
                                    TextSpan(text: ' → ', style: TextStyle(color: t.textMeta)),
                                    TextSpan(text: link.shownFinalHost),
                                  ]),
                                  key: const ValueKey('link-card-host'),
                                  maxLines: 1,
                                  overflow: TextOverflow.ellipsis,
                                  style: hostStyle,
                                ),
                        ),
                        const SizedBox(width: 12),
                        AnimatedSwitcher(
                          duration: const Duration(milliseconds: 80),
                          child: GlyphIcon(Glyph.arrowOut, key: ValueKey(_pressed), color: _pressed ? t.textPrimary : t.textSecondary, size: 9),
                        ),
                      ],
                    ),
                    if (title != null) ...[
                      const SizedBox(height: 3),
                      Text(
                        title,
                        key: const ValueKey('link-card-title'),
                        maxLines: 2,
                        overflow: TextOverflow.ellipsis,
                        style: TextStyle(fontFamily: fontSans, fontSize: 14, height: 20 / 14, fontWeight: FontWeight.w600, color: t.textPrimary),
                      ),
                    ],
                    if (description != null) ...[
                      const SizedBox(height: 3),
                      Text(
                        description,
                        key: const ValueKey('link-card-description'),
                        maxLines: 2,
                        overflow: TextOverflow.ellipsis,
                        style: TextStyle(fontFamily: fontSans, fontSize: 13, height: 19 / 13, color: t.textSecondary),
                      ),
                    ],
                  ],
                ),
              ),
            ],
          ),
        );
      },
    );
    return ConstrainedBox(
      constraints: const BoxConstraints(maxWidth: linkCardMaxWidth),
      child: Semantics(
        link: true,
        label: '${link.host}${link.shownFinalHost == null ? '' : ' to ${link.shownFinalHost}'}${title == null ? '' : ', $title'}',
        child: FocusableActionDetector(
          onShowFocusHighlight: (v) => setState(() => _focused = v),
          actions: {ActivateIntent: CallbackAction<ActivateIntent>(onInvoke: (_) => _open())},
          child: GestureDetector(
            behavior: HitTestBehavior.opaque,
            onTap: _open,
            onTapDown: (_) => setState(() => _pressed = true),
            onTapUp: (_) => setState(() => _pressed = false),
            onTapCancel: () => setState(() => _pressed = false),
            onLongPress: () {
              setState(() => _pressed = false);
              if (isWebUrl(link.url)) showLinkSheet(context, link.url);
            },
            child: Stack(
              clipBehavior: Clip.none,
              children: [
                card,
                if (_focused)
                  Positioned.fill(
                    left: -4,
                    top: -4,
                    right: -4,
                    bottom: -4,
                    child: IgnorePointer(
                      child: DecoratedBox(
                        key: const ValueKey('link-card-focus'),
                        decoration: BoxDecoration(border: Border.all(color: t.textPrimary, width: 2)),
                      ),
                    ),
                  ),
              ],
            ),
          ),
        ),
      ),
    );
  }

  Widget _imageBox(CatenaryTokens t, double width) {
    final link = widget.link;
    // The outer width, as the canvas computes it: 1748×1187 is 358×243.
    final height = linkImageHeight(width: width, imageWidth: link.imageWidth!, imageHeight: link.imageHeight!);
    return Container(
      key: const ValueKey('link-card-image'),
      height: height,
      decoration: BoxDecoration(border: Border(bottom: BorderSide(color: t.lineHair))),
      child: Stack(
        fit: StackFit.expand,
        children: [
          BlurhashFill(hash: link.placeholder!, fallback: t.surfaceRaised),
          if (widget.image != null)
            Image(
              image: widget.image!,
              fit: BoxFit.cover,
              frameBuilder: (context, child, frame, sync) => AnimatedOpacity(
                opacity: frame == null ? 0 : 1,
                duration: const Duration(milliseconds: 160),
                curve: Curves.linear,
                child: child,
              ),
              // A failed image leaves the blurhash: nothing collapses.
              errorBuilder: (context, error, stack) => const SizedBox.shrink(),
            ),
        ],
      ),
    );
  }
}

/// The long-press sheet: the full URL, which the card never shows, and COPY
/// LINK. On Android message actions live here too once the app has them.
Future<void> showLinkSheet(BuildContext context, String url) {
  final t = CatenaryTokens.of(context);
  return showModalBottomSheet<void>(
    context: context,
    backgroundColor: t.surfaceRail,
    shape: const RoundedRectangleBorder(),
    builder: (sheet) => SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 16, 16, 12),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Text(
              url,
              key: const ValueKey('link-sheet-url'),
              style: TextStyle(fontFamily: fontMono, fontSize: 11, height: 16 / 11, color: t.textSecondary),
            ),
            const SizedBox(height: 12),
            GestureDetector(
              key: const ValueKey('link-sheet-copy'),
              behavior: HitTestBehavior.opaque,
              onTap: () {
                Clipboard.setData(ClipboardData(text: url));
                Navigator.of(sheet).pop();
              },
              child: Container(
                height: 44,
                alignment: Alignment.center,
                decoration: BoxDecoration(border: Border.all(color: t.lineHair)),
                child: Text('COPY LINK', style: TextStyle(fontFamily: fontMono, fontSize: 9.5, letterSpacing: 9.5 * 0.1, color: t.textPrimary)),
              ),
            ),
          ],
        ),
      ),
    ),
  );
}
