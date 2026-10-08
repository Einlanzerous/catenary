// Enrolling a device: the twin of `login` and `enrollErrorText` in
// web/src/account.ts, and the one place the app calls `/enroll`.
//
// THE ORDER IS THE ADDRESS FUNCTIONS' (store/address.dart `acceptAddress`):
// normalize, probe `GET /healthz`, write the `server` file, and only then is
// the token spent, so a mistyped host never looks like a refused token. A
// first enrollment on an empty store is `enrollCredential`; a store that
// already holds a pair, including one the server stopped recognizing (CANT-31
// §6's credential terminal), is `reenrollCredential`, which is the only thing
// that ends that terminal.
//
// RULING 0 → TWO FIELDS THAT ALSO ACCEPT A LINK. The form is an address and a
// token; [parseEnrollLink] is what lets a person paste `https://<host>/#enroll=<token>`
// into either and have both filled. RULING 1 → NO BUILD CARRIES AN ADDRESS:
// the address field starts empty on a first enrollment.

import 'package:catenary_client/catenary_client.dart';

import 'address.dart';

/// `checkToken` in the wire package: 43 characters of base64url.
final _token = RegExp(r'^[A-Za-z0-9_-]{43}$');

/// What a pasted link carries: the origin and the token.
typedef EnrollLink = ({String origin, String token});

/// `https://<host>/#enroll=<token>`, exactly: a scheme and a host, an optional
/// port, a path of `/` or none, and a fragment of `enroll=` and the 43-character
/// token. Anything else is not a link and is null, so the caller leaves it
/// where it was pasted. The origin is everything before `/#`; `http` is parsed
/// too, and [acceptAddress] is what refuses it in a release build.
EnrollLink? parseEnrollLink(String pasted) {
  final Uri uri;
  try {
    uri = Uri.parse(pasted.trim());
  } on FormatException {
    return null;
  }
  final scheme = uri.scheme.toLowerCase();
  if ((scheme != 'http' && scheme != 'https') ||
      uri.host.isEmpty ||
      uri.userInfo.isNotEmpty ||
      uri.hasQuery ||
      (uri.path != '' && uri.path != '/') ||
      !uri.fragment.startsWith('enroll=')) {
    return null;
  }
  final token = uri.fragment.substring('enroll='.length);
  if (!_token.hasMatch(token)) return null;
  final host = uri.host.contains(':') ? '[${uri.host}]' : uri.host;
  return (origin: '$scheme://$host${uri.hasPort ? ':${uri.port}' : ''}', token: token);
}

/// What the device-name field starts as, from `Platform.operatingSystem`.
/// Editable; no plugin is asked for a model name.
String defaultDeviceName(String operatingSystem) => switch (operatingSystem) {
      'android' => 'Android phone',
      'linux' => 'Linux desktop',
      _ => 'This device',
    };

/// How an enrollment came out.
sealed class EnrollOutcome {
  const EnrollOutcome();
}

/// The credential is stored and a session is starting on it.
final class Enrolled extends EnrollOutcome {
  const Enrolled(this.credential);

  final StoredCredential credential;
}

/// The address was refused, the probe failed, `/enroll` did not answer with a
/// pair, or this device could not keep the one it was given.
/// [text] is what the screen shows.
final class EnrollFailed extends EnrollOutcome {
  const EnrollFailed(this.text);

  final String text;
}

const _unreachable = 'Could not reach that server — check the address and your connection and try again.';

/// The server answered 200 and this device then failed to keep what it was
/// given. The token is spent by then, so "try again" would be refused: the
/// text says what happened and what is needed (CANT-220 ruling 8).
const _notStored =
    'The server accepted that token, but this device could not save the credential. The token is now used — ask whoever invited you for a fresh one.';

/// The credential store would not open, which is found out before any request
/// is made (CANT-222).
const enrollStorageText = 'This device could not open its own storage, so the token was not sent.';

