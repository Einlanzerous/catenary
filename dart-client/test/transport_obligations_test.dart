/// CANT-24's five obligations (docs/decisions/cant-24-resume.md) and CANT-103's
/// client rules (recorded on CANT-42), each BY NUMBER and each with the
/// negative control that must make it fail. The twin of
/// web/src/transport/test/journal.test.ts, with the two-context stale-journal
/// catch-up from idb-journal.test.ts over a real SQLite file.
///
/// EACH SCENARIO IS A FUNCTION OF ITS FAULTS. The clean run must hold the
/// property, and the run with the named fault must break it: a check nobody has
/// seen fail is a claim about the check.
library;

import 'dart:async';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';

// --- obligation 1 ---------------------------------------------------------------

Future<void> obligation1() async {
  final gate = Completer<void>();
  var holding = true;
  final journal = MemoryJournal(beforeCommit: (source) async {
    if (source == AppliedSource.page && holding) await gate.future;
  });
  final r = Rig(journal: journal);
  final applied = <Applied>[];
  r.t.onApply(applied.add);
  r.sync.answer = (_) => bootstrapPage(3, [message(1), message(2), message(3)]);
  r.t.start();
  await flush();
  expect(r.sync.requests, hasLength(1), reason: 'the page arrived and its write is held open');
  expect(applied, isEmpty, reason: 'no Applied before the write lands');
  expect(r.t.snapshot().messages, isEmpty, reason: 'no message visible');
  expect(r.t.snapshot().cursor, isNull, reason: 'and no cursor');
  expect(r.t.status().messages, 0);
  holding = false;
  gate.complete();
  await flush();
  expect(applied, hasLength(1));
  expect(applied.single.cursor, 3, reason: 'the Applied carries the cursor it landed with');
  expect(r.t.snapshot().messages, hasLength(3));
  expect(r.t.snapshot().cursor, 3);
}

// --- obligation 2 ---------------------------------------------------------------

Future<({bool liveMovedCursor, bool reemissionApplied, bool cursorWentBack, List<String> countedTwice, int? cursor})> obligation2(
  Faults faults,
) async {
  final r = Rig(faults: faults);
  final m = [for (var n = 1; n <= 4; n++) message(n)];
  var committed4 = false;
  r.sync.answer = (req) {
    if (req.after == 0) return bootstrapPage(3, m.sublist(0, 3));
    // Once message 4 is committed, the catch-up re-carries it, as a page does
    // for anything that arrived live.
    if (req.after == 3) return committed4 ? page(4, messages: [m[3]]) : page(3);
    // A stale page: bounded below the held cursor.
    return page(1);
  };
  r.t.start();
  final s = await r.connect();
  final cursorBefore = r.t.status().cursor;

  committed4 = true;
  s.frame(messageFrame(m[3]));
  await flush();
  final liveMovedCursor = r.t.status().cursor != cursorBefore;

  r.t.catchUp();
  await flush();
  // A CANT-92 re-emission of message 2: the same id, its original log_seq, a
  // new read_by.
  s.frame(messageFrame(message(2, readBy: 2, state: DeliveryState.read)));
  await flush();
  final held2 = r.t.snapshot().messages.firstWhere((x) => x.id == m[1].id);

  // And a page below the cursor must not move it back.
  r.sync.answer = (_) => page(1);
  final cursorHigh = r.t.status().cursor;
  r.t.catchUp();
  await flush();
  final cursorWentBack = (r.t.status().cursor ?? 0) < (cursorHigh ?? 0);

  final counted = r.journal.counted;
  return (
    liveMovedCursor: liveMovedCursor,
    reemissionApplied: held2.readBy == 2,
    cursorWentBack: cursorWentBack,
    countedTwice: [
      for (final (i, id) in counted.indexed)
        if (counted.indexOf(id) != i) id,
    ],
    cursor: cursorHigh,
  );
}

// --- obligation 3 and CANT-103 rules 1 and 3 ----------------------------------------

