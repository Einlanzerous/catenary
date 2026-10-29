// CANT-325: the link preview card, the held chips, and the quoted stub and
// armed bar, built to `Catenary Link Preview.dc.html` (narrow). Fed by
// fixtures: the app does not import the Dart client's preview call yet, only
// its detection rule (CANT-329).
//
// What each group holds:
//   host           derived from the URL, never from a page's own words
//   card           full, imageless, long title, empty preview, row ground
//   image box      the height is final before a byte of the image arrives,
//                  and stays when the image fails
//   interaction    pressed, focus-visible, open, long-press sheet
//   chips          arrived, fetching, both stacked, after ✕
//   composer       the chip appears with the host at once; dismissed stays
//                  dismissed; sent while fetching is plain text
//   stub and bar   with a preview and with an empty one

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:ui' as ui;

import 'package:catenary/fixtures.dart';
import 'package:catenary/store/attach.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
import 'package:catenary/store/link.dart';
import 'package:catenary/store/status.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary/tokens.dart';
import 'package:catenary/widgets/blurhash.dart';
import 'package:catenary/widgets/composer.dart';
import 'package:catenary/widgets/held_chip.dart';
import 'package:catenary/widgets/link_card.dart';
import 'package:catenary/widgets/reply_stub.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

/// The one detection table (CANT-329): schema/vectors/link-detection.json, read
/// by the web test and the Dart client's as well. The app imports the Dart
/// client's implementation of the rule, so this asserts the app's wrappers on it.
final detection = (jsonDecode(File('../schema/vectors/link-detection.json').readAsStringSync())['cases'] as List)
    .cast<Map<String, dynamic>>();

const _switchyard = LinkPreviewView(
  url: 'https://switchyard.zerogravity.industries/dashboard',
  title: 'Switchyard — Dashboard',
  description: "What's happening across switchyard at a glance.",
  imageUrl: '/media/og',
  imageWidth: 1748,
  imageHeight: 1187,
  placeholder: fixtureHashCool,
);

Widget app(Widget child, {Brightness brightness = Brightness.dark, double width = 358}) => MaterialApp(
      theme: catenaryTheme(brightness),
      home: Scaffold(body: Align(alignment: Alignment.topLeft, child: SizedBox(width: width, child: child))),
    );

CatenaryTokens tokens(WidgetTester tester, Finder f) => CatenaryTokens.of(tester.element(f));

/// An image that arrives when the test says so.
class LateImage extends ImageProvider<LateImage> {
  final Completer<ImageInfo> done = Completer();

  @override
  Future<LateImage> obtainKey(ImageConfiguration configuration) => SynchronousFuture(this);

  @override
  ImageStreamCompleter loadImage(LateImage key, ImageDecoderCallback decode) => OneFrameImageStreamCompleter(done.future);
}

