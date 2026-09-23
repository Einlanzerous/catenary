import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from '@/App.vue'
import {
  newCount,
  typingLabel,
  openSearch,
  searchHits,
  select,
  send,
  state,
  unreadCount,
} from '@/store'

const fail: string[] = []
const check = (name: string, ok: boolean, detail = '') => {
  if (!ok) fail.push(`${name}${detail ? ` — ${detail}` : ''}`)
  console.log(`${ok ? 'ok  ' : 'FAIL'}  ${name}${detail ? `  (${detail})` : ''}`)
}

const render = () => renderToString(createSSRApp(App))

async function main() {

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
  // The updated canvas states the guarantee that actually holds.
  check('TLS chip, not E2E', main.includes('7 MEMBERS · TLS') && !main.includes('E2E'))
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
  const kitchen = state.conversations.find((c) => c.id === 'c-kitchen')!
  const expectedNew = newCount(kitchen)
  check('unread rule drawn', main.includes(`${expectedNew} NEW`), `${expectedNew} NEW`)
  // You cannot have an unread message you sent.
  const mineAfterRule = state.messages.filter(
    (m) =>
      m.conversationId === 'c-kitchen' &&
      m.seq >= kitchen.firstUnreadSeq! &&
      m.authorId === state.me,
  ).length
  check('own messages excluded from the count', mineAfterRule > 0 && expectedNew ===
    state.messages.filter(
      (m) => m.conversationId === 'c-kitchen' && m.seq >= kitchen.firstUnreadSeq!,
    ).length - mineAfterRule, `${mineAfterRule} of mine skipped`)
  // Deliberate call 03: status as words, not tick glyphs. The two words this
  // render can show are SENT and READ — StatusLabel is guarded `v-if="mine"`
  // and CANT-90 made your own message a two-rung ladder, so `delivered` is
  // only ever the answer for somebody else's message. This asserts the two
  // that appear and that the third does not; the LIVE ladder is checked in
  // section 5, because this render is static and a timer cannot reach it.
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

  // 1b. The three typing cases are a rule, not a string.
  const typing = (ids: string[]) => {
    state.typing['c-kitchen'] = ids
    return typingLabel('c-kitchen')
  }
  check('typing · one is a first name', typing(['u-nadia']) === 'Nadia')
  check('typing · two, in the order they started',
    typing(['u-nadia', 'u-ted']) === 'Nadia, Ted')
  check('typing · three still name everyone',
    typing(['u-nadia', 'u-ted', 'u-marek']) === 'Nadia, Ted, Marek')
  check('typing · four or more drop names',
    typing(['u-nadia', 'u-ted', 'u-marek', 'u-rosa']) === 'Several people')
  check('typing · nobody renders nothing', typing([]) === null)

  // 2. Rail previews derive from the last message, per conversation kind.
  check('room preview carries a name', main.includes('Rosa: Listened to it'))
  check('own preview says You', main.includes('You: sent it to your inbox instead'))
  check('failed conversation marked', main.includes('FAILED'))
  check('muted conversation marked', main.includes('MUTED'))

  // 3. Search finds text and transcripts in one list.
  state.query = 'thursday'
  const hits = searchHits.value
  check('search spans both kinds', hits.some((h) => h.type === 'TXT') && hits.some((h) => h.type === 'VOX'))
  check('transcript hit seeks to a word', hits.some((h) => h.jumpToSec !== undefined),
    `jump=${hits.find((h) => h.jumpToSec !== undefined)?.jumpToSec}s`)
  openSearch()
  const search = await render()
  check('search view renders', search.includes('RESULTS ·'))
  check('VOX tag present', search.includes('>VOX<'))
  check('JUMP TO present', search.includes('JUMP TO'))

  // 4. Selecting a conversation clears its badge but keeps its unread rule.
  const bergen = state.conversations.find((c) => c.id === 'c-bergen')!
  check('unread before opening', unreadCount(bergen) > 0, `${unreadCount(bergen)}`)
  select('c-bergen')
  check('reading clears the badge', unreadCount(bergen) === 0)
  check('but keeps the rule', newCount(bergen) > 0, `${newCount(bergen)} NEW`)

  // 5. The mock ladder your own message actually walks ends at `sent`.
  //
  // SECTION 1 CANNOT SEE THIS. It renders once, so `advance`'s timers never
  // fire and a client-invented `delivered` would sit behind a green static
  // assertion — which is exactly what it did until CANT-90's review found it.
  // Driving `send` and waiting past the last timer is the only thing here that
  // watches the state a message reaches on its own.
  select('c-kitchen')
  state.composer.draft = 'does this walk past sent'
  send()
  const sent = state.messages[state.messages.length - 1]
  check('send authors it as mine', sent.authorId === state.me)
  await new Promise((r) => setTimeout(r, 2200))
  check(
    'my own message settles at SENT and never invents DELIVERED',
    sent.state === 'sent',
    `settled at ${sent.state}`,
  )

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
  // SECTION 1'S `7 MEMBERS · TLS` LANDMARK IS UNTOUCHED. It asserts against the
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
  // u-petra is deactivated but her membership rows stay (CANT-33 ruling 7):
  // she keeps authorship of her old message in Bergen Hill Co-op, and
  // c-petra's only message is hers. Both must carry the quiet textual
  // marker this ticket adds, and MAIN — captured at the top against Kitchen
  // Table, where nobody is deactivated — proves an active author gets none.
  check(
    'active authors carry no mark on the default screen',
    !main.includes('DEACTIVATED'),
  )

  select('c-bergen')
  const bergenPage = await render()
  check(
    'a deactivated author is dimmed in a group room',
    bergenPage.includes('Petra Lindqvist') && bergenPage.includes('DEACTIVATED'),
  )

  select('c-petra')
  const petraDm = await render()
  check(
    'the DM header marks a deactivated other half',
    petraDm.includes('Petra Lindqvist') && petraDm.includes('DEACTIVATED'),
  )

  // An active DM's header carries no mark — same component, other member.
  select('c-ilse')
  const ilseDm = await render()
  check('an active DM header carries no mark', !ilseDm.includes('DEACTIVATED'))

  console.log(fail.length ? `\n${fail.length} FAILED` : '\nall green')
  process.exit(fail.length ? 1 : 0)
}

main()
