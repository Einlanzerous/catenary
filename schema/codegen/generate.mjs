#!/usr/bin/env node
/* Catenary wire codegen — IDEA-27 (R4).
 *
 * Reads catenary.wire.v1.schema.json and emits congruent types plus JSON
 * codecs for TypeScript (Vue client), Dart (Flutter client) and Go (server).
 *
 * WHY THIS EXISTS RATHER THAN AN OFF-THE-SHELF GENERATOR
 *
 * Measured, not assumed — see FINDINGS.md for the runs:
 *
 *   * quicktype (the only tool that targets both Dart and TS from JSON Schema)
 *     unifies `oneOf` branches instead of emitting a tagged union. Our fifteen
 *     frame types came out as ONE class with 30 fields, 28 of them nullable,
 *     in both languages. The envelope union is the entire protocol; a generator
 *     that erases it is worse than useless, because the erasure is silent.
 *   * json-schema-to-typescript is genuinely good — real discriminated unions,
 *     every type name preserved — but it is TypeScript-only and types-only.
 *
 * Pairing a good TS generator with a bad Dart one produces two type systems of
 * different SHAPE from one schema, which is the drift R4 exists to prevent,
 * arrived at more expensively. One generator walking the schema once is the
 * only way the two clients are structurally guaranteed to agree.
 *
 * The tradeoff is honest: this file is ours to maintain. It stays cheap because
 * it supports exactly the JSON Schema subset the protocol uses and rejects
 * anything else loudly, rather than growing toward being a general tool.
 */

