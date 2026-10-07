/// The journal — mirrors web/src/transport/journal.ts, which mirrors
/// internal/client/journal.go (its message store; the credential half is the
/// credential layer's).
///
/// TWO IMPLEMENTATIONS OF ONE INTERFACE, AND ONE COPY OF THE RULES. `Journal` is
/// what the transport calls; `StagedJournal` holds the rules every
/// implementation shares — dedupe by id, the cursor forward only and on a page
/// only, the faults that break each — and stages every write on a copy of its
/// state. What differs is `commit`, the one step in which a staged write lands:
///
///   - `MemoryJournal` swaps the copy in. Rigs and tests use it.
///   - `SqliteJournal` (sqlite_journal.dart) writes the page to `catenary.db`
///     in one transaction and swaps the copy in only after its `COMMIT` has
///     returned, so a relaunch resumes from the stored cursor.
///
/// OBLIGATION 1 — PERSIST BEFORE RENDER. A page's messages, conversations,
/// users and cursor are staged and then land in one step, and nothing is
/// observable (`snapshot()`, the `Applied` the transport emits) until that step
/// has completed. In memory that is the ordering within one apply; over SQLite
/// it is also the durable half, because the step is the transaction.
///
/// Writes are serialized by the transport, one at a time, so an implementation
/// never sees two in flight FROM ONE TRANSPORT. Two contexts — two processes,
/// or two isolates of one — are two transports over one file, and
/// `SqliteJournal` guards that case itself: a wipe in one makes the other's
/// next write refuse and reload.
library;

import 'package:catenary_wire/catenary_wire.dart';
import 'package:meta/meta.dart';

import 'faults.dart';

enum AppliedSource { page, live, wipe }

/// Delivered through the transport's `onApply` once per journal write, and only
/// after that write — the cursor included — has completed (CANT-24 obligation
/// 1). Records are upserted by id; a later record replaces an earlier one.
final class Applied {
  const Applied({
    required this.source,
    required this.cursor,
    this.messages = const [],
    this.conversations = const [],
    this.users = const [],
    this.receipts = const [],
    this.wiped = false,
  });

  final AppliedSource source;

  /// The journal's cursor after this write; unchanged by `live`.
  final int? cursor;
  final List<Message> messages;
  final List<Conversation> conversations;
  final List<User> users;

  /// Live receipts as received. Under CANT-35 ruling 4 → B they have no store
  /// effect: an own-user receipt pulls a catch-up, another user's is a no-op.
  final List<ServerReceipt> receipts;

  /// Obligation 4: everything held before this write was discarded.
  final bool wiped;
}

/// The whole journal at a point in time, wire-shaped so soakrig can decode it
/// into `client.Snapshot`.
final class JournalSnapshot {
  const JournalSnapshot({
    required this.cursor,
    required this.messages,
    required this.conversations,
    required this.users,
  });

  final int? cursor;

  /// Ordered by (conversation_id, seq), then id.
  final List<Message> messages;

  /// By id.
  final List<Conversation> conversations;

  /// By id.
  final List<User> users;
}

/// What one live frame writes.
final class LiveWrite {
  const LiveWrite({this.messages = const [], this.conversations = const [], this.users = const []});

  final List<Message> messages;
  final List<Conversation> conversations;
  final List<User> users;
}

abstract interface class Journal {
  /// The committed cursor; null when none is held.
  int? get cursor;
  bool holdsConversation(Uuid id);
  bool holdsUser(Uuid id);

  /// Obligations 1 and 2: messages, conversations, users, then the cursor —
  /// which only ever moves forward — in one write.
  Future<Applied> applyPage(SyncResponse page, JournalFaults faults);

  /// A live frame, applied by id. Moves no cursor (unless the fault says so).
  Future<Applied> applyLive(LiveWrite write, JournalFaults faults);

  /// Obligation 4: messages, conversations, users, the cursor and the counted
  /// log. Never the credential, never the outbox, and never the [owner]: the
  /// wipes a transport decides are for the same account.
  Future<Applied> wipe();

  /// The `user_id` of the account this journal was written for; null until one
  /// has claimed it.
  Uuid? get owner;

  /// Makes this journal [accountId]'s, BEFORE ANYTHING IS BUILT OVER IT
  /// (CANT-230). A cursor is a position in the log as one account can see it,
  /// and the records are that account's: a credential must never run over a
  /// journal written for another. So, in one write:
  ///
  /// - held by [accountId] already: nothing is written;
  /// - held by another account: wiped exactly as [wipe] wipes, and [accountId]
  ///   recorded. Returns that wipe's `Applied`;
  /// - held by nobody (a journal from before there was an owner): kept, and
  ///   [accountId] recorded.
  ///
  /// Returns null when nothing was wiped. Whoever holds both a credential and
  /// a journal calls this with the credential's `userId` before constructing a
  /// transport; a re-enrollment interrupted before its wipe heals here.
  Future<Applied?> claim(Uuid accountId);
  JournalSnapshot snapshot();

