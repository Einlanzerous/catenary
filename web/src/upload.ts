/**
 * The real `Uploader` (CANT-313, under CANT-247): one `POST {origin}/uploads`
 * per attachment, the raw bytes as the body, the handle out of the `201`.
 *
 * ONE REQUEST, ONE PLACE. No presign, no JSON body, and no reliance on a
 * `Content-Type`: the server never reads the header the client chose, it decides
 * what the bytes are from the bytes. `fetch` sets whatever a Blob's own type
 * says and this file does not touch it.
 *
 * THE CREDENTIAL IS THE SESSION'S OWN INSTANCE. `startSession` hands the SAME
 * `CredentialSeam` to the transport and to this uploader, so a refresh by one
 * is not a second presentation of a spent token to the other (CANT-29). The
 * call shape is the one every REST call here already has: refresh when due,
 * Catenary's own 401 read from the BODY (a proxy's 401 is not an answer from
 * Catenary), one retry through `onSyncUnauthorized`, `answered()` on a genuine
 * answer.
 *
 * THE BOUND IS ONE TOTAL DEADLINE, SIZE-SCALED (CANT-247 ruling 2 → A). The
 * outbox keeps no deadline of its own and offers an entry once, so a promise
 * that never settles holds it `pending` until a reload. The deadline covers the
 * whole call including the credential refresh that precedes the request (a hang
 * in `refreshIfDue` must not escape it); on expiry the request is aborted and
 * the call rejects with `DID_NOT_FINISH`, which fails the entry and offers RETRY.
 *
 * THE MESSAGES ARE USER COPY. The outbox stores `String(error)` as the entry's
 * inline error, so every string below is shown to a person; none can contain a
 * token, and the code that builds them never has one in scope but the header.
 */

import { isCatenaryUnauthorized, type CredentialSeam } from '@/transport/credential'
import { browserTimers, type Timers } from '@/transport/seams'
import type { OutboundAttachmentDraft, OutboxEntry, Uploader } from '@/outbox'
import { UploadRefused } from '@/outbox'
import type { Uuid } from '@/wire/generated'

/** The bound's three numbers. JUDGMENTS, NOT MEASUREMENTS: the group's link
 *  quality has not been measured, which is why they sit here as single named
 *  constants a later measurement can change in one place. Not config — nothing
 *  about a phone's link is per deployment. */
export const UPLOAD_BASE_MS = 30_000
/** The slowest link an upload is still expected to finish on: 32 KiB/s, about
 *  256 kbit/s. */
export const UPLOAD_FLOOR_BYTES_PER_SECOND = 32 * 1024
export const UPLOAD_CEILING_MS = 10 * 60_000

/** 30 s plus the time the floor speed needs for `bytes`, capped at 10 minutes.
 *  About 33 s for 100 KB. */
export function uploadDeadlineMs(bytes: number): number {
  const transfer = Math.ceil((Math.max(0, bytes) / UPLOAD_FLOOR_BYTES_PER_SECOND) * 1000)
  return Math.min(UPLOAD_CEILING_MS, UPLOAD_BASE_MS + transfer)
}

/** The words a person sees, one meaning per cause. */
export const UPLOAD_MESSAGES = {
  /** 413: over the size cap, or over the pixel limit. */
  tooLarge: 'That image is too large to send.',
  /** 415: names what the server takes. */
  unsupportedType: "That file type isn't supported. Send a JPEG, PNG, GIF or WebP image.",
  /** 404: a deployment with no media directory has no route at all. */
  unavailable: "Uploads aren't available on this server.",
  /** 401 after the credential layer's repair. */
  signedOut: "You've been signed out. Sign in again to send this.",
  /** 5xx. */
  serverError: "The server couldn't take that upload right now. Try again in a moment.",
  /** A timeout or a network error. */
  didNotFinish: 'The upload did not finish. Check your connection and try again.',
  /** A reply that is not Catenary's: no valid `upload_id`, or a status nothing above names. */
  notCatenary: "The upload did not finish. The reply didn't look like it came from Catenary.",
} as const