import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { dirname, resolve, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const ROOT = resolve(HERE, '..', '..')
/* `--schema=<path>` points the whole walk at another schema. It exists for the
 * generator's own tests, which mutate the real schema in memory, write it to a
 * temp file and assert that a lint fires — never for producing the tree. */
const argSchema = process.argv.find((a) => a.startsWith('--schema='))
const SCHEMA = argSchema
  ? resolve(argSchema.slice('--schema='.length))
  : resolve(HERE, '..', 'catenary.wire.v1.schema.json')

const schema = JSON.parse(readFileSync(SCHEMA, 'utf8'))
const defs = schema.$defs
const WIRE_VERSION = schema['x-wire-version']

/* ------------------------------------------------------------------ *
 * IR
 * ------------------------------------------------------------------ */

const fail = (msg) => {
  console.error(`codegen: ${msg}`)
  process.exit(1)
}

const refName = (r) => {
  const m = /^#\/\$defs\/(.+)$/.exec(r)
  if (!m) fail(`unsupported $ref ${r} — only #/$defs/Name is understood`)
  return m[1]
}

/** Classify one $defs entry. The four shapes are all the protocol uses. */
function classify(name, node) {
  if (node.oneOf) return { kind: 'union', name, node }
  if (node.enum) return { kind: 'enum', name, node }
  if (node.type === 'object') return { kind: 'object', name, node }
  if (node.type === 'string' || node.type === 'integer' || node.type === 'number' || node.type === 'boolean') {
    return { kind: 'alias', name, node }
  }
  fail(`$defs.${name}: unsupported shape (no oneOf, enum, object or scalar type)`)
}

const model = Object.entries(defs).map(([name, node]) => classify(name, node))
const byName = new Map(model.map((d) => [d.name, d]))

/* ------------------------------------------------------------------ *
 * Direction — CANT-74.
 *
 * Every enum is either CLIENT-OPEN or CLOSED EVERYWHERE, and which is decided
 * by where it can be reached from, not by a hand-kept list. An enum reachable
 * from a server root is something a client can RECEIVE, so a value this schema
 * version does not know must not cost the client the frame: TypeScript and
 * Dart decode it to the sentinel `unknown`. An enum reachable only from client
 * roots is something a client AUTHORS and never has to decode ahead of itself,
 * so it stays closed on every side — which is also what stops a client from
 * typing `unknown` into an outbound frame and having it typecheck. Go is
 * closed on everything: the server is the trust boundary.
 *
 * Two rules keep the root lists honest. A `$defs` entry referenced by nothing
 * must be in exactly one of them, so a new REST body is classified by the
 * person adding it. An enum reachable from BOTH sides fails the build, so the
 * first one is split by a person rather than decided by whichever walk ran
 * last. Non-enum types shared by both sides — Uuid, Timestamp, the ordinals,
 * Token, Ping, Pong — are normal and neither rule touches them.
 * ------------------------------------------------------------------ */
const SERVER_ROOTS = ['ServerFrame', 'SyncResponse', 'EnrollResponse', 'RefreshResponse']
const CLIENT_ROOTS = ['ClientFrame', 'EnrollRequest', 'RefreshRequest']

/** Every $defs name a node references, at any depth. */
function refsIn(node, out = new Set()) {
  if (Array.isArray(node)) { for (const x of node) refsIn(x, out); return out }
  if (node && typeof node === 'object') {
    for (const [k, v] of Object.entries(node)) {
      if (k === '$ref' && typeof v === 'string') out.add(refName(v))
      else refsIn(v, out)
    }
  }
  return out
}

/** Reachability from a root list: name -> the path that reached it, root first. */
function reach(roots) {
  const paths = new Map()
  const queue = []
  for (const r of roots) {
    if (!defs[r]) fail(`root ${r} is not a $defs entry`)
    paths.set(r, [r])
    queue.push(r)
  }
  while (queue.length) {
    const n = queue.shift()
    for (const m of refsIn(defs[n])) {
      if (paths.has(m)) continue
      paths.set(m, [...paths.get(n), m])
      queue.push(m)
    }
  }
  return paths
}

const referenced = new Set()
for (const node of Object.values(defs)) for (const r of refsIn(node)) referenced.add(r)
for (const name of Object.keys(defs)) {
  const s = SERVER_ROOTS.includes(name)
  const c = CLIENT_ROOTS.includes(name)
  if (s && c) fail(`$defs.${name} is listed as both a server root and a client root`)
  if (!referenced.has(name) && !s && !c) {
    fail(`$defs.${name} is referenced by nothing and is in neither root list — say which side it is on: ` +
      'SERVER_ROOTS if the server emits it, CLIENT_ROOTS if a client authors it (CANT-74)')
  }
}
const serverReach = reach(SERVER_ROOTS)
const clientReach = reach(CLIENT_ROOTS)

/** Names of the enums a client can receive. Everything else is closed everywhere. */
const clientOpen = new Set()
for (const d of model) {
  if (d.kind !== 'enum') continue
  if (d.node.enum.includes('unknown')) {
    fail(`$defs.${d.name}: "unknown" is a reserved spelling — it is the sentinel a client decodes an ` +
      'unrecognised value to, so no enum may define it as a real value (CANT-74)')
  }
  const s = serverReach.get(d.name)
  const c = clientReach.get(d.name)
  if (s && c) {
    fail(`$defs.${d.name}: an enum reachable from both sides — from server root ${s[0]} via ` +
      `${s.join(' > ')}, and from client root ${c[0]} via ${c.join(' > ')}. One enum cannot be open on ` +
      'decode and closed on encode; split it into a receive-side type and an author-side type (CANT-74)')
  }
  if (s) clientOpen.add(d.name)
}
/* An inline enum has no name to hang a sentinel on: it would be a literal
 * union in TypeScript and a bare String in Dart, and the two clients would
 * give different answers on the same vector. So on anything a client can
 * receive, an enum is a named $defs entry or it is a build failure. */
for (const [name, node] of Object.entries(defs)) {
  if (!serverReach.has(name)) continue
  for (const [wire, prop] of Object.entries(node.properties || {})) {
    const inline = (prop.enum && prop.const === undefined) || (prop.items && prop.items.enum)
    if (inline) {
      fail(`${name}.${wire}: an inline enum on a server-emitted type (reachable via ` +
        `${serverReach.get(name).join(' > ')}). Name it in $defs so all three languages carry the same ` +
        'open type (CANT-74)')
    }
  }
}

/** The classification, as a comment for the generated file that carries it. */
function openness(name) {
  if (clientOpen.has(name)) {
    const s = serverReach.get(name)
    return `CLIENT-OPEN (CANT-74): reachable from server root ${s[0]} via ${s.join(' > ')}. A value this ` +
      'schema version does not know decodes to the sentinel `unknown` and is reported once; the server ' +
      'refuses it. Every switch over this type needs an arm for `unknown`.'
  }
  const c = clientReach.get(name)
  return `CLOSED EVERYWHERE (CANT-74): reachable only from client root ${c ? c[0] : '(none)'}` +
    `${c ? ' via ' + c.join(' > ') : ''}. A client authors it and never decodes a value newer than ` +
    'itself, so an unrecognised value is refused on every side.'
}

if (process.argv.includes('--classify')) {
  const enums = model.filter((d) => d.kind === 'enum').map((d) => d.name)
  const both = Object.keys(defs).filter((n) => serverReach.has(n) && clientReach.has(n))
  console.log(JSON.stringify({
    serverRoots: SERVER_ROOTS,
    clientRoots: CLIENT_ROOTS,
    unreferenced: Object.keys(defs).filter((n) => !referenced.has(n)),
    clientOpen: enums.filter((n) => clientOpen.has(n)),
    closedEverywhere: enums.filter((n) => !clientOpen.has(n)),
    bothSides: both,
  }, null, 2))
  process.exit(0)
}

/** Resolve a property schema to a type reference. */
function typeRef(prop, ctx) {
  if (prop.$ref) return { kind: 'named', name: refName(prop.$ref) }
  if (prop.const !== undefined) return { kind: 'const', value: prop.const }
  if (prop.type === 'array') return { kind: 'list', item: typeRef(prop.items, ctx) }
  if (prop.enum) return { kind: 'inlineEnum', values: prop.enum }
  if (prop.type) return { kind: 'prim', prim: prop.type }
  fail(`${ctx}: cannot resolve type`)
}

/** Fields of an object def, in schema declaration order. */
function fieldsOf(def) {
  const req = new Set(def.node.required || [])
  return Object.entries(def.node.properties || {}).map(([wire, prop]) => ({
    wire,
    ref: typeRef(prop, `${def.name}.${wire}`),
    required: req.has(wire),
    doc: prop.description || '',
  }))
}

/** The const-valued property that tags a union member, if any. */
function discriminantOf(def) {
  for (const [wire, prop] of Object.entries(def.node.properties || {})) {
    if (prop.const !== undefined) return { wire, value: prop.const }
  }
  return null
}

/** True when a type reference points at a discriminated union. */
const isUnionRef = (ref) => ref.kind === 'named' && byName.get(ref.name)?.kind === 'union'

/** True when any field anywhere is a floating-point number. */
const usesFloat = () => JSON.stringify(defs).includes('"type": "number"') ||
  JSON.stringify(defs).includes('"type":"number"')

const camel = (s) => s.replace(/_([a-z0-9])/g, (_, c) => c.toUpperCase())
const pascal = (s) => {
  const c = camel(s)
  return c.charAt(0).toUpperCase() + c.slice(1)
}
/* Go initialisms, so the output passes a linter rather than fighting one. */
const GO_INIT = { Id: 'ID', Url: 'URL', Uuid: 'UUID', Eta: 'ETA' }
const goName = (s) =>
  pascal(s).replace(/(Id|Url|Uuid|Eta)(?=[A-Z]|$)/g, (m) => GO_INIT[m] || m)

const enumConst = (v) => pascal(v.replace(/[^a-zA-Z0-9]+/g, '_'))

/* All three targets happen to use // for line comments, so the banner needs no
 * per-language handling. The first line is the form `gofmt`, most linters and
 * most code-review tools recognise as "generated". */
const BANNER = [
  '// Code generated from schema/catenary.wire.v1.schema.json. DO NOT EDIT.',
  '//',
  `// Regenerate with \`npm run gen\` from web/. Wire version ${WIRE_VERSION}.`,
  '// Editing this file by hand makes it a hand-written file with extra steps,',
  '// and `npm run gen:check` will fail in CI the moment you do.',
  '// See IDEA-27 (R4).',
].join('\n')

/** Wrap a doc comment to a width, with a per-language prefix. */
function doc(text, prefix, indent = '') {
  if (!text) return []
  const out = []
  for (const para of String(text).split('\n')) {
    if (!para.trim()) {
      out.push(`${indent}${prefix}`)
      continue
    }
    let line = ''
    for (const w of para.split(/\s+/)) {
      if (line && (line + ' ' + w).length > 84) {
        out.push(`${indent}${prefix} ${line}`)
        line = w
      } else line = line ? line + ' ' + w : w
    }
    if (line) out.push(`${indent}${prefix} ${line}`)
  }
  return out
}

/* ================================================================== *
 * TypeScript
 * ================================================================== */

function tsType(ref) {
  switch (ref.kind) {
    case 'named': return ref.name
    case 'list': return `${tsType(ref.item)}[]`
    case 'const': return JSON.stringify(ref.value)
    case 'inlineEnum': return ref.values.map((v) => JSON.stringify(v)).join(' | ')
    case 'prim':
      return ref.prim === 'integer' || ref.prim === 'number' ? 'number'
        : ref.prim === 'boolean' ? 'boolean' : 'string'
  }
}

/* Error paths are emitted as INTERPOLATING string literals in both languages —
 * a backtick template in TS, a single-quoted string in Dart, both carrying the
 * runtime `p` prefix and the live array index. A static path would name the
 * type but not the frame, which is the half you need to find the bad producer. */
const tsPath = (p) => '`' + p + '`'
const dartPath = (p) => "'" + p + "'"

/** Expression decoding wire value `src` into the TS shape for `ref`. */
function tsDecode(ref, src, path, depth = 0) {
  switch (ref.kind) {
    case 'named': {
      const d = byName.get(ref.name)
      if (d.kind === 'alias') return `as${d.name}(${src}, ${tsPath(path)})`
      if (d.kind === 'enum') return `as${d.name}(${src}, ${tsPath(path)})`
      return `decode${d.name}(${src}, ${tsPath(path)})`
    }
    case 'list': {
      const iv = `i${depth || ''}`
      const itemPath = path + '[${' + iv + '}]'
      const mapped = `asArray(${src}, ${tsPath(path)}).map((x, ${iv}) => ${tsDecode(ref.item, 'x', itemPath, depth + 1)})`
      /* A union decodes to null on an unrecognised tag — "ignore this element",
       * not "fail the message". Dropping the nulls here is what makes that
       * promise true for a list, and it is the reason the item type stays
       * non-nullable for every consumer downstream. */
      if (isUnionRef(ref.item)) {
        const t = tsType(ref.item)
        return `${mapped}.filter((x): x is ${t} => x !== null)`
      }
      return mapped
    }
    case 'const': return JSON.stringify(ref.value)
    case 'inlineEnum': return `asOneOf(${src}, ${JSON.stringify(ref.values)}, ${tsPath(path)})`
    case 'prim':
      return ref.prim === 'integer' ? `asInt(${src}, ${tsPath(path)})`
        : ref.prim === 'number' ? `asNum(${src}, ${tsPath(path)})`
        : ref.prim === 'boolean' ? `asBool(${src}, ${tsPath(path)})`
        : `asStr(${src}, ${tsPath(path)})`
  }
}

function tsEncode(ref, src) {
  switch (ref.kind) {
    case 'named': {
      const d = byName.get(ref.name)
      if (d.kind === 'alias' || d.kind === 'enum') return src
      return `encode${d.name}(${src})`
    }
    case 'list': return `${src}.map((x) => ${tsEncode(ref.item, 'x')})`
    default: return src
  }
}

function emitTS() {
  const L = []
  L.push(BANNER, '')
  L.push('/* eslint-disable */', '')
  L.push(`export const WIRE_VERSION = ${WIRE_VERSION}`, '')
  L.push(...doc(
    'Thrown when a frame does not match the schema. Carries the JSON path so a ' +
    'malformed field is identifiable from a log line rather than requiring a repro.',
    '//'
  ))
  L.push(
    'export class WireFormatError extends Error {',
    '  constructor(public readonly path: string, message: string) {',
    '    super(`${path}: ${message}`)',
    "    this.name = 'WireFormatError'",
    '  }',
    '}',
    ''
  )
  L.push(
    'const bad = (p: string, m: string): never => { throw new WireFormatError(p, m) }',
    "const asStr = (v: unknown, p: string): string => typeof v === 'string' ? v : bad(p, `expected string, got ${typeof v}`)",
    "const asBool = (v: unknown, p: string): boolean => typeof v === 'boolean' ? v : bad(p, `expected boolean, got ${typeof v}`)",
    "const asNum = (v: unknown, p: string): number => typeof v === 'number' && Number.isFinite(v) ? v : bad(p, `expected number, got ${typeof v}`)",
    'const asInt = (v: unknown, p: string): number => {',
    '  const n = asNum(v, p)',
    '  if (!Number.isInteger(n)) bad(p, `expected integer, got ${n}`)',
    '  // The schema caps every ordinal at 2^53-1 precisely so this cannot bite.',
    '  if (!Number.isSafeInteger(n)) bad(p, `integer ${n} exceeds the safe range and has already lost precision`)',
    '  return n',
    '}',
    "const asArray = (v: unknown, p: string): unknown[] => Array.isArray(v) ? v : bad(p, `expected array, got ${typeof v}`)",
    "const asObj = (v: unknown, p: string): Record<string, unknown> => (typeof v === 'object' && v !== null && !Array.isArray(v)) ? v as Record<string, unknown> : bad(p, `expected object, got ${v === null ? 'null' : typeof v}`)",
    'const asOneOf = <T extends string>(v: unknown, allowed: readonly T[], p: string): T => {',
    '  const s = asStr(v, p)',
    '  return (allowed as readonly string[]).includes(s) ? s as T : bad(p, `expected one of ${allowed.join(\'|\')}, got ${JSON.stringify(s)}`)',
    '}',
    '',
    '/* CANT-74: an enum a client can RECEIVE is open on the clients. A value this schema',
    ' * version does not know decodes to the sentinel "unknown" and is reported ONCE per',
    ' * (enum, raw) per process — a busy thread would otherwise print it hundreds of times. */',
    'let serverWireVersion: number | undefined',
    '/** Call on `ready` so the report can say how far apart the two ends are. */',
    'export function setServerWireVersion(v: number | undefined): void { serverWireVersion = v }',
    'let onUnknownWireValue: (message: string) => void = (m) => console.warn(m)',
    '/** Where the report goes. Defaults to console.warn; an app or a test may replace it. */',
    'export function setOnUnknownWireValue(fn: (message: string) => void): void { onUnknownWireValue = fn }',
    'const warned = new Set<string>()',
    'function warnUnknown(enumName: string, raw: string): void {',
    '  const key = `${enumName}\\u0000${raw}`',
    '  if (warned.has(key)) return',
    '  warned.add(key)',
    "  const server = serverWireVersion === undefined ? '' : `, server wire_version ${serverWireVersion}`",
    '  onUnknownWireValue(`wire: ${enumName}: unknown value ${JSON.stringify(raw)} decoded as unknown (client wire_version ${WIRE_VERSION}${server})`)',
    '}',
    '/** Exhaustiveness. `default: assertNever(v)` makes the compiler demand an arm for every',
    ' *  member of a client-open enum, the sentinel included. */',
    'export function assertNever(v: never): never { throw new Error(`unreachable: ${JSON.stringify(v)}`) }',
    '',
    '/** Drops undefined so an absent optional is omitted rather than emitted as null. */',
    'const compact = <T extends Record<string, unknown>>(o: T): T => {',
    '  for (const k of Object.keys(o)) if (o[k] === undefined) delete o[k]',
    '  return o',
    '}',
    ''
  )

  for (const d of model) {
    if (d.kind === 'alias') {
      L.push(...doc(d.node.description, '//'))
      const t = d.node.type === 'integer' || d.node.type === 'number' ? 'number'
        : d.node.type === 'boolean' ? 'boolean' : 'string'
      L.push(`export type ${d.name} = ${t}`)
      /* The constraints are the point. A schema that documents `Timestamp` as
       * millisecond-precision UTC and then accepts anything string-shaped has
       * not pinned the format, it has described it — and the two clients will
       * discover the difference by sorting a conversation differently. */
      const n = d.node
      const body = [`  const x = ${d.node.type === 'integer' ? 'asInt' : d.node.type === 'number' ? 'asNum' : 'asStr'}(v, p)`]
      if (n.pattern) body.push(`  if (!${d.name}Pattern.test(x)) bad(p, \`${d.name} must match ${n.pattern.replace(/`/g, '\\`')}, got \${JSON.stringify(x)}\`)`)
      if (n.minimum !== undefined) body.push(`  if (x < ${n.minimum}) bad(p, \`${d.name} must be >= ${n.minimum}, got \${x}\`)`)
      if (n.maximum !== undefined) body.push(`  if (x > ${n.maximum}) bad(p, \`${d.name} must be <= ${n.maximum}, got \${x}\`)`)
      body.push('  return x')
      if (n.pattern) L.push(`const ${d.name}Pattern = /${n.pattern}/`)
      L.push(`const as${d.name} = (v: unknown, p: string): ${d.name} => {`, ...body, '}', '')
    } else if (d.kind === 'enum') {
      L.push(...doc(d.node.description, '//'))
      L.push(...doc(openness(d.name), '//'))
      const vals = d.node.enum.map((v) => JSON.stringify(v))
      if (clientOpen.has(d.name)) {
        L.push(`export type ${d.name} = ${[...vals, '"unknown"'].join(' | ')}`)
        L.push('/** The wire set — the values this schema version defines. Never the sentinel. */')
        L.push(`export const ${d.name}Values = [${vals.join(', ')}] as const`)
        L.push(`const as${d.name} = (v: unknown, p: string): ${d.name} => {`)
        L.push('  const s = asStr(v, p)')
        L.push(`  if ((${d.name}Values as readonly string[]).includes(s)) return s as ${d.name}`)
        L.push(`  warnUnknown(${JSON.stringify(d.name)}, s)`)
        L.push('  return "unknown"')
        L.push('}', '')
      } else {
        L.push(`export type ${d.name} = ${vals.join(' | ')}`)
        L.push(`export const ${d.name}Values = [${vals.join(', ')}] as const`)
        L.push(`const as${d.name} = (v: unknown, p: string): ${d.name} => asOneOf(v, ${d.name}Values, p)`, '')
      }
    }
  }

  for (const d of model) {
    if (d.kind !== 'object') continue
    const fs = fieldsOf(d)
    L.push(...doc(d.node.description, '//'))
    L.push(`export interface ${d.name} {`)
    for (const f of fs) {
      if (f.ref.kind === 'const') {
        L.push(...doc(f.doc, '  //'))
        L.push(`  readonly ${camel(f.wire)}: ${tsType(f.ref)}`)
        continue
      }
      L.push(...doc(f.doc, '  //'))
      L.push(`  ${camel(f.wire)}${f.required ? '' : '?'}: ${tsType(f.ref)}`)
    }
    L.push('}', '')

    L.push(`export function decode${d.name}(v: unknown, p = ${JSON.stringify(d.name)}): ${d.name} {`)
    L.push('  const o = asObj(v, p)')
    L.push(`  return {`)
    for (const f of fs) {
      const key = JSON.stringify(f.wire)
      const acc = `o[${key}]`
      const sub = `\${p}.${f.wire}`
      if (f.ref.kind === 'const') {
        L.push(`    ${camel(f.wire)}: ${JSON.stringify(f.ref.value)},`)
      } else if (f.required) {
        L.push(`    ${camel(f.wire)}: ${acc} === undefined || ${acc} === null ? bad(\`${sub}\`, 'required field is missing') : ${tsDecode(f.ref, acc, '${p}.' + f.wire)},`)
      } else {
        L.push(`    ${camel(f.wire)}: ${acc} === undefined || ${acc} === null ? undefined : ${tsDecode(f.ref, acc, '${p}.' + f.wire)},`)
      }
    }
    L.push('  }')
    L.push('}', '')

    L.push(`export function encode${d.name}(v: ${d.name}): Record<string, unknown> {`)
    L.push('  return compact({')
    for (const f of fs) {
      const key = JSON.stringify(f.wire)
      if (f.ref.kind === 'const') {
        L.push(`    ${key}: ${JSON.stringify(f.ref.value)},`)
      } else if (f.required) {
        L.push(`    ${key}: ${tsEncode(f.ref, `v.${camel(f.wire)}`)},`)
      } else {
        L.push(`    ${key}: v.${camel(f.wire)} === undefined ? undefined : ${tsEncode(f.ref, `v.${camel(f.wire)}`)},`)
      }
    }
    L.push('  })')
    L.push('}', '')
  }

  for (const d of model) {
    if (d.kind !== 'union') continue
    const members = d.node.oneOf.map((m) => byName.get(refName(m.$ref)))
    const disc = d.node.discriminator.propertyName
    L.push(...doc(d.node.description, '//'))
    L.push(`export type ${d.name} = ${members.map((m) => m.name).join(' | ')}`, '')
    L.push(...doc(
      `Decode a ${d.name}. Returns null for an unrecognised ${JSON.stringify(disc)}, which callers ` +
      'MUST treat as "ignore this frame and carry on" rather than as an error — that is what lets ' +
      'the server ship a new frame type before every client understands it.',
      '//'
    ))
    L.push(`export function decode${d.name}(v: unknown, p = ${JSON.stringify(d.name)}): ${d.name} | null {`)
    L.push(`  const o = asObj(v, p)`)
    L.push(`  switch (o[${JSON.stringify(disc)}]) {`)
    for (const m of members) {
      const md = discriminantOf(m)
      if (!md) fail(`${d.name}: member ${m.name} has no const ${disc}`)
      L.push(`    case ${JSON.stringify(md.value)}: return decode${m.name}(o, p)`)
    }
    L.push('    default: return null')
    L.push('  }')
    L.push('}', '')
    L.push(`export function encode${d.name}(v: ${d.name}): Record<string, unknown> {`)
    L.push(`  switch (v.${camel(disc)}) {`)
    for (const m of members) {
      const md = discriminantOf(m)
      L.push(`    case ${JSON.stringify(md.value)}: return encode${m.name}(v as ${m.name})`)
    }
    L.push('  }')
    L.push('}', '')
  }

  /* Registry, so the conformance runner is generic: it names a type in the
   * vector file and does not need updating when the schema grows a type. A
   * runner that had to be edited alongside the schema would be one more place
   * for the two clients to test different things. */
  L.push('export interface WireCodec {')
  L.push('  decode(v: unknown): unknown')
  L.push('  encode(v: unknown): unknown')
  L.push('}', '')
  L.push('export const codecs: Record<string, WireCodec> = {')
  for (const d of model) {
    if (d.kind === 'object') {
      L.push(`  ${d.name}: { decode: (v) => decode${d.name}(v), encode: (v) => encode${d.name}(v as ${d.name}) },`)
    } else if (d.kind === 'union') {
      L.push(`  ${d.name}: { decode: (v) => decode${d.name}(v), encode: (v) => encode${d.name}(v as ${d.name}) },`)
    }
  }
  L.push('}', '')

  return L.join('\n') + '\n'
}

