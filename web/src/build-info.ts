/* The web client's build identity (CANT-373): the part of `hello.client_info`
 * after `catenary-web/`.
 *
 * ONE SOURCE WITH THE BINARY. vite.config.ts defines `__CATENARY_WEB_VERSION__`
 * from CATENARY_WEB_VERSION, and deploy/Dockerfile sets that from the same
 * `VERSION` build arg it stamps into `main.version`. publish.yml passes the
 * validated release stamp there on a tag build and nothing on a main build, so
 * a released image's client names its release, and every other build (`vite
 * dev` included) says `dev`: the same answer `catenary version` gives for a
 * build no release cut. package.json's own `version` is not used: it is 0.0.0,
 * and release-please (release-type `simple`, root manifest) never bumps it. */

declare const __CATENARY_WEB_VERSION__: string

/** The release this client was built as, or `dev` for a build no release cut. */
export function webClientVersion(): string {
  const v = typeof __CATENARY_WEB_VERSION__ === 'string' ? __CATENARY_WEB_VERSION__.trim() : ''
  return v === '' ? 'dev' : v
}
