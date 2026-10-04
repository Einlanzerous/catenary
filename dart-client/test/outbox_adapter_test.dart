/// The outbox over the real transport, through `TransportOutbox` — the twin of
/// web/src/outbox/test/transport-adapter.test.ts. The criteria run against a
/// scripted session; this is the same outbox against the transport's own
/// events, with a fake socket and a fake `/sync` under it, and the real SQLite
/// store and drain lock beside it.
library;

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

import 'harness.dart';

/// A record of `clientId`'s send, as the server echoes it to its author.
Message recordOf(Uuid clientId, int n) =>
    Message(id: uuid(8000 + n), seq: n, logSeq: n, conversationId: conv, authorId: me, at: at, state: DeliveryState.sent, text: 'echo $n', clientId: clientId);

ServerAck ackOf(ClientSend f, int n) => ServerAck(clientId: f.clientId, messageId: uuid(8000 + n), conversationId: f.conversationId, seq: n, logSeq: n, at: at);

/// An outbox over a transport over the fakes, with its files in `dir`.
Future<({Rig r, Outbox outbox, SqliteOutboxStore store, TransportOutbox adapter})> wired(String dir, {StagedJournal? journal}) async {
  final r = Rig(journal: journal);
  r.sync.answer = (req) => bootstrapPage(req.after);
  final store = SqliteOutboxStore.open(outboxDbPath(dir));
  final locks = SqliteLocks(dir, timers: r.clock);
  final adapter = TransportOutbox(r.t, r.logger);
  final outbox = await Outbox.open(
    store: store,
    transport: adapter,
    accountId: r.credential.userId,
    lock: SqliteDrainLock(locks, timers: r.clock),
    now: r.clock.now,
    timers: r.clock,
  );
  addTearDown(() {
    outbox.close();
    adapter.close();
    locks.close();
    store.close();
  });
  return (r: r, outbox: outbox, store: store, adapter: adapter);
}

OutboxState? stateOf(Outbox outbox, Uuid clientId) => outbox.view().where((i) => i.entry.clientId == clientId).firstOrNull?.state;

