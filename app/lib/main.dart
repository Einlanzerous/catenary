// Catenary — the Flutter client's entrypoint.
//
// The app is two full-screen panes, the rail and the thread, as the narrow
// canvas has it. On an enrolled device they read the store
// (store/app_store.dart): the journal and the outbox, projected. The YOU tab
// opens the token and state specimens, which is how a design change is looked
// at on a device.
//
// A DEVICE THAT IS NOT ENROLLED SHOWS THE ENROLLMENT SCREEN (enroll.dart), and
// nothing else. One that could not open what it keeps shows the failed-start
// screen (start_failed.dart), and is never mistaken for one that is not
// enrolled. The fixtures (fixtures.dart) are what the panes read only when
// the app is built without a store, which is how the canvas is looked at in a
// test.

import 'dart:async';
import 'dart:io' show Platform;

import 'package:flutter/foundation.dart' show kReleaseMode;
import 'package:flutter/material.dart';

import 'enroll.dart';
import 'fixtures.dart';
import 'rail.dart';
import 'specimen.dart';
import 'start_failed.dart';
import 'store/app_store.dart';
import 'store/connection.dart';
import 'store/conversation.dart';
import 'store/enrollment.dart';
import 'store/platform.dart';
import 'theme.dart';
import 'thread.dart';

/// The store is opened and started before the first frame: it reads two local
/// files, and what the first frame shows depends on whether this device is
/// enrolled. A FRAME IS ALWAYS DRAWN. A database that will not open, or a
/// directory the platform cannot name, is the failed-start screen
/// (start_failed.dart) and not an uncaught error with the launch screen left
/// up (CANT-222).
Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  runApp(await bootApp(deviceStore));
}

/// What `main` runs: the app over the store [open] builds, started. [open] is
/// a parameter so that a test can pass one that throws.
Future<Widget> bootApp(Future<AppStore> Function() open) async {
  final (store, unopened) = await _open(open);
  return DeviceApp(open: open, store: store, unopened: unopened);
}

/// Opens and starts a store. A store that opened and could not start is still
/// the store: it holds its own `startFailure`. One that could not be opened
/// at all is the second value.
Future<(AppStore?, StartFailure?)> _open(Future<AppStore> Function() open) async {
  final AppStore store;
  try {
    store = await open();
  } on Object catch (e) {
    return (null, StartFailure('${e.runtimeType}'));
  }
  await store.start();
  return (store, null);
}

/// The app on a device: [CatenaryApp] over a store, or over the fact that
/// none could be opened, with TRY AGAIN opening one.
class DeviceApp extends StatefulWidget {
  const DeviceApp({super.key, required this.open, this.store, this.unopened, this.initialMode = ThemeMode.system});

  final Future<AppStore> Function() open;
  final AppStore? store;
  final StartFailure? unopened;
  final ThemeMode initialMode;

  @override
  State<DeviceApp> createState() => _DeviceAppState();
}

class _DeviceAppState extends State<DeviceApp> {
  late AppStore? _store = widget.store;
  late StartFailure? _unopened = widget.unopened;

  Future<void> _retry() async {
    final (store, unopened) = await _open(widget.open);
    if (!mounted) {
      store?.dispose();
      return;
    }
    setState(() {
      _store = store;
      _unopened = unopened;
    });
  }

  @override
  Widget build(BuildContext context) =>
      CatenaryApp(initialMode: widget.initialMode, store: _store, unopened: _unopened, onRetryOpen: _retry);
}

class CatenaryApp extends StatefulWidget {
  const CatenaryApp({
    super.key,
    this.initialMode = ThemeMode.system,
    this.clock = DateTime.now,
    this.store,
    this.unopened,
    this.onRetryOpen,
  });

  /// Dark is primary. `system` follows the device, which is what ships; a
  /// test names the theme it wants.
  final ThemeMode initialMode;

  /// What time it is. Every stamp on screen is relative to this, so a test
  /// moves it rather than editing fixtures.
  final DateTime Function() clock;

  /// What the panes read, already started. With one that found no enrollment
  /// the first screen is the enrollment screen; without one, the panes read
  /// the fixtures.
  final AppStore? store;

  /// Set when no store could be opened at all: the failed-start screen, and
  /// nothing else. [onRetryOpen] is its TRY AGAIN.
  final StartFailure? unopened;
  final Future<void> Function()? onRetryOpen;

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
      home: widget.unopened != null
          ? StartFailedScreen(
              cause: StartFailedCause.directory,
              name: widget.unopened!.name,
              onRetry: widget.onRetryOpen ?? () async {},
            )
          : _Shell(clock: widget.clock, mode: _mode, onMode: (m) => setState(() => _mode = m), store: widget.store),
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
  // (`AppStore.retryNow`) and RE-ENROLL shows the enrollment screen (`_reenroll`).
  VoidCallback? get _onRetry => widget.store?.retryNow;
  VoidCallback? get _onReenroll => _reenroll;

  /// RE-ENROLL: the enrollment screen over everything, with the stored address
  /// shown and not editable. A success pops back to the first route, which
  /// the store's new session has by then redrawn.
  void _reenroll() {
    final store = widget.store;
    if (store == null) return;
    // Read once: the store lets go of its session while it re-enrolls, and
    // the form must go on showing the address it was opened for.
    final address = store.address;
    Navigator.of(context).push(MaterialPageRoute<void>(
      builder: (ctx) => EnrollScreen(
        deviceName: defaultDeviceName(Platform.operatingSystem),
        lockedAddress: address,
        onCancel: () => Navigator.of(ctx).pop(),
        onSubmit: (_, token, name) async {
          final outcome = await store.reenroll(token: token, deviceName: name, release: kReleaseMode);
          if (outcome is Enrolled && ctx.mounted) Navigator.of(ctx).popUntil((r) => r.isFirst);
          return outcome;
        },
      ),
    ));
  }

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
      builder: (context, _) {
        // Asked first: a device whose stores will not open has no session
        // either, and must not be shown the enrollment form.
        final failure = store.startFailure;
        if (failure != null) {
          return StartFailedScreen(
            cause: failure.wipeOwed ? StartFailedCause.wipeOwed : StartFailedCause.stores,
            name: failure.name,
            onRetry: store.start,
          );
        }
        return store.enrolled ? _storeRail(context, store) : _enrollScreen(store);
      },
    );
  }

  /// The first screen of a device that is not enrolled: nothing else is shown
  /// until it has a credential.
  Widget _enrollScreen(AppStore store) => EnrollScreen(
        deviceName: defaultDeviceName(Platform.operatingSystem),
        onSubmit: (address, token, name) => store.enroll(typedAddress: address, token: token, deviceName: name, release: kReleaseMode),
      );

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
