import { writeFileSync } from 'node:fs'
import type { PluginOption } from 'vite'
// vitest/config re-exports Vite's defineConfig with the `test` field typed —
// one config file drives dev, build and the unit tests.
import { defineConfig } from 'vitest/config'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { compression } from 'vite-plugin-compression2'

// Dev proxy target: the local cmd/admin the dev server forwards API calls to.
// cmd/admin binds 127.0.0.1:8081 by default (Doc 17 ADMIN_ADDR), so it is
// host-reachable in dev with no compose port-map override (unlike ctxd, which
// published no port). Set ADMIN_DEV_PROXY when it differs.
const proxyTarget = process.env.ADMIN_DEV_PROXY ?? 'http://localhost:8081'

// Vite's emptyOutDir wipes dist/ including the committed .gitkeep that keeps the
// //go:embed all:dist pattern satisfiable on fresh clones. Restore it after every
// build so local builds never leave a "deleted: .gitkeep" behind.
function keepGitkeep(): PluginOption {
  return {
    name: 'keep-gitkeep',
    closeBundle() {
      writeFileSync('dist/.gitkeep', '')
    },
  }
}

export default defineConfig({
  plugins: [
    svelte(),
    // Pre-compress at build time; the Go handler (web/web.go) negotiates the
    // .br/.gz siblings — cmd/admin runs no on-the-fly compression middleware.
    compression({ algorithms: ['gzip', 'brotliCompress'], threshold: 1024 }),
    keepGitkeep(),
  ],
  build: {
    sourcemap: false,
  },
  server: {
    // Same-origin dev loop: the proxy is server-side, the browser sees one
    // origin — cmd/admin needs zero CORS code.
    proxy: {
      '/api': { target: proxyTarget, changeOrigin: false },
      '/healthz': { target: proxyTarget, changeOrigin: false },
    },
  },
  test: {
    // Logic modules only (design 19 §6) — no component snapshots, no DOM:
    // fetch/sessionStorage are stubbed per test, runes compile fine in node.
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
})
