/* Faults — mirrors `Faults` in internal/client/client.go, switch for switch.
 *
 * Each field deliberately breaks one rule. They exist so that an assertion
 * claiming zero loss, zero duplication or a bound can be shown to fail against
 * a client that deserves it: a measurement nobody has seen say "not zero" is a
 * claim about the instrument, not about the system. The all-false value is a
 * correct client. Never set outside a test or a rig proving that an assertion
 * can fail.
 *
 * Go's two injection hooks, `HelloInstead` and `AfterReady`, are not mirrored:
 * they exist so the Go tests can make a REAL server produce each close CANT-31
 * §4 lists, and the TypeScript tests produce those closes from a fake server
 * directly (CANT-35's plan, *Approach*).
 */

export interface Faults {
  /** Obligation 2: a live `message` frame moves the cursor to its log_seq. */
  cursorOnLiveFrames: boolean
  /** Obligation 2's dedupe rule: a record counts as new exactly when its
   *  log_seq is above the cursor, whatever its id. Counts a message twice when
   *  a page re-carries one that arrived live, and drops a CANT-92 re-emission. */
  dedupeByLogSeq: boolean
  /** Obligation 3: catch-up ends on the first has_more:false page, even one
   *  requested before the latest trigger. */
  endCatchUpEarly: boolean
  /** Obligation 4: on `ready.log_seq` below the cursor, re-sync from 0 and keep
   *  the store and the (monotonic) cursor. */
  skipWipe: boolean
  /** CANT-103 rule 3, Go's `IgnoreRetrigger` (CANT-171): a trigger that
   *  arrives while a catch-up is running is dropped instead of re-arming the
   *  end condition, so a catch-up that ends on an in-flight page bounded below
   *  the triggering message never returns it. */
  ignoreRetrigger: boolean
  /** CANT-175, TS-only: `IdbJournal`'s stale refusal (idb-journal.ts) has no
   *  Go counterpart, so this is not a Go switch either. A live journal write
   *  refused with `JournalStale` does not pull a catch-up, so the refused
   *  record sits above the stored cursor, unseen, until some other trigger
   *  happens to come along. */
  skipStaleCatchUp: boolean
  /** CANT-199, and like `skipStaleCatchUp` not a Go switch: a journal write
   *  refused with `JournalStale` leaves the epoch where it was, so a `/sync`
   *  page requested before another tab's wipe still lands when it arrives —
   *  on the reloaded, empty store — and moves the cursor above messages the
   *  store no longer holds. */
  staleKeepsEpoch: boolean

  /* CANT-31 §2, §3 and §5, and CANT-127/129. Declared here so the switch list
   * is Go's whole list; the credential layer (CANT-152) is what reads them. */
  refreshUnlocked: boolean
  noChain: boolean
  proposeAfresh: boolean
  unbounded: boolean
  presentRefusedToken: boolean
  neverPresentRefusedToken: boolean

  /** CANT-31 §4's two negative controls, one in each direction: every close
   *  reconnects, or any session that ends, ends the client. */
  neverTerminal: boolean
  alwaysTerminal: boolean

  /** CANT-46, Go's `KeepHeldConversation`: a `Conversation` served for a
   *  conversation the journal already holds is dropped and the held record
   *  stays, on a page and on a live frame alike. The client keeps the
   *  `firstUnreadSeq` and `headSeq` it had and is still clean under
   *  `client.Compare` — the control for the convergence rig's `SameState`. */
  keepHeldConversation: boolean
}

export const NO_FAULTS: Readonly<Faults> = Object.freeze({
  cursorOnLiveFrames: false,
  dedupeByLogSeq: false,
  endCatchUpEarly: false,
  skipWipe: false,
  ignoreRetrigger: false,
  skipStaleCatchUp: false,
  staleKeepsEpoch: false,
  refreshUnlocked: false,
  noChain: false,
  proposeAfresh: false,
  unbounded: false,
  presentRefusedToken: false,
  neverPresentRefusedToken: false,
  neverTerminal: false,
  alwaysTerminal: false,
  keepHeldConversation: false,
})

/** The journal's three switches, which is all a `Journal` implementation reads. */
export type JournalFaults = Pick<Faults, 'cursorOnLiveFrames' | 'dedupeByLogSeq' | 'keepHeldConversation'>