/// A trigger that lands while a page is in flight. The page was issued before
/// it and is bounded below the triggering message's log_seq; only a catch-up
/// that re-arms its end condition asks again and finds the message.
Future<({int held, int discards, bool caughtUp, int maxInFlight, int counted})> obligation3(Faults faults) async {
  final r = Rig(faults: faults);
  final c2 = uuid(102);
  final newRoom = message(5, conversationId: c2, seq: 1);
  final held = Completer<SyncResponse>();
  var inFlight = 0;
  var maxInFlight = 0;
  r.sync.answer = (req) async {
    inFlight++;
    if (inFlight > maxInFlight) maxInFlight = inFlight;
    try {
      if (req.after == 0) return bootstrapPage(3, [message(1), message(2), message(3)]);
      if (req.after == 3) return await held.future;
      // Asked again after the trigger: the new room, introduced on its page.
      return page(5, messages: [newRoom], conversations: [conversation(c2)]);
    } finally {
      inFlight--;
    }
  };
  r.t.start();
  final s = await r.connect();
  s.frame(const ServerResyncRequired(reason: ResyncReason.cursorTooOld, logSeq: 4));
  await flush();
  // The request after=3 is in flight. The new room's first message arrives
  // live, naming a conversation this client does not hold.
  s.frame(messageFrame(newRoom));
  await flush();
  final discards = r.t.status().stats.introductionDiscards;
  // The in-flight page is bounded at 4: below the message's log_seq of 5.
  held.complete(page(4, messages: [message(4)]));
  await flush();
  return (
    held: r.t.snapshot().messages.where((m) => m.id == newRoom.id).length,
    discards: discards,
    caughtUp: r.t.status().caughtUp,
    maxInFlight: maxInFlight,
    counted: r.journal.counted.where((id) => id == newRoom.id).length,
  );
}

// --- obligation 4 -------------------------------------------------------------------

/// A server whose log is behind the cursor — a restore. The page requested from
/// the old cursor before `ready` must be dropped, and the client must end with
/// exactly the regrown log.
Future<({List<String> ids, List<String> want, int? cursor, int discards, int wipes})> obligation4(Faults faults) async {
  final r = Rig(faults: faults);
  final old = [for (var n = 1; n <= 6; n++) message(n)];
  final regrown = [for (var n = 1; n <= 3; n++) message(n, id: uuid(3000 + n), text: 'regrown $n')];
  r.sync.answer = (req) => req.after == 0 ? bootstrapPage(6, old) : page(req.after);
  r.t.start();
  final s = await r.connect();
  expect(r.t.status().cursor, 6);

  // The server is restored to log_seq 3 while this client is away.
  final stale = Completer<SyncResponse>();
  var restored = false;
  r.sync.answer = (req) {
    if (!restored) return stale.future;
    return req.after == 0 ? bootstrapPage(3, regrown) : page(req.after < 3 ? 3 : req.after);
  };
  s.serverClose(1001);
  await r.clock.advance(250);
  // The dial's /sync (after=6) is in flight, issued before any ready.
  final s2 = r.net.last;
  s2.open();
  restored = true;
  s2.frame(ready(logSeq: 3));
  await flush();
  // The old server's answer to the request issued before the wipe lands now.
  stale.complete(page(7, messages: [message(7)]));
  await flush();
  final snap = r.t.snapshot();
  return (
    ids: [for (final m in snap.messages) m.id]..sort(),
    want: [for (final m in regrown) m.id]..sort(),
    cursor: snap.cursor,
    discards: r.t.status().stats.discards,
    wipes: r.t.status().wipes,
  );
}

// --- obligation 5 -------------------------------------------------------------------

/// A room held with `firstUnreadSeq` 1 and one name is renamed and read on the
/// server. Nothing live says so; the next catch-up's page does.
Future<({String? beforeCatchUp, String? afterCatchUp, int? unreadAfter, int? cursor, bool holdsPageMessage, String? afterLive})> obligation5(
  Faults faults,
) async {
  final r = Rig(faults: faults);
  final m = [for (var n = 1; n <= 4; n++) message(n)];
  var moved = false;
  r.sync.answer = (req) {
    if (req.after == 0) {
      return page(3, messages: m.sublist(0, 3), conversations: [conversation(conv, name: 'before', firstUnreadSeq: 1, headSeq: 3)], users: [user(me), user(other)]);
    }
    if (moved && req.after == 3) {
      return page(4, messages: [m[3]], conversations: [conversation(conv, name: 'after', firstUnreadSeq: 3, headSeq: 4)]);
    }
    return page(moved ? 4 : 3);
  };
  r.t.start();
  final s = await r.connect();
  Conversation held() => r.t.snapshot().conversations.firstWhere((c) => c.id == conv);

  // The rename happens. A live message arrives; it carries no conversation.
  moved = true;
  s.frame(const Ping(id: 'the-socket-is-live'));
  await flush();
  final beforeCatchUp = held().name;

  r.t.catchUp();
  await flush();
  final afterCatchUp = held().name;
  final unreadAfter = held().firstUnreadSeq;
  final cursor = r.t.status().cursor;
  final holdsPageMessage = r.t.snapshot().messages.any((x) => x.id == m[3].id);

  s.frame(ServerConversationFrame(conversation: conversation(conv, name: 'live', firstUnreadSeq: 5, headSeq: 4)));
  await flush();
  return (
    beforeCatchUp: beforeCatchUp,
    afterCatchUp: afterCatchUp,
    unreadAfter: unreadAfter,
    cursor: cursor,
    holdsPageMessage: holdsPageMessage,
    afterLive: held().name,
  );
}

