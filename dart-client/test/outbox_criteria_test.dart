/// The outbox's criteria — the check that stands in for an oracle (CANT-36),
/// ported from web/outbox.test.ts under CANT-42 ruling 4.
///
/// The outbox is the one part of the client with nothing external to check it
/// against: a scripted fake built from the same understanding as the code will
/// agree with the code by construction. So every criterion below is a plain
/// function that throws when its rule is broken, run twice over: once as
/// written, where it must PASS, and once per named fault from the reference's
/// table, where it must FAIL. A fault whose criterion still passes fails the
/// run — the criterion was not testing what it claims.
///
/// CRITERIA ARE NUMBERED AS web/outbox.test.ts NUMBERS THEM, and a test reads
/// that file's table and fails when this one's fault names or criterion
/// numbers differ. The ones a headless outbox can be held to are 0, 1, 2, 3, 4,
/// 7, 8, 9, 10, 11, 12 and 15: 5, 6 and 16 render the app, and 13 and 14 are
/// attachments.
///
/// WHERE THE REFERENCE CHECKS ANOTHER TAB'S RENDER, THIS CHECKS ANOTHER
/// CONTEXT'S NEXT READ. There is no `BroadcastChannel` here (types.dart): a
/// second context sees a write when it calls `refresh()`, and the lock's
/// holder re-reads on a timer.
library;

import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';
import 'outbox_harness.dart';

/// One row of the fault table switched on: the outbox's faults and the store's.
typedef F = ({OutboxFaults outbox, StoreFaults store});

const F clean = (outbox: OutboxFaults.none, store: StoreFaults.none);

/// The reference's rollout table, row for row: the fault, and the criterion it
/// must make fail.
const faultTable = <(String, int)>[
  ('sendBeforePersist', 0),
  ('neverCompletes', 0),
  ('relaxedDurability', 0),
  ('remintOnRetry', 1),
  ('persistSending', 3),
  ('settlePendingOnly', 4),
  ('misclassifyRetryable', 7),
  ('misjudgeRetryBudget', 7),
  ('retryNonRetryable', 7),
  ('resendAfterBare1008', 8),
  ('resendFailedWithoutRetry', 9),
  ('deleteOnTerminal', 10),
  ('crossAccountLeak', 11),
  ('orderOutsideTxn', 12),
  ('everyTabDrains', 15),
  ('lockWithoutReady', 15),
];

/// The named fault, on. A name with no arm here is an error, so a row added to
/// the table without its switch cannot pass as a fault that broke nothing.
F fault(String name) => switch (name) {
      'sendBeforePersist' => (outbox: const OutboxFaults(sendBeforePersist: true), store: StoreFaults.none),
      'neverCompletes' => (outbox: OutboxFaults.none, store: const StoreFaults(neverCompletes: true)),
      'relaxedDurability' => (outbox: OutboxFaults.none, store: const StoreFaults(relaxedDurability: true)),
      'remintOnRetry' => (outbox: const OutboxFaults(remintOnRetry: true), store: StoreFaults.none),
      'persistSending' => (outbox: const OutboxFaults(persistSending: true), store: StoreFaults.none),
      'settlePendingOnly' => (outbox: const OutboxFaults(settlePendingOnly: true), store: StoreFaults.none),
      'misclassifyRetryable' => (outbox: const OutboxFaults(misclassifyRetryable: true), store: StoreFaults.none),
      'misjudgeRetryBudget' => (outbox: const OutboxFaults(misjudgeRetryBudget: true), store: StoreFaults.none),
      'retryNonRetryable' => (outbox: const OutboxFaults(retryNonRetryable: true), store: StoreFaults.none),
      'resendAfterBare1008' => (outbox: const OutboxFaults(resendAfterBare1008: true), store: StoreFaults.none),
      'resendFailedWithoutRetry' => (outbox: const OutboxFaults(resendFailedWithoutRetry: true), store: StoreFaults.none),
      'deleteOnTerminal' => (outbox: const OutboxFaults(deleteOnTerminal: true), store: StoreFaults.none),
      'crossAccountLeak' => (outbox: const OutboxFaults(crossAccountLeak: true), store: StoreFaults.none),
      'orderOutsideTxn' => (outbox: const OutboxFaults(orderOutsideTxn: true), store: StoreFaults.none),
      'everyTabDrains' => (outbox: const OutboxFaults(everyTabDrains: true), store: StoreFaults.none),
      'lockWithoutReady' => (outbox: const OutboxFaults(lockWithoutReady: true), store: StoreFaults.none),
      _ => throw ArgumentError('no fault named $name'),
    };

/// A broken rule. Thrown by a criterion, and by nothing else here.
void check(bool ok, String what) {
  if (!ok) throw TestFailure(what);
}

void same(Object? got, Object? want, String what) => check(got == want, '$what: got $got, want $want');

SqliteOutboxStore openStore(String dir, [StoreFaults faults = StoreFaults.none]) {
  final store = SqliteOutboxStore.open(outboxDbPath(dir), faults);
  addTearDown(store.close);
  return store;
}

// ── criterion 0 · durable before render and before any frame ────────────────

