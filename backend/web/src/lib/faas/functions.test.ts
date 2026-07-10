import { describe, it, expect } from 'vitest'
import { TRIGGER_TYPES, NAME_RE, validName, newFunctionError, blastRadiusLabel } from './functions'
import type { TriggerType } from './types'

// F1 — validName mirrors the server charset (^[a-z0-9][a-z0-9._-]{0,127}$). Red: a name the server would
// 422 passes the client, so the operator submits a doomed create; or a valid name is rejected.
describe('validName', () => {
  it('accepts lowercase slugs with the allowed punctuation', () => {
    for (const ok of ['weather', 'a', 'pac-man', 'my.fn_2', '0', 'a'.repeat(128)]) {
      expect(validName(ok), ok).toBe(true)
    }
  })
  it('rejects spaces, capitals, leading punctuation, empties and over-length', () => {
    for (const bad of ['Bad Name!', '', 'Weather', '-lead', '.lead', '_lead', 'has space', 'a'.repeat(129), 'x/y']) {
      expect(validName(bad), bad).toBe(false)
    }
  })
  it('NAME_RE is anchored (no partial match on an invalid name)', () => {
    expect(NAME_RE.test('ok\nBad Name!')).toBe(false)
  })
})

// F2 — newFunctionError surfaces the first blocking reason in server order, null when submittable. Red:
// the form submits an invalid draft, or blocks a valid one.
describe('newFunctionError', () => {
  const ok = { name: 'weather', source: 'export default async()=>({})', triggerType: 'render' as TriggerType }
  it('passes a valid draft', () => {
    expect(newFunctionError(ok)).toBeNull()
  })
  it('requires a name first', () => {
    // A34.2: messages resolve through the catalog; node falls back to baseLocale (de).
    expect(newFunctionError({ ...ok, name: '   ' })).toBe('Name ist erforderlich')
  })
  it('validates the name charset', () => {
    expect(newFunctionError({ ...ok, name: 'Bad Name!' })).toContain('^[a-z0-9]')
  })
  it('requires source (an empty function has nothing to run)', () => {
    expect(newFunctionError({ ...ok, source: '  \n ' })).toBe('Quelltext ist erforderlich')
  })
  it('checks name before source (server order)', () => {
    expect(newFunctionError({ name: '', source: '', triggerType: 'render' })).toBe('Name ist erforderlich')
  })
  it('rejects an out-of-vocabulary trigger type', () => {
    expect(newFunctionError({ ...ok, triggerType: 'cron' as TriggerType })).toContain('render, schedule')
  })
})

// F3 — blast-radius label pluralization (D25.9). Red: "1 devices" / a bare 0 misleads the operator about
// how many devices an edit or delete touches.
describe('blastRadiusLabel', () => {
  it('spells zero, singular and plural', () => {
    expect(blastRadiusLabel(0)).toBe('keine Geräte')
    expect(blastRadiusLabel(1)).toBe('1 Gerät')
    expect(blastRadiusLabel(3)).toBe('3 Geräte')
  })
  it('treats a negative count as none (defensive)', () => {
    expect(blastRadiusLabel(-1)).toBe('keine Geräte')
  })
})

// F4 — the trigger vocabulary is exactly the three 0009 kinds, in author order.
describe('TRIGGER_TYPES', () => {
  it('is render, schedule, webhook', () => {
    expect([...TRIGGER_TYPES]).toEqual(['render', 'schedule', 'webhook'])
  })
})
