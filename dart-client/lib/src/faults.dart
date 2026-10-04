/// Faults — mirrors `Faults` in web/src/transport/faults.ts, switch for switch,
/// which mirrors internal/client/client.go.
///
/// Each field deliberately breaks one rule. They exist so that an assertion
/// claiming zero loss, zero duplication or a bound can be shown to fail against
/// a client that deserves it: a measurement nobody has seen say "not zero" is a
/// claim about the instrument, not about the system. The all-false value is a
/// correct client. Never set outside a test or a rig proving that an assertion
/// can fail.
///
/// The names are the TypeScript names, because the lanes set them by name.
library;

/// The journal's two switches, which is all a `Journal` implementation reads.
abstract interface class JournalFaults {
  /// Obligation 2: a live `message` frame moves the cursor to its log_seq.
  bool get cursorOnLiveFrames;

  /// Obligation 2's dedupe rule: a record counts as new exactly when its
  /// log_seq is above the cursor, whatever its id. Counts a message twice when
  /// a page re-carries one that arrived live, and drops a CANT-92 re-emission.
  bool get dedupeByLogSeq;
}

final class Faults implements JournalFaults {
  const Faults({
    this.cursorOnLiveFrames = false,
    this.dedupeByLogSeq = false,
    this.endCatchUpEarly = false,
    this.skipWipe = false,
    this.ignoreRetrigger = false,
    this.skipStaleCatchUp = false,
    this.refreshUnlocked = false,
    this.noChain = false,
    this.proposeAfresh = false,
    this.unbounded = false,
    this.presentRefusedToken = false,
    this.neverPresentRefusedToken = false,
    this.neverTerminal = false,
    this.alwaysTerminal = false,
  });

  /// A correct client.
  static const none = Faults();

  @override
  final bool cursorOnLiveFrames;

  @override
  final bool dedupeByLogSeq;

  /// Obligation 3: catch-up ends on the first has_more:false page, even one
  /// requested before the latest trigger.
  final bool endCatchUpEarly;

  /// Obligation 4: on `ready.log_seq` below the cursor, re-sync from 0 and keep
  /// the store and the (monotonic) cursor.
  final bool skipWipe;

  /// CANT-103 rule 3, Go's `IgnoreRetrigger` (CANT-171): a trigger that
  /// arrives while a catch-up is running is dropped instead of re-arming the
  /// end condition, so a catch-up that ends on an in-flight page bounded below
  /// the triggering message never returns it.
  final bool ignoreRetrigger;

  /// CANT-175. `SqliteJournal`'s stale refusal (sqlite_journal.dart) is
  /// `IdbJournal`'s, and has no Go counterpart: the Go reference has one
  /// writer. A live journal write refused with `JournalStale` does not pull a
  /// catch-up, so the refused record sits above the stored cursor, unseen,
  /// until some other trigger happens to come along.
  final bool skipStaleCatchUp;

  // CANT-31 §2, §3 and §5, and CANT-127/129. Declared here so the switch list
  // is the reference's whole list; the credential layer is what reads them.
  final bool refreshUnlocked;
  final bool noChain;
  final bool proposeAfresh;
  final bool unbounded;
  final bool presentRefusedToken;
  final bool neverPresentRefusedToken;

  /// CANT-31 §4's two negative controls, one in each direction: every close
  /// reconnects, or any session that ends, ends the client.
  final bool neverTerminal;
  final bool alwaysTerminal;
}
