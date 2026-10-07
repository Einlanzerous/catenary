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
import { endSession, liveTransport, startSession, state } from '@/store'
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
