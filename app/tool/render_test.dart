// Renders the app to PNGs, one per theme, for a pull request to show: the
// estate's rule is that a change to something visible carries a picture.
// Not part of `flutter test` (it is outside test/); run it by name:
//
//     flutter test tool/render_test.dart --update-goldens
//
// and the images land in build/render/. It loads the bundled IBM Plex files,
// because a widget test otherwise paints every glyph as a box.

import 'dart:io';

import 'package:catenary/enroll.dart';
import 'package:catenary/fixtures.dart';
import 'package:catenary/metrics.dart';
import 'package:catenary/rail.dart';
import 'package:catenary/specimen.dart';
import 'package:catenary/start_failed.dart';
import 'package:catenary/states.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
import 'package:catenary/store/enrollment.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/widgets/composer.dart';
import 'package:catenary/widgets/held_chip.dart';
import 'package:catenary/widgets/link_card.dart';
import 'package:catenary/widgets/reply_stub.dart';
import 'package:catenary/thread.dart';
import 'package:catenary/tokens.dart';
import 'package:catenary_client/catenary_client.dart' show EnrollAnswerUnreadable, EnrollRefused;
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

Future<void> loadFonts() async {
  for (final (family, files) in [
    (fontSans, ['IBMPlexSans-Regular', 'IBMPlexSans-Medium', 'IBMPlexSans-SemiBold']),
    (fontMono, ['IBMPlexMono-Regular', 'IBMPlexMono-Medium', 'IBMPlexMono-SemiBold']),
  ]) {
    final loader = FontLoader(family);
    for (final f in files) {
      final bytes = File('assets/fonts/$f.ttf').readAsBytesSync();
      loader.addFont(Future.value(ByteData.sublistView(bytes)));
    }
    await loader.load();
  }
}

