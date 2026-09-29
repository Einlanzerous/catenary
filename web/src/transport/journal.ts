/* The journal — mirrors internal/client/journal.go (its message store; the
 * credential half is credential.ts).
 *
 * CANT-35 RULING 2 → B: THE JOURNAL IS IN MEMORY. The credential is durable
 * (CANT-31 §2 and §3 require it, and CANT-152 builds it); cursor, messages,
 * conversations and users live here, behind the `Journal` interface, so a page
 * load bootstraps from 0. The durable journal is a follow-up ticket, and it
 * replaces `MemoryJournal` without touching a caller.
 *
 * OBLIGATION 1 — PERSIST BEFORE RENDER — IS ORDERING HERE, NOT DURABILITY. A
 * page's messages, conversations, users and cursor are staged and then land in
 * one step, and nothing is observable (`snapshot()`, the `Applied` the
 * transport emits) until that step has completed. In memory that proves the
 * ordering within one apply and nothing about surviving a reload; the durable
 * half belongs to the durable journal's ticket, which is what `journal.go`'s
 * "proving it there is CANT-35's and CANT-42's" now points at.
 *
 * Writes are serialized by the transport, one at a time, so an implementation
 * never sees two in flight.
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

/** What `MemoryJournal` holds; replaced wholesale on commit. */
interface State {
  cursor: number | null
  messages: Map<Uuid, Message>
  conversations: Map<Uuid, Conversation>
  users: Map<Uuid, User>
  counted: Uuid[]
  wipes: number
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

export class MemoryJournal implements Journal {
  private s: State = empty(0)

  constructor(private readonly opts: MemoryJournalOptions = {}) {}

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
    const next = clone(this.s)
    const messages: Message[] = []
    for (const m of page.messages) if (record(next, m, faults)) messages.push(m)
    for (const c of page.conversations) next.conversations.set(c.id, c)
    for (const u of page.users) next.users.set(u.id, u)
    if (next.cursor === null || page.logSeq > next.cursor) next.cursor = page.logSeq
    await this.commit(next, 'page')
    return {
      source: 'page', cursor: next.cursor, messages,
      conversations: [...page.conversations], users: [...page.users], receipts: [], wiped: false,
    }
  }

  async applyLive(write: LiveWrite, faults: JournalFaults): Promise<Applied> {
    const next = clone(this.s)
    const messages: Message[] = []
    for (const m of write.messages ?? []) {
      if (!record(next, m, faults)) continue
      messages.push(m)
      if (faults.cursorOnLiveFrames && (next.cursor === null || m.logSeq > next.cursor)) next.cursor = m.logSeq
    }
    for (const c of write.conversations ?? []) next.conversations.set(c.id, c)
    for (const u of write.users ?? []) next.users.set(u.id, u)
    await this.commit(next, 'live')
    return {
      source: 'live', cursor: next.cursor, messages,
      conversations: [...(write.conversations ?? [])], users: [...(write.users ?? [])], receipts: [], wiped: false,
    }
  }

  async wipe(): Promise<Applied> {
    await this.commit(empty(this.s.wipes + 1), 'wipe')
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

  /** THE ONE STEP IN WHICH A WRITE LANDS. Everything before it is staged on a
   *  copy nobody else can see. */
  private async commit(next: State, source: Applied['source']): Promise<void> {
    if (this.opts.beforeCommit) await this.opts.beforeCommit(source)
    this.s = next
  }
}

/**
 * Holds a message. THE DEDUPE KEY IS THE ID: an id not yet held is counted,
 * and a later record for a held id replaces it without a count — a CANT-92
 * re-emission is exactly that. Returns whether the record was written.
 */
function record(s: State, m: Message, faults: JournalFaults): boolean {
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

function empty(wipes: number): State {
  return { cursor: null, messages: new Map(), conversations: new Map(), users: new Map(), counted: [], wipes }
}

function clone(s: State): State {
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
