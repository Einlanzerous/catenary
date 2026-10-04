/// The drain lock — mirrors web/src/outbox/coordination.ts (CANT-36 §8), with
/// the SQLite-held lock (CANT-42 ruling 2) where the reference has a Web Lock.
///
/// Exactly one context drains: the holder of `catenary.outbox`. A context
/// requests the lock only while its own transport session is `ready`, and
/// releases it when that session stops being ready or the context goes away —
/// a holder that is killed releases it by dying, as lock.dart shows.
///
/// The in-process hub is not a mock of the file's: it has the semantics the
/// outbox relies on — a queued request is granted only on the holder's release,
/// in request order — so two `Outbox` instances sharing one hub behave as two
/// contexts sharing one lock. A host with nowhere to put a file uses it too.
library;

import 'dart:async';

import '../lock.dart';
import '../seams.dart';
import 'types.dart';

const drainLockName = 'catenary.outbox';

/// How long a waiter leaves between attempts on a lock another context holds.
const _retryMs = 50;

/// The drain lock, held in SQLite: every isolate and process over the same
/// directory respects it. ONE NON-BLOCKING ATTEMPT, retried on a timer — never
/// a wait on the event loop.
final class SqliteDrainLock implements DrainLock {
  SqliteDrainLock(this._locks, {Timers timers = const SystemTimers(), String name = drainLockName})
      : _timers = timers,
        _name = name;

  final SqliteLocks _locks;
  final Timers _timers;
  final String _name;

  @override
  void Function() request(void Function() onGranted) {
    var withdrawn = false;
    var held = false;
    Object? timer;
    void attempt() {
      timer = null;
      if (withdrawn) return;
      if (_locks.tryAcquire(_name)) {
        held = true;
        // Granted asynchronously, as the in-process hub and a Web Lock do.
        scheduleMicrotask(() {
          if (!withdrawn) onGranted();
        });
        return;
      }
      timer = _timers.setTimeout(attempt, _retryMs);
    }

    attempt();
    return () {
      if (withdrawn) return;
      withdrawn = true;
      if (timer != null) _timers.clearTimeout(timer);
      if (held) _locks.release(_name);
    };
  }
}

/// A lock shared by every `DrainLock` handed out by one hub.
final class InProcessLockHub {
  Object? _holder;
  final _queue = <({Object id, void Function() onGranted})>[];

  DrainLock lock() => _HubLock(this);

  /// Test visibility: is anyone holding it?
  bool get held => _holder != null;

  void _grant() {
    if (_holder != null || _queue.isEmpty) return;
    final next = _queue.removeAt(0);
    _holder = next.id;
    // Granted asynchronously.
    scheduleMicrotask(next.onGranted);
  }
}

final class _HubLock implements DrainLock {
  const _HubLock(this._hub);

  final InProcessLockHub _hub;

  @override
  void Function() request(void Function() onGranted) {
    final id = Object();
    _hub._queue.add((id: id, onGranted: onGranted));
    _hub._grant();
    return () {
      _hub._queue.removeWhere((q) => identical(q.id, id));
      if (identical(_hub._holder, id)) {
        _hub._holder = null;
        _hub._grant();
      }
    };
  }
}