Future<void> c0(F f) async {
  final dir = tempDir();
  final store = openStore(dir, f.store);
  // A second, independent connection sees only what has committed.
  final probe = openStore(dir);
  final failures = <String>[];
  Future<void> visible(Uuid id, String at) async {
    if (!(await probe.list()).any((r) => r.clientId == id)) failures.add('entry not committed at the first $at');
  }

  final checks = <Future<void>>[];
  final firstRender = <Uuid>{};
  final transport = ScriptedTransport();
  var frames = 0;
  transport.onFrame = (frame) {
    if (frames++ == 0) checks.add(visible(frame.clientId, 'frame'));
  };
  final ctx = await context(
    store: store,
    transport: transport,
    faults: f.outbox,
    onView: (items) {
      for (final i in items) {
        if (firstRender.add(i.entry.clientId)) checks.add(visible(i.entry.clientId, 'render'));
      }
    },
  );
  transport.open();
  await flush();

  final entry = await ctx.composeText();
  await flush();
  same(transport.framesFor(entry.clientId).length, 1, 'the entry was sent');
  check(firstRender.contains(entry.clientId), 'the entry was rendered');
  await Future.wait(checks);
  check(failures.isEmpty, failures.join('; '));
  // A flush is not observable from here, so the connection is asked what it
  // was opened with: 2 is FULL.
  same(store.database.select('PRAGMA synchronous').single.values.single, 2, 'the writing connection\'s synchronous');

  // The crash: the moment compose completes, drop everything without awaiting.
  final crashStore = SqliteOutboxStore.open(outboxDbPath(dir), f.store);
  final crash = await context(store: crashStore, faults: f.outbox);
  final crashed = await within('compose before the crash', crash.outbox.compose(OutboxDraft(conversationId: conv, text: 'then the power went')));
  crash.close();
  crashStore.close();
  final fresh = openStore(dir);
  check((await fresh.list()).any((e) => e.clientId == crashed.clientId), 'the entry is loadable after the crash');
}

// ── criterion 1 · one clientId, minted once, on every frame ─────────────────

Future<void> c1(F f) async {
  var mints = 0;
  Uuid spy() {
    mints++;
    return mintUuid();
  }

  final store = MemoryOutboxStore();
  final server = ScriptedServer();
  final clock = FakeClock();
  final t1 = ScriptedTransport(server);
  var ctx = await context(store: store, transport: t1, clock: clock, faults: f.outbox, mintId: spy);
  t1.open();
  await flush();
  final entry = await ctx.composeText();
  await flush();
  same(mints, 1, 'minted once, at compose');
  // Dropped before the ack.
  t1.close();
  t1.open();
  await flush();
  // A relaunch.
  ctx.close();
  final t2 = ScriptedTransport(server);
  ctx = await context(store: store, transport: t2, clock: clock, faults: f.outbox, mintId: spy);
  t2.open();
  await flush();
  // A retryable refusal, held and resent.
  t2.refuse(t2.frames.last.clientId, ErrorCode.internal, 'try again', retryable: true);
  await flush();
  await clock.advance(outboxBackoffCapMs);
  // A non-retryable refusal, then a manual RETRY.
  t2.refuse(t2.frames.last.clientId, ErrorCode.notAMember, 'not a member', retryable: false);
  await flush();
  await ctx.outbox.retry((await store.list()).first.clientId);
  await flush();

  final all = [for (final fr in server.received) fr.clientId];
  check(all.length >= 5, 'five frames written (first, after a drop, after a relaunch, after a hold, after RETRY): ${all.length}');
  for (final id in all) {
    same(id, entry.clientId, 'every frame carries the minted clientId');
  }
  same((await store.list()).length, 1, 'still one entry');
}

// ── criterion 2 · a send survives a relaunch before its ack ─────────────────

Future<void> c2(F f) async {
  final store = MemoryOutboxStore();
  final server = ScriptedServer();
  final t1 = ScriptedTransport(server);
  var ctx = await context(store: store, transport: t1, faults: f.outbox);
  t1.open();
  await flush();
  final entry = await ctx.composeText();
  await flush();
  ctx.close();

  final t2 = ScriptedTransport(server);
  ctx = await context(store: store, transport: t2, faults: f.outbox);
  same(ctx.item(entry.clientId)?.state, OutboxState.queued, 'reads QUEUED after the relaunch');
  t2.open();
  await flush();
  same(t2.framesFor(entry.clientId).length, 1, 'written again on the next ready');
  t2.ack(entry.clientId);
  await flush();
  same(ctx.item(entry.clientId)?.state, OutboxState.sent, 'SENT on the ack');
  same((await store.list()).length, 1, 'no second entry');
}

// ── criterion 3 · sending, queued and the ack are never persisted ───────────

Future<void> recordShape(OutboxStore store) async {
  for (final e in await store.list()) {
    final stored = e.toJson();
    check(stored['status'] == 'pending' || stored['status'] == 'failed', 'stored status is ${stored['status']}');
    for (final k in stored.keys) {
      check(OutboxEntry.storedKeys.contains(k), 'stored field $k is not in the entry shape');
    }
    check(!stored.containsKey('ack'), 'an ack is stored');
  }
}

Future<void> c3(F f) async {
  final store = MemoryOutboxStore();
  final server = ScriptedServer();
  final t1 = ScriptedTransport(server);
  var ctx = await context(store: store, transport: t1, faults: f.outbox);
  t1.open();
  await flush();
  final flying = await ctx.composeText('in flight');
  final acked = await ctx.composeText('acked');
  await flush();
  t1.ack(acked.clientId);
  await flush();
  await recordShape(store);
  same(ctx.item(flying.clientId)?.state, OutboxState.sending, 'in flight reads SENDING, from memory');
  same(ctx.item(acked.clientId)?.state, OutboxState.sent, 'acked reads SENT, from memory');

  ctx.close();
  ctx = await context(store: store, transport: ScriptedTransport(server), faults: f.outbox);
  same(ctx.item(flying.clientId)?.state, OutboxState.queued, 'an in-flight entry reloads QUEUED');
  same(ctx.item(acked.clientId)?.state, OutboxState.queued, 'an acked, unsettled entry reloads QUEUED');
  await recordShape(store);
}

