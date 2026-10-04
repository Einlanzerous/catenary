/// `enrollDevice` against a scripted `HttpFetch`: the twin of what
/// web/src/transport/enroll.ts does, held to the three behaviors the plan names
/// (CANT-200 criterion 1): the request, the clock offset learned from the
/// response's `Date`, and a refusal that carries its status.
library;

import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'credential_test.dart' show accessTokenNo, firstRefresh, minute, wireTime;
import 'harness.dart';

const enrollmentToken = 'enrollment_token_FIXTURE_seven_day_lifetime';

/// A server that answers `/enroll` with [answer] and records what it was sent.
final class ScriptedEnroll {
  ScriptedEnroll(this.answer);

  final HttpAnswer Function(HttpExchange x) answer;
  final requests = <HttpExchange>[];

  Future<HttpAnswer> fetch(HttpExchange x) async {
    requests.add(x);
    return answer(x);
  }
}

String pairBody({num servedAt = 0}) => jsonEncode(
  EnrollResponse(
    userId: me,
    deviceId: device,
    accessToken: accessTokenNo(1),
    accessExpiresAt: wireTime(servedAt + 15 * minute),
    refreshToken: firstRefresh,
    refreshExpiresAt: wireTime(servedAt + 30 * 24 * 60 * minute),
  ).toJson(),
);

void main() {
  test('criterion 1 · enrollDevice posts the generated EnrollRequest encoding to <baseUrl>/enroll, trailing slashes removed', () async {
    final server = ScriptedEnroll((_) => HttpAnswer(200, pairBody()));
    await enrollDevice(
      EnrollOptions(baseUrl: 'https://catenary.test///', fetch: server.fetch, now: () => epoch),
      enrollmentToken,
      'Android phone',
    );
    final x = server.requests.single;
    expect(x.method, 'POST');
    expect(x.url.toString(), 'https://catenary.test/enroll');
    expect(x.headers['Content-Type'], 'application/json');
    expect(jsonDecode(x.body!), EnrollRequest(enrollmentToken: enrollmentToken, deviceName: 'Android phone').toJson());
    expect(jsonDecode(x.body!), {'enrollment_token': enrollmentToken, 'device_name': 'Android phone'});
  });

  test('criterion 1 · enrollDevice returns a StoredCredential whose clock offset comes from the response Date', () async {
    // The server's clock is ten minutes ahead of the device's. HTTP dates have
    // second resolution, so both instants are whole seconds.
    final served = epoch + 10 * minute;
    final date = HttpDate.format(DateTime.fromMillisecondsSinceEpoch(served, isUtc: true));
    final server = ScriptedEnroll((_) => HttpAnswer(200, pairBody(servedAt: served), {'date': date}));
    final c = await enrollDevice(
      EnrollOptions(baseUrl: Rig.baseUrl, fetch: server.fetch, now: () => epoch),
      enrollmentToken,
      'Linux desktop',
    );
    expect(c.userId, me);
    expect(c.deviceId, device);
    expect(c.accessToken, accessTokenNo(1));
    expect(c.refreshToken, firstRefresh);
    expect(c.accessIssuedAt, served);
    expect(c.clockOffsetMs, 10 * minute);
    expect(c.accessExpiresAt, served + 15 * minute);
    expect(c.chain, isEmpty);

    // No usable Date: no offset and no issue time, which reads against the floor.
    final bare = ScriptedEnroll((_) => HttpAnswer(200, pairBody()));
    final b = await enrollDevice(EnrollOptions(baseUrl: Rig.baseUrl, fetch: bare.fetch, now: () => epoch), enrollmentToken, 'x');
    expect(b.accessIssuedAt, isNull);
    expect(b.clockOffsetMs, 0);
  });

  test('criterion 1 · enrollDevice throws EnrollRefused carrying the status on any non-200', () async {
    for (final status in [400, 401, 403, 404, 429, 500, 503]) {
      final server = ScriptedEnroll((_) => HttpAnswer(status, '{"error":"refused"}'));
      await expectLater(
        enrollDevice(EnrollOptions(baseUrl: Rig.baseUrl, fetch: server.fetch, now: () => epoch), enrollmentToken, 'x'),
        throwsA(isA<EnrollRefused>().having((e) => e.status, 'status', status)),
        reason: 'HTTP $status',
      );
    }
  });

  test('criterion 1 · a 200 that is not a pair is a decode failure, not a credential', () async {
    final server = ScriptedEnroll((_) => HttpAnswer(200, '{"user_id":"nope"}'));
    await expectLater(
      enrollDevice(EnrollOptions(baseUrl: Rig.baseUrl, fetch: server.fetch, now: () => epoch), enrollmentToken, 'x'),
      throwsA(isNot(isA<EnrollRefused>())),
    );
  });
}
