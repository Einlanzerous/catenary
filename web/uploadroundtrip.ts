/* `npm run smoke:upload` — the web round trip of an image upload, against a live
 * server (CANT-313, criteria "Pass-through" and "Web round trip" of CANT-247).
 *
 * `cmd/catenary/webupload_test.go` serves `catenary`'s own composition root over
 * a real Postgres and a media directory on a real TCP port, mints an enrollment
 * token, and runs this bundle with the fixture's path. The bundle does what the
 * app does: the login form's own `login()`, `startSession()` over the credential
 * it stored (which builds the REAL uploader and hands it to the outbox through
 * `useTransport`), a composed image attachment, and the outbox's drain. The Go
 * side then reads what the server holds.
 *
 * TWO MODES, ONE BUNDLE. `positive` composes the picked JPEG and waits for it to
 * be sent and acked. `negative` is the pass-through's negative control: it
 * rebuilds the outbox over the live transport with NO uploader option, which is
 * exactly what a composition root that forgot the pass-through does, and
 * expects the same compose to fail with "Attachments can't be sent yet". It
 * proves the positive run is not green by accident.
 */

import { readFileSync } from 'node:fs'
import { accountState, configureAccount, login, onEnrolled } from '@/account'
import { liveTransport, outboxReady, startSession, state, useTransport } from '@/store'
import { MemoryCredentialStore } from '@/transport/credential-store'
import type { Logger } from '@/transport/seams'
import { RefusingUploader, project } from '@/outbox'
import type { Uuid } from '@/wire/generated'

const quiet: Logger = { info: () => {}, warn: (msg, fields) => console.warn(`transport: ${msg}`, fields ?? {}) }

function die(msg: string): never {
  console.log(`FAIL  ${msg}`)
  process.exit(1)
}

async function until(what: string, pred: () => boolean, ms = 20_000, why: () => string = () => ''): Promise<void> {
  const deadline = Date.now() + ms
  while (!pred()) {
    if (Date.now() > deadline) die(`timed out waiting: ${what} ${why()}`)
    await new Promise((r) => setTimeout(r, 25))
  }
}

async function main() {
  const baseUrl = process.env.CATENARY_UPLOAD_BASE_URL
  const token = process.env.CATENARY_UPLOAD_ENROLLMENT_TOKEN
  const conversationId = process.env.CATENARY_UPLOAD_CONVERSATION_ID as Uuid | undefined
  const fixture = process.env.CATENARY_UPLOAD_FIXTURE
  const mode = process.env.CATENARY_UPLOAD_MODE
  if (!baseUrl || !token || !conversationId || !fixture || (mode !== 'positive' && mode !== 'negative')) {
    console.error('uploadroundtrip: run `npm run smoke:upload`, which starts a server and sets CATENARY_UPLOAD_*.')
    process.exit(2)
  }

  // The outbox every module load builds, before any session: startSession
  // replaces it (useTransport) as its LAST step, a moment after the transport
  // reports live, so a compose made in that gap would land in an outbox that is
  // then closed. The session's own is the first one that is not this.
  const initial = outboxReady
  const credentials = new MemoryCredentialStore()
  configureAccount({ baseUrl, store: credentials })
  onEnrolled(() => void startSession({ baseUrl, store: credentials, logger: quiet }))
  if (!(await login(token, 'Web upload'))) die(`login: ${accountState.error ?? 'refused'}`)
  await until('the live transport to be ready and caught up', () =>
    liveTransport() !== null && state.connection.state === 'live' && state.conversations.length > 0)

  await until("startSession to install its outbox", () => outboxReady !== initial)

  if (mode === 'negative') {
    // The broken composition root: the outbox rebuilt over the same live
    // transport, with no uploader to pass.
    await useTransport(liveTransport()!, quiet)
  }

  const bytes = readFileSync(fixture)
  const outbox = await outboxReady
  const entry = await outbox.compose({
    conversationId,
    attachments: [
      {
        kind: 'image',
        blob: new Blob([bytes], { type: 'image/jpeg' }),
        filename: 'photo-exif-gps.jpg',
        upload: 'pending',
      },
    ],
  })
  const itemOf = () => outbox.view().items.find((i) => i.entry.clientId === entry.clientId)

  if (mode === 'positive') {
    // SETTLED IS "SENT" OR GONE: once the sender's own record is held the outbox
    // drops the entry, so on a fast server `sent` is a state it may never be
    // seen in. A `failed` entry stays, and times this out. Whether the record
    // really arrived is the Go side's question, asked of the server.
    await until('the image to be uploaded, sent and acked', () => {
      const i = itemOf()
      return i === undefined || i.state === 'sent'
    }, 20_000, () => {
      const i = itemOf()
      return i ? `(state ${i.state}; ${i.entry.status}; ${JSON.stringify(i.entry.lastError ?? null)})` : '(no item)'
    })
    console.log(`ok    the image was sent with the handle the server minted (client_id ${entry.clientId})`)
    console.log(`RESULT ${JSON.stringify({ clientId: entry.clientId })}`)
    process.exit(0)
  }

  await until('the unwired outbox to fail the attachment', () => itemOf()?.state === 'failed')
  const shown = project(itemOf()!).error
  if (shown !== RefusingUploader.message) die(`the negative control failed with ${JSON.stringify(shown)}, not ${JSON.stringify(RefusingUploader.message)}`)
  console.log(`ok    without the pass-through the same compose fails with "${shown}"`)
  process.exit(0)
}

main().catch((e: unknown) => die(e instanceof Error ? (e.stack ?? e.message) : String(e)))
