import { accountState } from './account'

/** What the sessions view says when a login or re-enrollment stored its
 *  credential and the session over it would not start (CANT-240). Plain about
 *  what the client knows: the device is enrolled; nothing is syncing. */
export const SESSION_NOT_STARTED_TEXT =
  'This device is enrolled, but its session could not start, so nothing is syncing. Try reloading the page.'

export interface RestartSeams {
  endSession: () => void
  /** Resolves the tab's journal, or undefined where none would open. */
  journal: () => Promise<{ wipe(): Promise<unknown> } | undefined>
  start: () => Promise<unknown>
}

/**
 * THE `onEnrolled` LISTENER'S BODY: end the old session, wipe the journal the
 * previous credential left, start over the new pair. It never rejects. A step
 * that throws is logged and leaves `accountState.sessionError` set, and the
 * account view is already open at its sessions list — an unhandled rejection
 * there left no session and said nothing. Any later step added in this chain
 * (CANT-230's claim) falls into the same handler.
 */
export async function restartAfterEnrollment(seams: RestartSeams): Promise<void> {
  accountState.sessionError = null
  try {
    seams.endSession()
    await (await seams.journal())?.wipe()
    await seams.start()
  } catch (e) {
    console.error('catenary: a session could not start after enrollment', e)
    accountState.sessionError = SESSION_NOT_STARTED_TEXT
  }
}
