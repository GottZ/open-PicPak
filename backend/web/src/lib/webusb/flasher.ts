// esptool-js flash engine wrap (Design 26 §4.2 / D26.1), lifted near-verbatim from the private web/onboard/
// app.js flash paths so the behaviour verified on this hardware (timings, chunking, reset pulses, at 3 Mbaud)
// ports byte-for-byte. Framework-free DOM/Web-API code (not Svelte). The device-touching methods
// (connect/backup/flash/restore) are the W3 on-device gate (G3) — the pure helpers below (manifestFlashBytes,
// sha256Hex) and the console line protocol (console.ts) are the device-free unit surface.

import { ESPLoader, Transport, type Terminal } from './vendor/esptool-js/bundle.js'
import type { FlashManifest, LogSink, SerialLink } from './types'
import { readNvsIdentity, type NvsIdentity } from './nvs-read'
import { parseAppDesc, type AppDesc } from './appdesc'

// Baud is overridable by the page. esptool-js hardcodes romBaudrate=115200, so any target rate != 115200
// triggers its changeBaud after the stub loads — stable here (verified 3 Mbaud, 16 MB read in 108 s). USB-JTAG
// readFlash DOES honor the rate despite the docs (115200 ≈ 11 KB/s bottlenecked; 3 Mbaud ≈ 151 KB/s; beyond
// ~3M the readFlash protocol overhead dominates).
export const DEFAULT_BAUD = 3_000_000
export const CONSOLE_BAUD = 115200 // firmware setup console (FW console is 115200; separate port.open)
export const BACKUP_CHUNK = 256 * 1024 // read backup in chunks: esptool-js readFlash appends O(n^2)
export const ESP_IMAGE_MAGIC = 0xe9
export const ESPRESSIF_USB_VID = 0x303a // public Espressif USB VID (reuse an already-authorized C3 port)

// W-A26.10 flash-read identity: the stock/CFW NVS partition (0x9000, 0x6000) and the app-partition
// head (0x20000, first 512 B — enough for the 176 B esp_app_desc_t). Both are pure reads; flashing
// is untouched. Offsets match firmware/partitions.csv and onboard-fw/manifest.json.
export const NVS_OFFSET = 0x9000
export const NVS_SIZE = 0x6000
export const APP_OFFSET = 0x20000
export const APP_HEAD_LEN = 512

export type ProgressSink = (pct: number) => void

/**
 * What a post-connect flash read learned about the device. Fail-open: each half is independently
 * null when its read failed, so a stock/blank/foreign device still connects and flashes normally.
 */
export interface FlashIdent {
  nvs: NvsIdentity | null // factory serial + provisioned Wi-Fi/URL (never the c2_sk private key — see nvs-read)
  firmware: AppDesc | null // installed esp_app_desc_t, or null (unreadable / bad magic)
}

/** SHA-256 as lowercase hex (Web Crypto). Used for the per-part integrity gate before writing. */
export async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', bytes as unknown as BufferSource)
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('')
}

/** Flash byte-size from a manifest flashSize like "16MB" (default 16 MB). Pure — the device-free unit gate. */
export function manifestFlashBytes(manifest: Pick<FlashManifest, 'flashSize'> | null): number {
  const s = (manifest && manifest.flashSize) || '16MB'
  const m = /^(\d+)MB$/.exec(s)
  return m ? parseInt(m[1], 10) * 1048576 : 16 * 1048576
}

/**
 * USB-Serial-JTAG hard reset (reboot into the app). esptool-js's after("hard_reset") only drives RTS low and
 * never pulses it high→low, so it doesn't reboot the C3 over USB-JTAG. This mirrors esptool's HardReset
 * (uses_usb=True): RTS maps to EN, ~200 ms timing, a DTR write after each RTS write so SET_CONTROL_LINE_STATE
 * carries the updated RTS state (the Windows-driver work-around). ON-DEVICE (W3).
 */
export async function usbJtagHardReset(transport: Transport, log: LogSink = () => {}): Promise<void> {
  const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
  try {
    await transport.setRTS(true)
    await transport.setDTR(false) // EN low → chip in reset
    await sleep(200)
    await transport.setRTS(false)
    await transport.setDTR(false) // EN high → chip runs
    await sleep(200)
    log('Reset pulse sent (USB-JTAG).', 'dim')
  } catch (e) {
    log('Auto-reset incomplete (' + (e as Error).message + ') — unplug/replug if the screen stays blank.', 'dim')
  }
}

