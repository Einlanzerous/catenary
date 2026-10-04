/* The journal — mirrors internal/client/journal.go (its message store; the
 * credential half is credential.ts).
 *
 * TWO IMPLEMENTATIONS OF ONE INTERFACE, AND ONE COPY OF THE RULES. `Journal` is
 * what the transport calls; `StagedJournal` holds the rules every
 * implementation shares — dedupe by id, the cursor forward only and on a page
 * only, the faults that break each — and stages every write on a copy of its
 * state. What differs is `commit`, the one step in which a staged write lands:
 *
 *   - `MemoryJournal` (CANT-151) swaps the copy in. A page load bootstraps
 *     from 0 (CANT-35 ruling 2 → B), and rigs and tests use it.
 *   - `IdbJournal` (CANT-169, idb-journal.ts) writes the page to CANT-35's
 *     `catenary` IndexedDB database in one transaction and swaps the copy in
 *     only after `oncomplete`, so a page load resumes from the stored cursor.
 *
 * OBLIGATION 1 — PERSIST BEFORE RENDER. A page's messages, conversations,
 * users and cursor are staged and then land in one step, and nothing is
 * observable (`snapshot()`, the `Applied` the transport emits) until that step
 * has completed. In memory that is the ordering within one apply (CANT-35
 * criterion 25); over IndexedDB it is also the durable half (criterion 32),
 * because the step is the transaction.
 *
 * Writes are serialized by the transport, one at a time, so an implementation
 * never sees two in flight FROM ONE TRANSPORT. Two tabs are two transports over
 * one IndexedDB database, and `IdbJournal` guards that case itself: a wipe in
 * one tab makes the other's next write refuse and reload (idb-journal.ts).
 */

import type { Conversation, Message, ServerReceipt, SyncResponse, User, Uuid } from '@/wire/generated'
import type { JournalFaults } from './faults'

/**
 * Delivered through `Transport.onApply` once per journal write, and only after
 * that write — the cursor included — has completed (CANT-24 obligation 1).
 * Records are upserted by id; a later record replaces an earlier one.
 */
export interface Applied {
  source: 'page' | 'live' | 'wipe'
  /** The journal's cursor after this write; unchanged by 'live'. */
  cursor: number | null
  messages: Message[]
  conversations: Conversation[]
  users: User[]
  /** Live receipts as received. Under CANT-35 ruling 4 → B they have no store
   *  effect: an own-user receipt pulls a catch-up, another user's is a no-op. */
  receipts: ServerReceipt[]
  /** Obligation 4: everything held before this write was discarded. */
  wiped: boolean
}

/** The whole journal at a point in time, wire-shaped so soakrig can decode it
 *  into `client.Snapshot`. */
export interface JournalSnapshot {
  cursor: number | null
  /** Ordered by (conversation_id, seq), then id. */
  messages: Message[]
  /** By id. */
  conversations: Conversation[]
  /** By id. */
  users: User[]
}

/** What one live frame writes. */
export interface LiveWrite {
  messages?: Message[]
  conversations?: Conversation[]
  users?: User[]
}

export interface Journal {
  /** The committed cursor; null when none is held. */
  cursor(): number | null
  holdsConversation(id: Uuid): boolean
  holdsUser(id: Uuid): boolean
  /** Obligations 1 and 2: messages, conversations, users, then the cursor —
   *  which only ever moves forward — in one write. */
  applyPage(page: SyncResponse, faults: JournalFaults): Promise<Applied>
  /** A live frame, applied by id. Moves no cursor (unless the fault says so). */
  applyLive(write: LiveWrite, faults: JournalFaults): Promise<Applied>
  /** Obligation 4: messages, conversations, users and the cursor. Never the
   *  credential, which is not in here at all. */
  wipe(): Promise<Applied>
  snapshot(): JournalSnapshot
  /** How many messages are held; `snapshot().messages.length` without the copy. */
  messageCount(): number
  /** Sum of `head_seq` over every conversation currently held — the resync
   *  progress bar's target (CANT-37). A conversation not yet touched by this
   *  catch-up isn't counted, so the total grows as one is discovered, same as
   *  `messageCount()` does; every message held belongs to a held conversation
   *  (CANT-103 rule 4 discards anything that doesn't), so the two are always
   *  comparable without a second pass over `messages`. */
  headSeqTotal(): number
}

