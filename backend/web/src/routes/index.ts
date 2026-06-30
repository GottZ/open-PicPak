// Route table as a pure module (design 19 §4.4): sv-router's createRouter touches
// DOM observers at call time, so the table + the entry-redirect rule live here
// where vitest can assert them in a plain node environment (router.ts does the
// instantiation). F4 pins the namespace invariant.

import type { Routes } from 'sv-router'

/**
 * Path prefixes the cmd/admin process registers — the SPA must never claim them
 * or a deep link would shadow a real endpoint. Just `/api` + `/healthz`: the
 * admin origin registers nothing else (`/firmware.bin`, `/c2`, `/pp` live on the
 * SEPARATE cmd/ingest origin and are never reachable from this SPA). Pinned by F4.
 */
export const RESERVED_SERVER_PREFIXES = ['/api', '/healthz'] as const

/** Nav + placeholder metadata for one operator area. */
export interface AreaMeta {
  path: string
  title: string
  /** Which design doc delivers the real UI (shown on the placeholder). */
  ships: string
}

/**
 * The operator areas (design 19 §4.4). Single source for the shell nav and the
 * placeholder copy; areaRoutes below mounts the matching components. Every path
 * here must also be an areaRoutes key (pinned by the route-namespace test).
 */
export const AREAS: AreaMeta[] = [
  { path: '/fleet', title: 'Fleet', ships: 'Doc 22 — telemetry dashboard' },
  { path: '/ota', title: 'OTA', ships: 'Doc 20 — OTA serving + rollout' },
  { path: '/logs', title: 'Logs', ships: 'Doc 21 — log reassembly + viewer' },
  { path: '/berry', title: 'Berry', ships: 'Doc 23 — Berry config editor' },
  { path: '/functions', title: 'Functions', ships: 'Doc 25 — FaaS function editor' },
  { path: '/onboard', title: 'Onboard', ships: 'Doc 26 — Web-USB onboarding' },
  { path: '/settings', title: 'Settings', ships: 'Doc 18 — secrets KV form' },
]

/**
 * Lazy per area so each is its own chunk; the feature docs replace the
 * AreaPlaceholder slots with their real pages. `/fleet` ships the scaffold
 * roster now (the SSE/whoami liveness proof — Doc 22 replaces it).
 */
export const areaRoutes = {
  '/fleet': () => import('./fleet/FleetRoster.svelte'),
  '/ota': () => import('./AreaPlaceholder.svelte'),
  '/logs': () => import('./AreaPlaceholder.svelte'),
  '/berry': () => import('./AreaPlaceholder.svelte'),
  '/functions': () => import('./AreaPlaceholder.svelte'),
  '/onboard': () => import('./AreaPlaceholder.svelte'),
  '/settings': () => import('./AreaPlaceholder.svelte'),
  '*': () => import('./NotFound.svelte'),
} satisfies Routes

/**
 * Landing redirect (design 19 §4.4): `/` is no area — it canonicalizes to
 * `/fleet`. Returns the target for `/`, else null. The literal return type keeps
 * the value assignable to sv-router's typed `navigate(Path<T>)` (router.ts).
 * Kept pure (no session read) so it stays node-testable.
 */
export function entryRedirect(pathname: string): '/fleet' | null {
  return pathname === '/' ? '/fleet' : null
}