/// The server answered 200 and the answer was not a pair. It does not claim
/// the token is spent: Catenary's own 200 has spent it, a 200 from a hop in
/// front of Catenary has not, and from here the two look the same.
const _unreadable =
    'The server answered, but not with anything this app could read. The token may already be used — if trying again is refused, ask whoever invited you for a fresh one.';

/// A status that is neither 200 nor Catenary refusing: its own 500, or a 502,
/// 404 or 429 from a hop in front of it. Nobody refused the token.
const _couldNotAnswer = 'The server could not answer just now — the token was not refused. Try again in a moment.';

/// Something on this device threw that is not a refusal and not the network:
/// an `Error`. It does not say whether the token was spent, because the app
/// does not know: the request may or may not have left.
const enrollDeviceFaultText =
    'This device hit an error while enrolling. Try again — if the token is then refused, it was used, and you need a fresh one from whoever invited you.';

/// A failure to store the credential after `/enroll` answered 200.
final class _NotStored implements Exception {
  const _NotStored();
}

String _refusalText(AddressRefusal r) => switch (r) {
      AddressRefusal.invalid => 'That does not look like a server address.',
      AddressRefusal.cleartextInRelease => 'This build only connects over https:// — use an https:// address.',
      AddressRefusal.cleartextToPublicHost => 'A plain http:// address is only accepted for this machine or a private network.',
    };

/// The web's texts, `enrollErrorText` in web/src/account.ts, told apart by
/// type and never by matching a message. Of the statuses only 400 and 401 are
/// Catenary refusing (internal/api/router.go): a 400 is a malformed token or
/// name, and every 401 answers identically on purpose (CANT-28), so it is one
/// message. Any other status is the server failing to answer, and does not get
/// to tell a person to throw away a token nobody refused. And one the web does
/// not have: an `Error` on this device.
String enrollErrorText(Object e) {
  if (e is _NotStored) return _notStored;
  if (e is EnrollAnswerUnreadable) return _unreadable;
  if (e is Error) return enrollDeviceFaultText;
  if (e is! EnrollRefused) return _unreachable;
  if (e.status == 400) return 'That does not look like a valid enrollment token or device name.';
  if (e.status != 401) return _couldNotAnswer;
  return 'That enrollment token was not accepted — it may be wrong, expired or already used. Ask whoever invited you for a fresh one.';
}

/// Spends [token] at [typedAddress] and stores what comes back in [store]. The
/// address is stored first and put back if the enrollment fails
/// ([acceptAddress]). Never throws: every way this can come out is an outcome.
Future<EnrollOutcome> enrollDeviceAt({
  required String directory,
  required CredentialStore store,
  required String typedAddress,
  required String token,
  required String deviceName,
  required bool release,
  HttpFetch? fetch,
}) async {
  final lock = inProcessLock();
  try {
    final result = await acceptAddress<StoredCredential>(
      directory,
      typedAddress,
      release: release,
      fetch: fetch,
      enroll: (origin) async {
        final stored = await enrollDevice(EnrollOptions(baseUrl: origin, fetch: fetch), token, deviceName);
        // Past this line the token is spent, and a failure is this device's.
        try {
          if (await store.read() == null) {
            await enrollCredential(store, lock, stored);
          } else {
            await reenrollCredential(store, lock, stored);
          }
        } on Object {
          // An `Error` too: whatever it was, the token is spent and the
          // credential is not kept, which is the one true thing to say.
          throw const _NotStored();
        }
        return stored;
      },
    );
    return switch (result) {
      AddressRejected(:final refusal) => EnrollFailed(_refusalText(refusal)),
      CouldNotReach() => const EnrollFailed(_unreachable),
      AddressAccepted(:final value) => Enrolled(value),
    };
  } on Object catch (e) {
    // The web rule: what is left once the typed cases are gone is a failure
    // before any status arrived, and that alone reads as unreachable. An
    // `Error` is this device's.
    return EnrollFailed(enrollErrorText(e));
  }
}
