/* `npm run test:account` — what the login form says when enrollment fails, and
 * that it says the true thing (CANT-228, the web's half of CANT-220 ruling 8).
 *
 * `POST /enroll` spends a single-use token. So WHERE a login fails decides what
 * a person has to do next, and "could not reach the server — try again" is
 * only true of a failure before any status arrived. Each test below plants one
 * failure at one point of `login()` and reads the text the screen would show:
 *
 *   before the request   the store will not open or read → the token is unsent
 *   the request itself   fetch rejects                    → unreachable
 *   a refusal            400, 401                         → the two refusal texts
 *   a 200, unreadable    not JSON, not an EnrollResponse  → may be used
 *   a 200, not stored    the store's write or read throws → used; ask for another
 *
 * node:test over a vite-ssr bundle, as `transport.test.ts` and `outbox.test.ts`
 * are, and chained into `npm run test:transport` so `./verify.sh` runs it.
 */

import { afterEach, beforeEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { IDBFactory } from 'fake-indexeddb'
import { accountState, configureAccount, CREDENTIAL_NOT_STORED_TEXT, credentialStore, login, onEnrolled } from '@/account'
import {
  enrollCredential, MemoryCredentialStore, type CredentialStore, type IdbCredentialStore, type StoredCredential,
} from '@/transport'
import { browserLock } from '@/transport/seams'

const BASE = 'http://account.test'
const UNREACHABLE = 'Could not reach the server — check your connection and try again.'
const REFUSED = 'That enrollment token was not accepted — it may be wrong, expired or already used. Ask whoever invited you for a fresh one.'

const uuid = (n: number): string => `00000000-0000-4000-8000-${n.toString(16).padStart(12, '0')}`
const padToken = (name: string): string => name + '_'.repeat(43 - name.length)

/** A well-formed `EnrollResponse`, as the server writes it. */
function pair(device = 7): Record<string, unknown> {
  const now = Date.now()
  return {
    user_id: uuid(1),
    device_id: uuid(device),
    access_token: padToken(`access-${device}`),
    access_expires_at: new Date(now + 900_000).toISOString(),
    refresh_token: padToken(`refresh-${device}`),
    refresh_expires_at: new Date(now + 86_400_000).toISOString(),
  }
}

interface Server {
  fetch: typeof globalThis.fetch
  /** How many times `/enroll` was asked — each one is a token spent. */
  enrolls: number
}

/** A server whose `/enroll` answers with `answer()`, and whose `/devices` is
 *  empty so a login that succeeds has somewhere to land. */
function server(answer: () => Response | Promise<Response>): Server {
  const s: Server = {
    enrolls: 0,
    fetch: (async (input: RequestInfo | URL): Promise<Response> => {
      const url = String(input)
      if (url.endsWith('/enroll')) {
        s.enrolls++
        return answer()
      }
      if (url.endsWith('/devices')) return new Response(JSON.stringify({ devices: [] }), { status: 200 })
      return new Response('not found', { status: 404 })
    }) as typeof globalThis.fetch,
  }
  return s
}

const accepts = (device = 7) => () => new Response(JSON.stringify(pair(device)), { status: 200, headers: { Date: new Date().toUTCString() } })

/** A store that works until it is told not to: `failing` names the operations
 *  that throw from then on. */
class PlantedStore implements CredentialStore {
  readonly inner = new MemoryCredentialStore()
  failing = new Set<'read' | 'update'>()
  reads = 0

  async read(): Promise<StoredCredential | null> {
    this.reads++
    if (this.failing.has('read')) throw new DOMException('the read was refused', 'InvalidStateError')
    return this.inner.read()
  }

  async update<T>(fn: (held: StoredCredential | null) => { write?: StoredCredential; result: T }): Promise<T> {
    if (this.failing.has('update')) throw new DOMException('the quota is full', 'QuotaExceededError')
    return this.inner.update(fn)
  }
}

let enrolledCalls = 0
let unsubscribe = (): void => {}
let logged: unknown[][] = []
const realError = console.error
const realIndexedDB = Object.getOwnPropertyDescriptor(globalThis, 'indexedDB')

beforeEach(() => {
  enrolledCalls = 0
  unsubscribe = onEnrolled(() => void enrolledCalls++)
  logged = []
  console.error = (...args: unknown[]) => void logged.push(args)
})

afterEach(() => {
  unsubscribe()
  console.error = realError
  if (realIndexedDB) Object.defineProperty(globalThis, 'indexedDB', realIndexedDB)
  else delete (globalThis as { indexedDB?: unknown }).indexedDB
  configureAccount()
})

/** Nothing about the screen may claim a session: still the login form, no
 *  device id, nobody told to start a transport. */
function assertNotLoggedIn(): void {
  assert.equal(accountState.mode, 'login', 'the screen stays on the login form')
  assert.equal(accountState.deviceId, null, 'no device id is shown for a credential that is not held')
  assert.equal(enrolledCalls, 0, 'nothing starts a session over a pair that was not stored')
  assert.equal(accountState.busy, false)
}

test('the storage text is the Flutter app’s, word for word', () => {
  // One event, one sentence, on both clients (CANT-220 ruling 8). Read from
  // the Dart source rather than typed twice here, so either moving alone fails.
  const dart = readFileSync(resolve(process.cwd(), '../app/lib/store/enrollment.dart'), 'utf8')
  const m = /const _notStored =\s*'([^']+)';/.exec(dart)
  assert.ok(m, 'app/lib/store/enrollment.dart still declares _notStored as one single-quoted string')
  assert.equal(CREDENTIAL_NOT_STORED_TEXT, m[1])
})

