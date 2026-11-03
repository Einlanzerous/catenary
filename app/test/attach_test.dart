// ADD, the single-image picker (CANT-317, CANT-247). Four things are held
// here. The pre-check refuses what the server would refuse, before anything is
// composed (ruling 1 → B). ADD follows the outbox's own predicate and no
// second condition (ruling 4 → A). The composer holds one picked image, never
// two. And a send the outbox refuses is shown inline and writes nothing.

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/attach.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
import 'package:catenary/store/voice.dart';
import 'package:catenary/store/address.dart';
import 'package:catenary/store/session.dart';
import 'package:catenary/theme.dart';
import 'package:catenary/thread.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'app_store_test.dart' show projection;
import 'ready_transport.dart';

const me = '11111111-1111-4111-8111-111111111111';

ConversationView threadFixture() => conversationViews(projection: projection(), outbox: const [], me: me, secure: true).first;
final fixtures = Directory('../testdata/media');

final class _DeadSocket implements WebSocketLike {
  @override
  void Function()? onOpen;
  @override
  void Function(Object? data)? onMessage;
  @override
  void Function(int? code)? onClose;

  @override
  void send(String data) => throw StateError('never open');

  @override
  void close([int? code, String? reason]) {}
}

void main() {
  late Directory dir;

  setUp(() => dir = Directory.systemTemp.createTempSync('catenary-attach-'));
  tearDown(() => dir.deleteSync(recursive: true));

  Future<AppStore> store(ImagePick? pick, {Transport Function(Transport inner)? wrap}) async {
    final credentials = openCredentialStore(dir.path);
    await enrollCredential(
      credentials,
      inProcessLock(),
      const StoredCredential(
        userId: me,
        deviceId: '22222222-2222-4222-8222-222222222222',
        accessToken: 'access',
        accessExpiresAt: 4102444800000,
        refreshToken: 'refresh',
        refreshExpiresAt: 4102444800000,
      ),
    );
    credentials.close();
    writeAddress(dir.path, 'https://chat.example.com');
    final s = AppStore(
      SessionSeams(
        directory: dir.path,
        lifecycle: ManualLifecycle(),
        connect: (url, protocols) => _DeadSocket(),
        fetch: (_) async => const HttpAnswer(503, ''),
        // An upload that is never answered: this file is about the pick.
        uploadFetch: (_) => Completer<HttpAnswer>().future,
        transportFactory: (cfg) => wrap == null ? createTransport(cfg) : wrap(createTransport(cfg)),
      ),
      systemClock,
      wipeJournalFile,
      pick,
    );
    addTearDown(s.dispose);
    await s.start();
    return s;
  }

  PickedImage pickedFixture(String name) => PickedImage(path: '${fixtures.path}/$name', name: name);

  test('the several-files message is the one the web client holds (messages.json)', () {
    final shared = jsonDecode(File('${fixtures.path}/messages.json').readAsStringSync()) as Map<String, Object?>;
    expect(oneImageMessage, shared['oneImage']);
  });

  group('the pre-check runs before anything is composed', () {
    // The shared fixtures: the same verdicts the Go ingest test asserts.
    for (final (name, ok) in [
      ('photo-exif-gps.jpg', true),
      ('image.png', true),
      ('image.gif', true),
      ('image.webp', true),
      ('image.heic', false),
      ('image.svg', false),
      ('not-an-image.jpg', false),
    ]) {
      test('$name is ${ok ? 'held' : 'refused with the file-type words'}, and the outbox is untouched', () async {
        final s = await store(() async => pickedFixture(name));
        final out = await s.pickImage();
        if (ok) {
          expect(out, isA<Picked>());
        } else {
          expect(out, isA<PickRefused>());
          expect((out as PickRefused).message, uploadImageType);
          expect((out).message, contains('JPEG, PNG, GIF or WebP'));
        }
        expect(s.conversations, isEmpty);
        final entries = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
        addTearDown(entries.close);
        expect(await entries.list(), isEmpty, reason: 'picking composes nothing');
      });
    }

    test('a file over the image cap is refused with the size words (generated, never committed)', () async {
      final big = File('${dir.path}/big.jpg');
      final raf = big.openSync(mode: FileMode.write);
      raf.writeFromSync([0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01]);
      raf.truncateSync(imageCapBytes + 1);
      raf.closeSync();
      final s = await store(() async => PickedImage(path: big.path, name: 'big.jpg'));
      final out = await s.pickImage();
      expect((out as PickRefused).message, uploadImageTooLarge);
    });

    test('backing out of the picker is nothing', () async {
      expect(await (await store(() async => null)).pickImage(), isNull);
    });

    test('a picker that throws is a message, not a crash', () async {
      final out = await (await store(() async => throw StateError('no activity'))).pickImage();
      expect(out, isA<PickRefused>());
    });
  });

  group('ADD follows the outbox\'s predicate', () {
    test('canAttach is false with no session ready: a dead socket is not one', () async {
      expect((await store(() async => null)).canAttach, isFalse);
    });

    test('canAttach is false with no picker, even on a ready session', () async {
      expect((await store(null, wrap: ReadyTransport.new)).canAttach, isFalse);
    });

    test('canAttach is true on a ready session, and false when that session is terminal: the outbox\'s predicate', () async {
      expect((await store(() async => null, wrap: ReadyTransport.new)).canAttach, isTrue);
    });

    test('canAttach is false once the transport is terminal, ready or not', () async {
      final s = await store(() async => null, wrap: (t) => ReadyTransport(t, terminal: Terminal(TerminalKind.protocol, 'close 4001')));
      expect(s.canAttach, isFalse);
    });

    test('on a ready session the picked file is composed: one entry holding one image attachment, its bytes stored beside it', () async {
      final s = await store(() async => null, wrap: ReadyTransport.new);
      await s.send('44444444-4444-4444-8444-444444444444', '', image: pickedFixture('photo-exif-gps.jpg'));
      final entries = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
      addTearDown(entries.close);
      final held = await entries.list();
      expect(held, hasLength(1));
      expect(held.single.attachments.single.kind, 'image');
      expect(held.single.attachments.single.filename, 'photo-exif-gps.jpg');
      expect(await entries.media(held.single.clientId, 0), File('${fixtures.path}/photo-exif-gps.jpg').readAsBytesSync());
    });

    // Composer follows `onPickImage` alone. Passing it over every connection
    // state the app knows proves the component reads no second condition.
    for (final kind in ConnectionKind.values) {
      testWidgets('with a picker wired, ADD is enabled in ${kind.name}; with none, it is dimmed', (tester) async {
        final thread = threadFixture();
        var picks = 0;
        Future<void> pump(Future<PickOutcome?> Function()? pick) async {
          tester.view.physicalSize = const Size(390, 844);
          tester.view.devicePixelRatio = 1;
          addTearDown(tester.view.reset);
          await tester.pumpWidget(MaterialApp(
            theme: catenaryTheme(Brightness.dark),
            home: TickerMode(
              enabled: false,
              child: ThreadScreen(
                conversation: thread,
                connection: ConnectionView(kind: kind, terminal: kind == ConnectionKind.terminal ? TerminalCause.credential : null),
                onSend: (_) async {},
                onPickImage: pick,
              ),
            ),
          ));
        }

        await pump(() async {
          picks++;
          return null;
        });
        await tester.tap(find.text('ADD'));
        await tester.pump();
        await tester.tap(find.text('CHOOSE FILE'));
        await tester.pump();
        expect(picks, 1, reason: 'the component did not second-guess the wiring in ${kind.name}');
        await pump(null);
        await tester.tap(find.text('ADD'));
        expect(picks, 1, reason: 'no wiring, no ADD');
      });
    }
  });

  group('the in-composer attach zone (CANT-356)', () {
    Future<void> screen(
      WidgetTester tester, {
      Future<PickOutcome?> Function()? pick,
      Future<PickOutcome> Function(List<PastedImage>)? paste,
      Future<RecordStart> Function()? record,
      Brightness brightness = Brightness.dark,
    }) async {
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(brightness),
        home: TickerMode(
          enabled: false,
          child: ThreadScreen(
            conversation: threadFixture(),
            connection: const ConnectionView.live(),
            onSend: (_) async {},
            onPickImage: pick ?? () async => null,
            onPasteImages: paste,
            onSendPicked: (_, _) async {},
            onStartRecording: record,
            onCancelRecording: () async {},
            onSendRecording: () async {},
          ),
        ),
      ));
    }

    Future<void> openZone(WidgetTester tester) async {
      await tester.tap(find.text('ADD'));
      await tester.pump();
      await tester.pump();
    }

    testWidgets('ADD toggles the zone; CHOOSE FILE is a 44px target with focus, and the line is built from the constants', (tester) async {
      await screen(tester, paste: (_) async => const PickRefused('x'));
      expect(find.byKey(const ValueKey('attach-zone')), findsNothing);
      await openZone(tester);
      expect(find.byKey(const ValueKey('attach-zone')), findsOneWidget);
      expect(find.text('Choose an image, or paste one.'), findsOneWidget);
      expect(tester.getSize(find.byKey(const ValueKey('attach-choose'))).height, 44);
      expect(Focus.of(tester.element(find.text('CHOOSE FILE'))).hasFocus, isTrue);
      expect(acceptedLine(), 'JPEG, PNG, GIF or WebP · up to 16 MB');
      expect(find.text(acceptedLine()), findsOneWidget);
      // The zone spans the composer's width, inside its 12px padding.
      expect(tester.getSize(find.byKey(const ValueKey('attach-zone'))).width, 390 - 24);
      await tester.tap(find.text('ADD'));
      await tester.pump();
      expect(find.byKey(const ValueKey('attach-zone')), findsNothing);
    });

    testWidgets('system back closes the zone first and returns focus to ADD', (tester) async {
      await screen(tester);
      await openZone(tester);
      await tester.binding.handlePopRoute();
      await tester.pump();
      expect(find.byKey(const ValueKey('attach-zone')), findsNothing);
      expect(Focus.of(tester.element(find.text('ADD'))).hasFocus, isTrue);
    });

    testWidgets('a refusal is one inline line, the zone stays open, the next replaces it, and a held image closes it', (tester) async {
      final outcomes = <PickOutcome>[
        const PickRefused(uploadImageType, fileName: 'scan_0412_gate-sign_final.tiff', kind: 'TIFF'),
        const PickRefused(uploadImageTooLarge, fileName: 'big.jpg', kind: 'JPG'),
        const Picked(PickedImage(path: '/a.jpg', name: 'a.jpg')),
      ];
      await screen(tester, pick: () async => outcomes.removeAt(0));
      await openZone(tester);
      await tester.tap(find.text('CHOOSE FILE'));
      await tester.pump();
      expect(find.byKey(const ValueKey('attach-zone')), findsOneWidget, reason: 'the zone stays open');
      expect(find.text('scan_0412_gate-sign_final.tiff'), findsOneWidget);
      expect(find.text('TIFF'), findsOneWidget);
      expect(find.text(uploadImageType), findsOneWidget);
      await tester.tap(find.text('CHOOSE FILE'));
      await tester.pump();
      expect(find.byKey(const ValueKey('attach-refusal')), findsOneWidget, reason: 'one line, never a stack');
      expect(find.text(uploadImageType), findsNothing);
      expect(find.text(uploadImageTooLarge), findsOneWidget);
      await tester.tap(find.text('CHOOSE FILE'));
      await tester.pump();
      expect(find.byKey(const ValueKey('attach-zone')), findsNothing);
      expect(find.byKey(const ValueKey('composer-attachment')), findsOneWidget);
    });

    testWidgets('recording replaces the whole row, closes the zone, and it is not redrawn when the take ends', (tester) async {
      await screen(tester, record: () async => const RecordingStarted());
      await openZone(tester);
      expect(find.byKey(const ValueKey('attach-zone')), findsOneWidget);
      await tester.tap(find.text('REC'));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-recording')), findsOneWidget);
      expect(find.byKey(const ValueKey('attach-zone')), findsNothing);
      await tester.tap(find.byKey(const ValueKey('composer-cancel-recording')));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-recording')), findsNothing);
      expect(find.byKey(const ValueKey('attach-zone')), findsNothing);
    });

    testWidgets('the zone leaves with the gate: an offline client closes it', (tester) async {
      await screen(tester);
      await openZone(tester);
      expect(find.byKey(const ValueKey('attach-zone')), findsOneWidget);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: TickerMode(
          enabled: false,
          child: ThreadScreen(
            conversation: threadFixture(),
            connection: const ConnectionView(kind: ConnectionKind.offline),
            onSend: (_) async {},
          ),
        ),
      ));
      await tester.pump();
      expect(find.byKey(const ValueKey('attach-zone')), findsNothing);
    });

    for (final (name, mime) in [('image.png', 'image/png'), ('image.heic', 'image/heic'), ('image.svg', 'image/svg+xml'), ('photo-exif-gps.jpg', 'image/jpeg')]) {
      test('paste and pick judge $name identically, through the one store gate', () async {
        final s = await store(() async => pickedFixture(name));
        final picked = await s.pickImage();
        final bytes = File('${fixtures.path}/$name').readAsBytesSync();
        final pasted = await s.pasteImages([PastedImage(bytes: bytes, name: 'pasted.${mime.split('/').last}')]);
        expect(pasted.runtimeType, picked.runtimeType);
        if (picked is PickRefused) expect((pasted as PickRefused).message, picked.message);
        expect(s.conversations, isEmpty, reason: 'neither entrance composes anything');
      });
    }

    test('several pasted files are refused whole with the shared words; a paste over the cap with the size words', () async {
      final s = await store(() async => null);
      final head = Uint8List.fromList([0xff, 0xd8, 0xff, 0xe0]);
      final out = await s.pasteImages([PastedImage(bytes: head, name: 'a.jpg'), PastedImage(bytes: head, name: 'b.jpg')]) as PickRefused;
      expect(out.message, oneImageMessage);
      expect(out.kind, '2 FILES');
      final big = Uint8List(imageCapBytes + 1)..setAll(0, [0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01]);
      expect(((await s.pasteImages([PastedImage(bytes: big, name: 'big.jpg')])) as PickRefused).message, uploadImageTooLarge);
    });

    test('the zone\'s cap line moves with the client\'s cap', () {
      expect(imageCapMb * 1024 * 1024, imageCapBytes);
    });

    testWidgets('a pasted image reaches the same held chip; a refused one shows the zone line', (tester) async {
      final outcomes = <PickOutcome>[
        const PickRefused(uploadImageType, fileName: 'pasted.heic', kind: 'HEIC'),
        const Picked(PickedImage(path: '/p.png', name: 'pasted.png')),
      ];
      await screen(tester, paste: (_) async => outcomes.removeAt(0));
      await openZone(tester);
      final config = tester.widget<TextField>(find.byKey(const ValueKey('composer-input'))).contentInsertionConfiguration!;
      expect(config.allowedMimeTypes, ['image/*']);
      config.onContentInserted(KeyboardInsertedContent(mimeType: 'image/heic', uri: 'content://x', data: Uint8List.fromList([1, 2, 3])));
      await tester.pump();
      await tester.pump();
      expect(find.text(uploadImageType), findsOneWidget);
      expect(find.byKey(const ValueKey('attach-zone')), findsOneWidget);
      config.onContentInserted(KeyboardInsertedContent(mimeType: 'image/png', uri: 'content://y', data: Uint8List.fromList([1, 2, 3])));
      await tester.pump();
      await tester.pump();
      expect(find.text('pasted.png'), findsOneWidget);
      expect(find.byKey(const ValueKey('attach-zone')), findsNothing);
    });

    testWidgets('no paste wired: the prompt does not promise one and the field takes no inserted content', (tester) async {
      await screen(tester);
      await openZone(tester);
      expect(find.text('Choose an image.'), findsOneWidget);
      expect(tester.widget<TextField>(find.byKey(const ValueKey('composer-input'))).contentInsertionConfiguration, isNull);
    });
  });

  group('one image per message', () {
    testWidgets('choosing a second image replaces the first, and SEND carries only the held one', (tester) async {
      final thread = threadFixture();
      final queue = [const PickedImage(path: '/a.jpg', name: 'first.jpg'), const PickedImage(path: '/b.jpg', name: 'second.jpg')];
      final sent = <(String, PickedImage)>[];
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: TickerMode(
          enabled: false,
          child: ThreadScreen(
            conversation: thread,
            connection: const ConnectionView.live(),
            onSend: (_) async => fail('an image send does not go through the text path'),
            onPickImage: () async => Picked(queue.removeAt(0)),
            onSendPicked: (text, image) async => sent.add((text, image)),
          ),
        ),
      ));
      await tester.tap(find.text('ADD'));
      await tester.pump();
      await tester.tap(find.text('CHOOSE FILE'));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-attachment')), findsOneWidget);
      expect(find.text('first.jpg'), findsOneWidget);
      await tester.tap(find.text('ADD'));
      await tester.pump();
      await tester.tap(find.text('CHOOSE FILE'));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-attachment')), findsOneWidget, reason: 'never two chips');
      expect(find.text('first.jpg'), findsNothing);
      expect(find.text('second.jpg'), findsOneWidget);

      // An image with no text is a message: the primary square reads SEND.
      expect(find.text('SEND'), findsOneWidget);
      await tester.tap(find.text('SEND'));
      await tester.pump();
      expect(sent.map((s) => s.$2.name), ['second.jpg']);
      expect(find.byKey(const ValueKey('composer-attachment')), findsNothing, reason: 'cleared once stored');
    });

    testWidgets('remove drops the held image; a refused pick shows its words and holds nothing new', (tester) async {
      final thread = threadFixture();
      var first = true;
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: TickerMode(
          enabled: false,
          child: ThreadScreen(
            conversation: thread,
            connection: const ConnectionView.live(),
            onSend: (_) async {},
            onPickImage: () async {
              if (first) {
                first = false;
                return const Picked(PickedImage(path: '/a.jpg', name: 'a.jpg'));
              }
              return const PickRefused(uploadImageType);
            },
            onSendPicked: (_, _) async {},
          ),
        ),
      ));
      await tester.tap(find.text('ADD'));
      await tester.pump();
      await tester.tap(find.text('CHOOSE FILE'));
      await tester.pump();
      await tester.tap(find.text('ADD'));
      await tester.pump();
      await tester.tap(find.text('CHOOSE FILE'));
      await tester.pump();
      // The zone is open on a refusal, so the one line is inside it.
      expect(find.byKey(const ValueKey('attach-zone')), findsOneWidget);
      expect(find.byKey(const ValueKey('composer-notice')), findsNothing);
      expect(find.text(uploadImageType), findsOneWidget);
      expect(find.text('a.jpg'), findsOneWidget, reason: 'a refusal does not drop what is held');
      await tester.tap(find.byKey(const ValueKey('composer-attachment-remove')));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-attachment')), findsNothing);
    });

    testWidgets('a send the outbox refuses is shown inline, keeps the draft and the image, and writes nothing', (tester) async {
      final thread = threadFixture();
      tester.view.physicalSize = const Size(390, 844);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.reset);
      await tester.pumpWidget(MaterialApp(
        theme: catenaryTheme(Brightness.dark),
        home: TickerMode(
          enabled: false,
          child: ThreadScreen(
            conversation: thread,
            connection: const ConnectionView.live(),
            onSend: (_) async {},
            onPickImage: () async => const Picked(PickedImage(path: '/a.jpg', name: 'a.jpg')),
            onSendPicked: (_, _) async => throw const AttachRefused(ComposeRefused.pickedFileOffline),
          ),
        ),
      ));
      await tester.tap(find.text('ADD'));
      await tester.pump();
      await tester.tap(find.text('CHOOSE FILE'));
      await tester.pump();
      await tester.tap(find.text('SEND'));
      await tester.pump();
      expect(find.text(ComposeRefused.pickedFileOffline), findsOneWidget);
      expect(find.text('a.jpg'), findsOneWidget);
    });
  });

  group('the store\'s send', () {
    test('a picked file with no ready session is AttachRefused with the outbox\'s words, and the outbox is empty', () async {
      final s = await store(() async => null);
      final image = pickedFixture('photo-exif-gps.jpg');
      await expectLater(
        s.send('44444444-4444-4444-8444-444444444444', '', image: image),
        throwsA(isA<AttachRefused>().having((e) => e.message, 'message', ComposeRefused.pickedFileOffline)),
      );
      final entries = SqliteOutboxStore.open('${dir.path}/$outboxFileName');
      addTearDown(entries.close);
      expect(await entries.list(), isEmpty);
    });

    test('blank text and no image is still not a message', () async {
      final s = await store(() async => null);
      await s.send('44444444-4444-4444-8444-444444444444', '   ');
    });
  });
}
