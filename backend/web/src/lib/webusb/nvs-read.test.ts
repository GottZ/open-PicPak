import { describe, it, expect } from 'vitest'
import { readNvsIdentity, readFactorySerial, selectWifi, prefillField, type NvsIdentity } from './nvs-read'

// Device-free unit surface for the NVS reader (the flash read itself is the W3 on-device gate). We
// synthesise a real NVS page per the on-flash spec — 32 B page header + 32 B entry-state bitmap +
// 126 × 32 B entries — with the exact entry shapes the parser must handle: a U8 namespace table, a
// v2 chunked BLOB (BLOB_IDX + BLOB_DATA, for dev_sn / wifi.*), and SZ strings (for picpak ssid/pass/url).

const PAGE = 4096
const HDR = 32
const EA = 64 // page header + entry-state bitmap
const ENTRY = 32
const T_U8 = 0x01
const T_SZ = 0x21
const T_BLOB_DATA = 0x42
const T_BLOB_IDX = 0x48

class PageBuilder {
  page = new Uint8Array(PAGE).fill(0xff)
  private next = 0
  constructor() {
    // page state = ACTIVE (0xFFFFFFFE)
    this.page[0] = 0xfe
  }
  private markWritten(i: number) {
    // WRITTEN = 0b10 → clear the low bit of entry i's 2-bit field (freshly erased flash = 0b11)
    this.page[HDR + (i >> 2)] &= ~(0b01 << ((i & 3) * 2)) & 0xff
  }
  private base(i: number) {
    return EA + i * ENTRY
  }
  private writeKey(base: number, key: string) {
    const n = Math.min(key.length, 15)
    for (let j = 0; j < n; j++) this.page[base + 8 + j] = key.charCodeAt(j)
    this.page[base + 8 + n] = 0 // NUL terminator
  }
  private u16(o: number, v: number) {
    this.page[o] = v & 0xff
    this.page[o + 1] = (v >> 8) & 0xff
  }
  private u32(o: number, v: number) {
    this.page[o] = v & 0xff
    this.page[o + 1] = (v >> 8) & 0xff
    this.page[o + 2] = (v >> 16) & 0xff
    this.page[o + 3] = (v >> 24) & 0xff
  }

  namespace(name: string, index: number): this {
    const i = this.next++
    this.markWritten(i)
    const b = this.base(i)
    this.page[b] = 0 // ns 0 = namespace table
    this.page[b + 1] = T_U8
    this.page[b + 2] = 1 // span
    this.page[b + 3] = 0xff
    this.writeKey(b, name)
    this.page[b + 24] = index // U8 value = assigned namespace index
    return this
  }

  str(ns: number, key: string, value: string): this {
    const bytes = new TextEncoder().encode(value + '\0') // NVS strings carry the NUL terminator
    const span = 1 + Math.ceil(bytes.length / 32)
    const i = this.next
    for (let k = 0; k < span; k++) this.markWritten(i + k)
    this.next += span
    const b = this.base(i)
    this.page[b] = ns
    this.page[b + 1] = T_SZ
    this.page[b + 2] = span
    this.page[b + 3] = 0xff
    this.writeKey(b, key)
    this.u16(b + 24, bytes.length)
    this.page.set(bytes, this.base(i + 1))
    return this
  }

  blob(ns: number, key: string, value: Uint8Array): this {
    // v2 single-chunk blob: BLOB_IDX header + one BLOB_DATA chunk (chunkIndex 0)
    const idxI = this.next++
    this.markWritten(idxI)
    const ib = this.base(idxI)
    this.page[ib] = ns
    this.page[ib + 1] = T_BLOB_IDX
    this.page[ib + 2] = 1
    this.page[ib + 3] = 0xff
    this.writeKey(ib, key)
    this.u32(ib + 24, value.length) // dataSize
    this.page[ib + 28] = 1 // chunkCount
    this.page[ib + 29] = 0 // chunkStart

    const span = 1 + Math.ceil(value.length / 32)
    const di = this.next
    for (let k = 0; k < span; k++) this.markWritten(di + k)
    this.next += span
    const db = this.base(di)
    this.page[db] = ns
    this.page[db + 1] = T_BLOB_DATA
    this.page[db + 2] = span
    this.page[db + 3] = 0 // chunkIndex 0
    this.writeKey(db, key)
    this.u16(db + 24, value.length)
    this.page.set(value, this.base(di + 1))
    return this
  }

  build(): Uint8Array {
    // pad to two pages; the second stays uninitialised (all 0xFF) → must be skipped by the parser
    const out = new Uint8Array(PAGE * 2).fill(0xff)
    out.set(this.page, 0)
    return out
  }
}

