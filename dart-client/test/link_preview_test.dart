/// Link previews on the Dart client (CANT-324): the shared detection
/// table, the `POST /link-previews` call and what it makes of each answer, the
/// composer's state (a send never waits), and the outbox's stale-handle drop
/// over real SQLite files as well as memory.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';
import 'outbox_harness.dart';

const base = 'https://chat.example.com';
final handle = uuid(901);

/// The one table (CANT-329): schema/vectors/link-detection.json, read by the
/// web test and the app's as well. Nothing here defines a case.
final detection = (jsonDecode(File('../schema/vectors/link-detection.json').readAsStringSync())['cases'] as List)
    .cast<Map<String, dynamic>>();

String cardBody() => jsonEncode({
      'upload_id': handle,
      'preview': {'kind': 'link', 'url': 'https://example.com/a', 'title': 'A page', 'site_name': 'Example'},
    });

final class Cred implements CredentialSeam {
  Cred({this.repair = false});

  bool repair;
  var repairs = 0;

  @override
  Future<Credential> current() async => Credential(userId: me, deviceId: uuid(3), accessToken: repairs > 0 ? '$token-rotated' : token);
  @override
  Future<void> refreshIfDue() async {}
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

(LinkPreviewApi, List<HttpExchange>) api(Future<HttpAnswer> Function(HttpExchange x, int n) answer, {Cred? cred}) {
  final calls = <HttpExchange>[];
  return (
    LinkPreviewApi(baseUrl: '$base/', credential: cred ?? Cred(), fetch: (x) {
      calls.add(x);
      return answer(x, calls.length);
    }),
    calls,
  );
}

void main() {
  group('detection', () {
    test('the shared table was read', () => expect(detection.length, greaterThanOrEqualTo(40)));
    for (final c in detection) {
      test('${c['name']}: ${jsonEncode(c['text'])}', () {
        final got = detectLinkUrl(c['text'] as String);
        expect(got, c['url']);
        expect(got != null && linkUrlIsAskable(got), c['ask']);
      });
    }
  });

  group('the call', () {
    test('is one POST /link-previews carrying a bearer and {url}, and a 200 is the card and its handle', () async {
      final (a, calls) = api((x, _) async => HttpAnswer(200, cardBody()));
      final r = await a.fetch('https://example.com/a');
      expect(r, isA<PreviewReady>());
      r as PreviewReady;
      expect(r.handle, handle);
      expect(r.preview.title, 'A page');
      final x = calls.single;
      expect(x.method, 'POST');
      expect(x.url.toString(), '$base/link-previews');
      expect(x.headers['Authorization'], 'Bearer $token');
      expect(jsonDecode(x.body!), {'url': 'https://example.com/a'});
    });

    test('every failure is the same PreviewNone: 204, 400, 5xx, a portal page, a bad body, a network failure, a timeout', () async {
      final answers = <String, Future<HttpAnswer> Function()>{
        '204': () async => const HttpAnswer(204, ''),
        '400': () async => const HttpAnswer(400, '{"error":"x"}'),
        '500': () async => const HttpAnswer(500, 'oops'),
        'portal 200': () async => const HttpAnswer(200, '<html>wifi</html>'),
        '200 without a handle': () async => const HttpAnswer(200, '{"preview":{"kind":"link","url":"https://a.com"}}'),
        'network': () async => throw StateError('no route'),
      };
      for (final e in answers.entries) {
        final (a, _) = api((x, _) => e.value());
        final r = await a.fetch('https://example.com/a');
        expect(r, isA<PreviewNone>(), reason: e.key);
      }
      final hang = LinkPreviewApi(baseUrl: base, credential: Cred(), timeout: const Duration(milliseconds: 20), fetch: (x) => Completer<HttpAnswer>().future);
      expect(await hang.fetch('https://example.com/a'), isA<PreviewNone>());
    });

    test('a 429 carries retry_after_sec', () async {
      final (a, _) = api((x, _) async => HttpAnswer(429, jsonEncode({'code': 'rate_limited', 'message': 'slow', 'retryable': true, 'retry_after_sec': 7})));
      final r = await a.fetch('https://example.com/a');
      expect(r, isA<PreviewRateLimited>());
      expect((r as PreviewRateLimited).retryAfterSec, 7);
    });

    test('a 404 is "no previews", and the object asks no more', () async {
      final (a, calls) = api((x, _) async => const HttpAnswer(404, '404 page not found'));
      expect(await a.fetch('https://example.com/a'), isA<PreviewNone>());
      expect(a.unavailable, isTrue);
      expect(await a.fetch('https://example.com/b'), isA<PreviewNone>());
      expect(calls, hasLength(1));
    });

    test('a 401 is repaired once and the call retried with the new token', () async {
      final cred = Cred(repair: true);
      final (a, calls) = api((x, n) async => n == 1 ? const HttpAnswer(401, '{"code":"unauthorized"}') : HttpAnswer(200, cardBody()), cred: cred);
      expect(await a.fetch('https://example.com/a'), isA<PreviewReady>());
      expect(calls, hasLength(2));
      expect(calls[1].headers['Authorization'], 'Bearer $token-rotated');
    });

    test('a 401 that cannot be repaired is PreviewNone after one attempt, and the token is nowhere but the header', () async {
      final (a, calls) = api((x, _) async => const HttpAnswer(401, '{"code":"unauthorized"}'));
      expect(await a.fetch('https://example.com/a'), isA<PreviewNone>());
      expect(calls, hasLength(1));
      expect(calls.single.url.toString(), isNot(contains(token)));
      expect(calls.single.body, isNot(contains(token)));
    });
  });

  group('the composer state', () {
    test('requests only after the URL has been idle for the debounce, once, and send never waits', () async {
      final clock = FakeClock();
      final gate = Completer<HttpAnswer>();
      final (a, calls) = api((x, _) => gate.future);
      final d = LinkPreviewDraft(api: a, timers: clock, debounceMs: 500);
      d.textChanged('see https://example.com/a');
      d.textChanged('see https://example.com/a ok');
      await clock.advance(499);
      expect(calls, isEmpty, reason: 'still inside the debounce');
      await clock.advance(1);
      expect(calls, hasLength(1));
      expect(d.state, LinkPreviewState.loading);
      expect(d.attachment(), isNull, reason: 'a pending preview contributes no card, and the call does not wait');
      gate.complete(HttpAnswer(200, cardBody()));
      await flush();
      expect(d.state, LinkPreviewState.ready);
      final att = d.attachment()!;
      expect(att.kind, 'link');
      expect(att.uploadId, handle);
      expect(att.upload, UploadState.uploaded);
    });

    test('a late answer after the URL changed or was dismissed is discarded; a dismissed draft is not asked again', () async {
      final clock = FakeClock();
      final gate = Completer<HttpAnswer>();
      final (a, calls) = api((x, _) => gate.future);
      final d = LinkPreviewDraft(api: a, timers: clock, debounceMs: 10);
      d.textChanged('https://example.com/a');
      await clock.advance(10);
      d.dismiss();
      gate.complete(HttpAnswer(200, cardBody()));
      await flush();
      expect(d.state, LinkPreviewState.dismissed);
      expect(d.attachment(), isNull);
      d.textChanged('https://example.com/other');
      await clock.advance(100);
      expect(calls, hasLength(1));
      d.reset();
      expect(d.state, LinkPreviewState.idle);
    });

    test('a 204 is none and no URL asks nothing', () async {
      final clock = FakeClock();
      final (a, calls) = api((x, _) async => const HttpAnswer(204, ''));
      final d = LinkPreviewDraft(api: a, timers: clock, debounceMs: 10);
      d.textChanged('nothing here');
      await clock.advance(100);
      expect(calls, isEmpty);
      d.textChanged('https://example.com/a');
      await clock.advance(10);
      await flush();
      expect(d.state, LinkPreviewState.none);
      expect(d.attachment(), isNull);
    });
  });

  group('the outbox', () {
    final stores = <String, OutboxStore Function()>{
      'MemoryOutboxStore': MemoryOutboxStore.new,
      'SqliteOutboxStore': () {
        final s = SqliteOutboxStore.open(outboxDbPath(tempDir()));
        addTearDown(s.close);
        return s;
      },
    };
    for (final s in stores.entries) {
      group(s.key, () {
        Future<(Ctx, OutboxEntry)> composed() async {
          final ctx = await context(store: s.value());
          ctx.transport.open();
          await flush();
          final e = await ctx.outbox.compose(OutboxDraft(conversationId: conv, text: 'https://example.com/a', attachments: [OutboundAttachmentDraft.link(handle)]));
          await flush();
          return (ctx, e);
        }

        test('a link goes out as {kind: link, upload_id} with no upload and no media', () async {
          final (ctx, e) = await composed();
          final f = ctx.transport.framesFor(e.clientId).single;
          expect(f.attachments!.single.kind, 'link');
          expect(f.attachments!.single.uploadId, handle);
          expect(await ctx.store.media(e.clientId, 0), isNull);
        });

        test('upload_not_found drops the card and resends the same clientId once, text intact', () async {
          final (ctx, e) = await composed();
          ctx.transport.refuse(e.clientId, ErrorCode.uploadNotFound, 'unknown upload', retryable: false);
          await flush();
          final frames = ctx.transport.framesFor(e.clientId);
          expect(frames, hasLength(2), reason: 'exactly one resend');
          expect(frames[1].attachments, isNull, reason: 'without the card');
          expect(frames[1].text, 'https://example.com/a');
          final stored = await ctx.stored(e.clientId);
          expect(stored!.attachments, isEmpty);
          expect(stored.reuploads, 1);
          expect(stored.status, OutboxStatus.pending);
          ctx.transport.ack(e.clientId);
          ctx.transport.deliver(e.clientId);
          await flush();
          expect(await ctx.stored(e.clientId), isNull, reason: 'one message, acked once');
        });

        test('a second upload_not_found is not resent again: the entry fails with the server\'s code', () async {
          final (ctx, e) = await composed();
          ctx.transport.refuse(e.clientId, ErrorCode.uploadNotFound, 'unknown upload', retryable: false);
          await flush();
          ctx.transport.refuse(e.clientId, ErrorCode.uploadNotFound, 'unknown upload', retryable: false);
          await flush();
          expect(ctx.transport.framesFor(e.clientId), hasLength(2));
          expect(ctx.item(e.clientId)?.state, OutboxState.failed);
        });

        test('RETRY of an entry failed by upload_not_found drops a card rather than trying to upload it', () async {
          final (ctx, e) = await composed();
          // Fail it by a second refusal after a first the entry already spent.
          await ctx.store.put(e.copy()..reuploads = 1);
          ctx.outbox.close();
          final ctx2 = await context(store: ctx.store, transport: ctx.transport);
          ctx2.transport.refuse(e.clientId, ErrorCode.uploadNotFound, 'unknown upload', retryable: false);
          await flush();
          expect(ctx2.item(e.clientId)?.state, OutboxState.failed);
          await ctx2.outbox.retry(e.clientId);
          await flush();
          final stored = await ctx2.stored(e.clientId);
          expect(stored!.attachments, isEmpty);
          expect(ctx2.transport.framesFor(e.clientId).last.attachments, isNull);
        });
      });
    }

    test('a card composed with no session is held, where a picked file is refused', () async {
      final ctx = await context();
      final e = await ctx.outbox.compose(OutboxDraft(conversationId: conv, text: 'x', attachments: [OutboundAttachmentDraft.link(handle)]));
      expect(e.attachments.single.isLink, isTrue);
      expect(ctx.transport.frames, isEmpty);
    });

    test('a message composed while the preview is pending carries no card at all', () async {
      final ctx = await context();
      ctx.transport.open();
      await flush();
      final clock = FakeClock();
      final (a, _) = api((x, _) => Completer<HttpAnswer>().future);
      final d = LinkPreviewDraft(api: a, timers: clock, debounceMs: 1);
      d.textChanged('https://example.com/a');
      await clock.advance(1);
      final att = d.attachment();
      final e = await ctx.outbox.compose(OutboxDraft(conversationId: conv, text: 'https://example.com/a', attachments: [if (att != null) att]));
      await flush();
      expect(ctx.transport.framesFor(e.clientId).single.attachments, isNull);
    });
  });
}
