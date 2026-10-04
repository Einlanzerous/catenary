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
/// THE NAMES ARE THE TYPESCRIPT NAMES, because the lanes set them by name, and
/// a test reads faults.ts and fails when `Faults.names` differs from it.
library;

/// The journal's three switches, which is all a `Journal` implementation reads.
abstract interface class JournalFaults {
  /// Obligation 2: a live `message` frame moves the cursor to its log_seq.
  bool get cursorOnLiveFrames;

  /// Obligation 2's dedupe rule: a record counts as new exactly when its
  /// log_seq is above the cursor, whatever its id. Counts a message twice when
  /// a page re-carries one that arrived live, and drops a CANT-92 re-emission.
  bool get dedupeByLogSeq;

  /// CANT-46, Go's `KeepHeldConversation`: a `Conversation` served for a
  /// conversation the journal already holds is dropped and the held record
  /// stays, on a page and on a live frame alike. The client keeps the
  /// `firstUnreadSeq` and `headSeq` it had and is still clean under
  /// `client.Compare` — the control for the convergence rig's `SameState`.
  bool get keepHeldConversation;
}

final class Faults implements JournalFaults {
  const Faults({
    this.cursorOnLiveFrames = false,
    this.dedupeByLogSeq = false,
    this.endCatchUpEarly = false,
    this.skipWipe = false,
    this.ignoreRetrigger = false,
    this.skipStaleCatchUp = false,
    this.keepHeldConversation = false,
    this.refreshUnlocked = false,
    this.noChain = false,
    this.proposeAfresh = false,
    this.unbounded = false,
    this.presentRefusedToken = false,
    this.neverPresentRefusedToken = false,
    this.neverTerminal = false,
    this.alwaysTerminal = false,
  });

  /// The switches named in [set], which is how a driver is told them. A name
  /// this list does not know is refused: a fault that silently did nothing
  /// would make its lane's control pass for the wrong reason.
  factory Faults.named(Iterable<String> set) {
    final on = set.toSet();
    final unknown = on.difference(names.toSet());
    if (unknown.isNotEmpty) throw ArgumentError('faults: unknown switch ${unknown.join(', ')}');
    return Faults(
      cursorOnLiveFrames: on.contains('cursorOnLiveFrames'),
      dedupeByLogSeq: on.contains('dedupeByLogSeq'),
      endCatchUpEarly: on.contains('endCatchUpEarly'),
      skipWipe: on.contains('skipWipe'),
      ignoreRetrigger: on.contains('ignoreRetrigger'),
      skipStaleCatchUp: on.contains('skipStaleCatchUp'),
      keepHeldConversation: on.contains('keepHeldConversation'),
      refreshUnlocked: on.contains('refreshUnlocked'),
      noChain: on.contains('noChain'),
      proposeAfresh: on.contains('proposeAfresh'),
      unbounded: on.contains('unbounded'),
      presentRefusedToken: on.contains('presentRefusedToken'),
      neverPresentRefusedToken: on.contains('neverPresentRefusedToken'),
      neverTerminal: on.contains('neverTerminal'),
      alwaysTerminal: on.contains('alwaysTerminal'),
    );
  }

  /// A correct client.
  static const none = Faults();

  /// Every switch, in faults.ts's order.
  static const names = [
    'cursorOnLiveFrames',
    'dedupeByLogSeq',
    'endCatchUpEarly',
    'skipWipe',
    'ignoreRetrigger',
    'skipStaleCatchUp',
    'refreshUnlocked',
    'noChain',
    'proposeAfresh',
    'unbounded',
    'presentRefusedToken',
    'neverPresentRefusedToken',
    'neverTerminal',
    'alwaysTerminal',
    'keepHeldConversation',
  ];

  /// Each switch by name, which is what a status or a log line prints.
  Map<String, bool> toMap() => {
        'cursorOnLiveFrames': cursorOnLiveFrames,
        'dedupeByLogSeq': dedupeByLogSeq,
        'endCatchUpEarly': endCatchUpEarly,
        'skipWipe': skipWipe,
        'ignoreRetrigger': ignoreRetrigger,
        'skipStaleCatchUp': skipStaleCatchUp,
        'refreshUnlocked': refreshUnlocked,
        'noChain': noChain,
        'proposeAfresh': proposeAfresh,
        'unbounded': unbounded,
        'presentRefusedToken': presentRefusedToken,
        'neverPresentRefusedToken': neverPresentRefusedToken,
        'neverTerminal': neverTerminal,
        'alwaysTerminal': alwaysTerminal,
        'keepHeldConversation': keepHeldConversation,
      };

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

  @override
  final bool keepHeldConversation;

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
