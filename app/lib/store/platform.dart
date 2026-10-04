// The shipped platform seams: where the directory is, what the network and the
// app's own lifecycle tell the transport, and where its log lines go. Each is
// handed to `startSession` through `SessionSeams`; a test passes its own
// instead and never touches this file.

import 'dart:async';

import 'package:catenary_client/catenary_client.dart';
import 'package:connectivity_plus/connectivity_plus.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/widgets.dart';
import 'package:path_provider/path_provider.dart';

/// The application-support directory: private to the app, backed up by
/// nothing the app does not ask for, and the same on every launch. It holds
/// both databases, the lock files and the address.
Future<String> supportDirectory() async => (await getApplicationSupportDirectory()).path;

/// A `Lifecycle` over the two things a device knows. `visible` and `hidden`
/// are the app coming to the foreground and leaving it
/// (`AppLifecycleState`); `online` and `offline` are the network's, and only
/// a CHANGE is reported, so a flapping radio is one signal per flap.
///
/// `resumed` is `visible`, which the transport answers by pulling a trigger
/// and running its wake detector, so a process the OS suspended finds its dead
/// socket on the way back. `connectivity_plus` says a network exists, not that
/// the server is reachable: the transport's own dial finds that out.
final class FlutterLifecycle with WidgetsBindingObserver implements Lifecycle {
  /// [connectivity] defaults to the plugin's stream; a test passes its own.
  FlutterLifecycle({Stream<List<ConnectivityResult>>? connectivity}) : _connectivity = connectivity ?? Connectivity().onConnectivityChanged;

  final Stream<List<ConnectivityResult>> _connectivity;
  final _fns = <void Function(LifecycleEvent)>{};
  StreamSubscription<List<ConnectivityResult>>? _network;
  bool? _online;

  @override
  void Function() subscribe(void Function(LifecycleEvent e) fn) {
    if (_fns.isEmpty) {
      WidgetsBinding.instance.addObserver(this);
      _network = _connectivity.listen((results) {
        final online = results.any((r) => r != ConnectivityResult.none);
        if (online == _online) return;
        _online = online;
        _emit(online ? LifecycleEvent.online : LifecycleEvent.offline);
      });
    }
    _fns.add(fn);
    return () {
      _fns.remove(fn);
      if (_fns.isEmpty) {
        WidgetsBinding.instance.removeObserver(this);
        _network?.cancel();
        _network = null;
        _online = null;
      }
    };
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    switch (state) {
      case AppLifecycleState.resumed:
        _emit(LifecycleEvent.visible);
      case AppLifecycleState.hidden || AppLifecycleState.paused:
        _emit(LifecycleEvent.hidden);
      case AppLifecycleState.inactive || AppLifecycleState.detached:
        break;
    }
  }

  void _emit(LifecycleEvent e) {
    for (final fn in _fns.toList()) {
      fn(e);
    }
  }
}

/// The transport's log lines through `debugPrint`, which `adb logcat` shows.
/// Like every `Logger` here it is never given a token. Silent in a release
/// build, where nothing reads them.
final class DebugLogger implements Logger {
  const DebugLogger();

  @override
  void info(String msg, [Map<String, Object?> fields = const {}]) => _line('info', msg, fields);

  @override
  void warn(String msg, [Map<String, Object?> fields = const {}]) => _line('warn', msg, fields);

  void _line(String level, String msg, Map<String, Object?> fields) {
    if (kReleaseMode) return;
    debugPrint('catenary $level: $msg${fields.isEmpty ? '' : ' $fields'}');
  }
}
