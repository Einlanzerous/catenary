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

/// `POST /enroll` answered with a status other than 200. Only 400 and 401 are
/// Catenary refusing (internal/api/router.go), and every 401 answers
/// identically by design (CANT-28); any other status is the server, or a hop
/// in front of it, failing to answer.
final class EnrollRefused implements Exception {
  const EnrollRefused(this.status);

  final int status;

  @override
  String toString() => 'enroll: HTTP $status';
}

/// `POST /enroll` answered 200 and the answer could not be read as a pair: the
/// body was not JSON, or was not an `EnrollResponse`. NOT a refusal and NOT an
/// unreachable server: a status arrived. Whether the token is spent is unknown
/// from here: Catenary's own 200 has spent it, and a 200 from a hop in front
/// of Catenary (a captive portal's page) has not. [cause] is what failed, for
/// a log; a screen should not show it.
final class EnrollAnswerUnreadable implements Exception {
  const EnrollAnswerUnreadable(this.cause);

  final Object cause;

  @override
  String toString() => 'enroll: HTTP 200 with an answer that could not be read';
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
  // THE STATUS IS THE ANSWER. Past a 200 nothing that fails is "the server
  // could not be reached" (CANT-228): it is typed, an `Error` from the decode
  // included, so the caller can say which it was.
  if (res.status != 200) throw EnrollRefused(res.status);
  try {
    return credentialFromEnroll(EnrollResponse.fromJson(jsonDecode(res.body)), res.headers['date'], arrived);
  } on Object catch (e) {
    throw EnrollAnswerUnreadable(e);
  }
}
