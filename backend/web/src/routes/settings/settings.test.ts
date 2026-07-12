// W2 gates (design 02-settings-spa §5/§7-W2): structural source-string pins, no
// component-render harness exists in this repo (Muster src/routes/ota/ota.test.ts
// header) — every invariant below is verified against the ACTUAL wired-up source
// so a regression turns the pin red. Live red→green verification for N2/N3/N4 is
// documented in the B2 report (temporary local edits, reverted, never committed).

import { describe, expect, it } from 'vitest'
import settingsHomeSrc from './SettingsHome.svelte?raw'
import secretsPanelSrc from './SecretsPanel.svelte?raw'
import diagnosticsPanelSrc from './DiagnosticsPanel.svelte?raw'
import tokensPanelSrc from './TokensPanel.svelte?raw'

// N4 — {@html} ban (§5 B4/B6). Muster ota.test.ts's sibling gate; the AST gate
// (scripts/lint-no-html.ts) covers every .svelte file structurally, this pins it
// locally at the settings sources too.
describe('Settings sources never render an operator string via {@html} (B4/B6, N4)', () => {
  it('SettingsHome, SecretsPanel, DiagnosticsPanel and TokensPanel carry no {@html} directive', () => {
    expect(settingsHomeSrc).not.toMatch(/\{@html[\s(]/)
    expect(secretsPanelSrc).not.toMatch(/\{@html[\s(]/)
    expect(diagnosticsPanelSrc).not.toMatch(/\{@html[\s(]/)
    expect(tokensPanelSrc).not.toMatch(/\{@html[\s(]/)
  })
})

// B3 (design 02-settings-spa §7-W3): GET /api/config is auth-only (adminhttp.Auth,
// telemetry_http.go:66), NOT RequireAdmin, so DiagnosticsPanel must sit OUTSIDE
// the session.is_admin branch — a confirmed non-admin sees it too. Live-verified
// (§ report): nesting <DiagnosticsPanel /> inside the `{:else if session.is_admin}`
// branch (alongside <SecretsPanel />) makes the assertion below fail red.
describe('SettingsHome mounts DiagnosticsPanel outside the session.is_admin branch (auth-only route, B3)', () => {
  it('<DiagnosticsPanel /> sits after the admin-gate {#if}/{:else if}/{:else}{/if}, not nested inside it', () => {
    const template = settingsHomeSrc.slice(settingsHomeSrc.indexOf('</script>'))
    const adminIdx = template.indexOf('{:else if session.is_admin}')
    const elseIdx = template.indexOf('{:else}', adminIdx)
    const closeIfIdx = template.indexOf('{/if}', elseIdx)
    const diagIdx = template.indexOf('<DiagnosticsPanel')
    expect(adminIdx).toBeGreaterThan(-1)
    expect(elseIdx).toBeGreaterThan(-1)
    expect(closeIfIdx).toBeGreaterThan(-1)
    expect(diagIdx).toBeGreaterThan(-1)
    expect(diagIdx).toBeGreaterThan(closeIfIdx)
  })
})

// N2 — B5 "leere Tabelle statt ehrlicher Sperre": SecretsPanel is only ever
// mounted from the confirmed-admin branch. Live-verified (§ report): replacing
// `{:else if session.is_admin}` with a bare `{:else}` (dropping the admin check,
// so a non-admin would reach SecretsPanel) makes the first assertion below fail
// red, because the exact gated-mount string it searches for no longer exists.
describe('SettingsHome gates SecretsPanel on session.is_admin, not a bare fallthrough (§5 B5, N2)', () => {
  it('mounts <SecretsPanel /> only inside the {:else if session.is_admin} branch', () => {
    expect(settingsHomeSrc).toMatch(/\{:else if session\.is_admin\}\s*<SecretsPanel \/>/)
  })

  it('the trailing {:else} branch (confirmed non-admin) renders needs_admin, never SecretsPanel', () => {
    const elseIdx = settingsHomeSrc.lastIndexOf('{:else}')
    const endIdx = settingsHomeSrc.indexOf('{/if}', elseIdx)
    expect(elseIdx).toBeGreaterThan(-1)
    const needsAdminBlock = settingsHomeSrc.slice(elseIdx, endIdx)
    expect(needsAdminBlock).toMatch(/m\['settings\.needs_admin'\]\(\)/)
    expect(needsAdminBlock).not.toMatch(/SecretsPanel/)
  })

  // Restore-Ordnung (§5 B5 Korrektheit): the FIRST branch must be session.restoring
  // (idle render), not session.is_admin — else a legitimate admin flashes the
  // needs-admin lock on every reload until whoami resolves (auth.svelte.ts:15,43).
  it('checks session.restoring BEFORE session.is_admin (idle-first, no false-lock flash)', () => {
    // scoped to the TEMPLATE only (after </script>) — the design-comment prose
    // above legitimately mentions both identifiers out of branch order.
    const template = settingsHomeSrc.slice(settingsHomeSrc.indexOf('</script>'))
    const restoringIdx = template.indexOf('{#if session.restoring}')
    const adminIdx = template.indexOf('{:else if session.is_admin}')
    expect(restoringIdx).toBeGreaterThan(-1)
    expect(adminIdx).toBeGreaterThan(-1)
    expect(restoringIdx).toBeLessThan(adminIdx)
  })
})

// N3 — Write-only invariant (§5 B1): value never survives a submit in a draft,
// and it is synchronously cleared on a successful PUT. Live-verified (§ report):
// removing the `value = ''` reset line from doPut makes the second assertion
// below fail red (the reset-before-notify pin), and temporarily adding a
// saveDraft/loadDraft import makes the first assertion fail red.
describe('SecretsPanel never drafts the value and clears it on a successful PUT (§5 B1, N3)', () => {
  it('imports neither saveDraft nor loadDraft (value is never persisted to localStorage)', () => {
    // matches only an actual import line, not the design-comment prose above that
    // documents the absence (which legitimately names both identifiers).
    expect(secretsPanelSrc).not.toMatch(/^import\s.*\b(saveDraft|loadDraft)\b/m)
  })

  it('doPut resets value to \'\' before notifying success (no lingering plaintext)', () => {
    const fnStart = secretsPanelSrc.indexOf('async function doPut')
    const fn = secretsPanelSrc.slice(fnStart, secretsPanelSrc.indexOf('\n  }\n', fnStart))
    const resetIdx = fn.search(/value = ''/)
    const notifyIdx = fn.indexOf('notify.success')
    expect(resetIdx).toBeGreaterThan(-1)
    expect(notifyIdx).toBeGreaterThan(-1)
    expect(resetIdx).toBeLessThan(notifyIdx) // cleared BEFORE the success toast, not after
  })

  it('the success toast is built from {name, action} only, never from value', () => {
    const fnStart = secretsPanelSrc.indexOf('async function doPut')
    const fn = secretsPanelSrc.slice(fnStart, secretsPanelSrc.indexOf('\n  }\n', fnStart))
    expect(fn).toMatch(/notify\.success\(\s*res\.action === 'created'/)
    expect(fn).not.toMatch(/notify\.success\([^)]*\bvalue\b/)
  })

  it('the catch branch maps the error via settingsErrorText, never echoing value', () => {
    const fnStart = secretsPanelSrc.indexOf('async function doPut')
    const fn = secretsPanelSrc.slice(fnStart, secretsPanelSrc.indexOf('\n  }\n', fnStart))
    const catchIdx = fn.indexOf('} catch (err) {')
    const catchBlock = fn.slice(catchIdx, fn.indexOf('} finally {', catchIdx))
    const assignLine = catchBlock.split('\n').find((l) => l.includes('putError ='))
    expect(assignLine).toMatch(/putError = settingsErrorText\(err\)/)
    // only the CODE half of the line (before any trailing comment) must be checked —
    // the comment legitimately explains the value-never-echoed invariant in prose.
    expect(assignLine?.split('//')[0]).not.toMatch(/\bvalue\b/)
  })
})

describe('SecretsPanel delete is two-step armed, not a single-click destroy (§5 B7/§8 E3, Muster FunctionsEditor)', () => {
  // the load-bearing negative probe: doDelete() MUST arm-and-return on an unarmed
  // row BEFORE it ever reaches the DELETE call.
  it('doDelete arms on the first click and returns before deleteSecret() runs', () => {
    const fnStart = secretsPanelSrc.indexOf('async function doDelete')
    const fn = secretsPanelSrc.slice(fnStart, secretsPanelSrc.indexOf('\n  }\n', fnStart))
    const armIdx = fn.search(/if \(armedName !== secretName\) \{\s*armedName = secretName/)
    const deleteCallIdx = fn.indexOf('await deleteSecret(secretName)')
    expect(armIdx).toBeGreaterThan(-1)
    expect(deleteCallIdx).toBeGreaterThan(-1)
    expect(armIdx).toBeLessThan(deleteCallIdx)
  })

  it('the armed confirm label names the secret AND the silent-degradation warning (E3), not a generic "delete"', () => {
    expect(secretsPanelSrc).toMatch(/m\['settings\.secrets\.delete_confirm'\]\(\{ name: s\.name \}\)/)
  })
})

describe('SecretsPanel mutations are admin-gated (§5 B1) and use mutationAffordance (Live-Demotion-Fenster)', () => {
  it('doPut returns before PUTing when session.is_admin is false', () => {
    const fnStart = secretsPanelSrc.indexOf('async function doPut')
    const fn = secretsPanelSrc.slice(fnStart, fnStart + 300)
    expect(fn).toMatch(/if \(!session\.is_admin[^)]*\)\s*return/)
  })

  it('doDelete returns before deleting when session.is_admin is false', () => {
    const fnStart = secretsPanelSrc.indexOf('async function doDelete')
    const fn = secretsPanelSrc.slice(fnStart, fnStart + 300)
    expect(fn).toMatch(/if \(!session\.is_admin[^)]*\)\s*return/)
  })

  it('the Put submit button is wired to mutationAffordance (disabled/title/aria-disabled)', () => {
    const btnStart = secretsPanelSrc.indexOf('type="submit"')
    const btn = secretsPanelSrc.slice(btnStart, secretsPanelSrc.indexOf("{m['settings.secrets.put']()}"))
    expect(btn).toMatch(/title=\{affordance\.disabled \? affordance\.title : ''\}/)
    expect(btn).toMatch(/aria-disabled=\{affordance\['aria-disabled'\]/)
  })

  it('the Delete button is wired to mutationAffordance (disabled/title)', () => {
    const btnStart = secretsPanelSrc.indexOf('class="danger"')
    const btn = secretsPanelSrc.slice(btnStart, secretsPanelSrc.indexOf('onclick={() => doDelete(s.name)}'))
    expect(btn).toMatch(/disabled=\{affordance\.disabled/)
    expect(btn).toMatch(/title=\{affordance\.disabled \? affordance\.title : ''\}/)
  })
})

describe('SecretsPanel meta table never renders value/length/fingerprint (§5 B3)', () => {
  it('the table row interpolates only name, key_version, created_at and rotated_at from Meta', () => {
    const theadIdx = secretsPanelSrc.indexOf('<thead>')
    const tbodyEnd = secretsPanelSrc.indexOf('</tbody>')
    const tableBlock = secretsPanelSrc.slice(theadIdx, tbodyEnd)
    expect(tableBlock).toMatch(/\{s\.name\}/)
    expect(tableBlock).toMatch(/\{s\.key_version\}/)
    expect(tableBlock).toMatch(/\{s\.created_at\}/)
    expect(tableBlock).toMatch(/\{s\.rotated_at/)
    expect(tableBlock).not.toMatch(/\.value\b/)
    expect(tableBlock).not.toMatch(/\.length\b/)
  })
})

describe('SecretsPanel LIST-load fires on mount, not eagerly at module scope (§4/§8 E4)', () => {
  it('secrets.load() is called from onMount, mirroring the admin-gated mount ordering', () => {
    expect(secretsPanelSrc).toMatch(/onMount\(\(\) => void secrets\.load\(\)\)/)
  })
})

// --- W4 (API-Token-Panel) ---------------------------------------------------------------------

// Platzierungs-Pin analog B3 (dort: DiagnosticsPanel MUSS ausserhalb des is_admin-Zweigs sitzen, weil
// GET /api/config auth-only ist). Hier ist der Kontrapunkt: ALLE DREI Token-Routen sind RequireAdmin
// (token_http.go:26-28) — TokensPanel MUSS also INNERHALB des {:else if session.is_admin}-Zweigs sitzen,
// nach SecretsPanel, wie SecretsPanel selbst. Ein TokensPanel ausserhalb dieses Zweigs waere die B5-
// Falle: ein Nicht-Admin bekaeme auf GET /api/tokens ein 403, das als leere Tabelle liest wie "keine
// Tokens" statt einer ehrlichen Sperre.
describe('SettingsHome mounts TokensPanel inside the session.is_admin branch, after SecretsPanel (RequireAdmin, Muster B5)', () => {
  it('<SecretsPanel /> is immediately followed by <TokensPanel /> inside {:else if session.is_admin}', () => {
    expect(settingsHomeSrc).toMatch(/\{:else if session\.is_admin\}\s*<SecretsPanel \/>\s*<TokensPanel \/>/)
  })

  it('the trailing {:else} branch (confirmed non-admin) never mounts TokensPanel', () => {
    const elseIdx = settingsHomeSrc.lastIndexOf('{:else}')
    const endIdx = settingsHomeSrc.indexOf('{/if}', elseIdx)
    const needsAdminBlock = settingsHomeSrc.slice(elseIdx, endIdx)
    expect(needsAdminBlock).not.toMatch(/TokensPanel/)
  })
})

describe('TokensPanel never drafts the minted token and clears the mint form on success (§7-W4, N2/N3)', () => {
  it('imports neither saveDraft nor loadDraft (the minted token is never persisted to localStorage)', () => {
    expect(tokensPanelSrc).not.toMatch(/^import\s.*\b(saveDraft|loadDraft)\b/m)
  })

  it('doMint sets mintedToken from the response before notifying success', () => {
    const fnStart = tokensPanelSrc.indexOf('async function doMint')
    const fn = tokensPanelSrc.slice(fnStart, tokensPanelSrc.indexOf('\n  }\n', fnStart))
    const mintedIdx = fn.search(/mintedToken = \{ label: res\.label, token: res\.token \}/)
    const notifyIdx = fn.indexOf('notify.success')
    expect(mintedIdx).toBeGreaterThan(-1)
    expect(notifyIdx).toBeGreaterThan(-1)
  })

  // N2 — the load-bearing negative probe (§7-W4 once-shown): the success toast is built from
  // {label: res.label} ONLY — the minted plaintext (res.token) must never be interpolated into a
  // notify.*/log call. Live-verified (§ report): temporarily changing the notify.success call to
  // interpolate res.token makes the second assertion below fail red.
  it('the mint success toast is built from {label: res.label} only, never from res.token', () => {
    const fnStart = tokensPanelSrc.indexOf('async function doMint')
    const fn = tokensPanelSrc.slice(fnStart, tokensPanelSrc.indexOf('\n  }\n', fnStart))
    expect(fn).toMatch(/notify\.success\(\s*m\['settings\.tokens\.minted'\]\(\{ label: res\.label \}\)\)/)
    expect(fn).not.toMatch(/notify\.success\([^)]*\bres\.token\b/)
  })

  it('the mint form fields (label, scopes, expiresLocal) are reset synchronously after a successful mint', () => {
    const fnStart = tokensPanelSrc.indexOf('async function doMint')
    const fn = tokensPanelSrc.slice(fnStart, tokensPanelSrc.indexOf('\n  }\n', fnStart))
    expect(fn).toMatch(/label = ''/)
    expect(fn).toMatch(/scopes = \[\]/)
    expect(fn).toMatch(/expiresLocal = ''/)
  })

  it('the catch branch maps the error via tokensErrorText/tokenErrorField, never echoing the token', () => {
    const fnStart = tokensPanelSrc.indexOf('async function doMint')
    const fn = tokensPanelSrc.slice(fnStart, tokensPanelSrc.indexOf('\n  }\n', fnStart))
    const catchIdx = fn.indexOf('} catch (err) {')
    const catchBlock = fn.slice(catchIdx, fn.indexOf('} finally {', catchIdx))
    expect(catchBlock).toMatch(/mintError = tokensErrorText\(err\)/)
    expect(catchBlock).toMatch(/mintErrorField = tokenErrorField\(err\)/)
    expect(catchBlock.split('//')[0]).not.toMatch(/\btoken\b/)
  })
})

describe('TokensPanel meta table never renders a token/secret field (Muster SecretsPanel §5 B3)', () => {
  it('the table row interpolates only label, scopes, status, created_at, expires_at and last_used from TokenMeta', () => {
    const theadIdx = tokensPanelSrc.indexOf('<thead>')
    const tbodyEnd = tokensPanelSrc.indexOf('</tbody>')
    const tableBlock = tokensPanelSrc.slice(theadIdx, tbodyEnd)
    expect(tableBlock).toMatch(/\{t\.label\}/)
    expect(tableBlock).toMatch(/t\.scopes\.join/)
    expect(tableBlock).toMatch(/tokenStatusText\(t\.status\)/)
    expect(tableBlock).toMatch(/\{t\.created_at\}/)
    expect(tableBlock).toMatch(/t\.expires_at/)
    expect(tableBlock).toMatch(/t\.last_used/)
    expect(tableBlock).not.toMatch(/\.token\b/)
    expect(tableBlock).not.toMatch(/secret_hash/)
  })
})

describe('TokensPanel revoke is two-step armed, not a single-click destroy (Muster SecretsPanel §5 B7/E3)', () => {
  it('doRevoke arms on the first click and returns before revokeToken() runs', () => {
    const fnStart = tokensPanelSrc.indexOf('async function doRevoke')
    const fn = tokensPanelSrc.slice(fnStart, tokensPanelSrc.indexOf('\n  }\n', fnStart))
    const armIdx = fn.search(/if \(armedId !== id\) \{\s*armedId = id/)
    const revokeCallIdx = fn.indexOf('await revokeToken(id)')
    expect(armIdx).toBeGreaterThan(-1)
    expect(revokeCallIdx).toBeGreaterThan(-1)
    expect(armIdx).toBeLessThan(revokeCallIdx)
  })

  it('the armed confirm label names the token label (E3-Muster), not a generic "revoke"', () => {
    expect(tokensPanelSrc).toMatch(/m\['settings\.tokens\.revoke_confirm'\]\(\{ label: t\.label \}\)/)
  })
})

describe('TokensPanel mutations are admin-gated and use mutationAffordance (Muster SecretsPanel)', () => {
  it('doMint returns before minting when session.is_admin is false', () => {
    const fnStart = tokensPanelSrc.indexOf('async function doMint')
    const fn = tokensPanelSrc.slice(fnStart, fnStart + 300)
    // non-greedy [\s\S]*? (not [^)]*): the guard's condition itself contains a nested-parens call
    // (mintFormValid({ label, scopes })), so a single-)-stop class would never reach the closing ")".
    expect(fn).toMatch(/if \(!session\.is_admin[\s\S]*?\)\s*return/)
  })

  it('doRevoke returns before revoking when session.is_admin is false', () => {
    const fnStart = tokensPanelSrc.indexOf('async function doRevoke')
    const fn = tokensPanelSrc.slice(fnStart, fnStart + 300)
    expect(fn).toMatch(/if \(!session\.is_admin[^)]*\)\s*return/)
  })

  it('the mint submit button is wired to mutationAffordance (disabled/title/aria-disabled)', () => {
    const btnStart = tokensPanelSrc.indexOf('type="submit"')
    const btn = tokensPanelSrc.slice(btnStart, tokensPanelSrc.indexOf("{minting ? m['settings.tokens.minting']()"))
    expect(btn).toMatch(/title=\{affordance\.disabled \? affordance\.title : ''\}/)
    expect(btn).toMatch(/aria-disabled=\{affordance\['aria-disabled'\]/)
  })

  it('the revoke button is wired to mutationAffordance (disabled/title)', () => {
    const btnStart = tokensPanelSrc.indexOf('class="danger"')
    const btn = tokensPanelSrc.slice(btnStart, tokensPanelSrc.indexOf('onclick={() => doRevoke(t.id)}'))
    expect(btn).toMatch(/disabled=\{affordance\.disabled/)
    expect(btn).toMatch(/title=\{affordance\.disabled \? affordance\.title : ''\}/)
  })
})

// N3 — Scope-Pflicht (structural pin; the pure-function coverage lives in lib/settings/tokens.test.ts).
describe('TokensPanel submit is guarded by mintFormValid — never enabled without a label or a scope (N3)', () => {
  it('canSubmit is derived from mintFormValid({ label, scopes })', () => {
    expect(tokensPanelSrc).toMatch(/canSubmit = \$derived\(!affordance\.disabled && !minting && mintFormValid\(\{ label, scopes \}\)\)/)
  })
})

describe('TokensPanel LIST-load fires on mount, not eagerly at module scope (Muster SecretsPanel)', () => {
  it('tokens.load() is called from onMount', () => {
    expect(tokensPanelSrc).toMatch(/onMount\(\(\) => void tokens\.load\(\)\)/)
  })
})
