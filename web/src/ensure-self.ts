/* CANT-262 — ensure the self conversation, once, after the first catch-up.
 *
 * CANT-254 ruling 3 → B: each client calls the idempotent `POST /conversations/self`
 * once its first catch-up has completed and the journal holds no self
 * conversation. That is how an account that existed before `self` did gets a
 * Notes row without enrollment changing, and it is why nothing here is a button:
 * a person did not ask for it, so it must also never be a thing that can go
 * wrong where they can see it.
 *
 * THREE RULES, EACH ONE A TEST:
 *
 *   * AT MOST ONCE PER SESSION. The first observation of a completed catch-up
 *     is the only one that acts; a failure is not retried inside the launch
 *     (the next launch retries), because a retry loop against a server that
 *     answers 404 is a request every ~second for as long as the tab is open.
 *   * ZERO WHEN ONE IS HELD. The journal is read at the moment of the decision,
 *     so an account that already has its Notes never makes the call.
 *   * NEVER A BANNER. A 404 (a server that predates the route, which the
 *     rollout order makes unlikely but not impossible), an offline failure and
 *     a refusal all end here, quietly. Nothing is written to any state a person
 *     reads.
 *
 * Pure of Vue and of the store so a test drives it with a fake: the store wires
 * the real `holdsSelf`, the real request and the real catch-up trigger in.
 */

/** The two facts of a transport status the decision reads. */
export interface CatchUpFacts {
  /** A socket is open and `ready` has arrived. */
  ready: boolean
  /** No catch-up trigger is outstanding. */
  caughtUp: boolean
}

export interface EnsureSelfDeps {
  /** Whether the journal, as projected, already holds a self conversation. */
  holdsSelf(): boolean
  /** `POST /conversations/self`. Resolves true on a 200; may reject or resolve
   *  false on anything else. */
  request(): Promise<boolean>
  /** Called after a 200, to fetch the conversation it created through `/sync`. */
  afterCreated(): void
}

export interface EnsureSelf {
  /** Feed every status the transport emits. */
  observe(facts: CatchUpFacts): void
}

export function createEnsureSelf(deps: EnsureSelfDeps): EnsureSelf {
  let decided = false
  return {
    observe(facts) {
      if (decided || !facts.ready || !facts.caughtUp) return
      // The first completed catch-up decides, whichever way it goes.
      decided = true
      if (deps.holdsSelf()) return
      void deps
        .request()
        .then((created) => {
          if (created) deps.afterCreated()
        })
        .catch(() => {
          // Offline, a server without the route, a refused credential: none
          // is a banner. The next launch asks again.
        })
    },
  }
}
