// sv-router 0.16.3 (exactly pinned, O2), history mode — deep links work because
// cmd/admin serves the SPA fallback for HTML navigations (web/web.go, D19.5). The
// login gate lives in App.svelte before the Router renders, so routes need no
// per-route auth guard; picpak has no role tiers, so no area guard either (the
// is_admin gate is per-mutation in the feature pages, D19.6 — server-authoritative).

import { createRouter } from 'sv-router'
import { areaRoutes, entryRedirect } from './routes'

export const { p, navigate, isActive, route } = createRouter({
  ...areaRoutes,
  hooks: {
    beforeLoad({ pathname }) {
      // `/` canonicalizes to /gallery (documented sv-router redirect idiom:
      // navigate() queues the new navigation, the throw aborts this one).
      const landing = entryRedirect(pathname)
      if (landing !== null) throw navigate(landing, { replace: true })
    },
  },
})
