// The failed-start screen (CANT-222): what a device draws when it could not
// open what it keeps. Before this the app ended in `main()` and drew nothing.
//
// NEITHER CANVAS HAS A FRAME FOR IT, so it is laid out as the enrollment
// screen is (enroll.dart), from the same tokens and type, and adds none.
//
// IT CLAIMS WHAT THE APP DID, NOT WHAT THE DATA IS. "Catenary has not deleted
// anything" is the app's own conduct. The stores paragraph says "has been set
// up" and not "is enrolled": the credential is in `catenary.db`, so when that
// file is the one that will not open, the `server` file is all that was read.
//
// RULING 0 → THE TYPE NAME ONLY. The error's Dart type name is shown and its
// message never is: a `SqliteException` prints its statement's parameters.
// RULING 1 → TRY AGAIN, which runs the start again and deletes nothing. There
// is no reset here: removing the outbox's file destroys unsent messages.

import 'package:flutter/material.dart';

import 'metrics.dart';
import 'tokens.dart';
import 'widgets/logomark.dart';

/// Which of the three things happened.
enum StartFailedCause {
  /// The address was read and a database, a lock or the outbox would not open.
  stores,

  /// The platform could not name the directory, so nothing was read at all.
  directory,

  /// A re-enrollment stored its credential and could not then wipe the
  /// journal the previous one left.
  wipeOwed,
}

const startFailedTitle = "This device's data could not be opened";

String startFailedText(StartFailedCause cause) => switch (cause) {
      StartFailedCause.stores =>
        'This device has been set up for Catenary, but Catenary could not open what it keeps here: your messages, and anything waiting to send. Catenary has not deleted anything.',
      StartFailedCause.directory =>
        'Catenary could not reach its own storage on this device, so it cannot tell whether this device is enrolled. Catenary has not deleted anything.',
      StartFailedCause.wipeOwed =>
        'This device was re-enrolled, but Catenary could not clear the messages the previous sign-in left here, so it has not opened them. Anything waiting to send is untouched.',
    };

class StartFailedScreen extends StatefulWidget {
  const StartFailedScreen({super.key, required this.cause, required this.name, required this.onRetry});

  final StartFailedCause cause;

  /// The error's type name, and nothing else of it.
  final String name;

  /// TRY AGAIN. Completes when the attempt has come out, either way; a
  /// success replaces this screen and a failure leaves it.
  final Future<void> Function() onRetry;

  @override
  State<StartFailedScreen> createState() => _StartFailedScreenState();
}

class _StartFailedScreenState extends State<StartFailedScreen> {
  var _trying = false;

  Future<void> _retry() async {
    setState(() => _trying = true);
    try {
      await widget.onRetry();
    } finally {
      if (mounted) setState(() => _trying = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final t = CatenaryTokens.of(context);
    return Scaffold(
      backgroundColor: t.surfaceBase,
      body: SafeArea(
        child: Center(
          child: SingleChildScrollView(
            padding: const EdgeInsets.all(CatenaryMetrics.s6),
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 420),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  const Logomark(size: 28),
                  const SizedBox(height: CatenaryMetrics.s6),
                  Text(startFailedTitle, style: CatenaryType.display.style.copyWith(color: t.textPrimary)),
                  const SizedBox(height: CatenaryMetrics.s2),
                  Text(
                    startFailedText(widget.cause),
                    key: const Key('start-failed-text'),
                    style: CatenaryType.secondary.style.copyWith(color: t.textSecondary),
                  ),
                  const SizedBox(height: CatenaryMetrics.s4),
                  Text(widget.name, key: const Key('start-failed-name'), style: CatenaryType.label.tracked.copyWith(color: t.textMeta)),
                  const SizedBox(height: CatenaryMetrics.s6),
                  FilledButton(
                    key: const Key('start-failed-retry'),
                    onPressed: _trying ? null : _retry,
                    child: Text(_trying ? 'TRYING…' : 'TRY AGAIN', style: CatenaryType.label.tracked.copyWith(color: t.onAccent)),
                  ),
                ],
              ),
            ),
          ),
        ),
      ),
    );
  }
}
