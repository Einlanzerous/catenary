// Catenary — the Flutter client's entrypoint.
//
// The app is two full-screen panes, the rail and the thread, as the narrow
// canvas has it. On an enrolled device they read the store
// (store/app_store.dart): the journal and the outbox, projected. The YOU tab
// opens the token and state specimens, which is how a design change is looked
// at on a device.
//
// A DEVICE THAT IS NOT ENROLLED STILL SHOWS THE FIXTURES (fixtures.dart), and
// only until CANT-208 lands the enrollment screen: this row arrives first, and
// a build between the two with nothing to show and no way to enroll would be
// worse than the canvas. CANT-208 replaces that branch with its screen, and
// after it nothing here reads the fixtures.

import 'dart:async';

import 'package:flutter/material.dart';

import 'fixtures.dart';
import 'rail.dart';
import 'specimen.dart';
import 'store/app_store.dart';
import 'store/connection.dart';
import 'store/conversation.dart';
import 'store/platform.dart';
import 'theme.dart';
import 'thread.dart';

/// The store is started before the first frame: it reads two local files, and
/// what the first frame shows depends on whether this device is enrolled. A
/// database that will not open fails here, loudly, and draws nothing.
Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  final store = await deviceStore();
  await store.start();
  runApp(CatenaryApp(store: store));
}

class CatenaryApp extends StatefulWidget {
  const CatenaryApp({super.key, this.initialMode = ThemeMode.system, this.clock = DateTime.now, this.store});

  /// Dark is primary. `system` follows the device, which is what ships; a
  /// test names the theme it wants.
  final ThemeMode initialMode;

  /// What time it is. Every stamp on screen is relative to this, so a test
  /// moves it rather than editing fixtures.
  final DateTime Function() clock;

  /// What the panes read, already started. Without one, or with one that
  /// found no enrollment, they read the fixtures.
  final AppStore? store;

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
      home: _Shell(clock: widget.clock, mode: _mode, onMode: (m) => setState(() => _mode = m), store: widget.store),
    );
  }
}

class _Shell extends StatefulWidget {
  const _Shell({required this.clock, required this.mode, required this.onMode, required this.store});

  final DateTime Function() clock;
  final ThemeMode mode;
  final ValueChanged<ThemeMode> onMode;
  final AppStore? store;

  @override
  State<_Shell> createState() => _ShellState();
}

class _ShellState extends State<_Shell> {
  late DateTime _now = widget.clock();
  late final List<ConversationView> _fixtures = fixtureConversations(_now);
  String? _openId;
  Timer? _minute;

  // THE BANNER'S TWO ACTIONS ARE SEAMS HERE. Both are handed to the rail and
  // to every thread, which pass them to the banner: RETRY dials now
  // (`AppStore.retryNow`) and RE-ENROLL is CANT-208's (the enrollment screen).
  VoidCallback? get _onRetry => widget.store?.retryNow;
  VoidCallback? get _onReenroll => null;

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
    final store = widget.store;
    if (store == null) return _fixtureRail(context);
    return ListenableBuilder(
      listenable: store,
      builder: (context, _) => store.enrolled ? _storeRail(context, store) : _fixtureRail(context),
    );
  }

  Widget _storeRail(BuildContext context, AppStore store) {
    return RailScreen(
      conversations: store.conversations,
      now: _now,
      connection: store.connection,
      myInitials: store.myInitials,
      openId: _openId,
      onRetry: _onRetry,
      onReenroll: _onReenroll,
      onOpen: (c) {
        setState(() => _openId = c.id);
        Navigator.of(context).push(MaterialPageRoute<void>(
          // The thread is its own route, so it listens for itself. A
          // conversation the journal has since dropped keeps what was on
          // screen when it was opened.
          builder: (_) => ListenableBuilder(
            listenable: store,
            builder: (_, _) => ThreadScreen(
              conversation: store.conversation(c.id) ?? c,
              connection: store.connection,
              onRetry: _onRetry,
              onReenroll: _onReenroll,
              onSend: (text) => store.send(c.id, text),
              onRetryMessage: store.retry,
              onDiscardMessage: store.discard,
            ),
          ),
        ));
      },
      onYou: _openSpecimens,
    );
  }

  Widget _fixtureRail(BuildContext context) {
    return RailScreen(
      conversations: _fixtures,
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
      onYou: _openSpecimens,
    );
  }

  void _openSpecimens() => Navigator.of(context).push(MaterialPageRoute<void>(
        builder: (context) => SpecimenScreen(mode: widget.mode, onMode: widget.onMode),
      ));
}
