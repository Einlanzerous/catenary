/* CANT-35 criteria 0 and 7's greps, over the transport's own source: no Vue,
 * every module names the internal/client file it mirrors, no monotonic clock,
 * and no heartbeat number that did not arrive on `ready`.
 *
 * And CANT-199's rule: every branch of transport.ts that sees `JournalStale`
 * increments the epoch, and the transport's journal write calls are the three
 * that were looked at — a fourth has to be classified here before it lands.
 * Proved against planted source every run, as dart-client/test/source_test.dart
 * proves its twin: a rule nobody has seen refuse something is a claim about
 * the rule. */

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

/** The `Journal` members the transport calls that write nothing. */
const JOURNAL_READS = new Set(['cursor', 'snapshot', 'holdsConversation', 'holdsUser', 'messageCount', 'headSeqTotal'])

/** Every other `this.journal.` call site in the transport, by member: its
 *  writes. `applyLive` is two sites, the `skipStaleCatchUp` control's and the
 *  guarded one. A `wipe` skips the generation check, so it is never refused
 *  as stale. */
const JOURNAL_WRITES: Record<string, number> = { applyLive: 2, applyPage: 1, wipe: 1 }

function journalWriteCalls(transport: string): Record<string, number> {
  const found: Record<string, number> = {}
  for (const m of code(transport).matchAll(/\bthis\s*\.\s*journal\s*\.\s*([A-Za-z_]\w*)/g)) {
    if (!JOURNAL_READS.has(m[1])) found[m[1]] = (found[m[1]] ?? 0) + 1
  }
  return found
}

/** The block whose `{` is at or after `from`, braces included. */
function blockAt(source: string, from: number): string {
  const open = source.indexOf('{', from)
  let depth = 0
  for (let i = open; i < source.length; i++) {
    if (source[i] === '{') depth++
    if (source[i] === '}' && --depth === 0) return source.slice(open, i + 1)
  }
  throw new Error(`unbalanced braces after offset ${from}`)
}

/** Every `catch` in `transport` whose body names `JournalStale`. */
function staleBranches(transport: string): string[] {
  const c = code(transport)
  return [...c.matchAll(/\bcatch\s*(\([^)]*\))?\s*\{/g)]
    .map((m) => blockAt(c, m.index + m[0].length - 1))
    .filter((b) => b.includes('JournalStale'))
}

/** Why `transport` breaks the rule, or an empty list. */
function staleEpochRule(transport: string): string[] {
  const calls = journalWriteCalls(transport)
  const branches = staleBranches(transport)
  const out: string[] = []
  for (const name of new Set([...Object.keys(calls), ...Object.keys(JOURNAL_WRITES)])) {
    if (calls[name] !== JOURNAL_WRITES[name]) {
      out.push(`this.journal.${name} is called at ${calls[name] ?? 0} site(s), and ${JOURNAL_WRITES[name] ?? 0} were classified: say whether it can be refused as stale`)
    }
  }
  if (branches.length !== 2) out.push(`${branches.length} branches see JournalStale, and two were looked at: the live write's and the page's`)
  for (const b of branches) {
    if (!/\bthis\.epoch\+\+/.test(b)) out.push(`a branch that sees JournalStale does not increment the epoch: ${b.split('\n').slice(0, 2).join(' ').trim()}`)
  }
  return out
}

const TRANSPORT = modules.find((m) => m.name === 'transport.ts')!.src

test('CANT-199 · every branch that sees JournalStale increments the epoch, and the journal writes are the three classified', () => {
  assert.deepEqual(journalWriteCalls(TRANSPORT), JOURNAL_WRITES)
  assert.equal(staleBranches(TRANSPORT).length, 2)
  assert.deepEqual(staleEpochRule(TRANSPORT), [])
})

test('CANT-199 · the stale-epoch rule refuses each thing it names, planted', () => {
  const refusals = (src: string) => staleEpochRule(src).join('\n')

  // The increment taken out of each branch in turn.
  const live = 'if (!this.faults.staleKeepsEpoch) this.epoch++'
  const paged = 'if (e instanceof JournalStale && !this.faults.staleKeepsEpoch) this.epoch++'
  for (const [line, without] of [[live, ''], [paged, "if (e instanceof JournalStale) this.log.warn('stale')"]]) {
    const planted = TRANSPORT.replace(line, without)
    assert.notEqual(planted, TRANSPORT, `the plant landed: ${line}`)
    assert.match(refusals(planted), /does not increment the epoch/, line)
  }

  // The increment in a comment is not an increment.
  assert.match(refusals(TRANSPORT.replace(live, `// ${live}`)), /does not increment the epoch/)

  // A third branch, and a third write path with no branch at all.
  const third = `
  private async third(): Promise<void> {
    try {
      await this.journal.applyPage(p, this.faults)
    } catch (e) {
      if (e instanceof JournalStale) this.catchup.trigger()
    }
  }
`
  const withThird = refusals(TRANSPORT + third)
  assert.match(withThird, /3 branches see JournalStale/)
  assert.match(withThird, /does not increment the epoch/)
  assert.match(withThird, /this\.journal\.applyPage is called at 2 site\(s\)/)
  assert.match(refusals(`${TRANSPORT}\nfunction mark() { this.journal.applyReceipt(r) }\n`), /this\.journal\.applyReceipt is called at 1 site\(s\), and 0 were classified/)
  assert.match(refusals(`${TRANSPORT}\nfunction again() { this.journal.wipe() }\n`), /this\.journal\.wipe is called at 2 site\(s\)/)

  // And a transport with no stale handling at all does not pass by matching nothing.
  assert.notDeepEqual(staleEpochRule('function f() { this.journal.cursor() }'), [])
})