// ── criterion 4 · the ack places it; a record settles it, in any status ─────

Future<void> c4(F f) async {
  final store = MemoryOutboxStore();
  final lock = InProcessLockHub();
  final transport = ScriptedTransport();
  final ctx = await context(store: store, transport: transport, faults: f.outbox, lock: lock.lock());
  // A second context on the same store, never ready.
  final other = await context(store: store, faults: f.outbox, lock: lock.lock());
  transport.open();
  await flush();

  // The SERVER's conversation, seq and at — not the entry's.
  final ca = uuid(701);
  final cb = uuid(702);
  final moved = await ctx.composeText('answered elsewhere', ca);
  await flush();
  final ack = transport.ack(moved.clientId, cb);
  await flush();
  final shown = projectOutbox(ctx.item(moved.clientId)!);
  same(shown.state, OutboxState.sent, 'acked');
  same(shown.conversationId, cb, 'shown in the ack\'s conversation');
  same(shown.seq, ack.seq, 'at the ack\'s seq');
  same(shown.at, ack.at, 'at the ack\'s time, not composedAt');
  check(shown.at != moved.composedAt, 'the ack\'s time is not the device\'s');

  // duplicate: true is handled identically.
  final dup = transport.ack(moved.clientId, cb);
  same(dup.duplicate, true, 'a replay is answered duplicate');
  await flush();
  same(projectOutbox(ctx.item(moved.clientId)!).state, OutboxState.sent, 'still sent');
  same(projectOutbox(ctx.item(moved.clientId)!).seq, ack.seq, 'at the original seq');

  // One entry in each status, each then met by its record.
  final internal = await ctx.composeText('internal');
  final tooLarge = await ctx.composeText('too large');
  await flush();
  transport.refuse(internal.clientId, ErrorCode.internal, 'unknown outcome', retryable: false);
  transport.refuse(tooLarge.clientId, ErrorCode.messageTooLarge, 'too large', retryable: false);
  await flush();
  final bare = await ctx.composeText('bare 1008');
  await flush();
  transport.close(bare1008: true);
  await flush();
  transport.open();
  await flush();
  final flying = await ctx.composeText('in flight');
  final queued = await ctx.composeText('queued');
  await flush();
  transport.refuse(queued.clientId, ErrorCode.rateLimited, 'later', retryable: true, retryAfterSec: 60);
  await flush();
  for (final e in [internal, tooLarge, bare]) {
    same(ctx.item(e.clientId)?.state, OutboxState.failed, '${e.text} failed');
  }
  same(ctx.item(flying.clientId)?.state, OutboxState.sending, 'one in flight');
  same(ctx.item(queued.clientId)?.state, OutboxState.queued, 'one queued');
  same(ctx.item(moved.clientId)?.state, OutboxState.sent, 'one acked');

  final all = [moved, flying, internal, tooLarge, bare, queued];
  for (final e in all) {
    transport.deliver(e.clientId);
  }
  await flush();
  for (final e in all) {
    same(await ctx.stored(e.clientId), null, '${e.text} settled from the store');
    same(ctx.item(e.clientId), null, '${e.text} settled from this context\'s view');
  }
  await other.outbox.refresh();
  same(other.outbox.view().length, 0, 'the other context\'s view at its next read');
  check(!ctx.outbox.view().any((i) => i.state == OutboxState.failed), 'no FAILED row beside a record');
}

// ── criterion 7 · the error table ───────────────────────────────────────────

