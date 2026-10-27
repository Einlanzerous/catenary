/**
 * Link previews on the web (CANT-323, under CANT-308): detection, the one
 * `POST /link-previews` call, and what a card may claim.
 *
 * THE SEND NEVER WAITS ON A PREVIEW. Nothing here is awaited by a send; the
 * composer's state (`store.ts`) is read at the moment of sending, whatever it
 * is, and a request still in flight is simply abandoned.
 *
 * EVERY FAILURE LOOKS THE SAME. The server answers `204` for a blocked
 * address, a timeout, a non-HTML page and a host that does not resolve alike,
 * so that the endpoint cannot be used to ask which internal addresses answer.
 * This file keeps that property on the client: a network error, a 5xx, a body
 * that is not Catenary's, a `400`, a `404` (the feature is off) and a `204`
 * all become `{ kind: 'none' }`, carry no reason, and are never shown. No URL
 * or path from a failed fetch reaches a message, a log line or the DOM.
 *
 * THE CARD NEVER VOUCHES FOR ITSELF. The host a card shows is derived from
 * `url`, never from the page's own `site_name`; title and description are the
 * page's words, rendered as text. Nothing here produces "verified", "safe" or
 * "secure".
 */

import { isCatenaryUnauthorized, type CredentialSeam } from '@/transport/credential'
import { browserTimers, type Timers } from '@/transport/seams'
import { decodeLinkPreviewResponse, type LinkAttachment, type Uuid } from '@/wire/generated'

/* ── detection ───────────────────────────────────────────────────────────── */

/** The rule pinned by the CANT-308 plan, identical in the Dart client: the URL
 *  is the first token matching `https?://` up to the next whitespace, with
 *  trailing `.,;:!?)` trimmed. A draft with no such token requests nothing.
 *  `ftp://` and the like never match. */
export function detectLink(text: string): string | null {
  const hit = /https?:\/\/\S*/.exec(text)
  if (!hit) return null
  const url = hit[0].replace(/[.,;:!?)]+$/, '')
  // `https://` and nothing after it is a URL still being typed.
  return /^https?:\/\/./.test(url) ? url : null
}

/** Whether a detected URL looks finished enough to ask about: it parses and its
 *  host has a dot (a half-typed `https://exa` would otherwise be fetched).
 *  A courtesy only; the server is the authority on what it will fetch. */
export function looksComplete(url: string): boolean {
  try {
    const u = new URL(url)
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.hostname.includes('.') && !u.hostname.endsWith('.')
  } catch {
    return false
  }
}

/** The host a card shows, derived from the URL the sender typed (canvas 08,
 *  "untrusted text"): lowercased, a leading `www.` dropped, the port kept, an
 *  IDN as punycode (`xn--`) so a lookalike host looks different. `URL` already
 *  lowercases and punycodes; the `www.` is ours. Empty when the URL does not
 *  parse, in which case the card shows no host line rather than one it made up.
 *
 *  TODO(CANT-331): the final host after a redirect (`youtu.be` to
 *  `youtube.com`) and the login-wall rule need the server to put the final
 *  host on the wire. Until that ticket lands this is the pasted host and
 *  nothing is guessed here. */
export function hostOf(url: string): string {
  try {
    return new URL(url).host.replace(/^www\./, '')
  } catch {
    return ''
  }
}

/** Words a page chose, made safe to draw (canvas 08): control and bidi
 *  characters (which can reorder what is on screen) stripped, whitespace runs
 *  and newlines collapsed to one space. The result is still only ever drawn as
 *  text. */
const BIDI = /[\u200e\u200f\u202a-\u202e\u2066-\u2069]/g
// eslint-disable-next-line no-control-regex
const SPACE_OR_CONTROL = /[\s\u0000-\u001f\u007f-\u009f]+/g
export function plainText(s: string | null | undefined): string {
  return (s ?? '').replace(BIDI, '').replace(SPACE_OR_CONTROL, ' ').trim()
}

/** A card is opened only when its URL is http or https, whatever a stored
 *  record says: `javascript:` and friends never become an `href`. */
export function openableUrl(url: string): string | null {
  try {
    const u = new URL(url)
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.href : null
  } catch {
    return null
  }
}

/** Whether `url` is on `origin`. The card's image is served by this server's
 *  own `/media`; one that is not is not drawn, so a record can never make the
 *  page contact a third party (and a strict `img-src`, CANT-242, needs none). */
