/// The journal-to-app projection (CANT-200 criterion 2): twin of
/// web/src/transport/test/project.test.ts's criterion 31. Neither has a Go
/// counterpart; the Go client renders nothing.
library;

import 'dart:convert';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';

/// A projection as the JSON its records encode to: the wire types have no `==`.
Map<String, Object?> shape(Projection p) => {
  'messages': [for (final m in p.messages) jsonEncode(m.toJson())],
  'conversations': [for (final c in p.conversations) jsonEncode(c.toJson())],
  'users': {for (final e in p.users.entries) e.key: jsonEncode(e.value.toJson())},
};

void main() {
  test('criterion 2 · project(snapshot) equals projectApplied folded from empty over every Applied, at every step', () async {
    final r = Rig();
    var projected = emptyProjection;
    final seen = <AppliedSource>[];
    r.t.onApply((a) {
      projected = projectApplied(projected, a);
      seen.add(a.source);
    });
    void same(String why) => expect(shape(projected), shape(project(r.t.snapshot())), reason: why);

    final c2 = uuid(104);
    final newcomer = uuid(270);
    var stage = 0;
    r.sync.answer = (req) {
      if (stage == 0) return bootstrapPage(3, [message(1), message(2), message(3)]);
      if (stage == 1) return page(req.after < 3 ? 3 : req.after);
      // After the restore: a regrown log.
      return req.after == 0 ? bootstrapPage(2, [message(1, id: uuid(4001)), message(2, id: uuid(4002))]) : page(req.after);
    };
    r.t.start();
    final s = await r.connect();
    await flush();
    same('a bootstrap page');
    expect(projected.messages, hasLength(3));
    stage = 1;

    s.frame(messageFrame(message(4)));
    await flush();
    same('a live message');
    expect(projected.messages.map((m) => m.id), contains(message(4).id));

    s.frame(messageFrame(message(2, readBy: 2, state: DeliveryState.read)));
    await flush();
    same('a read: a re-emission replacing a held message');
    expect(projected.messages.firstWhere((m) => m.id == message(2).id).readBy, 2);
    expect(projected.messages, hasLength(4), reason: 'replaced by id, not appended');

    s.frame(ServerConversationFrame(conversation: conversation(c2)));
    s.frame(ServerUserFrame(user: user(newcomer, 'New Person')));
    s.frame(messageFrame(message(5, conversationId: c2, seq: 1, authorId: newcomer)));
    await flush();
    same('an introduction');
    expect(projected.messages.any((m) => m.conversationId == c2), isTrue);
    expect(projected.users[newcomer]?.name, 'New Person');

    stage = 2;
    s.frame(ready(logSeq: 1));
    await flush();
    same('a wipe and a bootstrap');
    expect(seen, containsAll([AppliedSource.wipe, AppliedSource.page, AppliedSource.live]));
    expect([for (final m in projected.messages) m.id]..sort(), [uuid(4001), uuid(4002)]);
  });

  test('criterion 2 · each case is load-bearing: a wipe empties, a later record replaces, a page upserts', () {
    final held = JournalSnapshot(cursor: 3, messages: [message(1), message(2)], conversations: [conversation(conv)], users: [user(me)]);
    final base = project(held);
    final read = Applied(
      source: AppliedSource.live,
      cursor: 3,
      messages: [message(2, readBy: 2, state: DeliveryState.read)],
    );
    const wipe = Applied(source: AppliedSource.wipe, cursor: null, wiped: true);
    final regrown = Applied(
      source: AppliedSource.page,
      cursor: 1,
      messages: [message(1, id: uuid(4001))],
      users: [user(other)],
    );

    final wiped = projectApplied(base, wipe);
    expect(wiped.messages, isEmpty);
    expect(wiped.conversations, isEmpty);
    expect(wiped.users, isEmpty);
    expect(projectApplied(base, read).messages.last.readBy, 2);
    expect(projectApplied(base, read).messages, hasLength(2));
    expect([for (final m in projectApplied(wiped, regrown).messages) m.id], [uuid(4001)]);
    expect(projectApplied(base, regrown).messages, hasLength(3), reason: 'without a wipe the page adds to what is held');
  });

  test('criterion 2 · the projection is pure and ordered by (conversation, seq, id)', () {
    final a = Applied(
      source: AppliedSource.page,
      cursor: 1,
      messages: [
        message(3),
        message(1),
        message(2, conversationId: uuid(99), seq: 1),
      ],
      conversations: [conversation(uuid(101)), conversation(conv)],
      users: [user(me)],
    );
    final before = shape(emptyProjection);
    final out = projectApplied(emptyProjection, a);
    expect(shape(emptyProjection), before, reason: 'the input is not mutated');
    expect(shape(projectApplied(emptyProjection, a)), shape(out), reason: 'the same input gives the same output');
    expect([for (final m in out.messages) m.id], [uuid(1002), uuid(1001), uuid(1003)]);
    expect([for (final c in out.conversations) c.id], [conv, uuid(101)]);
  });
}
