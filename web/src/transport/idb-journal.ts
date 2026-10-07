/* The durable journal (CANT-169) — mirrors internal/client/journal.go's
 * `Journal` (its message store) on real storage. CANT-35 ruling 2's option A,
 * built as the implementation of the `Journal` interface CANT-151 shipped, so no caller
 * changes. It owns CANT-24 obligation 1's DURABLE half, which ruling 2 → B left
 * open: "the cursor is written durably before any message it covers is shown or
 * counted".
 *
 * WHERE: CANT-35's own `catenary` database, through `openCatenaryDb()` and its
 * second upgrade step (db.ts). Messages, conversations and users as wire JSON
 * keyed by `id`, the cursor and the wipe count in `journal`, and the evidence
 * log in `counted`. The credential shares the database and never this code's
 * transactions: a wipe (obligation 4) clears the journal's five stores and not
 * `credential`.
 *
 * ONE TRANSACTION PER WRITE. A page's messages, conversations, users, counted
 * ids and cursor go in one readwrite transaction, and the in-memory mirror —
 * which is what `snapshot()`, `cursor()` and the `Applied` the transport emits
 * read — is updated only after its `oncomplete`. A transaction that aborts (a
 * tab closed mid-page, a quota refused) lands none of it, and the write's
 * promise rejects with the transaction's error, which the transport surfaces as
 * `status().journalError` (a `QuotaExceededError` included) and retries on its
 * catch-up backoff. It does not fall back to memory: a journal that silently
 * stops being durable is the one failure this ticket exists to rule out.
 *
 * A PAGE LOAD RESUMES: `IdbJournal.open()` reads the whole journal back, and a
 * transport over it dials with `resume_from_log_seq` equal to the stored cursor
 * and asks `/sync` from it.
 *
 * READS ARE SYNCHRONOUS, from the mirror. The interface's reads are, and the
 * mirror is what this journal's last completed transaction left.
 *
 * TWO TABS ARE TWO WRITERS (CANT-35 ruling 1 → A: one transport per tab), each
 * with its own mirror, over one database. Records from either are the server's
 * and upsert harmlessly; what one tab can break for the other is a WIPE, which
 * clears messages the other tab's mirror — and so its cursor — still covers.
 * So a wipe stamps the journal with a fresh `generation`, and every other write
 * reads the stored generation inside its own transaction first: a journal wiped
 * under this tab refuses the write (`JournalStale`), reloads its mirror from
 * what is stored, and the transport's catch-up asks again from the stored
 * cursor. The cursor written is never below the stored one, so two tabs never
 * move it backward (obligation 2).
 */

import {
  type Conversation, type Message, type User, type Uuid,
  decodeConversation, decodeMessage, decodeUser, encodeConversation, encodeMessage, encodeUser,
} from '@/wire/generated'
import {
  CONVERSATIONS_STORE, COUNTED_STORE, JOURNAL_STORE, JOURNAL_STORES, MESSAGES_STORE, USERS_STORE,
  openCatenaryDb, type OpenCatenaryDbOptions,
} from './db'
import { type Applied, type JournalDelta, type JournalState, StagedJournal, emptyState, wipedApplied } from './journal'

export interface IdbJournalOptions extends OpenCatenaryDbOptions {
  /**
   * Awaited after a write's transaction has completed and before anything it
   * carries is observable. THE BROWSER PASSES NOTHING: IndexedDB's `oncomplete`
   * is its durability point. The Node driver passes its flush of the database
   * to a file (driver/persist.ts), which is that process's stand-in for the
   * browser's disk, so a driver killed with SIGKILL and relaunched finds every
   * write it ever showed.
   */
  durable?: () => Promise<void>
  /**
   * TEST SEAM. Called synchronously once every request of a write has been
   * issued and before its transaction can complete — the moment a tab closed
   * mid-page dies at. A test aborts `tx` here, or throws (a
   * `QuotaExceededError`, say), and the write lands nothing.
   */
  midWrite?: (tx: IDBTransaction, source: Applied['source']) => void
  /**
   * NEGATIVE CONTROL for CANT-35 criterion 32, never set outside a test: the
   * cursor goes in a second transaction after the records, so a teardown
   * between the two keeps a page's messages without the cursor that covers
   * them. `midWrite` fires in the second.
   */
  splitCursor?: boolean
  /**
   * NEGATIVE CONTROL for the two-tab rule, never set outside a test: writes
   * skip the stored-generation check, so a tab writes its own mirror's cursor
   * over a journal another tab wiped.
   */
  ignoreGeneration?: boolean
}

