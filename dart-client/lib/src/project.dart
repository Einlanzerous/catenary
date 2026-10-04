/// The journal-to-app projection — mirrors web/src/transport/project.ts, which
/// mirrors no Go file: the Go client renders nothing, so it has nothing to
/// project onto.
///
/// Pure functions from what the transport emits (`onApply`, `snapshot()`) to
/// the shapes a store renders from: wire `Message`s, wire `Conversation`s and a
/// `User` map by id. A store folds [projectApplied] over every `Applied`, and
/// the result equals [project] over the final `snapshot()` (CANT-200).
///
/// NO FLUTTER. The result is plain data a `ChangeNotifier` can take.
library;

import 'package:catenary_wire/catenary_wire.dart';

import 'journal.dart';

final class Projection {
  const Projection({this.messages = const [], this.conversations = const [], this.users = const {}});

  /// Ordered by (conversation, seq), then id — the order `snapshot()` holds.
  final List<Message> messages;

  /// By id.
  final List<Conversation> conversations;

  /// By id.
  final Map<Uuid, User> users;
}

const emptyProjection = Projection();

/// The whole journal, projected. A server-held message's `state` is one of the
/// wire's own; the outbox's three are never on a record from here.
Projection project(JournalSnapshot snapshot) => Projection(
  messages: List<Message>.unmodifiable(snapshot.messages.toList()..sort(_byThread)),
  conversations: List<Conversation>.unmodifiable(snapshot.conversations.toList()..sort(_byConversationId)),
  users: Map.unmodifiable({for (final u in snapshot.users) u.id: u}),
);

/// One `Applied`, folded into a projection. Returns a new projection and never
/// mutates [state]. A wipe starts from empty; records upsert by id, and a later
/// record replaces an earlier one. Receipts change nothing here (CANT-35 ruling
/// 4 → B: a receipt's effect arrives on a page).
Projection projectApplied(Projection state, Applied applied) {
  final base = applied.wiped ? emptyProjection : state;
  final messages = {for (final m in base.messages) m.id: m};
  for (final m in applied.messages) {
    messages[m.id] = m;
  }
  final conversations = {for (final c in base.conversations) c.id: c};
  for (final c in applied.conversations) {
    conversations[c.id] = c;
  }
  final users = {...base.users};
  for (final u in applied.users) {
    users[u.id] = u;
  }
  return Projection(
    messages: List.unmodifiable(messages.values.toList()..sort(_byThread)),
    conversations: List.unmodifiable(conversations.values.toList()..sort(_byConversationId)),
    users: Map.unmodifiable(users),
  );
}

int _byConversationId(Conversation a, Conversation b) => a.id.compareTo(b.id);

int _byThread(Message a, Message b) {
  final c = a.conversationId.compareTo(b.conversationId);
  if (c != 0) return c;
  return a.seq != b.seq ? a.seq.compareTo(b.seq) : a.id.compareTo(b.id);
}
