<script lang="ts">
  // Web-USB onboarding (Design 26): flash the open-picpak CFW + provision Wi-Fi/URL + C2-enroll, all in the
  // operator's browser over WebSerial. The USB/console engine is the framework-free lib/webusb modules (W1);
  // this component is the step UI + flow state (D26.1). Device behaviour (BOND emits a valid key, bonds e2e)
  // is the W3 on-device gate (G1-G3) — not exercisable headless.
  import { onMount } from 'svelte'
  import { apiFetch, toApiError } from '../../lib/api'
  import { session } from '../../lib/auth.svelte'
  import { notify } from '../../lib/toasts.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import {
    Flasher,
    manifestFlashBytes,
    webSerialLink,
    pulseHardReset,
    CONSOLE_BAUD,
    ESPRESSIF_USB_VID,
    ESP_IMAGE_MAGIC,
  } from '../../lib/webusb/flasher'
  import { ConsoleSession } from '../../lib/webusb/console'
  import { nudgeDeviceExit, type NudgeLink, type NudgeOutcome } from '../../lib/webusb/exitNudge'
  import { runProvision } from '../../lib/webusb/provision'
  import { validateProvisionFields, hasErrors, type ProvisionFields } from '../../lib/webusb/validate'
  import { buildEnrollBody, isEnrollSuccess, canEnroll, enrollNeedsConfirm } from '../../lib/webusb/enroll'
  import { webSerialReady } from '../../lib/webusb/support'
  import { compareFirmware, type AppDesc } from '../../lib/webusb/appdesc'
  import { prefillField, selectWifi, type NvsIdentity } from '../../lib/webusb/nvs-read'
  import { prefilledUrlFields, prefilledFromEndpoint } from '../../lib/onboard/defaults'
  import type { OnboardDefaultsResponse } from '../../lib/onboard/defaults'
  import type { FlashManifest, LogSink } from '../../lib/webusb/types'
  import type { Device } from '../../lib/api/types'
  import { m } from '../../paraglide/messages.js'
  // NOTE (A34.2): the operator-facing UI chrome, notify.* toasts and confirm()
  // texts below are catalog-backed. The low-level log() lines that stream into the
  // monospace onboarding console stay English by design — they are transport /
  // esptool diagnostics (a serial-monitor surface), consistent with §2 "the server
  // stays English" and the LogViewer device-payload exemption.

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
  let nudgeState = $state<'idle' | 'running' | NudgeOutcome>('idle')

  // --- USB ident (A26.10): what the post-connect flash read found. Pure display + prefill source;
  // reading is fail-open (a null half just shows "unknown"). The c2_sk private key is deliberately
  // never read into this state (see lib/webusb/nvs-read SECURITY BOUNDARY). ------------------------
  let identDone = $state(false)
  let identNvs = $state<NvsIdentity | null>(null)
  let identFw = $state<AppDesc | null>(null)
  let manifestVersion = $state<string | null>(null)
  let wifiPrefilled = $state(false)
  const fwStatus = $derived(compareFirmware(identFw, manifestVersion))

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

  // --- prefill (A35.3 + A35.3b): under A35's ONE backend, location.origin IS the device endpoint
  // (DECISIONS §A35). Pre-fill ONLY blank frame-/C2-URL + C2 period; a value the operator already typed is
  // never overwritten. Fallback chain (DECISIONS §A35-W3b): for an ADMIN session, GET /api/onboard/defaults
  // hands the REAL tokenized URLs (the token is strictly weaker than an admin RCE enqueue); a non-admin
  // session, or any endpoint failure, keeps the A35.3 origin-PLACEHOLDER path (INGEST_TOKEN stays
  // server-side-only there). urlsTokenized flips the hint text once real URLs replaced the placeholder. ---
  let urlsTokenized = $state(false)
  onMount(async () => {
    const origin = typeof location !== 'undefined' ? location.origin : ''
    if (!origin) return
    let d = prefilledUrlFields(fields, origin) // A35.3 placeholder base
    if (session.is_admin) {
      try {
        const resp = await apiFetch<OnboardDefaultsResponse>(
          '/api/onboard/defaults?origin=' + encodeURIComponent(origin),
        )
        d = prefilledFromEndpoint(fields, resp, origin) // real token fills blanks; blank payload → placeholder
        urlsTokenized = !!(resp.frame_url || resp.c2_url)
      } catch {
        /* keep the placeholder base — endpoint dark or unreachable */
      }
    }
    fields.frameUrl = d.frameUrl
    fields.c2Url = d.c2Url
    fields.c2PeriodSeconds = d.c2PeriodSeconds
  })

  // --- helpers -------------------------------------------------------------------------------------------
  /** Fetch a firmware artifact by manifest path from the same-origin /onboard-fw/ mount (D26.9). Binary, so
   *  a raw fetch (not apiFetch, which is JSON-only); the httpOnly ppk_sid cookie authenticates it via
   *  credentials: 'same-origin' (design 28 §4.3). A GET, so no CSRF header. */
  async function fetchPart(path: string): Promise<Uint8Array> {
    const res = await fetch('/onboard-fw/' + path, { credentials: 'same-origin', cache: 'no-store' })
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
      await readIdentity()
    } catch (e) {
      log('Connect failed: ' + (e as Error).message, 'err')
      notify.warn(m['onboard.notify.connect_failed']())
    } finally {
      busy = false
    }
  }

  // Read factory serial + provisioned Wi-Fi + installed firmware straight from flash, then prefill any
  // BLANK field the operator hasn't touched (A35.3 prefill discipline: never overwrite a typed value).
  // Everything here is best-effort — a read miss just leaves the panel showing "unknown".
  async function readIdentity() {
    if (!flasher) return
    try {
      const ident = await flasher.readIdent()
      identNvs = ident.nvs
      identFw = ident.firmware
      identDone = true
      const mf = await loadManifest().catch(() => null)
      manifestVersion = mf?.version ?? null
      if (ident.nvs) {
        fields.serial = prefillField(fields.serial, ident.nvs.serial)
        const wifi = selectWifi(ident.nvs)
        if (wifi) {
          const beforeSsid = fields.ssid
          fields.ssid = prefillField(fields.ssid, wifi.ssid)
          fields.password = prefillField(fields.password, wifi.pass)
          if (fields.ssid !== beforeSsid) wifiPrefilled = true
        }
      }
    } catch (e) {
      log('Identity read skipped: ' + (e as Error).message, 'dim')
    }
  }

  async function backup() {
    if (!flasher || busy) return
    busy = true
    backupProgress = 0
    try {
      const mf = await loadManifest().catch(() => null)
      const bytes = manifestFlashBytes(mf)
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
      const mf = await loadManifest()
      // G7: reflash preserves NVS (Wi-Fi/dev_sn/identity keypair) unless the operator opts into a full erase.
      // A "fresh flash" that keeps a stale keypair would re-enroll an old identity believing it is new.
      const manifest: FlashManifest = { ...mf, eraseBeforeFlash: eraseNvs || !!mf.eraseBeforeFlash }
      log(`Flashing ${mf.name} ${mf.version}${eraseNvs ? ' (full erase — NVS/identity wiped)' : ''}…`, 'info')
      await flasher.flash(manifest, fetchPart, (p) => (flashProgress = p))
      flashed = true
      connected = false // esptool transport released; provision re-opens at console baud
      log('Flash complete. Rebooting into the CFW…', 'ok')
      notify.success(m['onboard.notify.flashed']())
    } catch (e) {
      const err = toApiError(e)
      if (err.status === 404) {
        notify.warn(m['onboard.notify.fw_missing']())
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
        notify.warn(m['onboard.notify.provisioning_reason']({ reason: res.reason }))
        log('Provisioning stopped: ' + res.reason, 'err')
        return
      }
      pubkeyHex = res.pubkeyHex
      provisioned = true
      log('Provisioned; device emitted its public key.', 'ok')
      notify.success(m['onboard.notify.provisioned']())
    } catch (e) {
      log('Provisioning failed: ' + (e as Error).message, 'err')
      notify.warn(m['onboard.notify.provision_failed']())
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
          existing?.bonded
            ? m['onboard.confirm_reenroll_bonded']({ serial: fields.serial })
            : m['onboard.confirm_reenroll']({ serial: fields.serial }),
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
        notify.warn(m['onboard.notify.enroll_unexpected']({ action: res.action }))
        return
      }
      enrolled = true
      notify.success(m['onboard.notify.enrolled']({ action: res.action }))
      // C1 (design 03 §4.3): the device is still trapped in its setup console and would never poll. Nudge it
      // out NOW — after the pubkey landed on the server (POST above), before pollBond — so its FIRST poll
      // finds the registered key and bonds inside the 180 s window. pollBond runs regardless of the outcome
      // ('failed' included): the device can still bond later, e.g. via a firmware self-exit.
      nudgeState = 'running'
      nudgeState = await nudgeDeviceExit({ reopen: nudgeReopen, hardReset: nudgeHardReset, log })
      void pollBond()
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      busy = false
    }
  }

  // --- exit-nudge transports (C1): the deps injected into nudgeDeviceExit. Same reopen pattern as
  // provision() (stale handle after USB re-enumeration → re-fetch the authorized port; never requestPort —
  // no fresh user gesture here). The reset fallback reopens the port only for the pulse. -----------------
  async function openConsolePort(): Promise<SerialPort> {
    let p = port ?? (await findPicpakPort())
    if (!p) throw new Error('no authorized PicPak port')
    try {
      await p.open({ baudRate: CONSOLE_BAUD })
    } catch {
      p = await findPicpakPort()
      if (!p) throw new Error('no authorized PicPak port')
      await p.open({ baudRate: CONSOLE_BAUD })
    }
    port = p
    return p
  }

  async function nudgeReopen(): Promise<NudgeLink> {
    const p = await openConsolePort()
    const link = webSerialLink(p)
    const cs = new ConsoleSession(link, log)
    cs.start()
    return {
      sendCommand: (line, opts) => cs.sendCommand(line, opts),
      close: async () => {
        await link.close()
        try {
          await p.close()
        } catch {
          /* already closed */
        }
      },
    }
  }

  async function nudgeHardReset(): Promise<void> {
    const p = await openConsolePort()
    try {
      await pulseHardReset(p, log)
    } finally {
      try {
        await p.close()
      } catch {
        /* already closed */
      }
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
          notify.success(m['onboard.notify.bonded']())
          return
        }
      } catch {
        /* keep polling — the device bonds on its next C2 poll */
      }
    }
  }