Future<void> c7(F f) async {
  final clock = FakeClock();
  final transport = ScriptedTransport();
  final ctx = await context(transport: transport, clock: clock, faults: f.outbox);
  transport.open();
  await flush();

  // Every non-retryable row fails at once with the server's message.
  const failing = [
    (ErrorCode.notAMember, false),
    (ErrorCode.conversationNotFound, false),
    (ErrorCode.messageTooLarge, false),
    (ErrorCode.internal, false),
    (ErrorCode.unauthorized, false),
    (ErrorCode.wireVersionUnsupported, false),
    (ErrorCode.unknown, true),
  ];
  final bystander = await ctx.composeText('bystander');
  await flush();
  for (final (code, retryable) in failing) {
    final e = await ctx.composeText(code.wire);
    await flush();
    final before = jsonEncode(await ctx.stored(bystander.clientId));
    transport.refuse(e.clientId, code, 'refused: ${code.wire}', retryable: retryable);
    await flush();
    final item = ctx.item(e.clientId)!;
    same(item.state, OutboxState.failed, '${code.wire} fails at once');
    same(projectOutbox(item).error, 'refused: ${code.wire}', 'the server\'s message is the inline error');
    same(jsonEncode(await ctx.stored(bystander.clientId)), before, 'an error naming another clientId changes nothing else');
    same(ctx.item(bystander.clientId)?.state, OutboxState.sending, 'the bystander is still in flight');
    final sent = transport.framesFor(e.clientId).length;
    await clock.advance(outboxBackoffCapMs * 2);
    same(transport.framesFor(e.clientId).length, sent, '${code.wire} is not resent');
  }

  // rate_limited: held, with retryAfterSec as a floor.
  final limited = await ctx.composeText('rate limited');
  await flush();
  transport.refuse(limited.clientId, ErrorCode.rateLimited, 'slow down', retryable: true, retryAfterSec: 30);
  await flush();
  same(ctx.item(limited.clientId)?.state, OutboxState.queued, 'rate_limited holds');
  await clock.advance(29000);
  same(transport.framesFor(limited.clientId).length, 1, 'not before retryAfterSec');
  await clock.advance(1000);
  same(transport.framesFor(limited.clientId).length, 2, 'resent at retryAfterSec');

  // internal, retryable: held under 2 s doubling, capped at 5 min; RETRYING
  // after three; never failed.
  final held = await ctx.composeText('held');
  await flush();
  for (var n = 1; n <= 10; n++) {
    final now = clock.now();
    transport.refuse(held.clientId, ErrorCode.internal, 'db down', retryable: true);
    await flush();
    final item = ctx.item(held.clientId)!;
    check(item.state != OutboxState.failed, 'refusal $n of a retryable internal never fails');
    same(item.entry.internalRetries, n, 'one count per refusal');
    final ceiling = outboxBackoffBaseMs * (1 << (n - 1)) < outboxBackoffCapMs ? outboxBackoffBaseMs * (1 << (n - 1)) : outboxBackoffCapMs;
    same(DateTime.parse(item.entry.notBefore!).millisecondsSinceEpoch - now, (0.5 * ceiling).floor(), 'backoff $n');
    same(item.retrying, n >= 3, 'RETRYING after three (at $n)');
    await clock.advance(ceiling);
    same(transport.framesFor(held.clientId).length, n + 1, 'resent after backoff $n');
  }
}

// ── criterion 8 · CANT-31 §7, consumed ──────────────────────────────────────

Future<void> c8(F f) async {
  final transport = ScriptedTransport();
  final ctx = await context(transport: transport, faults: f.outbox);
  transport.open();
  await flush();
  final bare = await ctx.composeText('bare');
  await flush();
  transport.close(bare1008: true);
  await flush();
  final item = ctx.item(bare.clientId)!;
  same(item.state, OutboxState.failed, 'a bare 1008 fails the in-flight entry');
  check(item.entry.lastError is Bare1008, 'its error is the bare 1008');
  same(projectOutbox(item).error, bare1008Message, 'with the fixed message');
  transport.open();
  await flush();
  same(transport.framesFor(bare.clientId).length, 1, 'and it is not resent');

  // Every other close, including a 1008 the transport reports as
  // frame-preceded: the transport's classification is bare1008 false.
  for (final close in ['1008 frame-preceded', '4001', '4000', '4002', '1001', 'abnormal']) {
    final e = await ctx.composeText(close);
    await flush();
    transport.close();
    await flush();
    same(ctx.item(e.clientId)?.state, OutboxState.queued, '$close: stays pending');
    transport.open();
    await flush();
    same(transport.framesFor(e.clientId).length, 2, '$close: resent on the next ready');
    transport.deliver(e.clientId);
    await flush();
  }
}

// ── criterion 9 · failed is reachable and recoverable ───────────────────────

Future<void> c9(F f) async {
  final store = MemoryOutboxStore();
  final lock = InProcessLockHub();
  final server = ScriptedServer();
  final clock = FakeClock();
  final t1 = ScriptedTransport(server);
  var ctx = await context(store: store, transport: t1, clock: clock, faults: f.outbox, lock: lock.lock());
  t1.open();
  await flush();
  final e = await ctx.composeText('will fail');
  final other = await ctx.composeText('will be deleted');
  await flush();
  t1.refuse(e.clientId, ErrorCode.notAMember, 'removed from the room', retryable: false);
  t1.refuse(other.clientId, ErrorCode.messageTooLarge, 'too large', retryable: false);
  await flush();
  ctx.close();

  final t2 = ScriptedTransport(server);
  ctx = await context(store: store, transport: t2, clock: clock, faults: f.outbox, lock: lock.lock());
  final second = await context(store: store, faults: f.outbox, lock: lock.lock());
  final item = ctx.item(e.clientId)!;
  same(item.state, OutboxState.failed, 'survives a relaunch as FAILED');
  same(projectOutbox(item).error, 'removed from the room', 'with its inline error');
  t2.open();
  await flush();
  await clock.advance(outboxBackoffCapMs * 2);
  same(t2.frames.length, 0, 'never resent without a RETRY');

  await ctx.outbox.retry(e.clientId);
  await flush();
  same(t2.framesFor(e.clientId).length, 1, 'RETRY sends it, under the same clientId');
  t2.ack(e.clientId);
  await flush();
  same(ctx.item(e.clientId)?.state, OutboxState.sent, 'and it reaches SENT');

  same(await ctx.outbox.discard(e.clientId), false, 'DELETE is not offered on a non-failed entry');
  check(await ctx.stored(e.clientId) != null, 'and it is still stored');
  same(await ctx.outbox.discard(other.clientId), true, 'DELETE on a failed entry');
  same(await ctx.stored(other.clientId), null, 'DELETE removes it from the store');
  same(ctx.item(other.clientId), null, 'and from this view');
  await second.outbox.refresh();
  check(!second.outbox.view().any((i) => i.entry.clientId == other.clientId), 'and from the other context\'s, at its next read');
}

