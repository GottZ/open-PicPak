// Minimal ambient types for the vendored esptool-js bundle (v0.6.0, Apache-2.0 — see LICENSE/VERSION.txt). The
// bundle is untyped self-contained ESM; these declarations cover only the surface the flasher uses (Design 26
// §4.2, lifted from the private web/onboard/app.js flash paths) so tsc/svelte-check stay green. A version bump
// that changes this surface (or introduces a Worker — re-triggering the D26.6 CSP measurement) updates this file.

export interface Terminal {
  clean(): void
  writeLine(data: string): void
  write(data: string): void
}

export interface LoaderOptions {
  transport: Transport
  baudrate: number
  romBaudrate: number
  terminal?: Terminal
}

export interface FlashOptions {
  fileArray: { data: Uint8Array; address: number }[]
  flashSize: string
  flashMode: string
  flashFreq: string
  eraseAll: boolean
  compress: boolean
  reportProgress?: (fileIndex: number, written: number, total: number) => void
}

export class Transport {
  constructor(port: SerialPort, tracing?: boolean)
  setRTS(state: boolean): Promise<void>
  setDTR(state: boolean): Promise<void>
  disconnect(): Promise<void>
}

export class ESPLoader {
  constructor(options: LoaderOptions)
  /** connect (auto USB-JTAG reset) + load stub; resolves with the detected chip name. */
  main(): Promise<string>
  readFlash(
    offset: number,
    length: number,
    onProgress?: (packet: number, soFar: number) => void,
  ): Promise<Uint8Array>
  writeFlash(options: FlashOptions): Promise<void>
  after(mode: string): Promise<void>
}
