/* When a client must stop — mirrors internal/client/terminal.go (CANT-123).
 *
 * CANT-31's record §6 (docs/decisions/cant-31-refresh-and-terminal-reconnect.md).
 * Two terminal states that end differently and tell the person different
 * things. NEITHER ENDS ON A TIMER OR ON A NETWORK CHANGE, and NOTHING IS
 * DELETED: the credential and the journal both survive, so a false terminal is
 * recovered by a relaunch — a new Transport over the same stores — and a true
 * one loses nothing, because the server refuses that credential for ever anyway.
 * Where this file and the record disagree, fix the record first.
 */

/**
 * `none`: running, or stopped for any other reason, however long the backoff
 * has grown. `credential`: Catenary itself refused the stored refresh token
 * (§5; entered by the credential layer, CANT-152). `protocol`: close `4001`, or
 * a `1008` that §4 makes permanent; it ends on relaunch.
 */
export type TerminalKind = 'none' | 'credential' | 'protocol'

export interface Terminal {
  kind: TerminalKind
  /** The observation that put the client there. Never carries a token. */
  reason: string
}

export const NOT_TERMINAL: Readonly<Terminal> = Object.freeze({ kind: 'none', reason: '' })
