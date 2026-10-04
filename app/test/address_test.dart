// The address functions (lib/store/address.dart), with no widget involved: a
// scripted HttpFetch stands in for the server and a temporary directory for
// the application-support directory.

import 'dart:async';
import 'dart:io';

import 'package:catenary/store/address.dart';
import 'package:catenary_client/catenary_client.dart';
import 'package:flutter_test/flutter_test.dart';

/// Answers by path and records every request, in order.
final class Scripted {
  Scripted({this.health = 200, this.onEnroll});

  /// The `/healthz` status; null makes the request fail as a refused
  /// connection does.
  int? health;
  void Function()? onEnroll;
  final requests = <String>[];

  Future<HttpAnswer> call(HttpExchange x) async {
    requests.add('${x.method} ${x.url}');
    if (x.url.path == '/healthz') {
      final h = health;
      if (h == null) throw const SocketException('connection refused');
      return HttpAnswer(h, '{}');
    }
    if (x.url.path == '/enroll') {
      onEnroll?.call();
      return const HttpAnswer(200, '{}');
    }
    return const HttpAnswer(404, '');
  }

  bool get enrolled => requests.any((r) => r.contains('/enroll'));
}

String origin(String typed, {bool release = false}) {
  final c = normalizeAddress(typed, release: release);
  expect(c, isA<AddressOk>(), reason: typed);
  return (c as AddressOk).origin;
}

AddressRefusal? refusal(String typed, {required bool release}) {
  final c = normalizeAddress(typed, release: release);
  return c is AddressRefused ? c.refusal : null;
}

void main() {
  late Directory dir;
  setUp(() => dir = Directory.systemTemp.createTempSync('catenary-address-'));
  tearDown(() => dir.deleteSync(recursive: true));

  group('a bare host is https', () {
    test('and so is one with a port, a trailing slash or surrounding space', () {
      expect(origin('chat.example.com'), 'https://chat.example.com');
      expect(origin('  chat.example.com/ '), 'https://chat.example.com');
      expect(origin('chat.example.com:8443'), 'https://chat.example.com:8443');
      expect(origin('192.168.1.10:4099', release: true), 'https://192.168.1.10:4099');
      expect(origin('HTTPS://Chat.Example.com//'), 'https://chat.example.com');
    });

    test('the stored file holds it', () async {
      final fetch = Scripted();
      final r = await acceptAddress<void>(dir.path, 'chat.example.com', release: true, fetch: fetch.call, enroll: (_) async {});
      expect(r, isA<AddressAccepted<void>>());
      expect(readAddress(dir.path), 'https://chat.example.com');
    });

    test('anything after the authority is refused', () {
      for (final s in ['', '   ', 'https://', 'ftp://chat.example.com', 'https://chat.example.com/app', 'https://chat.example.com/?a=1', 'https://chat.example.com/#enroll=x', 'https://me@chat.example.com']) {
        expect(refusal(s, release: false), AddressRefusal.invalid, reason: '"$s"');
      }
    });
  });

  group('a release build', () {
    test('refuses every http:// address, loopback and private ones included', () {
      for (final h in ['chat.example.com', 'localhost', '127.0.0.1', '10.0.0.5', '172.16.0.1', '192.168.1.10:4099', '[::1]', '[fd00::1]']) {
        expect(refusal('http://$h', release: true), AddressRefusal.cleartextInRelease, reason: h);
      }
    });

    test('accepts https://', () => expect(origin('https://chat.example.com', release: true), 'https://chat.example.com'));
  });

  group('a debug build', () {
    test('accepts http:// to a loopback or private-range host', () {
      for (final h in ['localhost', '127.0.0.1', '127.8.8.8', '10.0.0.5', '172.16.0.1', '172.31.255.1', '192.168.1.10:4099', '169.254.1.1', '[::1]', '[fd00::1]', '[fe80::1]']) {
        expect(refusal('http://$h', release: false), isNull, reason: h);
      }
      expect(origin('http://192.168.1.10:4099'), 'http://192.168.1.10:4099');
      expect(origin('http://[::1]:4099/'), 'http://[::1]:4099');
    });

    test('refuses http:// to any other host', () {
      for (final h in ['chat.example.com', 'printer.local', '8.8.8.8', '172.15.0.1', '172.32.0.1', '192.169.0.1', '11.0.0.1', '[2001:db8::1]']) {
        expect(refusal('http://$h', release: false), AddressRefusal.cleartextToPublicHost, reason: h);
      }
    });

    test('and the scheme it accepts is the one that is not secure', () {
      expect(addressIsSecure('http://192.168.1.10:4099'), isFalse);
      expect(addressIsSecure('https://chat.example.com'), isTrue);
    });
  });

  group('the server file and the probe', () {
    test('the file is written before any /enroll request is made', () async {
      final fetch = Scripted();
      String? atEnroll;
      fetch.onEnroll = () => atEnroll = readAddress(dir.path);
      final r = await acceptAddress<int>(
        dir.path,
        'https://chat.example.com',
        release: true,
        fetch: fetch.call,
        enroll: (o) async => (await fetch.call(HttpExchange(method: 'POST', url: Uri.parse('$o/enroll'), abort: Completer<void>().future))).status,
      );
      expect(atEnroll, 'https://chat.example.com', reason: 'the file existed when /enroll arrived');
      expect(fetch.requests, ['GET https://chat.example.com/healthz', 'POST https://chat.example.com/enroll']);
      expect(r, isA<AddressAccepted<int>>().having((a) => a.value, 'enroll result', 200));
    });

    test('a failed GET /healthz is could-not-reach, with no /enroll request and no file', () async {
      for (final health in <int?>[null, 503, 404]) {
        final fetch = Scripted(health: health);
        var enrolled = false;
        final r = await acceptAddress<void>(dir.path, 'https://chat.example.com', release: true, fetch: fetch.call, enroll: (_) async => enrolled = true);
        expect(r, isA<CouldNotReach<void>>(), reason: 'health $health');
        expect(enrolled, isFalse);
        expect(fetch.enrolled, isFalse);
        expect(fetch.requests, ['GET https://chat.example.com/healthz']);
        expect(readAddress(dir.path), isNull, reason: 'an address nobody reached is not remembered');
      }
    });

    test('a refused address makes no request at all', () async {
      final fetch = Scripted();
      final r = await acceptAddress<void>(dir.path, 'http://chat.example.com', release: true, fetch: fetch.call, enroll: (_) async {});
      expect(r, isA<AddressRejected<void>>().having((a) => a.refusal, 'refusal', AddressRefusal.cleartextInRelease));
      expect(fetch.requests, isEmpty);
      expect(readAddress(dir.path), isNull);
    });

    test('a probe that never answers gives up at its timeout', () async {
      Future<HttpAnswer> hung(HttpExchange x) => Completer<HttpAnswer>().future;
      expect(await probeServer('https://chat.example.com', hung, timeout: const Duration(milliseconds: 20)), isFalse);
    });
  });

  // RULING 1 → NO BUILD CARRIES AN ADDRESS. A compile-time define is exactly a
  // default address baked into the binary, so nothing under lib/ may read one.
  test('no file under lib reads a compile-time define', () {
    final offenders = [
      for (final f in Directory('lib').listSync(recursive: true).whereType<File>().where((f) => f.path.endsWith('.dart')))
        if (f.readAsStringSync().contains('fromEnvironment')) f.path,
    ];
    expect(offenders, isEmpty);
  });
}
