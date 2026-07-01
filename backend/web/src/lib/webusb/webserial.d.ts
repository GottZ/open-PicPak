// Minimal Web Serial API ambient types — NOT in TS lib.dom, and we take no @types/w3c-web-serial dependency
// (Design 26 §7: no new frontend dep). Scoped to exactly the surface onboarding uses. Chromium-desktop only;
// the page feature-gates on `'serial' in navigator` (Design 26 §4.2 / F7). ReadableStream/WritableStream are
// already in lib.dom.

interface SerialPortInfo {
  usbVendorId?: number
  usbProductId?: number
}

interface SerialPort {
  open(options: { baudRate: number }): Promise<void>
  close(): Promise<void>
  readonly readable: ReadableStream<Uint8Array> | null
  readonly writable: WritableStream<Uint8Array> | null
  getInfo(): SerialPortInfo
  setSignals(signals: { requestToSend?: boolean; dataTerminalReady?: boolean }): Promise<void>
}

interface Serial extends EventTarget {
  requestPort(options?: unknown): Promise<SerialPort>
  getPorts(): Promise<SerialPort[]>
}

interface Navigator {
  readonly serial: Serial
}
