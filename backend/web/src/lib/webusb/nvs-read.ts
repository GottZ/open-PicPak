// Minimal ESP-IDF NVS partition reader (W-A26.10). Resolves exactly the onboarding
// read-backs the SPA surfaces after an esptool connect — NOT a general NVS driver:
//
//   • storage / dev_sn   → BLOB, JSON envelope {"serial_number":"<S>"} → bare factory serial
//   • storage / wifi.*   → BLOB, JSON {"ssid","pass","prio"} → the multi-Wi-Fi profile store
//   • picpak  / ssid,pass,url → STRING entries → the active provisioned Wi-Fi + device URL
//
// Parsed subset of the on-flash NVS format (versions 1 & 2):
//   - Partition = N pages of 4096 B. Page = 32 B header + 32 B entry-state bitmap +
//     126 entries × 32 B (32 + 32 + 126·32 = 4096).
//   - Page header: state uint32 LE @0 (ACTIVE 0xFFFFFFFE / FULL 0xFFFFFFFC live; others skipped).
//   - Entry-state bitmap: 2 bits/entry, LSB-first bit order; only WRITTEN (0b10) entries are live
//     (an updated key erases the old entry → 0b00, so this drops stale values automatically).
//   - Entry (32 B): ns u8 @0, type u8 @1, span u8 @2, chunkIndex u8 @3, crc @4,
//     key[16] @8 (NUL-terminated), data-union[8] @24. `span` = entries the item occupies.
//   - Namespace table: entries in ns 0, type U8 (0x01); the u8 value @24 is the namespace index.
//   - STRING (SZ 0x21): u16 size @24 (incl. NUL), payload in the span-1 following 32-B entries.
//   - v1 BLOB (0x41): u16 size @24, payload in following entries.
//   - v2 BLOB: BLOB_IDX (0x48) header — u32 dataSize @24, u8 chunkCount @28, u8 chunkStart @29 —
//     plus BLOB_DATA (0x42) chunks (chunkIndex ∈ [chunkStart, chunkStart+chunkCount)), reassembled in order.
//
// Any malformed / short / absent input ⇒ null-ish (never throws into the onboarding flow).
//
// SECURITY BOUNDARY (W17) — NOT negotiable:
//   Namespace `picpak` ALSO holds `c2_sk` (32 B blob = the device's ECDSA PRIVATE key / identity)
//   and `c2_pk`. This parser reads ONLY ssid/pass/url from `picpak` and NEVER looks up c2_sk/c2_pk.
//   The point is not obscurity — a physical USB read of flash is inherently possible — it is that
//   surfacing a private key into SPA state / screen / logs would be the actual leak, and it has zero
//   onboarding value. Do not add a c2_sk lookup here or anywhere it could reach JS state.
//   Factory calibration is out of scope by construction: it lives in eFuse (factory), not app-NVS —
//   there is no blob to read, so nothing is built for it.

const PAGE_SIZE = 4096
const HEADER_SIZE = 32
const ENTRY_SIZE = 32
const ENTRY_AREA = HEADER_SIZE + 32 // page header + entry-state bitmap
const ENTRIES_PER_PAGE = 126

const PAGE_ACTIVE = 0xfffffffe
const PAGE_FULL = 0xfffffffc
const STATE_WRITTEN = 0x2

const TYPE_U8 = 0x01
const TYPE_SZ = 0x21
const TYPE_BLOB = 0x41
const TYPE_BLOB_DATA = 0x42
const TYPE_BLOB_IDX = 0x48

interface Entry {
  ns: number
  type: number
  chunkIndex: number
  key: string
  valueByte: number // data-union byte @24 (primitive U8 value = namespace index)
  data: Uint8Array | null // inline payload for SZ / v1 BLOB / BLOB_DATA
  idxDataSize: number // BLOB_IDX: total reassembled size
  idxChunkStart: number
  idxChunkCount: number
}

export interface NvsWifiProfile {
  ssid: string
  pass: string
  prio: number
}

export interface NvsIdentity {
  serial: string | null // storage/dev_sn envelope → bare serial
  ssid: string | null // picpak/ssid (active provisioned SSID)
  pass: string | null // picpak/pass (cleartext, as stored)
  url: string | null // picpak/url (tokenized device URL)
  wifiProfiles: NvsWifiProfile[] // storage/wifi.* profiles, sorted by prio descending
}