/* ================================================================== *
 * Dart
 * ================================================================== */

function dartType(ref, nullable) {
  const q = nullable ? '?' : ''
  switch (ref.kind) {
    case 'named': return ref.name + q
    case 'list': return `List<${dartType(ref.item, false)}>${q}`
    case 'const': return `String${q}`
    case 'inlineEnum': return `String${q}`
    case 'prim':
      return (ref.prim === 'integer' ? 'int' : ref.prim === 'number' ? 'double'
        : ref.prim === 'boolean' ? 'bool' : 'String') + q
  }
}

function dartDecode(ref, src, path, depth = 0) {
  switch (ref.kind) {
    case 'named': {
      const d = byName.get(ref.name)
      if (d.kind === 'alias') return `_as${d.name}(${src}, ${dartPath(path)})`
      if (d.kind === 'enum') return `${d.name}.fromWire(${src}, ${dartPath(path)})`
      return `${d.name}.fromJson(${src}, ${dartPath(path)})`
    }
    case 'list': {
      const iv = `i${depth || ''}`
      const xv = `x${depth || ''}`
      const itemPath = path + '[${' + iv + '}]'
      const mapped = `[for (final (${iv}, ${xv}) in _arr(${src}, ${dartPath(path)}).indexed) ${dartDecode(ref.item, xv, itemPath, depth + 1)}]`
      /* See the TypeScript half: an unrecognised union tag is an element to
       * skip, so the list type stays non-nullable for every consumer. */
      if (isUnionRef(ref.item)) return `${mapped}.whereType<${dartType(ref.item, false)}>().toList()`
      return mapped
    }
    case 'const': return `_str(${src}, ${dartPath(path)})`
    case 'inlineEnum':
      return `_oneOf(${src}, const [${ref.values.map((v) => JSON.stringify(v)).join(', ')}], ${dartPath(path)})`
    case 'prim':
      return ref.prim === 'integer' ? `_int(${src}, ${dartPath(path)})`
        : ref.prim === 'number' ? `_num(${src}, ${dartPath(path)})`
        : ref.prim === 'boolean' ? `_bool(${src}, ${dartPath(path)})`
        : `_str(${src}, ${dartPath(path)})`
  }
}

