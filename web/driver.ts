/* The Node transport driver's entry (CANT-35 ruling 0 → A). `npm run
 * build:driver` builds this file into `dist-transport-driver/driver.js` with
 * `vite build --ssr`, in the house style of `smoke` and `test:transport`, so
 * the `@/` alias and the generated codecs resolve as they do in the app. The
 * protocol is in src/transport/driver/driver.ts; the Go half that runs it is
 * server/cmd/soakrig/tsdriver.go. */

import { serve } from './src/transport/driver/driver'

serve({
  input: process.stdin,
  write: (line) => process.stdout.write(line + '\n'),
  log: (line) => process.stderr.write(line + '\n'),
  exit: () => process.exit(0),
})
