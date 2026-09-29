/* Enrollment over HTTP — mirrors internal/client/enroll.go.
 *
 * `POST /enroll` redeems an enrollment token and returns the device's first
 * pair, WITH the server's `Date` and the device's clock offset captured from
 * that response: CANT-31's record §1 names `/enroll` beside `/refresh` as the
 * two moments an expiry is learned. A pair built from the body alone has no
 * offset and reads an uncorrected clock against the 60 s floor until its first
 * rotation.
 *
 * This obtains a credential and stores nothing. The caller (CANT-38's login
 * flow) persists what it returns with `enrollCredential`, or
 * `reenrollCredential` when replacing a device the server stopped recognising.
 */

import { decodeEnrollResponse, encodeEnrollRequest } from '@/wire/generated'
import { credentialFromEnroll, type StoredCredential } from './credential-store'
import type { Clock } from './seams'

export interface EnrollOptions {
  /** The server's origin; `/enroll` is appended. */
  baseUrl: string
  fetch?: typeof globalThis.fetch
  now?: Clock
}

/** `POST /enroll` answered with something other than a pair. Every refusal
 *  answers identically by design (CANT-28), so there is nothing finer to say. */
export class EnrollRefused extends Error {
  constructor(readonly status: number) {
    super(`enroll: HTTP ${status}`)
    this.name = 'EnrollRefused'
  }
}

export async function enrollDevice(opts: EnrollOptions, enrollmentToken: string, deviceName: string): Promise<StoredCredential> {
  const fetchFn = opts.fetch ?? ((input, init) => globalThis.fetch(input, init))
  const now = opts.now ?? (() => Date.now())
  const res = await fetchFn(`${opts.baseUrl.replace(/\/+$/, '')}/enroll`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(encodeEnrollRequest({ enrollmentToken, deviceName })),
  })
  const arrived = now()
  const body = await res.text()
  if (res.status !== 200) throw new EnrollRefused(res.status)
  return credentialFromEnroll(decodeEnrollResponse(JSON.parse(body)), res.headers.get('Date'), arrived)
}
