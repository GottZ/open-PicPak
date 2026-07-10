// Template store API wrappers (A30 W7) — thin apiFetch shims for the picker. Reads are auth-gated (a
// read-only operator may browse + load); /apply is admin-only (RCE-equivalent, server-enforced).

import { apiFetch } from '../api'
import type {
  ApplyResponse,
  TemplateDetail,
  TemplateKind,
  TemplateResponse,
  TemplateSummary,
  TemplatesResponse,
} from './types'

/** GET /api/templates?kind= — the kind-filtered list projection (the picker offers only its editor's kind). */
export async function listTemplates(kind: TemplateKind): Promise<TemplateSummary[]> {
  const res = await apiFetch<TemplatesResponse>(`/api/templates?kind=${encodeURIComponent(kind)}`)
  return res.templates
}

/** GET /api/templates/{id} — the full row (source + params schema), loaded lazily on select. */
export async function getTemplate(id: number): Promise<TemplateDetail> {
  const res = await apiFetch<TemplateResponse>(`/api/templates/${id}`)
  return res.template
}

/** POST /api/templates/{id}/apply — instantiate the template. body carries params + the target set. */
export async function applyTemplate(id: number, body: Record<string, unknown>): Promise<ApplyResponse> {
  return apiFetch<ApplyResponse>(`/api/templates/${id}/apply`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
}
