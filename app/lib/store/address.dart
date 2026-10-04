// Which server this install belongs to, and the rules for accepting one.
//
// THE WEB NEVER NEEDED THIS: it talks to `location.origin`. An installed app
// has no origin, so a person types one, and it is kept in a one-line file,
// `server`, beside `catenary.db` — a file and not a preferences plugin because
// a second isolate (E7's push handler) has to read it with no Flutter binding
// (CANT-200, "Enrollment and the server address"). No build carries a default
// address (ruling 1): nothing here reads a compile-time define, and
// test/address_test.dart fails the build on one anywhere under lib/.
//
// THE ADDRESS IS NORMALIZED AND PROBED BEFORE THE TOKEN IS SPENT. An enrollment
// token is single-use, so a mistyped host must not look like a refused one:
// `GET /healthz` consumes nothing, and a failure there is `CouldNotReach`.
//
// CLEARTEXT. A release build accepts only `https://` — [acceptAddress] takes
// the flag, set from `kReleaseMode` by its caller, and with it set refuses
// every `http://` address, loopback and private-range ones included. Without
// it `http://` is accepted for a loopback or private-range host only, which is
// what a development server on the LAN needs. The thread header's `TLS` is
// derived from the scheme stored here ([addressIsSecure]).

import 'dart:async';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';

/// The file's name, in the same directory as `catenary.db`.
const addressFileName = 'server';

/// Why [normalizeAddress] said no.
enum AddressRefusal {
  /// Nothing, or something that is not a host: no host, a scheme that is not
  /// http or https, credentials in it, a path, a query or a fragment.
  invalid,

  /// `http://`, in a release build.
  cleartextInRelease,

  /// `http://`, in a debug or profile build, to a host that is neither
  /// loopback nor in a private range.
  cleartextToPublicHost,
}

/// What [normalizeAddress] made of what was typed.
sealed class AddressCheck {
  const AddressCheck();
}

/// [origin] is `scheme://host[:port]` with nothing after it.
final class AddressOk extends AddressCheck {
  const AddressOk(this.origin);

  final String origin;
}

final class AddressRefused extends AddressCheck {
  const AddressRefused(this.refusal);

  final AddressRefusal refusal;
}

final _hasScheme = RegExp(r'^[a-z][a-z0-9+.-]*://', caseSensitive: false);

/// A bare host is read as `https://`. Trailing slashes are dropped; anything
/// else after the authority is refused rather than guessed at, because an
/// origin is everything the app ever appends to.
AddressCheck normalizeAddress(String typed, {required bool release}) {
  final text = typed.trim();
  if (text.isEmpty) return const AddressRefused(AddressRefusal.invalid);
  final Uri uri;
  try {
    uri = Uri.parse(_hasScheme.hasMatch(text) ? text : 'https://$text');
  } on FormatException {
    return const AddressRefused(AddressRefusal.invalid);
  }
  final scheme = uri.scheme.toLowerCase();
  if ((scheme != 'http' && scheme != 'https') ||
      uri.host.isEmpty ||
      uri.userInfo.isNotEmpty ||
      uri.hasQuery ||
      uri.hasFragment ||
      uri.path.replaceFirst(RegExp(r'/+$'), '').isNotEmpty) {
    return const AddressRefused(AddressRefusal.invalid);
  }
  if (scheme == 'http') {
    if (release) return const AddressRefused(AddressRefusal.cleartextInRelease);
    if (!isLocalHost(uri.host)) return const AddressRefused(AddressRefusal.cleartextToPublicHost);
  }
  final host = uri.host.contains(':') ? '[${uri.host}]' : uri.host;
  return AddressOk('$scheme://$host${uri.hasPort ? ':${uri.port}' : ''}');
}

/// Loopback or a private range: `localhost`, 127/8, 10/8, 172.16/12,
/// 192.168/16, 169.254/16, and for IPv6 `::1`, fc00::/7 and fe80::/10. A name
/// that is not `localhost` is not looked up, so `printer.local` is not local
/// here: it can say `http://` only by being typed as an address.
bool isLocalHost(String host) {
  if (host.toLowerCase() == 'localhost') return true;
  final ip = InternetAddress.tryParse(host);
  if (ip == null) return false;
  final b = ip.rawAddress;
  if (ip.type == InternetAddressType.IPv4) {
    return b[0] == 127 ||
        b[0] == 10 ||
        (b[0] == 172 && b[1] >= 16 && b[1] <= 31) ||
        (b[0] == 192 && b[1] == 168) ||
        (b[0] == 169 && b[1] == 254);
  }
  return ip.isLoopback || (b[0] & 0xfe) == 0xfc || (b[0] == 0xfe && (b[1] & 0xc0) == 0x80);
}

