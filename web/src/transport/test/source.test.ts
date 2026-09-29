/* CANT-35 criteria 0 and 7's greps, over the transport's own source: no Vue,
 * every module names the internal/client file it mirrors, no monotonic clock,
 * and no heartbeat number that did not arrive on `ready`. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readdirSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const DIR = resolve(process.cwd(), 'src', 'transport')
const modules = readdirSync(DIR)
  .filter((f) => f.endsWith('.ts'))
  .map((f) => ({ name: f, src: readFileSync(resolve(DIR, f), 'utf8') }))

/** Source with comments removed, so prose citing a number is not code. */
const code = (src: string) => src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '')

test('criterion 0 · the modules exist, none imports Vue, and each header names what it mirrors', () => {
  const names = modules.map((m) => m.name).sort()
  for (const want of ['transport.ts', 'heartbeat.ts', 'closes.ts', 'backoff.ts', 'catchup.ts', 'journal.ts', 'project.ts', 'credential.ts', 'terminal.ts', 'faults.ts', 'status.ts', 'seams.ts', 'index.ts']) {
    assert.ok(names.includes(want), `${want} exists`)
  }
  for (const m of modules) {
    assert.ok(!/from\s+['"]vue['"]|from\s+['"]@vue\//.test(m.src), `${m.name} imports no Vue`)
    const header = m.src.slice(0, m.src.indexOf('*/'))
    assert.ok(/internal\/client/.test(header), `${m.name}'s header names the internal/client file it mirrors`)
  }
})

test('criterion 0 · the monotonic clock is not used anywhere in the transport', () => {
  for (const m of modules) assert.ok(!/performance\.now/.test(code(m.src)), m.name)
})

test('criterion 7 · no numeric heartbeat constant in the transport', () => {
  for (const m of modules) {
    const c = code(m.src)
    assert.ok(!/\b(35|105)\b/.test(c), `${m.name}: neither default appears as a number`)
    assert.ok(
      !/(heartbeat|interval|pong|ping|missed)\w*\s*[:=]\s*[1-9]\d/i.test(c),
      `${m.name}: nothing heartbeat-named is assigned a number`,
    )
  }
})
