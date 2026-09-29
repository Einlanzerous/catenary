/* Catch-up — mirrors `catchUpLoop`, `catchUp` and `fetch` in
 * internal/client/client.go: CANT-24 obligations 2–4 and CANT-103 rules 2–3.
 *
 * ONE LOOP, NEVER TWO CONCURRENT. Triggers bump `gen`; a pass ends only on a
 * `has_more: false` page whose request was issued after the most recent
 * trigger, so a trigger that arrives while a page is in flight re-arms the end
 * condition instead of starting a second catch-up (obligation 3, CANT-103 rule
 * 3). The triggers are the dial (pulled before the upgrade, so the `/sync` goes
 * out beside it), `ready`, `resync_required`, `catchUp()`, CANT-103's
 * discard, an own-user receipt (ruling 4 → B) and the page-lifecycle policy
 * (ruling 5 → B).
 *
 * A FAILED PASS WHILE NO SESSION IS READY WAITS FOR THE NEXT TRIGGER, not for
 * a timer of its own. Go retries on its own backoff as well, which during an
 * outage puts about two `/sync`s beside every dial; here the next dial's
 * trigger is the retry, so an outage costs at most one `/sync` per dial
 * (CANT-35 criterion 9). With a ready session, or while the credential layer
 * says the token is refused — when no dial is coming to pull a trigger, and
 * CANT-129's one `/sync` per hold is this loop's to send — it retries on its
 * own backoff, as Go does.
 */

import type { SyncResponse } from '@/wire/generated'
import { advance } from './backoff'
import type { Faults } from './faults'
import type { Logger, Timers } from './seams'
import type { Stats } from './status'

export interface CatchUpHost {
  faults: Faults
  stats: Stats
  timers: Timers
  log: Logger
  backoffMinMs: number
  backoffMaxMs: number
  sessionReady(): boolean
  /** CANT-129's refused-token mark (the credential layer's). */
  tokenRefused(): boolean
  /** CANT-129's hold predicate: true withholds this `/sync`. */
  withholdSync(): Promise<boolean>
  /** Drains the journal's write queue, then reads the committed cursor and
   *  the epoch (the wipe count) a request is issued in. */
  settled(): Promise<{ cursor: number | null; epoch: number }>
  /** One `GET /sync`, with the reactive refresh's single retry. */
  fetchPage(after: number): Promise<SyncResponse>
  /** Lands a page; false when a wipe intervened since `epoch` and the page
   *  was dropped (obligation 4). */
  applyPage(page: SyncResponse, epoch: number): Promise<boolean>
  notify(): void
}

export class CatchUp {
  /** Triggers pulled, and the trigger the last completed pass satisfied. */
  private gen = 0
  private doneGen = 0
  /** Obligation 4's `skipWipe` fault: the next pass starts at 0. */
  private from0 = false
  private inPass = false
  private kick: (() => void) | null = null

  constructor(private readonly host: CatchUpHost) {}

  /** A trigger. Withheld sources are the transport's to decide, not this. */
  trigger(): void {
    if (this.host.faults.ignoreRetrigger && this.inPass) return
    this.gen++
    this.wake()
    this.host.notify()
  }

  caughtUp(): boolean {
    return this.gen === this.doneGen
  }

  /** `skipWipe`'s re-sync from 0, keeping the store and the cursor. */
  restartFromZero(): void {
    this.from0 = true
  }

  /** Unblocks a waiting loop, so it can see it is no longer alive. */
  wake(): void {
    const k = this.kick
    this.kick = null
    k?.()
  }

  /** The loop, once per `start()`. `alive` goes false for good when that run
   *  stops or turns terminal, and the loop returns at its next check. */
  async run(alive: () => boolean): Promise<void> {
    const h = this.host
    let backoff = h.backoffMinMs
    while (alive()) {
      if (this.caughtUp()) {
        await this.nextTrigger()
        continue
      }
      // CANT-129: one `/sync` per refresh hold, and this loop owns it.
      if (await h.withholdSync()) {
        h.stats.syncsWithheld++
        h.notify()
        await this.sleep(h.backoffMaxMs, false, alive)
        continue
      }
      try {
        await this.pass(alive)
        backoff = h.backoffMinMs
      } catch {
        // Counted, and not logged per attempt: a dead network would otherwise
        // write a line per dial.
        if (!alive()) return
        h.stats.syncErrors++
        h.notify()
        if (h.sessionReady() || h.tokenRefused()) await this.sleep(backoff, true, alive)
        else await this.nextTrigger()
        backoff = advance(backoff, h.backoffMaxMs)
      }
    }
  }

  /** Pages until obligation 3 says it is done. */
  private async pass(alive: () => boolean): Promise<void> {
    const h = this.host
    this.inPass = true
    try {
      let next = -1 // -1: start from the cursor
      for (;;) {
        const issued = this.gen
        const { cursor, epoch } = await h.settled()
        let after = next >= 0 ? next : (cursor ?? 0)
        if (next < 0 && this.from0) {
          this.from0 = false
          after = 0
        }
        const page = await h.fetchPage(after)
        if (!alive()) return
        const applied = await h.applyPage(page, epoch)
        h.stats.pages++
        if (!applied) {
          next = -1 // a wipe intervened: start again from the new cursor
        } else if (page.hasMore) {
          next = page.logSeq
        } else if (issued === this.gen || h.faults.endCatchUpEarly) {
          this.doneGen = this.gen
          h.notify()
          return
        } else {
          next = -1 // a trigger arrived after this request: ask again
        }
        h.notify()
      }
    } finally {
      this.inPass = false
    }
  }

  private nextTrigger(): Promise<void> {
    return new Promise((resolve) => {
      this.kick = resolve
    })
  }

  /** Waits `ms`, or — when `onTrigger` — until the next trigger, whichever is
   *  first: a new trigger retries at once. */
  private sleep(ms: number, onTrigger: boolean, alive: () => boolean): Promise<void> {
    return new Promise((resolve) => {
      let finished = false
      const done = () => {
        if (finished) return
        finished = true
        this.host.timers.clearTimeout(handle)
        if (this.kick === onKick) this.kick = null
        resolve()
      }
      // Even a sleep that ignores triggers wakes for a stop.
      const onKick = () => {
        if (!onTrigger && alive()) {
          this.kick = onKick
          return
        }
        done()
      }
      const handle = this.host.timers.setTimeout(done, ms)
      this.kick = onKick
    })
  }
}