/// True when the stored address is `https://`, which is what lets the thread
/// header end in `TLS`; `http://` ends it in `CLEARTEXT`.
bool addressIsSecure(String origin) => origin.toLowerCase().startsWith('https://');

File _file(String directory) => File('$directory${directory.endsWith('/') ? '' : '/'}$addressFileName');

/// The stored origin, or null for no file or an empty one.
String? readAddress(String directory) {
  final f = _file(directory);
  if (!f.existsSync()) return null;
  final text = f.readAsStringSync().trim();
  return text.isEmpty ? null : text;
}

/// Writes through a temporary file and a rename, so a reader in another
/// isolate sees the old address or the new one and never half of either.
void writeAddress(String directory, String origin) {
  final target = _file(directory);
  final tmp = File('${target.path}.tmp')..writeAsStringSync('$origin\n', flush: true);
  tmp.renameSync(target.path);
}

/// `GET <origin>/healthz`: unauthenticated, dependency-free, consumes nothing.
/// Any 200 is yes; a refused connection, a timeout or any other status is no.
Future<bool> probeServer(String origin, HttpFetch fetch, {Duration timeout = const Duration(seconds: 10)}) async {
  final abort = Completer<void>();
  final timer = Timer(timeout, () {
    if (!abort.isCompleted) abort.complete();
  });
  try {
    final request = fetch(HttpExchange(method: 'GET', url: Uri.parse('$origin/healthz'), abort: abort.future));
    // An implementation tears its request down on `abort`, but this does not
    // wait for it to: the caller has given up, and says so.
    final answer = await Future.any([request, abort.future.then<HttpAnswer?>((_) => null)]);
    unawaited(request.then<void>((_) {}, onError: (Object _) {}));
    return answer?.status == 200;
  } on Object {
    return false;
  } finally {
    timer.cancel();
  }
}

/// What [acceptAddress] came to.
sealed class AddressResult<T> {
  const AddressResult();
}

/// The address was refused before anything was sent.
final class AddressRejected<T> extends AddressResult<T> {
  const AddressRejected(this.refusal);

  final AddressRefusal refusal;
}

/// `GET /healthz` failed. No `/enroll` request was made and no token was spent.
final class CouldNotReach<T> extends AddressResult<T> {
  const CouldNotReach(this.origin);

  final String origin;
}

/// The address is stored and [value] is what [acceptAddress]'s callback made
/// of it.
final class AddressAccepted<T> extends AddressResult<T> {
  const AddressAccepted(this.origin, this.value);

  final String origin;
  final T value;
}

/// Normalize [typed], probe it, write it to [directory], and only then run
/// [enroll] with the origin: the one place that order is held. [enroll] is
/// where `/enroll` is called (store/enrollment.dart), so no request to it can
/// precede the `server` file, and none is made at all when the probe fails.
///
/// The file is written before the token is spent so that an enrollment that
/// lands and is then lost to a crash leaves an app that knows where it
/// enrolled. IF THE ENROLLMENT THEN FAILS, the address that was there before
/// is put back (or the file removed, if there was none): a held credential
/// must never be paired with an address it was not issued by, and a failed
/// attempt at another host is not a move. [enroll] fails by throwing, or by
/// returning a value [enrolled] calls false.
Future<AddressResult<T>> acceptAddress<T>(
  String directory,
  String typed, {
  required bool release,
  required Future<T> Function(String origin) enroll,
  bool Function(T value)? enrolled,
  HttpFetch? fetch,
}) async {
  final check = normalizeAddress(typed, release: release);
  switch (check) {
    case AddressRefused(:final refusal):
      return AddressRejected(refusal);
    case AddressOk(:final origin):
      if (!await probeServer(origin, fetch ?? ioHttpFetch)) return CouldNotReach(origin);
      final before = readAddress(directory);
      writeAddress(directory, origin);
      var ok = false;
      try {
        final value = await enroll(origin);
        ok = enrolled?.call(value) ?? true;
        return AddressAccepted(origin, value);
      } finally {
        if (!ok) {
          if (before == null) {
            final f = _file(directory);
            if (f.existsSync()) f.deleteSync();
          } else {
            writeAddress(directory, before);
          }
        }
      }
  }
}
