import { describe, it, expect } from 'vitest'
import { manifestFlashBytes, DEFAULT_BAUD, CONSOLE_BAUD, ESP_IMAGE_MAGIC } from './flasher'

// The device-touching flash lifecycle is the W3 on-device gate (G3, real silicon); the pure helpers below are
// the device-free unit surface. manifestFlashBytes drives the backup read length, so an off-by-a-factor here
// would read the wrong span (app.js:230-234).
describe('manifestFlashBytes', () => {
  it('parses NNMBb sizes', () => {
    expect(manifestFlashBytes({ flashSize: '16MB' })).toBe(16 * 1048576)
    expect(manifestFlashBytes({ flashSize: '4MB' })).toBe(4 * 1048576)
  })
  it('defaults to 16 MB on null / unparseable', () => {
    expect(manifestFlashBytes(null)).toBe(16 * 1048576)
    expect(manifestFlashBytes({ flashSize: 'weird' })).toBe(16 * 1048576)
  })
})

describe('flasher constants (locked to the private hardware-verified values)', () => {
  it('keeps the verified baud + image magic', () => {
    expect(DEFAULT_BAUD).toBe(3_000_000)
    expect(CONSOLE_BAUD).toBe(115200)
    expect(ESP_IMAGE_MAGIC).toBe(0xe9)
  })
})
