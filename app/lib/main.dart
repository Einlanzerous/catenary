// Catenary — the Flutter client's entrypoint (CANT-40).
//
// The skeleton: the app, its two themes, and one screen that renders the
// token table so both can be looked at. The protocol half lives in
// dart-client/ (`catenary_client`) and imports no Flutter; the rail and the
// thread land on top of this in CANT-43.

import 'package:flutter/material.dart';

import 'specimen.dart';
import 'theme.dart';

void main() => runApp(const CatenaryApp());

class CatenaryApp extends StatefulWidget {
  const CatenaryApp({super.key, this.initialMode = ThemeMode.system});

  /// Dark is primary. `system` follows the device, which is what ships; a
  /// test names the theme it wants.
  final ThemeMode initialMode;

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
      home: SpecimenScreen(mode: _mode, onMode: (m) => setState(() => _mode = m)),
    );
  }
}
