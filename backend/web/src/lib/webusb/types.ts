// Web-USB onboarding shared types (Design 26 §4.1). The flash/USB engine is framework-free DOM/Web-API code,
// unit-testable through the SerialLink abstraction below — the line protocol never touches navigator.serial in
// tests, so the load-bearing sequencing (matchers, redaction) is device-free verifiable (F2/F8/F11).

/**
 * A minimal duplex byte link over a serial connection. The Web Serial adapter (flasher.ts `webSerialLink`)
 * wraps a real, open SerialPort into this; ConsoleSession drives only this interface. A fake link drives the
 * protocol in tests with no device.
 */
export interface SerialLink {
  /** Resolve with the next chunk of bytes, or null when the link closes. */
  read(): Promise<Uint8Array | null>
  /** Write bytes to the device. */
  write(bytes: Uint8Array): Promise<void>
}

/**
 * Log sink. The `.svelte` page renders this into its log pane; lib code stays presentation-free. `cls` mirrors
 * the private onboarding classes (web/onboard/app.js): info | ok | err | dim.
 */
export type LogSink = (msg: string, cls?: 'info' | 'ok' | 'err' | 'dim') => void

/**
 * Firmware flash manifest (bin/manifest.json), fetched at runtime — never hardcoded in the SPA (Policy=Data,
 * D26.9). The per-part sha256 is the integrity gate the flasher enforces before writing.
 */
export interface FlashManifest {
  name: string
  version: string
  chip: string
  flashSize: string // e.g. "16MB"
  flashMode?: string // "dio"
  flashFreq?: string // "80m"
  eraseBeforeFlash?: boolean
  parts: FlashPart[]
}

export interface FlashPart {
  path: string
  offset: number
  sha256?: string
}
