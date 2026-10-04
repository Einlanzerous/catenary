/// Fixtures, in the shape of web/src/transport/test/harness.ts so a ported
/// test reads like the one it was ported from.
library;

import 'dart:convert';
import 'dart:io';

import 'package:catenary_client/catenary_client.dart';
import 'package:catenary_wire/catenary_wire.dart';
import 'package:test/test.dart';

String uuid(int n) => '00000000-0000-4000-8000-${n.toRadixString(16).padLeft(12, '0')}';

const at = '2026-09-29T12:00:00.000Z';

final me = uuid(1);
final other = uuid(2);
final conv = uuid(100);

User user(String id) => User(id: id, name: 'User ${id.substring(id.length - 4)}');

Conversation conversation(String id, {int headSeq = 0}) => Conversation(
      id: id,
      kind: ConversationKind.group,
      name: 'Room ${id.substring(id.length - 4)}',
      memberCount: 2,
      headSeq: headSeq,
    );

Message message(int n, {String? id, String? conversationId, int? seq, int? readBy, DeliveryState state = DeliveryState.sent}) => Message(
      id: id ?? uuid(1000 + n),
      seq: seq ?? n,
      logSeq: n,
      conversationId: conversationId ?? conv,
      authorId: other,
      at: at,
      state: state,
      text: 'message $n',
      readBy: readBy,
    );

SyncResponse page(int logSeq, {List<Message> messages = const [], List<Conversation> conversations = const [], List<User> users = const []}) =>
    SyncResponse(logSeq: logSeq, messages: messages, conversations: conversations, users: users, hasMore: false, serverTime: at);

/// A first page that introduces `conv`, `me` and `other`.
SyncResponse bootstrapPage([int logSeq = 0, List<Message> messages = const []]) =>
    page(logSeq, messages: messages, conversations: [conversation(conv)], users: [user(me), user(other)]);

/// A snapshot as the JSON its records encode to, which is what two journals
/// holding the same thing agree on.
Map<String, Object?> wireShape(JournalSnapshot s) => {
      'cursor': s.cursor,
      'messages': [for (final m in s.messages) jsonEncode(m.toJson())],
      'conversations': [for (final c in s.conversations) jsonEncode(c.toJson())],
      'users': [for (final u in s.users) jsonEncode(u.toJson())],
    };

/// A fresh directory for one test's database files, removed when it ends.
String tempDir() {
  final dir = Directory.systemTemp.createTempSync('catenary-client-test-');
  addTearDown(() => dir.deleteSync(recursive: true));
  return dir.path;
}