/** What a journal holds, in memory; replaced wholesale on commit. */
export interface JournalState {
  cursor: number | null
  messages: Map<Uuid, Message>
  conversations: Map<Uuid, Conversation>
  users: Map<Uuid, User>
  /** R1's evidence log since the last wipe; see `StagedJournal.counted()`. */
  counted: Uuid[]
  wipes: number
}

/**
 * What one staged write changes, for an implementation that writes deltas
 * rather than whole states: the records it upserts, the ids it newly counted,
 * and the cursor and wipe count after it. `wiped` says everything held before
 * it goes first.
 */
export interface JournalDelta {
  source: Applied['source']
  wiped: boolean
  messages: Message[]
  conversations: Conversation[]
  users: User[]
  /** Appended to the evidence log, in order, after `countedFrom` entries. */
  counted: Uuid[]
  countedFrom: number
  cursor: number | null
  wipes: number
}

/**
 * The rules, once. Every write is staged on a copy of the state and handed to
 * `commit` with the delta it amounts to; `commit` lands it, and only then does
 * the state (and so `snapshot()`, `cursor()` and the rest) show it. A `commit`
 * that rejects leaves the state as it was, and the write's promise rejects.
 */
export abstract class StagedJournal implements Journal {
  protected s: JournalState

  protected constructor(initial: JournalState = emptyState(0)) {
    this.s = initial
  }

  /** THE ONE STEP IN WHICH A WRITE LANDS. Resolves once it has; everything
   *  before it is staged on a copy nobody else can see. */
  protected abstract commit(next: JournalState, delta: JournalDelta): Promise<void>

  cursor(): number | null {
    return this.s.cursor
  }

  holdsConversation(id: Uuid): boolean {
    return this.s.conversations.has(id)
  }

  holdsUser(id: Uuid): boolean {
    return this.s.users.has(id)
  }

  /**
   * R1's evidence log, which `client.Compare` reads: every message id in the
   * order it was first counted since the last wipe. A correct client counts
   * each id exactly once, and `Duplicated` is an id counted twice. Not part of
   * `JournalSnapshot`, whose shape is the contract CANT-36 compiles against; a
   * rig that holds the journal reads it here.
   */
  counted(): Uuid[] {
    return [...this.s.counted]
  }

  /** How many discard-and-bootstraps this journal has been through. */
  wipes(): number {
    return this.s.wipes
  }

  async applyPage(page: SyncResponse, faults: JournalFaults): Promise<Applied> {
    const next = cloneState(this.s)
    const messages: Message[] = []
    for (const m of page.messages) if (record(next, m, faults)) messages.push(m)
    const conversations = hold(next, page.conversations, faults)
    for (const u of page.users) next.users.set(u.id, u)
    if (next.cursor === null || page.logSeq > next.cursor) next.cursor = page.logSeq
    await this.land(next, 'page', messages, conversations, page.users)
    return {
      source: 'page', cursor: next.cursor, messages,
      conversations, users: [...page.users], receipts: [], wiped: false,
    }
  }

  async applyLive(write: LiveWrite, faults: JournalFaults): Promise<Applied> {
    const next = cloneState(this.s)
    const messages: Message[] = []
    for (const m of write.messages ?? []) {
      if (!record(next, m, faults)) continue
      messages.push(m)
      if (faults.cursorOnLiveFrames && (next.cursor === null || m.logSeq > next.cursor)) next.cursor = m.logSeq
    }
    const conversations = hold(next, write.conversations ?? [], faults)
    for (const u of write.users ?? []) next.users.set(u.id, u)
    await this.land(next, 'live', messages, conversations, write.users ?? [])
    return {
      source: 'live', cursor: next.cursor, messages,
      conversations, users: [...(write.users ?? [])], receipts: [], wiped: false,
    }
  }