function dartEncode(ref, src) {
  switch (ref.kind) {
    case 'named': {
      const d = byName.get(ref.name)
      if (d.kind === 'alias') return src
      if (d.kind === 'enum') return `${src}.wire`
      return `${src}.toJson()`
    }
    case 'list': {
      const inner = dartEncode(ref.item, 'x')
      return inner === 'x' ? src : `[for (final x in ${src}) ${inner}]`
    }
    default: return src
  }
}

function emitDart() {
  const L = []
  L.push(BANNER, '')
  L.push('// ignore_for_file: unnecessary_this, prefer_const_constructors, lines_longer_than_80_chars', '')
  L.push("import 'dart:developer' as developer;", '')
  L.push(`const int wireVersion = ${WIRE_VERSION};`, '')
  L.push(
    '/// Thrown when a frame does not match the schema. Carries the JSON path so a',
    '/// malformed field is identifiable from a log line rather than needing a repro.',
    'class WireFormatException implements Exception {',
    '  const WireFormatException(this.path, this.message);',
    '  final String path;',
    '  final String message;',
    '  @override',
    "  String toString() => 'WireFormatException: \$path: \$message';",
    '}',
    '',
    'Never _bad(String p, String m) => throw WireFormatException(p, m);',
    '',
    'Map<String, dynamic> _obj(Object? v, String p) => v is Map<String, dynamic>',
    '    ? v',
    "    : _bad(p, 'expected object, got \${v.runtimeType}');",
    "String _str(Object? v, String p) => v is String ? v : _bad(p, 'expected String, got \${v.runtimeType}');",
    "bool _bool(Object? v, String p) => v is bool ? v : _bad(p, 'expected bool, got \${v.runtimeType}');",
    'int _int(Object? v, String p) {',
    "  if (v is! int) _bad(p, 'expected int, got \${v.runtimeType}');",
    '  // Mirrors the TypeScript guard. The schema caps ordinals at 2^53-1 so that a',
    '  // value legal in Dart cannot be a value JavaScript has already rounded; a Dart',
    '  // client that accepted more would be the half of the pair that disagrees.',
    "  if (v > 9007199254740991 || v < -9007199254740991) _bad(p, 'integer \$v exceeds the range JavaScript can represent exactly');",
    '  return v;',
    '}',
    /* Emitted only when the schema actually uses a floating-point field, which
     * today it deliberately does not — see DurationMs. Dart flags an unused
     * private helper, so a generator that always emitted it would leave the
     * output failing analysis for a type nothing references. */
    ...(usesFloat() ? [
      'double _num(Object? v, String p) {',
      '  if (v is num) return v.toDouble();',
      "  return _bad(p, 'expected num, got \${v.runtimeType}');",
      '}',
    ] : []),
    "List<Object?> _arr(Object? v, String p) => v is List ? v : _bad(p, 'expected List, got \${v.runtimeType}');",
    'String _oneOf(Object? v, List<String> allowed, String p) {',
    '  final s = _str(v, p);',
    "  return allowed.contains(s) ? s : _bad(p, 'expected one of \${allowed.join('|')}, got \"\$s\"');",
    '}',
    '',
    '/// CANT-74: an enum a client can RECEIVE is open on the clients. A value this schema',
    '/// version does not know decodes to the sentinel `unknown` and is reported ONCE per',
    '/// (enum, raw) per process — a busy thread would otherwise print it hundreds of times.',
    '/// Set on `ready` so the report can say how far apart the two ends are.',
    'int? serverWireVersion;',
    "/// Where the report goes. Defaults to dart:developer's log; an app or a test may replace it.",
    "void Function(String message) onUnknownWireValue = (m) => developer.log(m, name: 'wire');",
    'final Set<String> _warned = <String>{};',
    'void _warnUnknown(String enumName, String raw) {',
    "  if (!_warned.add('$enumName\\u0000$raw')) return;",
    "  final server = serverWireVersion == null ? '' : ', server wire_version $serverWireVersion';",
    "  onUnknownWireValue('wire: $enumName: unknown value \"$raw\" decoded as unknown (client wire_version $wireVersion$server)');",
    '}',
    '',
    '/// Drops null entries so an absent optional is omitted rather than encoded as null.',
    'Map<String, dynamic> _compact(Map<String, dynamic> m) {',
    '  m.removeWhere((_, v) => v == null);',
    '  return m;',
    '}',
    ''
  )

  for (const d of model) {
    if (d.kind === 'alias') {
      L.push(...doc(d.node.description, '///'))
      const t = d.node.type === 'integer' ? 'int' : d.node.type === 'number' ? 'double'
        : d.node.type === 'boolean' ? 'bool' : 'String'
      L.push(`typedef ${d.name} = ${t};`)
      const n = d.node
      if (n.pattern) {
        L.push(`final RegExp _${camel(d.name)}Pattern = RegExp(r'${n.pattern}');`)
      }
      L.push(`${t} _as${d.name}(Object? v, String p) {`)
      L.push(`  final x = ${d.node.type === 'integer' ? '_int' : d.node.type === 'number' ? '_num' : '_str'}(v, p);`)
      if (n.pattern) {
        /* Escape for a Dart single-quoted string: backslashes first, then the
         * interpolation sigil, then the quote. Anchored patterns end in `$`,
         * which Dart would otherwise read as the start of an interpolation. */
        const lit = n.pattern
          .replace(/\\/g, '\\\\')
          .replace(/\$/g, '\\$')
          .replace(/'/g, "\\'")
        L.push(`  if (!_${camel(d.name)}Pattern.hasMatch(x)) _bad(p, '${d.name} must match ${lit}, got \"\$x\"');`)
      }
      if (n.minimum !== undefined) L.push(`  if (x < ${n.minimum}) _bad(p, '${d.name} must be >= ${n.minimum}, got \$x');`)
      if (n.maximum !== undefined) L.push(`  if (x > ${n.maximum}) _bad(p, '${d.name} must be <= ${n.maximum}, got \$x');`)
      L.push('  return x;', '}', '')
    } else if (d.kind === 'enum') {
      L.push(...doc(d.node.description, '///'))
      L.push(...doc(openness(d.name), '///'))
      const open = clientOpen.has(d.name)
      L.push(`enum ${d.name} {`)
      for (const v of d.node.enum) {
        L.push(`  ${camel(v)}(${JSON.stringify(v)}),`)
      }
      if (open) {
        L.push('  /// The sentinel: a value this schema version does not define. Never authored by a client.')
        L.push('  unknown("unknown"),')
      }
      L.push('  ;', '')
      L.push(`  const ${d.name}(this.wire);`)
      L.push('  /// The exact string this value has on the wire. Never derive it from the')
      L.push('  /// Dart identifier — the two differ wherever the wire uses snake_case.')
      L.push('  final String wire;', '')
      L.push(`  static ${d.name} fromWire(Object? v, String p) {`)
      if (open) {
        L.push('    final s = _str(v, p);')
        L.push(`    for (final e in ${d.name}.values) { if (e != ${d.name}.unknown && e.wire == s) return e; }`)
        L.push(`    _warnUnknown(${JSON.stringify(d.name)}, s);`)
        L.push(`    return ${d.name}.unknown;`)
      } else {
        L.push(`    for (final e in ${d.name}.values) { if (e.wire == v) return e; }`)
        L.push(`    return _bad(p, 'not a valid ${d.name}: "\$v"');`)
      }
      L.push('  }')
      L.push('}', '')
    }
  }

  /* Union membership, so a frame in two unions implements both interfaces. */
  const unions = model.filter((d) => d.kind === 'union')
  const memberOf = new Map()
  for (const u of unions) {
    for (const m of u.node.oneOf) {
      const n = refName(m.$ref)
      if (!memberOf.has(n)) memberOf.set(n, [])
      memberOf.get(n).push(u.name)
    }
  }

  for (const u of unions) {
    L.push(...doc(u.node.description, '///'))
    L.push(`sealed class ${u.name} {`)
    L.push('  /// The wire tag for this frame.')
    L.push(`  String get ${camel(u.node.discriminator.propertyName)};`)
    L.push('  Map<String, dynamic> toJson();', '')
    const members = u.node.oneOf.map((m) => byName.get(refName(m.$ref)))
    L.push(...doc(
      `Decode a ${u.name}. Returns null for an unrecognised ` +
      `${JSON.stringify(u.node.discriminator.propertyName)}, which callers MUST treat as ` +
      '"ignore this frame and carry on" rather than as an error — that is what lets the ' +
      'server ship a new frame type before every client understands it.',
      '///', '  '
    ))
    L.push(`  static ${u.name}? fromJson(Object? v, [String p = ${JSON.stringify(u.name)}]) {`)
    L.push('    final o = _obj(v, p);')
    L.push(`    switch (o[${JSON.stringify(u.node.discriminator.propertyName)}]) {`)
    for (const m of members) {
      const md = discriminantOf(m)
      L.push(`      case ${JSON.stringify(md.value)}: return ${m.name}.fromJson(o, p);`)
      }
    L.push('      default: return null;')
    L.push('    }')
    L.push('  }')
    L.push('}', '')
  }

  for (const d of model) {
    if (d.kind !== 'object') continue
    const fs = fieldsOf(d)
    const ifaces = memberOf.get(d.name) || []
    L.push(...doc(d.node.description, '///'))
    L.push(`final class ${d.name}${ifaces.length ? ' implements ' + ifaces.join(', ') : ''} {`)
    /* Constructor */
    const ctorParams = fs.filter((f) => f.ref.kind !== 'const')
    L.push(`  const ${d.name}({`)
    for (const f of ctorParams) {
      L.push(`    ${f.required ? 'required ' : ''}this.${camel(f.wire)},`)
    }
    L.push('  });', '')
    for (const f of fs) {
      L.push(...doc(f.doc, '  ///'))
      if (f.ref.kind === 'const') {
        L.push('  @override')
        L.push(`  String get ${camel(f.wire)} => ${JSON.stringify(f.ref.value)};`, '')
      } else {
        L.push(`  final ${dartType(f.ref, !f.required)} ${camel(f.wire)};`, '')
      }
    }
    L.push(`  factory ${d.name}.fromJson(Object? v, [String p = ${JSON.stringify(d.name)}]) {`)
    L.push('    final o = _obj(v, p);')
    L.push(`    return ${d.name}(`)
    for (const f of ctorParams) {
      const acc = `o[${JSON.stringify(f.wire)}]`
      const path = '${p}.' + f.wire
      if (f.required) {
        L.push(`      ${camel(f.wire)}: ${acc} == null ? _bad(${dartPath(path)}, 'required field is missing') : ${dartDecode(f.ref, acc, path)},`)
      } else {
        L.push(`      ${camel(f.wire)}: ${acc} == null ? null : ${dartDecode(f.ref, acc, path)},`)
      }
    }
    L.push('    );')
    L.push('  }', '')
    if (ifaces.length) L.push('  @override')
    L.push('  Map<String, dynamic> toJson() => _compact({')
    for (const f of fs) {
      const key = JSON.stringify(f.wire)
      if (f.ref.kind === 'const') {
        L.push(`    ${key}: ${JSON.stringify(f.ref.value)},`)
      } else if (f.required) {
        L.push(`    ${key}: ${dartEncode(f.ref, camel(f.wire))},`)
      } else {
        const inner = dartEncode(f.ref, camel(f.wire) + '!')
        L.push(`    ${key}: ${camel(f.wire)} == null ? null : ${inner},`)
      }
    }
    L.push('  });')
    L.push('}', '')
  }

  /* See the TypeScript registry: keeps the conformance runner generic so both
   * clients are held to the same vectors without either runner being edited. */
  L.push('/// A decode/encode pair for one named wire type.')
  L.push('class WireCodec {')
  L.push('  const WireCodec(this.decode, this.encode);')
  L.push('  final Object? Function(Object?) decode;')
  L.push('  final Map<String, dynamic> Function(Object) encode;')
  L.push('}', '')
  L.push('const Map<String, WireCodec> codecs = {')
  for (const d of model) {
    if (d.kind === 'object') {
      L.push(`  ${JSON.stringify(d.name)}: WireCodec(${d.name}.fromJson, _enc${d.name}),`)
    } else if (d.kind === 'union') {
      L.push(`  ${JSON.stringify(d.name)}: WireCodec(${d.name}.fromJson, _enc${d.name}),`)
    }
  }
  L.push('};', '')
  for (const d of model) {
    if (d.kind === 'object') {
      L.push(`Map<String, dynamic> _enc${d.name}(Object v) => (v as ${d.name}).toJson();`)
    } else if (d.kind === 'union') {
      L.push(`Map<String, dynamic> _enc${d.name}(Object v) => (v as ${d.name}).toJson();`)
    }
  }
  L.push('')

  return L.join('\n') + '\n'
}

/* ================================================================== *
 * Go — the server generates from the same walk, so it cannot drift either.
 * ================================================================== */

/* A Go string literal for a regex. Backquoted so the pattern's backslashes stay
 * literal — `\\.` in a JSON Schema pattern is one backslash and a dot, and a
 * double-quoted Go literal would eat it. Falls back to a quoted literal if the
 * pattern itself contains a backquote, which no pattern here does. */
function goRawString(v) {
  return v.includes('`') ? JSON.stringify(v) : '`' + v + '`'
}

/* The check a Go decoder runs on one value, mirroring tsDecode's dispatch.
 *
 * Returns lines, because Go has no expression form for "validate or return an
 * error". `expr` is the already-decoded value and `path` is the JSON path the
 * error names, so a Go failure reads like the TS and Dart ones: the vectors
 * hold all three to the same rules, and a message naming the field is what
 * makes a reject case debuggable rather than merely red.
 */
function goCheck(ref, expr, path, depth = 0) {
  switch (ref.kind) {
    case 'named': {
      const d = byName.get(ref.name)
      if (d.kind === 'alias' && !hasConstraints(d.node)) return []
      if (d.kind === 'object' || d.kind === 'union') return [] // its own UnmarshalJSON ran
      return [`\tif err := check${d.name}(${expr}, ${JSON.stringify(path)}); err != nil {`,
        '\t\treturn err', '\t}']
    }
    case 'list': {
      const inner = goCheck(ref.item, `v${depth}`, `${path}[]`, depth + 1)
      if (!inner.length) return []
      /* PATH_EXPR is a placeholder the object emitter substitutes, so a nested
       * list's path is built from the PARENT's runtime path rather than from
       * the type name baked in here. The first version embedded the type name
       * in a fmt.Sprintf and left the emitter's .replace() looking for a quoted
       * literal the line no longer contained — the substitution no-opped in
       * silence, and one frame reported ServerTyping.user_ids[0] while every
       * sibling field on it reported ServerFrame[typing].… */
      return [`\tfor i${depth}, v${depth} := range ${expr} {`,
        `\t\t_ = i${depth}`,
        ...inner.map((l) => '\t' + l.replace(JSON.stringify(`${path}[]`),
          `fmt.Sprintf("%s[%d]", PATH_EXPR, i${depth})`)),
        '\t}']
    }
    case 'inlineEnum':
      return [`\tif !oneOf(${expr}, []string{${ref.values.map((v) => JSON.stringify(v)).join(', ')}}) {`,
        `\t\treturn badf(${JSON.stringify(path)}, "expected one of %s, got %q", ${JSON.stringify(ref.values.join('|'))}, ${expr})`,
        '\t}']
    default:
      return []
  }
}

/* Whether an alias carries anything worth checking. An unconstrained one is a
 * name for a Go builtin and needs no function. */
function hasConstraints(n) {
  return n.pattern !== undefined || n.minimum !== undefined || n.maximum !== undefined
}

function goType(ref, optional, ctx = '') {
  const ptr = optional ? '*' : ''
  switch (ref.kind) {
    case 'named': {
      const d = byName.get(ref.name)
      if (d.kind === 'union') {
        /* A bare interface-typed field cannot be unmarshalled by encoding/json:
         * it has no concrete type to allocate. Arrays of a union get a named
         * slice type below, which is where the dispatch lives. A scalar union
         * field would need the same treatment; the schema has none today, so
         * fail loudly rather than emit code that compiles and then panics. */
        fail(`${ctx}: union ${ref.name} used as a scalar field. Go needs a named wrapper type for that; add one to the generator before adding such a field.`)
      }
      return ptr + ref.name
    }
    case 'list':
      if (isUnionRef(ref.item)) return `${ref.item.name}List`
      return `[]${goType(ref.item, false, ctx)}`
    case 'const': return 'string'
    case 'inlineEnum': return ptr + 'string'
    case 'prim':
      return ptr + (ref.prim === 'integer' ? 'int64' : ref.prim === 'number' ? 'float64'
        : ref.prim === 'boolean' ? 'bool' : 'string')
  }
}


/* Run the emitted Go through `gofmt`.
 *
 * CANT-15. The emitter does not column-align const blocks and struct fields,
 * which gofmt does, so the committed file needed 307 lines of gofmt diff. That
 * put two checks in DIRECT CONFLICT and only one of them was satisfiable by
 * hand: `gen:check` byte-compares the committed file against a fresh generator
 * run, so `gofmt -w` on the file makes the staleness guard fail, and leaving it
 * makes a `gofmt -l` lane fail. No arrangement of the committed file satisfies
 * both — which is why the fix belongs in the generator and not in the file.
 *
 * Formatting the bytes on the way out keeps the byte-comparison honest, because
 * the guard then compares gofmt'd output against gofmt'd output.
 *
 * A missing gofmt is a hard failure rather than a warning. Emitting
 * unformatted Go "just this once" is how the 307 lines happened, and it would
 * come back as a red CI lane on somebody else's branch. */
function gofmt(src) {
  try {
    return execFileSync('gofmt', [], { input: src, encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 })
  } catch (err) {
    if (err.code === 'ENOENT') fail('gofmt is not on PATH — it is needed to emit gofmt-clean Go (CANT-15)')
    fail(`gofmt rejected the generated Go, which means the emitter produced something that does not parse:\n${err.stderr || err.message}`)
  }
}

function emitGo() {
  const L = []
  L.push(BANNER, '')
  L.push('package wire', '')
  L.push('import (', '\t"encoding/json"', '\t"errors"', '\t"fmt"', '\t"regexp"', ')', '')
  L.push(`// WireVersion is the schema version this package was generated from.`)
  L.push(`const WireVersion = ${WIRE_VERSION}`, '')

  /* CANT-25. The server is the trust boundary and was the only one of the three
   * implementations that would not refuse bad input: TypeScript and Dart
   * validated, Go merely decoded. The gap was structural rather than an
   * oversight — encoding/json cannot distinguish an absent required scalar from
   * an explicit zero one, so `{"seq": 0}` and `{}` produced the same struct.
   *
   * Every object below now carries an UnmarshalJSON that decodes into a shadow
   * whose fields are ALL pointers, which is what makes presence observable, and
   * then runs the schema's constraints with the same rules and the same JSON
   * paths the TS and Dart decoders use. The ten reject vectors are the oracle. */
  L.push('// DecodeError is a frame that does not match the schema. It carries the JSON')
  L.push('// path, so a failure names the field rather than the frame.')
  L.push('type DecodeError struct {')
  L.push('\tPath string')
  L.push('\tMsg  string')
  L.push('}', '')
  L.push('func (e *DecodeError) Error() string { return "wire: " + e.Path + ": " + e.Msg }', '')
  L.push('func badf(path, format string, args ...any) error {')
  L.push('\treturn &DecodeError{Path: path, Msg: fmt.Sprintf(format, args...)}')
  L.push('}', '')
  L.push('// decodeErr renders an encoding/json failure as a DecodeError with a path.')
  L.push('//')
  L.push('// A raw UnmarshalTypeError names the SHADOW struct, which is an anonymous type')
  L.push('// with every field and tag spelled out — fourteen of them on Message. The field')
  L.push('// and the expected type are the useful part, and they are what TS and Dart')
  L.push('// report. A DecodeError coming back from a nested decode already carries its')
  L.push('// own path and is passed through untouched.')
  L.push('func decodeErr(p string, err error) error {')
  L.push('\tvar de *DecodeError')
  L.push('\tif errors.As(err, &de) {')
  L.push('\t\treturn de')
  L.push('\t}')
  L.push('\tvar te *json.UnmarshalTypeError')
  L.push('\tif errors.As(err, &te) {')
  L.push('\t\tif te.Field != "" {')
  L.push('\t\t\treturn badf(p+"."+te.Field, "expected %s, got %s", te.Type, te.Value)')
  L.push('\t\t}')
  L.push('\t\treturn badf(p, "expected %s, got %s", te.Type, te.Value)')
  L.push('\t}')
  L.push('\treturn badf(p, "%s", err)')
  L.push('}', '')
  L.push('// oneOf reports whether s is in allowed. Used by inline enums, which have no')
  L.push('// named type to hang a Valid method on.')
  L.push('func oneOf(s string, allowed []string) bool {')
  L.push('\tfor _, a := range allowed {')
  L.push('\t\tif s == a {')
  L.push('\t\t\treturn true')
  L.push('\t\t}')
  L.push('\t}')
  L.push('\treturn false')
  L.push('}', '')

  for (const d of model) {
    if (d.kind === 'alias') {
      L.push(...doc(d.node.description, '//'))
      const t = d.node.type === 'integer' ? 'int64' : d.node.type === 'number' ? 'float64'
        : d.node.type === 'boolean' ? 'bool' : 'string'
      L.push(`type ${d.name} = ${t}`, '')
      /* The constraints are the point, and this is the half Go was missing. A
       * schema that documents Timestamp as millisecond-precision UTC and then
       * accepts anything string-shaped has described the format rather than
       * pinned it — and the two clients discover the difference by sorting a
       * conversation differently. */
      if (hasConstraints(d.node)) {
        const n = d.node
        if (n.pattern) {
          L.push(`var ${camel(d.name)}Pattern = regexp.MustCompile(${goRawString(n.pattern)})`, '')
        }
        L.push(`// check${d.name} enforces the schema's constraints on a ${d.name}.`)
        L.push(`func check${d.name}(v ${t}, p string) error {`)
        if (n.pattern) {
          L.push(`\tif !${camel(d.name)}Pattern.MatchString(v) {`)
          L.push(`\t\treturn badf(p, "${d.name} must match %s, got %q", ${goRawString(n.pattern)}, v)`)
          L.push('\t}')
        }
        if (n.minimum !== undefined) {
          L.push(`\tif v < ${n.minimum} {`)
          L.push(`\t\treturn badf(p, "${d.name} must be >= ${n.minimum}, got %v", v)`)
          L.push('\t}')
        }
        if (n.maximum !== undefined) {
          L.push(`\tif v > ${n.maximum} {`)
          L.push(`\t\treturn badf(p, "${d.name} must be <= ${n.maximum}, got %v", v)`)
          L.push('\t}')
        }
        L.push('\treturn nil')
        L.push('}', '')
      }
    } else if (d.kind === 'enum') {
      L.push(...doc(d.node.description, '//'))
      L.push(`type ${d.name} string`, '')
      L.push('const (')
      for (const v of d.node.enum) {
        L.push(`\t${d.name}${enumConst(v)} ${d.name} = ${JSON.stringify(v)}`)
      }
      L.push(')', '')
      L.push(`// Valid reports whether v is a value this schema version defines.`)
      L.push(`func (v ${d.name}) Valid() bool {`)
      L.push('\tswitch v {')
      L.push(`\tcase ${d.node.enum.map((x) => d.name + enumConst(x)).join(', ')}:`)
      L.push('\t\treturn true')
      L.push('\t}')
      L.push('\treturn false')
      L.push('}', '')
      L.push(`// check${d.name} rejects a value this schema version does not define.`)
      L.push(`func check${d.name}(v ${d.name}, p string) error {`)
      L.push('\tif !v.Valid() {')
      L.push(`\t\treturn badf(p, "expected one of %s, got %q", ${JSON.stringify(d.node.enum.join('|'))}, string(v))`)
      L.push('\t}')
      L.push('\treturn nil')
      L.push('}', '')
    }
  }

  const unions = model.filter((d) => d.kind === 'union')
  const memberOf = new Map()
  for (const u of unions) {
    for (const m of u.node.oneOf) {
      const n = refName(m.$ref)
      if (!memberOf.has(n)) memberOf.set(n, [])
      memberOf.get(n).push(u.name)
    }
  }

  for (const u of unions) {
    L.push(...doc(u.node.description, '//'))
    L.push(`type ${u.name} interface {`)
    L.push(`\tis${u.name}()`)
    L.push(`\t// WireTag returns the discriminator value this member carries.`)
    L.push('\tWireTag() string')
    L.push('}', '')

    /* Named slice type carrying the dispatch, because encoding/json cannot
     * unmarshal into []Interface. This is also where the "unknown member is
     * skipped, not fatal" rule is implemented for Go — the same rule the TS
     * and Dart decoders apply, and the vectors hold all three to it. */
    L.push(`// ${u.name}List is a JSON array of ${u.name}. It exists because encoding/json`)
    L.push(`// cannot unmarshal into a slice of interfaces: there is no concrete type to`)
    L.push('// allocate. Unmarshalling SKIPS an element whose tag is unrecognised rather')
    L.push('// than failing, so a client or server built against an older schema loses the')
    L.push('// element and not the whole message.')
    L.push(`type ${u.name}List []${u.name}`, '')
    L.push(`func (l *${u.name}List) UnmarshalJSON(b []byte) error {`)
    L.push(`\treturn l.decode(b, ${JSON.stringify(u.name + 'List')})`)
    L.push('}', '')
    L.push(`// decode is UnmarshalJSON with the caller's JSON path.`)
    L.push(`func (l *${u.name}List) decode(b []byte, p string) error {`)
    L.push('\tvar raw []json.RawMessage')
    L.push('\tif err := json.Unmarshal(b, &raw); err != nil {')
    L.push('\t\treturn decodeErr(p, err)')
    L.push('\t}')
    L.push(`\tout := make(${u.name}List, 0, len(raw))`)
    L.push('\tfor i, r := range raw {')
    L.push(`\t\tv, err := decode${u.name}(r, fmt.Sprintf("%s[%d]", p, i))`)
    L.push('\t\tif err != nil {')
    L.push('\t\t\treturn err // already carries its element path')
    L.push('\t\t}')
    L.push('\t\tif v == nil {')
    L.push('\t\t\tcontinue')
    L.push('\t\t}')
    L.push('\t\tout = append(out, v)')
    L.push('\t}')
    L.push('\t*l = out')
    L.push('\treturn nil')
    L.push('}', '')
  }

  for (const d of model) {
    if (d.kind !== 'object') continue
    const fs = fieldsOf(d)
    L.push(...doc(d.node.description, '//'))
    L.push(`type ${d.name} struct {`)
    for (const f of fs) {
      if (f.ref.kind === 'const') continue
      L.push(...doc(f.doc, '\t//'))
      const omit = f.required ? '' : ',omitempty'
      L.push(`\t${goName(f.wire)} ${goType(f.ref, !f.required, `${d.name}.${f.wire}`)} \`json:"${f.wire}${omit}"\``)
    }
    L.push('}', '')

    /* CANT-25's mechanism. A shadow whose fields are ALL pointers is what makes
     * "absent" distinguishable from "present and zero" — the exact thing
     * encoding/json cannot do on its own, and the reason the Go decoders used
     * to accept a Message with no seq. Everything else follows from that:
     * required fields are the nil ones, and the constraints run on what is
     * left.
     *
     * Unknown fields are IGNORED, deliberately and in parity with TS and Dart.
     * The ignore_unknown_* vectors require it: a client built against a newer
     * schema must not break an older peer. */
    const checkable = fs.filter((f) => f.ref.kind !== 'const')
    L.push(`// UnmarshalJSON decodes and VALIDATES a ${d.name}: required fields must be`)
    L.push('// present, and every constrained value is checked against the schema.')
    L.push(`func (v *${d.name}) UnmarshalJSON(b []byte) error {`)
    L.push(`\treturn v.decode(b, ${JSON.stringify(d.name)})`)
    L.push('}', '')
    L.push('// decode carries the JSON path, so a nested failure names the field it came')
    L.push('// from rather than the outermost type.')
    L.push(`func (v *${d.name}) decode(b []byte, p string) error {`)
    /* A field whose value has its own decode() is taken as RawMessage and
     * decoded EXPLICITLY, so it is handed this frame's path. Letting
     * json.Unmarshal reach it instead calls its UnmarshalJSON with the child's
     * own default path, and a nested failure then reads "ReplyRef.preview"
     * where TS and Dart say "Message.reply_to.preview" — the union dispatch had
     * the same bug and this is the other half of it. */
    const nested = (ref) => {
      if (ref.kind === 'named') {
        const t = byName.get(ref.name)
        return t.kind === 'object' ? 'object' : null
      }
      if (ref.kind === 'list') {
        if (isUnionRef(ref.item)) return 'unionList'
        if (ref.item.kind === 'named' && byName.get(ref.item.name).kind === 'object') return 'objectList'
      }
      return null
    }
    L.push('\tvar s struct {')
    for (const f of checkable) {
      const n = nested(f.ref)
      const t = n === 'objectList' ? '[]json.RawMessage' : n ? 'json.RawMessage' : goType(f.ref, false, `${d.name}.${f.wire}`)
      L.push(`\t\t${goName(f.wire)} *${t} \`json:"${f.wire}"\``)
    }
    L.push('\t}')
    L.push('\tif err := json.Unmarshal(b, &s); err != nil {')
    L.push('\t\treturn decodeErr(p, err)')
    L.push('\t}')
    L.push(`\tvar out ${d.name}`)
    for (const f of checkable) {
      const name = goName(f.wire)
      const path = `${d.name === 'X' ? '' : ''}`
      const fieldPath = `\${p}.${f.wire}`
      const realOptionalPtr = goType(f.ref, true, '').startsWith('*')
      const n = nested(f.ref)
      const fp = `p+${JSON.stringify('.' + f.wire)}`
      if (f.required) {
        L.push(`\tif s.${name} == nil {`)
        L.push(`\t\treturn badf(${fp}, "required field is missing")`)
        L.push('\t}')
      } else {
        L.push(`\tif s.${name} != nil {`)
      }
      const ind = f.required ? '\t' : '\t\t'
      if (n === 'object') {
        if (!f.required) L.push(`${ind}out.${name} = new(${goType(f.ref, false, '')})`)
        L.push(`${ind}if err := out.${name}.decode(*s.${name}, ${fp}); err != nil {`)
        L.push(`${ind}\treturn err`)
        L.push(`${ind}}`)
      } else if (n === 'unionList') {
        L.push(`${ind}if err := out.${name}.decode(*s.${name}, ${fp}); err != nil {`)
        L.push(`${ind}\treturn err`)
        L.push(`${ind}}`)
      } else if (n === 'objectList') {
        const et = goType(f.ref.item, false, '')
        L.push(`${ind}out.${name} = make([]${et}, len(*s.${name}))`)
        L.push(`${ind}for i, raw := range *s.${name} {`)
        L.push(`${ind}\tif err := out.${name}[i].decode(raw, fmt.Sprintf("%s[%d]", ${fp}, i)); err != nil {`)
        L.push(`${ind}\t\treturn err`)
        L.push(`${ind}\t}`)
        L.push(`${ind}}`)
      } else if (!f.required && realOptionalPtr) {
        L.push(`${ind}out.${name} = s.${name}`)
      } else {
        L.push(`${ind}out.${name} = *s.${name}`)
      }
      if (!f.required) L.push('\t}')
    }
    for (const f of checkable) {
      const name = goName(f.wire)
      const realOptionalPtr = goType(f.ref, true, '').startsWith('*')
      const expr = !f.required && realOptionalPtr ? `*out.${name}` : `out.${name}`
      if (nested(f.ref)) continue
      const lines = goCheck(f.ref, expr, `${d.name}.${f.wire}`)
      if (!lines.length) continue
      const pathExpr = `p+${JSON.stringify('.' + f.wire)}`
      const body = lines.map((l) => l
        .replace(JSON.stringify(`${d.name}.${f.wire}`), pathExpr)
        .replaceAll('PATH_EXPR', pathExpr))
      if (!f.required) {
        L.push(`\tif out.${name} != nil {`)
        L.push(...body.map((l) => '\t' + l))
        L.push('\t}')
      } else {
        L.push(...body)
      }
    }
    L.push('\t*v = out')
    L.push('\treturn nil')
    L.push('}', '')

    const disc = discriminantOf(d)
    const ifaces = memberOf.get(d.name) || []
    if (disc) {
      L.push(`// WireTag returns the discriminator value ${JSON.stringify(disc.value)}.`)
      L.push(`func (${d.name}) WireTag() string { return ${JSON.stringify(disc.value)} }`, '')
      L.push(`// MarshalJSON injects the constant ${JSON.stringify(disc.wire)} tag, so the tag cannot be`)
      L.push('// forgotten at a call site or set to something the schema does not allow.')
      L.push(`func (v ${d.name}) MarshalJSON() ([]byte, error) {`)
      L.push(`\ttype alias ${d.name}`)
      L.push('\treturn json.Marshal(struct {')
      L.push(`\t\tT string \`json:"${disc.wire}"\``)
      L.push('\t\talias')
      L.push(`\t}{T: ${JSON.stringify(disc.value)}, alias: alias(v)})`)
      L.push('}', '')
      for (const i of ifaces) {
        L.push(`func (${d.name}) is${i}() {}`, '')
      }
    }
  }

  for (const u of unions) {
    const members = u.node.oneOf.map((m) => byName.get(refName(m.$ref)))
    const disc = u.node.discriminator.propertyName
    L.push(`// Decode${u.name} dispatches on ${JSON.stringify(disc)}. A nil result with a nil error`)
    L.push('// means an unrecognised tag, which callers MUST treat as "ignore and carry on".')
    L.push(`func Decode${u.name}(b []byte) (${u.name}, error) {`)
    L.push(`\treturn decode${u.name}(b, ${JSON.stringify(u.name)})`)
    L.push('}', '')
    L.push(`// decode${u.name} is Decode${u.name} with the caller's JSON path, so a failure`)
    L.push('// names the field the frame arrived in rather than the union type.')
    L.push(`func decode${u.name}(b []byte, p string) (${u.name}, error) {`)
    L.push('\tvar probe struct {')
    L.push(`\t\tT string \`json:"${disc}"\``)
    L.push('\t}')
    L.push('\tif err := json.Unmarshal(b, &probe); err != nil {')
    L.push('\t\treturn nil, decodeErr(p, err)')
    L.push('\t}')
    L.push('\tswitch probe.T {')
    for (const m of members) {
      const md = discriminantOf(m)
      L.push(`\tcase ${JSON.stringify(md.value)}:`)
      L.push(`\t\tvar v ${m.name}`)
      /* decode rather than json.Unmarshal, so the member's failures carry a
       * path that names the union and the tag it dispatched on. Wrapping the
       * error instead produced "wire: ready: wire: ServerReady.server_time: …",
       * which says "wire" twice and buries the field. */
      L.push(`\t\tif err := v.decode(b, p+${JSON.stringify('[' + md.value + ']')}); err != nil {`)
      L.push('\t\t\treturn nil, err')
      L.push('\t\t}')
      L.push('\t\treturn v, nil')
    }
    L.push('\t}')
    L.push('\treturn nil, nil')
    L.push('}', '')
  }

  /* Registry for the conformance runner, matching the TS and Dart ones.
   *
   * THE GAP THIS COMMENT USED TO DESCRIBE IS CLOSED (CANT-25). These decoders
   * enforce the schema's constraints like the TS and Dart ones: every object
   * carries an UnmarshalJSON that decodes into an all-pointer shadow — which is
   * what makes an absent required scalar distinguishable from an explicit zero
   * one — and then runs the same checks with the same JSON paths. The Go runner
   * executes all 41 vectors and skips none. */
  L.push('// DecodeNamed decodes a named wire type. Unions return a nil value with a nil')
  L.push('// error for an unrecognised tag.')
  L.push('func DecodeNamed(name string, b []byte) (any, error) {')
  L.push('\tswitch name {')
  for (const d of model) {
    if (d.kind === 'object') {
      L.push(`\tcase ${JSON.stringify(d.name)}:`)
      L.push(`\t\tvar v ${d.name}`)
      L.push(`\t\tif err := v.decode(b, ${JSON.stringify(d.name)}); err != nil {`)
      L.push('\t\t\treturn nil, err')
      L.push('\t\t}')
      L.push('\t\treturn v, nil')
    } else if (d.kind === 'union') {
      L.push(`\tcase ${JSON.stringify(d.name)}:`)
      L.push(`\t\tv, err := Decode${d.name}(b)`)
      L.push('\t\tif err != nil {')
      L.push('\t\t\treturn nil, err')
      L.push('\t\t}')
      L.push('\t\tif v == nil {')
      L.push('\t\t\treturn nil, nil')
      L.push('\t\t}')
      L.push('\t\treturn v, nil')
    }
  }
  L.push('\t}')
  L.push('\treturn nil, fmt.Errorf("wire: unknown type %q", name)')
  L.push('}', '')

  return L.join('\n') + '\n'
}


/* ------------------------------------------------------------------ *
 * OpenAPI 3.0.3
 *
 * CANT-12 Ruling 0 came back the wrong way for the arrangement rev 2 wanted:
 * the house pipeline pins OpenAPI 3.0.3 and 3.1 is not reachable, because
 * oapi-codegen parses through kin-openapi and kin-openapi does not do 3.1.
 * Under 3.0 the frame schema cannot `$ref` the spec and the spec cannot `$ref`
 * the frame schema, so NEITHER ownership direction is expressible — and "both
 * define it, a script keeps them in step" is the drift R4 exists to prevent
 * wearing a different hat.
 *
 * So the wire schema emits the spec. `openapi.yaml` becomes a generated
 * artefact like `generated.ts`, under the same regenerate-and-diff guard, and
 * the question of which pipeline OWNS a shared type dissolves: neither does,
 * the schema owns all of them and both pipelines are downstream.
 *
 * Three transforms, all mechanical, and a census over the whole schema found
 * nothing else that needs one:
 *
 *   1. `const: "message"` -> `enum: ["message"]`. Semantically identical in
 *      3.0, which has no `const`.
 *   2. Explicit `discriminator.mapping` on all three unions. REQUIRED, not
 *      optional: 3.0's implicit mapping keys on the SCHEMA NAME, and our names
 *      do not match our values -- ServerMessageFrame carries "message",
 *      ClientHello carries "hello". Left implicit, the discriminator resolves
 *      nothing, silently.
 *   3. `#/$defs/X` -> `#/components/schemas/X`.
 *
 * The YAML is emitted by hand for the same reason the rest of this file is:
 * the generator supports exactly the subset the protocol uses and takes no
 * dependency it does not need.
 * ------------------------------------------------------------------ */

/** True when a string can be a literal block scalar without a round-trip
 *  surprise. Trailing whitespace does not survive one, a trailing newline
 *  changes what the chomping indicator has to be, and a CR is not ours to
 *  guess at. Anything rejected here falls back to a double-quoted scalar,
 *  which is JSON — and YAML 1.2 is a superset of JSON, so that path is always
 *  correct even when it is ugly.
 *
 *  Leading spaces are NOT a reason to reject: the emitter writes an explicit
 *  indentation indicator, which is exactly what it is for. Most of the wire
 *  schema's long descriptions have an indented bullet list in them, and
 *  without the indicator every one of them came out as a single JSON line. */
function yamlBlockSafe(s) {
  if (!s.includes('\n')) return false
  if (s.endsWith('\n')) return false
  return s.split('\n').every((line) => !/[ \t]$/.test(line) && !line.includes('\r'))
}

/* The block scalar's content is written two columns in from its own key, so
 * the explicit indentation indicator is always 2. Stated once, because getting
 * it wrong is a parse error at the far end rather than here. */
const YAML_BLOCK_INDENT = 2

function yamlScalar(v, indent) {
  if (v === null) return 'null'
  if (typeof v === 'boolean' || typeof v === 'number') return String(v)
  if (typeof v !== 'string') fail(`openapi: cannot emit ${typeof v} as a scalar`)
  if (yamlBlockSafe(v)) {
    const pad = ' '.repeat(indent + YAML_BLOCK_INDENT)
    return `|${YAML_BLOCK_INDENT}-\n` + v.split('\n').map((l) => (l === '' ? '' : pad + l)).join('\n')
  }
  return JSON.stringify(v)
}

function yamlEmit(node, indent = 0) {
  const pad = ' '.repeat(indent)
  if (Array.isArray(node)) {
    if (node.length === 0) return '[]'
    return '\n' + node.map((item) => {
      if (item !== null && typeof item === 'object') {
        const inner = yamlEmit(item, indent + 2)
        return pad + '- ' + inner.replace(/^\n/, '').replace(new RegExp('^' + ' '.repeat(indent + 2)), '')
      }
      return pad + '- ' + yamlScalar(item, indent)
    }).join('\n')
  }
  if (node !== null && typeof node === 'object') {
    const keys = Object.keys(node)
    if (keys.length === 0) return '{}'
    return '\n' + keys.map((k) => {
      const v = node[k]
      const key = /^[A-Za-z_][A-Za-z0-9_.-]*$/.test(k) ? k : JSON.stringify(k)
      if (v !== null && typeof v === 'object') {
        const inner = yamlEmit(v, indent + 2)
        return pad + key + ':' + (inner.startsWith('\n') ? inner : ' ' + inner)
      }
      return pad + key + ': ' + yamlScalar(v, indent)
    }).join('\n')
  }
  return yamlScalar(node, indent)
}

/** One JSON Schema node, rewritten as an OpenAPI 3.0 Schema Object. */
function toOpenAPISchema(node, ctx) {
  const out = {}
  for (const [k, v] of Object.entries(node)) {
    switch (k) {
      case '$ref':
        // A Reference Object in 3.0 is `$ref` AND NOTHING ELSE. Sibling keys —
        // 17 properties here carry a `description` next to their `$ref` — are
        // ignored by the spec and discarded by kin-openapi on unmarshal, so
        // emitting them would put text in the artefact that no consumer reads
        // and that a reader would reasonably believe was live.
        //
        // Dropping them is the honest emission, not the fix. Those field docs
        // still reach TypeScript, Dart and Go through the bespoke generator, so
        // once both pipelines are live this is doc parity failing quietly along
        // the seam the split creates. The 3.0 idiom that preserves them is
        // `allOf: [{$ref: …}]` plus `description` — which changes the node
        // shape, and what the four generators do with THAT is exactly what
        // Rulings 2 and 4 are measuring. Deliberately deferred to that ruling
        // rather than fixed by reflex here.
        return { $ref: '#/components/schemas/' + refName(v) }
      case 'const':
        // Transform 1. 3.0 has no `const`; a single-value enum is the same
        // statement and every 3.0 generator understands it.
        out.enum = [v]
        // `type` is not implied by an enum in 3.0, so state it.
        if (out.type === undefined && node.type === undefined) out.type = typeof v
        break
      case 'oneOf':
        out.oneOf = v.map((m) => toOpenAPISchema(m, ctx))
        break
      case 'discriminator': {
        // Transform 2. The mapping is built from each member's own const, so
        // it cannot drift from the values the codecs actually emit.
        const mapping = {}
        for (const member of node.oneOf || []) {
          const name = refName(member.$ref)
          const def = byName.get(name)
          if (!def) fail(`openapi: ${ctx} discriminator member ${name} is not a $def`)
          const disc = discriminantOf(def)
          if (!disc) fail(`openapi: ${ctx} union member ${name} has no const-valued discriminant`)
          if (disc.wire !== v.propertyName) {
            fail(`openapi: ${ctx} discriminates on ${v.propertyName} but ${name} tags itself with ${disc.wire}`)
          }
          mapping[disc.value] = '#/components/schemas/' + name
        }
        out.discriminator = { propertyName: v.propertyName, mapping }
        break
      }
      case 'properties': {
        const props = {}
        for (const [pn, pv] of Object.entries(v)) props[pn] = toOpenAPISchema(pv, `${ctx}.${pn}`)
        out.properties = props
        break
      }
      case 'items':
        out.items = toOpenAPISchema(v, `${ctx}[]`)
        break
      case '$schema': case '$id': case '$defs':
        break
      default:
        out[k] = v
    }
  }
  return out
}

function emitOpenAPI() {
  const schemas = {}
  for (const [name, node] of Object.entries(defs)) {
    schemas[name] = toOpenAPISchema(node, name)
    /* CANT-74. OpenAPI 3.0 has no open-enum keyword, so the policy rides as a
     * specification extension: any house generator wired over this spec must
     * pass the `tolerate` vectors for every enum carrying it, or be wrapped. */
    if (clientOpen.has(name)) schemas[name]['x-catenary-client-open'] = true
  }

  const doc = {
    openapi: '3.0.3',
    info: {
      title: schema.title,
      version: `${WIRE_VERSION}.0.0`,
      description: [
        'GENERATED from schema/catenary.wire.v1.schema.json. DO NOT EDIT.',
        '',
        'Regenerate with `npm run gen` from web/; `npm run gen:check` fails in CI on a hand edit.',
        '',
        'This document is the wire schema expressed as OpenAPI 3.0.3, so the house pipeline',
        '(oapi-codegen, openapi-typescript, openapi-generator) can consume the same single source',
        'of truth the bespoke generator does. It is not a second schema and nothing here is',
        'authored: `const` is emitted as a single-value `enum`, every discriminator carries an',
        'explicit mapping, and `#/$defs/` is rewritten to `#/components/schemas/`.',
        '',
        'CANT-12. `paths` is empty on purpose — the REST surface arrives with E2 (CANT-20 /sync,',
        'CANT-28 auth, CANT-48 media). The schemas are the contract that exists now.',
        '',
        schema.description || '',
      ].join('\n').trimEnd(),
    },
    paths: {},
    components: { schemas },
  }

  return yamlEmit(doc).replace(/^\n/, '') + '\n'
}

/* ------------------------------------------------------------------ */

const targets = [
  [join(ROOT, 'web', 'src', 'wire', 'generated.ts'), emitTS()],
  [join(ROOT, 'dart', 'lib', 'src', 'generated.dart'), emitDart()],
  [join(ROOT, 'internal', 'wire', 'generated.go'), gofmt(emitGo())],
  [join(ROOT, 'schema', 'openapi.yaml'), emitOpenAPI()],
]

/* `--dry-run` runs every lint and every emitter and writes nothing; it is how
 * the generator's tests ask "does this schema pass" without touching the tree. */
if (process.argv.includes('--dry-run')) {
  console.log(`ok — schema lints and all ${targets.length} emitters pass`)
  process.exit(0)
}

const check = process.argv.includes('--check')
let stale = 0
for (const [path, content] of targets) {
  if (check) {
    let existing = null
    try { existing = readFileSync(path, 'utf8') } catch { /* absent */ }
    if (existing !== content) {
      console.error(`STALE: ${path.replace(ROOT + '/', '')}`)
      stale++
    }
  } else {
    mkdirSync(dirname(path), { recursive: true })
    writeFileSync(path, content)
    console.log(`wrote ${path.replace(ROOT + '/', '')} (${content.split('\n').length} lines)`)
  }
}
if (check) {
  if (stale) {
    console.error(`\n${stale} generated file(s) do not match the schema. Run \`npm run gen\` and commit the result.`)
    process.exit(1)
  }
  console.log('generated files are current')
}
