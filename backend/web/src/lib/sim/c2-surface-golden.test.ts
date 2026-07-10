/// <reference types="node" />
// Firmware golden for the C2 stub surface (lead finding #10). SURFACE_SIZE === 36 alone is a
// self-golden — a constant checked against a number of the same authorship. This test mechanises the
// one-time manual verification: it extracts the registered function names from the REAL firmware
// registration sites (firmware/main/{cmd,store,dev}.c be_regfunc lines — same repo-relative read the
// examples.test.ts drift-guard uses) and compares BIDIRECTIONALLY against the TS surface:
//   (1) firmware registers X, TS surface lacks X  → red (the sim would run valid code into "undefined")
//   (2) TS surface has Y with no firmware source  → red (a phantom stub faking a capability)
// Plus an extraction negative probe (pattern: playlist_types_golden TestPlaylistTypesParsers_Extract):
// the regex parser must demonstrably yield >0 names and a known member per file — otherwise a silent
// regex/path rot would make the golden vacuously green.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, it, expect } from 'vitest'
import { SURFACE } from './c2-surface'

// firmware/main/*.c relative to this test file (backend/web/src/lib/sim/ → repo firmware/main/).
const fwPath = (name: string) =>
  fileURLToPath(new URL(`../../../../../firmware/main/${name}`, import.meta.url))

// The truth is the C source: `be_regfunc(vm, "name", l_handler);` (whitespace-padded in cmd.c).
const REGFUNC_RE = /be_regfunc\(\s*vm\s*,\s*"([A-Za-z0-9_]+)"/g

function registeredNames(file: string): string[] {
  const src = readFileSync(fwPath(file), 'utf8')
  return [...src.matchAll(REGFUNC_RE)].map((m) => m[1])
}

// The three C files backing berry_c2's registration (cmd 13 + store.c 10 [store 6 + rtc 4] + dev 13).
const FW_FILES: { file: string; knownMember: string }[] = [
  { file: 'cmd.c', knownMember: 'nvs_set' },
  { file: 'store.c', knownMember: 'store_set' },
  { file: 'dev.c', knownMember: 'dev_batt_pct' },
]

describe('C2 surface ↔ firmware be_regfunc golden', () => {
  // Negative probe FIRST: prove the extractor extracts. Without this, a moved file or a drifted
  // registration syntax would empty the firmware set and the bidirectional diff could pass vacuously.
  it('extracts >0 names and a known member from every firmware file (extractor not vacuous)', () => {
    for (const { file, knownMember } of FW_FILES) {
      const names = registeredNames(file)
      expect(names.length, `${file}: regex extracted no be_regfunc names — syntax/path drift?`).toBeGreaterThan(0)
      expect(names, `${file}: expected known member ${knownMember}`).toContain(knownMember)
    }
  })

  it('every firmware-registered function has a TS stub, and no TS stub is a phantom', () => {
    const firmware = new Set(FW_FILES.flatMap(({ file }) => registeredNames(file)))
    const ts = new Set(Object.keys(SURFACE))

    const missingInTs = [...firmware].filter((n) => !ts.has(n)).sort()
    const phantomInTs = [...ts].filter((n) => !firmware.has(n)).sort()

    expect(
      missingInTs,
      `firmware registers ${missingInTs.join(', ')} but the TS surface lacks them — valid device code would hit "undefined" in the sim`,
    ).toEqual([])
    expect(
      phantomInTs,
      `TS surface stubs ${phantomInTs.join(', ')} with no firmware be_regfunc source — phantom capability`,
    ).toEqual([])
  })

  it('the surfaces agree on the exact count (36 today — but the count follows the firmware, not a constant)', () => {
    const firmware = new Set(FW_FILES.flatMap(({ file }) => registeredNames(file)))
    expect(Object.keys(SURFACE).length).toBe(firmware.size)
  })
})