void main() {
  test('compose with no session is durable and QUEUED; ready sends it; the ack shows SENT; its record settles it', () async {
    final dir = tempDir();
    final w = await wired(dir);
    final entry = await w.outbox.compose(OutboxDraft(conversationId: conv, text: 'written offline'));
    expect(entry.clientId, matches(RegExp(r'^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')), reason: 'a version 4 UUID, minted here');
    expect(stateOf(w.outbox, entry.clientId), OutboxState.queued);
    expect((await w.store.list()).single.clientId, entry.clientId, reason: 'stored before anything else happened');

    w.r.t.start();
    final s = await w.r.connect();
    await flush();
    final sent = s.written_<ClientSend>();
    expect([for (final f in sent) f.toJson()], [ClientSend(clientId: entry.clientId, conversationId: conv, text: 'written offline').toJson()], reason: 'one frame, built from the entry and nothing else');
    expect(stateOf(w.outbox, entry.clientId), OutboxState.sending);
    expect(w.outbox.isHolder, isTrue);

    s.frame(ackOf(sent.single, 4));
    await flush();
    expect(stateOf(w.outbox, entry.clientId), OutboxState.sent);
    expect((await w.store.list()).single.toJson().containsKey('ack'), isFalse, reason: 'no ack is stored');

    s.frame(ServerMessageFrame(message: recordOf(entry.clientId, 4)));
    await flush();
    expect(w.outbox.view(), isEmpty, reason: 'the record carrying its clientId settles it');
    expect(await w.store.list(), isEmpty);
  });

  test('a record on a /sync page settles an entry as a live frame does, and a record with no clientId settles nothing', () async {
    final w = await wired(tempDir());
    w.r.t.start();
    final s = await w.r.connect();
    final a = await w.outbox.compose(OutboxDraft(conversationId: conv, text: 'a'));
    final b = await w.outbox.compose(OutboxDraft(conversationId: conv, text: 'b'));
    await flush();
    expect(s.written_<ClientSend>(), hasLength(2));
    s.frame(messageFrame(message(1)));
    await flush();
    expect(w.outbox.view(), hasLength(2), reason: 'somebody else\'s message is passed over');

    w.r.sync.answer = (req) => page(5, messages: [recordOf(a.clientId, 5)]);
    w.r.t.catchUp();
    await flush();
    expect([for (final i in w.outbox.view()) i.entry.clientId], [b.clientId]);
  });

  test('a session that ends before the ack leaves the entry pending, and it is resent under the same clientId', () async {
    final w = await wired(tempDir());
    w.r.t.start();
    final s = await w.r.connect();
    final entry = await w.outbox.compose(OutboxDraft(conversationId: conv, text: 'dropped mid-flight'));
    await flush();
    s.serverClose(1006);
    await flush();
    expect(stateOf(w.outbox, entry.clientId), OutboxState.queued);
    expect(w.outbox.isHolder, isFalse, reason: 'the lock is held only while ready');
    await w.r.clock.advance(250);
    final s2 = await w.r.connect();
    await w.r.clock.advance(100);
    expect(s2.written_<ClientSend>().single.clientId, entry.clientId);
    expect((await w.store.list()).single.attempts, 2);
  });

  test('a bare 1008 fails the in-flight entry, as the transport classified it; a refusal fails it with the server\'s message', () async {
    final w = await wired(tempDir());
    w.r.t.start();
    final s = await w.r.connect();
    final refused = await w.outbox.compose(OutboxDraft(conversationId: conv, text: 'refused'));
    await flush();
    s.frame(ServerError(code: ErrorCode.notAMember, message: 'removed from the room', retryable: false, clientId: refused.clientId));
    await flush();
    final item = w.outbox.view().single;
    expect(item.state, OutboxState.failed);
    expect(projectOutbox(item).error, 'removed from the room');

    final bare = await w.outbox.compose(OutboxDraft(conversationId: conv, text: 'bare'));
    await flush();
    s.serverClose(1008);
    await flush();
    final failed = w.outbox.view().firstWhere((i) => i.entry.clientId == bare.clientId);
    expect(failed.state, OutboxState.failed);
    expect(failed.entry.lastError, isA<Bare1008>());
    expect(w.r.t.status().terminal.kind, TerminalKind.protocol);
    expect(await w.store.list(), hasLength(2), reason: 'and the terminal deleted nothing');
  });

  test('obligation 4\'s wipe reaches the journal and leaves every outbox entry in place', () async {
    final dir = tempDir();
    final journal = SqliteJournal.open(catenaryDbPath(dir));
    addTearDown(journal.close);
    final w = await wired(dir, journal: journal);
    w.r.sync.answer = (req) => req.after == 0 ? bootstrapPage(6, [for (var n = 1; n <= 6; n++) message(n)]) : page(req.after);
    final entry = await w.outbox.compose(OutboxDraft(conversationId: conv, text: 'unsent when the server was restored'));
    w.r.t.start();
    await flush();
    expect(w.r.t.status().cursor, 6);

    // The server's log is behind the cursor: discard and bootstrap.
    w.r.sync.answer = (req) => bootstrapPage(2, [message(1), message(2)]);
    final s = await w.r.connect(ready(logSeq: 2));
    await flush();
    expect(w.r.t.status().wipes, 1);
    expect(journal.wipes, 1, reason: 'the journal was wiped, durably');
    expect((await w.store.list()).single.clientId, entry.clientId, reason: 'and the outbox entry is where it was');
    expect(s.written_<ClientSend>().single.clientId, entry.clientId, reason: 'and is sent on this session like any other');
  });

  test('an outbox opened after its record landed settles at once, and sends nothing', () async {
    final dir = tempDir();
    // An entry stored by an earlier launch, whose record is already held.
    final seed = SqliteOutboxStore.open(outboxDbPath(dir));
    await seed.add(OutboxEntry(clientId: uuid(5001), accountId: me, conversationId: conv, order: 0, composedAt: at, text: 'sent before the relaunch'));
    seed.close();
    final journal = MemoryJournal();
    await journal.applyPage(bootstrapPage(4, [recordOf(uuid(5001), 4)]), Faults.none);

    final w = await wired(dir, journal: journal);
    await flush();
    expect(w.outbox.view(), isEmpty);
    expect(await w.store.list(), isEmpty);
    w.r.t.start();
    final s = await w.r.connect();
    await w.r.clock.advance(100);
    expect(s.written_<ClientSend>(), isEmpty);
  });

  test('a relaunch after a kill finds the entry unsettled and sends it under the same clientId', () async {
    final dir = tempDir();
    final first = await wired(dir);
    final entry = await first.outbox.compose(OutboxDraft(conversationId: conv, text: 'composed, then the process died'));
    // No close, no stop: the next launch has only the file.
    final w = await wired(dir);
    expect(w.outbox.view().single.entry.clientId, entry.clientId);
    expect(w.outbox.view().single.state, OutboxState.queued);
    w.r.t.start();
    final s = await w.r.connect();
    await w.r.clock.advance(100);
    expect(s.written_<ClientSend>().single.clientId, entry.clientId);
  });

  test('NullTransport: an outbox with no session keeps what is composed, QUEUED', () async {
    final store = MemoryOutboxStore();
    final outbox = await Outbox.open(store: store, transport: const NullTransport(), accountId: me);
    addTearDown(outbox.close);
    final entry = await outbox.compose(OutboxDraft(conversationId: conv, text: 'kept'));
    expect(stateOf(outbox, entry.clientId), OutboxState.queued);
    expect(await store.list(), hasLength(1));
    await expectLater(Outbox.open(store: store, transport: const NullTransport(), accountId: null).then((o) => o.compose(OutboxDraft(conversationId: conv))), throwsStateError);
  });
}
