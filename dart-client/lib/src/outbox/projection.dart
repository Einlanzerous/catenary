/// The render projection — mirrors web/src/outbox/projection.ts: an outbox
/// item as a row would show it. Nothing here renders; a widget that does takes
/// this.
///
/// An acked entry is placed by the SERVER's ack — its conversationId, seq and
/// at — never by its own conversationId or composedAt (the server builds the
/// ack from what was stored, and dedup is per author, not per conversation).
/// An unacked one carries no seq at all and belongs in the tail by `order`.
library;

import 'package:catenary_wire/catenary_wire.dart';

import 'outbox.dart';
import 'types.dart';

String? errorText(OutboxError? e) => switch (e) {
      null => null,
      ServerRefusal(:final message) || UploadFailure(:final message) => message,
      Bare1008() => bare1008Message,
    };

/// An unsettled entry, as it is shown.
final class OutboxMessage {
  const OutboxMessage({
    required this.clientId,
    required this.conversationId,
    this.seq,
    required this.authorId,
    required this.at,
    this.text,
    this.replyTo,
    required this.state,
    this.error,
    required this.retrying,
  });

  final Uuid clientId;
  final Uuid conversationId;

  /// The ack's; absent until there is one.
  final int? seq;
  final Uuid authorId;

  /// The ack's time once acked; the device's `composedAt` until then.
  final String at;
  final String? text;
  final ReplyRef? replyTo;
  final OutboxState state;

  /// The inline error, on a `failed` entry.
  final String? error;
  final bool retrying;
}

OutboxMessage project(OutboxItem item) {
  final entry = item.entry;
  final ack = item.ack;
  return OutboxMessage(
    clientId: entry.clientId,
    conversationId: ack?.conversationId ?? entry.conversationId,
    seq: ack?.seq,
    authorId: entry.accountId,
    at: ack?.at ?? entry.composedAt,
    text: entry.text,
    replyTo: entry.replyPreview,
    state: item.state,
    error: item.state == OutboxState.failed ? errorText(entry.lastError) : null,
    retrying: item.retrying,
  );
}