function u16(b: Uint8Array, o: number): number {
  return b[o] | (b[o + 1] << 8)
}
function u32(b: Uint8Array, o: number): number {
  return (b[o] | (b[o + 1] << 8) | (b[o + 2] << 16) | (b[o + 3] << 24)) >>> 0
}
function cstr(b: Uint8Array, o: number, max: number): string {
  let end = o
  const stop = Math.min(o + max, b.length)
  while (end < stop && b[end] !== 0) end++
  let s = ''
  for (let i = o; i < end; i++) s += String.fromCharCode(b[i])
  return s
}

function parsePageEntries(page: Uint8Array, out: Entry[]): void {
  const state = u32(page, 0)
  if (state !== PAGE_ACTIVE && state !== PAGE_FULL) return // skip uninitialized / freeing / corrupt pages
  let i = 0
  while (i < ENTRIES_PER_PAGE) {
    const es = (page[HEADER_SIZE + (i >> 2)] >> ((i & 3) * 2)) & 0x3
    if (es !== STATE_WRITTEN) {
      i += 1 // erased/empty header — data entries of a live item are skipped via `span` below
      continue
    }
    const base = ENTRY_AREA + i * ENTRY_SIZE
    if (base + ENTRY_SIZE > page.length) break
    const type = page[base + 1]
    let span = page[base + 2]
    if (span < 1 || span > ENTRIES_PER_PAGE - i) span = 1 // clamp against corruption / infinite loop
    const entry: Entry = {
      ns: page[base],
      type,
      chunkIndex: page[base + 3],
      key: cstr(page, base + 8, 16),
      valueByte: page[base + 24],
      data: null,
      idxDataSize: 0,
      idxChunkStart: 0,
      idxChunkCount: 0,
    }
    if (type === TYPE_BLOB_IDX) {
      entry.idxDataSize = u32(page, base + 24)
      entry.idxChunkCount = page[base + 28]
      entry.idxChunkStart = page[base + 29]
    } else if (type === TYPE_SZ || type === TYPE_BLOB || type === TYPE_BLOB_DATA) {
      const size = u16(page, base + 24)
      const dataStart = base + ENTRY_SIZE
      const avail = Math.max(0, Math.min(size, page.length - dataStart))
      entry.data = page.subarray(dataStart, dataStart + avail)
    }
    out.push(entry)
    i += span
  }
}

function collectEntries(partition: Uint8Array): Entry[] {
  const entries: Entry[] = []
  for (let off = 0; off + PAGE_SIZE <= partition.length; off += PAGE_SIZE) {
    parsePageEntries(partition.subarray(off, off + PAGE_SIZE), entries)
  }
  return entries
}

function nsIndexOf(entries: Entry[], name: string): number {
  const e = entries.find((x) => x.ns === 0 && x.type === TYPE_U8 && x.key === name)
  return e ? e.valueByte : -1
}

/** Reassemble a blob/string entry's bytes (handles v1 BLOB, v2 BLOB_IDX+BLOB_DATA, and SZ). */
function blobBytes(entries: Entry[], nsIndex: number, head: Entry): Uint8Array | null {
  if (head.type === TYPE_BLOB_IDX) {
    const chunks = entries
      .filter(
        (e) =>
          e.ns === nsIndex &&
          e.key === head.key &&
          e.type === TYPE_BLOB_DATA &&
          e.chunkIndex >= head.idxChunkStart &&
          e.chunkIndex < head.idxChunkStart + head.idxChunkCount,
      )
      .sort((a, b) => a.chunkIndex - b.chunkIndex)
    if (chunks.length === 0) return null
    const total = chunks.reduce((n, c) => n + (c.data ? c.data.length : 0), 0)
    const buf = new Uint8Array(total)
    let p = 0
    for (const c of chunks) {
      if (c.data) {
        buf.set(c.data, p)
        p += c.data.length
      }
    }
    return head.idxDataSize > 0 ? buf.subarray(0, Math.min(head.idxDataSize, buf.length)) : buf
  }
  if (head.type === TYPE_BLOB || head.type === TYPE_SZ) return head.data
  return null
}

function decodeText(bytes: Uint8Array): string {
  let end = bytes.length
  while (end > 0 && bytes[end - 1] === 0) end-- // strip SZ terminator / blob NUL padding
  return new TextDecoder().decode(bytes.subarray(0, end))
}

