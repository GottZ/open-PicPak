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
import { m } from '../paraglide/messages.js'

export { locales, type Locale }

/**
 * Localized nav/area label for an operator area path (routes/index.ts AREAS).
 * The route table stays a pure, node-tested data module (its title is the stable
 * identifier); the display string is resolved here so the shell nav and the
 * AreaPlaceholder render the same translated label. Unknown paths fall through
 * to the raw path (never throws).
 */
export function navLabel(path: string): string {
  switch (path) {
    case '/fleet':
      return m['nav.fleet']()
    case '/ota':
      return m['nav.ota']()
    case '/logs':
      return m['nav.logs']()
    case '/berry':
      return m['nav.berry']()
    case '/functions':
      return m['nav.functions']()
    case '/onboard':
      return m['nav.onboard']()
    case '/settings':
      return m['nav.settings']()
    case '/media':
      return m['nav.media']()
    default:
      return path
  }
}

/** Localized "ships in <doc>" placeholder copy for the not-yet-built areas. */
export function areaShips(path: string): string {
  switch (path) {
    case '/ota':
      return m['nav.ota.ships']()
    case '/settings':
      return m['nav.settings.ships']()
    default:
      return ''
  }
}

const STORAGE_KEY = 'picpak.locale'
// A navigator miss falls back to English, not to the (German) baseLocale (§2).
const FALLBACK: Locale = 'en'

/** Endonyms — language names shown in their own language, never translated. */
export const localeLabels: Record<Locale, string> = {
  de: 'Deutsch',
  en: 'English',
  fr: 'Français',
  it: 'Italiano',
  es: 'Español',
  ru: 'Русский',
  zh: '中文',
  ko: '한국어',
  ja: '日本語',
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