test('a 200 whose credential write throws is not "could not reach the server"', async () => {
  const store = new PlantedStore()
  store.failing.add('update')
  const s = server(accepts())
  configureAccount({ baseUrl: BASE, fetch: s.fetch, store })

  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(s.enrolls, 1, 'the server was reached and answered — the token is spent')
  assert.equal(accountState.error, CREDENTIAL_NOT_STORED_TEXT)
  assert.notEqual(accountState.error, UNREACHABLE)
  assertNotLoggedIn()
  assert.equal(await store.inner.read(), null, 'and nothing was stored')
  assert.equal(logged.length, 1, 'the cause is logged once, since the screen does not show it')
  assert.match(String(logged[0][0]), /could not store it/)
  assert.ok(logged[0][1] instanceof DOMException && logged[0][1].name === 'QuotaExceededError')
})

test('a 200 whose re-enrollment write throws leaves the held pair alone and says the same', async () => {
  const store = new PlantedStore()
  const s = server(accepts(7))
  configureAccount({ baseUrl: BASE, fetch: s.fetch, store })
  assert.equal(await login('first-token', 'This browser'), true)
  const before = await store.inner.read()
  assert.equal(before?.deviceId, uuid(7))

  store.failing.add('update')
  const again = server(accepts(8))
  configureAccount({ baseUrl: BASE, fetch: again.fetch, store })
  enrolledCalls = 0
  assert.equal(await login('second-token', 'This browser'), false)
  assert.equal(accountState.error, CREDENTIAL_NOT_STORED_TEXT)
  assertNotLoggedIn()
  assert.deepEqual(await store.inner.read(), before, 'the pair that was held is still the pair that is held')
})

test('a 200 followed by a store that will not be read says the same', async () => {
  // The read after the answer — the one that picks enroll or re-enroll — is
  // past the 200 as surely as the write is.
  const store = new PlantedStore()
  const s = server(() => {
    store.failing.add('read')
    return accepts()()
  })
  configureAccount({ baseUrl: BASE, fetch: s.fetch, store })

  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(s.enrolls, 1)
  assert.equal(accountState.error, CREDENTIAL_NOT_STORED_TEXT)
  assertNotLoggedIn()
})

test('a 200 whose write never gets its lock says the same', async () => {
  // The write runs under the credential's lock (CANT-31 §2), and a lock that
  // fails is one more thing past the 200.
  const store = new MemoryCredentialStore()
  const s = server(accepts())
  configureAccount({
    baseUrl: BASE,
    fetch: s.fetch,
    store,
    lock: () => Promise.reject(new DOMException('the lock manager is gone', 'InvalidStateError')),
  })
  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(accountState.error, CREDENTIAL_NOT_STORED_TEXT)
  assertNotLoggedIn()
  assert.equal(await store.read(), null)
})

