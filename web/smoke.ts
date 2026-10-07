/* `npm run smoke` — the app, server-side rendered, asserting that the design
 * canvas's landmarks are actually on screen.
 *
 * AGAINST A LIVE SERVER (CANT-39). The landmarks used to come from
 * `web/src/mock/fixtures.ts`; that file is gone, and every record rendered
 * below arrived the way a browser's would: a real `POST /enroll` through the
 * login form's own `login()`, `startSession()` over the credential it stored,
 * the transport's WebSocket and `/sync` catch-up, and CANT-35's projection of
 * the journal into `state`. The server is `catenary`'s own composition root
 * over a real Postgres, seeded with the canvas's corpus through the store —
 * `cmd/catenary/websmoke_test.go`, which builds nothing by hand that the store
 * has an API for, runs this bundle, and hands it the two facts below.
 *
 * So this file does not start without them: run `npm run smoke`, which starts
 * that server, rather than this bundle on its own.
 *
 * THE LIVE HALF ENDS AT SECTION 6. From there on the sections build pages a
 * server WOULD serve — a room with an offboarded member, a read fraction a
 * rename cannot yet produce — on top of what this one did, and a live session
 * re-projecting underneath them would erase what they add. So the session is
 * ended first, and what it projected stays on screen.
 */

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from '@/App.vue'
import StatusLabel from '@/components/StatusLabel.vue'
import { decodeVoiceAttachment, type Conversation, type User } from '@/wire/generated'
import {
  closeAccount,
  closeSearch,
  conversationTitle,
  endSession,
  lastMessageOf,
  liveTransport,
  messageById,
  newCount,
  outboxMessages,
  outboxReady,
  roomsPendingIn,
  typingIn,
  typingLabel,
  openAccount,
  openSearch,
  searchHits,
  select,
  send,
  startRecording,
  startSession,
  state,
  stopRecording,
  unreadCount,
  voiceOf,
} from '@/store'
import {
  accountState,
  beginReenroll,
  checkExistingCredential,
  configureAccount,
  loadDevices,
  login,
  onEnrolled,
  requestReenrollBeforeMount,
  revokeDevice,
} from '@/account'
import { MemoryCredentialStore } from '@/transport/credential-store'
import { connectionInfo, emptyStats, type TransportStatus } from '@/transport/status'
import { NOT_TERMINAL } from '@/transport/terminal'
import type { Logger } from '@/transport/seams'

const fail: string[] = []
const check = (name: string, ok: boolean, detail = '') => {
  if (!ok) fail.push(`${name}${detail ? ` — ${detail}` : ''}`)
  console.log(`${ok ? 'ok  ' : 'FAIL'}  ${name}${detail ? `  (${detail})` : ''}`)
}

const render = () => renderToString(createSSRApp(App))

/** One thread row, bounded by its own `data-message` and the next one (or the
 *  end of the stream). */
function messageRow(html: string, id: string): string {
  const at = html.indexOf(`data-message="${id}"`)
  if (at < 0) return ''
  const ends = ['data-message="', 'class="persist-notice', 'class="typing', 'class="composer']
    .map((marker) => html.indexOf(marker, at + 1))
    .filter((i) => i > 0)
  return html.slice(at, ends.length ? Math.min(...ends) : undefined)
}
const rowText = (html: string) => html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()

/** Waits for something the live server does, or fails the run naming it. A
 *  hang is not a verdict: every wait here is bounded. */
async function until(what: string, pred: () => boolean, ms = 20_000): Promise<void> {
  const deadline = Date.now() + ms
  while (!pred()) {
    if (Date.now() > deadline) {
      console.log(`FAIL  timed out waiting: ${what}`)
      console.log(`\n${fail.length + 1} FAILED`)
      process.exit(1)
    }
    await new Promise((r) => setTimeout(r, 25))
  }
}

/** The transport's structured lines, kept out of the assertion log unless
 *  something went wrong enough to warn about. */
const quiet: Logger = {
  info: () => {},
  warn: (msg, fields) => console.warn(`transport: ${msg}`, fields ?? {}),
}

