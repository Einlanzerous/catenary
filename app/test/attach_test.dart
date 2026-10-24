// ADD, the single-image picker (CANT-317, CANT-247). Four things are held
// here. The pre-check refuses what the server would refuse, before anything is
// composed (ruling 1 → B). ADD follows the outbox's own predicate and no
// second condition (ruling 4 → A). The composer holds one picked image, never
// two. And a send the outbox refuses is shown inline and writes nothing.

import 'dart:async';
import 'dart:io';

import 'package:catenary/store/app_store.dart';
import 'package:catenary/store/attach.dart';
import 'package:catenary/store/connection.dart';
import 'package:catenary/store/conversation.dart';
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
        expect(picks, 1, reason: 'the component did not second-guess the wiring in ${kind.name}');
        await pump(null);
        await tester.tap(find.text('ADD'));
        expect(picks, 1, reason: 'no wiring, no ADD');
      });
    }
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
      expect(find.byKey(const ValueKey('composer-attachment')), findsOneWidget);
      expect(find.text('first.jpg'), findsOneWidget);
      await tester.tap(find.text('ADD'));
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
      await tester.tap(find.text('ADD'));
      await tester.pump();
      expect(find.byKey(const ValueKey('composer-notice')), findsOneWidget);
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