export function sameOrigin(url: string, origin: string): boolean {
  try {
    const base = origin || 'http://invalid.invalid'
    return new URL(url, base).origin === new URL(base).origin
  } catch {
    return false
  }
}

/* ── the call ────────────────────────────────────────────────────────────── */

export type PreviewResult =
  | { kind: 'card'; uploadId: Uuid; preview: LinkAttachment }
  /** No card, for any reason, and none given. */
  | { kind: 'none' }
  /** `429`: ask again no sooner than this. */
  | { kind: 'limited'; retryAfterSec: number }
  /** `404`: the deployment has no previews. Treated as "no previews" for the
   *  rest of the session, without a word to the person. */
  | { kind: 'unavailable' }

export interface LinkPreviewClient {
  /** Never rejects. `signal` abandons the call (the draft moved on). */
  preview(url: string, signal: AbortSignal): Promise<PreviewResult>
}

export interface LinkPreviewConfig {
  baseUrl: string
  credential: CredentialSeam
  fetch?: typeof globalThis.fetch
  timers?: Timers
}

/** The server's whole-request bound is 10 s; this is a little over it. */
export const PREVIEW_DEADLINE_MS = 12_000
/** The composer waits for the draft to be idle this long before asking. */
export const PREVIEW_IDLE_MS = 600
/** A server's `retry_after_sec` is believed within this range. */
const RETRY_MIN_SEC = 1
const RETRY_MAX_SEC = 300

const NONE: PreviewResult = { kind: 'none' }

export function createLinkPreviewClient(cfg: LinkPreviewConfig): LinkPreviewClient {
  const base = cfg.baseUrl.replace(/\/+$/, '')
  const timers = cfg.timers ?? browserTimers
  const cred = cfg.credential
  const fetchFn = cfg.fetch ?? ((input: RequestInfo | URL, init?: RequestInit) => globalThis.fetch(input, init))

  const post = async (presented: { accessToken: string }, url: string, signal: AbortSignal) => {
    const res = await fetchFn(`${base}/link-previews`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${presented.accessToken}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ url }),
      signal,
    })
    return { status: res.status, body: await res.text(), retryAfter: res.headers?.get?.('Retry-After') ?? null }
  }

  const run = async (url: string, signal: AbortSignal): Promise<PreviewResult> => {
    await cred.refreshIfDue()
    if (signal.aborted) return NONE
    const presented = await cred.current()
    let out = await post(presented, url, signal)
    // A 401 is handled like every other call here: Catenary's own, read from
    // the BODY (a proxy's is not an answer from Catenary), one repair, one retry.
    if (isCatenaryUnauthorized(out.status, out.body)) {
      cred.answered()
      if (!(await cred.onSyncUnauthorized(presented))) return NONE
      if (signal.aborted) return NONE
      out = await post(await cred.current(), url, signal)
      if (isCatenaryUnauthorized(out.status, out.body)) {
        cred.answered()
        return NONE
      }
    }
    if (out.status === 200) {
      try {
        const r = decodeLinkPreviewResponse(JSON.parse(out.body))
        cred.answered()
        return { kind: 'card', uploadId: r.uploadId, preview: r.preview }
      } catch {
        return NONE
      }
    }
    if (out.status === 429) return { kind: 'limited', retryAfterSec: retryAfterOf(out.body, out.retryAfter) }
    if (out.status === 404) return { kind: 'unavailable' }
    return NONE
  }

  return {
    preview(url, signal) {
      return new Promise<PreviewResult>((resolve) => {
        let settled = false
        const done = (r: PreviewResult) => {
          if (settled) return
          settled = true
          timers.clearTimeout(timer)
          resolve(r)
        }
        const timer = timers.setTimeout(() => done(NONE), PREVIEW_DEADLINE_MS)
        run(url, signal).then(done, () => done(NONE))
      })
    },
  }
}

function retryAfterOf(body: string, header: string | null): number {
  let sec = Number.NaN
  try {
    const v = JSON.parse(body) as { retry_after_sec?: unknown }
    if (typeof v.retry_after_sec === 'number') sec = v.retry_after_sec
  } catch {
    /* the header below */
  }
  if (!Number.isFinite(sec) && header !== null) sec = Number(header)
  if (!Number.isFinite(sec)) sec = 30
  return Math.min(RETRY_MAX_SEC, Math.max(RETRY_MIN_SEC, Math.ceil(sec)))
}
