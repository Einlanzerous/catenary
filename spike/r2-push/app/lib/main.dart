// R2 / IDEA-25 — FCM delivery probe.
//
// This is a measuring instrument, not an app. Its only job is to answer:
// does a high-priority DATA message wake a cold, Doze-sleeping device, how
// fast, and how reliably over hours?
//
// MEASUREMENT DESIGN, because the obvious approach is wrong:
//
// The tempting thing is to put the send time in the payload and have the phone
// compute `now - sent_at`. That measures clock skew as much as latency. Phones
// NTP-sync, but not perfectly, and our threshold is "within seconds" — a 2 s
// skew would swamp the result and we would never know.
//
// So the SERVER times a round trip instead: it records T0 when it hands the
// message to FCM, the phone calls back the instant the handler runs, and the
// server records T1. `T1 - T0` is an UPPER BOUND on delivery latency, measured
// entirely on one clock, and it is wrong only in the safe direction — it
// includes the callback's own network hop. The phone's own clock is reported
// alongside purely as a cross-check, never as the headline number.
//
// The handler also records its wall-clock entry time BEFORE doing anything
// else, so a slow HTTP callback cannot be misread as slow delivery.

import 'dart:convert';
import 'dart:io';

import 'package:firebase_core/firebase_core.dart';
import 'package:firebase_messaging/firebase_messaging.dart';
import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;

/// Where the Go collector lives. Overridden at build time with
/// --dart-define=COLLECTOR=https://...
const collector = String.fromEnvironment('COLLECTOR', defaultValue: '');

/// Shared bearer token for the collector, injected at build time. The collector
/// gets a public hostname for the duration of the test and must not be open.
const collectorToken = String.fromEnvironment('COLLECTOR_TOKEN', defaultValue: '');

Map<String, String> get _headers => {
      'content-type': 'application/json',
      if (collectorToken.isNotEmpty) 'authorization': 'Bearer $collectorToken',
    };

/// Reports one delivery back to the collector.
///
/// [entryMs] is when the handler STARTED, captured before any work, so the
/// server can separate "the push was slow" from "the callback was slow".
Future<void> _report(RemoteMessage m, int entryMs, String path) async {
  // LOG FIRST, NETWORK SECOND. This line is the primary evidence that a message
  // arrived; the HTTP callback is only how it gets timed.
  //
  // The first version of this probe reported delivery ONLY by calling the
  // collector, and swallowed the error if that call failed. On a device whose
  // DNS could not resolve the collector's hostname, that made "FCM never
  // delivered" and "FCM delivered but the callback failed" completely
  // indistinguishable — and produced a confident 0/9 that was measuring the
  // instrument, not the phone. Same class of mistake as a watcher whose regex
  // never matches: silence looked like a result.
  //
  // print() reaches logcat in release builds under the `flutter` tag, so
  // delivery is now observable over adb even with no working network at all.
  print('CATENARY_PROBE receipt probe_id=${m.data['probe_id']} '
      'path=$path entry_ms=$entryMs sent_at_ms=${m.data['sent_at_ms']}');

  if (collector.isEmpty) return;
  final body = jsonEncode({
    'probe_id': m.data['probe_id'] ?? '',
    'sent_at_ms': m.data['sent_at_ms'] ?? '',
    // The phone's own clock. Diagnostic only — see the note above.
    'device_entry_ms': entryMs,
    'device_now_ms': DateTime.now().millisecondsSinceEpoch,
    // FCM's own view of when it accepted the message, when present.
    'fcm_sent_time': m.sentTime?.millisecondsSinceEpoch,
    'path': path,
    'message_id': m.messageId ?? '',
  });
  // Retry briefly: in Doze the network is briefly unavailable even under the
  // high-priority temporary allowlist, and a single attempt would drop the
  // timing for a message that genuinely arrived.
  for (var attempt = 1; attempt <= 3; attempt++) {
    try {
      final r = await http
          .post(Uri.parse('$collector/receipt'),
              headers: _headers, body: body)
          .timeout(const Duration(seconds: 15));
      print('CATENARY_PROBE callback probe_id=${m.data['probe_id']} '
          'attempt=$attempt status=${r.statusCode}');
      return;
    } catch (e) {
      // Logged, never swallowed — a failing callback must be visible as a
      // callback failure rather than looking like a missing message.
      print('CATENARY_PROBE callback_failed probe_id=${m.data['probe_id']} '
          'attempt=$attempt error=$e');
      await Future<void>.delayed(Duration(seconds: attempt * 2));
    }
  }
}

