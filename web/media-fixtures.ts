/* The shared media fixtures (CANT-312) as TypeScript reads them: the manifest in
 * testdata/media, each file's bytes, and the oversize body, which is GENERATED
 * from the manifest's recipe and never a committed file. Used by the pre-check
 * test and by the live round trip; node-only, so it lives beside the other
 * test entry points and not under src/. */

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

export interface MediaManifest {
  imageCapBytes: number
  files: { name: string; verdict: 'accept' | 'refuse'; status: number; reason?: 'type' | 'size' }[]
  oversize: { verdict: 'refuse'; status: number; reason: 'size'; headerHex: string; padByte: number }
}

export const MEDIA_DIR = resolve(process.cwd(), '../testdata/media')

export const loadManifest = (dir = MEDIA_DIR): MediaManifest =>
  JSON.parse(readFileSync(resolve(dir, 'manifest.json'), 'utf8')) as MediaManifest

export const readFixture = (name: string, dir = MEDIA_DIR): Uint8Array => new Uint8Array(readFileSync(resolve(dir, name)))

/** A valid JPEG header padded to one byte past the cap. */
export function oversizeImage(m: MediaManifest = loadManifest()): Uint8Array {
  const out = new Uint8Array(m.imageCapBytes + 1).fill(m.oversize.padByte)
  out.set(Uint8Array.from(Buffer.from(m.oversize.headerHex, 'hex')))
  return out
}