const json = (o: unknown) => new TextEncoder().encode(JSON.stringify(o))

describe('readNvsIdentity / readFactorySerial', () => {
  it('extracts the bare serial from the storage/dev_sn JSON envelope (v2 blob chunking)', () => {
    const part = new PageBuilder()
      .namespace('storage', 1)
      .blob(1, 'dev_sn', json({ serial_number: 'PP-042' }))
      .build()
    expect(readFactorySerial(part)).toBe('PP-042')
    expect(readNvsIdentity(part).serial).toBe('PP-042')
  })

  it('reads picpak ssid/pass/url STRING entries and never surfaces the private key', () => {
    const part = new PageBuilder()
      .namespace('storage', 1)
      .namespace('picpak', 2)
      .blob(1, 'dev_sn', json({ serial_number: 'PP-001' }))
      .str(2, 'ssid', 'HomeNet')
      .str(2, 'pass', 's3cr3t-pw')
      .str(2, 'url', 'https://host.example/d/tok123')
      .blob(2, 'c2_sk', new Uint8Array(32).fill(0xaa)) // present on real devices — must be ignored
      .build()
    const id = readNvsIdentity(part)
    expect(id).toEqual({
      serial: 'PP-001',
      ssid: 'HomeNet',
      pass: 's3cr3t-pw',
      url: 'https://host.example/d/tok123',
      wifiProfiles: [],
    })
    // security boundary: the parser exposes only ssid/pass/url from picpak — the c2_sk private-key
    // bytes (0xAA) never reach the identity (pass is the real password, not the key).
    expect(id.pass).toBe('s3cr3t-pw')
    expect(String.fromCharCode(0xaa).repeat(4)).not.toContain(id.pass ?? '')
  })

  it('collects storage/wifi.* profiles and orders them by priority (highest first)', () => {
    const part = new PageBuilder()
      .namespace('storage', 1)
      .blob(1, 'wifi.home-1a', json({ ssid: 'Home', pass: 'p1', prio: 5 }))
      .blob(1, 'wifi.cafe-2b', json({ ssid: 'Cafe', pass: 'p2', prio: 9 }))
      .build()
    const profiles = readNvsIdentity(part).wifiProfiles
    expect(profiles.map((p) => p.ssid)).toEqual(['Cafe', 'Home'])
    expect(profiles[0]).toEqual({ ssid: 'Cafe', pass: 'p2', prio: 9 })
  })

  it('returns all-null / empty for a blank (uninitialised) or too-short partition', () => {
    expect(readFactorySerial(new Uint8Array(PAGE).fill(0xff))).toBeNull()
    expect(readNvsIdentity(new Uint8Array(PAGE).fill(0xff))).toEqual({
      serial: null,
      ssid: null,
      pass: null,
      url: null,
      wifiProfiles: [],
    })
    expect(readFactorySerial(new Uint8Array(16))).toBeNull() // shorter than one page
  })

  it('returns null serial when the dev_sn envelope is corrupt JSON', () => {
    const part = new PageBuilder()
      .namespace('storage', 1)
      .blob(1, 'dev_sn', new TextEncoder().encode('{not json'))
      .build()
    expect(readFactorySerial(part)).toBeNull()
  })
})

describe('selectWifi', () => {
  const base: NvsIdentity = { serial: null, ssid: null, pass: null, url: null, wifiProfiles: [] }
  it('prefers the active picpak config', () => {
    expect(selectWifi({ ...base, ssid: 'Active', pass: 'pw' })).toEqual({ ssid: 'Active', pass: 'pw' })
  })
  it('falls back to the highest-priority stored profile', () => {
    expect(selectWifi({ ...base, wifiProfiles: [{ ssid: 'Top', pass: 'tp', prio: 9 }] })).toEqual({
      ssid: 'Top',
      pass: 'tp',
    })
  })
  it('returns null when nothing was read', () => {
    expect(selectWifi(base)).toBeNull()
  })
})

describe('prefillField (A35.3 prefill discipline)', () => {
  it('fills a blank field from the detected value', () => {
    expect(prefillField('', 'PP-007')).toBe('PP-007')
    expect(prefillField('   ', 'PP-007')).toBe('PP-007')
  })
  it('never overwrites a value the operator already typed', () => {
    expect(prefillField('PP-typed', 'PP-detected')).toBe('PP-typed')
  })
  it('leaves a blank field blank when nothing was detected', () => {
    expect(prefillField('', null)).toBe('')
  })
})