void main() {
  group('the host is derived from the URL', () {
    test('lowercased, www dropped, port kept', () {
      expect(linkHost('https://www.Switchyard.ZeroGravity.industries/dashboard'), 'switchyard.zerogravity.industries');
      expect(linkHost('http://example.com:8080/x?y=1'), 'example.com:8080');
      expect(linkHost('https://example.com:443/'), 'example.com');
      expect(linkHost('https://go.nebula.tv/mustard'), 'go.nebula.tv');
    });

    test('an internationalized host is shown as punycode so a lookalike looks different', () {
      expect(linkHost('https://münchen.de/'), 'xn--mnchen-3ya.de');
      expect(linkHost('https://日本.jp/'), 'xn--wgv71a.jp');
      expect(linkHost('https://аррӏе.com/'), startsWith('xn--'));
    });

    test('a URL with no host is shown as it is', () {
      expect(linkHost('not a url'), 'not a url');
    });

    testWidgets('the card shows the URL\'s host, whatever the page called itself', (tester) async {
      await tester.pumpWidget(app(const LinkCard(
        link: LinkPreviewView(url: 'https://www.evil.example:8443/login', title: 'Your bank, really'),
        ground: Colors.black,
      )));
      expect(tester.widget<Text>(find.byKey(const ValueKey('link-card-host'))).data, 'evil.example:8443');
    });
  });

  group('the redirect row (CANT-339)', () {
    String hostText(WidgetTester tester) => tester.widget<Text>(find.byKey(const ValueKey('link-card-host'))).textSpan?.toPlainText() ??
        tester.widget<Text>(find.byKey(const ValueKey('link-card-host'))).data!;

    testWidgets('redirected: youtu.be → youtube.com, the arrow in text.meta, both hosts in the host style', (tester) async {
      await tester.pumpWidget(app(const LinkCard(
        link: LinkPreviewView(url: 'https://youtu.be/abc', title: 'A video', finalHost: 'www.YouTube.com'),
        ground: Colors.black,
      )));
      expect(hostText(tester), 'youtu.be → youtube.com');
      final t = tokens(tester, find.byType(LinkCard));
      final host = tester.widget<Text>(find.byKey(const ValueKey('link-card-host')));
      expect(host.style!.color, t.textSecondary);
      final spans = (host.textSpan! as TextSpan).children!.cast<TextSpan>();
      expect(spans.map((s) => s.text), ['youtu.be', ' → ', 'youtube.com']);
      expect(spans[1].style!.color, t.textMeta);
      expect(spans[0].style, isNull);
      expect(spans[2].style, isNull);
    });

    testWidgets('same-site: no final_host, one host, no arrow', (tester) async {
      await tester.pumpWidget(app(const LinkCard(link: LinkPreviewView(url: 'https://www.go.nebula.tv/x', title: 'A video'), ground: Colors.black)));
      expect(hostText(tester), 'go.nebula.tv');
      expect(find.textContaining('→'), findsNothing);
    });

    testWidgets('old shape: a message from before the field renders exactly as before', (tester) async {
      await tester.pumpWidget(app(const LinkCard(link: _switchyard, ground: Colors.black)));
      expect(tester.widget<Text>(find.byKey(const ValueKey('link-card-host'))).data, 'switchyard.zerogravity.industries');
      expect(find.textContaining('→'), findsNothing);
    });

    testWidgets('an empty final_host draws no arrow', (tester) async {
      await tester.pumpWidget(app(const LinkCard(link: LinkPreviewView(url: 'https://a.example/', title: 't', finalHost: ' '), ground: Colors.black)));
      expect(find.textContaining('→'), findsNothing);
    });

    test('final_host goes through the one display rule', () {
      expect(linkFinalHost('www.Example.COM'), 'example.com');
      expect(linkFinalHost('xn--mnchen-3ya.de'), 'xn--mnchen-3ya.de');
      expect(linkFinalHost('[2001:db8::1]'), '[2001:db8::1]');
    });

    testWidgets('a long pair of hosts stays on one line', (tester) async {
      await tester.pumpWidget(app(const LinkCard(
        link: LinkPreviewView(url: 'https://a-very-long-subdomain.of-the-pasted-site.example/x', title: 't', finalHost: 'another-very-long-subdomain.of-the-landing-site.example'),
        ground: Colors.black,
      )));
      expect(tester.takeException(), isNull);
      expect(tester.getSize(find.byKey(const ValueKey('link-card-host'))).height, lessThan(16));
    });
  });

  group('page text is cleaned, and URLs are found', () {
    test('whitespace collapses; control and bidi characters go', () {
      expect(cleanPageText('  a\n\n b\t\tc  '), 'a b c');
      expect(cleanPageText('safe\u202Etxt.exe\u2066x\u2069\u0007'), 'safetxt.exex');
    });

    test('the first URL is the one the shared table says, and only when it is askable', () {
      for (final c in detection) {
        final url = c['url'] as String?;
        expect(firstUrl(c['text'] as String), (c['ask'] as bool) ? url : isNull, reason: '${c['name']}');
      }
    });

    test('every URL in a body is found by the same rule, and the text keeps every character', () {
      expect(urlParts('see https://a.example/1, then HTTPS://B.EXAMPLE/2 and https://c.example/3.').where((p) => p.isUrl).map((p) => p.text),
          ['https://a.example/1', 'https://c.example/3']);
    });

    test('the text is split on URLs and keeps every character', () {
      const text = 'oh hey https://a.example/x. and (https://b.example/y)';
      expect(urlParts(text).map((p) => p.text).join(), text);
      expect(urlParts(text).where((p) => p.isUrl).map((p) => p.text), ['https://a.example/x', 'https://b.example/y']);
    });
  });

  group('the image box is known before paint', () {
    test('2:1 to 1:1, capped at 240', () {
      // 1748×1187 is 358×243 in the narrow column, as the canvas says; the
      // cap then takes it to 240.
      expect(linkImageHeight(width: 358, imageWidth: 1748, imageHeight: 1187, maxHeight: 1000), closeTo(243.1, 0.1));
      expect(linkImageHeight(width: 358, imageWidth: 1748, imageHeight: 1187), 240);
      expect(linkImageHeight(width: 358, imageWidth: 1920, imageHeight: 1080), closeTo(358 * 1080 / 1920, 0.01));
      // Wider than 2:1 clamps up to 2:1; portrait caps at square.
      expect(linkImageHeight(width: 358, imageWidth: 4000, imageHeight: 1000), 179);
      expect(linkImageHeight(width: 200, imageWidth: 1000, imageHeight: 3000), 200);
    });

    test('a blurhash decodes, and a malformed one does not', () {
      expect(decodeBlurhash(fixtureHashWarm)!.length, 32 * 32 * 4);
      expect(decodeBlurhash('nope'), isNull);
      expect(decodeBlurhash('${fixtureHashWarm}x'), isNull);
    });

    testWidgets('the row is its final height before the image bytes arrive, and after, and if they never do', (tester) async {
      Future<(Size, Size)> measure(Future<void> Function(LateImage) arrive) async {
        final image = LateImage();
        await tester.pumpWidget(app(LinkCard(link: _switchyard, ground: Colors.black, image: image)));
        final before = tester.getSize(find.byType(LinkCard));
        final box = tester.getSize(find.byKey(const ValueKey('link-card-image')));
        await arrive(image);
        await tester.pump(const Duration(milliseconds: 200));
        expect(tester.getSize(find.byType(LinkCard)), before, reason: 'nothing reflows');
        expect(tester.getSize(find.byKey(const ValueKey('link-card-image'))), box);
        return (before, box);
      }

      final (arrived, box) = await measure((image) async {
        final decoded = (await tester.runAsync(() => _testImage()))!;
        image.done.complete(ImageInfo(image: decoded));
      });
      expect(box.width, closeTo(358 - 2, 0.01));
      expect(box.height, 240);
      final (failed, failedBox) = await measure((image) async {
        image.done.completeError('404');
      });
      expect(failed, arrived);
      expect(failedBox, box);
      // The blurhash is still the thing in the box.
      expect(find.byType(BlurhashFill), findsOneWidget);
    });

    testWidgets('the image crossfades over the blurhash in 160 ms, linear', (tester) async {
      final image = LateImage();
      await tester.pumpWidget(app(LinkCard(link: _switchyard, ground: Colors.black, image: image)));
      AnimatedOpacity fade() => tester.widget<AnimatedOpacity>(find.descendant(of: find.byKey(const ValueKey('link-card-image')), matching: find.byType(AnimatedOpacity)));
      expect(fade().opacity, 0);
      expect(fade().duration, const Duration(milliseconds: 160));
      expect(fade().curve, Curves.linear);
      final decoded = (await tester.runAsync(() => _testImage()))!;
      image.done.complete(ImageInfo(image: decoded));
      await tester.pump();
      await tester.pump();
      expect(fade().opacity, 1);
    });
  });

  group('the card', () {
    testWidgets('a full card: image, host, title and description', (tester) async {
      await tester.pumpWidget(app(const LinkCard(link: _switchyard, ground: Colors.black)));
      expect(find.byKey(const ValueKey('link-card-image')), findsOneWidget);
      expect(find.byType(BlurhashFill), findsOneWidget);
      expect(find.text('switchyard.zerogravity.industries'), findsOneWidget);
      expect(find.text('Switchyard — Dashboard'), findsOneWidget);
      expect(find.text("What's happening across switchyard at a glance."), findsOneWidget);
      expect(tester.getSize(find.byType(LinkCard)).width, 358);
    });

    testWidgets('an imageless card is text only: no box, no ratio guessed', (tester) async {
      await tester.pumpWidget(app(const LinkCard(
        link: LinkPreviewView(url: 'https://chronicle.zerogravity.industries/w/sunday', title: 'Chronicle — Sunday dinner notes', description: 'Written notes.'),
        ground: Colors.black,
      )));
      expect(find.byKey(const ValueKey('link-card-image')), findsNothing);
      expect(find.byType(BlurhashFill), findsNothing);
      expect(find.text('Chronicle — Sunday dinner notes'), findsOneWidget);
      // Dimensions with no hash, and a hash with no dimensions, are both text only.
      expect(const LinkPreviewView(url: 'https://a.example', title: 't', imageWidth: 4, imageHeight: 3).hasImage, isFalse);
      expect(const LinkPreviewView(url: 'https://a.example', title: 't', imageWidth: 0, imageHeight: 3, placeholder: fixtureHashCool).hasImage, isFalse);
      expect(const LinkPreviewView(url: 'https://a.example', title: 't', placeholder: fixtureHashCool).hasImage, isFalse);
    });

    testWidgets('a long title and description clamp at two lines', (tester) async {
      final link = LinkPreviewView(url: 'https://lyceum.zerogravity.industries/reader/64', title: 'Long title ' * 40, description: 'Long description ' * 40);
      await tester.pumpWidget(app(LinkCard(link: link, ground: Colors.black)));
      expect(tester.getSize(find.byKey(const ValueKey('link-card-title'))).height, 40);
      expect(tester.getSize(find.byKey(const ValueKey('link-card-description'))).height, closeTo(38, 0.01));
      final title = tester.widget<Text>(find.byKey(const ValueKey('link-card-title')));
      expect(title.maxLines, 2);
      expect(title.overflow, TextOverflow.ellipsis);
    });

    testWidgets('page text is drawn cleaned, as plain text', (tester) async {
      await tester.pumpWidget(app(const LinkCard(
        link: LinkPreviewView(url: 'https://a.example/', title: '<b>Hi</b>\n\n  there\u202E'),
        ground: Colors.black,
      )));
      expect(find.text('<b>Hi</b> there'), findsOneWidget);
    });

    testWidgets('the tokens: ground one step off the row, hairline border, secondary host, primary 600 title', (tester) async {
      await tester.pumpWidget(app(Builder(builder: (context) => LinkCard(link: _switchyard, ground: CatenaryTokens.of(context).surfaceRail))));
      final t = tokens(tester, find.byType(LinkCard));
      final surface = tester.widget<AnimatedContainer>(find.byKey(const ValueKey('link-card-surface')));
      final deco = surface.decoration! as BoxDecoration;
      expect(deco.color, t.surfaceRail);
      expect((deco.border! as Border).top.color, t.lineHair);
      expect(tester.widget<Text>(find.byKey(const ValueKey('link-card-host'))).style!.color, t.textSecondary);
      final title = tester.widget<Text>(find.byKey(const ValueKey('link-card-title'))).style!;
      expect(title.color, t.textPrimary);
      expect(title.fontWeight, FontWeight.w600);
      expect(tester.widget<Text>(find.byKey(const ValueKey('link-card-description'))).style!.color, t.textSecondary);
      // Not one pixel of copper on the card.
      expect(find.byWidgetPredicate((w) => w is DecoratedBox && (w.decoration as BoxDecoration).color == t.accentWire), findsNothing);
    });

    testWidgets('the same card in light', (tester) async {
      await tester.pumpWidget(app(const LinkCard(link: _switchyard, ground: Colors.white), brightness: Brightness.light));
      expect(find.text('Switchyard — Dashboard'), findsOneWidget);
    });
  });

  group('interaction: the whole card is one target', () {
    testWidgets('only http and https are ever handed to the launcher', (tester) async {
      expect(isWebUrl('https://a.example/x'), isTrue);
      expect(isWebUrl('javascript:alert(1)'), isFalse);
      expect(isWebUrl('intent://x#Intent;end'), isFalse);
      expect(isWebUrl('file:///etc/passwd'), isFalse);
      String? opened;
      await tester.pumpWidget(app(LinkCard(link: const LinkPreviewView(url: 'javascript:alert(1)', title: 'x'), ground: Colors.black, onOpen: (u) => opened = u)));
      await tester.tap(find.text('x'));
      await tester.longPress(find.text('x'));
      await tester.pumpAndSettle();
      expect(opened, isNull);
      expect(find.byKey(const ValueKey('link-sheet-url')), findsNothing);
    });

    testWidgets('a tap opens the URL as the sender pasted it', (tester) async {
      String? opened;
      await tester.pumpWidget(app(LinkCard(link: _switchyard, ground: Colors.black, onOpen: (u) => opened = u)));
      await tester.tap(find.text('Switchyard — Dashboard'));
      expect(opened, 'https://switchyard.zerogravity.industries/dashboard');
    });

    testWidgets('pressed: surface.raised and a line-strong border, in 80 ms linear; released it goes back', (tester) async {
      await tester.pumpWidget(app(const LinkCard(link: _switchyard, ground: Color(0xFF0E1112))));
      final t = tokens(tester, find.byType(LinkCard));
      final gesture = await tester.startGesture(tester.getCenter(find.byType(LinkCard)));
      // A press is told apart from the start of a long-press after 100 ms.
      await tester.pump(const Duration(milliseconds: 120));
      await tester.pump(const Duration(milliseconds: 80));
      final surface = tester.widget<AnimatedContainer>(find.byKey(const ValueKey('link-card-surface')));
      expect(surface.duration, const Duration(milliseconds: 80));
      expect(surface.curve, Curves.linear);
      var deco = surface.decoration! as BoxDecoration;
      expect(deco.color, t.surfaceRaised);
      expect((deco.border! as Border).top.color, t.lineStrong);
      await gesture.up();
      await tester.pump(const Duration(milliseconds: 80));
      deco = tester.widget<AnimatedContainer>(find.byKey(const ValueKey('link-card-surface'))).decoration! as BoxDecoration;
      expect(deco.color, const Color(0xFF0E1112));
      expect((deco.border! as Border).top.color, t.lineHair);
    });

    testWidgets('focus-visible: a 2px text.primary outline at offset 2, and Enter opens', (tester) async {
      String? opened;
      await tester.pumpWidget(app(Padding(padding: const EdgeInsets.all(8), child: LinkCard(link: _switchyard, ground: Colors.black, onOpen: (u) => opened = u))));
      expect(find.byKey(const ValueKey('link-card-focus')), findsNothing);
      await tester.sendKeyEvent(LogicalKeyboardKey.tab);
      await tester.pump();
      final ring = find.byKey(const ValueKey('link-card-focus'));
      expect(ring, findsOneWidget);
      final t = tokens(tester, ring);
      final border = ((tester.widget<DecoratedBox>(ring)).decoration as BoxDecoration).border! as Border;
      expect(border.top.color, t.textPrimary);
      expect(border.top.width, 2);
      // Offset 2: the ring's outer edge sits 4 from the card (2 gap + 2 line).
      final card = tester.getRect(find.byKey(const ValueKey('link-card-surface')));
      expect(tester.getRect(ring), card.inflate(4));
      await tester.sendKeyEvent(LogicalKeyboardKey.enter);
      expect(opened, isNotNull);
    });

    testWidgets('long-press: the full URL and COPY LINK', (tester) async {
      String? copied;
      tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (call) async {
        if (call.method == 'Clipboard.setData') copied = (call.arguments as Map)['text'] as String;
        return null;
      });
      addTearDown(() => tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, null));
      var opened = false;
      await tester.pumpWidget(app(LinkCard(link: _switchyard, ground: Colors.black, onOpen: (_) => opened = true)));
      await tester.longPress(find.byType(LinkCard));
      await tester.pumpAndSettle();
      expect(opened, isFalse);
      expect(find.byKey(const ValueKey('link-sheet-url')), findsOneWidget);
      expect(tester.widget<Text>(find.byKey(const ValueKey('link-sheet-url'))).data, 'https://switchyard.zerogravity.industries/dashboard');
      await tester.tap(find.byKey(const ValueKey('link-sheet-copy')));
      await tester.pumpAndSettle();
      expect(copied, 'https://switchyard.zerogravity.industries/dashboard');
      expect(find.byKey(const ValueKey('link-sheet-url')), findsNothing);
    });
  });

  group('in the thread', () {
    final now = DateTime(2026, 8, 16, 14, 16);

    Future<void> thread(WidgetTester tester, ConversationView c, {Brightness brightness = Brightness.dark}) {
      tester.view.physicalSize = const Size(390, 2400);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      return tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(brightness),
        home: TickerMode(enabled: false, child: ThreadScreen(conversation: c, connection: const ConnectionView.live())),
      ));
    }

    testWidgets('three received and one sent pending: four cards; the empty preview draws none', (tester) async {
      final c = fixtureLinkConversation(now);
      await thread(tester, c);
      expect(find.byType(LinkCard), findsNWidgets(4));
      expect(find.byKey(const ValueKey('link-card-l-3')), findsNothing, reason: 'an empty preview is no card; the URL stands alone');
      expect(find.byKey(const ValueKey('link-card-l-0')), findsOneWidget);
      // The pending row's card is fully rendered, and the row says SENDING.
      expect(find.byKey(const ValueKey('link-card-l-4')), findsOneWidget);
      expect(find.text('SENDING'), findsOneWidget);
    });

    testWidgets('ground is one step off the row: rail on theirs, base on yours', (tester) async {
      await thread(tester, fixtureLinkConversation(now));
      final t = tokens(tester, find.byType(ThreadScreen));
      Color ground(String id) => ((tester.widget<AnimatedContainer>(find.descendant(of: find.byKey(ValueKey('link-card-$id')), matching: find.byKey(const ValueKey('link-card-surface')))).decoration) as BoxDecoration).color!;
      expect(ground('l-0'), t.surfaceRail);
      expect(ground('l-4'), t.surfaceBase);
    });

    testWidgets('the URL in the body is underlined in text.meta and left exactly as pasted', (tester) async {
      await thread(tester, fixtureLinkConversation(now));
      final t = tokens(tester, find.byType(ThreadScreen));
      final rich = tester.widgetList<RichText>(find.descendant(of: find.byKey(const ValueKey('group-l-0')), matching: find.byType(RichText))).firstWhere((r) => r.text.toPlainText().startsWith('Last night'));
      final spans = <InlineSpan>[];
      rich.text.visitChildren((s) {
        spans.add(s);
        return true;
      });
      final url = spans.whereType<TextSpan>().firstWhere((s) => s.text == 'https://switchyard.zerogravity.industries/dashboard');
      expect(url.style!.decoration, TextDecoration.underline);
      expect(url.style!.decorationColor, t.textMeta);
    });

    testWidgets('a long title does not push the row past its clamp', (tester) async {
      await thread(tester, fixtureLinkConversation(now));
      final lyceum = find.descendant(of: find.byKey(const ValueKey('link-card-l-2')), matching: find.byKey(const ValueKey('link-card-title')));
      expect(tester.getSize(lyceum).height, 40);
    });

    testWidgets('both themes', (tester) async {
      await thread(tester, fixtureLinkConversation(now), brightness: Brightness.light);
      expect(find.byType(LinkCard), findsNWidgets(4));
    });
  });

  group('the held chip', () {
    Future<void> composer(WidgetTester tester, {HeldLink? link, String? image, VoidCallback? onRemoveLink, VoidCallback? onRemoveImage}) {
      return tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: Scaffold(
          body: Align(
            alignment: Alignment.bottomCenter,
            child: SizedBox(
              width: 390,
              child: Composer(
                connection: const ConnectionView.live(),
                link: link,
                onRemoveLink: onRemoveLink,
                attachment: image,
                onRemoveAttachment: onRemoveImage,
                onAttach: () {},
                onSend: () {},
              ),
            ),
          ),
        ),
      ));
    }

    testWidgets('arrived: LINK, the host, the title, 44 high, a 44×44 remove target', (tester) async {
      var removed = 0;
      await composer(tester, link: const HeldLink(host: 'switchyard.zerogravity.industries', title: 'Switchyard — Dashboard'), onRemoveLink: () => removed++);
      expect(find.text('LINK'), findsOneWidget);
      expect(find.text('switchyard.zerogravity.industries'), findsOneWidget);
      expect(find.text('Switchyard — Dashboard'), findsOneWidget);
      expect(find.text('FETCHING PREVIEW'), findsNothing);
      expect(tester.getSize(find.byKey(const ValueKey('composer-link-chip'))).height, 44);
      expect(tester.getSize(find.byKey(const ValueKey('composer-link-remove'))), const Size(44, 44));
      await tester.tap(find.byKey(const ValueKey('composer-link-remove')));
      expect(removed, 1);
      final t = tokens(tester, find.byType(Composer));
      expect(tester.widget<Text>(find.text('LINK')).style!.color, t.textSecondary);
      expect(tester.widget<Text>(find.text('Switchyard — Dashboard')).style!.color, t.textPrimary);
    });

    testWidgets('fetching: the host at once, FETCHING PREVIEW in text.meta, and the 1.4 s sweep', (tester) async {
      await composer(tester, link: const HeldLink(host: 'chronicle.zerogravity.industries', fetching: true));
      expect(find.text('chronicle.zerogravity.industries'), findsOneWidget);
      final fetching = tester.widget<Text>(find.text('FETCHING PREVIEW'));
      expect(fetching.style!.color, tokens(tester, find.byType(Composer)).textMeta);
      expect(find.byKey(const ValueKey('held-chip-sweep')), findsOneWidget);
      expect(tester.getSize(find.byKey(const ValueKey('composer-link-chip'))).height, 44, reason: 'no height animation');
      // Linear, left to right, off the edge and back round in 1.4 s.
      final x0 = tester.getRect(find.byKey(const ValueKey('held-chip-sweep'))).left;
      await tester.pump(const Duration(milliseconds: 700));
      final x1 = tester.getRect(find.byKey(const ValueKey('held-chip-sweep'))).left;
      await tester.pump(const Duration(milliseconds: 350));
      final x2 = tester.getRect(find.byKey(const ValueKey('held-chip-sweep'))).left;
      expect(x1, greaterThan(x0));
      expect((x2 - x1 - (x1 - x0) / 2).abs(), lessThan(0.5), reason: 'constant speed');
      expect(tester.getSize(find.byKey(const ValueKey('held-chip-sweep'))).height, 1);
    });

    testWidgets('both: link above image, one tag column, 8 apart; the image chip keeps its name', (tester) async {
      await composer(tester, link: const HeldLink(host: 'argosy.zerogravity.industries', title: 'Severance — Season 2 is in the library'), image: 'IMG_4471.HEIC');
      final link = tester.getRect(find.byKey(const ValueKey('composer-link-chip')));
      final image = tester.getRect(find.byKey(const ValueKey('composer-attachment')));
      expect(image.top - link.bottom, 8);
      expect(link.height, 44);
      expect(image.height, 44);
      expect(find.text('IMAGE'), findsOneWidget);
      expect(find.text('IMG_4471.HEIC'), findsOneWidget);
      // One component, two tags: both tag cells are 58 wide, so the two
      // columns' right edges line up.
      expect(tester.getCenter(find.text('LINK')).dx, tester.getCenter(find.text('IMAGE')).dx);
      expect(tester.getSize(find.byKey(const ValueKey('composer-attachment-remove'))), const Size(44, 44));
      final t = tokens(tester, find.byType(Composer));
      expect(tester.widget<Text>(find.text('IMG_4471.HEIC')).style!.color, t.textSecondary);
    });

    testWidgets('after ✕: the chip row is gone and the field stays', (tester) async {
      await composer(tester);
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing);
      expect(find.byKey(const ValueKey('composer-chips')), findsNothing);
      expect(find.byKey(const ValueKey('composer-input')), findsOneWidget);
    });
  });

  group('the composer holds a chip for the first URL of a draft', () {
    const url = 'https://switchyard.zerogravity.industries/dashboard';
    late List<String> asked;
    late Completer<LinkPreviewView?> answer;
    late List<String> sentPlain;
    late List<(String, LinkPreviewView)> sentLinked;

    late List<(String, LinkPreviewView)> sentBoth;

    Future<void> screen(WidgetTester tester, {bool picked = false, bool both = false}) async {
      sentBoth = [];
      asked = [];
      sentPlain = [];
      sentLinked = [];
      answer = Completer();
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: TickerMode(
          enabled: false,
          child: ThreadScreen(
            conversation: fixtureLinkConversation(DateTime(2026, 8, 16, 14, 16)),
            connection: const ConnectionView.live(),
            onSend: (text) async => sentPlain.add(text),
            onPreviewLink: (u) {
              asked.add(u);
              return answer.future;
            },
            onSendLinked: (text, preview) async => sentLinked.add((text, preview)),
            onPickImage: picked ? () async => const Picked(PickedImage(path: '/tmp/a.jpg', name: 'a.jpg')) : null,
            onSendPickedLinked: both ? (text, image, preview) async => sentBoth.add((text, preview)) : null,
          ),
        ),
      ));
    }

    Future<void> type(WidgetTester tester, String text) async {
      await tester.enterText(find.byKey(const ValueKey('composer-input')), text);
      await tester.pump();
    }

    testWidgets('the chip shows the host the instant the URL is in the draft; the fetch follows', (tester) async {
      await screen(tester);
      await type(tester, 'runs: $url');
      expect(find.byKey(const ValueKey('composer-link-chip')), findsOneWidget);
      expect(find.text('switchyard.zerogravity.industries'), findsOneWidget);
      expect(find.text('FETCHING PREVIEW'), findsOneWidget);
      expect(asked, isEmpty, reason: 'debounced: the URL has to stop changing');
      await tester.pump(const Duration(milliseconds: 650));
      expect(asked, [url]);
      answer.complete(const LinkPreviewView(url: url, title: 'Switchyard — Dashboard'));
      await tester.pump();
      await tester.pump();
      final chip = find.byKey(const ValueKey('composer-link-chip'));
      expect(find.descendant(of: chip, matching: find.text('Switchyard — Dashboard')), findsOneWidget);
      expect(find.text('FETCHING PREVIEW'), findsNothing);
    });

    testWidgets('sent with a settled preview, the message carries it', (tester) async {
      await screen(tester);
      await type(tester, url);
      await tester.pump(const Duration(milliseconds: 650));
      answer.complete(const LinkPreviewView(url: url, title: 'Switchyard — Dashboard'));
      await tester.pump();
      await tester.tap(find.byKey(const ValueKey('composer-primary')));
      await tester.pump();
      expect(sentPlain, isEmpty);
      expect(sentLinked.single.$1, url);
      expect(sentLinked.single.$2.title, 'Switchyard — Dashboard');
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing, reason: 'the draft is clear');
    });

    testWidgets('sent while fetching, the message goes as plain text and never gains a card', (tester) async {
      await screen(tester);
      await type(tester, url);
      await tester.pump(const Duration(milliseconds: 650));
      expect(find.text('FETCHING PREVIEW'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('composer-primary')));
      await tester.pump();
      expect(sentPlain, [url]);
      expect(sentLinked, isEmpty);
    });

    testWidgets('dismissed: no chip, the URL text untouched, and that URL is not fetched again in this draft', (tester) async {
      await screen(tester);
      await type(tester, url);
      await tester.pump(const Duration(milliseconds: 650));
      expect(asked, hasLength(1));
      await tester.tap(find.byKey(const ValueKey('composer-link-remove')));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing);
      expect(find.byKey(const ValueKey('composer-chips')), findsNothing);
      expect(find.text(url), findsOneWidget, reason: 'the URL text is the sender\'s and stays');
      // More words after it: the first URL is the same one, and stays dismissed.
      await type(tester, '$url and more');
      await tester.pump(const Duration(milliseconds: 650));
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing);
      expect(asked, hasLength(1));
      // A late answer to the dismissed fetch brings nothing back.
      answer.complete(const LinkPreviewView(url: url, title: 'late'));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing);
      // And sending is plain text.
      await tester.tap(find.byKey(const ValueKey('composer-primary')));
      await tester.pump();
      expect(sentPlain, ['$url and more']);
    });

    testWidgets('an empty or failed fetch removes the chip itself', (tester) async {
      await screen(tester);
      await type(tester, url);
      await tester.pump(const Duration(milliseconds: 650));
      answer.complete(null);
      await tester.pump();
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing);
      expect(find.text(url), findsOneWidget);
    });

    testWidgets('a held image hides the link chip unless the send can carry both', (tester) async {
      await screen(tester, picked: true);
      await type(tester, url);
      expect(find.byKey(const ValueKey('composer-link-chip')), findsOneWidget);
      await tester.tap(find.text('ADD'));
      await tester.pump();
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-attachment')), findsOneWidget);
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing, reason: 'the send would drop the card');
    });

    testWidgets('with a send that carries both, the image and the link chips stack and one send carries both', (tester) async {
      await screen(tester, picked: true, both: true);
      await type(tester, url);
      await tester.pump(const Duration(milliseconds: 650));
      answer.complete(const LinkPreviewView(url: url, title: 'Switchyard — Dashboard'));
      await tester.pump();
      await tester.pump();
      await tester.tap(find.text('ADD'));
      await tester.pump();
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-link-chip')), findsOneWidget);
      expect(find.byKey(const ValueKey('composer-attachment')), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('composer-primary')));
      await tester.pump();
      expect(sentBoth.single.$2.title, 'Switchyard — Dashboard');
      expect(sentPlain, isEmpty);
    });

    testWidgets('a URL taken out of the draft takes its chip with it', (tester) async {
      await screen(tester);
      await type(tester, url);
      expect(find.byKey(const ValueKey('composer-link-chip')), findsOneWidget);
      await type(tester, 'never mind');
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing);
      await tester.pump(const Duration(milliseconds: 650));
      expect(asked, isEmpty, reason: 'the pending fetch was cancelled with it');
    });

    testWidgets('with no preview call wired, there is no chip', (tester) async {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: TickerMode(
          enabled: false,
          child: ThreadScreen(conversation: fixtureLinkConversation(DateTime(2026, 8, 16, 14, 16)), connection: const ConnectionView.live(), onSend: (_) async {}),
        ),
      ));
      await type(tester, url);
      expect(find.byKey(const ValueKey('composer-link-chip')), findsNothing);
    });
  });

  group('the quoted stub and the armed bar', () {
    const empty = LinkPreviewView(url: 'https://www.riverline.org/page');

    testWidgets('stub with a preview: sender, ↗ LINK in text.meta, host, title', (tester) async {
      await tester.pumpWidget(app(const QuotedLinkStub(sender: 'Ted', link: _switchyard)));
      final t = tokens(tester, find.byType(QuotedLinkStub));
      expect(find.text('TED'), findsOneWidget);
      expect(tester.widget<Text>(find.text('\u2197 LINK')).style!.color, t.textMeta, reason: 'not copper');
      expect(tester.widget<Text>(find.byKey(const ValueKey('stub-host'))).data, 'switchyard.zerogravity.industries');
      expect(tester.widget<Text>(find.byKey(const ValueKey('stub-host'))).style!.color, t.textSecondary);
      expect(tester.widget<Text>(find.byKey(const ValueKey('stub-title'))).data, 'Switchyard — Dashboard');
      expect(tester.widget<Text>(find.byKey(const ValueKey('stub-title'))).style!.color, t.textMeta);
      expect(tester.widget<Text>(find.byKey(const ValueKey('stub-title'))).maxLines, 1);
      final rule = (tester.widget<Container>(find.byKey(const ValueKey('stub-rule'))).decoration! as BoxDecoration).border! as Border;
      expect(rule.left.color, t.lineHair);
      expect(rule.left.width, 2);
    });

    testWidgets('stub with an empty preview ends at the host: no title, no filler', (tester) async {
      await tester.pumpWidget(app(const QuotedLinkStub(sender: 'Nadia', link: empty)));
      expect(find.text('NADIA'), findsOneWidget);
      expect(tester.widget<Text>(find.byKey(const ValueKey('stub-host'))).data, 'riverline.org');
      expect(find.byKey(const ValueKey('stub-title')), findsNothing);
      expect(find.textContaining('preview', findRichText: true), findsNothing);
    });

    testWidgets('armed bar with a preview: accent rule, REPLYING TO in the accent, ✕ a 44×44 target', (tester) async {
      var cleared = 0;
      await tester.pumpWidget(app(ArmedLinkBar(sender: 'Ted', link: _switchyard, onClear: () => cleared++)));
      final t = tokens(tester, find.byType(ArmedLinkBar));
      final label = tester.widget<Text>(find.byKey(const ValueKey('bar-label')));
      final spans = (label.textSpan! as TextSpan).children!.cast<TextSpan>();
      expect(spans[0].text, 'REPLYING TO TED');
      expect(spans[0].style!.color, t.accentWire);
      expect(spans[1].text, ' · \u2197 LINK');
      expect(spans[1].style!.color, t.textMeta);
      expect(find.byKey(const ValueKey('bar-title')), findsOneWidget);
      expect(tester.getSize(find.byKey(const ValueKey('bar-clear'))), const Size(44, 44));
      final rule = tester.widgetList<Container>(find.descendant(of: find.byType(ArmedLinkBar), matching: find.byType(Container))).firstWhere((c) => c.color == t.accentWire);
      expect(rule.constraints!.maxWidth, 2);
      await tester.tap(find.byKey(const ValueKey('bar-clear')));
      expect(cleared, 1);
    });

    testWidgets('armed bar with an empty preview: the host alone', (tester) async {
      await tester.pumpWidget(app(ArmedLinkBar(sender: 'Nadia', link: empty, onClear: () {})));
      expect(tester.widget<Text>(find.byKey(const ValueKey('bar-host'))).data, 'riverline.org');
      expect(find.byKey(const ValueKey('bar-title')), findsNothing);
    });

    testWidgets('narrow: a long title ellipsizes on one line and nothing overflows', (tester) async {
      final long = LinkPreviewView(url: 'https://switchyard.zerogravity.industries/dashboard', title: 'A very long page title ' * 10);
      await tester.pumpWidget(app(Column(children: [QuotedLinkStub(sender: 'Ted', link: long), ArmedLinkBar(sender: 'Ted', link: long, onClear: () {})]), width: 320));
      expect(tester.takeException(), isNull);
    });
  });

  test('the message status enum is untouched by a link: a link card is not a delivery state', () {
    expect(MessageStatus.values.map((s) => s.name), containsAll(['sending', 'sent']));
  });
}

Future<ui.Image> _testImage() => createTestImage(width: 8, height: 8);
