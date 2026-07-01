import { describe, it, expect } from 'vitest'
import {
  PUBKEY_LEN,
  parsePubkeyLine,
  hexToBytes,
  bytesToBase64,
  pubkeyHexToBase64,
} from './pubkey'

// A valid uncompressed P-256 point: 0x04 lead + 64 bytes of X||Y. Value is arbitrary (air-gap: not a real key).
const VALID_HEX = '04' + 'ab'.repeat(64) // 130 hex chars, 65 bytes, 0x04 lead

describe('parsePubkeyLine (F1 field extract)', () => {
  it('extracts the hex from a C2 BOND success line', () => {
    expect(parsePubkeyLine(`C2   ecdsa_pubkey(65)=${VALID_HEX}`)).toBe(VALID_HEX)
  })
  it('lowercases the captured hex', () => {
    expect(parsePubkeyLine('C2 ecdsa_pubkey(65)=04ABCDEF')).toBe('04abcdef')
  })
  it('returns null on a non-pubkey line', () => {
    expect(parsePubkeyLine('OK')).toBeNull()
    expect(parsePubkeyLine('ERR  bond/keygen failed')).toBeNull()
  })
})

describe('pubkeyHexToBase64 (F1 transform + D26.7 pre-check)', () => {
  it('round-trips a valid 130-hex point to base64 and back to the same bytes', () => {
    const b64 = pubkeyHexToBase64(VALID_HEX)
    // decode the base64 back and assert it equals the original 65 bytes
    const back = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))
    expect(back.length).toBe(PUBKEY_LEN)
    expect([...back]).toEqual([...hexToBytes(VALID_HEX)])
  })

  // Red: a bare hex→b64 with no length/prefix check would POST a bad key → a server round-trip 422 instead
  // of a legible bench error. The pre-check rejects before POST.
  it('rejects a 128-hex (truncated, 64-byte) blob before POST', () => {
    const truncated = '04' + 'ab'.repeat(63) // 128 hex, 64 bytes
    expect(() => pubkeyHexToBase64(truncated)).toThrow(/65 bytes/)
  })
  it('rejects a non-0x04-leading 65-byte blob before POST', () => {
    const badLead = '02' + 'ab'.repeat(64) // 65 bytes but compressed-point lead
    expect(() => pubkeyHexToBase64(badLead)).toThrow(/0x04/)
  })
  it('rejects odd-length / non-hex input', () => {
    expect(() => pubkeyHexToBase64('04abc')).toThrow(/odd hex/)
    expect(() => pubkeyHexToBase64('04xy' + 'ab'.repeat(63))).toThrow(/non-hex/)
  })
})

describe('bytesToBase64', () => {
  it('encodes bytes to standard base64', () => {
    expect(bytesToBase64(new Uint8Array([0x04, 0x00, 0xff]))).toBe(btoa('\x04\x00\xff'))
  })
})
