// Catenary — the Flutter client's entrypoint.
//
// The app is two full-screen panes, the rail and the thread, as the narrow
// canvas has it. They are fed by fixtures (fixtures.dart) until the app is
// connected to `catenary_client` (CANT-200); the YOU tab opens the token and
// state specimens, which is how a design change is looked at on a device.

import 'dart:async';

import 'package:flutter/material.dart';

import 'fixtures.dart';
import 'rail.dart';
import 'specimen.dart';
import 'store/connection.dart';
import 'store/conversation.dart';
import 'theme.dart';
import 'thread.dart';

void main() => runApp(const CatenaryApp());

class CatenaryApp extends StatefulWidget {
  const CatenaryApp({super.key, this.initialMode = ThemeMode.system, this.clock = DateTime.now});

  /// Dark is primary. `system` follows the device, which is what ships; a
  /// test names the theme it wants.
  final ThemeMode initialMode;

  /// What time it is. Every stamp on screen is relative to this, so a test
  /// moves it rather than editing fixtures.
  final DateTime Function() clock;

  @override
  State<CatenaryApp> createState() => _CatenaryAppState();
}

class _CatenaryAppState extends State<CatenaryApp> {
  late ThemeMode _mode = widget.initialMode;

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Catenary',
      debugShowCheckedModeBanner: false,
      theme: catenaryTheme(Brightness.light),
      darkTheme: catenaryTheme(Brightness.dark),
      themeMode: _mode,
      // A theme change is a swap between two fixed tables (tokens.dart).
      themeAnimationDuration: Duration.zero,
      home: _Shell(clock: widget.clock, mode: _mode, onMode: (m) => setState(() => _mode = m)),
    );
  }
}

class _Shell extends StatefulWidget {
  const _Shell({required this.clock, required this.mode, required this.onMode});

  final DateTime Function() clock;
  final ThemeMode mode;
  final ValueChanged<ThemeMode> onMode;

  @override
  State<_Shell> createState() => _ShellState();
}

class _ShellState extends State<_Shell> {
  late DateTime _now = widget.clock();
  late final List<ConversationView> _conversations = fixtureConversations(_now);
  String? _openId;
  Timer? _minute;

  @override
  void initState() {
    super.initState();
    // A stamp is relative to now, so the rail is rebuilt as now moves: 23:59's
    // clock is a weekday a minute later.
    _minute = Timer.periodic(const Duration(minutes: 1), (_) => setState(() => _now = widget.clock()));
  }

  @override
  void dispose() {
    _minute?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return RailScreen(
      conversations: _conversations,
      now: _now,
      connection: const ConnectionView.live(),
      myInitials: 'HB',
      openId: _openId,
      onOpen: (c) {
        setState(() => _openId = c.id);
        Navigator.of(context).push(MaterialPageRoute<void>(
          builder: (_) => ThreadScreen(conversation: c, connection: const ConnectionView.live()),
        ));
      },
      onYou: () => Navigator.of(context).push(MaterialPageRoute<void>(
        builder: (context) => SpecimenScreen(mode: widget.mode, onMode: widget.onMode),
      )),
    );
  }
}
