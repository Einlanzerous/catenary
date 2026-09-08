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

  console.log(fail.length ? `\n${fail.length} FAILED` : '\nall green')
  process.exit(fail.length ? 1 : 0)
}

main()