// ── criterion 10 · nothing deletes an entry on terminal or bootstrap ────────

Future<void> c10(F f) async {
  final dir = tempDir();
  // The journal's file, as it holds a cursor, a page and a credential.
  final journal = SqliteJournal.open(catenaryDbPath(dir));
  await journal.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), Faults.none);
  final store = openStore(dir);
  final transport = ScriptedTransport();
  final ctx = await context(store: store, transport: transport, faults: f.outbox);
  transport.open();
  await flush();
  final failed = await ctx.composeText('failed');
  final pending = await ctx.composeText('pending');
  await flush();
  transport.refuse(failed.clientId, ErrorCode.notAMember, 'no', retryable: false);
  await flush();

  // Obligation 4's wipe, and then the journal's file gone altogether.
  await journal.wipe();
  same((await store.list()).length, 2, 'a journal wipe leaves every entry');
  journal.close();
  File(catenaryDbPath(dir)).deleteSync();
  same((await store.list()).length, 2, 'deleting the journal\'s file leaves every entry');

  transport.close(terminal: true);
  await flush();
  same((await store.list()).length, 2, 'a terminal state leaves every entry');
  transport.emit(const Bootstrapped());
  await flush();
  same((await store.list()).length, 2, 'a bootstrap leaves every entry');

  transport.open();
  await flush();
  same(transport.framesFor(pending.clientId).length, 2, 'the pending entry resends once live again');
  same(ctx.item(failed.clientId)?.state, OutboxState.failed, 'the failed one is still there, still failed');
}

// ── criterion 11 · entries belong to an account ─────────────────────────────

Future<void> c11(F f) async {
  final store = MemoryOutboxStore();
  final theirs = await context(store: store, accountId: uuid(77), faults: f.outbox);
  final foreign = await theirs.composeText('composed as someone else');
  theirs.close();

  final transport = ScriptedTransport();
  final ctx = await context(store: store, transport: transport, faults: f.outbox);
  final mine = await ctx.composeText('mine');
  same(ctx.item(foreign.clientId), null, 'another account\'s entry is shown nowhere');
  transport.open();
  await flush();
  same(transport.framesFor(mine.clientId).length, 1, 'mine is sent');
  same(transport.framesFor(foreign.clientId).length, 0, 'another account\'s entry is never sent');
}

// ── criterion 12 · order is drawn inside the insert's own transaction ───────
//
// WHAT THIS CAN AND CANNOT SEE. It shows two contexts, each over its own
// connection, drawing distinct values, and `orderOutsideTxn` — an order taken
// from a context's own memory — colliding. It does NOT race two transactions:
// package:sqlite3 is synchronous, so each `add` is BEGIN IMMEDIATE … COMMIT
// inside one event-loop turn, and the gaps below only reorder whole
// transactions. That the highest order is read INSIDE the transaction is
// store.dart's `add`, read there; across isolates and processes it is
// BEGIN IMMEDIATE that makes it hold.

Future<void> c12(F f) async {
  final dir = tempDir();
  final a = await context(store: openStore(dir), faults: f.outbox);
  final b = await context(store: openStore(dir), faults: f.outbox);
  final composed = <OutboxEntry>[];
  for (var gap = 0; gap < 6; gap++) {
    final first = a.outbox.compose(OutboxDraft(conversationId: conv, text: 'a$gap'));
    for (var i = 0; i < gap; i++) {
      await Future<void>.delayed(Duration.zero);
    }
    final second = b.outbox.compose(OutboxDraft(conversationId: conv, text: 'b$gap'));
    composed.addAll(await within('concurrent compose', Future.wait([first, second])));
  }
  final orders = [for (final e in composed) e.order];
  same(orders.toSet().length, orders.length, 'distinct orders: ${orders.join(',')}');
  a.close();
  b.close();

  final transport = ScriptedTransport();
  await context(store: openStore(dir), transport: transport, faults: f.outbox);
  transport.open();
  await flush();
  final expected = [for (final e in composed.toList()..sort((x, y) => x.order.compareTo(y.order))) e.clientId];
  same(jsonEncode([for (final fr in transport.frames) fr.clientId]), jsonEncode(expected), 'drained in allocation order');
}

// ── criterion 15 · one drainer, held only while ready ───────────────────────

