/// The transport's status. Mirrors web/src/transport/status.ts, which mirrors
/// `Stats` and `Status` in internal/client/client.go.
///
/// STATS USE GO'S FIELD NAMES, camelCased, so a soak report reads the same for
/// every cohort and soakrig decodes one into `client.Status` field by field:
/// `toJson()` here is the JSON the TypeScript status serializes to. The one
/// rename is `LastRTT`, a Go duration, which is `lastRttMs`.
///
/// What the banner reads from this (the reference's `connectionInfo`) is the
/// app's to derive, and is not here.
///
/// NO TOKEN APPEARS IN ANY FIELD, and a test scans for one.
library;

import 'credential.dart';
import 'terminal.dart';

final class Stats {
  Stats();

  Stats.copy(Stats s)
      : dials = s.dials,
        dialErrors = s.dialErrors,
        readys = s.readys,
        resyncs = s.resyncs,
        discards = s.discards,
        liveFrames = s.liveFrames,
        pages = s.pages,
        syncErrors = s.syncErrors,
        syncsBeforeReady = s.syncsBeforeReady,
        pingsSent = s.pingsSent,
        pongsReceived = s.pongsReceived,
        heartbeatSevers = s.heartbeatSevers,
        refreshes = s.refreshes,
        refreshesSkipped = s.refreshesSkipped,
        refreshErrors = s.refreshErrors,
        refreshWalkBacks = s.refreshWalkBacks,
        chainLength = s.chainLength,
        refreshesHeldUnreachable = s.refreshesHeldUnreachable,
        refreshesHeldBackoff = s.refreshesHeldBackoff,
        dialsWithheld = s.dialsWithheld,
        syncsWithheld = s.syncsWithheld,
        undecodable = s.undecodable,
        lastRttMs = s.lastRttMs,
        lastClose = s.lastClose,
        closeStatuses = Map.of(s.closeStatuses),
        introductionDiscards = s.introductionDiscards;

  /// Connection attempts.
  int dials = 0;

  /// Dials whose socket never opened. Not also counted in `closeStatuses`.
  int dialErrors = 0;

  /// `ready` frames received — sessions established.
  int readys = 0;

  /// `resync_required` frames received.
  int resyncs = 0;

  /// `ready.log_seq` below the cursor (obligation 4).
  int discards = 0;

  /// `message` frames applied.
  int liveFrames = 0;

  /// `/sync` pages applied or dropped.
  int pages = 0;
  int syncErrors = 0;

  /// `/sync` requests issued while a dial had not yet received `ready`.
  int syncsBeforeReady = 0;
  int pingsSent = 0;
  int pongsReceived = 0;

  /// Sockets this client severed for unanswered pings.
  int heartbeatSevers = 0;

  // The credential layer's; zero while the credential is held.
  int refreshes = 0;
  int refreshesSkipped = 0;
  int refreshErrors = 0;
  int refreshWalkBacks = 0;
  int chainLength = 0;
  int refreshesHeldUnreachable = 0;
  int refreshesHeldBackoff = 0;

  /// Requests CANT-129's refused wait did not make — one per poll — and the
  /// triggers it suppressed. `dialsWithheld` ALSO counts wake signals the early
  /// dial's once-per-interval limit suppressed. Counted, never logged per
  /// attempt.
  int dialsWithheld = 0;
  int syncsWithheld = 0;

  /// Message events that were not a frame: one the generated decoder refuses,
  /// text that is not JSON, or a binary message.
  int undecodable = 0;
  int lastRttMs = 0;
  String lastClose = '';

  /// How each session this client HELD ended, keyed by the close code the peer
  /// sent, or -1 for none. A dial that never opened is `dialErrors`, not here.
  Map<int, int> closeStatuses = {};

  /// CANT-103 rule 1: `message` frames discarded for naming a conversation or
  /// an author the journal does not hold.
  int introductionDiscards = 0;