/**
 * Adapt an already-open Web Serial port into the SerialLink ConsoleSession consumes (opened at CONSOLE_BAUD by
 * the page after the flash transport is released). Holds the reader/writer for the session; close() releases
 * them so the port can be re-opened.
 */
export function webSerialLink(port: SerialPort): SerialLink & { close(): Promise<void> } {
  let reader: ReadableStreamDefaultReader<Uint8Array> | null = port.readable ? port.readable.getReader() : null
  let writer: WritableStreamDefaultWriter<Uint8Array> | null = port.writable ? port.writable.getWriter() : null
  return {
    async read() {
      if (!reader) return null
      const { value, done } = await reader.read()
      return done ? null : (value ?? null)
    },
    async write(bytes) {
      if (!writer) throw new Error('serial port not writable')
      await writer.write(bytes)
    },
    async close() {
      try {
        if (reader) {
          await reader.cancel()
          reader.releaseLock()
        }
      } catch {
        /* already released */
      }
      try {
        if (writer) writer.releaseLock()
      } catch {
        /* already released */
      }
      reader = null
      writer = null
    },
  }
}

const espTerminal = (log: LogSink): Terminal => ({
  clean() {},
  writeLine(data: string) {
    log(data, 'dim')
  },
  write() {
    /* partial-line spam; keep the log readable */
  },
})

/**
 * Drives the esptool-js flash lifecycle over one authorized Web Serial port. All methods here are the W3
 * on-device gate (G3): they require real silicon. The page injects the port (from a user-gesture
 * requestPort()) and the firmware artifact fetches, keeping this module navigator- and fetch-free.
 */
export class Flasher {
  private transport: Transport | null = null
  private esploader: ESPLoader | null = null

  constructor(
    private readonly port: SerialPort,
    private readonly log: LogSink = () => {},
    private readonly baud: number = DEFAULT_BAUD,
  ) {}

  get ready(): boolean {
    return this.esploader !== null
  }

  /**
   * Connect + auto USB-JTAG reset + load the stub, retrying for ~40 s over the brownout refresh-cycle USB-pad
   * detach (a provisioned device drops its USB pad ~30 s each refresh — app.js:124-137). Resolves with the
   * detected chip name. Tip surfaced to the operator: a triple-press forces setup mode, where no cycle runs.
   */
  async connect(): Promise<string> {
    let chip: string | null = null
    let lastErr: unknown = null
    for (let attempt = 1; attempt <= 30 && !chip; attempt++) {
      try {
        this.transport = new Transport(this.port, false)
        this.esploader = new ESPLoader({
          transport: this.transport,
          baudrate: this.baud,
          romBaudrate: this.baud, // ignored by esptool-js (romBaudrate is hardcoded 115200)
          terminal: espTerminal(this.log),
        })
        chip = await this.esploader.main()
      } catch (e) {
        lastErr = e
        try {
          if (this.transport) await this.transport.disconnect()
        } catch {
          /* transport already gone */
        }
        this.transport = null
        this.esploader = null
        if (attempt === 1) {
          this.log(
            'Device busy (mid refresh cycle?) — retrying for ~40s. Tip: triple-press the button to force setup mode.',
            'dim',
          )
        }
        await new Promise((r) => setTimeout(r, 2000))
      }
    }
    if (!chip) throw (lastErr as Error) ?? new Error('could not connect')
    return chip
  }

  /**
   * Read the device's on-flash identity after connect: the NVS partition (factory serial + the
   * provisioned Wi-Fi/URL config) and the app-partition head (installed esp_app_desc_t). ON-DEVICE
   * (W3) — the read itself needs real silicon; the parsers (nvs-read / appdesc) are the device-free
   * unit surface. Fail-open by design: a failed read of either region yields a null half and a dim
   * log line, never an exception — identity read must never break the connect/flash flow. The 24 KB
   * NVS read runs at DEFAULT_BAUD (~151 KB/s at 3 Mbaud ⇒ well under a second).
   */
  async readIdent(): Promise<FlashIdent> {
    if (!this.esploader) throw new Error('not connected')
    let nvs: NvsIdentity | null = null
    let firmware: AppDesc | null = null
    try {
      const raw = await this.esploader.readFlash(NVS_OFFSET, NVS_SIZE, () => {})
      nvs = readNvsIdentity(raw)
    } catch (e) {
      this.log('NVS read failed (' + (e as Error).message + ') — factory serial/Wi-Fi unknown.', 'dim')
    }
    try {
      const head = await this.esploader.readFlash(APP_OFFSET, APP_HEAD_LEN, () => {})
      firmware = parseAppDesc(head)
    } catch (e) {
      this.log('App-descriptor read failed (' + (e as Error).message + ') — installed firmware unknown.', 'dim')
    }
    return { nvs, firmware }
  }

