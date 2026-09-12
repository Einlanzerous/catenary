/* TypeScript conformance runner — IDEA-27 (R4).
 *
 * Reads schema/vectors/vectors.json and holds the generated TypeScript codecs
 * to it. The Dart runner (dart/bin/conformance.dart) and the Go runner
 * (server/cmd/conformance) read the SAME file and assert the SAME outcomes.
 *
 * Generating types from one schema makes the clients agree about shape. This
 * is what makes them agree about behaviour, which is where two hand-written
 * implementations actually diverge: whether an absent optional is omitted or
 * nulled, whether an unknown attachment drops the attachment or the message,
 * whether a constraint is enforced or merely documented. None of those is a
 * type error, so none of them is caught by codegen alone.
 *
 * THIS IS A CLIENT-SIDE RUNNER (CANT-74). `tolerate` is the one expectation
 * whose answer differs by side: an enum value this schema version does not
 * know decodes here to the sentinel `unknown` and round-trips as `encoded`,
 * exactly as `roundtrip` does. The Go runner is the server side and treats the
 * same case as `reject`. Which side a runner is on is stated here, once, and
 * the vector file stays one word per case.
 */

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { codecs, setOnUnknownWireValue } from '@/wire/generated'

/* Resolved from the package directory rather than import.meta.url: this file is
 * bundled into dist-conformance/ before it runs, so a path relative to the
 * module would be relative to the build output. npm runs scripts from the
 * package root, which is stable. */
const VECTORS = resolve(process.cwd(), '..', 'schema', 'vectors', 'vectors.json')

interface Case {
  name: string
  kind: string
  expect: 'roundtrip' | 'ignore' | 'reject' | 'tolerate'
  why?: string
  json: unknown
  encoded?: unknown
}

/** Recursively key-sorted JSON, so key ORDER is not asserted and all else is. */
function canonical(v: unknown): string {
  const walk = (x: unknown): unknown => {
    if (Array.isArray(x)) return x.map(walk)
    if (x && typeof x === 'object') {
      const o = x as Record<string, unknown>
      const out: Record<string, unknown> = {}
      for (const k of Object.keys(o).sort()) out[k] = walk(o[k])
      return out
    }
    return x
  }
  return JSON.stringify(walk(v))
}

const fail: string[] = []
const check = (name: string, ok: boolean, detail = '') => {
  if (!ok) fail.push(`${name}${detail ? ` — ${detail}` : ''}`)
  console.log(`${ok ? 'ok  ' : 'FAIL'}  ${name}${detail ? `  (${detail})` : ''}`)
}

const { cases } = JSON.parse(readFileSync(VECTORS, 'utf8')) as { cases: Case[] }

for (const c of cases) {
  const codec = codecs[c.kind]
  if (!codec) {
    check(c.name, false, `no codec for kind ${c.kind}`)
    continue
  }

  if (c.expect === 'reject') {
    let threw = false
    let how = ''
    try {
      codec.decode(c.json)
    } catch (e) {
      threw = true
      how = e instanceof Error ? e.message : String(e)
    }
    check(c.name, threw, threw ? how.slice(0, 90) : 'decoded a frame the schema forbids')
    continue
  }

  let decoded: unknown
  try {
    decoded = codec.decode(c.json)
  } catch (e) {
    check(c.name, false, `threw: ${e instanceof Error ? e.message : String(e)}`)
    continue
  }

  if (c.expect === 'ignore') {
    check(c.name, decoded === null, decoded === null ? 'ignored' : 'decoded an unknown tag')
    continue
  }

  if (decoded === null) {
    check(c.name, false, 'decoder returned null for a known tag')
    continue
  }

  // `roundtrip` and, on this side, `tolerate`: decode, re-encode, compare.
  const want = canonical(c.encoded ?? c.json)
  const got = canonical(codec.encode(decoded))
  check(c.name, want === got, want === got ? '' : `\n    want ${want}\n    got  ${got}`)
}

/* CANT-74 criterion 11: the unknown-value report fires ONCE per (enum, raw) per
 * process. Measured with a value no vector uses, so the vector loop above has
 * not already spent it, and the same frame decoded twice yields one line. */
{
  const seen: string[] = []
  setOnUnknownWireValue((m) => { seen.push(m) })
  const frame = { type: 'resync_required', reason: 'warn_once_probe', log_seq: 1 }
  codecs.ServerFrame.decode(frame)
  codecs.ServerFrame.decode(frame)
  const ok = seen.length === 1 && /ResyncReason: unknown value "warn_once_probe"/.test(seen[0])
  check('unknown_value_is_reported_once_per_enum_and_value', ok, ok ? seen[0] : `${seen.length} report(s): ${seen.join(' | ')}`)
}

/* The probe above is one check beyond the vector file, so the total says so:
 * a failing probe must not read as a failing vector. */
const RUNNER_CHECKS = 1
console.log(
  fail.length
    ? `\n${fail.length} of ${cases.length + RUNNER_CHECKS} FAILED`
    : `\nall green — ${cases.length} vectors + ${RUNNER_CHECKS} runner check`
)
process.exit(fail.length ? 1 : 0)