export interface UploaderConfig {
  /** The server's origin, as the transport has it. */
  baseUrl: string
  credential: CredentialSeam
  fetch?: typeof globalThis.fetch
  timers?: Timers
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function createUploader(cfg: UploaderConfig): Uploader {
  const base = cfg.baseUrl.replace(/\/+$/, '')
  const timers = cfg.timers ?? browserTimers
  const cred = cfg.credential
  const fetchFn = cfg.fetch ?? ((input: RequestInfo | URL, init?: RequestInit) => globalThis.fetch(input, init))

  const post = async (presented: { accessToken: string }, url: string, blob: Blob, signal: AbortSignal) => {
    const res = await fetchFn(url, {
      method: 'POST',
      headers: { Authorization: `Bearer ${presented.accessToken}` },
      body: blob,
      signal,
    })
    return { status: res.status, body: await res.text() }
  }

  const run = async (url: string, blob: Blob, signal: AbortSignal): Promise<Uuid> => {
    await cred.refreshIfDue()
    if (signal.aborted) throw new UploadRefused(UPLOAD_MESSAGES.didNotFinish)
    const presented = await cred.current()
    if (signal.aborted) throw new UploadRefused(UPLOAD_MESSAGES.didNotFinish)
    let out = await post(presented, url, blob, signal)
    if (isCatenaryUnauthorized(out.status, out.body)) {
      cred.answered()
      if (!(await cred.onSyncUnauthorized(presented))) throw new UploadRefused(UPLOAD_MESSAGES.signedOut)
      if (signal.aborted) throw new UploadRefused(UPLOAD_MESSAGES.didNotFinish)
      out = await post(await cred.current(), url, blob, signal)
      if (isCatenaryUnauthorized(out.status, out.body)) {
        cred.answered()
        throw new UploadRefused(UPLOAD_MESSAGES.signedOut)
      }
    }
    if (out.status === 201) {
      const id = handleOf(out.body)
      if (id === null) throw new UploadRefused(UPLOAD_MESSAGES.notCatenary)
      cred.answered()
      return id
    }
    // Any other status is a genuine answer only when its body is Catenary's;
    // the credential layer's `answered()` is not called for the rest.
    if (out.status === 413) throw new UploadRefused(UPLOAD_MESSAGES.tooLarge)
    if (out.status === 415) throw new UploadRefused(UPLOAD_MESSAGES.unsupportedType)
    if (out.status === 404) throw new UploadRefused(UPLOAD_MESSAGES.unavailable)
    if (out.status >= 500) throw new UploadRefused(UPLOAD_MESSAGES.serverError)
    throw new UploadRefused(UPLOAD_MESSAGES.notCatenary)
  }

  return {
    upload(_entry: OutboxEntry, attachment: OutboundAttachmentDraft): Promise<Uuid> {
      const kind = attachment.kind
      const name = attachment.filename ? `&filename=${encodeURIComponent(attachment.filename)}` : ''
      const url = `${base}/uploads?kind=${kind}${name}`
      const controller = new AbortController()
      return new Promise<Uuid>((resolve, reject) => {
        let settled = false
        const timer = timers.setTimeout(() => {
          if (settled) return
          settled = true
          controller.abort()
          reject(new UploadRefused(UPLOAD_MESSAGES.didNotFinish))
        }, uploadDeadlineMs(attachment.blob.size))
        const finish = (fn: () => void) => {
          if (settled) return
          settled = true
          timers.clearTimeout(timer)
          fn()
        }
        run(url, attachment.blob, controller.signal).then(
          (id) => finish(() => resolve(id)),
          (e: unknown) =>
            finish(() =>
              // A refusal is already user copy. Anything else is a request that
              // never got a status (a network error, an abort): one message, and
              // never the error's own text, which may name the URL.
              reject(e instanceof UploadRefused ? e : new UploadRefused(UPLOAD_MESSAGES.didNotFinish)),
            ),
        )
      })
    },
  }
}

/** The `upload_id` of a `201` body, or null when the body is not Catenary's. */
function handleOf(body: string): Uuid | null {
  try {
    const parsed: unknown = JSON.parse(body)
    if (typeof parsed !== 'object' || parsed === null) return null
    const id = (parsed as { upload_id?: unknown }).upload_id
    return typeof id === 'string' && UUID.test(id) ? id : null
  } catch {
    return null
  }
}
