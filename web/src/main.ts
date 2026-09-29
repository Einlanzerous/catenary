import { createApp } from 'vue'
import App from './App.vue'
import { credentialStore, onEnrolled } from './account'
import { endSession, liveTransport, openAccount, setTheme, startSession, state } from './store'
import { IdbJournal, type Journal } from './transport'
import './styles/base.css'

// Dark is primary; light is derived. Honour an explicit OS preference, but
// default to dark rather than to whatever the machine happens to say.
if (window.matchMedia('(prefers-color-scheme: light)').matches) {
  setTheme('light')
} else {
  setTheme(state.theme)
}

createApp(App).mount('#app')

// THE COMPOSITION ROOT FOR THE LIVE SESSION (CANT-39). Same origin: the Go
// binary serves the API beside this app, and `vite dev` proxies it there
// (vite.config.ts), so the WebSocket door's origin check holds either way.
// A device with no credential starts no transport and lands on the login
// form; a login or a re-enrollment restarts the session over the new pair.
// The conversation opened first is chosen once the first records land —
// `showProjection` in store.ts.
//
// ONE DURABLE JOURNAL FOR THE TAB'S LIFE (CANT-169), so a reload resumes from
// the cursor it stored rather than bootstrapping. Where IndexedDB will not
// open at all, the transport's own in-memory journal is used and nothing
// claims otherwise; a journal that opens and then fails a write says so in
// the banner (`journalError`) and does not fall back.
const journal: Promise<Journal | undefined> = IdbJournal.open().catch((e: unknown) => {
  console.warn('catenary: the durable journal would not open; this visit keeps its records in memory', e)
  return undefined
})

const start = async () =>
  startSession({ baseUrl: location.origin, store: await credentialStore(), journal: await journal })

// A NEW ENROLLMENT STARTS FROM AN EMPTY JOURNAL. The pair just written is a
// new device, possibly another person's, and a resume cursor is a position in
// the log as the PREVIOUS credential's account could see it: resuming from it
// would skip everything below it that this account can see and that one
// could not. One bootstrap is the price; a wipe never touches the credential.
onEnrolled(() => {
  void (async () => {
    endSession()
    await (await journal)?.wipe()
    await start()
  })()
})

void start().then(
  (started) => {
    // Not started AND nothing else started one meanwhile: a login that
    // overtook this read has already left the account view where it wants.
    if (!started && !liveTransport()) openAccount()
  },
  (e: unknown) => {
    // The credential store would not open. The login form is still the
    // honest screen — a blank app says nothing at all.
    console.error('catenary: could not read this device\'s credential', e)
    openAccount()
  },
)
