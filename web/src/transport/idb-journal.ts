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
 * mirror is exactly what the last completed transaction left, so a read never
 * shows what the database does not hold.
 */

import {
  type Conversation, type Message, type User,
  decodeConversation, decodeMessage, decodeUser, encodeConversation, encodeMessage, encodeUser,
} from '@/wire/generated'
import {
  CONVERSATIONS_STORE, COUNTED_STORE, JOURNAL_STORE, JOURNAL_STORES, MESSAGES_STORE, USERS_STORE,
  openCatenaryDb, type OpenCatenaryDbOptions,
} from './db'
import { type Applied, type JournalDelta, type JournalState, StagedJournal, emptyState } from './journal'

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
  key: 'cursor' | 'wipes'
  value: number | null
}

interface CountedRecord {
  n: number
  id: string
}

export class IdbJournal extends StagedJournal {
  private constructor(
    private readonly db: IDBDatabase,
    initial: JournalState,
    private readonly opts: IdbJournalOptions,
  ) {
    super(initial)
  }

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

  protected async commit(_next: JournalState, d: JournalDelta): Promise<void> {
    if (this.opts.splitCursor && !d.wiped) {
      await this.write(d, 'records')
      await this.write(d, 'cursor')
    } else {
      await this.write(d, 'all')
    }
    if (this.opts.durable) await this.opts.durable()
  }

  private write(d: JournalDelta, part: 'all' | 'records' | 'cursor'): Promise<void> {
    return new Promise((resolve, reject) => {
      let tx: IDBTransaction
      try {
        tx = this.db.transaction([...JOURNAL_STORES], 'readwrite')
      } catch (e) {
        reject(e)
        return
      }
      let thrown: { e: unknown } | null = null
      tx.oncomplete = () => resolve()
      tx.onabort = () => reject(thrown ? thrown.e : (tx.error ?? new JournalWriteAborted(d.source)))
      try {
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
        if (part !== 'records') {
          const journal = tx.objectStore(JOURNAL_STORE)
          journal.put({ key: 'cursor', value: d.cursor } satisfies JournalRecord)
          journal.put({ key: 'wipes', value: d.wipes } satisfies JournalRecord)
        }
        if (part !== 'records') this.opts.midWrite?.(tx, d.source)
      } catch (e) {
        thrown = { e }
        try {
          tx.abort()
        } catch {
          // Already aborted by the seam itself; its onabort carries `thrown`.
        }
      }
    })
  }
}

/** Reads every journal store in one readonly transaction. */
function load(db: IDBDatabase): Promise<JournalState> {
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
function decodeState(reqs: Record<'messages' | 'conversations' | 'users' | 'journal' | 'counted', IDBRequest>): JournalState {
  const meta = new Map((reqs.journal.result as JournalRecord[]).map((r) => [r.key, r.value]))
  const s = emptyState((meta.get('wipes') as number | null | undefined) ?? 0)
  s.cursor = (meta.get('cursor') as number | null | undefined) ?? null
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
  return s
}
