// A message's status as the thread words it (deliberate call 03: status as
// words, not tick glyphs). The twin of web/src/components/StatusLabel.vue's
// `text`, kept out of the widget for the reason the typing rule is: it is a
// rule two clients have to agree on.
//
// TWO VOCABULARIES MEET HERE and stay apart. `sent`, `delivered` and `read`
// are the wire's DeliveryState. `queued`, `sending` and `failed` are the
// outbox's own — a message's relationship to its outbox, which the server has
// no opinion on and the wire cannot express.

enum MessageStatus { queued, sending, sent, delivered, read, failed }

/// The label for one message's status.
///
/// THE READ FRACTION'S NUMERATOR IS CLAMPED TO THE ROOM — the wire's rule on
/// `Message.read_by` (CANT-140 ruling 2), implemented once per client. The two
/// halves arrive on two records with two freshnesses: `member_count` rides
/// every page on which the room's membership changed, while a held message is
/// never re-served by a catch-up, so after an offboard this can be handed
/// `readBy: 7` beside `memberCount: 6`. `READ 7/6` is a number no serve ever
/// said. The rows this is checked against are
/// server/spec/testdata/read-fraction.json, shared with the Go harness and the
/// web client, so the three cannot drift.
///
/// A two-member room renders bare `READ`: there the fraction says nothing the
/// word does not.
String statusLabel(MessageStatus status, {int? readBy, int memberCount = 0, bool retrying = false}) {
  switch (status) {
    // Ruling 3 C: a send held under backoff past its third retryable refusal
    // says so, rather than looking like any other queued row.
    case MessageStatus.sending:
      return retrying ? 'RETRYING' : 'SENDING';
    case MessageStatus.queued:
      return retrying ? 'RETRYING' : 'QUEUED';
    case MessageStatus.sent:
      return 'SENT';
    case MessageStatus.delivered:
      return 'DELIVERED';
    case MessageStatus.failed:
      return 'FAILED';
    case MessageStatus.read:
      if (memberCount > 2 && readBy != null && readBy > 0) {
        return 'READ ${readBy < memberCount ? readBy : memberCount}/$memberCount';
      }
      return 'READ';
  }
}

/// `m:ss`, as the web client's `duration` words a length of time.
String clock(Duration d) {
  final s = d.inMilliseconds <= 0 ? 0 : (d.inMilliseconds / 1000).round();
  return '${s ~/ 60}:${(s % 60).toString().padLeft(2, '0')}';
}
