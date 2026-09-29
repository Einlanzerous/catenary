/* The journal-to-Vue projection. Mirrors no internal/client file: the Go
 * client renders nothing, so it has nothing to project onto.
 *
 * Pure functions from what the transport emits (`onApply`, `snapshot()`) to
 * the shapes `store.ts` renders from (`client-types.ts`'s `Message`, the
 * generated `Conversation`, a `User` map by id). CANT-39 wires them into
 * `store.ts` when it deletes the mock (CANT-35 ruling 8 → A); this row owns
 * them and their equivalence: `projectApplied` folded over every `Applied`
 * equals `project` over the final `snapshot()`.
 *
 * NO VUE IMPORT. The result is plain data a reactive store can take.
 */

import type { Conversation, User } from '@/wire/generated'
import type { Message } from '@/client-types'
import type { Applied, JournalSnapshot } from './journal'

export interface Projection {
  /** Ordered by (conversation, seq), then id — the order `snapshot()` holds. */
  messages: Message[]
  /** By id. */
  conversations: Conversation[]
  /** By id, as `store.ts` keeps them. */
  users: Record<string, User>
}

export const EMPTY_PROJECTION: Projection = Object.freeze({
  messages: [],
  conversations: [],
  users: {},
}) as Projection

/** The whole journal, projected. A server-held message's `state` is one of
 *  the wire's own; the outbox's three are never on a record from here. */
export function project(snapshot: JournalSnapshot): Projection {
  return {
    messages: snapshot.messages.map(toMessage).sort(byThread),
    conversations: [...snapshot.conversations].sort(byId),
    users: Object.fromEntries(snapshot.users.map((u) => [u.id, u])),
  }
}

/** One `Applied`, folded into a projection. Returns a new projection and
 *  never mutates `state`. A wipe starts from empty; records upsert by id, and
 *  a later record replaces an earlier one. Receipts change nothing here
 *  (CANT-35 ruling 4 → B: a receipt's effect arrives on a page). */
export function projectApplied(state: Projection, applied: Applied): Projection {
  const base = applied.wiped ? EMPTY_PROJECTION : state
  const messages = new Map(base.messages.map((m) => [m.id, m]))
  for (const m of applied.messages) messages.set(m.id, toMessage(m))
  const conversations = new Map(base.conversations.map((c) => [c.id, c]))
  for (const c of applied.conversations) conversations.set(c.id, c)
  const users = { ...base.users }
  for (const u of applied.users) users[u.id] = u
  return {
    messages: [...messages.values()].sort(byThread),
    conversations: [...conversations.values()].sort(byId),
    users,
  }
}

function toMessage(m: JournalSnapshot['messages'][number]): Message {
  return { ...m }
}

const cmp = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)
const byId = (a: { id: string }, b: { id: string }) => cmp(a.id, b.id)
const byThread = (a: Message, b: Message) =>
  a.conversationId !== b.conversationId
    ? cmp(a.conversationId, b.conversationId)
    : a.seq !== b.seq
      ? a.seq - b.seq
      : cmp(a.id, b.id)
