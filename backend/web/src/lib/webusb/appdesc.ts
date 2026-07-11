// esp_app_desc_t reader (W-A26.10) — extracts project_name / version / idf_ver of the app image
// already installed in flash, so /onboard can tell Stock from CFW and flag an available update
// WITHOUT any firmware cooperation (a pure flash read; works on Stock and CFW alike).
//
// Layout, verified empirically against the real artifact /compose/picpak/onboard-fw/picpak_fw.bin
// (first 176 bytes). The descriptor follows the image + first-segment headers:
//   esp_image_header_t (24 B) + esp_image_segment_header_t (8 B) = 32 B, then esp_app_desc_t:
//     @0   magic_word   u32 LE  = 0xABCD5432
//     @4   secure_version u32
//     @8   reserv1[2]    (8 B)
//     @16  version[32]        → "0.6.2"
//     @48  project_name[32]   → "picpak_fw"
//     @80  time[16]           → "13:38:02"
//     @96  date[16]           → "Jun 28 2026"
//     @112 idf_ver[32]        → "v5.5.3"
// (All fields NUL-terminated. Descriptor ends at 32+144 = 176 B, so a 512 B flash read of the app
//  partition head is ample.) Wrong magic ⇒ null.

const IMAGE_HEADER = 24
const SEGMENT_HEADER = 8
const DESC = IMAGE_HEADER + SEGMENT_HEADER // 32: esp_app_desc_t start within the app image
const DESC_END = DESC + 144 // through the end of idf_ver[32]

export const APP_DESC_MAGIC = 0xabcd5432

// Empirical: esp_app_desc_t.project_name of the CFW build artifact. Pins Stock-vs-CFW detection.
export const CFW_PROJECT_NAME = 'picpak_fw'

export interface AppDesc {
  projectName: string
  version: string
  idfVer: string
  date: string
  time: string
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

/** Parse esp_app_desc_t from the head of an app-partition read. Wrong magic / short input ⇒ null. */
export function parseAppDesc(head: Uint8Array): AppDesc | null {
  if (!head || head.length < DESC_END) return null
  if (u32(head, DESC) !== APP_DESC_MAGIC) return null
  return {
    version: cstr(head, DESC + 16, 32),
    projectName: cstr(head, DESC + 48, 32),
    time: cstr(head, DESC + 80, 16),
    date: cstr(head, DESC + 96, 16),
    idfVer: cstr(head, DESC + 112, 32),
  }
}

/** Compare dotted numeric versions (leading 'v' tolerated). -1 / 0 / 1. Non-numeric parts → 0. */
export function semverCompare(a: string, b: string): number {
  const pa = parseVer(a)
  const pb = parseVer(b)
  for (let i = 0; i < 3; i++) {
    if (pa[i] !== pb[i]) return pa[i] < pb[i] ? -1 : 1
  }
  return 0
}
function parseVer(v: string): [number, number, number] {
  const parts = v
    .replace(/^[vV]/, '')
    .split(/[.\-+]/)
    .map((p) => {
      const n = parseInt(p, 10)
      return Number.isFinite(n) ? n : 0
    })
  return [parts[0] ?? 0, parts[1] ?? 0, parts[2] ?? 0]
}

export type FirmwareStatus =
  | { kind: 'unknown' } // no descriptor could be read
  | { kind: 'stock'; projectName: string; version: string } // project_name ≠ CFW
  | { kind: 'update'; projectName: string; version: string; to: string } // CFW, installed < manifest
  | { kind: 'current'; projectName: string; version: string } // CFW, installed ≥ manifest

/**
 * Three-way verdict of an installed descriptor against the flash manifest version:
 * (i) foreign project_name ⇒ stock; (ii) CFW but older than the manifest ⇒ update; (iii) else current.
 */
export function compareFirmware(
  desc: AppDesc | null,
  manifestVersion: string | null,
  cfwName: string = CFW_PROJECT_NAME,
): FirmwareStatus {
  if (!desc) return { kind: 'unknown' }
  if (desc.projectName !== cfwName) return { kind: 'stock', projectName: desc.projectName, version: desc.version }
  if (manifestVersion && semverCompare(desc.version, manifestVersion) < 0) {
    return { kind: 'update', projectName: desc.projectName, version: desc.version, to: manifestVersion }
  }
  return { kind: 'current', projectName: desc.projectName, version: desc.version }
}