/** picpak/<key> STRING value, or null. Never used for c2_sk/c2_pk (see SECURITY BOUNDARY). */
function stringValue(entries: Entry[], nsIndex: number, key: string): string | null {
  if (nsIndex < 0) return null
  const e = entries.find((x) => x.ns === nsIndex && x.key === key && x.type === TYPE_SZ)
  if (!e || !e.data) return null
  const v = decodeText(e.data)
  return v.length > 0 ? v : null
}

function envelopeSerial(entries: Entry[], storageNs: number): string | null {
  if (storageNs < 0) return null
  const head = entries.find((e) => e.ns === storageNs && e.key === 'dev_sn' && e.type !== TYPE_BLOB_DATA)
  if (!head) return null
  const bytes = blobBytes(entries, storageNs, head)
  if (!bytes || bytes.length === 0) return null
  try {
    const obj = JSON.parse(decodeText(bytes)) as { serial_number?: unknown }
    return typeof obj.serial_number === 'string' && obj.serial_number.length > 0 ? obj.serial_number : null
  } catch {
    return null
  }
}

function wifiProfiles(entries: Entry[], storageNs: number): NvsWifiProfile[] {
  if (storageNs < 0) return []
  const out: NvsWifiProfile[] = []
  const heads = entries.filter(
    (e) => e.ns === storageNs && e.key.startsWith('wifi.') && e.type !== TYPE_BLOB_DATA,
  )
  for (const head of heads) {
    const bytes = blobBytes(entries, storageNs, head)
    if (!bytes || bytes.length === 0) continue
    try {
      const obj = JSON.parse(decodeText(bytes)) as { ssid?: unknown; pass?: unknown; prio?: unknown }
      if (typeof obj.ssid === 'string' && obj.ssid.length > 0) {
        out.push({
          ssid: obj.ssid,
          pass: typeof obj.pass === 'string' ? obj.pass : '',
          prio: typeof obj.prio === 'number' ? obj.prio : 0,
        })
      }
    } catch {
      /* skip a corrupt profile — best-effort */
    }
  }
  // highest prio first; stable for ties (insertion order preserved by the comparator)
  return out.sort((a, b) => b.prio - a.prio)
}

/**
 * Parse the onboarding-relevant identity out of a raw NVS partition image. Best-effort and total:
 * every field independently degrades to null / [] rather than throwing, so a partial or foreign
 * partition still yields whatever could be read. Deliberately reads ONLY the fields above — never c2_sk.
 */
export function readNvsIdentity(partition: Uint8Array): NvsIdentity {
  const empty: NvsIdentity = { serial: null, ssid: null, pass: null, url: null, wifiProfiles: [] }
  try {
    if (!partition || partition.length < PAGE_SIZE) return empty
    const entries = collectEntries(partition)
    const storageNs = nsIndexOf(entries, 'storage')
    const picpakNs = nsIndexOf(entries, 'picpak')
    return {
      serial: envelopeSerial(entries, storageNs),
      ssid: stringValue(entries, picpakNs, 'ssid'),
      pass: stringValue(entries, picpakNs, 'pass'),
      url: stringValue(entries, picpakNs, 'url'),
      wifiProfiles: wifiProfiles(entries, storageNs),
    }
  } catch {
    return empty
  }
}

/** Bare factory serial from a raw NVS partition, or null. Thin wrapper over {@link readNvsIdentity}. */
export function readFactorySerial(partition: Uint8Array): string | null {
  return readNvsIdentity(partition).serial
}

/**
 * The Wi-Fi credentials to offer for prefill: the active picpak config if present, else the
 * highest-priority stored profile. CHOICE: picpak/ssid+pass is the *currently provisioned* link
 * (config.c reads exactly these at boot), so it beats the multi-profile store; within the store the
 * highest `prio` wins. Returns null when nothing usable was read.
 */
export function selectWifi(id: NvsIdentity): { ssid: string; pass: string } | null {
  if (id.ssid) return { ssid: id.ssid, pass: id.pass ?? '' }
  const top = id.wifiProfiles[0]
  if (top && top.ssid) return { ssid: top.ssid, pass: top.pass }
  return null
}

/** Prefill discipline (A35.3): a detected value fills a field ONLY when the operator left it blank. */
export function prefillField(current: string, detected: string | null): string {
  return current.trim() === '' && detected ? detected : current
}
