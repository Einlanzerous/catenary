/// A second PROCESS over the lock directory, for lock_test.dart.
///
///   dart run test/helpers/hold_lock.dart DIRECTORY NAME hold
///     takes the lock, prints `held` (or `busy` and exits), and keeps it until
///     it is killed or its stdin closes — and never releases it itself, which
///     is what a process killed mid-refresh leaves behind.
///   dart run test/helpers/hold_lock.dart DIRECTORY NAME try
///     makes one attempt and prints `held` or `busy`.
library;

import 'dart:io';

import 'package:catenary_client/catenary_client.dart';

Future<void> main(List<String> args) async {
  final [directory, name, mode] = args;
  final locks = SqliteLocks(directory);
  final held = locks.tryAcquire(name);
  stdout.writeln(held ? 'held' : 'busy');
  await stdout.flush();
  if (mode == 'hold' && held) await stdin.drain<void>();
  // Exits holding it: no COMMIT, no close.
  exit(0);
}