test('a request that never gets an answer is still "could not reach the server"', async () => {
  const store = new PlantedStore()
  const s = server(() => Promise.reject(new TypeError('fetch failed')))
  configureAccount({ baseUrl: BASE, fetch: s.fetch, store })

  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(accountState.error, UNREACHABLE)
  assertNotLoggedIn()
  assert.equal(logged.length, 0)
})

test('a refusal keeps its two texts, with or without a body that arrives', async () => {
  const store = new PlantedStore()
  const refused = server(() => new Response(JSON.stringify({ code: 'unauthorized' }), { status: 401 }))
  configureAccount({ baseUrl: BASE, fetch: refused.fetch, store })
  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(accountState.error, REFUSED)

  const malformed = server(() => new Response(JSON.stringify({ code: 'bad_request' }), { status: 400 }))
  configureAccount({ baseUrl: BASE, fetch: malformed.fetch, store })
  assert.equal(await login('', 'This browser'), false)
  assert.equal(accountState.error, 'That does not look like a valid enrollment token or device name.')

  // The status is the answer: a refusal whose body never arrives is a
  // refusal, said at once rather than after waiting for it.
  let cancelled = false
  const stalled = server(
    () => new Response(new ReadableStream({ cancel: () => void (cancelled = true) }), { status: 401 }),
  )
  configureAccount({ baseUrl: BASE, fetch: stalled.fetch, store })
  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(accountState.error, REFUSED)
  assert.equal(cancelled, true, 'and the body nobody will read is let go')
  assertNotLoggedIn()
})

test('a status that is not Catenary refusing is not the token being refused', async () => {
  // Catenary's own 500 (`enrollment failed`), a proxy's 502 while it restarts,
  // and whatever else a hop in front can answer: nobody refused the token, so
  // nobody is told to replace it. Only 400 and 401 are refusals.
  for (const status of [403, 404, 413, 429, 500, 502, 503]) {
    const store = new PlantedStore()
    const s = server(() => new Response('upstream', { status }))
    configureAccount({ baseUrl: BASE, fetch: s.fetch, store })
    assert.equal(await login('a-token', 'This browser'), false)
    assert.notEqual(accountState.error, REFUSED, String(status))
    assert.match(accountState.error ?? '', /the token was not refused\. Try again in a moment\.$/, String(status))
    assertNotLoggedIn()
  }
})

test('a 200 that cannot be read is neither unreachable nor a claim that the token is spent', async () => {
  const answers: Array<[string, () => Response]> = [
    ['a page from something in front of Catenary', () => new Response('<html>Sign in to the hotel Wi-Fi</html>', { status: 200 })],
    ['JSON that is not an EnrollResponse', () => new Response(JSON.stringify({ ok: true }), { status: 200 })],
    ['a pair with a timestamp that is not one', () => new Response(JSON.stringify({ ...pair(), access_expires_at: 'soon' }), { status: 200 })],
    ['a body that breaks off', () => {
      const res = new Response(null, { status: 200 })
      res.text = () => Promise.reject(new TypeError('terminated'))
      return res
    }],
  ]
  for (const [what, answer] of answers) {
    const store = new PlantedStore()
    const s = server(answer)
    configureAccount({ baseUrl: BASE, fetch: s.fetch, store })
    assert.equal(await login('a-token', 'This browser'), false, what)
    assert.notEqual(accountState.error, UNREACHABLE, what)
    assert.notEqual(accountState.error, CREDENTIAL_NOT_STORED_TEXT, `${what}: nothing reached the store`)
    assert.match(accountState.error ?? '', /^The server answered, but not with anything this app could read\. The token may already be used/, what)
    assertNotLoggedIn()
    assert.equal(await store.inner.read(), null, what)
  }
})

test('a store that will not be read is found out before the token is sent', async () => {
  const store = new PlantedStore()
  store.failing.add('read')
  const s = server(accepts())
  configureAccount({ baseUrl: BASE, fetch: s.fetch, store })

  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(s.enrolls, 0, '/enroll was never asked, so the token is still good')
  assert.match(accountState.error ?? '', /the token was not sent and is still unused/)
  assert.notEqual(accountState.error, UNREACHABLE)
  assertNotLoggedIn()
})

