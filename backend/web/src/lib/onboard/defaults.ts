// A35.3 — Onboard URL prefill derived from the SPA origin.
//
// Since A35 ("EIN Backend", DECISIONS §A35) serves admin + ingest as one process behind both hosts,
// `location.origin` IS the device endpoint from any loaded SPA — "die URL kann also location.origin
// nutzen." (DECISIONS §Nachträge). This module turns an origin string into the device frame-/C2-URL and
// the C2 period the onboarding form pre-fills.
//
// Token handling (deliberate): the ingest path token is the server-side secret INGEST_TOKEN
// (ingestcore/server.go:62, air-gap "keep out of code"). It is NOT available client-side and we do NOT
// invent an endpoint to hand it to the SPA (it is a secret with its own governance). So the URL prefill
// carries a clearly-marked PLACEHOLDER for the token path segment, which the operator replaces.
//
// Path scheme (ingest Ist-Code, ingestcore/server.go Handle + firmware):
//   frame URL → <origin>/<token>/frame  (SETURL; net_http_fetch_frame; OTA derives .../firmware.bin — ota.c:124)
//   C2 URL    → <origin>/<token>/c2      (device appends ?sn= / challenge?sn= / rekey?sn= — cmd.c:244,271,349)

/** Clearly-marked placeholder for the secret ingest-token path segment (never available client-side). */
export const INGEST_TOKEN_PLACEHOLDER = '<INGEST_TOKEN>'

/** Default C2 poll period in seconds (design/26 §2.2 / D26.4 form default; the 1800 the field hints). */
export const DEFAULT_C2_PERIOD_SECONDS = '1800'

export interface OnboardUrlDefaults {
  frameUrl: string
  c2Url: string
  c2PeriodSeconds: string
}

/** Fields this module can pre-fill (a subset of ProvisionFields). */
export interface PrefillableUrlFields {
  frameUrl: string
  c2Url: string
  c2PeriodSeconds?: string
}

/** Derive the device frame-/C2-URL + C2 period from the SPA origin (design/26 §2.2 path scheme). */
export function onboardUrlDefaults(origin: string): OnboardUrlDefaults {
  const base = origin.replace(/\/+$/, '') // drop trailing slash(es) → clean single-'/' joins
  const seg = `${base}/${INGEST_TOKEN_PLACEHOLDER}`
  return {
    frameUrl: `${seg}/frame`,
    c2Url: `${seg}/c2`,
    c2PeriodSeconds: DEFAULT_C2_PERIOD_SECONDS,
  }
}

const isBlank = (v: string | undefined): boolean => (v ?? '').trim() === ''

/** Shape of GET /api/onboard/defaults (A35.3b): the tokenized device URLs an ADMIN session may read. */
export interface OnboardDefaultsResponse {
  frame_url: string
  c2_url: string
  c2_period_s: number | string
}

/**
 * A35.3b fallback chain (Endpoint → Placeholder). Resolve frame-/C2-URL + C2 period from the admin
 * endpoint's tokenized payload, filling ONLY blank fields; any field the endpoint left empty (feature
 * dark: INGEST_TOKEN unset ⇒ blank URLs) falls back to the origin PLACEHOLDER path, and a value the
 * operator already typed is never overwritten. So a partial or empty payload degrades cleanly to the
 * exact A35.3 behaviour instead of clearing a field.
 */
export function prefilledFromEndpoint(
  fields: PrefillableUrlFields,
  resp: OnboardDefaultsResponse,
  origin: string,
): OnboardUrlDefaults {
  const ph = onboardUrlDefaults(origin) // placeholder fallback for any field the endpoint left blank
  const period = String(resp.c2_period_s ?? '').trim()
  const frame = isBlank(resp.frame_url) ? ph.frameUrl : resp.frame_url
  const c2 = isBlank(resp.c2_url) ? ph.c2Url : resp.c2_url
  return {
    frameUrl: isBlank(fields.frameUrl) ? frame : fields.frameUrl,
    c2Url: isBlank(fields.c2Url) ? c2 : fields.c2Url,
    c2PeriodSeconds: isBlank(fields.c2PeriodSeconds)
      ? (isBlank(period) ? ph.c2PeriodSeconds : period)
      : (fields.c2PeriodSeconds as string),
  }
}

/**
 * Resolve the frame-/C2-URL + C2 period, filling ONLY blank fields from the origin-derived defaults; a
 * value the operator already typed is returned unchanged (never overwritten). The caller applies the
 * result to the reactive form so mount-time prefill and re-runs stay idempotent and non-destructive.
 */
export function prefilledUrlFields(fields: PrefillableUrlFields, origin: string): OnboardUrlDefaults {
  const d = onboardUrlDefaults(origin)
  return {
    frameUrl: isBlank(fields.frameUrl) ? d.frameUrl : fields.frameUrl,
    c2Url: isBlank(fields.c2Url) ? d.c2Url : fields.c2Url,
    c2PeriodSeconds: isBlank(fields.c2PeriodSeconds) ? d.c2PeriodSeconds : (fields.c2PeriodSeconds as string),
  }
}
