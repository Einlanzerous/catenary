import { fileURLToPath, URL } from 'node:url'
import { defineConfig, type ProxyOptions } from 'vite'
import vue from '@vitejs/plugin-vue'

/**
 * The dev server talks to a real `catenary serve` (CANT-39), at
 * CATENARY_DEV_API or the service's own default port.
 *
 * THROUGH A PROXY, SO THE BROWSER ONLY EVER TALKS TO ITS OWN ORIGIN. The
 * WebSocket door refuses a browser whose `Origin` host is not the request's
 * `Host` (internal/api/socket.go; CANT-22's handshake, a Mode C surface that
 * is not loosened for a dev server). `changeOrigin` is deliberately OFF: it
 * would rewrite `Host` to the upstream while the browser's `Origin` stays this
 * page's, which is exactly the mismatch the door refuses. With it off both
 * name this page, and the door lets the upgrade through.
 */
const api = process.env.CATENARY_DEV_API ?? 'http://127.0.0.1:4012'
const route: ProxyOptions = { target: api, changeOrigin: false }
const proxy: Record<string, ProxyOptions> = {
  '/ws': { ...route, ws: true },
  '/sync': route,
  '/enroll': route,
  '/refresh': route,
  '/devices': route,
  '/conversations': route,
}

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  // Loopback only. This used to bind every interface while it served mock
  // data and nothing else; it now fronts a real server and a real credential.
  server: { port: 4009, proxy },
  preview: { proxy },
  // CANT-241: the production client is written INTO the Go package that embeds
  // it (internal/webui), because go:embed cannot reach outside its own package
  // directory. `emptyOutDir` has to be explicit for a directory outside Vite's
  // root; it empties static/dist only, so the committed static/PLACEHOLDER one
  // level up is never touched. The `--ssr` scripts pass their own --outDir.
  build: { outDir: '../internal/webui/static/dist', emptyOutDir: true },
})