  /// How many messages are held; `snapshot().messages.length` without the copy.
  int get messageCount;

  /// Sum of `head_seq` over every conversation currently held — the resync
  /// progress bar's target (CANT-37).
  int get headSeqTotal;
}

/// What a journal holds, in memory; replaced wholesale on commit.
final class JournalState {
  JournalState.empty(this.wipes)
      : messages = {},
        conversations = {},
        users = {},
        counted = [];

  JournalState.copy(JournalState s)
      : cursor = s.cursor,
        messages = Map.of(s.messages),
        conversations = Map.of(s.conversations),
        users = Map.of(s.users),
        counted = List.of(s.counted),
        wipes = s.wipes,
        owner = s.owner;

  int? cursor;
  final Map<Uuid, Message> messages;
  final Map<Uuid, Conversation> conversations;
  final Map<Uuid, User> users;

  /// R1's evidence log since the last wipe; see `StagedJournal.counted`.
  final List<Uuid> counted;
  int wipes;

  /// `Journal.owner`. Not part of what a wipe clears.
  Uuid? owner;
}

/// What one staged write changes, for an implementation that writes deltas
/// rather than whole states: the records it upserts, the ids it newly counted,
/// and the cursor after it. `wiped` says everything held before it goes first.
final class JournalDelta {
  const JournalDelta({
    required this.source,
    required this.wiped,
    required this.messages,
    required this.conversations,
    required this.users,
    required this.counted,
    required this.countedFrom,
    required this.cursor,
  });

  final AppliedSource source;
  final bool wiped;
  final List<Message> messages;
  final List<Conversation> conversations;
  final List<User> users;

  /// Appended to the evidence log, in order, after `countedFrom` entries.
  final List<Uuid> counted;
  final int countedFrom;
  final int? cursor;
}

/// The rules, once. Every write is staged on a copy of the state and handed to
/// `commit` with the delta it amounts to; `commit` lands it, and only then does
/// the state (and so `snapshot()`, `cursor` and the rest) show it. A `commit`
/// that throws leaves the state as it was, and the write's future fails.
abstract class StagedJournal implements Journal {
  StagedJournal([JournalState? initial]) : state = initial ?? JournalState.empty(0);

  /// What the last completed write left. An implementation whose store can
  /// change under it replaces this when it reloads.
  @protected
  JournalState state;

  /// THE ONE STEP IN WHICH A WRITE LANDS. Completes once it has; everything
  /// before it is staged on a copy nobody else can see.
  @protected
  Future<void> commit(JournalState next, JournalDelta delta);

  @override
  int? get cursor => state.cursor;

  @override
  bool holdsConversation(Uuid id) => state.conversations.containsKey(id);

  @override
  bool holdsUser(Uuid id) => state.users.containsKey(id);

  /// R1's evidence log, which `client.Compare` reads: every message id in the
  /// order it was first counted since the last wipe. A correct client counts
  /// each id exactly once, and `Duplicated` is an id counted twice. Not part of
  /// `JournalSnapshot`; a rig that holds the journal reads it here.
  List<Uuid> get counted => List.of(state.counted);

  /// How many discard-and-bootstraps this journal has been through.
  int get wipes => state.wipes;

  @override
  Future<Applied> applyPage(SyncResponse page, JournalFaults faults) async {
    final next = JournalState.copy(state);
    final messages = <Message>[];
    for (final m in page.messages) {
      if (_record(next, m, faults)) messages.add(m);
    }
    final conversations = _hold(next, page.conversations, faults);
    for (final u in page.users) {
      next.users[u.id] = u;
    }
    final held = next.cursor;
    if (held == null || page.logSeq > held) next.cursor = page.logSeq;
    await _land(next, AppliedSource.page, messages, conversations, page.users);
    return Applied(
      source: AppliedSource.page,
      cursor: next.cursor,
      messages: messages,
      conversations: conversations,
      users: List.of(page.users),
    );
  }

