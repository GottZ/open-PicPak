import { describe, it, expect } from 'vitest'
import { buildEnrollBody, isEnrollSuccess, canEnroll, enrollNeedsConfirm } from './enroll'
import { serialApiSupported, secureContextOk, webSerialReady } from './support'
import type { Device } from '../api/types'

const PUBKEY_HEX = '04' + 'ab'.repeat(64)

describe('buildEnrollBody (D26.5 bare serial + D26.7 pubkey + COH34 omit-to-preserve)', () => {
  it('POSTs the bare inner serial and the base64 pubkey, omitting label/channel when unset', () => {
    const body = buildEnrollBody({ serial: 'PP-001', pubkeyHex: PUBKEY_HEX })
    expect(body.serial).toBe('PP-001') // bare inner <S>, NOT the JSON blob (D26.5)
    expect(body.ecdsa_pubkey.length).toBeGreaterThan(80) // base64 of 65 bytes
    expect('label' in body).toBe(false)
    expect('channel' in body).toBe(false) // COH34: absent → Doc 17 COALESCE preserves the stored channel
  })
  it('includes label/channel only when the operator set them this session', () => {
    const body = buildEnrollBody({ serial: 'PP-001', pubkeyHex: PUBKEY_HEX, label: 'lobby', channel: 'beta' })
    expect(body.label).toBe('lobby')
    expect(body.channel).toBe('beta')
    // an empty string is treated as unset (omit-to-preserve)
    expect('channel' in buildEnrollBody({ serial: 'PP-001', pubkeyHex: PUBKEY_HEX, channel: '' })).toBe(false)
  })
  it('throws before POST on a bad pubkey (D26.7 pre-check)', () => {
    expect(() => buildEnrollBody({ serial: 'PP-001', pubkeyHex: '04ab' })).toThrow(/65 bytes/)
  })
})

describe('isEnrollSuccess (D17.4 / F5)', () => {
  it('treats created / rebonded / updated all as success', () => {
    expect(isEnrollSuccess('created')).toBe(true)
    expect(isEnrollSuccess('rebonded')).toBe(true)
    expect(isEnrollSuccess('updated')).toBe(true) // idempotent re-enroll is NOT a failure
  })
  it('is false for an unknown action', () => {
    expect(isEnrollSuccess('nope')).toBe(false)
  })
})

describe('canEnroll (D26.8 / F6)', () => {
  it('needs admin AND a pubkey', () => {
    expect(canEnroll(true, true)).toBe(true)
    expect(canEnroll(false, true)).toBe(false) // non-admin: affordance hidden; server 403s regardless
    expect(canEnroll(true, false)).toBe(false) // no pubkey yet
  })
})

describe('enrollNeedsConfirm (G9 serial-collision)', () => {
  const dev: Device = { serial: 'PP-001', label: null, channel: 'stable', last_seen: null, bonded: true }
  it('requires confirm when the serial already exists, not for a fresh serial', () => {
    expect(enrollNeedsConfirm(dev)).toBe(true)
    expect(enrollNeedsConfirm(null)).toBe(false)
  })
})

describe('WebSerial support gate (F7)', () => {
  it('serialApiSupported detects navigator.serial', () => {
    expect(serialApiSupported({ serial: {} })).toBe(true)
    expect(serialApiSupported({})).toBe(false)
    expect(serialApiSupported(null)).toBe(false)
  })
  it('secureContextOk requires a secure context', () => {
    expect(secureContextOk(true)).toBe(true)
    expect(secureContextOk(false)).toBe(false)
    expect(secureContextOk(undefined)).toBe(false)
  })
  it('webSerialReady needs both', () => {
    expect(webSerialReady({ serial: {} }, true)).toBe(true)
    expect(webSerialReady({ serial: {} }, false)).toBe(false)
    expect(webSerialReady({}, true)).toBe(false)
  })
})