/// Background/terminated-state handler. MUST be a top-level function and MUST
/// carry the vm:entry-point pragma, or the tree-shaker drops it in release and
/// the app silently receives nothing in exactly the state we care about.
@pragma('vm:entry-point')
Future<void> _onBackground(RemoteMessage message) async {
  final entry = DateTime.now().millisecondsSinceEpoch;
  await Firebase.initializeApp();
  await _report(message, entry, 'background');
}

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await Firebase.initializeApp();
  FirebaseMessaging.onBackgroundMessage(_onBackground);
  runApp(const ProbeApp());
}

class ProbeApp extends StatefulWidget {
  const ProbeApp({super.key});
  @override
  State<ProbeApp> createState() => _ProbeAppState();
}

class _ProbeAppState extends State<ProbeApp> {
  String _token = 'requesting…';
  String _registered = '';
  int _foreground = 0;

  @override
  void initState() {
    super.initState();
    _setup();
  }

  Future<void> _setup() async {
    // Android 13+ gates notifications behind a runtime permission. A data-only
    // message does not need it, but request it anyway: the real client will,
    // and a denied permission changes delivery behaviour on some OEM builds.
    await FirebaseMessaging.instance.requestPermission();

    FirebaseMessaging.onMessage.listen((m) {
      final entry = DateTime.now().millisecondsSinceEpoch;
      setState(() => _foreground++);
      _report(m, entry, 'foreground');
    });

    final token = await FirebaseMessaging.instance.getToken();
    setState(() => _token = token ?? 'null');
    await _register(token);

    // A token can rotate at any time; a probe that missed a rotation would look
    // exactly like a probe that stopped receiving.
    FirebaseMessaging.instance.onTokenRefresh.listen((t) {
      setState(() => _token = t);
      _register(t);
    });
  }

  Future<void> _register(String? token) async {
    if (token == null || collector.isEmpty) return;
    try {
      final r = await http.post(Uri.parse('$collector/register'),
          headers: _headers,
          body: jsonEncode({
            'token': token,
            'model': Platform.operatingSystemVersion,
          }));
      setState(() => _registered = 'registered: HTTP ${r.statusCode}');
    } catch (e) {
      setState(() => _registered = 'register failed: $e');
    }
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      debugShowCheckedModeBanner: false,
      theme: ThemeData.dark(useMaterial3: true),
      home: Scaffold(
        body: SafeArea(
          child: Padding(
            padding: const EdgeInsets.all(20),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                const Text('Catenary push probe',
                    style: TextStyle(fontSize: 22, fontWeight: FontWeight.w600)),
                const SizedBox(height: 4),
                Text('R2 / IDEA-25 · collector: ${collector.isEmpty ? "(unset)" : collector}',
                    style: const TextStyle(fontSize: 12, color: Colors.white54)),
                const SizedBox(height: 20),
                Text(_registered, style: const TextStyle(fontSize: 13)),
                const SizedBox(height: 12),
                Text('foreground messages: $_foreground',
                    style: const TextStyle(fontSize: 13)),
                const SizedBox(height: 20),
                const Text('FCM token',
                    style: TextStyle(fontSize: 12, color: Colors.white54)),
                const SizedBox(height: 4),
                SelectableText(_token,
                    style: const TextStyle(fontFamily: 'monospace', fontSize: 11)),
              ],
            ),
          ),
        ),
      ),
    );
  }
}
