// Gallery apply-body builders (design/33 §4.5b, W-A33.5b). Pure node tests — the request body is the
// contract with the A30 /apply route, so it is asserted here without a DOM. Each names its red.

import { describe, expect, it } from 'vitest'
import { buildBerryApply, buildRenderFnApply } from './apply'
import type { ParamSpec } from '../templates/types'

const specs: ParamSpec[] = [
  { name: 'city', label: 'Stadt', type: 'string', required: true },
  { name: 'ttl', label: 'TTL', type: 'number', required: false, default: 60 },
]
const values = { city: 'Berlin', ttl: '120' }

describe('buildRenderFnApply', () => {
  // Probe (a) — the mandatory device step: no device picked ⇒ null (the component derives Bestätigen
  // disabled from this). Red: a builder that defaulted target_serials would let a laie apply to nothing.
  it('returns null when no device is picked (mandatory-step gate)', () => {
    expect(buildRenderFnApply(specs, values, { serial: null })).toBeNull()
  })

  // Probe (b) — the body carries enable:true + bind:true + target_serials, and params coerce. Red
  // (against the pre-delta gallery / raw TemplatePicker, which sent neither bind nor enable): the minted
  // function would come out unbound and disabled.
  it('a picked device yields enable:true + bind:true + target_serials + coerced params', () => {
    const body = buildRenderFnApply(specs, values, { serial: 'dev-1' })
    expect(body).toEqual({
      params: { city: 'Berlin', ttl: 120 },
      target_serials: ['dev-1'],
      bind: true,
      enable: true,
    })
  })

  it('rides an explicit fn_name when given, omits it when blank', () => {
    const withName = buildRenderFnApply(specs, values, { serial: 'dev-1', fnName: ' weather ' })
    expect(withName?.fn_name).toBe('weather')
    const blank = buildRenderFnApply(specs, values, { serial: 'dev-1', fnName: '  ' })
    expect('fn_name' in (blank ?? {})).toBe(false)
  })
})

describe('buildBerryApply', () => {
  it('returns null with no device in one-mode (mandatory-step gate, probe a)', () => {
    expect(buildBerryApply(specs, values, { mode: 'one', serial: null, armed: false })).toBeNull()
  })

  it('returns null for an un-armed fleet broadcast (the deliberate * arm is required)', () => {
    expect(buildBerryApply(specs, values, { mode: 'fleet', serial: null, armed: false })).toBeNull()
  })

  it('one device ⇒ target_serials:[serial]; no bind/enable (a snippet mints no function)', () => {
    const body = buildBerryApply(specs, values, { mode: 'one', serial: 'dev-2', armed: false })
    expect(body).toEqual({ params: { city: 'Berlin', ttl: 120 }, target_serials: ['dev-2'] })
  })

  it('an armed fleet broadcast ⇒ target_serials:["*"]', () => {
    const body = buildBerryApply(specs, values, { mode: 'fleet', serial: null, armed: true })
    expect(body?.target_serials).toEqual(['*'])
  })
})
