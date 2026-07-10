import { describe, it, expect } from 'vitest'
import {
  validateSerial,
  validateSsid,
  validatePassword,
  validateUrl,
  validateSeconds,
  validateProvisionFields,
  hasErrors,
  lineWithinConsoleCap,
  CON_LINE_MAX,
  type ProvisionFields,
} from './validate'

const okFields: ProvisionFields = {
  serial: 'PP_test-01',
  ssid: 'homenet',
  password: 'a good passphrase', // spaces allowed in a passphrase (rest-of-line)
  frameUrl: 'https://frames.example/pic.bin',
  c2Url: 'https://c2.example/poll',
  c2PeriodSeconds: '1800',
  wakeSeconds: '',
}

describe('validateSerial (D26.11 / F9 charset + on-device cap)', () => {
  it('accepts the charset', () => {
    expect(validateSerial('PP_test-01')).toBeNull()
    expect(validateSerial('a')).toBeNull()
    expect(validateSerial('A'.repeat(31))).toBeNull()
  })
  // Red: an unvalidated serial with CR/LF injects a second console command (console.c:69 line split);
  // a 40-char serial truncates to 31 on-device and silently breaks D26.5.
  it('rejects CR/LF injection, quotes, spaces, over-length', () => {
    expect(validateSerial('x\r\nNVSSET picpak c2_sk deadbeef')).not.toBeNull()
    expect(validateSerial('a"b')).not.toBeNull()
    expect(validateSerial('a b')).not.toBeNull()
    expect(validateSerial('Z'.repeat(32))).not.toBeNull()
    expect(validateSerial('')).not.toBeNull()
  })
})

describe('validateSsid / validatePassword', () => {
  it('rejects a spaced SSID (console tokenization) and control chars', () => {
    // A34.2: validation messages resolve through the catalog; node → baseLocale (de).
    expect(validateSsid('my net')).toMatch(/Leerzeichen/)
    expect(validateSsid('net\r')).toMatch(/Steuerzeichen/)
    expect(validateSsid('ok-ssid')).toBeNull()
  })
  it('rejects a password with CR/LF but allows spaces and empty', () => {
    expect(validatePassword('pass\nword')).toMatch(/Steuerzeichen/)
    expect(validatePassword('a spaced pass')).toBeNull()
    expect(validatePassword('')).toBeNull() // open network
  })
})

describe('validateUrl (https-only firmware gate)', () => {
  it('requires https and rejects control chars', () => {
    expect(validateUrl('http://x/y', 'C2 URL')).toMatch(/https/)
    expect(validateUrl('https://x/\ry', 'C2 URL')).toMatch(/Steuerzeichen/)
    expect(validateUrl('https://x/y', 'C2 URL')).toBeNull()
    expect(validateUrl('', 'C2 URL')).toMatch(/erforderlich/)
  })
})

describe('validateSeconds (optional numeric)', () => {
  it('accepts blank/undefined and digits, rejects non-numeric', () => {
    expect(validateSeconds('', 'C2 period')).toBeNull()
    expect(validateSeconds(undefined, 'C2 period')).toBeNull()
    expect(validateSeconds('1800', 'C2 period')).toBeNull()
    expect(validateSeconds('30s', 'C2 period')).toMatch(/ganze Sekundenzahl/)
  })
})

describe('validateProvisionFields', () => {
  it('passes a good field set', () => {
    expect(hasErrors(validateProvisionFields(okFields))).toBe(false)
  })
  it('collects per-field errors', () => {
    const errs = validateProvisionFields({ ...okFields, serial: 'bad serial!', c2Url: 'http://insecure' })
    expect(errs.serial).toBeDefined()
    expect(errs.c2Url).toBeDefined()
    expect(hasErrors(errs)).toBe(true)
  })
})

describe('lineWithinConsoleCap', () => {
  it('bounds at CON_LINE_MAX', () => {
    expect(lineWithinConsoleCap('x'.repeat(CON_LINE_MAX))).toBe(true)
    expect(lineWithinConsoleCap('x'.repeat(CON_LINE_MAX + 1))).toBe(false)
  })
})