  async wipe(): Promise<Applied> {
    const next = emptyState(this.s.wipes + 1)
    await this.commit(next, {
      source: 'wipe', wiped: true, messages: [], conversations: [], users: [],
      counted: [], countedFrom: 0, cursor: null, wipes: next.wipes,
    })
    this.s = next
    return { source: 'wipe', cursor: null, messages: [], conversations: [], users: [], receipts: [], wiped: true }
  }

  messageCount(): number {
    return this.s.messages.size
  }

  headSeqTotal(): number {
    let total = 0
    for (const c of this.s.conversations.values()) total += c.headSeq
    return total
  }

  snapshot(): JournalSnapshot {
    const s = this.s
    const messages = [...s.messages.values()].sort((a, b) =>
      a.conversationId !== b.conversationId
        ? cmp(a.conversationId, b.conversationId)
        : a.seq !== b.seq
          ? a.seq - b.seq
          : cmp(a.id, b.id),
    )
    return {
      cursor: s.cursor,
      messages,
      conversations: [...s.conversations.values()].sort((a, b) => cmp(a.id, b.id)),
      users: [...s.users.values()].sort((a, b) => cmp(a.id, b.id)),
    }
  }

  private async land(
    next: JournalState, source: Applied['source'], messages: Message[], conversations: Conversation[], users: User[],
  ): Promise<void> {
    const countedFrom = this.s.counted.length
    await this.commit(next, {
      source, wiped: false, messages, conversations, users,
      counted: next.counted.slice(countedFrom), countedFrom, cursor: next.cursor, wipes: next.wipes,
    })
    this.s = next
  }
}

export interface MemoryJournalOptions {
  /**
   * Awaited between staging a write and landing it. Production passes nothing.
   * It exists so a test can hold a write open and watch that nothing it carries
   * — the page's messages, its cursor, its `Applied` — is observable before it
   * lands (CANT-35 criterion 25).
   */
  beforeCommit?: (source: Applied['source']) => Promise<void>
}

/** The journal in memory: a commit is the swap `StagedJournal` does after it. */
export class MemoryJournal extends StagedJournal {
  constructor(private readonly opts: MemoryJournalOptions = {}) {
    super()
  }

  protected async commit(_next: JournalState, delta: JournalDelta): Promise<void> {
    if (this.opts.beforeCommit) await this.opts.beforeCommit(delta.source)
  }
}

/**
 * Holds a message. THE DEDUPE KEY IS THE ID: an id not yet held is counted,
 * and a later record for a held id replaces it without a count — a CANT-92
 * re-emission is exactly that. Returns whether the record was written.
 */
function record(s: JournalState, m: Message, faults: JournalFaults): boolean {
  if (faults.dedupeByLogSeq) {
    if (s.cursor !== null && m.logSeq <= s.cursor) return false
    s.messages.set(m.id, m)
    s.counted.push(m.id)
    return true
  }
  if (!s.messages.has(m.id)) s.counted.push(m.id)
  s.messages.set(m.id, m)
  return true
}

/**
 * Holds served conversations: a later record replaces an earlier one, by id.
 * Returns the records written, which is every one of them in a correct client.
 */
function hold(s: JournalState, served: readonly Conversation[], faults: JournalFaults): Conversation[] {
  const written: Conversation[] = []
  for (const c of served) {
    if (faults.keepHeldConversation && s.conversations.has(c.id)) continue
    s.conversations.set(c.id, c)
    written.push(c)
  }
  return written
}

export function emptyState(wipes: number): JournalState {
  return { cursor: null, messages: new Map(), conversations: new Map(), users: new Map(), counted: [], wipes }
}

function cloneState(s: JournalState): JournalState {
  return {
    cursor: s.cursor,
    messages: new Map(s.messages),
    conversations: new Map(s.conversations),
    users: new Map(s.users),
    counted: [...s.counted],
    wipes: s.wipes,
  }
}

const cmp = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)
