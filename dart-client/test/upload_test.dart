/// The real `Uploader` (CANT-316, CANT-247): the request it makes, the words
/// it throws, the bound it keeps, and what RETRY costs. Every test that needs
/// an outbox uses the real one, over a scripted session.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';
import 'outbox_harness.dart';

const base = 'https://chat.example.com';

Uint8List recording([int seed = 7]) => Uint8List.fromList([for (var i = 0; i < 4096; i++) (i * 31 + seed) & 0xff]);

OutboxDraft imageDraft(Uint8List bytes) =>
    OutboxDraft(conversationId: conv, attachments: [OutboundAttachmentDraft(kind: 'image', source: MediaBytes(bytes), filename: 'a.png')]);
final handle = uuid(777);

/// Bytes that are not valid UTF-8 and not the same at every index: a body that
/// went through a string would not come back.
Uint8List jpegLike([int n = 4096]) => Uint8List.fromList([0xff, 0xd8, 0xff, for (var i = 0; i < n; i++) 0x80 | ((i * 31) & 0x7f)]);

HttpAnswer created([Uuid? id]) => HttpAnswer(201, jsonEncode({'upload_id': id ?? handle}));

/// A credential the test steers: [repair] answers a 401, [refresh] is the
/// work before a request.
final class Cred implements CredentialSeam {
  Cred({this.repair = false, this.refresh});

  bool repair;
  Future<void> Function()? refresh;
  var repairs = 0;

  @override
  Future<Credential> current() async => Credential(userId: me, deviceId: uuid(3), accessToken: repairs > 0 ? '$token-rotated' : token);

  @override
  Future<void> refreshIfDue() => refresh?.call() ?? Future.value();

  @override
  Future<bool> onSyncUnauthorized(Credential presented) async {
    repairs++;
    return repair;
  }

  @override
  void attach(CredentialHost host) {}
  @override
  Future<bool> withholdDial() async => false;
  @override
  Future<bool> withholdSync() async => false;
  @override
  bool get refused => false;
  @override
  Future<void> beforeDial() async {}
  @override
  void answered() {}
  @override
  CredentialStatus status() => const CredentialStatus();
}

final class Rig {
  Rig(this.ctx, this.uploader, this.calls, this.clock);

  final Ctx ctx;
  final HttpUploader uploader;
  final List<UploadExchange> calls;
  final FakeClock clock;
}

Future<Rig> rig(
  Future<HttpAnswer> Function(UploadExchange x, int call) answer, {
  Cred? cred,
  OutboxStore? store,
  bool open = false,
}) async {
  final s = store ?? MemoryOutboxStore();
  final clock = FakeClock();
  final calls = <UploadExchange>[];
  final uploader = HttpUploader(
    baseUrl: base,
    credential: cred ?? Cred(),
    store: s,
    timers: clock,
    fetch: (x) {
      calls.add(x);
      return answer(x, calls.length);
    },
  );
  final ctx = await context(store: s, uploader: uploader, clock: clock);
  if (open) {
    ctx.transport.open();
    await flush();
  }
  return Rig(ctx, uploader, calls, clock);
}

var _n = 0;

/// An entry as the store holds one, with its media: what the outbox hands the
/// uploader once it has composed.
Future<OutboxEntry> held(Rig r, List<OutboundAttachmentDraft> drafts) async {
  final media = [for (final d in drafts) await d.source!.read()];
  return r.ctx.store.add(
    OutboxEntry(
      clientId: uuid(500 + ++_n),
      accountId: me,
      conversationId: conv,
      order: _n,
      composedAt: '2026-09-29T12:00:00.000Z',
      attachments: [for (final d in drafts) d.held()],
    ),
    media,
  );
}

OutboundAttachmentDraft image(Uint8List bytes, [String? filename]) => OutboundAttachmentDraft(kind: 'image', source: MediaBytes(bytes), filename: filename);

Future<String> rejection(Future<Object?> f) async {
  try {
    await f;
  } catch (e) {
    return '$e';
  }
  throw StateError('expected a rejection');
}

String? failure(OutboxEntry e) => e.lastError is UploadFailure ? (e.lastError as UploadFailure).message : null;

