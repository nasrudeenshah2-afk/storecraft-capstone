/// <reference types="vitest/config" />
import { defineConfig, type ProxyOptions } from 'vite'
import vue from '@vitejs/plugin-vue'

// The Go server (primary) is expected on :8090. In development the console is
// served by Vite on :5173 and these paths are proxied, so the browser only
// ever talks to one origin and the Go server needs no CORS handling.
// STORECRAFT_API_TARGET overrides the target (the browser tests use it to
// point at their own temporary server).
const goServer = process.env.STORECRAFT_API_TARGET ?? 'http://localhost:8090'

// If the Go server's side of a proxied response ends abruptly (for example
// the server is stopped while the change stream is open), close the browser's
// side too. Without this the dev proxy keeps the browser connection open but
// silent, and the console would only notice through its 45s watchdog.
const proxyToGo: ProxyOptions = {
  target: goServer,
  configure(proxy) {
    proxy.on('proxyRes', (proxyRes, _req, res) => {
      proxyRes.on('close', () => {
        if (!res.writableEnded) {
          res.destroy()
        }
      })
    })
  },
}

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/v1': proxyToGo,
      '/metrics': proxyToGo,
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
    restoreMocks: true,
    unstubGlobals: true,
  },
})
