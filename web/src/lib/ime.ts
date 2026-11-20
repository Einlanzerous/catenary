/* CANT-371: is this keydown the user's own "send"?
 *
 * Enter that commits an IME composition (or an Android keyboard's suggestion)
 * is a keydown too, and sending on it ships the message half-written.
 * `isComposing` marks it, except that Firefox on some versions reports false
 * on the composition-ending Enter; those still carry the legacy keyCode 229,
 * so the 229 check stays. Every Enter-submit handler goes through here. */
export function isPlainEnter(event: Pick<KeyboardEvent, 'key' | 'shiftKey' | 'isComposing' | 'keyCode'>): boolean {
  if (event.isComposing || event.keyCode === 229) return false
  return event.key === 'Enter' && !event.shiftKey
}