  Map<String, Object?> toJson() => {
        'dials': dials,
        'dialErrors': dialErrors,
        'readys': readys,
        'resyncs': resyncs,
        'discards': discards,
        'liveFrames': liveFrames,
        'pages': pages,
        'syncErrors': syncErrors,
        'syncsBeforeReady': syncsBeforeReady,
        'pingsSent': pingsSent,
        'pongsReceived': pongsReceived,
        'heartbeatSevers': heartbeatSevers,
        'refreshes': refreshes,
        'refreshesSkipped': refreshesSkipped,
        'refreshErrors': refreshErrors,
        'refreshWalkBacks': refreshWalkBacks,
        'chainLength': chainLength,
        'refreshesHeldUnreachable': refreshesHeldUnreachable,
        'refreshesHeldBackoff': refreshesHeldBackoff,
        'dialsWithheld': dialsWithheld,
        'syncsWithheld': syncsWithheld,
        'undecodable': undecodable,
        'lastRttMs': lastRttMs,
        'lastClose': lastClose,
        'closeStatuses': {for (final e in closeStatuses.entries) '${e.key}': e.value},
        'introductionDiscards': introductionDiscards,
      };
}

/// A journal write that did not land, by the error's type and message.
final class JournalError {
  const JournalError(this.name, this.message);

  final String name;
  final String message;

  Map<String, Object?> toJson() => {'name': name, 'message': message};
}

final class TransportStatus {
  const TransportStatus({
    required this.terminal,
    required this.refreshHold,
    required this.tokenRefused,
    required this.nextRefreshAt,
    required this.connected,
    required this.ready,
    required this.sessionId,
    required this.heartbeatIntervalSec,
    required this.missedPongLimit,
    required this.caughtUp,
    required this.cursor,
    required this.attempt,
    required this.nextDialAt,
    required this.stats,
    required this.messages,
    required this.wipes,
    required this.headSeqTotal,
    required this.journalError,
  });

  final Terminal terminal;
  final RefreshHold refreshHold;
  final bool tokenRefused;

  /// Wall-clock ms, or null.
  final num? nextRefreshAt;

  /// A socket is open and the hello is on it.
  final bool connected;

  /// And it has received `ready`.
  final bool ready;
  final String? sessionId;
  final int? heartbeatIntervalSec;
  final int? missedPongLimit;

  /// No trigger is outstanding: the last catch-up ended on a page requested
  /// after the most recent trigger.
  final bool caughtUp;
  final int? cursor;

  /// Dials since the ramp last reset. The banner's "attempt N".
  final int attempt;

  /// When the pending dial goes out, wall-clock ms; null when none is pending.
  final num? nextDialAt;
  final Stats stats;

  /// Go's `Status.Messages` and `Status.Wipes`, which the soak's predicates
  /// read: how many messages the journal holds, and how many wipes it has been
  /// through in this transport's life.
  final int messages;
  final int wipes;

  /// `journal.headSeqTotal`: with `messages`, the resync progress fraction
  /// (CANT-37) — `messages` toward `headSeqTotal`, never a spinner.
  final int headSeqTotal;

  /// The last journal write's failure, null once one lands again. The journal
  /// does not fall back to memory; it says it could not write.
  final JournalError? journalError;

  Map<String, Object?> toJson() => {
        'terminal': terminal.toJson(),
        'refreshHold': refreshHold.name,
        'tokenRefused': tokenRefused,
        'nextRefreshAt': nextRefreshAt,
        'connected': connected,
        'ready': ready,
        'sessionId': sessionId,
        'heartbeatIntervalSec': heartbeatIntervalSec,
        'missedPongLimit': missedPongLimit,
        'caughtUp': caughtUp,
        'cursor': cursor,
        'attempt': attempt,
        'nextDialAt': nextDialAt,
        'stats': stats.toJson(),
        'messages': messages,
        'wipes': wipes,
        'headSeqTotal': headSeqTotal,
        'journalError': journalError?.toJson(),
      };
}
