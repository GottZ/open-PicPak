// Session state (design 28 §4.3): the human login is a COOKIE session. POST /api/session verifies
// username+password and sets the httpOnly ppk_sid cookie — the Bearer key never enters the JS heap
// (the XSS-exfil win over the old sessionStorage key). Auth state is derived from a GET /api/whoami
// probe (the cookie rides along automatically via credentials: 'same-origin'); read-only degradation
// from whoami.is_admin (D19.6). Logout is DELETE /api/session (server-authoritative revoke). The api
// client's 401 interceptor tears the session down when the cookie expires/is revoked; the SSE terminal
// `event: error` reuses the same teardown (D19.7).

import { apiFetch, configureApi } from './api'
import type { WhoamiResponse } from './api/types'

export class Session {
  whoami = $state<WhoamiResponse | null>(null)
  /** True while the boot-time whoami probe is in flight. */
  restoring = $state(false)
  /** Why the previous session ended — rendered on the login screen. */
  notice = $state<string | null>(null)

  readonly active = $derived(this.whoami !== null)
  // Read-only degradation source (D19.6) — the server stays authoritative via requireAdmin/requireScope
  // (design 28 §4.1); this flag is the comfort affordance only.
  readonly is_admin = $derived(this.whoami?.is_admin ?? false)
  readonly label = $derived(this.whoami?.label ?? null)

  /**
   * Exchange credentials for a session cookie, then load the identity from whoami. A 401 from the
   * POST is a bad-credentials login attempt owned by the caller (probe → no session teardown); it
   * propagates to the login form. On success the httpOnly cookie is set server-side and whoami
   * populates the identity/admin flag.
   */
  async login(username: string, password: string): Promise<void> {
    await apiFetch('/api/session', { method: 'POST', body: JSON.stringify({ username, password }) }, { probe: true })
    this.whoami = await apiFetch<WhoamiResponse>('/api/whoami', {}, { probe: true })
    this.notice = null
  }

  /**
   * Boot-time restore: probe /api/whoami — the httpOnly cookie (if present) rides along automatically.
   * A 401 means "not signed in" (a fresh visitor), NOT a revoke, so it silently shows the login screen
   * with no notice; a transient failure (network/5xx) also leaves the app signed-out to retry on a
   * later reload. The probe flag suppresses the 401 interceptor throughout.
   */
  async restore(): Promise<void> {
    if (this.active || this.restoring) return
    this.restoring = true
    try {
      this.whoami = await apiFetch<WhoamiResponse>('/api/whoami', {}, { probe: true })
    } catch {
      // Not signed in / transient — stay on the login screen, no notice.
    } finally {
      this.restoring = false
    }
  }

  /** User-initiated logout (D19.16): revoke the session server-side, then clear local state. */
  async logout(): Promise<void> {
    try {
      await apiFetch('/api/session', { method: 'DELETE' }, { probe: true })
    } catch {
      // A failed DELETE (already-expired cookie, offline) must not trap the user in the shell — clear
      // locally regardless; the server row expires on its own absolute TTL.
    }
    this.clear()
  }

  /** Interceptor path: the cookie stopped working mid-session (401 / SSE revoke). */
  invalidate(reason: string): void {
    this.clear()
    this.notice = reason
  }

  private clear(): void {
    this.whoami = null
  }
}

export const session = new Session()

// The mutation-affordance gate consumed by every feature surface (D19.6/D19.17):
// the UI hides/disables what the server would 403. Server-authoritative; cosmetic here.
export const canMutate = (): boolean => session.is_admin

configureApi({
  onUnauthorized: () => session.invalidate('Session ended: please sign in again.'),
})