  /**
   * Full-flash backup → the recovery `.bin`. Read in chunks and assemble once (esptool-js readFlash grows its
   * result via O(n²) append; a single 16 MB call copies gigabytes — app.js:176-206).
   */
  async backup(flashBytes: number, onProgress: ProgressSink = () => {}): Promise<Uint8Array> {
    if (!this.esploader) throw new Error('not connected')
    const parts: Uint8Array[] = []
    let done = 0
    for (let off = 0; off < flashBytes; off += BACKUP_CHUNK) {
      const len = Math.min(BACKUP_CHUNK, flashBytes - off)
      const part = await this.esploader.readFlash(off, len, (_pkt, soFar) =>
        onProgress(((done + soFar) / flashBytes) * 100),
      )
      parts.push(part)
      done += len
    }
    const data = new Uint8Array(flashBytes)
    let pos = 0
    for (const c of parts) {
      const n = Math.min(c.length, flashBytes - pos)
      data.set(c.subarray(0, n), pos)
      pos += n
    }
    onProgress(100)
    return data
  }

  /**
   * Manifest-driven multi-part flash: SHA-256-verify every part against the manifest before writing, then
   * writeFlash + USB-JTAG hard-reset into the CFW (app.js:236-294). `fetchPart` fetches an artifact by its
   * manifest path (the page wires it to the same-origin /onboard-fw/ mount — D26.9); the manifest is fetched
   * at runtime, never hardcoded (Policy=Data).
   */
  async flash(
    manifest: FlashManifest,
    fetchPart: (path: string) => Promise<Uint8Array>,
    onProgress: ProgressSink = () => {},
  ): Promise<void> {
    if (!this.esploader) throw new Error('not connected')
    const fileArray: { data: Uint8Array; address: number }[] = []
    const sizes: number[] = []
    for (const p of manifest.parts) {
      const buf = await fetchPart(p.path)
      if (p.sha256) {
        const got = await sha256Hex(buf)
        if (got !== p.sha256.toLowerCase()) throw new Error(`${p.path}: SHA-256 mismatch (corrupt download?)`)
      }
      fileArray.push({ data: buf, address: p.offset })
      sizes.push(buf.length)
      this.log(`loaded ${p.path} (${buf.length} B)`, 'dim')
    }
    const totalBytes = sizes.reduce((a, b) => a + b, 0)
    const before = (i: number) => sizes.slice(0, i).reduce((a, b) => a + b, 0)

    this.log('Flashing — do not unplug...', 'info')
    await this.esploader.writeFlash({
      fileArray,
      flashSize: manifest.flashSize || '16MB',
      flashMode: manifest.flashMode || 'dio',
      flashFreq: manifest.flashFreq || '80m',
      eraseAll: !!manifest.eraseBeforeFlash,
      compress: true,
      reportProgress: (i, written) => onProgress(((before(i) + written) / totalBytes) * 100),
    })
    onProgress(100)
    if (this.transport) await usbJtagHardReset(this.transport, this.log)
    await this.disconnect()
  }

  /**
   * Restore a saved backup `.bin` at 0x0 (recovery). Keep the original flash header — do NOT patch
   * size/mode/freq (app.js:434-466). Warns (does not block) if the image magic is missing.
   */
  async restore(data: Uint8Array, onProgress: ProgressSink = () => {}): Promise<void> {
    if (!this.esploader) throw new Error('not connected')
    if (data[0] !== ESP_IMAGE_MAGIC) {
      this.log('Warning: file does not start with the ESP image magic (0xE9) — flashing anyway.', 'err')
    }
    await this.esploader.writeFlash({
      fileArray: [{ data, address: 0 }],
      flashSize: 'keep',
      flashMode: 'keep',
      flashFreq: 'keep',
      eraseAll: false,
      compress: true,
      reportProgress: (_i, written, total) => onProgress((written / total) * 100),
    })
    onProgress(100)
    await this.esploader.after('hard_reset')
    await this.disconnect()
  }

  /** Release the esptool transport so the port can be re-opened at CONSOLE_BAUD for provisioning. */
  async disconnect(): Promise<void> {
    try {
      if (this.transport) await this.transport.disconnect()
    } catch {
      /* transport already gone */
    }
    this.transport = null
    this.esploader = null
  }
}
