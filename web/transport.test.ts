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