test('IndexedDB that will not open spends no token, and is asked again on the next try', async () => {
  // No `store` seam here: this is `openStore()`'s own path, the one a browser
  // with storage blocked takes. Blocked storage is an `indexedDB` that exists
  // and refuses, which is why the in-memory fallback (for a host with no
  // `indexedDB` at all) never catches it.
  let opens = 0
  const blocked = {
    open(): never {
      opens++
      throw new DOMException('The operation is insecure.', 'SecurityError')
    },
  }
  Object.defineProperty(globalThis, 'indexedDB', { value: blocked, configurable: true, writable: true })
  const s = server(accepts())
  configureAccount({ baseUrl: BASE, fetch: s.fetch })

  await assert.rejects(credentialStore(), /insecure/, 'main.ts’s own read at boot fails the same way')
  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(s.enrolls, 0, 'two tries, no token spent')
  assert.equal(opens, 5, 'each try asked the browser again (twice: once more after dropping the cache) rather than replaying the first refusal')
  assert.match(accountState.error ?? '', /the token was not sent and is still unused/)
  assertNotLoggedIn()

  // The person allows storage and tries again, without reloading.
  Object.defineProperty(globalThis, 'indexedDB', { value: new IDBFactory(), configurable: true, writable: true })
  assert.equal(await login('a-token', 'This browser'), true)
  assert.equal(s.enrolls, 1)
  assert.equal(accountState.error, null)
  assert.equal(accountState.mode, 'sessions')
  assert.equal((await (await credentialStore()).read())?.deviceId, uuid(7), 'and the pair is in IndexedDB')
})

test('a database handle that closed under this tab is reopened, not reported as blocked storage', async () => {
  // db.ts closes the handle when another tab upgrades the database. The
  // cached store is then dead for good, and storage is not blocked at all.
  Object.defineProperty(globalThis, 'indexedDB', { value: new IDBFactory(), configurable: true, writable: true })
  const s = server(accepts())
  configureAccount({ baseUrl: BASE, fetch: s.fetch })
  const first = await credentialStore()
  ;(first as IdbCredentialStore).close()
  await assert.rejects(first.read(), 'the cached handle is dead')

  assert.equal(await login('a-token', 'This browser'), true)
  assert.equal(accountState.error, null)
  const now = await credentialStore()
  assert.notEqual(now, first, 'main.ts restarts the session over the reopened store')
  assert.equal((await now.read())?.deviceId, uuid(7))
})

test('a host with nowhere durable to keep a credential is refused before the token is sent', async () => {
  // No `indexedDB` at all: `credentialStore()` falls back to memory so a
  // read still answers, but a login over it would be a session that the next
  // reload forgets, paid for with a single-use token.
  delete (globalThis as { indexedDB?: unknown }).indexedDB
  const s = server(accepts())
  configureAccount({ baseUrl: BASE, fetch: s.fetch })
  assert.equal(await (await credentialStore()).read(), null, 'the fallback still reads as "nothing held"')

  assert.equal(await login('a-token', 'This browser'), false)
  assert.equal(s.enrolls, 0)
  assert.match(accountState.error ?? '', /the token was not sent and is still unused/)
  assertNotLoggedIn()
})

test('a login that works still works: first enrollment, then re-enrollment over a held pair', async () => {
  const store = new PlantedStore()
  const s = server(accepts(7))
  configureAccount({ baseUrl: BASE, fetch: s.fetch, store })
  assert.equal(await login('a-token', 'This browser'), true)
  assert.equal(accountState.error, null)
  assert.equal(accountState.mode, 'sessions')
  assert.equal(accountState.deviceId, uuid(7))
  assert.equal(enrolledCalls, 1)
  assert.equal((await store.inner.read())?.refreshToken, padToken('refresh-7'))
  assert.equal(logged.length, 0)

  // A pair already held (seeded directly, as another visit would have left
  // it) is replaced, not refused.
  const held = new MemoryCredentialStore()
  await enrollCredential(held, browserLock(), (await store.inner.read())!)
  const next = server(accepts(8))
  configureAccount({ baseUrl: BASE, fetch: next.fetch, store: held })
  assert.equal(await login('another-token', 'This browser'), true)
  assert.equal((await held.read())?.deviceId, uuid(8))
})
