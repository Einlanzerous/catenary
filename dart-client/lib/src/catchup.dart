/// Catch-up — mirrors web/src/transport/catchup.ts, which mirrors
/// `catchUpLoop`, `catchUp` and `fetch` in internal/client/client.go: CANT-24
/// obligations 2–4 and CANT-103 rules 2–3.
///
/// ONE LOOP, NEVER TWO CONCURRENT. Triggers bump `gen`; a pass ends only on a
/// `has_more: false` page whose request was issued after the most recent
/// trigger, so a trigger that arrives while a page is in flight re-arms the end
/// condition instead of starting a second catch-up (obligation 3, CANT-103 rule
/// 3). The triggers are the dial (pulled before the upgrade, so the `/sync`
/// goes out beside it), `ready`, `resync_required`, `catchUp()`, CANT-103's
/// discard, a stale-journal refusal, an own-user receipt (CANT-35 ruling 4 → B)
/// and the lifecycle policy (ruling 5 → B).
///
/// A FAILED PASS WHILE NO SESSION IS READY WAITS FOR THE NEXT TRIGGER, not for
/// a timer of its own. Go retries on its own backoff as well, which during an
/// outage puts about two `/sync`s beside every dial; here the next dial's
/// trigger is the retry, so an outage costs at most one `/sync` per dial. With
/// a ready session, or while the credential layer says the token is refused —
/// when no dial is coming to pull a trigger, and CANT-129's one `/sync` per
/// hold is this loop's to send — it retries on its own backoff, as Go does.
library;

import 'dart:async';

import 'package:catenary_wire/catenary_wire.dart';

import 'backoff.dart';
import 'faults.dart';
import 'seams.dart';
import 'status.dart';

abstract interface class CatchUpHost {
  Faults get faults;
  Stats get stats;
  Timers get timers;
  num get backoffMinMs;
  num get backoffMaxMs;
  bool sessionReady();

  /// CANT-129's refused-token mark (the credential layer's).
  bool tokenRefused();

  /// CANT-129's hold predicate: true withholds this `/sync`.
  Future<bool> withholdSync();

  /// Drains the journal's write queue, then reads the committed cursor and
  /// the epoch (the wipe count) a request is issued in.
  Future<({int? cursor, int epoch})> settled();

  /// One `GET /sync`, with the reactive refresh's single retry.
  Future<SyncResponse> fetchPage(int after);

  /// Lands a page; false when a wipe intervened since `epoch` and the page
  /// was dropped (obligation 4).
  Future<bool> applyPage(SyncResponse page, int epoch);
  void notify();
}

final class CatchUp {
  CatchUp(this._host);

  final CatchUpHost _host;

  /// Triggers pulled, and the trigger the last completed pass satisfied.
  var _gen = 0;
  var _doneGen = 0;

  /// Obligation 4's `skipWipe` fault: the next pass starts at 0.
  var _from0 = false;
  var _inPass = false;
  void Function()? _kick;

  /// A trigger. Withheld sources are the transport's to decide, not this.
  void trigger() {
    if (_host.faults.ignoreRetrigger && _inPass) return;
    _gen++;
    wake();
    _host.notify();
  }

  bool get caughtUp => _gen == _doneGen;

  /// `skipWipe`'s re-sync from 0, keeping the store and the cursor.
  void restartFromZero() {
    _from0 = true;
  }

  /// Unblocks a waiting loop, so it can see it is no longer alive.
  void wake() {
    final k = _kick;
    _kick = null;
    k?.call();
  }

  /// The loop, once per `start()`. `alive` goes false for good when that run
  /// stops or turns terminal, and the loop returns at its next check.
  Future<void> run(bool Function() alive) async {
    final h = _host;
    var backoff = h.backoffMinMs;
    while (alive()) {
      if (caughtUp) {
        await _nextTrigger();
        continue;
      }
      // CANT-129: one `/sync` per refresh hold, and this loop owns it.
      if (await h.withholdSync()) {
        h.stats.syncsWithheld++;
        h.notify();
        await _sleep(h.backoffMaxMs, false, alive);
        continue;
      }
      try {
        await _pass(alive);
        backoff = h.backoffMinMs;
      } catch (_) {
        // Counted, and not logged per attempt: a dead network would otherwise
        // write a line per dial.
        if (!alive()) return;
        h.stats.syncErrors++;
        h.notify();
        if (h.sessionReady() || h.tokenRefused()) {
          await _sleep(backoff, true, alive);
        } else {
          await _nextTrigger();
        }
        backoff = advance(backoff, h.backoffMaxMs);
      }
    }
  }

  /// Pages until obligation 3 says it is done.
  Future<void> _pass(bool Function() alive) async {
    final h = _host;
    _inPass = true;
    try {
      var next = -1; // -1: start from the cursor
      for (;;) {
        final issued = _gen;
        final (:cursor, :epoch) = await h.settled();
        var after = next >= 0 ? next : (cursor ?? 0);
        if (next < 0 && _from0) {
          _from0 = false;
          after = 0;
        }
        final page = await h.fetchPage(after);
        if (!alive()) return;
        final applied = await h.applyPage(page, epoch);
        h.stats.pages++;
        if (!applied) {
          next = -1; // a wipe intervened: start again from the new cursor
        } else if (page.hasMore) {
          next = page.logSeq;
        } else if (issued == _gen || h.faults.endCatchUpEarly) {
          _doneGen = _gen;
          h.notify();
          return;
        } else {
          next = -1; // a trigger arrived after this request: ask again
        }
        h.notify();
      }
    } finally {
      _inPass = false;
    }
  }

  Future<void> _nextTrigger() {
    final c = Completer<void>();
    _kick = c.complete;
    return c.future;
  }

  /// Waits `ms`, or — when `onTrigger` — until the next trigger, whichever is
  /// first: a new trigger retries at once.
  Future<void> _sleep(num ms, bool onTrigger, bool Function() alive) {
    final c = Completer<void>();
    Object? handle;
    late final void Function() onKick;
    void done() {
      if (c.isCompleted) return;
      _host.timers.clearTimeout(handle);
      if (_kick == onKick) _kick = null;
      c.complete();
    }

    // Even a sleep that ignores triggers wakes for a stop.
    onKick = () {
      if (!onTrigger && alive()) {
        _kick = onKick;
        return;
      }
      done();
    };
    handle = _host.timers.setTimeout(done, ms);
    _kick = onKick;
    return c.future;
  }
}
