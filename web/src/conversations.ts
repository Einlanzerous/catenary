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
import { AuthedUnauthorized, authedRequest } from '@/account'
import { openConversation } from '@/store'
import {
  decodeConversation,
  decodeRosterResponse,
  decodeServerError,
  encodeCreateGroupRequest,
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
  /** CANT-271 — the people chosen for a group, in the order chosen. The
   *  caller is never in it: the server adds them. One of them is not a group
   *  (a single pick is a direct); two or more reveal the name field. */
  selected: RosterEntry[]
  /** The name field, as typed. Trimmed only when sent. */
  groupName: string
  /** The `request_id` of the current create attempt, '' until one is made.
   *  It survives a failure so RETRY replays the same id and cannot make a
   *  second room; it is dropped when the attempt succeeds or the selection
   *  changes, so the next attempt is a new one. */
  groupRequestId: string
  /** The sorted handles `groupRequestId` was minted for. */
  groupRequestKey: string
  /** The failed create was a group's, so RETRY repeats `createGroup`. */
  failedGroup: boolean
}

export const pickerState: PickerState = reactive({
  roster: null,
  loading: false,
  busy: false,
  opening: '',
  error: null,
  failedFor: null,
  selected: [],
  groupName: '',
  groupRequestId: '',
  groupRequestKey: '',
  failedGroup: false,
})

/** The server's bounds on a group (`CreateGroupRequest`): a name of 1..80
 *  characters after trimming. The handles' bound (49) cannot be reached from a
 *  roster this small, but the picker stops there rather than send a refusal. */