  @override
  Future<Applied> applyLive(LiveWrite write, JournalFaults faults) async {
    final next = JournalState.copy(state);
    final messages = <Message>[];
    for (final m in write.messages) {
      if (!_record(next, m, faults)) continue;
      messages.add(m);
      final held = next.cursor;
      if (faults.cursorOnLiveFrames && (held == null || m.logSeq > held)) next.cursor = m.logSeq;
    }
    final conversations = _hold(next, write.conversations, faults);
    for (final u in write.users) {
      next.users[u.id] = u;
    }
    await _land(next, AppliedSource.live, messages, conversations, write.users);
    return Applied(
      source: AppliedSource.live,
      cursor: next.cursor,
      messages: messages,
      conversations: conversations,
      users: List.of(write.users),
    );
  }

  @override
  Uuid? get owner => state.owner;

  /// In memory, where the state is the store. A durable implementation
  /// decides against what is STORED, in one transaction, and overrides this.
  @override
  Future<Applied?> claim(Uuid accountId) async {
    final held = state.owner;
    if (held == accountId) return null;
    if (held == null) {
      state = JournalState.copy(state)..owner = accountId;
      return null;
    }
    state = JournalState.empty(state.wipes + 1)..owner = accountId;
    return const Applied(source: AppliedSource.wipe, cursor: null, wiped: true);
  }

  @override
  Future<Applied> wipe() async {
    final next = JournalState.empty(state.wipes + 1)..owner = state.owner;
    await commit(
      next,
      const JournalDelta(
        source: AppliedSource.wipe,
        wiped: true,
        messages: [],
        conversations: [],
        users: [],
        counted: [],
        countedFrom: 0,
        cursor: null,
      ),
    );
    state = next;
    return const Applied(source: AppliedSource.wipe, cursor: null, wiped: true);
  }

  @override
  int get messageCount => state.messages.length;

  @override
  int get headSeqTotal => state.conversations.values.fold(0, (total, c) => total + c.headSeq);

  @override
  JournalSnapshot snapshot() {
    final s = state;
    final messages = s.messages.values.toList()
      ..sort((a, b) {
        final byConversation = a.conversationId.compareTo(b.conversationId);
        if (byConversation != 0) return byConversation;
        return a.seq != b.seq ? a.seq.compareTo(b.seq) : a.id.compareTo(b.id);
      });
    return JournalSnapshot(
      cursor: s.cursor,
      messages: messages,
      conversations: s.conversations.values.toList()..sort((a, b) => a.id.compareTo(b.id)),
      users: s.users.values.toList()..sort((a, b) => a.id.compareTo(b.id)),
    );
  }

  Future<void> _land(
    JournalState next,
    AppliedSource source,
    List<Message> messages,
    List<Conversation> conversations,
    List<User> users,
  ) async {
    final countedFrom = state.counted.length;
    await commit(
      next,
      JournalDelta(
        source: source,
        wiped: false,
        messages: messages,
        conversations: conversations,
        users: users,
        counted: next.counted.sublist(countedFrom),
        countedFrom: countedFrom,
        cursor: next.cursor,
      ),
    );
    state = next;
  }
}

/// The journal in memory: a commit is the swap `StagedJournal` does after it.
final class MemoryJournal extends StagedJournal {
  MemoryJournal({this.beforeCommit});

  /// Awaited between staging a write and landing it. Production passes nothing.
  /// It exists so a test can hold a write open and watch that nothing it
  /// carries — the page's messages, its cursor, its `Applied` — is observable
  /// before it lands.
  final Future<void> Function(AppliedSource source)? beforeCommit;

  @override
  Future<void> commit(JournalState next, JournalDelta delta) async {
    await beforeCommit?.call(delta.source);
  }
}

/// Holds a message. THE DEDUPE KEY IS THE ID: an id not yet held is counted,
/// and a later record for a held id replaces it without a count — a CANT-92
/// re-emission is exactly that. Returns whether the record was written.
bool _record(JournalState s, Message m, JournalFaults faults) {
  if (faults.dedupeByLogSeq) {
    final cursor = s.cursor;
    if (cursor != null && m.logSeq <= cursor) return false;
    s.messages[m.id] = m;
    s.counted.add(m.id);
    return true;
  }
  if (!s.messages.containsKey(m.id)) s.counted.add(m.id);
  s.messages[m.id] = m;
  return true;
}

/// Holds served conversations: a later record replaces an earlier one, by id.
/// Returns the records written, which is every one of them in a correct client.
List<Conversation> _hold(JournalState s, List<Conversation> served, JournalFaults faults) {
  final written = <Conversation>[];
  for (final c in served) {
    if (faults.keepHeldConversation && s.conversations.containsKey(c.id)) continue;
    s.conversations[c.id] = c;
    written.add(c);
  }
  return written;
}
