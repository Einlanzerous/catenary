/* `npm run test:conversations` — starting a conversation from the rail
 * (CANT-270), and that the picker claims nothing the server cannot keep.
 *
 *   an empty conversation   head_seq 0 renders no preview, no marker, no
 *                           TRANSCRIBING and no delivery state anywhere
 *   a create in flight      the rail's control and every roster row are
 *                           disabled, so a second create cannot start
 *   a refusal               conversation_not_found is a visible message with
 *                           RETRY over a roster that is still listed — never
 *                           an empty rail — and RETRY repeats the same pick
 *   a repeat pick           a conversation already held opens at once and is
 *                           held once
 *
 * node:test over a vite-ssr bundle, as `account.test.ts` is, and chained into
 * `npm run test:transport` so `./verify.sh` runs it.
 */

import { afterEach, beforeEach, test } from 'node:test'
import assert from 'node:assert/strict'
import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from '@/App.vue'
import { configureAccount } from '@/account'
import { pickerState, retryPick, resetPicker, startDirect } from '@/conversations'
import { closeNew, openNew, select, state } from '@/store'
import { enrollCredential, MemoryCredentialStore, type StoredCredential } from '@/transport'
import { browserLock } from '@/transport/seams'
import type { Conversation, RosterEntry } from '@/wire/generated'

const BASE = 'http://picker.test'
const uuid = (n: number): string => `00000000-0000-4000-8000-${n.toString(16).padStart(12, '0')}`
const ME = uuid(1)
const INES = uuid(2)
const ROSA = uuid(3)
const DM = uuid(10)

const roster: RosterEntry[] = [
  { id: INES, name: 'Ines Calloway', initials: 'IC', handle: 'ines' },
  { id: ROSA, name: 'Rosa Whitfield', initials: 'RW', handle: 'rosa' },
]

const direct = (over: Partial<Conversation> = {}): Conversation => ({
  id: DM, kind: 'direct', name: 'Ines Calloway', otherMemberId: INES, memberCount: 2, headSeq: 0, ...over,
})

const wireConversation = (c: Conversation) => ({
  id: c.id, kind: c.kind, name: c.name, other_member_id: c.otherMemberId, member_count: c.memberCount, head_seq: c.headSeq,
})

const render = () => renderToString(createSSRApp(App))
const text = (html: string) => html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()

/** The whole row, from its opening <button> to its close. */
function row(html: string, id: string): string {
  const at = html.indexOf(`data-conversation-id="${id}"`)
  if (at < 0) return ''
  return html.slice(html.lastIndexOf('<button', at), html.indexOf('</button>', at))
}

/** Claims an empty conversation must not make anywhere on the page. */
const CLAIMS = ['TRANSCRIBING', 'transcript pending', 'QUEUED', 'SENDING', 'SENT', 'DELIVERED', 'READ', 'FAILED']

interface Posted {
  handle: string
  answer: (r: Response) => void
}

/** A server: `/users` answers the roster; each `POST /conversations/direct`
 *  waits for the test to answer it, so "in flight" is something to look at. */
function server() {
  const posts: Posted[] = []
  let users = roster
  const fetch = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = String(input)
    if (url.endsWith('/users')) return new Response(JSON.stringify({ users }), { status: 200 })
    if (url.endsWith('/conversations/direct')) {
      const handle = JSON.parse(String(init?.body)).handle as string
      return new Promise<Response>((resolve) => posts.push({ handle, answer: resolve }))
    }
    return new Response('not found', { status: 404 })
  }) as typeof globalThis.fetch
  return { fetch, posts, setUsers: (u: RosterEntry[]) => (users = u) }
}

const settle = () => new Promise((r) => setTimeout(r, 0))

beforeEach(async () => {
  const store = new MemoryCredentialStore()
  const now = Date.now()
  const stored: StoredCredential = {
    userId: ME, deviceId: uuid(7), accessToken: 'a'.repeat(43), accessExpiresAt: now + 3_600_000,
    refreshToken: 'r'.repeat(43), refreshExpiresAt: now + 86_400_000, accessIssuedAt: now, clockOffsetMs: 0,
    chain: [], lastSentAt: null,
  }
  await enrollCredential(store, browserLock(), stored)
  state.me = ME
  state.origin = BASE
  state.users = {}
  state.conversations = []
  state.messages = []
  state.activeId = ''
  state.pendingOpenId = ''
  state.view = 'thread'
  pickerState.roster = null
  pickerState.error = null
  pickerState.failedFor = null
  pickerState.busy = false
  pickerState.opening = ''
  ;(globalThis as { __store?: MemoryCredentialStore }).__store = store
})

afterEach(() => {
  closeNew()
  configureAccount()
})

function useServer(s: ReturnType<typeof server>) {
  configureAccount({ baseUrl: BASE, store: (globalThis as unknown as { __store: MemoryCredentialStore }).__store, fetch: s.fetch })
}

