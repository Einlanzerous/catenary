/**
 * The ATTACH picker's pre-check (CANT-314, CANT-247 ruling 1 → B).
 *
 * THE SERVER STAYS THE AUTHORITY. `POST /uploads` decides what a body is from
 * its bytes and answers 415 or 413; this is a courtesy so the person learns in
 * the picker, before anything is composed, that a HEIC will not go. The two
 * lists are kept honest by the shared fixtures (`testdata/media`): a JPEG, PNG,
 * GIF or WebP the server takes and a client list that drifted from it fail the
 * same manifest, in Go, here and in Dart.
 *
 * THE TYPE COMES FROM THE FIRST BYTES, never from the extension or the
 * browser's `type`, which is the same rule the server applies (it never reads
 * the client's Content-Type). The messages are the uploader's own, so a file
 * refused here and a file refused by the server's 415 or 413 read the same.
 */

import { UPLOAD_MESSAGES } from '@/upload'

/** The server's image cap, `DefaultMaxImageBytes` in internal/config/config.go
 *  (16 MiB). A deployment can lower it with CATENARY_MAX_IMAGE_BYTES, in which
 *  case the server's 413 is still what answers; the manifest test is what fails
 *  if the DEFAULT moves and this does not. */
export const IMAGE_CAP_BYTES = 16 * 1024 * 1024

/** What the server's ingest takes. Anything else, HEIC and SVG included, is a 415. */
export const ACCEPTED_IMAGE_TYPES = ['image/jpeg', 'image/png', 'image/gif', 'image/webp'] as const
export type AcceptedImageType = (typeof ACCEPTED_IMAGE_TYPES)[number]

/** The picker's `accept` attribute: a hint to the dialog, never the check. */
export const IMAGE_ACCEPT = ACCEPTED_IMAGE_TYPES.join(',')

export type PrecheckResult =
  | { ok: true; type: AcceptedImageType }
  | { ok: false; reason: 'type' | 'size'; message: string }

const ascii = (b: Uint8Array, at: number, s: string) => [...s].every((c, i) => b[at + i] === c.charCodeAt(0))

/** The type of an image from its first twelve bytes, or null. */
export function sniffImage(head: Uint8Array): AcceptedImageType | null {
  if (head[0] === 0xff && head[1] === 0xd8 && head[2] === 0xff) return 'image/jpeg'
  if (head[0] === 0x89 && ascii(head, 1, 'PNG') && head[4] === 0x0d && head[5] === 0x0a && head[6] === 0x1a && head[7] === 0x0a) return 'image/png'
  if (ascii(head, 0, 'GIF87a') || ascii(head, 0, 'GIF89a')) return 'image/gif'
  if (ascii(head, 0, 'RIFF') && ascii(head, 8, 'WEBP')) return 'image/webp'
  return null
}

/** Type first, then size: a HEIC over the cap is a type problem, which is what
 *  the server's ingest would say too. */
export async function precheckImage(file: Blob): Promise<PrecheckResult> {
  const head = new Uint8Array(await file.slice(0, 12).arrayBuffer())
  const type = sniffImage(head)
  if (type === null) return { ok: false, reason: 'type', message: UPLOAD_MESSAGES.unsupportedType }
  if (file.size > IMAGE_CAP_BYTES) return { ok: false, reason: 'size', message: UPLOAD_MESSAGES.tooLarge }
  return { ok: true, type }
}