void main() {
  for (final (mode, name) in [(ThemeMode.dark, 'dark'), (ThemeMode.light, 'light')]) {
    testWidgets('render the specimen, $name', (tester) async {
      await loadFonts();
      // A Pixel 9 Pro's logical size, at 2x so the text is legible in a PR.
      tester.view.physicalSize = const Size(820, 1830);
      tester.view.devicePixelRatio = 2;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        debugShowCheckedModeBanner: false,
        theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
        home: SpecimenScreen(mode: mode, onMode: (_) {}),
      ));
      await tester.pumpAndSettle();
      await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/specimen-$name.png'));
    });

    // The rail and the thread at the canvas's own moment (CANT-43), at its
    // 390 logical pixels. The thread's view is taller than the canvas's 844
    // because the app draws a photo at its real 3:2 where the mock has a
    // 100px placeholder.
    for (final (screen, height) in [('rail', 844.0), ('thread', 1090.0)]) {
      testWidgets('render the $screen, $name', (tester) async {
        await loadFonts();
        final now = DateTime(2026, 8, 16, 14, 16);
        tester.view.physicalSize = Size(780, height * 2);
        tester.view.devicePixelRatio = 2;
        addTearDown(tester.view.reset);
        final conversations = fixtureConversations(now);
        await tester.pumpWidget(MaterialApp(
          debugShowCheckedModeBanner: false,
          theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
          home: TickerMode(
            enabled: false,
            child: screen == 'rail'
                ? RailScreen(
                    conversations: conversations,
                    now: now,
                    connection: const ConnectionView(kind: ConnectionKind.reconnecting, attempt: 3, retryIn: Duration(seconds: 8)),
                    myInitials: 'HB',
                    openId: 'kitchen',
                  )
                : ThreadScreen(conversation: conversations.first, connection: const ConnectionView.live()),
          ),
        ));
        await tester.pumpAndSettle();
        await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/$screen-$name.png'));
      });
    }

    // Every connection, status, typing and composer state (CANT-44), with the
    // tickers muted: a pulse caught mid-fade is not what the state looks like.
    testWidgets('render the states, $name', (tester) async {
      await loadFonts();
      tester.view.physicalSize = const Size(820, 2560);
      tester.view.devicePixelRatio = 2;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        debugShowCheckedModeBanner: false,
        theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
        home: const TickerMode(enabled: false, child: StatesScreen()),
      ));
      await tester.pumpAndSettle();
      await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/states-$name.png'));
    });

    // A voice note pending, with and without the server's estimate, over the
    // collapsed transcript it turns into (CANT-229, the canvas's frame 04 B):
    // all three are the same height.
    testWidgets('render the voice note states, $name', (tester) async {
      await loadFonts();
      tester.view.physicalSize = const Size(780, 900);
      tester.view.devicePixelRatio = 2;
      addTearDown(tester.view.reset);
      final peaks = fixturePeaks(9931, 48);
      const length = Duration(seconds: 72);
      await tester.pumpWidget(MaterialApp(
        debugShowCheckedModeBanner: false,
        theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
        home: TickerMode(
          enabled: false,
          child: Builder(
            builder: (context) => Scaffold(
              backgroundColor: CatenaryTokens.of(context).surfaceBase,
              body: Padding(
                padding: const EdgeInsets.all(14),
                child: Column(
                  children: [
                    VoiceNoteBlock(voice: VoiceNote(duration: length, peaks: peaks, eta: const Duration(seconds: 20))),
                    const SizedBox(height: 14),
                    VoiceNoteBlock(voice: VoiceNote(duration: length, peaks: peaks)),
                    const SizedBox(height: 14),
                    VoiceNoteBlock(
                      voice: VoiceNote(
                        duration: length,
                        peaks: peaks,
                        transcript:
                            'Okay so I talked to Ted about the delivery and the short version is Thursday still works but they want us to confirm the count by tomorrow noon.',
                      ),
                    ),
                  ],
                ),
              ),
            ),
          ),
        ),
      ));
      await tester.pumpAndSettle();
      await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/voice-$name.png'));
    });

    // The link preview canvas, narrow (CANT-325): cards in a thread, then the
    // held chips, the quoted stubs and the armed bars. The card images are the
    // blurhash: no bytes are fetched in a render.
    testWidgets('render the link previews, thread, $name', (tester) async {
      await loadFonts();
      tester.view.physicalSize = const Size(780, 2900);
      tester.view.devicePixelRatio = 2;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        debugShowCheckedModeBanner: false,
        theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
        home: TickerMode(
          enabled: false,
          child: ThreadScreen(conversation: fixtureLinkConversation(DateTime(2026, 8, 16, 14, 16)), connection: const ConnectionView.live()),
        ),
      ));
      // The blurhash decodes off the frame: let it land before the picture.
      await tester.runAsync(() => Future<void>.delayed(const Duration(milliseconds: 200)));
      await tester.pumpAndSettle();
      await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/link-thread-$name.png'));
    });

    testWidgets('render the link previews, chips and stubs, $name', (tester) async {
      await loadFonts();
      tester.view.physicalSize = const Size(780, 1900);
      tester.view.devicePixelRatio = 2;
      addTearDown(tester.view.reset);
      const argosy = LinkPreviewView(
        url: 'https://argosy.zerogravity.industries/series/1',
        title: 'Severance — Season 2 is in the library',
        imageWidth: 1920,
        imageHeight: 1080,
        placeholder: fixtureHashWarm,
      );
      const switchyard = LinkPreviewView(url: 'https://switchyard.zerogravity.industries/dashboard', title: 'Switchyard — Dashboard');
      const riverline = LinkPreviewView(url: 'https://www.riverline.org/page');
      Widget tray(Widget child) => Builder(
            builder: (context) => Container(
              decoration: BoxDecoration(color: CatenaryTokens.of(context).surfaceRail, border: Border.all(color: CatenaryTokens.of(context).lineHair)),
              margin: const EdgeInsets.only(bottom: 12),
              child: child,
            ),
          );
      Widget composer({HeldLink? link, String? image, String? draft}) => tray(Composer(
            connection: const ConnectionView.live(),
            controller: TextEditingController(text: draft),
            link: link,
            attachment: image,
            onRemoveLink: () {},
            onRemoveAttachment: () {},
            onAttach: () {},
            onSend: () {},
          ));
      await tester.pumpWidget(MaterialApp(
        debugShowCheckedModeBanner: false,
        theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
        home: TickerMode(
          enabled: false,
          child: Builder(
            builder: (context) => Scaffold(
              backgroundColor: CatenaryTokens.of(context).surfaceBase,
              body: SingleChildScrollView(
                padding: const EdgeInsets.fromLTRB(0, 24, 0, 0),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    composer(link: const HeldLink(host: 'switchyard.zerogravity.industries', title: 'Switchyard — Dashboard'), draft: '…/dashboard'),
                    composer(link: const HeldLink(host: 'chronicle.zerogravity.industries', fetching: true), draft: '…/w/sunday'),
                    composer(link: const HeldLink(host: 'argosy.zerogravity.industries', title: 'Severance — Season 2 is in the library'), image: 'IMG_4471.HEIC', draft: "this one's the shed"),
                    Padding(
                      padding: const EdgeInsets.all(16),
                      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: const [
                        QuotedLinkStub(sender: 'Ted', link: switchyard),
                        SizedBox(height: 12),
                        QuotedLinkStub(sender: 'Nadia', link: riverline),
                      ]),
                    ),
                    tray(ArmedLinkBar(sender: 'Ted', link: switchyard, onClear: () {})),
                    tray(ArmedLinkBar(sender: 'Nadia', link: riverline, onClear: () {})),
                    Padding(padding: const EdgeInsets.all(16), child: LinkCard(link: argosy, ground: CatenaryTokens.of(context).surfaceRail)),
                  ],
                ),
              ),
            ),
          ),
        ),
      ));
      await tester.runAsync(() => Future<void>.delayed(const Duration(milliseconds: 200)));
      await tester.pumpAndSettle();
      await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/link-chips-$name.png'));
    });

    // The enrollment form saying what the server did (CANT-234): a 200 it
    // could not read, and a status that is not Catenary refusing.
    for (final (file, error) in <(String, Object)>[
      ('unreadable', const EnrollAnswerUnreadable('not a pair')),
      ('could-not-answer', const EnrollRefused(502)),
    ]) {
      testWidgets('render the enrollment form, $file, $name', (tester) async {
        await loadFonts();
        tester.view.physicalSize = const Size(780, 1688);
        tester.view.devicePixelRatio = 2;
        addTearDown(tester.view.reset);
        await tester.pumpWidget(MaterialApp(
          debugShowCheckedModeBanner: false,
          theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
          home: TickerMode(
            enabled: false,
            child: EnrollScreen(deviceName: 'Android phone', onSubmit: (_, _, _) async => EnrollFailed(enrollErrorText(error))),
          ),
        ));
        await tester.enterText(find.byKey(const Key('enroll-address')), 'chat.example.com');
        await tester.enterText(find.byKey(const Key('enroll-token')), 'A' * 43);
        await tester.pump();
        await tester.tap(find.byKey(const Key('enroll-submit')));
        await tester.pump();
        await tester.pump();
        await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/enroll-$file-$name.png'));
      });
    }

    // The failed-start screen (CANT-222): a device that could not open what
    // it keeps, and one whose re-enrollment could not clear the old journal.
    for (final (cause, file) in [(StartFailedCause.stores, 'start-failed'), (StartFailedCause.wipeOwed, 'wipe-owed')]) {
      testWidgets('render the failed start, $file, $name', (tester) async {
        await loadFonts();
        tester.view.physicalSize = const Size(780, 1688);
        tester.view.devicePixelRatio = 2;
        addTearDown(tester.view.reset);
        await tester.pumpWidget(MaterialApp(
          debugShowCheckedModeBanner: false,
          theme: catenaryTheme(mode == ThemeMode.dark ? Brightness.dark : Brightness.light),
          home: StartFailedScreen(cause: cause, name: 'SqliteException', onRetry: () async {}),
        ));
        await tester.pumpAndSettle();
        await expectLater(find.byType(MaterialApp), matchesGoldenFile('../build/render/$file-$name.png'));
      });
    }
  }
}
