/* Run the CLIENT'S OWN unread rules over a REAL server response.
 *
 *   node dist-readstate/readstate.js <served-sync.json>
 *
 * CANT-26's third `Done when` — "the web client's existing assertions pass
 * against real data". `validate.ts` proves a real response is a legal
 * SyncResponse; this proves the numbers in it are the numbers the canvas draws,
 * by calling `newCount` and `unreadCount` from `@/store` rather than
 * reimplementing them. A second copy of the rule that agreed with the first
 * would prove nothing, which is Invariant 2's whole argument.
 *
 * The capture is produced by `TestTheServedPageCarriesTheCanvasNumbers` in
 * `cmd/catenary` against a real Postgres: seven members, a mark at seq 1, then
 * a run of five above it, three of them from other people. Nothing in it is
 * tidied — the ids are whatever the store assigned and the timestamps are
 * whatever Postgres wrote.
 *
 * THE ADAPTER BELOW IS A STAND-IN AND SAYS SO. `@/types` is hand-written and
 * `@/types` itself records that this is temporary — R4 generates these. Until
 * the app has a real wire→client mapping, the shim lives here, in a check,
 * rather than in the app where it would quietly become the mapping.
 */

import { readFileSync } from 'node:fs'
import { codecs, type SyncResponse } from '@/wire/generated'
import type { Conversation, DeliveryState, Message } from '@/types'
import { newCount, select, state, unreadCount } from '@/store'

const path = process.argv[2]
if (!path) {
  console.error('usage: readstate <served-sync.json>')
  process.exit(2)
}

let page: SyncResponse
try {
  page = codecs.SyncResponse.decode(JSON.parse(readFileSync(path, 'utf8'))) as SyncResponse
} catch (e) {
  console.error(`FAIL  ${path} is not a SyncResponse: ${e instanceof Error ? e.message : e}`)
  process.exit(1)
}

const fail: string[] = []
const check = (name: string, ok: boolean, detail = '') => {
  if (!ok) fail.push(`${name}${detail ? ` — ${detail}` : ''}`)
  console.log(`${ok ? 'ok  ' : 'FAIL'}  ${name}${detail ? `  (${detail})` : ''}`)
}

/* The reader is the one member the page does NOT list a receipt for: `state` is
 * `sent` on their own messages and nothing else's, which is the property
 * CANT-20's review put there. Deriving the viewer from the page rather than
 * passing it in keeps this check honest about what the response carries. */
const mine = page.messages.filter((m) => m.state === 'sent')
if (mine.length === 0) {
  console.error('FAIL  the capture has no message authored by the viewer; cannot tell who is reading')
  process.exit(1)
}
const me = mine[0].authorId

state.me = me
state.users = Object.fromEntries(
  page.users.map((u) => [u.id, { id: u.id, name: u.name, initials: u.initials ?? '' }]),
) as typeof state.users
state.conversations = page.conversations.map(
  (c): Conversation => ({
    id: c.id,
    kind: c.kind,
    name: c.name,
    memberCount: c.memberCount,
    muted: c.muted,
    firstUnreadSeq: c.firstUnreadSeq,
    headSeq: c.headSeq,
  }),
)
state.messages = page.messages.map(
  (m): Message => ({
    id: m.id,
    seq: m.seq,
    conversationId: m.conversationId,
    authorId: m.authorId,
    at: m.at,
    text: m.text,
    state: m.state as DeliveryState,
    readBy: m.readBy,
  }),
)
state.read.clear()

const conversation = state.conversations[0]

// The canvas: "3 NEW" above a run of five, three of them from other people.
check('the run above the divider is five', state.messages.filter((m) => m.seq >= conversation.firstUnreadSeq!).length === 5)
check("newCount is the canvas's 3", newCount(conversation) === 3, `got ${newCount(conversation)}`)

// Invariant 3, from the client's side: your own messages are in that run and
// are not counted. Two of the five are the reader's.
check(
  'the reader authored two of the five and neither is unread',
  state.messages.filter((m) => m.seq >= conversation.firstUnreadSeq! && m.authorId === me).length === 2,
)

// The badge and the divider are the same number until the thread is opened,
// and opening clears the badge without moving the rule.
check('the badge matches the divider', unreadCount(conversation) === newCount(conversation))
select(conversation.id)
check('opening clears the badge', unreadCount(conversation) === 0)
check('the "N NEW" rule stays put for the visit', newCount(conversation) === 3)

// READ 6/7 — the fraction the room renders, against a real count of receipts:
// five members marked read, plus the author, out of seven.
const last = state.messages[state.messages.length - 1]
check('the last message reports READ 6/7', last.readBy === 6 && conversation.memberCount === 7,
  `readBy=${last.readBy} of ${conversation.memberCount}`)

// THE NUMERATOR NEVER EXCEEDS THE DENOMINATOR, and it counts the same
// population. The reader is the one member who has NOT read the last message,
// so 6 of 7 is the honest gap — and a numerator that ignored the author would
// read 5/7 with the same receipts, which is the off-by-one that made n/n
// unreachable.
check(
  'no message claims more readers than the room has members',
  state.messages.every((m) => (m.readBy ?? 0) <= conversation.memberCount),
)
check(
  'every message is read by at least its own author',
  state.messages.every((m) => (m.readBy ?? 0) >= 1),
)

if (fail.length) {
  console.error(`\n${fail.length} failed:\n  ${fail.join('\n  ')}`)
  process.exit(1)
}
console.log(`\nall green — the client's own rules over a real ${state.messages.length}-message page`)