/** A write refused because another tab wiped the journal since this one's
 *  mirror was read; the mirror has been reloaded from the database. */
export class JournalStale extends Error {
  constructor(source: Applied['source']) {
    super(`journal: the ${source} write was refused — the journal was wiped under this tab, and it has reloaded`)
    this.name = 'JournalStale'
  }
}

/** A journal write whose transaction aborted with no error of its own: the
 *  test seam's abort, or a teardown. */
export class JournalWriteAborted extends Error {
  constructor(source: Applied['source']) {
    super(`journal: the ${source} write's transaction aborted`)
    this.name = 'JournalWriteAborted'
  }
}

interface JournalRecord {
  key: 'cursor' | 'wipes' | 'generation' | 'owner'
  value: number | string | null
}

interface CountedRecord {
  n: number
  id: string
}

export class IdbJournal extends StagedJournal {
  private constructor(
    private readonly db: IDBDatabase,
    loaded: Loaded,
    private readonly opts: IdbJournalOptions,
  ) {
    super(loaded.state)
    this.generation = loaded.generation
  }

  /** The wipe generation this mirror was read at, or last wrote. */
  private generation: string | null

  /** Opens `catenary` (running any missing upgrade step) and reads the journal
   *  back. What it holds is what the last completed transaction left. */
  static async open(opts: IdbJournalOptions = {}): Promise<IdbJournal> {
    const db = await openCatenaryDb(opts)
    try {
      return new IdbJournal(db, await load(db), opts)
    } catch (e) {
      db.close()
      throw e
    }
  }

  /** Closes the connection. A write after this rejects. */
  close(): void {
    this.db.close()
  }

  protected async commit(next: JournalState, d: JournalDelta): Promise<void> {
    const generation = d.wiped ? globalThis.crypto.randomUUID() : this.generation
    try {
      if (this.opts.splitCursor && !d.wiped) {
        await this.write(d, 'records', generation)
        await this.write(d, 'cursor', generation)
      } else {
        // A wipe keeps the STORED owner, read in the wipe's own transaction,
        // and the mirror takes the same one.
        const owner = await this.write(d, 'all', generation)
        if (d.wiped) next.owner = owner
      }
    } catch (e) {
      if (e instanceof JournalStale) {
        const loaded = await load(this.db)
        this.s = loaded.state
        this.generation = loaded.generation
      }
      throw e
    }
    this.generation = generation
    if (this.opts.durable) await this.opts.durable()
  }

