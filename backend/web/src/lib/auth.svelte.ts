// Session state (design 19 §4.3): Bearer-key login via a GET /api/whoami probe,
// key held in sessionStorage (tab lifetime — a reload keeps it, a new tab does
// not; D19.9), read-only degradation derived from whoami.is_admin (D19.6). The
// api client's 401 interceptor tears the session down when the stored key is
// revoked; the SSE terminal `event: error` reuses the same teardown (D19.7, W4).

import { ApiError, apiFetch, configureApi } from './api'
import type { WhoamiResponse } from './api/types'

const STORAGE_KEY = 'picpak.api-key'

export class Session {
  key = $state<string | null>(null)
  whoami = $state<WhoamiResponse | null>(null)
  /** True while the boot-time sessionStorage probe is in flight. */
  restoring = $state(false)
  /** Why the previous session ended — rendered on the login screen. */
  notice = $state<string | null>(null)

  readonly active = $derived(this.key !== null && this.whoami !== null)
  // Read-only degradation source (D19.6) — the server stays authoritative via
  // requireAdmin (design 17 §4.2); this flag is the comfort affordance only.
  readonly is_admin = $derived(this.whoami?.is_admin ?? false)
  readonly label = $derived(this.whoami?.label ?? null)
  readonly keyId = $derived(this.whoami?.key_id ?? null)

  /** Probe the key against /api/whoami; persist it only on success. */
  async login(rawKey: string): Promise<void> {
    const key = rawKey.trim()
    const whoami = await apiFetch<WhoamiResponse>('/api/whoami', {}, { key })
    this.key = key
    this.whoami = whoami
    this.notice = null
    sessionStorage.setItem(STORAGE_KEY, key)
  }

  /**
   * Boot-time restore: re-probe a key surviving in sessionStorage (tab reload).
   * A rejected key (401/403) is dropped with a notice; transient failures
   * (network, 5xx) keep it so a later reload can retry.
   */
  async restore(): Promise<void> {
    if (this.active || this.restoring) return
    const stored = sessionStorage.getItem(STORAGE_KEY)
    if (stored === null || stored === '') return
    this.restoring = true
    try {
      const whoami = await apiFetch<WhoamiResponse>('/api/whoami', {}, { key: stored })
      this.key = stored
      this.whoami = whoami
    } catch (err) {
      if (err instanceof ApiError && (err.status === 401 || err.status === 403)) {
        sessionStorage.removeItem(STORAGE_KEY)
        this.notice = 'Stored key was rejected — sign in again.'
      }
    } finally {
      this.restoring = false
    }
  }

  /** User-initiated logout (D19.16). */
  logout(): void {
    this.clear()
  }

  /** Interceptor path: the stored key stopped working mid-session (401 / SSE revoke). */
  invalidate(reason: string): void {
    this.clear()
    this.notice = reason
  }

  private clear(): void {
    this.key = null
    this.whoami = null
    sessionStorage.removeItem(STORAGE_KEY)
  }
}

export const session = new Session()

// The mutation-affordance gate consumed by every feature surface (D19.6/D19.17):
// the UI hides/disables what the server would 403. Server-authoritative; cosmetic here.
export const canMutate = (): boolean => session.is_admin

configureApi({
  getKey: () => session.key,
  onUnauthorized: () => session.invalidate('Session ended: the API key is no longer valid.'),
})
