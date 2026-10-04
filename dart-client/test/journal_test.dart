/// The journal's rules, run against BOTH implementations: `MemoryJournal` and
/// `SqliteJournal` over a file. They share `StagedJournal`, and this is what
/// shows the durable one commits what the rules staged. Each rule with the
/// negative control that must make it fail: a check nobody has seen fail is a
/// claim about the check.
library;

import 'dart:async';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'support.dart';

void main() {
  final journals = <String, StagedJournal Function()>{
    'MemoryJournal': MemoryJournal.new,
    'SqliteJournal': () {
      final j = SqliteJournal.open(catenaryDbPath(tempDir()));
      addTearDown(j.close);
      return j;
    },
  };

  for (final MapEntry(key: name, value: open) in journals.entries) {
    group(name, () {
      test('a page lands whole: messages, conversations, users and the cursor', () async {
        final j = open();
        expect(j.cursor, isNull);
        final c2 = uuid(102);
        final a = await j.applyPage(
          page(
            3,
            messages: [message(2), message(1), message(3, conversationId: c2, seq: 1)],
            conversations: [conversation(conv, headSeq: 2), conversation(c2, headSeq: 1)],
            users: [user(other), user(me)],
          ),
          Faults.none,
        );
        expect(a.source, AppliedSource.page);
        expect(a.cursor, 3);
        expect(a.wiped, isFalse);
        expect(j.cursor, 3);
        expect(j.messageCount, 3);
        expect(j.headSeqTotal, 3);
        expect(j.holdsConversation(c2), isTrue);
        expect(j.holdsUser(me), isTrue);
        final s = j.snapshot();
        expect([for (final m in s.messages) m.id], [message(1).id, message(2).id, message(3).id], reason: 'by (conversation_id, seq)');
        expect([for (final c in s.conversations) c.id], [conv, c2]);
        expect([for (final u in s.users) u.id], [me, other]);
      });

      test('the cursor moves only on a page', () async {
        final j = open();
        await j.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), Faults.none);
        final a = await j.applyLive(LiveWrite(messages: [message(4)]), Faults.none);
        expect(a.source, AppliedSource.live);
        expect(a.cursor, 3);
        expect(j.cursor, 3, reason: 'a live message moved no cursor');
        expect(j.messageCount, 4, reason: 'and was held');
      });

      test('negative control cursorOnLiveFrames: a live message moves it', () async {
        final j = open();
        await j.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), Faults.none);
        await j.applyLive(LiveWrite(messages: [message(4)]), const Faults(cursorOnLiveFrames: true));
        expect(j.cursor, 4);
      });

      test('the cursor moves only forward', () async {
        final j = open();
        await j.applyPage(bootstrapPage(6, [message(6)]), Faults.none);
        final a = await j.applyPage(page(1), Faults.none);
        expect(a.cursor, 6);
        expect(j.cursor, 6, reason: 'a page below the cursor did not move it back');
        await j.applyPage(page(9), Faults.none);
        expect(j.cursor, 9);
      });

      /// A message that arrives live and is then re-carried by a page, and a
      /// CANT-92 re-emission of a held id below the cursor.
      Future<({List<String> counted, int? readBy})> dedupe(Faults faults) async {
        final j = open();
        await j.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), faults);
        await j.applyLive(LiveWrite(messages: [message(4)]), faults);
        await j.applyPage(page(3, messages: [message(4)]), faults);
        await j.applyLive(LiveWrite(messages: [message(2, readBy: 2, state: DeliveryState.read)]), faults);
        final held = j.snapshot().messages.firstWhere((m) => m.id == message(2).id);
        return (counted: j.counted, readBy: held.readBy);
      }

      test('a message is deduplicated by id and replaced by a later record', () async {
        final r = await dedupe(Faults.none);
        expect(r.counted, [for (final n in [1, 2, 3, 4]) message(n).id], reason: 'each id counted once');
        expect(r.readBy, 2, reason: 'the later record for a held id replaced it');
      });

      test('negative control dedupeByLogSeq: a duplicate count and a dropped re-emission', () async {
        final r = await dedupe(const Faults(dedupeByLogSeq: true));
        expect(r.counted.where((id) => id == message(4).id), hasLength(2), reason: 'the re-carried live message is counted twice');
        expect(r.readBy, isNull, reason: 'the re-emission below the cursor is dropped');
      });

      test('a wipe clears messages, conversations, users, the cursor and the counted log, and is counted', () async {
        final j = open();
        await j.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), Faults.none);
        expect(j.wipes, 0);
        final a = await j.wipe();
        expect(a.source, AppliedSource.wipe);
        expect(a.wiped, isTrue);
        expect(a.cursor, isNull);
        expect(wireShape(j.snapshot()), {'cursor': null, 'messages': [], 'conversations': [], 'users': []});
        expect(j.counted, isEmpty);
        expect(j.messageCount, 0);
        expect(j.holdsConversation(conv), isFalse);
        expect(j.holdsUser(me), isFalse);
        expect(j.wipes, 1);
        await j.wipe();
        expect(j.wipes, 2, reason: 'by one each time');
      });

      test('a message re-applied after a wipe is counted once, not twice', () async {
        final j = open();
        await j.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), Faults.none);
        await j.wipe();
        await j.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), Faults.none);
        expect(j.counted, [for (final n in [1, 2, 3]) message(n).id]);
        expect(j.cursor, 3);
      });
    });
  }

  test('MemoryJournal · nothing a page carries is observable before it lands', () async {
    final gate = Completer<void>();
    final j = MemoryJournal(beforeCommit: (_) => gate.future);
    Applied? applied;
    final write = j.applyPage(bootstrapPage(3, [message(1), message(2), message(3)]), Faults.none).then((a) => applied = a);
    await pumpEventQueue();
    expect(applied, isNull);
    expect(j.cursor, isNull);
    expect(j.messageCount, 0);
    expect(j.counted, isEmpty);
    gate.complete();
    await write;
    expect(applied!.cursor, 3);
    expect(j.messageCount, 3);
  });
}