test('a conversation with head_seq 0 renders no preview, no marker, no TRANSCRIBING and no delivery state', async () => {
  state.users = { [INES]: { id: INES, name: 'Ines Calloway', initials: 'IC' } }
  state.conversations = [direct()]
  select(DM)
  const html = await render()
  assert.equal(text(row(html, DM)), 'Ines Calloway', 'the rail row is the name alone')
  assert.ok(!html.includes('data-message='), 'the thread holds no message row')
  const claims = CLAIMS.filter((w) => html.includes(w))
  assert.deepEqual(claims, [], 'nothing is claimed about an empty thread')
})

test('the rail control and every roster row are disabled while a create is in flight', async () => {
  const s = server()
  useServer(s)
  openNew()
  resetPicker()
  await settle()
  assert.deepEqual(pickerState.roster, roster)
  const idle = await render()
  assert.ok(/class="new"(?![^>]*disabled)/.test(idle), 'the control is enabled before a create')
  assert.ok(!/data-roster-handle="ines"[^>]*disabled/.test(idle) && !/disabled[^>]*data-roster-handle="ines"/.test(idle))

  const first = startDirect(roster[0])
  await settle()
  assert.equal(pickerState.busy, true)
  const busy = await render()
  assert.ok(/class="new"[^>]*disabled/.test(busy) || /disabled[^>]*class="new"/.test(busy), 'the rail control is disabled')
  for (const handle of ['ines', 'rosa']) {
    assert.ok(new RegExp(`disabled[^>]*data-roster-handle="${handle}"|data-roster-handle="${handle}"[^>]*disabled`).test(busy),
      `the ${handle} row is disabled`)
  }
  assert.equal(await startDirect(roster[1]), false, 'a second create is refused while one is in flight')
  assert.equal(s.posts.length, 1, 'and never reaches the server')

  s.posts[0].answer(new Response(JSON.stringify(wireConversation(direct())), { status: 200 }))
  assert.equal(await first, true)
})

test('conversation_not_found is a visible, retryable message over a roster that is still listed', async () => {
  const s = server()
  useServer(s)
  openNew()
  resetPicker()
  await settle()

  const first = startDirect(roster[0])
  await settle()
  s.posts[0].answer(new Response(
    JSON.stringify({ type: 'error', code: 'conversation_not_found', message: 'find-or-create direct failed', retryable: false }),
    { status: 404 },
  ))
  assert.equal(await first, false)
  await settle()

  const html = await render()
  assert.ok(html.includes('role="alert"'), 'the refusal is on the page')
  assert.ok(text(html).includes('That conversation could not be started'), 'in words, not a code')
  assert.ok(html.includes('>RETRY<'), 'with a way forward')
  assert.ok(html.includes('Ines Calloway') && html.includes('Rosa Whitfield'), 'and the roster is still there, not an empty rail')
  assert.ok(!/disabled[^>]*data-roster-handle|data-roster-handle[^>]*disabled/.test(html), 'nothing stays disabled after a refusal')
  assert.equal(state.conversations.length, 0, 'and no conversation was invented')

  // RETRY repeats the same pick and, answered, leaves the failure behind.
  const retry = retryPick()
  await settle()
  assert.equal(s.posts.length, 2)
  assert.equal(s.posts[1].handle, 'ines', 'the same person')
  s.posts[1].answer(new Response(JSON.stringify(wireConversation(direct())), { status: 200 }))
  assert.equal(await retry, true)
  assert.equal(pickerState.error, null)
})

test('a conversation the server returns that is not held yet waits for the journal rather than being invented', async () => {
  const s = server()
  useServer(s)
  openNew()
  resetPicker()
  await settle()
  const pending = startDirect(roster[0])
  await settle()
  s.posts[0].answer(new Response(JSON.stringify(wireConversation(direct())), { status: 200 }))
  assert.equal(await pending, true)
  assert.equal(state.conversations.length, 0, 'nothing is written into the store from here')
  assert.equal(state.pendingOpenId, DM, 'it opens when the journal delivers it')
  assert.equal(pickerState.opening, DM)
  assert.ok(text(await render()).includes('Waiting for the server to deliver it'))
})

test('choosing another conversation abandons one still on its way', () => {
  state.conversations = [direct({ id: uuid(11), name: 'Rosa Whitfield', otherMemberId: ROSA })]
  state.pendingOpenId = DM
  select(uuid(11))
  assert.equal(state.pendingOpenId, '', 'it will not pull the reader away when it lands')
})

test('a conversation already held opens at once and is held once', async () => {
  const s = server()
  useServer(s)
  state.users = { [INES]: { id: INES, name: 'Ines Calloway', initials: 'IC' } }
  state.conversations = [direct()]
  state.activeId = ''
  openNew()
  resetPicker()
  await settle()
  const again = startDirect(roster[0])
  await settle()
  s.posts[0].answer(new Response(JSON.stringify(wireConversation(direct())), { status: 200 }))
  assert.equal(await again, true)
  assert.equal(state.activeId, DM)
  assert.equal(state.view, 'thread')
  assert.equal(state.pendingOpenId, '')
  assert.equal(state.conversations.filter((c) => c.id === DM).length, 1)
  assert.equal((await render()).split(`data-conversation-id="${DM}"`).length - 1, 1, 'one rail row')
})
