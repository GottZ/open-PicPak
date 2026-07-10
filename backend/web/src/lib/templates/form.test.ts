// Node tests for the template param-form logic (A30 W7). Each case names the red it guards.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import {
  applyErrorCode,
  buildApplyParams,
  formValueInit,
  isApplyErrorCode,
  missingRequired,
} from './form'
import type { ParamSpec } from './types'

const specs: ParamSpec[] = [
  { name: 'city', label: 'City', type: 'string', required: true },
  { name: 'count', label: 'Count', type: 'number', required: false, default: 3 },
  { name: 'mode', label: 'Mode', type: 'enum', required: true, options: ['a', 'b'] },
  { name: 'src', label: 'Source', type: 'url', required: false },
]

describe('formValueInit', () => {
  it('seeds a number default as text, an enum with its first option, others empty', () => {
    const v = formValueInit(specs)
    expect(v.city).toBe('') // no default → empty (red: a phantom default would prefill a required field)
    expect(v.count).toBe('3') // number default stringified
    expect(v.mode).toBe('a') // first enum option
    expect(v.src).toBe('')
  })
  it('drops the quotes on a string default', () => {
    const v = formValueInit([{ name: 'x', label: 'X', type: 'string', required: false, default: 'hello' }])
    expect(v.x).toBe('hello')
  })
})

describe('missingRequired', () => {
  it('blocks when a required field is blank (client-side missing_param guard)', () => {
    // red without the guard: an empty required field would POST straight to /apply and 422 server-side.
    const miss = missingRequired(specs, { city: '  ', count: '3', mode: 'a', src: '' })
    expect(miss).toEqual(['city'])
  })
  it('passes when all required fields are filled; an empty optional is fine', () => {
    expect(missingRequired(specs, { city: 'Berlin', count: '', mode: 'b', src: '' })).toEqual([])
  })
})

describe('buildApplyParams', () => {
  it('coerces a number, keeps strings, omits empty optionals', () => {
    const body = buildApplyParams(specs, { city: 'Berlin', count: '5', mode: 'a', src: '' })
    expect(body).toEqual({ city: 'Berlin', count: 5, mode: 'a' }) // src omitted (empty optional)
    expect(typeof body.count).toBe('number') // red: a stringified number would not read as a JSON number
  })
  it('lets a non-numeric number value flow through as a string for the server to reject', () => {
    const body = buildApplyParams(specs, { count: 'x' })
    expect(body.count).toBe('x')
  })
})

describe('applyErrorCode / isApplyErrorCode', () => {
  it('extracts the server machine code from the error envelope', () => {
    const err = new ApiError(422, 'validation', 'bad', 'req-1', {
      success: false,
      error: 'parameter is not declared',
      code: 'unknown_param',
    })
    const code = applyErrorCode(err)
    expect(code).toBe('unknown_param') // red: reading ApiError.code alone would show 'validation', not the field fault
    expect(isApplyErrorCode(code)).toBe(true)
  })
  it('falls back to the client code when no envelope code is present', () => {
    const err = new ApiError(0, 'internal', 'network down')
    expect(applyErrorCode(err)).toBe('internal')
    expect(isApplyErrorCode('internal')).toBe(false)
  })
})