  /**
   * CANT-230. Decided against what is STORED, in one transaction over the
   * journal's stores: nothing another tab commits lands between reading the
   * owner and acting on it. Another account's journal is wiped in every
   * respect — the same stores, the stored wipe count plus one, a fresh
   * generation, so another tab's next write is refused as stale — and the
   * new owner is put in its place. The mirror is then read back from what is
   * stored: the claim wrote from the database, not from this tab's copy.
   */
  override async claim(accountId: Uuid): Promise<Applied | null> {
    const wiped = await new Promise<boolean | null>((resolve, reject) => {
      let tx: IDBTransaction
      try {
        tx = this.db.transaction([...JOURNAL_STORES], 'readwrite')
      } catch (e) {
        reject(e)
        return
      }
      let outcome: boolean | null = null
      let thrown: { e: unknown } | null = null
      tx.oncomplete = () => resolve(outcome)
      tx.onabort = () => reject(thrown ? thrown.e : (tx.error ?? new JournalWriteAborted('wipe')))
      const journal = tx.objectStore(JOURNAL_STORE)
      const heldOwner = journal.get('owner')
      const heldWipes = journal.get('wipes')
      heldWipes.onsuccess = () => {
        try {
          const owner = ((heldOwner.result as JournalRecord | undefined)?.value ?? null) as Uuid | null
          if (owner === accountId) return // its own account, again: nothing is written
          outcome = owner !== null
          if (outcome) {
            const wipes = ((heldWipes.result as JournalRecord | undefined)?.value ?? 0) as number
            for (const name of JOURNAL_STORES) tx.objectStore(name).clear()
            journal.put({ key: 'generation', value: globalThis.crypto.randomUUID() } satisfies JournalRecord)
            journal.put({ key: 'cursor', value: null } satisfies JournalRecord)
            journal.put({ key: 'wipes', value: wipes + 1 } satisfies JournalRecord)
          }
          // Nobody's (a journal from before there was an owner) is kept and
          // adopted: CANT-230 ruling 1.
          journal.put({ key: 'owner', value: accountId } satisfies JournalRecord)
          this.opts.midWrite?.(tx, 'wipe')
        } catch (e) {
          thrown = { e }
          try {
            tx.abort()
          } catch {
            // Already aborted by the seam itself; its onabort carries `thrown`.
          }
        }
      }
    })
    if (wiped === null) return null
    const loaded = await load(this.db)
    this.s = loaded.state
    this.generation = loaded.generation
    if (this.opts.durable) await this.opts.durable()
    return wiped ? wipedApplied() : null
  }

  /** One transaction. A wipe resolves with the owner it found stored and put
   *  back; any other write resolves null and reads no owner. */
  private write(d: JournalDelta, part: 'all' | 'records' | 'cursor', generation: string | null): Promise<Uuid | null> {
    return new Promise((resolve, reject) => {
      let tx: IDBTransaction
      try {
        tx = this.db.transaction([...JOURNAL_STORES], 'readwrite')
      } catch (e) {
        reject(e)
        return
      }
      let thrown: { e: unknown } | null = null
      let storedOwner: Uuid | null = null
      tx.oncomplete = () => resolve(storedOwner)
      tx.onabort = () => reject(thrown ? thrown.e : (tx.error ?? new JournalWriteAborted(d.source)))
      const fail = (e: unknown) => {
        thrown = { e }
        try {
          tx.abort()
        } catch {
          // Already aborted by the seam itself; its onabort carries `thrown`.
        }
      }
      // THE STORED GENERATION AND CURSOR, read inside this transaction, so
      // nothing another tab commits can land between the check and the write.
      const journal = tx.objectStore(JOURNAL_STORE)
      const heldGeneration = journal.get('generation')
      // Only a wipe needs the owner, to put it back; a page or a live frame
      // asks for nothing it did not ask for before.
      const heldOwner = d.wiped ? journal.get('owner') : null
      const heldCursor = journal.get('cursor')
      heldCursor.onsuccess = () => {
        try {
          storedOwner = ((heldOwner?.result as JournalRecord | undefined)?.value ?? null) as Uuid | null
          const storedGeneration = ((heldGeneration.result as JournalRecord | undefined)?.value ?? null) as string | null
          const storedCursor = ((heldCursor.result as JournalRecord | undefined)?.value ?? null) as number | null
          if (!d.wiped && !this.opts.ignoreGeneration && storedGeneration !== this.generation) throw new JournalStale(d.source)
          const cursor = d.wiped || storedCursor === null || (d.cursor !== null && d.cursor > storedCursor) ? d.cursor : storedCursor
          this.puts(tx, d, part, generation, cursor, storedOwner)
        } catch (e) {
          fail(e)
        }
      }
    })
  }