</script>

<section class="onboard">
  <h1>{m['onboard.title']()}</h1>
  <p class="lede">
    {m['onboard.lede']()}
  </p>

  {#if !serialReady}
    <div class="banner err" role="alert">
      {m['onboard.no_serial']()}
    </div>
  {/if}

  <ol class="steps" class:disabled={!serialReady}>
    <li class="step" class:done={connected}>
      <h2>{m['onboard.step1']()}</h2>
      <button onclick={connect} disabled={!serialReady || busy || connected}>{m['onboard.connect']()}</button>
      {#if chip}<span class="ok">{m['onboard.connected']({ chip })}</span>{/if}
      {#if identDone}
        <dl class="ident">
          <dt>{m['onboard.ident.serial_label']()}</dt>
          <dd>{identNvs?.serial ?? m['onboard.ident.no_serial_found']()}</dd>
          <dt>{m['onboard.ident.fw_label']()}</dt>
          <dd>
            {#if fwStatus.kind === 'unknown'}
              <span class="dim">{m['onboard.ident.fw_unknown']()}</span>
            {:else}
              <span class="fwname">{fwStatus.projectName} {fwStatus.version}</span>
              {#if fwStatus.kind === 'stock'}
                <span class="badge warn">{m['onboard.ident.stock']()}</span>
              {:else if fwStatus.kind === 'update'}
                <span class="badge warn">{m['onboard.ident.update']({ from: fwStatus.version, to: fwStatus.to })}</span>
              {:else}
                <span class="badge ok">{m['onboard.ident.current']()}</span>
              {/if}
            {/if}
          </dd>
        </dl>
        {#if wifiPrefilled}<p class="hint">{m['onboard.ident.wifi_from_device']()}</p>{/if}
      {/if}
    </li>

    <li class="step" class:done={backupProgress >= 100}>
      <h2>{m['onboard.step2']()} <small>{m['onboard.recommended']()}</small></h2>
      <button onclick={backup} disabled={!connected || busy}>{m['onboard.backup']()}</button>
      {#if backupProgress > 0}<progress max="100" value={backupProgress}></progress>{/if}
    </li>

    <li class="step" class:done={flashed}>
      <h2>{m['onboard.step3']()}</h2>
      <label class="chk">
        <input type="checkbox" bind:checked={eraseNvs} disabled={busy} />
        {m['onboard.erase_label']()}
      </label>
      <button onclick={flash} disabled={!connected || busy}>{m['onboard.flash']()}</button>
      {#if flashProgress > 0}<progress max="100" value={flashProgress}></progress>{/if}
    </li>

    <li class="step" class:done={provisioned}>
      <h2>{m['onboard.step4']()}</h2>
      <p class="hint">{m['onboard.provision_hint']()}</p>
      <div class="grid">
        <label>{m['onboard.field.serial']()}<input bind:value={fields.serial} placeholder="PP-001" /></label>
        {#if fieldErrors.serial}<span class="fielderr">{fieldErrors.serial}</span>{/if}
        <label>{m['onboard.field.ssid']()}<input bind:value={fields.ssid} /></label>
        {#if fieldErrors.ssid}<span class="fielderr">{fieldErrors.ssid}</span>{/if}
        <label>{m['onboard.field.password']()}<input type="password" bind:value={fields.password} /></label>
        <label>{m['onboard.field.frame_url']()}<input bind:value={fields.frameUrl} placeholder="https://…" /></label>
        {#if fieldErrors.frameUrl}<span class="fielderr">{fieldErrors.frameUrl}</span>{/if}
        <label>{m['onboard.field.c2_url']()}<input bind:value={fields.c2Url} placeholder="https://…" /></label>
        {#if fieldErrors.c2Url}<span class="fielderr">{fieldErrors.c2Url}</span>{/if}
        <p class="hint urlhint">{urlsTokenized ? m['onboard.url_default_hint_tokenized']() : m['onboard.url_default_hint']()}</p>
        <label>{m['onboard.field.c2_period']()}<input bind:value={fields.c2PeriodSeconds} placeholder="1800" /></label>
        <label>{m['onboard.field.wake']()}<input bind:value={fields.wakeSeconds} placeholder={m['onboard.ph_optional']()} /></label>
      </div>
      <button onclick={provision} disabled={busy || !provisionValid}>{m['onboard.provision_btn']()}</button>
    </li>

    <li class="step" class:done={enrolled}>
      <h2>{m['onboard.step5']()}</h2>
      {#if !session.is_admin}
        <p class="hint">{m['onboard.enroll_admin_hint']()}</p>
      {/if}
      <div class="grid">
        <label>{m['onboard.field.label']()} <small>{m['onboard.optional']()}</small><input bind:value={label} /></label>
        <label>{m['onboard.field.channel']()} <small>{m['onboard.channel_hint']()}</small><input bind:value={channel} placeholder="stable" /></label>
      </div>
      <button onclick={enroll} disabled={!enrollAllowed || busy} title={affordance.title} aria-disabled={affordance['aria-disabled']}>
        {m['onboard.enroll_btn']()}
      </button>
      {#if nudgeState === 'running'}<span class="hint">{m['onboard.nudge.running']()}</span>{/if}
      {#if nudgeState === 'refresh'}<span class="ok">{m['onboard.nudge.refresh']()}</span>{/if}
      {#if nudgeState === 'hard-reset'}<span class="ok">{m['onboard.nudge.hard_reset']()}</span>{/if}
      {#if nudgeState === 'failed'}<span class="hint">{m['onboard.nudge.failed']()}</span>{/if}
      {#if bondState === 'waiting'}<span class="hint">{m['onboard.waiting_bond']()}</span>{/if}
      {#if bondState === 'bonded'}<span class="ok">{m['onboard.bonded']()}</span>{/if}
    </li>
  </ol>

  <section class="logpane" aria-label={m['onboard.log_aria']()}>
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
  .urlhint {
    margin: -0.15rem 0 0.15rem;
    font-size: 0.78rem;
  }
  .ok {
    color: #2e7d32;
  }
  .ident {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.15rem 0.6rem;
    margin: 0.5rem 0 0;
    font-size: 0.85rem;
  }
  .ident dt {
    color: var(--muted, #666);
  }
  .ident dd {
    margin: 0;
  }
  .ident .fwname {
    font-family: ui-monospace, monospace;
  }
  .badge {
    display: inline-block;
    margin-left: 0.4rem;
    padding: 0.05rem 0.4rem;
    border-radius: 999px;
    font-size: 0.75rem;
  }
  .badge.ok {
    background: #e6f4ea;
    color: #1e7d32;
  }
  .badge.warn {
    background: #fef3e0;
    color: #9a5b00;
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