// --- the stale-journal catch-up (CANT-175) ------------------------------------------

/// Two contexts over one SQLite file. A holds the bootstrap page through a
/// transport; B wipes the shared journal underneath it; then a live message
/// reaches A, whose mirror still believes it holds everything.
Future<({int syncsPulled, int? askedFrom, String? journalError, int? cursor, bool holdsLive})> staleLiveWrite(Faults faults) async {
  final path = catenaryDbPath(tempDir());
  final a = SqliteJournal.open(path);
  addTearDown(a.close);
  final r = Rig(journal: a, faults: faults);
  r.sync.answer = (_) => bootstrapPage(3, [message(1), message(2), message(3)]);
  r.t.start();
  final s = await r.connect();
  expect(r.t.status().cursor, 3, reason: 'context A holds the bootstrap page');

  final b = SqliteJournal.open(path);
  await b.wipe();
  b.close();

  final live = message(4);
  r.sync.answer = (_) => bootstrapPage(4, [message(1), message(2), message(3), live]);
  final before = r.sync.requests.length;
  s.frame(messageFrame(live));
  await flush();
  return (
    syncsPulled: r.sync.requests.length - before,
    askedFrom: r.sync.requests.length > before ? r.sync.requests[before].after : null,
    journalError: r.t.status().journalError?.name,
    cursor: r.t.status().cursor,
    holdsLive: r.t.snapshot().messages.any((m) => m.id == live.id),
  );
}

// --- a page in flight across another context's wipe (CANT-199) -----------------------

/// The same two contexts, with a `/sync` page IN FLIGHT across the wipe. A
/// holds 1–3 and has asked from 3; B wipes; a live message is refused as stale
/// in A, whose mirror reloads to the empty store; and only then does the page
/// A asked for from 3 arrive, carrying 4 and 5. The scripted server holds 1–5
/// at `log_seq` 5 throughout.
Future<({int? cursor, List<int> held, List<int> askedAfterRefusal, String? journalError})> pageInFlightAcrossAWipe(Faults faults) async {
  final path = catenaryDbPath(tempDir());
  final a = SqliteJournal.open(path);
  addTearDown(a.close);
  final r = Rig(journal: a, faults: faults);
  final all = [for (var n = 1; n <= 5; n++) message(n)];
  r.sync.answer = (_) => bootstrapPage(3, all.sublist(0, 3));
  r.t.start();
  final s = await r.connect();
  expect(r.t.status().cursor, 3, reason: 'context A holds the bootstrap page');

  final inFlight = Completer<SyncResponse>();
  r.sync.answer = (req) => req.after == 0 ? bootstrapPage(5, all) : (req.after == 3 ? inFlight.future : page(5));
  final before = r.sync.requests.length;
  r.t.catchUp();
  await flush();
  expect([for (final q in r.sync.requests.skip(before)) q.after], [3], reason: 'one page is in flight, asked from 3');

  final b = SqliteJournal.open(path);
  await b.wipe();
  b.close();

  s.frame(messageFrame(all[4]));
  await flush();
  expect(r.t.status().cursor, isNull, reason: 'the live write was refused as stale, and A reloaded the wiped store');
  final refusedAt = r.sync.requests.length;
  expect(refusedAt, before + 1, reason: 'and nothing is asked while the page is still in flight: one catch-up at a time');

  inFlight.complete(page(5, messages: all.sublist(3)));
  await flush();
  return (
    cursor: r.t.status().cursor,
    held: [for (final m in r.t.snapshot().messages) m.logSeq]..sort(),
    askedAfterRefusal: [for (final q in r.sync.requests.skip(refusedAt)) q.after],
    journalError: r.t.status().journalError?.name,
  );
}