  private puts(
    tx: IDBTransaction, d: JournalDelta, part: 'all' | 'records' | 'cursor', generation: string | null, cursor: number | null,
    owner: Uuid | null,
  ): void {
    if (part !== 'cursor') {
      // OBLIGATION 4's wipe: the journal's stores, and never `credential`.
      if (d.wiped) for (const name of JOURNAL_STORES) tx.objectStore(name).clear()
      const messages = tx.objectStore(MESSAGES_STORE)
      for (const m of d.messages) messages.put(encodeMessage(m))
      const conversations = tx.objectStore(CONVERSATIONS_STORE)
      for (const c of d.conversations) conversations.put(encodeConversation(c))
      const users = tx.objectStore(USERS_STORE)
      for (const u of d.users) users.put(encodeUser(u))
      const counted = tx.objectStore(COUNTED_STORE)
      d.counted.forEach((id, i) => counted.put({ n: d.countedFrom + i, id } satisfies CountedRecord))
    }
    const journal = tx.objectStore(JOURNAL_STORE)
    if (d.wiped) {
      journal.put({ key: 'generation', value: generation } satisfies JournalRecord)
      // THE OWNER SURVIVES A WIPE (CANT-230). The clear above took the
      // `journal` store with it, and a wipe is not a change of account: left
      // out, the next claim would find nobody's journal and adopt whatever
      // it had gathered since.
      journal.put({ key: 'owner', value: owner } satisfies JournalRecord)
    }
    if (part !== 'records') {
      journal.put({ key: 'cursor', value: cursor } satisfies JournalRecord)
      journal.put({ key: 'wipes', value: d.wipes } satisfies JournalRecord)
      this.opts.midWrite?.(tx, d.source)
    }
  }
}

interface Loaded {
  state: JournalState
  generation: string | null
}

/** Reads every journal store in one readonly transaction. */
function load(db: IDBDatabase): Promise<Loaded> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction([...JOURNAL_STORES], 'readonly')
    const all = (name: string) => tx.objectStore(name).getAll()
    const reqs = {
      messages: all(MESSAGES_STORE),
      conversations: all(CONVERSATIONS_STORE),
      users: all(USERS_STORE),
      journal: all(JOURNAL_STORE),
      counted: all(COUNTED_STORE),
    }
    tx.oncomplete = () => {
      try {
        resolve(decodeState(reqs))
      } catch (e) {
        reject(e)
      }
    }
    tx.onabort = () => reject(tx.error ?? new Error('journal: the read aborted'))
  })
}

/** The stored journal, decoded through the generated codecs: a record the
 *  wire schema would refuse is refused here too, loudly, not rendered. */
function decodeState(reqs: Record<'messages' | 'conversations' | 'users' | 'journal' | 'counted', IDBRequest>): Loaded {
  const meta = new Map((reqs.journal.result as JournalRecord[]).map((r) => [r.key, r.value]))
  const s = emptyState((meta.get('wipes') as number | null | undefined) ?? 0)
  s.cursor = (meta.get('cursor') as number | null | undefined) ?? null
  s.owner = (meta.get('owner') as string | null | undefined) ?? null
  for (const raw of reqs.messages.result as unknown[]) {
    const m: Message = decodeMessage(raw)
    s.messages.set(m.id, m)
  }
  for (const raw of reqs.conversations.result as unknown[]) {
    const c: Conversation = decodeConversation(raw)
    s.conversations.set(c.id, c)
  }
  for (const raw of reqs.users.result as unknown[]) {
    const u: User = decodeUser(raw)
    s.users.set(u.id, u)
  }
  // getAll returns in key order, and the key is the position.
  s.counted = (reqs.counted.result as CountedRecord[]).map((r) => r.id)
  return { state: s, generation: (meta.get('generation') as string | null | undefined) ?? null }
}
