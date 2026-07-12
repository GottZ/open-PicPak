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
  /**
   * Navigation tier (design 33 §4.6, E-A33-5). `core` areas answer a
   * layperson's primary questions and sit at the top level of the shell nav;
   * `advanced` areas live behind the collapsed "Erweitert" disclosure. Routing
   * ignores the tier entirely (areaRoutes is flat) — a deep link to an advanced
   * area always resolves regardless of the disclosure state; the tier only
   * groups the nav. Mandatory on every area (pinned by the namespace test).
   */
  tier: 'core' | 'advanced'
}

/**
 * The operator areas (design 19 §4.4). Single source for the shell nav and the
 * placeholder copy; areaRoutes below mounts the matching components. Every path
 * here must also be an areaRoutes key (pinned by the route-namespace test).
 */
export const AREAS: AreaMeta[] = [
  { path: '/gallery', title: 'Templates', ships: 'Doc 33 — template gallery (laien apply flow)', tier: 'core' },
  // /fleet stays core: it answers the layperson's "is my panel online?" until a
  // dedicated status area exists (design 33 §4.6).
  { path: '/fleet', title: 'Fleet', ships: 'Doc 22 — telemetry dashboard', tier: 'core' },
  { path: '/ota', title: 'OTA', ships: 'Doc 20 — OTA serving + rollout', tier: 'advanced' },
  { path: '/logs', title: 'Logs', ships: 'Doc 21 — log reassembly + viewer', tier: 'advanced' },
  { path: '/berry', title: 'Berry', ships: 'Doc 23 — Berry config editor', tier: 'advanced' },
  { path: '/functions', title: 'Functions', ships: 'Doc 25 — FaaS function editor', tier: 'advanced' },
  { path: '/onboard', title: 'Onboard', ships: 'Doc 26 — Web-USB onboarding', tier: 'advanced' },
  { path: '/settings', title: 'Settings', ships: 'Doc 18 — secrets KV form', tier: 'advanced' },
  { path: '/media', title: 'Media', ships: 'Doc 29 — image upload + playlist editor', tier: 'core' },
]

/**
 * The core areas — top level of the shell nav (design 33 §4.6). Derived from
 * AREAS so the tier assignment stays the single source of truth.
 */
export const coreAreas = (): AreaMeta[] => AREAS.filter((a) => a.tier === 'core')

/**
 * The advanced areas — tucked behind the collapsed "Erweitert" disclosure
 * (design 33 §4.6). Deep links to these resolve regardless of the disclosure
 * state; the grouping is nav affordance, not an access gate.
 */
export const advancedAreas = (): AreaMeta[] => AREAS.filter((a) => a.tier === 'advanced')

/**
 * Lazy per area so each is its own chunk; the feature docs replace the
 * AreaPlaceholder slots with their real pages. `/fleet` ships the Doc 22
 * telemetry dashboard (it replaced the Doc 19 scaffold roster).
 */
export const areaRoutes = {
  '/gallery': () => import('./gallery/GalleryHome.svelte'),
  '/fleet': () => import('./fleet/FleetDashboard.svelte'),
  '/ota': () => import('./ota/OtaHome.svelte'),
  '/logs': () => import('./logs/LogViewer.svelte'),
  '/berry': () => import('./berry/BerryEditor.svelte'),
  '/functions': () => import('./functions/FunctionsEditor.svelte'),
  '/onboard': () => import('./onboard/Onboard.svelte'),
  '/settings': () => import('./AreaPlaceholder.svelte'),
  '/media': () => import('./media/MediaHome.svelte'),
  '*': () => import('./NotFound.svelte'),
} satisfies Routes

/**
 * Landing redirect (design 33 §4.6, E-A33-1): `/` is no area — it canonicalizes
 * to `/gallery`, whose two-tile head carries both layperson entry points (own
 * image → /media, use a template → cards). Returns the target for `/`, else
 * null. The literal return type keeps the value assignable to sv-router's typed
 * `navigate(Path<T>)` (router.ts) and pins the redirect to a real area — the
 * route-namespace test asserts the target exists in areaRoutes. Kept pure (no
 * session read) so it stays node-testable.
 */
export function entryRedirect(pathname: string): '/gallery' | null {
  return pathname === '/' ? '/gallery' : null
}