export const GROUP_NAME_MAX = 80
export const GROUP_MEMBERS_MAX = 49

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
    res.answered()
    // Someone chosen may have been deactivated since the list was read: they
    // leave the selection, and a failure about the old set has nothing left to
    // retry (the request id is keyed to the set, so a new set mints a new one).
    const still = pickerState.selected.filter((s) => pickerState.roster!.some((r) => r.id === s.id))
    if (still.length !== pickerState.selected.length) {
      pickerState.selected = still
      pickerState.failedGroup = false
    }
    if (pickerState.error === UNREACHABLE) pickerState.error = null
  } catch (e) {
    if (pickerState.roster === null) {
      pickerState.error = e instanceof AuthedUnauthorized
        ? refusalText('unauthorized', 401)
        : 'Could not load the list of people — check your connection and try again.'
    }
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
  pickerState.failedGroup = false
  try {
    const res = await authedRequest('/conversations/direct', {
      method: 'POST',
      json: encodeDirectConversationRequest({ handle: entry.handle }),
    })
    if (res.status !== 200) {
      let code: string | undefined
      try {
        code = decodeServerError({ type: 'error', ...JSON.parse(res.body) }).code
        // A body Catenary wrote is an answer; a proxy's page is not.
        res.answered()
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
    res.answered()
    if (!openConversation(conversation)) pickerState.opening = conversation.id
    else pickerState.opening = ''
    return true
  } catch (e) {
    pickerState.error = e instanceof AuthedUnauthorized ? refusalText('unauthorized', 401) : UNREACHABLE
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

/** Add a person to, or remove them from, the people chosen for a group. The
 *  last failure was about a different set of people, so it goes; the request
 *  id stays and is compared by set at the next create, so un-ticking and
 *  re-ticking someone after a lost answer still replays the same id. */
export function toggleSelected(entry: RosterEntry): void {
  if (pickerState.busy || pickerState.opening !== '') return
  const at = pickerState.selected.findIndex((s) => s.id === entry.id)
  if (at >= 0) pickerState.selected.splice(at, 1)
  else if (pickerState.selected.length < GROUP_MEMBERS_MAX) pickerState.selected.push(entry)
  pickerState.error = null
  pickerState.failedFor = null
  pickerState.failedGroup = false
}

export function clearSelection(): void {
  if (pickerState.busy || pickerState.opening !== '') return
  pickerState.selected = []
  pickerState.error = null
  pickerState.failedGroup = false
}

/** The name field appears only when there is a group to name. */
export const groupNameShown = (): boolean => pickerState.selected.length >= 2

/** The name the server will be sent, or '' when there is none worth sending:
 *  1..80 characters after trimming, counted in code points as the schema does. */
export function trimmedGroupName(): string {
  const n = pickerState.groupName.trim()
  return [...n].length >= 1 && [...n].length <= GROUP_NAME_MAX ? n : ''
}

/** CREATE is offered when two or more are chosen and the name is usable. */
export const canCreateGroup = (): boolean => groupNameShown() && trimmedGroupName() !== ''

/** The group's refusals, in words. 404 `conversation_not_found` is "a handle
 *  names nobody or a deactivated account" (nothing was created); any other 4xx
 *  is a request the picker should not have been able to build (400). */
export function groupRefusalText(code: string | undefined, status: number): string {
  if (code === 'conversation_not_found') {
    return 'The group could not be created — someone chosen may no longer be available. The list has been refreshed; check who is chosen and try again.'
  }
  if (status === 400) {
    return 'The server refused that group — check the name and the people chosen, then try again.'
  }
  if (status === 403) return 'This account cannot start a group.'
  return refusalText(code, status)
}

/**
 * `POST /conversations` for the chosen people, then open what it returns the
 * same way `startDirect` does: at once when the journal holds it, else on the
 * way and opened by showProjection when `/sync` delivers it. The attempt's
 * `request_id` is minted on the first try and REUSED by RETRY, so a create
 * whose answer was lost cannot become two rooms (the server replays it, 200).
 */
export async function createGroup(): Promise<boolean> {
  if (pickerState.busy || pickerState.opening !== '') return false
  if (!canCreateGroup()) return false
  pickerState.busy = true
  pickerState.error = null
  pickerState.failedFor = null
  pickerState.failedGroup = false
  // The id belongs to a SET of people: the same set (however it was reached)
  // replays the same id, a different set is a different group and a new one.
  const key = pickerState.selected.map((s) => s.handle).sort().join(',')
  if (pickerState.groupRequestId === '' || pickerState.groupRequestKey !== key) {
    pickerState.groupRequestId = crypto.randomUUID()
    pickerState.groupRequestKey = key
  }
  try {
    const res = await authedRequest('/conversations', {
      method: 'POST',
      json: encodeCreateGroupRequest({
        name: trimmedGroupName(),
        memberHandles: pickerState.selected.map((s) => s.handle),
        requestId: pickerState.groupRequestId,
      }),
    })
    if (res.status !== 200 && res.status !== 201) {
      let code: string | undefined
      try {
        code = decodeServerError({ type: 'error', ...JSON.parse(res.body) }).code
        res.answered()
      } catch {
        code = undefined
      }
      pickerState.error = groupRefusalText(code, res.status)
      pickerState.failedGroup = true
      if (code === 'conversation_not_found') void loadRoster()
      return false
    }
    const conversation = decodeConversation(JSON.parse(res.body))
    res.answered()
    if (!openConversation(conversation)) pickerState.opening = conversation.id
    else pickerState.opening = ''
    // The attempt is over: the next one is a new one.
    pickerState.groupRequestId = ''
    pickerState.selected = []
    pickerState.groupName = ''
    return true
  } catch (e) {
    pickerState.error = e instanceof AuthedUnauthorized ? refusalText('unauthorized', 401) : UNREACHABLE
    pickerState.failedGroup = true
    return false
  } finally {
    pickerState.busy = false
  }
}

/** RETRY on a failed create: the same person, or the same group with the same
 *  request id. */
export function retryPick(): Promise<boolean> {
  if (pickerState.failedGroup) return createGroup()
  const entry = pickerState.failedFor
  return entry ? startDirect(entry) : Promise.resolve(false)
}

/** Each opening of the picker starts from a clean slate and a fresh roster. */
export function resetPicker(): void {
  pickerState.busy = false
  pickerState.opening = ''
  pickerState.error = null
  pickerState.failedFor = null
  pickerState.failedGroup = false
  pickerState.selected = []
  pickerState.groupName = ''
  pickerState.groupRequestId = ''
  void loadRoster()
}
