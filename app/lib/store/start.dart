// Starting a conversation (CANT-253's app half, CANT-273): what the roster
// offers and what a start came to, as the widgets read them. The REST calls
// are `catenary_client`'s `ConversationsApi`; this file is the vocabulary the
// store hands back so nothing outside lib/store/ imports that package.

import 'dart:math' as math;

import 'package:flutter/foundation.dart';

import 'conversation.dart' as conv;

/// One person the roster offers: `GET /users` lists active persons other than
/// you, with the handle a start names them by.
@immutable
class RosterPerson {
  const RosterPerson({required this.id, required this.name, required this.handle, this.initials});

  final String id;
  final String name;
  final String handle;

  /// The server's two letters when it sent them.
  final String? initials;

  /// The avatar tile's letters: the server's, derived from the name otherwise.
  String get tile => initials ?? conv.initials(name);
}

/// What a start call came to.
sealed class StartOutcome {
  const StartOutcome();
}

/// The server made, or found, the conversation. [held] is whether this device's
/// journal already has it: it arrives through `/sync` like every other, and a
/// slow catch-up leaves it false, in which case the rail gets it a moment
/// later and nothing is opened.
final class StartDone extends StartOutcome {
  const StartDone(this.conversationId, {required this.held});

  final String conversationId;
  final bool held;
}

/// The server answered and said no. [code] is the wire's, when it sent one:
/// `conversation_not_found` is a handle that names nobody active.
final class StartRefusedBy extends StartOutcome {
  const StartRefusedBy(this.code);

  final String? code;
}

/// Nothing usable came back. Nothing is known about whether it was made, and
/// asking again is safe: a direct is find-or-create, and a group carries its
/// `request_id`.
final class StartUnreachableNow extends StartOutcome {
  const StartUnreachableNow();
}

/// A version-4 UUID, for a group's `request_id`. One per form, so a retry of
/// the same form is a replay and not a second room.
String newRequestId([math.Random? random]) {
  final r = random ?? math.Random.secure();
  final b = List<int>.generate(16, (_) => r.nextInt(256));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  String h(int from, int to) => b.sublist(from, to).map((x) => x.toRadixString(16).padLeft(2, '0')).join();
  return '${h(0, 4)}-${h(4, 6)}-${h(6, 8)}-${h(8, 10)}-${h(10, 16)}';
}