Future<void> c15(F f) async {
  final dir = tempDir();
  final clock = FakeClock();
  final server = ScriptedServer();
  final tA = ScriptedTransport(server);
  final tB = ScriptedTransport(server);
  // Each context has its own connection to the store and its own to the lock;
  // the file is what they share.
  SqliteDrainLock drainLock() {
    final locks = SqliteLocks(dir, timers: clock);
    addTearDown(locks.close);
    return SqliteDrainLock(locks, timers: clock);
  }

  final probe = SqliteLocks(dir);
  addTearDown(probe.close);
  bool nobodyHolds() {
    final free = probe.tryAcquire(drainLockName);
    if (free) probe.release(drainLockName);
    return free;
  }

  final a = await context(store: openStore(dir), transport: tA, clock: clock, faults: f.outbox, lock: drainLock(), rereadMs: holderRereadMs);
  final b = await context(store: openStore(dir), transport: tB, clock: clock, faults: f.outbox, lock: drainLock(), rereadMs: holderRereadMs);
  final violations = <String>[];
  tA.onFrame = (fr) {
    if (!a.outbox.isHolder) violations.add('A wrote ${fr.text} without the lock');
  };
  tB.onFrame = (fr) {
    if (!b.outbox.isHolder) violations.add('B wrote ${fr.text} without the lock');
  };
  await flush();
  check(nobodyHolds(), 'nobody requests the lock without a ready session');

  tA.open();
  await flush();
  check(a.outbox.isHolder, 'A holds while ready');
  tB.open();
  await clock.advance(200);
  check(!b.outbox.isHolder, 'B waits');

  // Composed in the context that does not drain; sent by the one that does, at
  // its next read.
  final fromB = await b.composeText('from B');
  await clock.advance(holderRereadMs);
  same(tA.framesFor(fromB.clientId).length, 1, 'the holder sends it');
  same(tB.frames.length, 0, 'only the holder writes frames');
  same(a.item(fromB.clientId)?.state, OutboxState.sending, 'the holder reads it SENDING');
  tA.ack(fromB.clientId);
  await flush();
  same(a.item(fromB.clientId)?.state, OutboxState.sent, 'and SENT on its ack');

  // The holder drops mid-drain; the other takes over and resends.
  final dropped = await a.composeText('in flight when A dropped');
  await flush();
  same(tA.framesFor(dropped.clientId).length, 1, 'A had it in flight');
  tA.close();
  await flush();
  check(!a.outbox.isHolder, 'A releases when its session stops being ready');
  await clock.advance(200);
  check(b.outbox.isHolder, 'B takes over');
  same(tB.framesFor(dropped.clientId).length, 1, 'B resends under the same clientId');

  // Counted once per refusal received, not once per context.
  tB.refuse(dropped.clientId, ErrorCode.internal, 'db down', retryable: true);
  await flush();
  final record = (await b.stored(dropped.clientId))!;
  same(record.internalRetries, 1, 'one refusal, one count');
  same(record.attempts, 2, 'two frames, two attempts');

  tB.close();
  await flush();
  check(nobodyHolds(), 'released when the last ready session ends');
  same(violations.join('; '), '', 'no frame was written by a context without the lock');
}

final criteria = <int, Future<void> Function(F f)>{0: c0, 1: c1, 2: c2, 3: c3, 4: c4, 7: c7, 8: c8, 9: c9, 10: c10, 11: c11, 12: c12, 15: c15};

/// The fault table of web/outbox.test.ts: `['name', n],` rows inside
/// `const FAULTS … = [ … ]`.
List<(String, int)> referenceFaults(String source) {
  final start = source.indexOf('const FAULTS');
  if (start < 0) throw StateError('web/outbox.test.ts declares no FAULTS table');
  final body = source.substring(start, source.indexOf('\n]\n', start));
  return [for (final m in RegExp(r"\['(\w+)', (\d+)\]").allMatches(body)) (m.group(1)!, int.parse(m.group(2)!))];
}

/// The criterion numbers of web/outbox.test.ts: the keys of its `criteria`.
List<int> referenceCriteria(String source) {
  final start = source.indexOf('const criteria');
  if (start < 0) throw StateError('web/outbox.test.ts declares no criteria');
  final body = source.substring(start, source.indexOf('\n}\n', start));
  return [for (final m in RegExp(r'(\d+): c\d+').allMatches(body)) int.parse(m.group(1)!)];
}