void main() {
  group('the request', () {
    test('is one POST /uploads?kind=image&filename=… carrying a bearer and the raw bytes, and completes with the upload_id', () async {
      final bytes = jpegLike();
      final r = await rig((x, _) async => created());
      final e = await held(r, [image(bytes, 'a b&c.jpg')]);
      expect(await r.uploader.upload(e, e.attachments[0]), handle);
      expect(r.calls, hasLength(1));
      final x = r.calls.single;
      expect(x.url.toString(), '$base/uploads?kind=image&filename=a+b%26c.jpg');
      expect(x.headers['Authorization'], 'Bearer $token');
      expect(x.body, bytes, reason: 'raw bytes, byte for byte, not a string');
    });

    test('a voice attachment is kind=voice with no filename when it has none', () async {
      final r = await rig((x, _) async => created());
      final e = await held(r, [OutboundAttachmentDraft(kind: 'voice', source: MediaBytes(recording()), durationMs: 900)]);
      await r.uploader.upload(e, e.attachments[0]);
      expect(r.calls.single.url.toString(), '$base/uploads?kind=voice');
    });

    test('the bytes are the store\'s, read by clientId and index: a second attachment sends its own', () async {
      final r = await rig((x, _) async => created());
      final a = jpegLike(100), b = jpegLike(200);
      final e = await held(r, [image(a), image(b)]);
      await r.uploader.upload(e, e.attachments[1]);
      expect(r.calls.single.body, b);
    });

    test('a 201 whose body has no valid upload_id rejects', () async {
      for (final body in ['', 'not json', '{}', '{"upload_id":7}', '{"upload_id":"nope"}', '[]']) {
        final r = await rig((x, _) async => HttpAnswer(201, body));
        final e = await held(r, [image(jpegLike(10))]);
        expect(await rejection(r.uploader.upload(e, e.attachments[0])), uploadDidNotFinish, reason: body);
      }
    });

    test('a missing attachment index or null media throws, and makes no request', () async {
      final r = await rig((x, _) async => created());
      final e = await held(r, [image(jpegLike(10))]);
      final stranger = OutboundAttachmentDraft(kind: 'image');
      expect(await rejection(r.uploader.upload(e, stranger)), uploadMediaMissing);
      // The store forgets the media: a null read.
      await r.ctx.store.delete(e.clientId);
      expect(await rejection(r.uploader.upload(e, e.attachments[0])), uploadMediaMissing);
      expect(r.calls, isEmpty);
    });
  });

  group('error mapping', () {
    final cases = <String, (HttpAnswer, String)>{
      '413': (const HttpAnswer(413, '{"code":"too_large"}'), uploadImageTooLarge),
      '415': (const HttpAnswer(415, '{"code":"unsupported"}'), uploadImageType),
      '404, whatever the body (a server with no route)': (const HttpAnswer(404, '404 page not found'), uploadUnavailable),
      '500': (const HttpAnswer(500, 'oops'), uploadServerTrouble),
      '502 from a hop': (const HttpAnswer(502, '<html>Bad gateway</html>'), uploadServerTrouble),
      '401 after the repair': (const HttpAnswer(401, '{"code":"unauthorized"}'), uploadSignedOut),
      'a 200 that is a portal page': (const HttpAnswer(200, '<html>log in to wifi</html>'), uploadDidNotFinish),
      'a 403 that is not Catenary\'s': (const HttpAnswer(403, 'forbidden'), uploadDidNotFinish),
      'a 400 that is Catenary\'s': (const HttpAnswer(400, '{"code":"bad_request"}'), uploadRefusedGeneric),
    };
    for (final c in cases.entries) {
      test('${c.key} → its own words, and never the token', () async {
        final r = await rig((x, _) async => c.value.$1);
        final e = await held(r, [image(jpegLike(10))]);
        final msg = await rejection(r.uploader.upload(e, e.attachments[0]));
        expect(msg, c.value.$2);
        expect(msg, isNot(contains(token)));
      });
    }

    test('a network error is the did-not-finish words, with nothing of the error in them', () async {
      final r = await rig((x, _) => Future.error(SocketException('connect failed to https://x?t=$token')));
      final e = await held(r, [image(jpegLike(10))]);
      final msg = await rejection(r.uploader.upload(e, e.attachments[0]));
      expect(msg, uploadDidNotFinish);
      expect(msg, isNot(contains(token)));
    });

    test('the words are distinct where the meaning is', () {
      final words = [uploadDidNotFinish, uploadSignedOut, uploadUnavailable, uploadServerTrouble, uploadImageTooLarge, uploadImageType];
      expect(words.toSet(), hasLength(words.length));
    });

    test('a voice 413 and 415 say recording, not image', () async {
      for (final (status, want) in [(413, uploadVoiceTooLarge), (415, uploadVoiceType)]) {
        final r = await rig((x, _) async => HttpAnswer(status, '{"code":"x"}'));
        final e = await held(r, [OutboundAttachmentDraft(kind: 'voice', source: MediaBytes(recording()))]);
        expect(await rejection(r.uploader.upload(e, e.attachments[0])), want);
      }
    });

    test('Catenary\'s own 401 is repaired once and retried with the new pair; a second is signed out', () async {
      final cred = Cred(repair: true);
      final r = await rig((x, n) async => n == 1 ? const HttpAnswer(401, '{"code":"unauthorized"}') : created(), cred: cred);
      final e = await held(r, [image(jpegLike(10))]);
      expect(await r.uploader.upload(e, e.attachments[0]), handle);
      expect(r.calls.map((c) => c.headers['Authorization']), ['Bearer $token', 'Bearer $token-rotated']);

      final always = Cred(repair: true);
      final r2 = await rig((x, n) async => const HttpAnswer(401, '{"code":"unauthorized"}'), cred: always);
      final e2 = await held(r2, [image(jpegLike(10))]);
      expect(await rejection(r2.uploader.upload(e2, e2.attachments[0])), uploadSignedOut);
      expect(r2.calls, hasLength(2), reason: 'one retry and no more');
    });

    test('a 401 from a hop (no Catenary body) is not a repair: it did not finish', () async {
      final cred = Cred(repair: true);
      final r = await rig((x, n) async => const HttpAnswer(401, ''), cred: cred);
      final e = await held(r, [image(jpegLike(10))]);
      expect(await rejection(r.uploader.upload(e, e.attachments[0])), uploadDidNotFinish);
      expect(cred.repairs, 0);
    });
  });

  group('the bound (ruling 2 → A)', () {
    test('is 30 s plus the bytes at 32 KiB/s, capped at 10 min', () {
      expect(uploadDeadline(0), 30000);
      expect(uploadDeadline(100 * 1000), inInclusiveRange(33000, 33100), reason: 'about 33 s for 100 KB');
      expect(uploadDeadline(1024 * 1024), 30000 + 32000);
      expect(uploadDeadline(16 * 1024 * 1024), 30000 + 512000, reason: 'the image cap is under the ceiling');
      expect(uploadDeadline(20 * 1024 * 1024), 600000);
      expect(uploadDeadline(1 << 40), 600000);
    });

    test('a server that never answers rejects at the deadline, aborts the request, and not before', () async {
      final r = await rig((x, _) => Completer<HttpAnswer>().future);
      final e = await held(r, [image(Uint8List(100 * 1000))]);
      Object? out;
      unawaited(r.uploader.upload(e, e.attachments[0]).then((_) {}, onError: (Object x) => out = x));
      await r.clock.advance(32000);
      expect(out, isNull, reason: 'still within 100 KB\'s 33 s');
      var aborted = false;
      unawaited(r.calls.single.abort.then((_) => aborted = true));
      await r.clock.advance(1500);
      expect('$out', uploadDidNotFinish);
      expect(aborted, isTrue, reason: 'the request is torn down, not abandoned');
    });

    test('a credential refresh that never completes is inside the bound', () async {
      final cred = Cred(refresh: () => Completer<void>().future);
      final r = await rig((x, _) async => created(), cred: cred);
      final e = await held(r, [image(Uint8List(100 * 1000))]);
      Object? out;
      unawaited(r.uploader.upload(e, e.attachments[0]).then((_) {}, onError: (Object x) => out = x));
      await r.clock.advance(34000);
      expect('$out', uploadDidNotFinish);
      expect(r.calls, isEmpty, reason: 'no request was ever made');
    });

    test('through a real Outbox the entry reads failed with the words, RETRY offered, never left pending', () async {
      final r = await rig((x, _) => Completer<HttpAnswer>().future, open: true);
      final e = await r.ctx.outbox.compose(imageDraft(Uint8List(100 * 1000)));
      await flush();
      expect(r.ctx.item(e.clientId)!.state, OutboxState.queued, reason: 'in flight');
      await r.clock.advance(34000);
      await flush();
      final item = r.ctx.item(e.clientId)!;
      expect(item.state, OutboxState.failed);
      expect(errorText(item.entry.lastError), uploadDidNotFinish);
      await within('retry', r.ctx.outbox.retry(e.clientId));
      await flush();
      expect(r.calls, hasLength(2), reason: 'RETRY is offered and uploads again');
    });
  });

  group('RETRY after an upload failure', () {
    test('on a two-attachment entry uploads BOTH again, under the same clientId (CANT-36 §10 item 4)', () async {
      var fail = true;
      final r = await rig((x, n) async {
        if (fail && x.body.length == jpegLike(200).length) return const HttpAnswer(500, '');
        return created(uuid(1000 + n));
      }, open: true);
      final e = await r.ctx.outbox.compose(OutboxDraft(conversationId: conv, attachments: [
        OutboundAttachmentDraft(kind: 'image', source: MediaBytes(jpegLike(100))),
        OutboundAttachmentDraft(kind: 'image', source: MediaBytes(jpegLike(200))),
      ]));
      await flush();
      expect(r.calls, hasLength(2), reason: 'first attachment up, second refused');
      expect(r.ctx.item(e.clientId)!.state, OutboxState.failed);

      fail = false;
      r.calls.clear();
      await within('retry', r.ctx.outbox.retry(e.clientId));
      await flush();
      expect(r.calls, hasLength(2), reason: 'the attachment that went up before the failure goes up again');
      expect(r.calls.map((c) => c.body.length).toSet(), {jpegLike(100).length, jpegLike(200).length});
      final sent = r.ctx.transport.framesFor(e.clientId);
      expect(sent, hasLength(1), reason: 'and the one message goes out under the entry\'s own clientId');
    });
  });
}
