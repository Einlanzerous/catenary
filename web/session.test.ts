/* `npm run test:session` — what `startSession` does with a journal that is not
 * the held credential's (CANT-230, the web's half).
 *
 * A re-enrollment writes the pair and then wipes the journal, as two writes. A
 * tab closed between them leaves one account's credential over another
 * account's records and cursor. `startSession` claims the journal for the
 * credential's account before it builds a transport, so a journal that is
 * someone else's is wiped there, whatever was or was not written before.
 *
 * node:test over a vite-ssr bundle, as `account.test.ts` is, over
 * fake-indexeddb and a socket that never opens, and chained into
 * `npm run test:transport` so `./verify.sh` runs it with no database.
 * `smoke.ts` is the only other place `startSession` runs, and it needs a live
 * server.
 *
 * RUN WITH `--test-force-exit`, in an invocation of its own. A start that
 * wrongly builds a second transport leaves one nothing here can reach, and its
 * timers would hold the process open: the run would hang where it should fail.
 */

import { afterEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { IDBFactory } from 'fake-indexeddb'
import { watch } from 'vue'
import { endSession, jumpTo, liveTransport, restoreOpenFromUrl, select, startSession, state } from '@/store'
import { IdbJournal, MemoryCredentialStore, MemoryJournal, type Applied, type StoredCredential, type WebSocketLike } from '@/transport'
import { bootstrapPage, conversation, deferred, flush, ME, message, OTHER, user, uuid } from '@/transport/test/harness'
import { enrolled } from '@/transport/test/credential-harness'

const BASE = 'http://session.test'
const NO_FAULTS = { cursorOnLiveFrames: false, dedupeByLogSeq: false, keepHeldConversation: false }

/** A socket that is constructed and never opens: nothing leaves the process. */
let sockets = 0
class DeadSocket implements WebSocketLike {
  onopen: WebSocketLike['onopen'] = null
  onmessage: WebSocketLike['onmessage'] = null
  onclose: WebSocketLike['onclose'] = null
  onerror: WebSocketLike['onerror'] = null
  constructor() {
    sockets++
  }
  send(): void {
    throw new Error('never open')
  }
  close(): void {}
}

const quiet = { info() {}, warn() {} }
const unreachable = (async () => {
  throw new TypeError('fetch failed')
}) as typeof globalThis.fetch

/** `OTHER`'s pair: a new device, as a re-enrollment by another person mints. */
const othersPair = (): StoredCredential => ({ ...enrolled(Date.now()), userId: OTHER, deviceId: uuid(9) })

const P1 = {
  ...bootstrapPage(3, [message(1), message(2), message(3)]),
  conversations: [conversation(uuid(100))],
  users: [user(ME), user(OTHER)],
}

afterEach(() => {
  endSession()
  sockets = 0
})

test('CANT-303 · the store is seeded connecting, which draws no banner, and a hung first dial claims nothing', async () => {
  // Before any session exists, and then with a first dial that has not opened.
  assert.equal(state.connection.state, 'connecting', 'the seed is not `reconnecting`: nothing has been lost')
  const store = new MemoryCredentialStore(enrolled(Date.now()))
  const journal = new MemoryJournal()
  assert.equal(await startSession({ baseUrl: BASE, store, journal, WebSocket: DeadSocket, fetch: unreachable, logger: quiet }), true)
  assert.equal(state.connection.state, 'connecting', 'a first dial in flight is still not lost')
  endSession()
})

test('CANT-303 · a refused connection shows the banner state on the first status after the failure', async () => {
  /** A socket whose connect is refused: closes without ever opening. */
  class RefusedSocket extends DeadSocket {
    constructor() {
      super()
      queueMicrotask(() => setTimeout(() => {
        this.onerror?.({})
        this.onclose?.({ code: 1006 })
      }, 0))
    }
  }
  const store = new MemoryCredentialStore(enrolled(Date.now()))
  const journal = new MemoryJournal()
  await startSession({ baseUrl: BASE, store, journal, WebSocket: RefusedSocket, fetch: unreachable, logger: quiet })
  for (let i = 0; i < 50 && state.connection.state === 'connecting'; i++) await new Promise((r) => setTimeout(r, 10))
  assert.equal(state.connection.state, 'reconnecting', 'the failure is reported, so the banner draws')
  assert.equal(state.connection.attempt, 1)
  endSession()
})

test('CANT-230 · the web does not open another account’s journal: the claim wipes it before a transport is built', async () => {
  const factory = new IDBFactory()
  // The journal as one account's visits left it…
  const first = await IdbJournal.open({ factory })
  await first.claim(ME)
  await first.applyPage(P1, NO_FAULTS)
  first.close()
  // …and the credential store as a re-enrollment by another person left it,
  // with no wipe after: the tab was closed between the two writes.
  const store = new MemoryCredentialStore(othersPair())

  const journal = await IdbJournal.open({ factory })
  assert.equal(journal.cursor(), 3)
  assert.equal(journal.messageCount(), 3)
  const started = await startSession({ baseUrl: BASE, store, journal, WebSocket: DeadSocket, fetch: unreachable, logger: quiet })
  assert.equal(started, true)
  assert.equal(state.me, OTHER)
  assert.deepEqual(state.conversations, [])
  assert.deepEqual(state.messages, [])
  assert.equal(journal.cursor(), null)
  assert.equal(journal.messageCount(), 0)
  assert.equal(journal.owner(), OTHER)

  endSession()
  journal.close()
  const reopened = await IdbJournal.open({ factory })
  assert.deepEqual(reopened.snapshot(), { cursor: null, messages: [], conversations: [], users: [] })
  reopened.close()
})

test('CANT-230 · the same account’s journal is kept: its records and cursor are what the start shows', async () => {
  const factory = new IDBFactory()
  const first = await IdbJournal.open({ factory })
  await first.claim(ME)
  await first.applyPage(P1, NO_FAULTS)
  first.close()
  // A new device of the same account.
  const store = new MemoryCredentialStore({ ...enrolled(Date.now()), deviceId: uuid(9) })

  const journal = await IdbJournal.open({ factory })
  assert.equal(await startSession({ baseUrl: BASE, store, journal, WebSocket: DeadSocket, fetch: unreachable, logger: quiet }), true)
  assert.equal(journal.cursor(), 3)
  assert.equal(journal.messageCount(), 3)
  assert.equal(state.conversations.length, 1)
  endSession()
  journal.close()
})

test('CANT-230 · a start overtaken during its claim builds no transport', async () => {
  /** A journal whose FIRST claim is held open by the test. */
  class HeldClaim extends MemoryJournal {
    readonly gate = deferred<void>()
    claims = 0
    override async claim(accountId: string): Promise<Applied | null> {
      if (++this.claims === 1) await this.gate.promise
      return super.claim(accountId)
    }
  }
  const journal = new HeldClaim()
  const store = new MemoryCredentialStore(enrolled(Date.now()))
  const seams = { baseUrl: BASE, store, journal, WebSocket: DeadSocket, fetch: unreachable, logger: quiet }

  const overtaken = startSession(seams)
  await flush()
  assert.equal(journal.claims, 1, 'the first start is inside its claim')
  assert.equal(liveTransport(), null)

  // A second start overtakes it and runs to the end.
  assert.equal(await startSession(seams), true)
  const second = liveTransport()
  assert.ok(second)
  await flush()
  const dialed = sockets

  journal.gate.resolve()
  assert.equal(await overtaken, false, 'the overtaken start abandons itself after its claim')
  await flush()
  assert.equal(liveTransport(), second, 'the session running is the second start’s')
  assert.equal(sockets, dialed, 'and the overtaken start dialed nothing')
  assert.equal(sockets, 1, 'one transport, one dial')
})

/* ── CANT-369 · a reload keeps the open conversation ───────────────────── */

const ROOM_A = uuid(100)
const ROOM_B = uuid(101)
const TWO_ROOMS = {
  ...bootstrapPage(3, [message(1), message(2), message(3)]),
  conversations: [conversation(ROOM_A), conversation(ROOM_B)],
  users: [user(ME), user(OTHER)],
}

/** A browser's `location` and `history`, as far as the store touches them:
 *  `replaceState` rewrites the hash, `pushState` is counted and must stay 0. */
function fakeBrowser(hash: string) {
  const calls = { replace: 0, push: 0 }
  const loc = { hash, pathname: '/', search: '' }
  const g = globalThis as Record<string, unknown>
  g.location = loc
  g.history = {
    state: null,
    replaceState(_s: unknown, _t: string, url: string) {
      calls.replace++
      loc.hash = url.includes('#') ? url.slice(url.indexOf('#')) : ''
    },
    pushState() {
      calls.push++
    },
  }
  restoreOpenFromUrl()
  return { loc, calls }
}

afterEach(() => {
  const g = globalThis as Record<string, unknown>
  delete g.location
  delete g.history
  restoreOpenFromUrl()
})

/** Start a session over a journal holding `TWO_ROOMS`, recording every value
 *  `activeId` takes (synchronously, so a flash cannot hide between ticks). */
async function startOver(): Promise<string[]> {
  const factory = new IDBFactory()
  const first = await IdbJournal.open({ factory })
  await first.claim(ME)
  await first.applyPage(TWO_ROOMS, NO_FAULTS)
  first.close()
  state.me = ''
  state.activeId = ''
  const seen: string[] = []
  const stop = watch(() => state.activeId, (v) => seen.push(v), { flush: 'sync' })
  const journal = await IdbJournal.open({ factory })
  const store = new MemoryCredentialStore({ ...enrolled(Date.now()), deviceId: uuid(9) })
  await startSession({ baseUrl: BASE, store, journal, WebSocket: DeadSocket, fetch: unreachable, logger: quiet })
  stop()
  return seen
}

test('CANT-369 · a reload restores the conversation named in the URL, and the first in the rail is never shown', async () => {
  fakeBrowser(`#/c/${ROOM_B}`)
  const seen = await startOver()
  assert.equal(state.activeId, ROOM_B)
  // No flash: ROOM_A (the top of the rail) was never active, not even for a tick.
  assert.deepEqual(seen, [ROOM_B])
})

test('CANT-369 · an id that is not in the list falls back to the first in the rail', async () => {
  const { loc } = fakeBrowser(`#/c/${uuid(999)}`)
  await startOver()
  assert.equal(state.activeId, ROOM_A)
  // The fallback does not overwrite the id the URL carried: a reload before the
  // list is complete must still find it. The reader's own choice does.
  assert.equal(loc.hash, `#/c/${uuid(999)}`)
  select(ROOM_B)
  assert.equal(loc.hash, `#/c/${ROOM_B}`)
})

test('CANT-369 · no hash opens the first in the rail, as before', async () => {
  fakeBrowser('')
  await startOver()
  assert.equal(state.activeId, ROOM_A)
})

test('CANT-369 · a switch rewrites the hash with replaceState and never pushes history', async () => {
  const { loc, calls } = fakeBrowser('')
  await startOver()
  const before = calls.replace
  select(ROOM_B)
  assert.equal(loc.hash, `#/c/${ROOM_B}`)
  select(ROOM_A)
  assert.equal(loc.hash, `#/c/${ROOM_A}`)
  const m = state.messages[0]!
  jumpTo(m.id)
  assert.equal(loc.hash, `#/c/${m.conversationId}`)
  assert.ok(calls.replace > before)
  assert.equal(calls.push, 0, 'Back is not spammed')
})

test('CANT-369 · with no location at all (the SSR smoke) nothing throws', async () => {
  await startOver()
  select(ROOM_B)
  assert.equal(state.activeId, ROOM_B)
})

/** A socket that opens at once and keeps every frame the client writes. */
const written: string[] = []
class OpenSocket implements WebSocketLike {
  onopen: WebSocketLike['onopen'] = null
  onmessage: WebSocketLike['onmessage'] = null
  onclose: WebSocketLike['onclose'] = null
  onerror: WebSocketLike['onerror'] = null
  constructor() {
    setTimeout(() => this.onopen?.({}), 0)
  }
  send(data: string): void {
    written.push(data)
  }
  close(): void {}
}

test('CANT-373 · a client built with a release version names it on the hello, never `dev`', async () => {
  // This bundle is built with CATENARY_WEB_VERSION=0.0.0-session-test
  // (package.json, build:session-test), as deploy/Dockerfile builds the
  // released client with the image's VERSION.
  written.length = 0
  const store = new MemoryCredentialStore(enrolled(Date.now()))
  await startSession({ baseUrl: BASE, store, journal: new MemoryJournal(), WebSocket: OpenSocket, fetch: unreachable, logger: quiet })
  for (let i = 0; i < 200 && written.length === 0; i++) await new Promise((r) => setTimeout(r, 10))
  assert.ok(written.length > 0, 'the socket opened and nothing was written to it')
  const hello = JSON.parse(written[0]!) as { type: string; client_info?: string }
  assert.equal(hello.type, 'hello')
  assert.equal(hello.client_info, 'catenary-web/0.0.0-session-test')
})
