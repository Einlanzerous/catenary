import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  // host: true binds all interfaces so the client is reachable from another
  // machine on the LAN. It serves mock data and nothing else — do not carry
  // this over to a build that talks to a real server.
  server: { port: 4009, host: true },
})
