<script lang="ts">
  // Web-USB onboarding (Design 26): flash the open-picpak CFW + provision Wi-Fi/URL + C2-enroll, all in the
  // operator's browser over WebSerial. The USB/console engine is the framework-free lib/webusb modules (W1);
  // this component is the step UI + flow state (D26.1). Device behaviour (BOND emits a valid key, bonds e2e)
  // is the W3 on-device gate (G1-G3) — not exercisable headless.
  import { apiFetch, toApiError } from '../../lib/api'
  import { session } from '../../lib/auth.svelte'
  import { notify } from '../../lib/toasts.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import {
    Flasher,
    manifestFlashBytes,
    webSerialLink,
    CONSOLE_BAUD,
    ESPRESSIF_USB_VID,
    ESP_IMAGE_MAGIC,
  } from '../../lib/webusb/flasher'
  import { ConsoleSession } from '../../lib/webusb/console'
  import { runProvision } from '../../lib/webusb/provision'
  import { validateProvisionFields, hasErrors, type ProvisionFields } from '../../lib/webusb/validate'
  import { buildEnrollBody, isEnrollSuccess, canEnroll, enrollNeedsConfirm } from '../../lib/webusb/enroll'
  import { webSerialReady } from '../../lib/webusb/support'
  import type { FlashManifest, LogSink } from '../../lib/webusb/types'
  import type { Device } from '../../lib/api/types'

  // --- environment gate (F7): WebSerial is Chromium-desktop + secure-context only ------------------------
  const serialReady = webSerialReady(
    typeof navigator !== 'undefined' ? navigator : null,
    typeof window !== 'undefined' ? window.isSecureContext : undefined,
  )

  // --- log pane (RX credential redaction happens inside ConsoleSession, D26.10) --------------------------
  let logLines = $state<{ msg: string; cls: string }[]>([])
  const log: LogSink = (msg, cls = 'dim') => {
    logLines = [...logLines, { msg, cls }]
  }

  // --- device handles + flow flags -----------------------------------------------------------------------
  let port: SerialPort | null = null
  let flasher: Flasher | null = null
  let connected = $state(false)
  let chip = $state('')
  let busy = $state(false)
  let flashProgress = $state(0)
  let backupProgress = $state(0)
  let eraseNvs = $state(false) // G7: full chip-erase wipes NVS (Wi-Fi/dev_sn/identity keypair) on reflash
  let flashed = $state(false)
  let provisioned = $state(false)
  let pubkeyHex = $state('')
  let enrolled = $state(false)
  let bondState = $state<'idle' | 'waiting' | 'bonded'>('idle')

  // --- provision fields (Policy=Data: everything operator-entered, nothing hardcoded — D26.11 validated) -
  const fields = $state<ProvisionFields>({
    serial: '',
    ssid: '',
    password: '',
    frameUrl: '',
    c2Url: '',
    c2PeriodSeconds: '',
    wakeSeconds: '',
  })
  let label = $state('') // COH34: sent at enroll only if set
  let channel = $state('') // COH34: omit-to-preserve unless the operator picks one

  const fieldErrors = $derived(validateProvisionFields(fields))
  const provisionValid = $derived(!hasErrors(fieldErrors))
  const affordance = $derived(mutationAffordance(session.is_admin))
  const enrollAllowed = $derived(canEnroll(session.is_admin, pubkeyHex !== ''))

  // --- helpers -------------------------------------------------------------------------------------------
  const authHeaders = (): Record<string, string> =>
    session.key ? { Authorization: `Bearer ${session.key}` } : {}

  /** Fetch a firmware artifact by manifest path from the same-origin /onboard-fw/ mount (D26.9). Binary, so
   *  a raw fetch (not apiFetch, which is JSON-only) with the session bearer. */
  async function fetchPart(path: string): Promise<Uint8Array> {
    const res = await fetch('/onboard-fw/' + path, { headers: authHeaders(), cache: 'no-store' })
    if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`)
    return new Uint8Array(await res.arrayBuffer())
  }

  async function loadManifest(): Promise<FlashManifest> {
    return apiFetch<FlashManifest>('/onboard-fw/manifest.json')
  }

  async function findPicpakPort(): Promise<SerialPort | null> {
    const ports = await navigator.serial.getPorts()
    return ports.find((p) => p.getInfo().usbVendorId === ESPRESSIF_USB_VID) ?? null
  }

  function download(filename: string, data: Uint8Array): void {
    // Cast: TS types BlobPart as ArrayBuffer-backed, but a Uint8Array is ArrayBufferLike-backed. Safe at
    // runtime (the backup buffer is a plain ArrayBuffer).
    const blob = new Blob([data as BlobPart], { type: 'application/octet-stream' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = filename
    a.click()
    setTimeout(() => URL.revokeObjectURL(a.href), 4000)
  }

  // --- steps ---------------------------------------------------------------------------------------------
  async function connect() {
    if (busy) return
    busy = true
    try {
      port = await navigator.serial.requestPort() // user gesture (secure context) — lifted app.js
      flasher = new Flasher(port, log)
      log('Connecting (auto USB-JTAG reset; ~40s retry over the refresh cycle)…', 'info')
      chip = await flasher.connect()
      connected = true
      log(`Connected: ${chip}`, 'ok')
    } catch (e) {
      log('Connect failed: ' + (e as Error).message, 'err')
      notify.warn('Connect failed — see the log. Tip: triple-press the button to force setup mode.')
    } finally {
      busy = false
    }
  }

  async function backup() {
    if (!flasher || busy) return
    busy = true
    backupProgress = 0
    try {
      const m = await loadManifest().catch(() => null)
      const bytes = manifestFlashBytes(m)
      log(`Reading ${(bytes / 1048576).toFixed(0)} MB from flash…`, 'info')
      const data = await flasher.backup(bytes, (p) => (backupProgress = p))
      const stamp = new Date().toISOString().slice(0, 19).replace(/[-:T]/g, '')
      download(`picpak-backup-${stamp}.bin`, data)
      log(`Backup saved (${data.length} bytes).`, 'ok')
    } catch (e) {
      log('Backup failed: ' + (e as Error).message, 'err')
    } finally {
      busy = false
    }
  }

  async function flash() {
    if (!flasher || busy) return
    busy = true
    flashProgress = 0
    try {
      const m = await loadManifest()
      // G7: reflash preserves NVS (Wi-Fi/dev_sn/identity keypair) unless the operator opts into a full erase.
      // A "fresh flash" that keeps a stale keypair would re-enroll an old identity believing it is new.
      const manifest: FlashManifest = { ...m, eraseBeforeFlash: eraseNvs || !!m.eraseBeforeFlash }
      log(`Flashing ${m.name} ${m.version}${eraseNvs ? ' (full erase — NVS/identity wiped)' : ''}…`, 'info')
      await flasher.flash(manifest, fetchPart, (p) => (flashProgress = p))
      flashed = true
      connected = false // esptool transport released; provision re-opens at console baud
      log('Flash complete. Rebooting into the CFW…', 'ok')
      notify.success('Firmware flashed. Provision the device next.')
    } catch (e) {
      const err = toApiError(e)
      if (err.status === 404) {
        notify.warn('Firmware artifacts are not configured on this server (ADMIN_ONBOARD_FW_DIR unset).')
      }
      log('Flash failed: ' + (e as Error).message, 'err')
    } finally {
      busy = false
    }
  }

  async function provision() {
    if (busy || !provisionValid) return
    busy = true
    let link: ReturnType<typeof webSerialLink> | null = null
    try {
      if (!port) port = await findPicpakPort()
      if (!port) port = await navigator.serial.requestPort()
      try {
        await port.open({ baudRate: CONSOLE_BAUD })
      } catch {
        // stale handle after the reset's USB re-enumeration → re-fetch the authorized port
        port = (await findPicpakPort()) ?? (await navigator.serial.requestPort())
        await port.open({ baudRate: CONSOLE_BAUD })
      }
      link = webSerialLink(port)
      const cs = new ConsoleSession(link, log)
      cs.start()
      log('Provisioning over the setup console…', 'info')
      const res = await runProvision(cs, fields, { waitForBanner: true })
      if (!res.ok) {
        notify.warn('Provisioning: ' + res.reason)
        log('Provisioning stopped: ' + res.reason, 'err')
        return
      }
      pubkeyHex = res.pubkeyHex
      provisioned = true
      log('Provisioned; device emitted its public key.', 'ok')
      notify.success('Provisioned. Enroll to register the device.')
    } catch (e) {
      log('Provisioning failed: ' + (e as Error).message, 'err')
      notify.warn('Provisioning failed — see the log.')
    } finally {
      if (link) await link.close()
      try {
        await port?.close()
      } catch {
        /* already closed */
      }
      busy = false
    }
  }

  async function enroll() {
    if (!enrollAllowed || busy) return
    busy = true
    try {
      // G9: a serial already registered to a (possibly different) device must not be silently re-keyed.
      let existing: Device | null = null
      try {
        const r = await apiFetch<{ device: Device }>(`/api/devices/${encodeURIComponent(fields.serial)}`)
        existing = r.device
      } catch (e) {
        if (toApiError(e).status !== 404) throw e // 404 = a fresh serial (expected)
      }
      if (
        enrollNeedsConfirm(existing) &&
        !confirm(
          `Serial "${fields.serial}" is already registered${existing?.bonded ? ' and bonded' : ''}. ` +
            `Re-enroll will update it (or re-bond it if the key differs). Continue?`,
        )
      ) {
        return
      }
      const body = buildEnrollBody({
        serial: fields.serial,
        pubkeyHex,
        label: label || undefined,
        channel: channel || undefined,
      })
      const res = await apiFetch<{ action: string }>('/api/devices', {
        method: 'POST',
        body: JSON.stringify(body),
      })
      if (!isEnrollSuccess(res.action)) {
        notify.warn('Enroll returned an unexpected action: ' + res.action)
        return
      }
      enrolled = true
      notify.success(`Enrolled (${res.action}). Waiting for the first bond…`)
      void pollBond()
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      busy = false
    }
  }

  // G30: "Done" reflects the e2e bond (device_auth.session_bootstrapped via GET /api/devices/{serial}), NOT
  // just the POST — a mis-provisioned unit registers a pubkey instantly but never bonds. Latency = C2 period.
  async function pollBond() {
    bondState = 'waiting'
    for (let i = 0; i < 30 && bondState === 'waiting'; i++) {
      await new Promise((r) => setTimeout(r, 6000))
      try {
        const r = await apiFetch<{ device: Device }>(`/api/devices/${encodeURIComponent(fields.serial)}`)
        if (r.device.bonded) {
          bondState = 'bonded'
          notify.success('Device bonded end-to-end ✓')
          return
        }
      } catch {
        /* keep polling — the device bonds on its next C2 poll */
      }
    }
  }
</script>

<section class="onboard">
  <h1>Onboard a device</h1>
  <p class="lede">
    Flash the open firmware and provision Wi-Fi + C2 enrollment over USB, entirely in your browser. Nothing
    but the final public-key registration ever leaves this page.
  </p>

  {#if !serialReady}
    <div class="banner err" role="alert">
      WebSerial is unavailable here. Use a Chromium-based desktop browser over HTTPS (or localhost). On Windows
      the C3's native USB may need its serial driver assigned before it appears in the port picker.
    </div>
  {/if}

  <ol class="steps" class:disabled={!serialReady}>
    <li class="step" class:done={connected}>
      <h2>1 · Connect</h2>
      <button onclick={connect} disabled={!serialReady || busy || connected}>Connect device</button>
      {#if chip}<span class="ok">Connected: {chip}</span>{/if}
    </li>

    <li class="step" class:done={backupProgress >= 100}>
      <h2>2 · Backup <small>(recommended)</small></h2>
      <button onclick={backup} disabled={!connected || busy}>Full-flash backup → .bin</button>
      {#if backupProgress > 0}<progress max="100" value={backupProgress}></progress>{/if}
    </li>

    <li class="step" class:done={flashed}>
      <h2>3 · Flash</h2>
      <label class="chk">
        <input type="checkbox" bind:checked={eraseNvs} disabled={busy} />
        Full chip-erase (wipe Wi-Fi / dev_sn / identity keypair — a truly fresh device)
      </label>
      <button onclick={flash} disabled={!connected || busy}>Flash open-picpak CFW</button>
      {#if flashProgress > 0}<progress max="100" value={flashProgress}></progress>{/if}
    </li>

    <li class="step" class:done={provisioned}>
      <h2>4 · Provision</h2>
      <p class="hint">The device is in its setup console after a flash (or triple-press it to re-provision without reflashing).</p>
      <div class="grid">
        <label>Serial<input bind:value={fields.serial} placeholder="PP-001" /></label>
        {#if fieldErrors.serial}<span class="fielderr">{fieldErrors.serial}</span>{/if}
        <label>Wi-Fi SSID<input bind:value={fields.ssid} /></label>
        {#if fieldErrors.ssid}<span class="fielderr">{fieldErrors.ssid}</span>{/if}
        <label>Wi-Fi password<input type="password" bind:value={fields.password} /></label>
        <label>Frame URL<input bind:value={fields.frameUrl} placeholder="https://…" /></label>
        {#if fieldErrors.frameUrl}<span class="fielderr">{fieldErrors.frameUrl}</span>{/if}
        <label>C2 URL<input bind:value={fields.c2Url} placeholder="https://…" /></label>
        {#if fieldErrors.c2Url}<span class="fielderr">{fieldErrors.c2Url}</span>{/if}
        <label>C2 period (s)<input bind:value={fields.c2PeriodSeconds} placeholder="1800" /></label>
        <label>Wake interval (s)<input bind:value={fields.wakeSeconds} placeholder="optional" /></label>
      </div>
      <button onclick={provision} disabled={busy || !provisionValid}>Provision + emit key</button>
    </li>

    <li class="step" class:done={enrolled}>
      <h2>5 · Enroll</h2>
      {#if !session.is_admin}
        <p class="hint">Registering a device needs an admin key. The local steps above work with any operator login.</p>
      {/if}
      <div class="grid">
        <label>Label <small>(optional)</small><input bind:value={label} /></label>
        <label>Channel <small>(optional — blank keeps the default)</small><input bind:value={channel} placeholder="stable" /></label>
      </div>
      <button onclick={enroll} disabled={!enrollAllowed || busy} title={affordance.title} aria-disabled={affordance['aria-disabled']}>
        Register device (POST /api/devices)
      </button>
      {#if bondState === 'waiting'}<span class="hint">Registered — waiting for the first bond (bonds on the next C2 poll).</span>{/if}
      {#if bondState === 'bonded'}<span class="ok">Bonded end-to-end ✓</span>{/if}
    </li>
  </ol>

  <section class="logpane" aria-label="onboarding log">
    {#each logLines as line, i (i)}<div class={line.cls}>{line.msg}</div>{/each}
  </section>
</section>

<style>
  .onboard {
    max-width: 46rem;
  }
  .lede {
    color: var(--muted, #666);
  }
  .banner.err {
    border: 1px solid #c0392b;
    background: #fdecea;
    color: #922;
    padding: 0.6rem 0.8rem;
    border-radius: 4px;
    margin: 0.5rem 0;
  }
  .steps {
    list-style: none;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  .steps.disabled {
    opacity: 0.55;
  }
  .step {
    border: 1px solid var(--border, #ddd);
    border-radius: 6px;
    padding: 0.75rem 1rem;
  }
  .step.done {
    border-color: #2e7d32;
  }
  .step h2 {
    font-size: 1rem;
    margin: 0 0 0.4rem;
  }
  .grid {
    display: grid;
    grid-template-columns: 1fr;
    gap: 0.4rem;
    margin: 0.4rem 0;
  }
  .grid label {
    display: flex;
    flex-direction: column;
    font-size: 0.85rem;
  }
  .grid input {
    padding: 0.35rem;
  }
  .fielderr {
    color: #c0392b;
    font-size: 0.8rem;
  }
  .chk {
    display: block;
    font-size: 0.85rem;
    margin-bottom: 0.4rem;
  }
  .hint {
    font-size: 0.82rem;
    color: var(--muted, #666);
  }
  .ok {
    color: #2e7d32;
  }
  progress {
    width: 100%;
  }
  .logpane {
    margin-top: 1rem;
    font-family: ui-monospace, monospace;
    font-size: 0.78rem;
    background: #111;
    color: #ddd;
    padding: 0.6rem;
    border-radius: 4px;
    max-height: 16rem;
    overflow-y: auto;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .logpane .err {
    color: #ff6b6b;
  }
  .logpane .ok {
    color: #7bd88f;
  }
  .logpane .info {
    color: #8ab4f8;
  }
  .logpane .dim {
    color: #999;
  }
</style>
