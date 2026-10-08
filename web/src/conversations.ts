/* CANT-270 — starting a conversation from the rail: the roster the picker
 * lists, and the one call that turns a pick into a conversation.
 *
 * TWO REST CALLS, BOTH THE SERVER'S: `GET /users` (CANT-267) lists the people
 * a conversation can be started with, and `POST /conversations/direct`
 * (CANT-75) finds or creates the direct with one of them and answers with the
 * same `Conversation` either way — so a second pick of the same person opens
 * the conversation the first one made, and this module never has to decide
 * whether it already exists. Nothing here writes a conversation into the
 * store: `openConversation` hands the record to the journal's own path
 * (store.ts), and the rail row appears when the transport delivers it.
 *
 * The picker is built around a PICK, not around "a person": `pick` is the one
 * place a click on a roster row lands, so the group flow (CANT-271) adds a
 * selection set and a name field to the view and a second call beside
 * `startDirect`, without reshaping the state below.
 */

import { reactive } from 'vue'
import { authedRequest } from '@/account'
import { openConversation } from '@/store'
import {
  decodeConversation,
  decodeRosterResponse,
  decodeServerError,
  encodeDirectConversationRequest,
  type RosterEntry,
} from '@/wire/generated'

interface PickerState {
  /** null until a roster has been read; [] is a roster that really is empty. */
  roster: RosterEntry[] | null
  loading: boolean
  /** A create is in flight. Every control that could start another is
   *  disabled while it is (criterion 2). */
  busy: boolean
  /** The id of the conversation the server returned and the journal has not
   *  delivered yet: the picker says so rather than closing on nothing. */
  opening: string
  /** What the person is told, when a call failed. Never an empty rail. */
  error: string | null
  /** The person the failed create was for — what RETRY repeats. */
  failedFor: RosterEntry | null
}

export const pickerState: PickerState = reactive({
  roster: null,
  loading: false,
  busy: false,
  opening: '',
  error: null,
  failedFor: null,
})

const UNREACHABLE = 'Could not reach the server — check your connection and try again.'

/** One sentence per refusal the server can give here, in words a person can
 *  act on. `conversation_not_found` is all three of "no such handle", "that
 *  account is deactivated" and "that is you" (store.FindOrCreateDirect), so it
 *  says what is known: that conversation cannot be made. */
export function refusalText(code: string | undefined, status: number): string {
  switch (code) {
    case 'conversation_not_found':
      return 'That conversation could not be started — the person may no longer be available. The list has been refreshed; pick again or try once more.'
    case 'rate_limited':
      return 'Too many requests just now — wait a moment and try again.'
    case 'unauthorized':
      return 'This device is not signed in any more — sign in again from DEVICES.'
    default:
      return status >= 500 || status === 0
        ? 'The server could not answer just now — try again in a moment.'
        : 'The server refused to start that conversation — try again.'
  }
}

/** `GET /users`. A failure leaves `roster` as it was and says why. */
export async function loadRoster(): Promise<void> {
  pickerState.loading = true
  try {
    const res = await authedRequest('/users')
    if (res.status !== 200) throw new Error(`users: HTTP ${res.status}`)
    pickerState.roster = decodeRosterResponse(JSON.parse(res.body)).users
    if (pickerState.error === UNREACHABLE) pickerState.error = null
  } catch {
    pickerState.error = pickerState.roster === null ? 'Could not load the list of people — check your connection and try again.' : pickerState.error
  } finally {
    pickerState.loading = false
  }
}

/**
 * `POST /conversations/direct` for one roster entry, then open what it
 * returns. Resolves true when the conversation was returned (open or on its
 * way); false when the call failed, with `pickerState.error` set and the
 * roster still listed so the pick can be repeated.
 */
export async function startDirect(entry: RosterEntry): Promise<boolean> {
  if (pickerState.busy) return false
  pickerState.busy = true
  pickerState.error = null
  pickerState.failedFor = null
  try {
    const res = await authedRequest('/conversations/direct', {
      method: 'POST',
      json: encodeDirectConversationRequest({ handle: entry.handle }),
    })
    if (res.status !== 200) {
      let code: string | undefined
      try {
        code = decodeServerError({ type: 'error', ...JSON.parse(res.body) }).code
      } catch {
        code = undefined
      }
      pickerState.error = refusalText(code, res.status)
      pickerState.failedFor = entry
      // A refused handle is a stale roster more often than not.
      if (code === 'conversation_not_found') void loadRoster()
      return false
    }
    const conversation = decodeConversation(JSON.parse(res.body))
    if (!openConversation(conversation)) pickerState.opening = conversation.id
    else pickerState.opening = ''
    return true
  } catch {
    pickerState.error = UNREACHABLE
    pickerState.failedFor = entry
    return false
  } finally {
    pickerState.busy = false
  }
}

/** The picker's single entry point for a click on a roster row. */
export function pick(entry: RosterEntry): Promise<boolean> {
  return startDirect(entry)
}

/** RETRY on a failed create: the same person, again. */
export function retryPick(): Promise<boolean> {
  const entry = pickerState.failedFor
  return entry ? startDirect(entry) : Promise.resolve(false)
}

/** Each opening of the picker starts from a clean slate and a fresh roster. */
export function resetPicker(): void {
  pickerState.busy = false
  pickerState.opening = ''
  pickerState.error = null
  pickerState.failedFor = null
  void loadRoster()
}
