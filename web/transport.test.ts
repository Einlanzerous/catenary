/* The transport's unit runner (CANT-35 ruling 9 → A): `node:test` over a
 * `vite build --ssr` bundle, in the house style of `smoke`, `conformance` and
 * `readstate`, so the `@/` alias and the generated codecs resolve exactly as
 * they do in the app. `npm run test:transport` builds this file and runs it. */

import './src/transport/test/closes.test'
import './src/transport/test/session.test'
import './src/transport/test/journal.test'
import './src/transport/test/backoff.test'
import './src/transport/test/project.test'
import './src/transport/test/source.test'
import './src/transport/test/decisions.test'
import './src/transport/test/chain.test'
import './src/transport/test/hold.test'
import './src/transport/test/refused.test'
import './src/transport/test/credential-transport.test'
import './src/transport/test/credential-idb.test'
import './src/transport/test/refresh.test'