void main() {
  test('obligation 1 · persist before render: nothing a page carries is observable before its cursor is written', obligation1);

  test('obligation 1 · over SQLite the cursor is durable before the Applied is emitted', () async {
    final path = catenaryDbPath(tempDir());
    final journal = SqliteJournal.open(path);
    addTearDown(journal.close);
    final r = Rig(journal: journal);
    final storedAtApply = <int?>[];
    r.t.onApply((a) {
      // What a second connection reads from the file at the moment of the emit.
      final other = SqliteJournal.open(path);
      storedAtApply.add(other.cursor);
      expect(other.messageCount, a.messages.length);
      other.close();
    });
    r.sync.answer = (_) => bootstrapPage(3, [message(1), message(2), message(3)]);
    r.t.start();
    await flush();
    expect(storedAtApply, [3]);
  });

  test('obligation 2 · only a page moves the cursor, forward only, and dedupe is by id', () async {
    final clean = await obligation2(Faults.none);
    expect(clean.liveMovedCursor, isFalse, reason: 'a live message frame moved no cursor');
    expect(clean.cursor, 4, reason: 'the page moved it to its own log_seq');
    expect(clean.cursorWentBack, isFalse, reason: 'a lower page did not move it back');
    expect(clean.reemissionApplied, isTrue, reason: 'a later record for a held id replaced it');
    expect(clean.countedTwice, isEmpty, reason: 'and nothing was counted twice');
  });

  test('obligation 2 · negative control cursorOnLiveFrames makes it fail', () async {
    expect((await obligation2(const Faults(cursorOnLiveFrames: true))).liveMovedCursor, isTrue);
  });

  test('obligation 2 · negative control dedupeByLogSeq makes it fail', () async {
    final broken = await obligation2(const Faults(dedupeByLogSeq: true));
    expect(broken.countedTwice, isNotEmpty, reason: 'the re-carried live message is counted twice');
    expect(broken.reemissionApplied, isFalse, reason: 'the re-emission below the cursor is dropped');
  });

  test('obligation 3 · a trigger mid-page re-arms the end condition, one catch-up at a time (CANT-103 rules 1 and 3)', () async {
    final clean = await obligation3(Faults.none);
    expect(clean.discards, 1, reason: 'rule 1: the unheld conversation\'s message was discarded, not buffered');
    expect(clean.held, 1, reason: 'rule 3: and is held at the end, exactly once');
    expect(clean.counted, 1);
    expect(clean.caughtUp, isTrue);
    expect(clean.maxInFlight, 1, reason: 'rule 3: never two catch-ups at once');
  });

  test('obligation 3 · negative control endCatchUpEarly makes it fail', () async {
    expect((await obligation3(const Faults(endCatchUpEarly: true))).held, 0);
  });

  test('CANT-103 rule 3 · negative control ignoreRetrigger makes it fail', () async {
    expect((await obligation3(const Faults(ignoreRetrigger: true))).held, 0);
  });

  test('obligation 3 · each trigger pulls a catch-up: a reconnect, ready, and resync_required', () async {
    final r = Rig();
    r.sync.answer = (req) => bootstrapPage(req.after);
    r.t.start();
    await flush();
    expect(r.sync.requests, hasLength(1), reason: 'the dial is a trigger, pulled before the upgrade');
    final held = Completer<SyncResponse>();
    r.sync.answer = (req) => held.future;
    final s = r.net.last..open();
    s.frame(ready());
    await flush();
    expect(r.sync.requests, hasLength(2), reason: 'ready{resumed: false} is a trigger');
    expect(r.t.status().caughtUp, isFalse);
    r.sync.answer = (req) => page(req.after);
    held.complete(page(0));
    await flush();
    expect(r.t.status().caughtUp, isTrue);
    final n = r.sync.requests.length;
    s.frame(const ServerResyncRequired(reason: ResyncReason.cursorTooOld, logSeq: 9));
    await flush();
    expect(r.sync.requests, hasLength(n + 1), reason: 'resync_required is a trigger');
    expect(r.t.status().stats.resyncs, 1);
    s.serverClose(1001);
    await r.clock.advance(250);
    expect(r.sync.requests, hasLength(n + 2), reason: 'and so is the reconnect');
  });

  test('obligation 4 · a ready below the cursor wipes, drops the stale page, and bootstraps from 0', () async {
    final clean = await obligation4(Faults.none);
    expect(clean.discards, 1);
    expect(clean.wipes, 1);
    expect(clean.ids, clean.want, reason: 'exactly the regrown log: nothing old, and the stale page dropped');
    expect(clean.cursor, 3);
  });

  test('obligation 4 · the wipe never touches the credential', () async {
    // The credential is not in the journal at all: a wipe is the journal's, and
    // the next dial presents the same pair.
    final r = Rig();
    r.sync.answer = (req) => req.after == 0 ? bootstrapPage(6, [message(6)]) : page(req.after);
    r.t.start();
    final s = await r.connect();
    s.frame(ready(logSeq: 1));
    await flush();
    expect(r.t.status().wipes, 1);
    s.serverClose(1001);
    await r.clock.advance(250);
    expect(r.net.last.protocols, ['catenary.v1', 'catenary.token.$token']);
  });

  test('obligation 4 · negative control skipWipe makes it fail', () async {
    final broken = await obligation4(const Faults(skipWipe: true));
    expect(broken.ids, isNot(broken.want));
  });

  test('obligation 5 · a rename and a moved unread marker are learned at the next catch-up, and not before', () async {
    final clean = await obligation5(Faults.none);
    expect(clean.beforeCatchUp, 'before', reason: 'nothing live carried the rename');
    expect(clean.afterCatchUp, 'after', reason: 'the catch-up\'s page did');
    expect(clean.unreadAfter, 3);
    expect(clean.afterLive, 'live', reason: 'a re-served conversation frame replaces the held record too');
  });

  test('obligation 5 · negative control keepHeldConversation keeps the held record and nothing else', () async {
    final broken = await obligation5(const Faults(keepHeldConversation: true));
    expect(broken.afterCatchUp, 'before', reason: 'the held record survived the page');
    expect(broken.afterLive, 'before', reason: 'and the live frame');
    expect(broken.holdsPageMessage, isTrue, reason: 'the message the page carried is held all the same');
    expect(broken.cursor, 4, reason: 'and the cursor moved, so client.Compare cannot see this fault');
  });

  test('obligation 5 · receipts are live: another user\'s changes nothing and triggers nothing', () async {
    final r = Rig();
    r.sync.answer = (req) => req.after == 0 ? bootstrapPage(3, [message(1), message(2), message(3)]) : page(req.after);
    r.t.start();
    final s = await r.connect();
    final before = wireShape(r.t.snapshot());
    final requests = r.sync.requests.length;
    final applied = <Applied>[];
    r.t.onApply(applied.add);
    s.frame(ServerReceipt(conversationId: conv, userId: other, upToSeq: 3));
    await flush();
    expect(wireShape(r.t.snapshot()), before);
    expect(r.sync.requests, hasLength(requests));
    expect(applied.single.receipts.single.upToSeq, 3, reason: 'the receipt is delivered as received');
  });

  test('obligation 5 · an own-user receipt pulls exactly one catch-up, and first_unread_seq moves only with the page', () async {
    final r = Rig();
    r.sync.answer = (req) => req.after == 0
        ? page(3, messages: [message(1), message(2), message(3)], conversations: [conversation(conv, firstUnreadSeq: 1, headSeq: 3)], users: [user(me), user(other)])
        : page(req.after);
    r.t.start();
    final s = await r.connect();
    final requests = r.sync.requests.length;
    final held = Completer<SyncResponse>();
    r.sync.answer = (_) => held.future;
    s.frame(ServerReceipt(conversationId: conv, userId: me, upToSeq: 3));
    await flush();
    expect(r.sync.requests, hasLength(requests + 1), reason: 'exactly one catch-up');
    expect(r.t.snapshot().conversations.single.firstUnreadSeq, 1, reason: 'unchanged until a page lands');
    held.complete(page(3, conversations: [conversation(conv, headSeq: 3)]));
    await flush();
    expect(r.t.snapshot().conversations.single.firstUnreadSeq, isNull, reason: 'the page is what moved it');
    expect(r.sync.requests, hasLength(requests + 1));
  });

  test('CANT-103 rule 1 · a message naming an unheld conversation is discarded, never buffered, and the catch-up returns it', () async {
    final r = Rig();
    final c2 = uuid(104);
    final first = message(4, conversationId: c2, seq: 1);
    var committed = false;
    r.sync.answer = (req) {
      if (req.after == 0) return bootstrapPage(3, [message(1), message(2), message(3)]);
      return committed ? page(4, messages: [first], conversations: [conversation(c2)]) : page(3);
    };
    r.t.start();
    final s = await r.connect();
    final requests = r.sync.requests.length;
    final applied = <Applied>[];
    r.t.onApply(applied.add);
    committed = true;
    s.frame(messageFrame(first));
    await flush();
    expect(r.t.status().stats.introductionDiscards, 1);
    expect(r.t.status().stats.liveFrames, 0, reason: 'the frame itself was not applied');
    expect(applied.map((a) => a.source), [AppliedSource.page], reason: 'what holds it is the page, not a buffered frame');
    expect(r.sync.requests.length, greaterThan(requests), reason: 'the discard pulled a catch-up');
    expect(r.t.snapshot().messages.where((m) => m.id == first.id), hasLength(1));
    expect(r.journal.counted.where((id) => id == first.id), hasLength(1));
  });

  test('CANT-103 rule 2 · catch-up runs until has_more: false, so a conversation met on a later page is still met', () async {
    final r = Rig();
    final c2 = uuid(105);
    r.sync.answer = (req) => switch (req.after) {
          0 => bootstrapPage(3, [message(1), message(2), message(3)], true),
          3 => page(5, messages: [message(4), message(5)], hasMore: true),
          _ => page(6, messages: [message(6, conversationId: c2, seq: 1)], conversations: [conversation(c2, headSeq: 1)]),
        };
    r.t.start();
    await r.connect();
    expect([for (final q in r.sync.requests.take(3)) q.after], [0, 3, 5], reason: 'each page asks from the last one\'s log_seq');
    expect(r.t.status().caughtUp, isTrue);
    expect(r.t.status().cursor, 6);
    expect(r.journal.holdsConversation(c2), isTrue);
    expect(r.t.status().messages, 6);
    expect(r.t.status().headSeqTotal, 1, reason: 'CANT-37: the target grows as conversations are discovered');
  });

  test('CANT-103 rule 4 · the same for an unheld author, and conversation and user frames apply by id and move no cursor', () async {
    final r = Rig();
    final stranger = uuid(250);
    final byStranger = message(4, authorId: stranger);
    var committed = false;
    r.sync.answer = (req) {
      if (req.after == 0) return bootstrapPage(3, [message(1), message(2), message(3)]);
      if (!committed) return page(3);
      return page(4, messages: [byStranger], users: [user(stranger)]);
    };
    r.t.start();
    final s = await r.connect();
    final c2 = uuid(103);
    s.frame(ServerConversationFrame(conversation: conversation(c2, name: 'first')));
    s.frame(ServerConversationFrame(conversation: conversation(c2, name: 'second')));
    s.frame(ServerUserFrame(user: user(uuid(260), 'Once')));
    s.frame(ServerUserFrame(user: user(uuid(260), 'Twice')));
    await flush();
    var snap = r.t.snapshot();
    expect(snap.conversations.where((c) => c.id == c2).single.name, 'second', reason: 'a later record replaces');
    expect(snap.users.firstWhere((u) => u.id == uuid(260)).name, 'Twice');
    expect(snap.cursor, 3, reason: 'introductions move no cursor');

    final requests = r.sync.requests.length;
    committed = true;
    s.frame(messageFrame(byStranger));
    await flush();
    expect(r.t.status().stats.introductionDiscards, 1);
    expect(r.sync.requests.length, greaterThan(requests), reason: 'the discard pulled a catch-up');
    snap = r.t.snapshot();
    expect(snap.messages.where((m) => m.id == byStranger.id), hasLength(1), reason: 'held exactly once after it');
    expect(r.journal.counted.where((id) => id == byStranger.id), hasLength(1));
  });

  test('two contexts · a live write refused as stale pulls a catch-up from the stored cursor and ends holding the record', () async {
    final clean = await staleLiveWrite(Faults.none);
    expect(clean.syncsPulled, 1, reason: 'the stale refusal pulled exactly one catch-up');
    expect(clean.askedFrom, 0, reason: 'A\'s mirror reloaded to the wiped, empty store first');
    expect(clean.journalError, isNull, reason: 'a stale refusal is not surfaced as a journal error');
    expect(clean.cursor, 4);
    expect(clean.holdsLive, isTrue, reason: 'the refused message is held, within that one /sync');
  });

  test('two contexts · negative control skipStaleCatchUp: the same scenario, and the record stays unheld', () async {
    final broken = await staleLiveWrite(const Faults(skipStaleCatchUp: true));
    expect(broken.syncsPulled, 0, reason: 'no catch-up follows the refusal');
    expect(broken.journalError, 'JournalStale', reason: 'the refusal surfaces as a journal error instead');
    expect(broken.holdsLive, isFalse, reason: 'and the refused message is not held');
  });

  test('two contexts · a page refused as stale is retried by its catch-up, from the stored cursor', () async {
    final path = catenaryDbPath(tempDir());
    final a = SqliteJournal.open(path);
    addTearDown(a.close);
    final r = Rig(journal: a);
    r.sync.answer = (_) => bootstrapPage(3, [message(1), message(2), message(3)]);
    r.t.start();
    await r.connect();
    expect(r.t.status().cursor, 3);

    final b = SqliteJournal.open(path);
    await b.wipe();
    b.close();

    r.sync.answer = (req) => req.after == 0 ? bootstrapPage(4, [for (var n = 1; n <= 4; n++) message(n)]) : page(4, messages: [message(4)]);
    final before = r.sync.requests.length;
    r.t.catchUp();
    await flush();
    expect(r.sync.requests[before].after, 3, reason: 'asked from A\'s mirror, which the wipe had made stale');
    expect(r.t.status().stats.syncErrors, 1, reason: 'the refused page is a failed catch-up');
    expect(r.t.status().cursor, isNull, reason: 'and A reloaded what is stored');
    // With a ready session the catch-up retries on its own backoff.
    await r.clock.advance(250);
    expect(r.sync.requests.last.after, 0, reason: 'the retry asks from the stored cursor');
    expect(r.t.status().cursor, 4);
    expect(r.t.status().messages, 4);
    expect(r.t.status().journalError, isNull);
  });

  // The existing test above is left as CANT-192 wrote it (CANT-199 criterion
  // 4), so the one thing its TypeScript twin asserts and it does not is here.
  test('two contexts · a page refused as stale makes no request before its backoff timer fires', () async {
    final path = catenaryDbPath(tempDir());
    final a = SqliteJournal.open(path);
    addTearDown(a.close);
    final r = Rig(journal: a);
    r.sync.answer = (_) => bootstrapPage(3, [message(1), message(2), message(3)]);
    r.t.start();
    await r.connect();

    final b = SqliteJournal.open(path);
    await b.wipe();
    b.close();

    r.sync.answer = (req) => req.after == 0 ? bootstrapPage(4, [for (var n = 1; n <= 4; n++) message(n)]) : page(4, messages: [message(4)]);
    final before = r.sync.requests.length;
    r.t.catchUp();
    await flush();
    await flush();
    expect(r.t.status().stats.syncErrors, 1, reason: 'applyPage reported the refused page as failed');
    expect(r.sync.requests.length, before + 1, reason: 'no /sync request is made before the backoff timer fires');
    await r.clock.advance(250);
    expect([for (final q in r.sync.requests.skip(before + 1)) q.after], [0], reason: 'the request made when it fires asks from the stored cursor');
  });

  test('two contexts · a page in flight across another context\'s wipe is dropped, and the pass starts again from the stored cursor', () async {
    final clean = await pageInFlightAcrossAWipe(Faults.none);
    expect(clean.cursor, 5, reason: 'the cursor is the server\'s log_seq');
    expect(clean.held, [1, 2, 3, 4, 5], reason: 'and every message the server has is held');
    expect(clean.askedAfterRefusal.first, 0, reason: 'the first request after the refusal asks from 0');
    expect(clean.journalError, isNull);
  });

  test('two contexts · negative control staleKeepsEpoch: the page lands on the wiped store, and the cursor sits above messages never asked for', () async {
    final broken = await pageInFlightAcrossAWipe(const Faults(staleKeepsEpoch: true));
    expect(broken.cursor, 5);
    expect(broken.held, [4, 5], reason: 'messages 1 to 3 are below the cursor and not held');
    expect(broken.askedAfterRefusal, isNot(contains(0)), reason: 'and nothing ever asks from 0');
  });
}
