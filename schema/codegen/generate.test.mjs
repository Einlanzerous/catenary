// The generator's own tests — CANT-74.
//
// Three kinds of claim, none of which a conformance vector can make:
//
//   * CLASSIFICATION. Direction is computed from named roots, and the table in
//     the CANT-74 plan says what today's schema classifies to. `--classify`
//     prints it and this asserts it, so the walk is pinned rather than read.
//   * LINTS. Each rule that fails the build is proved to fail it, on a copy of
//     the real schema mutated in memory and written to a temp file. A lint
//     nobody has watched fire is a comment.
//   * THE SERVER IS CLOSED. Every enum block and every inline `oneOf` guard in
//     the generated Go is byte-identical to origin/main, except the delta this
//     ticket names. The Go emitter is not touched by CANT-74 and this is what
//     proves it, so "closed on the server" is checked by a test rather than by
//     a reader's attention.
//
// Run: node --test schema/codegen   (from the repository root; `npm run gen:test` in web/)

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const ROOT = resolve(HERE, '..', '..')
const GEN = join(HERE, 'generate.mjs')
const SCHEMA = join(ROOT, 'schema', 'catenary.wire.v1.schema.json')

const loadSchema = () => JSON.parse(readFileSync(SCHEMA, 'utf8'))

/** Run the generator against a schema object written to a temp file; return {status, out}. */
function runOn(schema, ...flags) {
  const dir = mkdtempSync(join(tmpdir(), 'cant74-'))
  const path = join(dir, 'schema.json')
  writeFileSync(path, JSON.stringify(schema))
  try {
    const r = spawnSync(process.execPath, [GEN, `--schema=${path}`, ...flags], { encoding: 'utf8' })
    return { status: r.status, out: (r.stdout || '') + (r.stderr || '') }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

const classify = () => JSON.parse(execFileSync(process.execPath, [GEN, '--classify'], { encoding: 'utf8' }))

/* ------------------------------------------------------------------ *
 * Classification — the table in section:approach, asserted.
 * ------------------------------------------------------------------ */

test('today\'s schema classifies exactly as the CANT-74 plan says', () => {
  const c = classify()
  assert.deepEqual(c.serverRoots, ['ServerFrame', 'SyncResponse', 'EnrollResponse', 'RefreshResponse'])
  assert.deepEqual(c.clientRoots, ['ClientFrame', 'EnrollRequest', 'RefreshRequest'])
  // Every unreferenced $defs entry is a root, and every root is unreferenced.
  assert.deepEqual([...c.unreferenced].sort(), [...c.serverRoots, ...c.clientRoots].sort())
  assert.deepEqual([...c.clientOpen].sort(),
    ['ConversationKind', 'DeliveryState', 'ErrorCode', 'ReplyRefKind', 'ResyncReason', 'TranscriptState'])
  assert.deepEqual(c.closedEverywhere, ['TypingState'])
  // Seven non-enum types are reachable from both sides today. That is normal,
  // stays legal, and neither rule touches them.
  assert.deepEqual([...c.bothSides].sort(), ['LogSeq', 'Ping', 'Pong', 'Seq', 'Timestamp', 'Token', 'Uuid'])
})

test('the real schema passes every lint and every emitter', () => {
  const r = runOn(loadSchema(), '--dry-run')
  assert.equal(r.status, 0, r.out)
})

/* ------------------------------------------------------------------ *
 * Lints — each one watched firing.
 * ------------------------------------------------------------------ */

test('an unreferenced $defs entry in neither root list fails, naming it', () => {
  const s = loadSchema()
  s.$defs.OrphanBody = { type: 'object', additionalProperties: false, properties: { id: { $ref: '#/$defs/Uuid' } } }
  const r = runOn(s, '--dry-run')
  assert.notEqual(r.status, 0)
  assert.match(r.out, /\$defs\.OrphanBody is referenced by nothing and is in neither root list/)
})

test('an enum reachable from both a server root and a client root fails, naming the enum and both paths', () => {
  const s = loadSchema()
  // TypingState is client-only today; hang it off a server frame too.
  s.$defs.ServerTyping.properties.state = { $ref: '#/$defs/TypingState' }
  const r = runOn(s, '--dry-run')
  assert.notEqual(r.status, 0)
  assert.match(r.out, /\$defs\.TypingState: an enum reachable from both sides/)
  assert.match(r.out, /from server root ServerFrame via ServerFrame > ServerTyping > TypingState/)
  assert.match(r.out, /from client root ClientFrame via ClientFrame > ClientTyping > TypingState/)
})

test('a NON-enum type reachable from both sides passes', () => {
  const s = loadSchema()
  // A fresh alias used by a client frame and a server frame alike.
  s.$defs.Nonce = { type: 'string', minLength: 1 }
  s.$defs.ClientRead.properties.nonce = { $ref: '#/$defs/Nonce' }
  s.$defs.ServerAck.properties.nonce = { $ref: '#/$defs/Nonce' }
  const r = runOn(s, '--dry-run')
  assert.equal(r.status, 0, r.out)
})

test('an enum that defines the reserved spelling "unknown" fails, naming the enum', () => {
  const s = loadSchema()
  s.$defs.TranscriptState.enum.push('unknown')
  const r = runOn(s, '--dry-run')
  assert.notEqual(r.status, 0)
  assert.match(r.out, /\$defs\.TranscriptState: "unknown" is a reserved spelling/)
})

test('an inline enum on a server-emitted type fails, naming the field, with no allow-list', () => {
  const s = loadSchema()
  s.$defs.ServerAck.properties.outcome = { type: 'string', enum: ['stored', 'deduplicated'] }
  const r = runOn(s, '--dry-run')
  assert.notEqual(r.status, 0)
  assert.match(r.out, /ServerAck\.outcome: an inline enum on a server-emitted type/)
  // The only inline enum left is on a client-authored type, and it is not
  // allow-listed — it passes because the rule does not reach it.
  assert.equal(s.$defs.OutboundAttachment.properties.kind.enum.length, 2)
  assert.doesNotMatch(readFileSync(GEN, 'utf8'), /allowList|allow-list|ALLOW_LIST/)
})

/* ------------------------------------------------------------------ *
 * The emitted OpenAPI carries the classification and nothing else does.
 * ------------------------------------------------------------------ */

test('x-catenary-client-open marks every client-open enum and no closed one', () => {
  const yaml = readFileSync(join(ROOT, 'schema', 'openapi.yaml'), 'utf8')
  const c = classify()
  // Each component is `    Name:` at four spaces; the marker sits inside it.
  const marked = []
  let current = null
  for (const line of yaml.split('\n')) {
    const m = /^    ([A-Za-z]+):$/.exec(line)
    if (m) current = m[1]
    if (/^      x-catenary-client-open: true$/.test(line)) marked.push(current)
  }
  assert.deepEqual(marked.sort(), [...c.clientOpen].sort())
})

/* ------------------------------------------------------------------ *
 * The server is closed: Go enum blocks and inline guards vs origin/main.
 * ------------------------------------------------------------------ */

const GO = 'internal/wire/generated.go'

/** Enum blocks keyed by type name: `type X string` through the end of `checkX`. */
function goEnumBlocks(src) {
  const out = new Map()
  const re = /^type (\w+) string\n\nconst \([\s\S]*?\nfunc check\1\([^)]*\) error \{[\s\S]*?\n\}\n/gm
  for (const m of src.matchAll(re)) out.set(m[1], m[0])
  return out
}

/** Inline enum guards: an `if !oneOf(` through its closing brace, keyed by their text. */
function goInlineGuards(src) {
  const out = new Set()
  for (const m of src.matchAll(/^\tif !oneOf\([\s\S]*?\n\t\}\n/gm)) out.add(m[0])
  return out
}

/** origin/main's generated.go, or null when no remote can be reached — a
 *  shallow checkout is fetched, a clone with no origin or no network is not. */
function mainGo() {
  const show = () => execFileSync('git', ['show', `origin/main:${GO}`], { cwd: ROOT, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] })
  try { return show() } catch { /* shallow checkout — fetch just main */ }
  try {
    execFileSync('git', ['fetch', '--depth=1', 'origin', 'main'], { cwd: ROOT, stdio: 'ignore' })
    return show()
  } catch {
    return null
  }
}

test('Go enum blocks and inline oneOf guards are byte-identical to origin/main, except the CANT-74 delta', (t) => {
  const head = readFileSync(join(ROOT, GO), 'utf8')
  const base = mainGo()
  if (base === null) {
    // A skip is not a pass. verify.sh treats an absent world the same way
    // (no Dart SDK, no database): say so loudly rather than go red on a
    // clone with no origin, and let CI — which always has one — make the claim.
    t.skip('SKIPPED, NOT PROVED: origin/main is unreachable, so "the Go enum blocks are unchanged" was not checked here')
    return
  }
  const hb = goEnumBlocks(head)
  const bb = goEnumBlocks(base)
  assert.ok(hb.size >= 6, `found only ${hb.size} enum blocks at HEAD`)

  // The delta CANT-74 names, and nothing else. Once this branch is on main the
  // sets below are simply empty differences, and any LATER change to what the
  // Go emitter produces for an enum fails here until a person names it.
  const ALLOWED_ADDED = new Set(['ResyncReason'])
  const ALLOWED_REMOVED_GUARDS = ['"cursor_too_old", "membership_changed", "retention_purge"']

  for (const [name, block] of bb) {
    assert.ok(hb.has(name), `enum block ${name} is on main and missing at HEAD`)
    assert.equal(hb.get(name), block, `enum block ${name} differs from main`)
  }
  for (const name of hb.keys()) {
    if (!bb.has(name)) assert.ok(ALLOWED_ADDED.has(name), `enum block ${name} was added and is not in the CANT-74 delta`)
  }

  const hg = goInlineGuards(head)
  const bg = goInlineGuards(base)
  for (const g of bg) {
    if (hg.has(g)) continue
    assert.ok(ALLOWED_REMOVED_GUARDS.some((k) => g.includes(k)), `an inline guard left main and is not in the CANT-74 delta:\n${g}`)
  }
  for (const g of hg) assert.ok(bg.has(g), `an inline guard appeared at HEAD that main does not have:\n${g}`)
  // What is left: exactly one inline guard, on the client-authored OutboundAttachment.kind.
  assert.equal(hg.size, 1)
  assert.match([...hg][0], /"voice", "image"/)
})