async function main() {
  const baseUrl = process.env.CATENARY_SMOKE_BASE_URL
  const enrollmentToken = process.env.CATENARY_SMOKE_ENROLLMENT_TOKEN
  if (!baseUrl || !enrollmentToken) {
    console.error(
      'smoke: CATENARY_SMOKE_BASE_URL and CATENARY_SMOKE_ENROLLMENT_TOKEN are unset.\n' +
        'The smoke renders against a live server: run `npm run smoke`, which seeds one and starts this bundle.',
    )
    process.exit(2)
  }

  // 0. Log in and connect, exactly as the app does: the login form's own
  // `login()` against the real /enroll, and `onEnrolled` starting the session
  // over the pair it stored — the seam main.ts wires.
  const credentials = new MemoryCredentialStore()
  configureAccount({ baseUrl, store: credentials })
  const offEnrolled = onEnrolled(() => void startSession({ baseUrl, store: credentials, logger: quiet }))
  check('the real /enroll accepts the seeded enrollment token', await login(enrollmentToken, 'Web smoke'),
    accountState.error ?? '')
  closeAccount()
  await until('the live transport to be ready and caught up', () =>
    liveTransport() !== null && state.connection.state === 'live' && state.conversations.length > 0)
  check('the transport is live over a real WebSocket', liveTransport()!.status().ready)
  check('state.me is the enrolled account', state.me === (await credentials.read())?.userId)

  // Everything below is found by what a person sees — a room's name, a
  // person's name — because the ids are whatever the server minted.
  const person = (name: string): User => {
    const u = Object.values(state.users).find((x) => x.name === name)
    if (!u) throw new Error(`no user named ${name} was served`)
    return u
  }
  const room = (name: string): Conversation => {
    const c = state.conversations.find((x) => x.kind === 'group' && x.name === name)
    if (!c) throw new Error(`no room named ${name} was served`)
    return c
  }
  const directWith = (name: string): Conversation => {
    const id = person(name).id
    const c = state.conversations.find((x) => x.kind === 'direct' && x.otherMemberId === id)
    if (!c) throw new Error(`no direct with ${name} was served`)
    return c
  }
  const KITCHEN = room('Kitchen Table').id
  const BERGEN = room('Bergen Hill Co-op').id
  const NADIA = person('Nadia Okonkwo').id
  const TED = person('Ted Almasy').id
  const MAREK = person('Marek Dubois').id
  const ROSA = person('Rosa Whitfield').id
  const TED_DM = directWith('Ted Almasy').id

  // A real peer: the server relays Nadia's `typing` from her own socket.
  await until('Nadia\'s typing frame, relayed by the server', () => typingIn(KITCHEN).includes(NADIA))

  // The canvas's failed send in Ted's DM, made the real way: a send the
  // server refuses outright (`message_too_large` fails at once, CANT-36 §6).
  // It is an outbox entry and never a record — the server stored nothing.
  const refused = await (await outboxReady).compose({
    conversationId: TED_DM,
    text: 'the co-op minutes, pasted whole: ' + 'motion carried, seconded, noted. '.repeat(600),
  })
  await until('the server to refuse the oversized send and the outbox to fail it', () =>
    outboxMessages.value.some((m) => m.id === refused.clientId && m.state === 'failed'))

  // 1. The default screen mounts and carries the design's landmarks.
  const main = await render()
  check('mounts', main.length > 2000, `${main.length} bytes`)
  check('rail wordmark', main.includes('CATENARY'))
  // Canvas 07: the 1C "Span" tile sits beside the wordmark — the mask's one
  // curve is the geometry that names the form, and it precedes the word.
  check(
    'logomark tile beside the wordmark',
    main.includes('class="logomark"') && main.includes('M12 15 Q24 27 36 15') &&
      main.indexOf('class="logomark"') < main.indexOf('CATENARY'),
  )
  check('rooms + direct headers', main.includes('ROOMS') && main.includes('DIRECT'))
  check('active thread title', main.includes('Kitchen Table'))
  // The chip states the guarantee that actually holds — for THIS server, which
  // the Go test serves over plain http, so the word is typed here and not
  // computed. The canvas's `· TLS` is asserted for an https origin in the
  // CANT-221 block, once the live half has ended.
  check('the live server is reached over plain http', baseUrl.startsWith('http://'), baseUrl)
  check('so its header reads CLEARTEXT, and never TLS or E2E',
    main.includes('7 MEMBERS · CLEARTEXT') && !main.includes('· TLS') && !main.includes('E2E'))
  check('CTRL+K, no Apple key', main.includes('CTRL+K') && !main.includes('⌘'))

  // Call 10a: typing is the thread's last row, not a strip under the composer.
  const iLastMessage = main.indexOf('Listened to it')
  const iTyping = main.indexOf('typing-line')
  const iComposer = main.indexOf('composer')
  check('typing row exists', iTyping > 0)
  check(
    'typing sits below the last message and above the composer',
    iLastMessage > 0 && iLastMessage < iTyping && iTyping < iComposer,
  )
  const kitchen = state.conversations.find((c) => c.id === KITCHEN)!
  const expectedNew = newCount(kitchen)
  check('unread rule drawn', main.includes(`${expectedNew} NEW`), `${expectedNew} NEW`)
  // You cannot have an unread message you sent.
  const mineAfterRule = state.messages.filter(
    (m) =>
      m.conversationId === KITCHEN &&
      m.seq >= kitchen.firstUnreadSeq! &&
      m.authorId === state.me,
  ).length
  check('own messages excluded from the count', mineAfterRule > 0 && expectedNew ===
    state.messages.filter(
      (m) => m.conversationId === KITCHEN && m.seq >= kitchen.firstUnreadSeq!,
    ).length - mineAfterRule, `${mineAfterRule} of mine skipped`)
  // Deliberate call 03: status as words, not tick glyphs. The two words this
  // render can show are SENT and READ — StatusLabel is guarded `v-if="mine"`
  // and CANT-90 made your own message a two-rung ladder, so `delivered` is
  // only ever the answer for somebody else's message. This asserts the two
  // that appear and that the third does not; the LIVE ladder is checked in
  // section 5, over a send this run makes.
  check('status words, not glyphs', main.includes('SENT') && main.includes('READ'))
  check('and DELIVERED is not one of them', !main.includes('DELIVERED'))
  check('read fraction in a room', main.includes('READ 7/7'))
  // And a PARTIAL one. The canvas draws only n/n, so a corpus of nothing but
  // complete fractions cannot catch a threshold that is off by one.
  check('a partial fraction too, not only n/n', main.includes('READ 4/7'))
  check('transcript collapsed by default', main.includes('EXPAND ·'))
  check('word count is derived', /EXPAND · \d+ W/.test(main))
  check('pending transcript labelled', main.includes('TRANSCRIBING'))
  check('reply stub chip', main.includes('▶ VOICE'))
  check('stub says pending rather than empty', main.includes('transcript pending'))
  check('image filename strip', main.includes('IMG_4471.HEIC'))
  check(
    'waveform bars rendered',
    (main.match(/height:\s*\d+%/g) ?? []).length > 100,
    `${(main.match(/height:\s*\d+%/g) ?? []).length} bars`,
  )

  // 1b. The three typing cases are a rule, not a string. The list is set by
  // hand here — one live peer cannot be four — and the next live frame would
  // replace it, which is why each case reads it back at once.
  const typing = (ids: string[]) => {
    state.typing[KITCHEN] = ids
    return typingLabel(KITCHEN)
  }
  check('typing · one is a first name', typing([NADIA]) === 'Nadia')
  check('typing · two, in the order they started',
    typing([NADIA, TED]) === 'Nadia, Ted')
  check('typing · three still name everyone',
    typing([NADIA, TED, MAREK]) === 'Nadia, Ted, Marek')
  check('typing · four or more drop names',
    typing([NADIA, TED, MAREK, ROSA]) === 'Several people')
  check('typing · nobody renders nothing', typing([]) === null)

  // 2. Rail previews derive from the last message, per conversation kind.
  check('room preview carries a name', main.includes('Rosa: Listened to it'))
  check('own preview says You', main.includes('You: sent it to your inbox instead'))
  // THE FAILED ROW IS AN OUTBOX ENTRY (CANT-161). A send the server never
  // stored cannot be a record in the log — it would need a seq of its own —
  // and this one is the refusal section 0 provoked from the live server.
  const tedRow = elementWithId(main, TED_DM)
  check('failed conversation marked', tedRow.includes('FAILED'), tedRow.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim().slice(0, 120))
  check('and FAILED as a fault, not a label', /class="(fault marker|marker fault)"/.test(tedRow))
  check('the failed row comes from the outbox store',
    lastMessageOf(TED_DM)?.id === refused.clientId)
  check('no record in the log is failed — only an outbox entry can be',
    !state.messages.some((m) => m.state === 'failed'))
  check('muted conversation marked', main.includes('MUTED'))

  // 3. Search finds text and transcripts in one list.
  state.query = 'thursday'
  const hits = searchHits.value
  check('search spans both kinds', hits.some((h) => h.type === 'TXT') && hits.some((h) => h.type === 'VOX'))
  check('transcript hit seeks to a word', hits.some((h) => h.jumpToMs !== undefined),
    `jump=${hits.find((h) => h.jumpToMs !== undefined)?.jumpToMs}ms`)
  openSearch()
  const search = await render()
  check('search view renders', search.includes('RESULTS ·'))
  check('VOX tag present', search.includes('>VOX<'))
  check('JUMP TO present', search.includes('JUMP TO'))

  // 4. Selecting a conversation clears its badge but keeps its unread rule.
  const bergen = state.conversations.find((c) => c.id === BERGEN)!
  check('unread before opening', unreadCount(bergen) > 0, `${unreadCount(bergen)}`)
  select(BERGEN)
  check('reading clears the badge', unreadCount(bergen) === 0)
  check('but keeps the rule', newCount(bergen) > 0, `${newCount(bergen)} NEW`)

  // 5. A send reaches SENT on an ack, and never DELIVERED — on the RENDERED row.
  //
  // CANT-90'S GUARD, KEPT ON THE PATH A PERSON SEES, and now over the live
  // server's own ack. Nothing moves a send to `sent` except an ack (CANT-161
  // deleted the timer that used to), so the transport is stopped first: the
  // send is kept and renders QUEUED, and only once `start()` has brought the
  // session back does the server's ack walk it to SENT.
  select(KITCHEN)
  const transport = liveTransport()!
  transport.stop()
  await until('the stopped transport to stop being ready', () => !transport.status().ready)
  state.composer.draft = 'kept, not sent'
  const keptId = await send()
  const keptRow = messageRow(await render(), keptId!)
  check('with no session, a send renders QUEUED', keptRow.includes('QUEUED'), rowText(keptRow))
  check('and never SENT without an ack', !keptRow.includes('SENT'), rowText(keptRow))
  check('a send is never put in the log', !state.messages.some((m) => m.clientId === keptId))

  transport.start()
  await until('the kept send to be acked by the live server once the session is back', () =>
    outboxMessages.value.some((m) => m.id === keptId && m.state === 'sent') ||
      state.messages.some((m) => m.clientId === keptId))
  const sentPage = await render()
  const record = state.messages.find((m) => m.clientId === keptId)
  const sentRow = messageRow(sentPage, record?.id ?? keptId!)
  check('send authors it as mine', sentRow.includes('You'), rowText(sentRow))
  check('my own message renders SENT on the ack', sentRow.includes('SENT'), rowText(sentRow))
  check('and the rendered row never invents DELIVERED', !sentRow.includes('DELIVERED'), rowText(sentRow))
  check('nor does the page', !sentPage.includes('DELIVERED'))
  await until('the server\'s own record of the send to reach the journal', () =>
    state.messages.some((m) => m.clientId === keptId))
  check('the server stored it under the clientId the entry was minted with',
    state.messages.filter((m) => m.clientId === keptId).length === 1)

  // The live half ends here; see the header. What it projected stays.
  endSession()
  offEnrolled()

  // CANT-221 — the header's transport word is derived from the origin the
  // session talks to, and is not a literal. The live session above stated its
  // origin the way the app does, through `startSession`'s `baseUrl`; it is
  // over, so from here the origin is stated the way every later section states
  // a page a server WOULD serve — on `state`. Kitchen Table is still open.
  {
    const header = async (origin: string) => {
      state.origin = origin
      return (await render()).match(/<span class="members"[^>]*>([^<]*)</)?.[1] ?? ''
    }
    check('the session left the origin it talked to on the store', state.origin === baseUrl, state.origin)
    const cleartext = await header('http://catenary.test')
    check('an http origin reads CLEARTEXT', cleartext === '7 MEMBERS · CLEARTEXT', cleartext)
    const unknown = await header('')
    check('an origin nobody stated does not claim TLS', unknown === '7 MEMBERS · CLEARTEXT', unknown)
    const tls = await header('https://catenary.test')
    check('an https origin reads TLS — the canvas\'s chip', tls === '7 MEMBERS · TLS', tls)
  }
  // STATED, NOT LEFT OVER: every page built from here on is the canvas's, and
  // the canvas is served over https. Sections 6 and 8 read their `· TLS` from
  // this line and from no check above it.
  state.origin = 'https://catenary.test'

  // 6. CANT-137 — a deactivated member is not one the header claims.
  //
  // THE HEADER NEEDS NO CHANGE AND THAT IS THE WHOLE RESULT. `member_count` is
  // now the count of ACTIVE members, computed on the server by one predicate
  // shared with `read_by` (CANT-135 ruling 1); `Thread.vue` renders the number it
  // is given and has no idea anything happened. So this section builds the page a
  // server WOULD serve for a room of seven with one member deprovisioned, and
  // asserts the rendered chip. There is no member list on the wire, which is
  // exactly why the count has to be honest before it leaves the server: a client
  // has nothing to subtract a flag from.
  //
  // THE SIX IS DERIVED, NOT TYPED. Counting a seven-name roster with one of them
  // deactivated is what makes this a test of the rule rather than of a literal —
  // typing `memberCount: 6` and asserting `6 MEMBERS` would pass against any
  // number at all. It is the same arithmetic the server's predicate does, and the
  // same 6 the `sync_response_with_a_deactivated_member` conformance vector
  // carries.
  //
  // SECTION 1'S `7 MEMBERS` LANDMARK IS UNTOUCHED. It asserts against the
  // `main` string captured at the top, and this section adds a NEW conversation
  // rather than editing Kitchen Table — so the shipped corpus, and the canvas's
  // own seven-member room, are exactly what they were.
  const roster = [
    { id: 'u-hollis', deactivated: false },
    { id: 'u-ilse', deactivated: false },
    { id: 'u-nadia', deactivated: false },
    { id: 'u-marek', deactivated: false },
    { id: 'u-ted', deactivated: false },
    { id: 'u-rosa', deactivated: false },
    // Deprovisioned. Their membership row stays (CANT-33 ruling 7), so their old
    // messages keep their author — and they stop being counted.
    { id: 'u-ada', deactivated: true },
  ]
  const active = roster.filter((m) => !m.deactivated).length
  check('the roster is seven with one deactivated', roster.length === 7 && active === 6,
    `${active} of ${roster.length}`)
  state.conversations.push({
    id: 'c-offboarded',
    kind: 'group',
    name: 'Allotment Six',
    memberCount: active,
    headSeq: 1,
  })
  select('c-offboarded')
  const honest = await render()
  check('a deactivated member is not in the header count', honest.includes('6 MEMBERS · TLS'))
  check('and the header still says TLS rather than E2E', !honest.includes('E2E'))
  check(
    'the room of seven is not claimed anywhere on that page',
    !honest.includes('7 MEMBERS'),
    'the count is the server\'s, and Thread.vue renders what it is given',
  )

  // 7. CANT-138 — a deactivated person's old message, and the DM whose other
  //    half is deactivated, are marked; an active person's is not.
  //
  // Petra is deactivated but her membership rows stay (CANT-33 ruling 7):
  // she keeps authorship of her old message in Bergen Hill Co-op, and her
  // direct's only message is hers — the server deactivated her after both
  // were sent, through the store's own DeactivateUser. Both must carry the quiet textual
  // marker this ticket adds, and MAIN — captured at the top against Kitchen
  // Table, where nobody is deactivated — proves an active author gets none.
  check(
    'active authors carry no mark on the default screen',
    !main.includes('DEACTIVATED'),
  )

  select(BERGEN)
  const bergenPage = await render()
  check(
    'a deactivated author is dimmed in a group room',
    bergenPage.includes('Petra Lindqvist') && bergenPage.includes('DEACTIVATED'),
  )

  select(directWith('Petra Lindqvist').id)
  const petraDm = await render()
  check(
    'the DM header marks a deactivated other half',
    petraDm.includes('Petra Lindqvist') && petraDm.includes('DEACTIVATED'),
  )

  // An active DM's header carries no mark — same component, other member.
  select(directWith('Ilse Marchetti').id)
  const ilseDm = await render()
  check('an active DM header carries no mark', !ilseDm.includes('DEACTIVATED'))

  // CANT-248 — a direct conversation's chip is the rail's own word and the
  // app's, `DIRECT · <transport>`, never a count: two is always the answer and
  // it goes false (`1 MEMBERS`) the moment the other half is deactivated.
  const chipOf = (html: string) => html.match(/<span class="members"[^>]*>([^<]*)</)?.[1] ?? ''
  check('a direct header over https reads DIRECT · TLS', chipOf(ilseDm) === 'DIRECT · TLS', chipOf(ilseDm))
  check('and the page never claims 2 MEMBERS or 1 MEMBERS',
    !ilseDm.includes('2 MEMBERS') && !ilseDm.includes('1 MEMBERS'))
  select(TED_DM)
  const tedDm = await render()
  check('the other direct reads DIRECT · TLS too', chipOf(tedDm) === 'DIRECT · TLS', chipOf(tedDm))
  check('and its page has no member count either', !tedDm.includes('2 MEMBERS') && !tedDm.includes('1 MEMBERS'))
  state.origin = 'http://catenary.test'
  const tedDmHttp = await render()
  check('a direct header over http reads DIRECT · CLEARTEXT', chipOf(tedDmHttp) === 'DIRECT · CLEARTEXT',
    chipOf(tedDmHttp))
  check('and over http the page still has no member count',
    !tedDmHttp.includes('2 MEMBERS') && !tedDmHttp.includes('1 MEMBERS'))
  state.origin = 'https://catenary.test'
  check('the DM whose other half is deactivated reads DIRECT · TLS beside the tag',
    chipOf(petraDm) === 'DIRECT · TLS' && petraDm.includes('DEACTIVATED'), chipOf(petraDm))
  check('and never 1 MEMBERS', !petraDm.includes('1 MEMBERS') && !petraDm.includes('2 MEMBERS'))

  // 8. CANT-145 — the read fraction is rendered by the wire's rule:
  //    `min(read_by, member_count)` over `member_count` (CANT-140 ruling 2).
  //
  // THE ROWS ARE THE SERVER'S, NOT THIS FILE'S. server/spec/testdata/
  // read-fraction.json is the plan's five-row table, and the Go harness
  // (cmd/catenary/readfraction_test.go) constructs every row live over real
  // /sync pages and asserts the same `clamped` column. Reading the one file
  // here is what makes the web's clamp and the server's statement of it a
  // single rule with two readers rather than two rules that agree today —
  // and E5's Flutter renderer is meant to read it third.
  //
  // EACH ROW IS RENDERED THROUGH StatusLabel ITSELF, with `readBy` from the
  // message as last served and `memberCount` from the conversation as last
  // served, which is exactly the pair a client holds after an offboard: the
  // conversation record rode the catch-up, the message did not.
  const fixturePath = resolve(process.cwd(), '../server/spec/testdata/read-fraction.json')
  const fixture = JSON.parse(readFileSync(fixturePath, 'utf8')) as {
    rule: string
    rows: {
      id: string
      held: { read_by: number; member_count: number }
      fresh_member_count: number
      unclamped: string
      clamped: string
    }[]
  }
  check('the read-fraction fixture has the plan\'s five rows', fixture.rows.length === 5,
    `${fixture.rows.length} rows from ${fixturePath}`)
  const label = (readBy: number | undefined, memberCount: number) =>
    renderToString(
      createSSRApp(StatusLabel, {
        message: {
          id: 'm-fixture', seq: 1, conversationId: 'c-fixture', authorId: state.me,
          at: '2026-09-24T00:00:00.000Z', state: 'read', readBy,
        },
        memberCount,
      }),
    )
  const text = (html: string) => html.replace(/<[^>]+>/g, '').trim()
  let sawAClampThatBit = false
  for (const row of fixture.rows) {
    const rendered = await label(row.held.read_by, row.fresh_member_count)
    check(`row ${row.id} renders READ ${row.clamped}`, rendered.includes(`READ ${row.clamped}`), text(rendered))
    if (row.unclamped !== row.clamped) {
      sawAClampThatBit = true
      check(`row ${row.id} does not render the unclamped READ ${row.unclamped}`,
        !rendered.includes(`READ ${row.unclamped}`))
    }
  }
  check('at least one row has a numerator above the room, so the clamp was exercised', sawAClampThatBit)

  // THE 7/6 CASE ON A WHOLE PAGE, not only through the component: a room the
  // client holds at six members with an own message it was served at seven
  // readers. Added as a NEW conversation, so every landmark above is untouched.
  state.conversations.push({
    id: 'c-clamped', kind: 'group', name: 'Allotment Seven', memberCount: 6, headSeq: 1,
  })
  state.messages.push({
    // logSeq is required on the wire type and unread by anything this section
    // asserts; any value above the fixtures' own is honest (CANT-34 leftover).
    id: 'm-clamped', seq: 1, logSeq: 1_000_001, conversationId: 'c-clamped', authorId: state.me,
    at: '2026-09-24T00:00:00.000Z', state: 'read', readBy: 7,
    text: 'read by all seven, then one of them was deprovisioned',
  })
  select('c-clamped')
  const clampedPage = await render()
  check('a held 7 over a fresh 6 renders READ 6/6 on the page', clampedPage.includes('READ 6/6'))
  check('and READ 7/6 appears nowhere', !clampedPage.includes('READ 7/6'))
  check('the header agrees with the denominator', clampedPage.includes('6 MEMBERS · TLS'))

  // THE TWO-MEMBER GUARD IS PINNED: a direct conversation renders bare READ
  // and no fraction, whatever the count says — `memberCount > 2 && readBy`
  // is the guard this ticket keeps, and a clamp that reached it would render
  // `READ 2/2` in every DM.
  const dm = text(await label(2, 2))
  check('a two-member room renders bare READ', dm === 'READ', dm)
  const dmClamped = text(await label(3, 2))
  check('and stays bare even with a numerator above two', dmClamped === 'READ', dmClamped)

  // 8b. CANT-227 — the transcript strip says what the server said, and no more.
  //
  // PLANTED, AND THROUGH THE WIRE'S OWN DECODER. The seed cannot serve these:
  // `transcript_state` is CHECKed to pending/ready/failed, so no row can hold
  // a state this build does not know, and a failed note seeded into the
  // canvas's rooms would move the counts every landmark above reads. So each
  // attachment is the JSON a server would send, decoded by the generated
  // decoder — which is what turns `superseded` into the sentinel `unknown`.
  // The failed and unknown ones carry TEXT, which no honest server sends:
  // it is there so that a surface quoting it regardless of state is caught.
  // A new conversation, removed again at the end, so nothing else is touched.
  {
    const ROOM = 'c-transcripts'
    const STALE = 'stale words from a job that did not finish'
    const planted = { ready: 'm-t-ready', unknown: 'm-t-unknown', failed: 'm-t-failed' }
    const note = (id: string, seq: number, ms: number, transcript: Record<string, unknown>) =>
      state.messages.push({
        id, seq, logSeq: 1_000_100 + seq, conversationId: ROOM, authorId: MAREK,
        at: `2026-09-24T00:0${seq}:00.000Z`, state: 'read',
        attachments: [decodeVoiceAttachment({
          kind: 'voice', url: `/media/${id}.opus`, duration_ms: ms,
          peaks: Array.from({ length: 96 }, (_, i) => 9 + ((i * 37) % 91)), transcript,
        })],
      })
    const before = state.activeId
    const queryBefore = state.query
    state.conversations.push({ id: ROOM, kind: 'group', name: 'Signal Box', memberCount: 7, headSeq: 3 })
    note(planted.ready, 1, 12_000, { state: 'ready', text: 'the gauge is in the blue case' })
    note(planted.unknown, 2, 23_000, { state: 'superseded', text: STALE })
    note(planted.failed, 3, 41_000, { state: 'failed', text: STALE })
    select(ROOM)

    /** The note's own block and nothing of the row around it: from the
     *  player's button to the row's last whole tag. */
    const noteOf = (page: string, id: string) => {
      const row = messageRow(page, id)
      return row.slice(Math.max(0, row.indexOf('aria-label="Play"')), row.lastIndexOf('<'))
    }
    /** Everything under the player: the strip's container, when one is drawn,
     *  up to its own closing tag — the row's status line follows it. */
    const stripOf = (noteHtml: string) => {
      const at = noteHtml.indexOf('<div class="transcript"')
      if (at < 0) return ''
      const divs = /<(\/?)div\b/g
      divs.lastIndex = at
      let depth = 0
      for (let m = divs.exec(noteHtml); m; m = divs.exec(noteHtml)) {
        depth += m[1] ? -1 : 1
        if (depth === 0) return noteHtml.slice(at, m.index)
      }
      return noteHtml.slice(at)
    }
    const classesIn = (html: string) =>
      [...html.matchAll(/class="([^"]*)"/g)].flatMap((m) => m[1].split(/\s+/)).filter(Boolean)
    const OFFERS = ['EXPAND', 'COLLAPSE', 'COPY', 'REPORT BAD TRANSCRIPT', 'TRANSCRIBING', '<p class="body', STALE]
    const offers = (html: string) => OFFERS.filter((word) => html.includes(word))

    // Twice: as the thread first draws it, and with EXPAND already pressed —
    // the fault had both halves, `EXPAND · 0 W` collapsed and an empty body
    // over COPY and REPORT expanded.
    for (const pressed of [false, true]) {
      const when = pressed ? 'expanded' : 'collapsed'
      for (const id of Object.values(planted)) {
        if (pressed) state.expandedTranscripts.add(id)
      }
      const page = await render()
      const ready = noteOf(page, planted.ready)
      const failed = noteOf(page, planted.failed)
      const unknown = noteOf(page, planted.unknown)

      check(`${when} · control: a ready transcript offers its text`,
        ready.includes('the gauge is in the blue case') &&
          (pressed
            ? ['COLLAPSE', 'COPY', 'REPORT BAD TRANSCRIPT'].every((w) => offers(ready).includes(w))
            : rowText(ready).includes('EXPAND · 7 W')), rowText(ready))
      check(`${when} · a failed transcript draws the strip, and it says NO TRANSCRIPT and nothing else`,
        rowText(stripOf(failed)) === 'NO TRANSCRIPT', rowText(failed))
      check(`${when} · no EXPAND, no word count, no body, no COPY, no REPORT, no stale text`,
        offers(failed).length === 0 && !/\d+ W\b/.test(rowText(failed)), offers(failed).join(', '))
      // `.label` is the meta-color text style (VoiceNote.vue); the pulse, the
      // accent label and every pressable thing have classes of their own, and
      // none of them may be here.
      check(`${when} · in the meta color: the strip holds one label and no pulse, accent or action`,
        classesIn(stripOf(failed)).sort().join(' ') === 'label strip transcript' &&
          !stripOf(failed).includes('<button'), classesIn(stripOf(failed)).join(' '))
      check(`${when} · a failed note is still a note: its player and length are drawn`,
        failed.includes('aria-label="Play"') && rowText(failed).includes('0:41'), rowText(failed))
      check(`${when} · an unknown state draws no strip at all`,
        unknown.includes('aria-label="Play"') && stripOf(unknown) === '' &&
          !unknown.includes('TRANSCRIPT') && offers(unknown).length === 0, rowText(unknown))

      if (pressed) continue
      // The rail, for a note the server holds, says what the app's does
      // (`preview` in app/lib/store/conversation.dart): only a pending note
      // says "transcript pending". Already true before CANT-227; pinned here.
      const rail = rowText(elementWithId(page, ROOM))
      check('the rail previews a failed note as `voice note · m:ss`',
        rail.includes('Marek: voice note · 0:41') && !rail.includes('pending'), rail)
    }
    check('the state this build does not know decoded to the sentinel',
      voiceOf(messageById(planted.unknown))?.transcript.state === 'unknown')

    // No other surface quotes a transcript the strip says is not there.
    state.query = 'stale words'
    check('search finds no transcript in a failed or unknown note', searchHits.value.length === 0,
      searchHits.value.map((h) => `${h.type} ${h.message.id}`).join(', '))
    state.query = 'blue case'
    check('control: and still finds a ready one', searchHits.value.some((h) => h.message.id === planted.ready))
    state.query = queryBefore

    state.messages.splice(state.messages.findIndex((m) => m.id === planted.failed), 1)
    const rail = rowText(elementWithId(await render(), ROOM))
    check('and the rail previews an unknown one the same way',
      rail.includes('Marek: voice note · 0:23') && !rail.includes('pending'), rail)

    // Take out what was planted: later sections search every held message.
    for (const id of Object.values(planted)) {
      state.expandedTranscripts.delete(id)
      const at = state.messages.findIndex((m) => m.id === id)
      if (at >= 0) state.messages.splice(at, 1)
    }
    state.conversations.splice(state.conversations.findIndex((c) => c.id === ROOM), 1)
    if (before) select(before)
  }

  // 8c. CANT-235 — a voice note that has not left this device is not
  // "transcript pending": nothing is transcribing it. The rail says what the
  // app's `preview` says of an unsent note (app/lib/store/conversation.dart,
  // pinned by app/test/rail_thread_test.dart): `You: voice note · m:ss`.
  //
  // THE ENTRY IS REAL: RECORD then SEND, through the outbox, in a room planted
  // for it and removed again. Whatever the outbox then does with the entry —
  // hold it QUEUED, or fail it with the refusing uploader's message once
  // CANT-162 lands — it has not reached the server, and the rail must not say
  // the server is working on it. What sits UNDER THE PLAYER for the same note
  // is not checked here: the app draws TRANSCRIBING there too, so what the two
  // clients should say is a decision the ticket's comments carry, not a line
  // this smoke can hold.
  {
    const ROOM = 'c-unsent-note'
    const before = state.activeId
    state.conversations.push({ id: ROOM, kind: 'group', name: 'Platform End', memberCount: 3, headSeq: 0 })
    select(ROOM)
    startRecording()
    await stopRecording(true)
    await new Promise((r) => setTimeout(r, 0))
    const unsent = outboxMessages.value.find((m) => m.conversationId === ROOM)
    check('RECORD then SEND composed one voice entry that has not reached the server',
      unsent !== undefined && unsent.state !== 'sent' && voiceOf(unsent) !== undefined, unsent?.state)
    const page = await render()
    const rail = rowText(elementWithId(page, ROOM))
    check('the rail previews an unsent voice note as `You: voice note · m:ss`, as the app does',
      rail.includes('You: voice note · 0:01'), rail)
    check('and never as transcript pending', !rail.includes('pending'), rail)
    // Take it out again: a settle is how an entry leaves the outbox in any
    // status, and later sections count what the outbox holds.
    if (unsent) await (await outboxReady).settle(unsent.id)
    state.conversations.splice(state.conversations.findIndex((c) => c.id === ROOM), 1)
    if (before) select(before)
  }

  // 9. CANT-141 — otherMemberId, not the two guesses it replaces.
  //
  // Oskar's direct is held with a Conversation.name reading 'Ted Almasy' — an
  // existing ACTIVE person's name — and Wren's with 'Petra Lindqvist' — an
  // existing DEACTIVATED person's. That is the record a client holds after a
  // rename: the wire says a held Conversation is NOT re-emitted when the other
  // member's name changes, while their User record is. This server has no
  // rename yet (nothing may UPDATE display_name outside metadata.go, and
  // metadata.go has no rename), so the stale half is stamped on the held
  // record below, after the live session has ended — the one thing in this
  // section a server did not send. Both directs hold only state.me's own messages, so
  // the deleted author-scan guess never had a candidate on either, and the
  // deleted name-match guess would have found the WRONG person on both: an
  // active Ted for a conversation whose real other half (Oskar) is
  // deactivated, and a deactivated Petra for one whose real other half
  // (Wren) is active. otherMemberId is immune to both, because it names the
  // person rather than guessing from what is on screen.
  //
  // nearId BOUNDS AN ASSERTION TO ONE ROW, which is what "asserted by id"
  // means on a rendered HTML string with no DOM to query: the rail lists
  // every conversation on every page, and c-ted's and c-petra's OWN real rows
  // still carry their own real names elsewhere on the same render — so
  // "the stale name is absent" is never asserted globally, only that the
  // bounded window around this one row's data-conversation-id shows the live
  // name and not the borrowed one.
  // Bounds an assertion to the WHOLE <button> that carries this
  // data-conversation-id — ConversationRow's row and SearchView's hit are
  // both one, with no nested <button> inside either template, so "the next
  // </button> after the marker" is the element's own real close rather than
  // a fixed radius that can end before content nested a few spans deep (the
  // room label, the pending-note text) is reached.
  function elementWithId(html: string, id: string): string {
    const marker = `data-conversation-id="${id}"`
    const at = html.indexOf(marker)
    if (at < 0) return ''
    const tagStart = html.lastIndexOf('<button', at)
    const closeAt = html.indexOf('</button>', at)
    if (tagStart < 0 || closeAt < 0) return ''
    return html.slice(tagStart, closeAt + '</button>'.length)
  }

  // The rail is not what search or the thread pane replaces — it is a
  // persistent sidebar, rendered on every page including a search results
  // page, and every row on it carries the SAME data-conversation-id marker
  // elementWithId keys on. So a search-page assertion has to look inside the
  // results list specifically, or the first match is the rail's row for that
  // id, not the hit — searchResultsOnly slices from SearchView's own landmark
  // (section 3's own 'RESULTS ·', which nothing in the rail renders) onward.
  function searchResultsOnly(html: string): string {
    return html.slice(html.indexOf('RESULTS ·'))
  }

  // The header is not the only place its own text can appear, either — a
  // real other person can share the words this DM's title happens to be
  // titled with (c-ted's OWN row legitimately reads "Ted Almasy"). tagText
  // bounds the check to the one tag that is unambiguous per render: there is
  // exactly one `<h1 class="title">` on any page, because only one thread is
  // ever active.
  function tagText(html: string, openTag: string, closeTag: string): string {
    const at = html.indexOf(openTag)
    if (at < 0) return ''
    const start = html.indexOf('>', at) + 1
    const end = html.indexOf(closeTag, start)
    return html.slice(start, end)
  }

  // The derivation itself, independent of any rendering: conversationTitle
  // resolves each DM's REAL other member, not the string its own `name`
  // field happens to carry.
  const cOskar = directWith('Oskar Lindgren')
  const cWren = directWith('Wren Castellano')
  check('the server serves a direct\'s name fresh, so a stale one is a held record\'s',
    cOskar.name === 'Oskar Lindgren' && cWren.name === 'Wren Castellano', `${cOskar.name}, ${cWren.name}`)
  cOskar.name = 'Ted Almasy'
  cWren.name = 'Petra Lindqvist'
  const OSKAR_DM = cOskar.id
  const WREN_DM = cWren.id
  check('conversationTitle(c-oskar) is Oskar\'s own name', conversationTitle(cOskar) === 'Oskar Lindgren',
    conversationTitle(cOskar))
  check('conversationTitle(c-wren) is Wren\'s own name', conversationTitle(cWren) === 'Wren Castellano',
    conversationTitle(cWren))

  // Site 1 (header) + site 2 (composer placeholder): only one thread is ever
  // active per render, so no id-scoping is needed — there is nothing else on
  // the page these strings could belong to.
  select(OSKAR_DM)
  const oskarPage = await render()
  const oskarTitle = tagText(oskarPage, '<h1 class="title"', '</h1>')
  check('oskar\'s DM is titled by his own live name, not the DM\'s stale name',
    oskarTitle === 'Oskar Lindgren', oskarTitle)
  check('oskar\'s DM header marks him deactivated', oskarPage.includes('DEACTIVATED'))
  check('the composer placeholder names oskar, not the stale name',
    oskarPage.includes('Message Oskar Lindgren') && !oskarPage.includes('Message Ted Almasy'))
  // Site 3 (rail row), bounded to c-oskar's own row: c-ted's real row, live
  // elsewhere on this same page, is not what this assertion is about.
  const oskarRow = elementWithId(oskarPage, OSKAR_DM)
  check('oskar\'s rail row is titled by his own live name', oskarRow.includes('Oskar Lindgren'), oskarRow)
  check('and not by the DM\'s stale name', !oskarRow.includes('Ted Almasy'), oskarRow)

  select(WREN_DM)
  const wrenPage = await render()
  const wrenTitle = tagText(wrenPage, '<h1 class="title"', '</h1>')
  check('wren\'s DM is titled by her own live name, not the DM\'s stale name',
    wrenTitle === 'Wren Castellano', wrenTitle)
  check('wren is active, so her DM header carries no mark', !wrenPage.includes('DEACTIVATED'))

  // CANT-248 — a kind this build does not know takes the count, never DIRECT.
  // Stamped on the held record the way `name` is above, and put back.
  cWren.kind = 'unknown'
  const unknownKind = chipOf(await render())
  check('an unknown kind reads its count, not DIRECT',
    unknownKind === `${cWren.memberCount} MEMBERS · TLS`, unknownKind)
  check('and never DIRECT', !unknownKind.includes('DIRECT'))
  cWren.kind = 'direct'
  const wrenRow = elementWithId(wrenPage, WREN_DM)
  check('wren\'s rail row is titled by her own live name', wrenRow.includes('Wren Castellano'), wrenRow)
  check('and not by the DM\'s stale name', !wrenRow.includes('Petra Lindqvist'), wrenRow)

  // Site 4 — the ordinary text match: the hit's room label follows the id,
  // not the DM's stale `name`.
  state.query = 'spelunking'
  openSearch()
  const oskarSearch = await render()
  const oskarHit = elementWithId(searchResultsOnly(oskarSearch), OSKAR_DM)
  check('a text hit in oskar\'s DM is found', oskarHit !== '')
  check('and its room label is oskar\'s live name', oskarHit.includes('Oskar Lindgren'), oskarHit)
  check('never the DM\'s stale name', !oskarHit.includes('Ted Almasy'), oskarHit)

  // Site 5 — the pending-voice name-match fallback. Searching OSKAR'S OWN
  // name cannot match through the author branch (both messages are
  // state.me's), so a hit here can ONLY come from conversationTitle matching
  // — which conversation.name ('Ted Almasy') never would have.
  state.query = 'oskar'
  const oskarNameSearch = await render()
  const oskarNameHit = elementWithId(searchResultsOnly(oskarNameSearch), OSKAR_DM)
  check('searching oskar\'s own name finds his pending voice note',
    oskarNameHit.includes('NOT SEARCHABLE YET'), oskarNameHit)
  check('by title, not by the stale conversation.name', !oskarNameHit.includes('Ted Almasy'), oskarNameHit)

  state.query = 'wren'
  const wrenNameSearch = await render()
  const wrenNameHit = elementWithId(searchResultsOnly(wrenNameSearch), WREN_DM)
  check('searching wren\'s own name finds her pending voice note',
    wrenNameHit.includes('NOT SEARCHABLE YET'), wrenNameHit)
  check('by title, not by the stale conversation.name', !wrenNameHit.includes('Petra Lindqvist'), wrenNameHit)
  closeSearch()

  // 10. CANT-35 criterion 30 — a terminal client claims nothing will queue.
  //
  // CANT-31 §6: a terminal state ends only on a relaunch or a re-enrollment,
  // so a message composed in one drains nowhere. Every non-live state used to
  // read as "will queue" (Composer's `offline`, store's `queued`), which the
  // moment `terminal` exists is the claim Invariant 3 forbids. The state is set
  // directly: the live session has ended, so nothing overwrites it, and a
  // real terminal is the transport's own tests' to provoke (CANT-151).
  select(KITCHEN)
  const queueClaims = (html: string) =>
    ['messages will queue', 'will send when reconnected'].filter((p) => html.includes(p))
      .concat(/>\s*QUEUE\s*</.test(html) ? ['a QUEUE button'] : [])
  for (const kind of ['credential', 'protocol'] as const) {
    state.connection.state = 'terminal'
    state.connection.terminal = { kind, reason: `smoke: a ${kind} terminal` }
    const page = await render()
    const claims = queueClaims(page)
    check(`terminal ${kind} · the page says nothing will queue`, claims.length === 0, claims.join(', '))
    check(`terminal ${kind} · the banner names the kind`, page.includes(kind.toUpperCase()) && page.includes('Nothing sends until then'))
    check(`terminal ${kind} · and offers no retry it cannot keep`, !page.includes('RETRY NOW') && !page.includes('RECONNECT<'))
    // CANT-38: a CREDENTIAL terminal is the one ConnectionBanner can actually
    // do something about — RE-ENROLL opens the account view built there. A
    // PROTOCOL terminal offers no such button; re-enrolling cannot fix a
    // client the server refuses to speak to at all.
    check(`terminal ${kind} · RE-ENROLL appears only for a credential terminal`,
      page.includes('RE-ENROLL') === (kind === 'credential'))
    const before = state.messages.length
    // Since CANT-161 a send lands in the outbox, never in state.messages, so
    // the outbox is where "not created" has to be read.
    const entriesBefore = outboxMessages.value.length
    state.composer.draft = `composed while ${kind} terminal`
    await send()
    await new Promise((r) => setTimeout(r, 0))
    const created = state.messages.slice(before)
    check(`terminal ${kind} · a send composed now is not created as queued`,
      created.every((m) => m.state !== 'queued'), `${created.length} created: ${created.map((m) => m.state).join(',')}`)
    check(`terminal ${kind} · and the outbox gains no entry`, outboxMessages.value.length === entriesBefore,
      `${outboxMessages.value.length - entriesBefore} created`)
    check(`terminal ${kind} · and the draft stays where the person can see it`,
      state.composer.draft === `composed while ${kind} terminal`)
    state.composer.draft = ''
  }
  state.connection.terminal = undefined

  // 11. CANT-37 — resync shows real numeric progress toward `head_seq`, never
  // a spinner, and never a 0 / 0 claim. Driven here by fake `TransportStatus`
  // values through the real `connectionInfo` adapter — the one `startSession`
  // feeds the live transport's status through — because a live resync is over
  // before a render could catch it.
  {
    const now = 1_000_000
    const fixture = (extra: Partial<TransportStatus>): TransportStatus => ({
      terminal: NOT_TERMINAL, refreshHold: 'none', tokenRefused: false, nextRefreshAt: null,
      connected: true, ready: true, sessionId: 'smoke', heartbeatIntervalSec: 35, missedPongLimit: 2,
      caughtUp: false, cursor: 0, attempt: 0, nextDialAt: null, stats: emptyStats(),
      messages: 0, wipes: 0, headSeqTotal: 0, journalError: null, ...extra,
    })
    // `synced`/`total`/`roomsPending` are genuinely absent, not 0, whenever
    // `connectionInfo` omits them — the banner's `v-if="c.total !== undefined"`
    // is what tells a real absence from a real zero, so the test must too.
    const apply = (info: ReturnType<typeof connectionInfo>) => {
      state.connection.state = info.state
      state.connection.synced = info.synced as number
      state.connection.total = info.total as number
      state.connection.roomsPending = info.roomsPending as number
    }

    // Before any page has landed there is no target yet — 0 / 0 would claim
    // "nothing to do" before this client has looked, so the banner shows no
    // number at all rather than a false one.
    apply(connectionInfo(fixture({}), { now }))
    const nothingYetPage = await render()
    check('resyncing with no page landed yet shows no number, not 0 / 0',
      nothingYetPage.includes('Reconnected — catching up') && !/\d+ \/ \d+ messages/.test(nothingYetPage))

    // A client back from six hours offline: it already held 823 of what the
    // conversations it has learned about so far say is a 1,000-message log.
    apply(connectionInfo(fixture({ messages: 823, headSeqTotal: 1000 }), { now }))
    const resyncPage = await render()
    check('resyncing banner counts real progress toward head_seq, not a spinner',
      resyncPage.includes('823 / 1,000 messages'))

    // It reaches head_seq: the transport reports caughtUp, and the resync
    // line is gone.
    apply(connectionInfo(fixture({ caughtUp: true, messages: 1000, headSeqTotal: 1000 }), { now }))
    const caughtUpPage = await render()
    check('once it reaches head_seq the resync banner is gone', !caughtUpPage.includes('catching up'))
    check('and the state reads live', state.connection.state === 'live')

    // CANT-169: a journal write that did not land is said, beside the
    // session's own state and never instead of it, until a write lands.
    state.connection = connectionInfo(fixture({
      caughtUp: true, journalError: { name: 'QuotaExceededError', message: 'the quota was exceeded' },
    }), { now })
    const quotaPage = await render()
    check('a journal write that failed is on the page, by name',
      quotaPage.includes('could not save messages') && quotaPage.includes('QuotaExceededError'))
    check('and the session state beside it is still its own', state.connection.state === 'live')
    state.connection = connectionInfo(fixture({ caughtUp: true }), { now })
    check('and the line is gone once a write lands', !(await render()).includes('could not save messages'))

    // ROOMS PENDING is derived from the records held (CANT-37's loose end,
    // closed by CANT-39): a room is pending while it holds fewer messages
    // than its head_seq says exist. Over what the live server served, every
    // room is whole; a room whose head has moved past what is held is not.
    const served = state.conversations.filter((c) => !c.id.startsWith('c-'))
    const heldServed = state.messages.filter((m) => served.some((c) => c.id === m.conversationId))
    check('every room the server served is whole, so none is pending',
      roomsPendingIn(served, heldServed) === 0, `${roomsPendingIn(served, heldServed)} of ${served.length}`)
    const behind = served.map((c) => (c.id === KITCHEN ? { ...c, headSeq: c.headSeq + 40 } : c))
    check('a room whose head_seq is past what is held counts as pending',
      roomsPendingIn(behind, heldServed) === 1)

    state.connection.synced = 0
    state.connection.total = 0
    state.connection.roomsPending = 0
  }

  // The two states that DO drain keep their wording exactly.
  state.connection.state = 'offline'
  const offlinePage = await render()
  check('offline still says messages will queue', offlinePage.includes('Offline — messages will queue'))
  check('offline composer still offers QUEUE', /\>\s*QUEUE\s*\</.test(offlinePage))
  state.connection.state = 'reconnecting'
  const reconnectingPage = await render()
  check('reconnecting still says it will send when reconnected',
    reconnectingPage.includes('will send when reconnected') && reconnectingPage.includes('Connection lost — reconnecting'))
  state.connection.state = 'live'

  // 12. CANT-38 — the auth UI: log in (enroll), name the device, see the
  // session list, revoke one. Conventional forms over CANT-28/29/30/117's
  // REST endpoints, driven directly rather than through simulated clicks —
  // the same idiom every other section above already uses (select(), send(),
  // typingLabel()). The credential layer is CANT-152's; this only exercises
  // what CANT-38 adds on top of it, over a scripted fetch rather than a
  // server. Section 0's `onEnrolled` listener is gone by now, so none of
  // these logins starts a transport against a server that is not there.
  {
    const GOOD_TOKEN = 'a-real-enrollment-token'
    // Wire Uuids, not slugs — the generated decoder enforces the schema's
    // pattern (CANT-106), and the whole point of using it here rather than a
    // hand-rolled parse is that a malformed id is caught the same way a real
    // server's would be.
    const USER = '00000000-0000-4000-8000-000000000001'
    const DEVICE_LIVE = '00000000-0000-4000-8000-00000000000b'
    // Same id: the device 12c enrolls IS the live row /devices already lists
    // for this account — a real GET /devices includes the caller's own
    // current device alongside every other one, which is what lets 12h
    // assert the session list's THIS DEVICE marker against a real row.
    const DEVICE_NEW = DEVICE_LIVE
    const DEVICE_GONE = '00000000-0000-4000-8000-00000000000c'
    const DEVICE_EXISTING = '00000000-0000-4000-8000-00000000000d'
    // The wire's Token is exactly 43 characters of unpadded base64url
    // (CANT-28) — same trick the credential layer's own test harness uses
    // (`transport/test/credential-harness.ts`'s padToken) rather than a
    // literal 43-character string nobody could tell apart at a glance.
    const padToken = (name: string): string => name + '_'.repeat(43 - name.length)
    const rawDevices = [
      { id: DEVICE_LIVE, name: "Rosa Pixel 8", created_at: '2026-09-01T10:00:00.000Z' },
      { id: DEVICE_GONE, name: 'Old iPad', created_at: '2026-08-01T10:00:00.000Z', revoked_at: '2026-09-15T09:00:00.000Z' },
    ]
    let revoked: string | null = null
    const scripted = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = String(input)
      if (url.endsWith('/enroll')) {
        const body = JSON.parse(String(init?.body)) as { enrollment_token: string; device_name: string }
        if (body.enrollment_token !== GOOD_TOKEN || !body.device_name) {
          return new Response(JSON.stringify({ code: 'unauthorized' }), { status: 401 })
        }
        const now = Date.now()
        return new Response(
          JSON.stringify({
            user_id: USER, device_id: DEVICE_NEW, access_token: padToken('access-smoke-1'),
            access_expires_at: new Date(now + 900_000).toISOString(), refresh_token: padToken('refresh-smoke-1'),
            refresh_expires_at: new Date(now + 86_400_000).toISOString(),
          }),
          { status: 200, headers: { Date: new Date(now).toUTCString() } },
        )
      }
      if (/\/devices\/[^/]+\/revoke$/.test(url)) {
        revoked = url.match(/\/devices\/([^/]+)\/revoke$/)![1]
        return new Response(null, { status: 204 })
      }
      if (url.endsWith('/devices')) {
        const devices = rawDevices.map((d) => (d.id === revoked ? { ...d, revoked_at: new Date().toISOString() } : d))
        return new Response(JSON.stringify({ devices }), { status: 200 })
      }
      return new Response('not found', { status: 404 })
    }) as typeof fetch

    // 12a. Landmarks: default mode is 'login', the honest render before
    // anything durable has been read (AccountView's onMounted, which would
    // check the store, never fires under renderToString).
    configureAccount({ baseUrl: 'http://smoke.test', fetch: scripted, store: new MemoryCredentialStore() })
    openAccount()
    const loginPage = await render()
    check('devices entry point sits in the rail footer', loginPage.includes('DEVICES'))
    check('login heading', loginPage.includes('SIGN IN'))
    check('enrollment token field', loginPage.includes('ENROLLMENT TOKEN'))
    check('device name field', loginPage.includes('DEVICE NAME'))
    check('enroll submit button', loginPage.includes('ENROLL DEVICE'))
    closeAccount()

    // 12b. CANT-28's one refusal shape: a bad token gets one message, and the
    // form stays put rather than pretending to have moved on.
    const refused = await login('not-the-real-token', 'My Phone')
    check('a bad enrollment token is refused', refused === false)
    check('and the login form is still what would render', accountState.mode === 'login')
    check('with an error a person can read', typeof accountState.error === 'string' && accountState.error.length > 0)

    // 12c. A good token enrolls, names the device, stores the credential and
    // moves straight to the session list — which is also where it loads from.
    const ok = await login(GOOD_TOKEN, "Rosa Pixel 8")
    check('a good enrollment token logs in', ok === true)
    check('and switches to the session list', accountState.mode === 'sessions')
    check('and the error from the earlier refusal is gone', accountState.error === null)
    check('the session list carries the live device by name', accountState.devices.some((d) => d.name === "Rosa Pixel 8"))
    check('and the already-revoked one too — revoked devices stay in the list (CANT-117)',
      accountState.devices.some((d) => d.name === 'Old iPad' && d.revokedAt !== undefined))
    check("and this browser's own device id is recorded", accountState.deviceId === DEVICE_LIVE)

    openAccount()
    const sessionsPage = await render()
    check('sessions heading', sessionsPage.includes('SESSIONS'))
    check('a live device is listed by name', sessionsPage.includes("Rosa Pixel 8"))
    check('and offers REVOKE', sessionsPage.includes('REVOKE'))
    // pr-review nit on #109: the row you are reading this on is marked, so
    // revoking it is a choice rather than a surprise.
    const rosaRow = sessionsPage.slice(sessionsPage.indexOf('Rosa Pixel 8'), sessionsPage.indexOf('Old iPad'))
    check("the caller's own device is marked THIS DEVICE", rosaRow.includes('THIS DEVICE'))
    const oldIpadRow = sessionsPage.slice(sessionsPage.indexOf('Old iPad'))
    check('and no other row is', !oldIpadRow.includes('THIS DEVICE'))
    check('a revoked device carries the REVOKED marker', sessionsPage.includes('REVOKED'))
    closeAccount()

    // 12d. Revoking takes effect on the next read (CANT-117's route is
    // idempotent 204 either way, so this re-reads the list rather than
    // trusting an id it was handed).
    await revokeDevice(DEVICE_LIVE)
    check('revoking reaches the server', revoked === DEVICE_LIVE)
    check('and the list re-read afterward shows it revoked', accountState.devices.find((d) => d.id === DEVICE_LIVE)?.revokedAt !== undefined)

    // 12e. A device that already holds a credential skips the form entirely —
    // checkExistingCredential() is AccountView's onMounted check.
    const preEnrolled = new MemoryCredentialStore({
      userId: USER, deviceId: DEVICE_EXISTING, accessToken: 'access-existing',
      accessExpiresAt: Date.now() + 900_000, refreshToken: 'refresh-existing', refreshExpiresAt: Date.now() + 86_400_000,
      accessIssuedAt: Date.now(), clockOffsetMs: 0, chain: [], lastSentAt: null,
    })
    configureAccount({ baseUrl: 'http://smoke.test', fetch: scripted, store: preEnrolled })
    await checkExistingCredential()
    check('a device that already holds a credential goes straight to the session list',
      accountState.mode === 'sessions')
    check('loading it on mount', accountState.devices.length > 0)

    // 12f. A server that will not answer /devices at all — never a 401, just
    // gone — surfaces a readable error rather than an unhandled rejection or
    // a page that silently keeps showing stale data.
    const deadFetch = (async () => {
      throw new TypeError('fetch failed')
    }) as typeof fetch
    configureAccount({ baseUrl: 'http://smoke.test', fetch: deadFetch, store: preEnrolled })
    await loadDevices()
    check('an unreachable server surfaces a readable error', typeof accountState.error === 'string' && accountState.error.length > 0)
    check('rather than throwing past loadDevices()', true)

    // 12g. The credential terminal (CANT-31 §5, entered by the credential
    // layer itself): the session list explains it and offers RE-ENROLL, which
    // reopens the login form without clearing anything (CANT-31 §6 — only
    // reenrollCredential ever replaces a held pair).
    configureAccount({ baseUrl: 'http://smoke.test', fetch: scripted, store: new MemoryCredentialStore() })
    accountState.mode = 'sessions'
    accountState.terminal = { kind: 'credential', reason: 'smoke: a dead refresh token' }
    openAccount()
    const terminalPage = await render()
    check('a credential terminal is explained on the session list', terminalPage.includes('re-enroll'))
    check('and offers RE-ENROLL', terminalPage.includes('RE-ENROLL'))
    closeAccount()
    beginReenroll()
    const modeAfterReenroll: string = accountState.mode
    check('RE-ENROLL returns to the login form', modeAfterReenroll === 'login')

    // 12h. pr-review nit on #109: ConnectionBanner's RE-ENROLL is clicked from
    // OUTSIDE the account view — it does not exist yet — so opening it must
    // not let AccountView's own onMounted check immediately navigate straight
    // past the login form this click asked for. requestReenrollBeforeMount()
    // is the fix; checkExistingCredential() is the onMounted call it guards.
    configureAccount({ baseUrl: 'http://smoke.test', fetch: scripted, store: preEnrolled })
    requestReenrollBeforeMount()
    const modeAfterRequest: string = accountState.mode
    check('requesting a re-enroll lands on the login form immediately', modeAfterRequest === 'login')
    await checkExistingCredential()
    const modeAfterGuardedMount: string = accountState.mode
    check("and the account view's own mount check — which would otherwise find the held credential and skip past it — honors the request",
      modeAfterGuardedMount === 'login')
    // The flag is consumed by that one mount, not left to leak into the next
    // ordinary visit: an unrelated later mount still auto-detects normally.
    await checkExistingCredential()
    const modeAfterOrdinaryMount: string = accountState.mode
    check('a later, unrelated mount is unaffected — it still goes straight to the session list',
      modeAfterOrdinaryMount === 'sessions')
  }

  console.log(fail.length ? `\n${fail.length} FAILED` : '\nall green')
  process.exit(fail.length ? 1 : 0)
}

main()
