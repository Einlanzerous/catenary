/// Enrollment over HTTP — mirrors web/src/transport/enroll.ts and
/// internal/client/enroll.go.
///
/// `POST /enroll` redeems an enrollment token and returns the device's first
/// pair, WITH the server's `Date` and the device's clock offset captured from
/// that response: CANT-31's record §1 names `/enroll` beside `/refresh` as the
/// two moments an expiry is learned. A pair built from the body alone has no
/// offset and reads an uncorrected clock against the 60 s floor until its first
/// rotation.
///
/// This obtains a credential and stores nothing. The caller persists what it
/// returns with `enrollCredential`, or `reenrollCredential` when replacing a
/// device the server stopped recognising.
library;

import 'dart:async';
import 'dart:convert';

import 'package:catenary_wire/catenary_wire.dart';

import 'credential_store.dart';
import 'io_seams.dart';
import 'seams.dart';

/// Where `/enroll` is, and the seams it goes through.
final class EnrollOptions {
  const EnrollOptions({required this.baseUrl, this.fetch, this.now});

  /// The server's origin; `/enroll` is appended.
  final String baseUrl;
  final HttpFetch? fetch;
  final Clock? now;
}

/// `POST /enroll` answered with something other than a pair. Every refusal
/// answers identically by design (CANT-28), so there is nothing finer to say.
final class EnrollRefused implements Exception {
  const EnrollRefused(this.status);

  final int status;

  @override
  String toString() => 'enroll: HTTP $status';
}

Future<StoredCredential> enrollDevice(EnrollOptions opts, String enrollmentToken, String deviceName) async {
  final fetch = opts.fetch ?? ioHttpFetch;
  final now = opts.now ?? systemClock;
  final res = await fetch(
    HttpExchange(
      method: 'POST',
      url: Uri.parse('${opts.baseUrl.replaceFirst(RegExp(r'/+$'), '')}/enroll'),
      headers: const {'Content-Type': 'application/json'},
      body: jsonEncode(EnrollRequest(enrollmentToken: enrollmentToken, deviceName: deviceName).toJson()),
      abort: Completer<void>().future,
    ),
  );
  final arrived = now();
  if (res.status != 200) throw EnrollRefused(res.status);
  return credentialFromEnroll(EnrollResponse.fromJson(jsonDecode(res.body)), res.headers['date'], arrived);
}