void main() {
  for (final MapEntry(key: n, value: run) in criteria.entries) {
    test('criterion $n holds', () => run(clean));
  }

  for (final (name, n) in faultTable) {
    test('fault $name makes criterion $n fail', () async {
      Object? caught;
      try {
        await criteria[n]!(fault(name));
      } catch (e) {
        caught = e;
      }
      // Which check caught it, so a fault failing for the wrong reason is
      // visible in the run's output.
      print('$name → criterion $n: ${'$caught'.split('\n').first}');
      expect(caught, isNotNull, reason: 'criterion $n still passed with $name on');
    });
  }

  group('the table is the reference\'s', () {
    final reference = File('../web/outbox.test.ts').readAsStringSync();

    test('fault names and the criterion each must fail, row for row', () {
      final want = referenceFaults(reference);
      expect(want.length, greaterThanOrEqualTo(16), reason: 'parsed ${want.length} rows from web/outbox.test.ts');
      expect(faultTable, want);
    });

    test('criterion numbers: the reference\'s, less the two that render the app (5, 6) and the one with no Dart counterpart (16)', () {
      final want = referenceCriteria(reference).where((n) => !const {5, 6, 16}.contains(n)).toList();
      expect(criteria.keys.toList(), want);
      expect(criteria.keys.toList(), [0, 1, 2, 3, 4, 7, 8, 9, 10, 11, 12, 15]);
    });

    test('the comparison fails when the tables differ: a row added to the reference, planted', () {
      final planted = reference.replaceFirst("  ['lockWithoutReady', 15],\n", "  ['lockWithoutReady', 15],\n  ['aFaultFromALaterTicket', 4],\n");
      expect(planted, isNot(reference), reason: 'the plant landed');
      expect(referenceFaults(planted), isNot(faultTable));
      final moved = reference.replaceFirst("['remintOnRetry', 1]", "['remintOnRetry', 2]");
      expect(referenceFaults(moved), isNot(faultTable), reason: 'and when a fault\'s criterion number differs');
    });

    test('every fault named in the table has its switch, and no other name does', () {
      for (final (name, _) in faultTable) {
        final f = fault(name);
        expect(identical(f.outbox, OutboxFaults.none) && identical(f.store, StoreFaults.none), isFalse, reason: name);
      }
      expect(() => fault('aFaultFromALaterTicket'), throwsArgumentError);
    });
  });

  group('the outbox\'s file', () {
    test('it is catenary-outbox.db, beside catenary.db and not in it', () async {
      final dir = tempDir();
      SqliteJournal.open(catenaryDbPath(dir)).close();
      final store = openStore(dir);
      await (await context(store: store)).composeText();
      expect(Directory(dir).listSync().map((e) => e.uri.pathSegments.last).toSet(), {'catenary.db', 'catenary-outbox.db'});
      expect(outboxDbPath(dir), '$dir/catenary-outbox.db');
      final tables = {for (final r in store.database.select("SELECT name FROM sqlite_schema WHERE type = 'table'")) r['name']};
      expect(tables, {'outbox'}, reason: 'and it holds the outbox and nothing of the journal\'s');
      expect(store.database.userVersion, outboxMigrations.length);
      if (Platform.isLinux) expect((File(outboxDbPath(dir)).statSync().mode & 0x1ff).toRadixString(8), '600');
    });

    test('the outbox store\'s own connection runs the rollback journal and synchronous = FULL (CANT-202 ruling 1)', () {
      final store = openStore(tempDir());
      expect(store.database.select('PRAGMA journal_mode').single.values.single, 'delete');
      expect(store.database.select('PRAGMA synchronous').single.values.single, 2, reason: '2 is FULL');
    });

    test('the table has no AUTOINCREMENT: order is a column this code writes', () {
      final store = openStore(tempDir());
      final sql = store.database.select("SELECT sql FROM sqlite_schema WHERE name = 'outbox'").single['sql'] as String;
      expect(sql.toUpperCase(), isNot(contains('AUTOINCREMENT')));
      expect(sql, contains('"order" INTEGER NOT NULL'));
      expect(store.database.select("SELECT name FROM sqlite_schema WHERE name = 'sqlite_sequence'"), isEmpty);
      expect(outboxMigrations.join().toUpperCase(), isNot(contains('AUTOINCREMENT')));
    });

    test('an order another entry of the account holds is a refused write, never a row replaced', () async {
      final store = openStore(tempDir());
      final first = await store.add(OutboxEntry(clientId: uuid(1), accountId: me, conversationId: conv, order: 0, composedAt: at, text: 'first'));
      expect(first.order, 1);
      await expectLater(store.put(OutboxEntry(clientId: uuid(2), accountId: me, conversationId: conv, order: 1, composedAt: at)), throwsA(anything));
      expect([for (final e in await store.list()) e.text], ['first']);
      final elsewhere = await store.add(OutboxEntry(clientId: uuid(3), accountId: other, conversationId: conv, order: 0, composedAt: at));
      expect(elsewhere.order, 1, reason: 'order is per account');
    });

    test('an update to an entry that is gone does nothing, and a stored field outside the shape is refused', () async {
      final store = openStore(tempDir());
      expect(await store.update(uuid(9), (e) => e.attempts++), isNull);
      expect(await store.list(), isEmpty);
      store.database.execute('INSERT INTO outbox (client_id, account_id, "order", record) VALUES (?, ?, 1, ?)', [uuid(1), me, '{"v":1,"ack":{}}']);
      await expectLater(store.list(), throwsFormatException);
    });
  });

  // CANT-202 ruling 0: the holder re-reads the store on a timer, so an entry another context composed is sent without a compose, ack or reconnect
  // in the holder.
  group('the holder\'s re-read', () {
    // Two contexts over one outbox file; B composes while A holds the lock. Returns how many frames A has written for the entry after the clock
    // advances `holderRereadMs`, with nothing else happening in A.
    Future<int> framesAfterOneInterval(num rereadMs) async {
      final dir = tempDir();
      final clock = FakeClock();
      final server = ScriptedServer();
      final tA = ScriptedTransport(server);
      final tB = ScriptedTransport(server);
      SqliteDrainLock drainLock() {
        final locks = SqliteLocks(dir, timers: clock);
        addTearDown(locks.close);
        return SqliteDrainLock(locks, timers: clock);
      }

      await context(store: openStore(dir), transport: tA, clock: clock, lock: drainLock(), rereadMs: rereadMs);
      final b = await context(store: openStore(dir), transport: tB, clock: clock, lock: drainLock(), rereadMs: rereadMs);
      await flush();
      tA.open();
      await flush();
      tB.open();
      await clock.advance(200);
      final entry = await b.composeText('from the other context');
      await clock.advance(holderRereadMs);
      same(tB.frames.length, 0, 'only the holder writes frames');
      return tA.framesFor(entry.clientId).length;
    }

    test('with the interval as built, the holder sends it after holderRereadMs', () async {
      same(await framesAfterOneInterval(holderRereadMs), 1, 'the holder sent it with no compose, ack or reconnect of its own');
    });

    test('with the interval at one hour, the same advance leaves it unsent', () async {
      same(await framesAfterOneInterval(3600000), 0, 'nothing woke the holder');
    });
  });

  group('attachments', () {
    test('the default uploader refuses, and the entry fails with its message — never a loop', () async {
      final transport = ScriptedTransport();
      final ctx = await context(transport: transport);
      transport.open();
      await flush();
      final entry = await ctx.outbox.compose(OutboxDraft(conversationId: conv, text: 'with a photo', attachments: const [OutboundAttachmentDraft(kind: 'image', filename: 'a.png')]));
      await flush();
      final item = ctx.item(entry.clientId)!;
      expect(item.state, OutboxState.failed);
      expect(projectOutbox(item).error, RefusingUploader.message);
      expect(transport.frames, isEmpty, reason: 'nothing was sent without its handle');
      await ctx.outbox.retry(entry.clientId);
      await ctx.clock.advance(outboxBackoffCapMs);
      expect(transport.frames, isEmpty, reason: 'and a RETRY does not send it either');
      // RETRY offers it to the uploader again, which refuses again: the entry
      // is FAILED, with its RETRY and its DELETE, and never QUEUED with
      // nothing that will send it.
      final again = ctx.item(entry.clientId)!;
      expect(again.state, OutboxState.failed, reason: 'failed again, not stuck pending');
      expect(projectOutbox(again).error, RefusingUploader.message);
      expect((await ctx.stored(entry.clientId))!.status, OutboxStatus.failed, reason: 'and stored so');
      expect(await ctx.outbox.discard(entry.clientId), isTrue, reason: 'so DELETE is still offered');
      expect(await ctx.store.list(), isEmpty);
    });

    test('an uploader that succeeds: each handle is persisted, and the entry then drains with them', () async {
      final transport = ScriptedTransport();
      final offered = <String?>[];
      final ctx = Ctx.over(MemoryOutboxStore(), transport, FakeClock());
      ctx.outbox = await Outbox.open(
        store: ctx.store,
        transport: transport,
        accountId: account,
        now: ctx.clock.now,
        timers: ctx.clock,
        uploader: _Handles((a) {
          offered.add(a.filename);
          return uuid(4000 + offered.length);
        }),
      );
      addTearDown(ctx.close);
      transport.open();
      await flush();
      final entry = await ctx.outbox.compose(OutboxDraft(
        conversationId: conv,
        text: 'two photos',
        attachments: const [OutboundAttachmentDraft(kind: 'image', filename: 'a.png'), OutboundAttachmentDraft(kind: 'voice', durationMs: 900)],
      ));
      await flush();
      expect(offered, ['a.png', null], reason: 'each offered once, in order');
      final stored = (await ctx.stored(entry.clientId))!;
      expect([for (final a in stored.attachments) (a.uploadId, a.upload)], [(uuid(4001), UploadState.uploaded), (uuid(4002), UploadState.uploaded)], reason: 'the handles are on the stored entry');
      expect(transport.framesFor(entry.clientId).single.toJson()['attachments'], [
        {'kind': 'image', 'upload_id': uuid(4001)},
        {'kind': 'voice', 'upload_id': uuid(4002)},
      ]);
      expect(ctx.item(entry.clientId)!.state, OutboxState.sending);
    });

    test('an upload that fails part-way keeps the handles it had, and RETRY asks only for the rest', () async {
      final transport = ScriptedTransport();
      final offered = <String?>[];
      var refuseSecond = true;
      final ctx = Ctx.over(MemoryOutboxStore(), transport, FakeClock());
      ctx.outbox = await Outbox.open(
        store: ctx.store,
        transport: transport,
        accountId: account,
        now: ctx.clock.now,
        timers: ctx.clock,
        uploader: _Handles((a) {
          offered.add(a.filename);
          if (a.filename == 'b.png' && refuseSecond) throw const UploadRefused('the second one would not go');
          return uuid(4100 + offered.length);
        }),
      );
      addTearDown(ctx.close);
      transport.open();
      await flush();
      final entry = await ctx.outbox.compose(OutboxDraft(
        conversationId: conv,
        attachments: const [OutboundAttachmentDraft(kind: 'image', filename: 'a.png'), OutboundAttachmentDraft(kind: 'image', filename: 'b.png')],
      ));
      await flush();
      expect(projectOutbox(ctx.item(entry.clientId)!).error, 'the second one would not go');
      expect(transport.frames, isEmpty);
      refuseSecond = false;
      await ctx.outbox.retry(entry.clientId);
      await flush();
      expect(offered, ['a.png', 'b.png', 'b.png'], reason: 'the first was not uploaded twice');
      expect(transport.framesFor(entry.clientId), hasLength(1));
    });

    test('a non-holder\'s refresh announces a status another context changed, not only a count', () async {
      final store = MemoryOutboxStore();
      final lock = InProcessLockHub();
      final transport = ScriptedTransport();
      final holder = await context(store: store, transport: transport, lock: lock.lock());
      final views = <List<OutboxState>>[];
      final watcher = await context(store: store, lock: lock.lock(), onView: (items) => views.add([for (final i in items) i.state]));
      transport.open();
      await flush();
      final entry = await holder.composeText('refused in the other context');
      await flush();
      await watcher.outbox.refresh();
      expect(views.last, [OutboxState.queued]);
      transport.refuse(entry.clientId, ErrorCode.notAMember, 'no', retryable: false);
      await flush();
      views.clear();
      await watcher.outbox.refresh();
      expect(views, [
        [OutboxState.failed],
      ], reason: 'one entry before and one after: the change is the status');
    });
  });
}

/// An uploader that answers with whatever `handle` returns, or fails with
/// what it throws.
final class _Handles implements Uploader {
  const _Handles(this.handle);

  final Uuid Function(OutboundAttachmentDraft a) handle;

  @override
  Future<Uuid> upload(OutboxEntry entry, OutboundAttachmentDraft attachment) async => handle(attachment);
}
