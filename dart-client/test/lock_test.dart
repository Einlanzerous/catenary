/// The SQLite-held named lock (CANT-42 ruling 2): what replaces a Web Lock.
/// A lock held by one context cannot be taken by a second ISOLATE of the same
/// process or by a second PROCESS; it is free again after the holder commits,
/// after its connection closes, and after its process is killed; and waiting
/// for it never blocks the event loop.
library;

import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:isolate';

import 'package:catenary_client/catenary_client.dart';
import 'package:test/test.dart';

import 'support.dart';

const name = 'catenary.credential.00000000-0000-4000-8000-000000000003';

/// One attempt from a second isolate of this process, over its own connection.
Future<bool> fromAnotherIsolate(String directory) => Isolate.run(() {
      final locks = SqliteLocks(directory);
      final held = locks.tryAcquire(name);
      locks.close();
      return held;
    });

/// The helper as a second process. Completes with the process and the first
/// word it printed: `held` or `busy`.
Future<({Process process, String said})> anotherProcess(String directory, String mode) async {
  final process = await Process.start(Platform.resolvedExecutable, ['run', 'test/helpers/hold_lock.dart', directory, name, mode]);
  final stderrText = process.stderr.transform(utf8.decoder).join();
  final said = await process.stdout
      .transform(utf8.decoder)
      .transform(const LineSplitter())
      .map((l) => l.trim())
      .firstWhere((l) => l.endsWith('held') || l.endsWith('busy'), orElse: () => '');
  if (said.isEmpty) fail('the helper process said neither: ${await stderrText}');
  return (process: process, said: said.endsWith('held') ? 'held' : 'busy');
}

