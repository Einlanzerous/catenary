/* `npm run test:ime` — Enter during IME composition does not send (CANT-371).
 *
 * Covers the guard (isPlainEnter) and that the Composer's keydown handler
 * returns on a composing event before it can reach send(). */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { isPlainEnter } from './src/lib/ime'

const ev = (o: Partial<Pick<KeyboardEvent, 'key' | 'shiftKey' | 'isComposing' | 'keyCode'>>) => ({
  key: 'Enter',
  shiftKey: false,
  isComposing: false,
  keyCode: 13,
  ...o,
})

test('a plain Enter sends', () => assert.equal(isPlainEnter(ev({})), true))
test('Shift+Enter is a newline, not a send', () => assert.equal(isPlainEnter(ev({ shiftKey: true })), false))
test('Enter while isComposing does not send', () => assert.equal(isPlainEnter(ev({ isComposing: true })), false))
test('Enter with keyCode 229 does not send, even when isComposing is false (Firefox)', () =>
  assert.equal(isPlainEnter(ev({ isComposing: false, keyCode: 229 })), false))
test('other keys do not send', () => assert.equal(isPlainEnter(ev({ key: 'a' })), false))

test('the Composer asks the guard and returns before send() on a composing keydown', () => {
  const src = readFileSync(resolve(process.cwd(), 'src/components/Composer.vue'), 'utf8')
  const handler = src.slice(src.indexOf('function onKeydown'), src.indexOf('function onRecordKeydown'))
  assert.match(handler, /isPlainEnter\(event\)/)
  assert.ok(
    handler.indexOf('keyCode === 229') !== -1 && handler.indexOf('keyCode === 229') < handler.indexOf('send()'),
    'the composition return must come before send()',
  )
})
