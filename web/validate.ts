/* Validate a JSON document against the generated wire codecs.
 *
 *   node dist-validate/validate.js <TypeName> <file.json>
 *
 * Exists so a REAL server response can be held to the schema, not just the
 * hand-written vectors. The vectors prove the three languages agree with each
 * other; this proves an actual implementation agrees with all three, which is
 * the check that catches a server quietly emitting a field the schema forbids
 * or omitting one it requires.
 */

import { readFileSync } from 'node:fs'
import { codecs } from '@/wire/generated'

const [typeName, path] = process.argv.slice(2)
if (!typeName || !path) {
  console.error('usage: validate <TypeName> <file.json>')
  process.exit(2)
}

const codec = codecs[typeName]
if (!codec) {
  console.error(`unknown type ${typeName}. known: ${Object.keys(codecs).sort().join(', ')}`)
  process.exit(2)
}

let raw: unknown
try {
  raw = JSON.parse(readFileSync(path, 'utf8'))
} catch (e) {
  console.error(`cannot read ${path}: ${e instanceof Error ? e.message : e}`)
  process.exit(2)
}

try {
  const decoded = codec.decode(raw)
  if (decoded === null) {
    console.error(`FAIL  ${typeName}: unrecognised discriminator — decoder returned null`)
    process.exit(1)
  }
  console.log(`ok    ${path} is a valid ${typeName}`)
} catch (e) {
  console.error(`FAIL  ${e instanceof Error ? e.message : e}`)
  process.exit(1)
}