void main() {
  test('a held lock cannot be acquired by a second connection, and is free again after the holder commits', () {
    final dir = tempDir();
    final a = SqliteLocks(dir);
    final b = SqliteLocks(dir);
    addTearDown(a.close);
    addTearDown(b.close);
    expect(a.tryAcquire(name), isTrue);
    expect(File(a.fileFor(name)).existsSync(), isTrue, reason: 'a dedicated file per name');
    expect(b.tryAcquire(name), isFalse, reason: 'one attempt, refused, and it returned');
    expect(b.tryAcquire('catenary.outbox.drain'), isTrue, reason: 'another name is another lock');
    a.release(name);
    expect(b.tryAcquire(name), isTrue, reason: 'free after the holder commits');
    expect(a.tryAcquire(name), isFalse);
  });

  test('a name cannot leave the lock directory', () {
    final dir = tempDir();
    final locks = SqliteLocks(dir);
    addTearDown(locks.close);
    expect(locks.fileFor('../../etc/passwd'), startsWith('$dir/'));
    expect(locks.fileFor('../../etc/passwd').substring(dir.length + 1), isNot(contains('/')));
  });

  test('it is free again after the holder\'s connection closes without committing', () {
    final dir = tempDir();
    final a = SqliteLocks(dir);
    final b = SqliteLocks(dir);
    addTearDown(b.close);
    expect(a.tryAcquire(name), isTrue);
    expect(b.tryAcquire(name), isFalse);
    a.connectionFor(name)!.close();
    expect(b.tryAcquire(name), isTrue);
  });

  test('a second isolate of the same process cannot acquire it, and can once it is released', () async {
    final dir = tempDir();
    final locks = SqliteLocks(dir);
    addTearDown(locks.close);
    expect(locks.tryAcquire(name), isTrue);
    expect(await fromAnotherIsolate(dir), isFalse, reason: 'held by this isolate');
    locks.release(name);
    expect(await fromAnotherIsolate(dir), isTrue, reason: 'released');
    expect(locks.tryAcquire(name), isTrue, reason: 'and the isolate that took it has gone, its connection with it');
  });

  test('a second process cannot acquire it', () async {
    final dir = tempDir();
    final locks = SqliteLocks(dir);
    addTearDown(locks.close);
    expect(locks.tryAcquire(name), isTrue);
    final other = await anotherProcess(dir, 'try');
    expect(other.said, 'busy');
    await other.process.exitCode;
    locks.release(name);
    final again = await anotherProcess(dir, 'try');
    expect(again.said, 'held', reason: 'the control: the same helper takes it once it is free');
    await again.process.exitCode;
  }, timeout: const Timeout(Duration(minutes: 2)));

  test('a lock held by another process is refused here, and is free again after that process is killed', () async {
    final dir = tempDir();
    final locks = SqliteLocks(dir);
    addTearDown(locks.close);
    final holder = await anotherProcess(dir, 'hold');
    addTearDown(() => holder.process.kill(ProcessSignal.sigkill));
    expect(holder.said, 'held');
    expect(locks.tryAcquire(name), isFalse, reason: 'held by the other process');
    expect(await fromAnotherIsolate(dir), isFalse);

    // SIGKILL: nothing in the holder runs again. No COMMIT, no close.
    expect(holder.process.kill(ProcessSignal.sigkill), isTrue);
    await holder.process.exitCode;
    expect(locks.tryAcquire(name), isTrue, reason: 'the lock died with its holder');
  }, timeout: const Timeout(Duration(minutes: 2)));

  test('waiting for a lock never blocks the event loop: a timer keeps firing while the attempt is retried', () async {
    final dir = tempDir();
    final holder = SqliteLocks(dir);
    final waiter = SqliteLocks(dir);
    addTearDown(holder.close);
    addTearDown(waiter.close);
    expect(holder.tryAcquire(name), isTrue);

    var ticks = 0;
    final ticker = Timer.periodic(const Duration(milliseconds: 5), (_) => ticks++);
    addTearDown(ticker.cancel);
    var ticksWhenAcquired = -1;
    var acquired = false;
    final waiting = waiter.call(name, () async {
      acquired = true;
      ticksWhenAcquired = ticks;
      return 'the waiter ran';
    });
    await Future<void>.delayed(const Duration(milliseconds: 300));
    expect(acquired, isFalse, reason: 'still held by the other connection');
    expect(ticks, greaterThan(10), reason: 'the timer fired throughout the wait');
    holder.release(name);
    expect(await waiting, 'the waiter ran');
    expect(ticksWhenAcquired, greaterThan(10));
    expect(holder.tryAcquire(name), isTrue, reason: 'and the waiter released it when its work was done');
  });

  test('the lock runs its holders one at a time, releases on a failure, and serves two names at once', () async {
    final dir = tempDir();
    final a = SqliteLocks(dir);
    final b = SqliteLocks(dir);
    addTearDown(a.close);
    addTearDown(b.close);
    var inside = 0;
    var most = 0;
    Future<int> work(SqliteLocks locks, int n) => locks.call(name, () async {
          inside++;
          if (inside > most) most = inside;
          await Future<void>.delayed(const Duration(milliseconds: 10));
          inside--;
          return n;
        });
    expect(await Future.wait([work(a, 1), work(b, 2), work(a, 3), work(b, 4)]), [1, 2, 3, 4]);
    expect(most, 1, reason: 'never two holders, across two connections and within one');

    await expectLater(a.call<void>(name, () async => throw StateError('the work failed')), throwsStateError);
    expect(b.tryAcquire(name), isTrue, reason: 'released when the work failed');
    expect(await a.call('catenary.outbox.drain', () async => 'another name'), 'another name', reason: 'while the first is held');
  });

  test('inProcessLock is a mutex per name for one isolate', () async {
    final lock = inProcessLock();
    final order = <String>[];
    final first = lock('n', () async {
      order.add('first in');
      await Future<void>.delayed(const Duration(milliseconds: 10));
      order.add('first out');
    });
    final second = lock('n', () async => order.add('second'));
    final other = lock('m', () async => order.add('another name'));
    await Future.wait([first, second, other]);
    expect(order, ['first in', 'another name', 'first out', 'second']);
    await expectLater(lock<void>('n', () async => throw StateError('failed')), throwsStateError);
    expect(await lock('n', () async => 'after a failure'), 'after a failure');
  });
}
