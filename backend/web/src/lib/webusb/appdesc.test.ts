import { describe, it, expect } from 'vitest'
import { parseAppDesc, compareFirmware, semverCompare, CFW_PROJECT_NAME } from './appdesc'

// Fixture = the first 176 bytes of the REAL artifact /compose/picpak/onboard-fw/picpak_fw.bin
// (image header 24 B + segment header 8 B + esp_app_desc_t through idf_ver). Embedded as hex so no
// .bin is committed. The empirical parse below (project_name "picpak_fw", version "0.6.2",
// idf_ver "v5.5.3", built Jun 28 2026) simultaneously pins the descriptor offsets AND the CFW-name
// constant the three-way firmware comparison keys on.
const REAL_HEAD_HEX =
  'e907024fe8023840ee0000000500030300c70000000000012000103c54d704003254cdab000000000000000000000000302e362e3200000000000000000000000000000000000000000000000000000070696370616b5f6677000000000000000000000000000000000000000000000031333a33383a303200000000000000004a756e2032382032303236000000000076352e352e330000000000000000000000000000000000000000000000000000'

function hex(s: string): Uint8Array {
  const out = new Uint8Array(s.length / 2)
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.slice(i * 2, i * 2 + 2), 16)
  return out
}

describe('parseAppDesc (verified against the real picpak_fw.bin)', () => {
  it('reads project_name / version / idf_ver / date / time from the real artifact head', () => {
    const desc = parseAppDesc(hex(REAL_HEAD_HEX))
    expect(desc).toEqual({
      projectName: 'picpak_fw',
      version: '0.6.2',
      idfVer: 'v5.5.3',
      date: 'Jun 28 2026',
      time: '13:38:02',
    })
    // the artifact's project_name is exactly the CFW-name the comparison keys on
    expect(desc?.projectName).toBe(CFW_PROJECT_NAME)
  })

  it('returns null on a wrong esp_app_desc_t magic', () => {
    const bad = hex(REAL_HEAD_HEX)
    bad[32] = 0x00 // corrupt the magic word (@ image+segment header = offset 32)
    expect(parseAppDesc(bad)).toBeNull()
  })

  it('returns null on a short read', () => {
    expect(parseAppDesc(hex(REAL_HEAD_HEX).subarray(0, 100))).toBeNull()
  })
})

describe('semverCompare', () => {
  it('orders dotted numeric versions and tolerates a leading v', () => {
    expect(semverCompare('0.6.1', '0.6.2')).toBe(-1)
    expect(semverCompare('0.6.2', '0.6.2')).toBe(0)
    expect(semverCompare('1.0.0', '0.9.9')).toBe(1)
    expect(semverCompare('v5.5.3', '5.5.3')).toBe(0)
  })
})

describe('compareFirmware (three-way verdict against the manifest)', () => {
  const cfw = { projectName: 'picpak_fw', version: '0.6.1', idfVer: 'v5.5.3', date: '', time: '' }
  it('flags a foreign project_name as stock firmware', () => {
    const stock = { ...cfw, projectName: 'esp-idf' }
    expect(compareFirmware(stock, '0.6.2')).toEqual({ kind: 'stock', projectName: 'esp-idf', version: '0.6.1' })
  })
  it('flags an older CFW install as an available update', () => {
    expect(compareFirmware(cfw, '0.6.2')).toEqual({
      kind: 'update',
      projectName: 'picpak_fw',
      version: '0.6.1',
      to: '0.6.2',
    })
  })
  it('reports a current CFW install (installed ≥ manifest)', () => {
    expect(compareFirmware({ ...cfw, version: '0.6.2' }, '0.6.2')).toEqual({
      kind: 'current',
      projectName: 'picpak_fw',
      version: '0.6.2',
    })
  })
  it('reports unknown when no descriptor was read', () => {
    expect(compareFirmware(null, '0.6.2')).toEqual({ kind: 'unknown' })
  })
})
