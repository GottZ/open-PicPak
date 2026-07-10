// App-locale plumbing for the admin SPA (design A34 §2). The <html lang>
// attribute is the single source of truth for the active locale: it is
// load-bearing for the system-font stacks (correct CJK/Cyrillic glyph
// selection needs the right lang), so getLocale() is overwritten to read it.
// Detection order (§2): localStorage['picpak.locale'] → navigator language
// match → 'en'. The switch persists the choice and reloads (plain-Svelte
// consistent — no reactive per-callsite rewrite, §Autonome Festlegungen).
import {
  baseLocale,
  isLocale,
  locales,
  overwriteGetLocale,
  type Locale,
} from '../paraglide/runtime.js'

export { locales, type Locale }

const STORAGE_KEY = 'picpak.locale'
// A navigator miss falls back to English, not to the (German) baseLocale (§2).
const FALLBACK: Locale = 'en'

/** Endonyms — language names shown in their own language, never translated. */
export const localeLabels: Record<Locale, string> = {
  de: 'Deutsch',
  en: 'English',
}

/** Detection cascade (§2): stored choice → browser preference → English. */
function detectLocale(): Locale {
  const stored = localStorage.getItem(STORAGE_KEY)
  if (isLocale(stored)) return stored
  const tags = navigator.languages.length ? navigator.languages : [navigator.language]
  for (const tag of tags) {
    const primary = tag.split('-')[0]?.toLowerCase()
    if (isLocale(primary)) return primary
  }
  return FALLBACK
}

/** The locale currently in effect (read from the load-bearing <html lang>). */
export function activeLocale(): Locale {
  const lang = document.documentElement.lang
  return isLocale(lang) ? lang : baseLocale
}

/**
 * Resolve the locale once at boot: write it to <html lang> and make it the
 * source getLocale() reads. Must run before the app renders any message.
 */
export function initLocale(): void {
  document.documentElement.lang = detectLocale()
  overwriteGetLocale(activeLocale)
}

/** Persist a new choice and reload — the plain-Svelte switch (§2). */
export function switchLocale(next: Locale): void {
  localStorage.setItem(STORAGE_KEY, next)
  document.documentElement.lang = next
  location.reload()
}
