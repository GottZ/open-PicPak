// WebSerial environment gate (Design 26 §4.2 / §2.5, F7). navigator.serial is Chromium-desktop only and
// requires a SECURE CONTEXT (HTTPS in prod behind the TLS proxy, or localhost in dev). Onboard.svelte shows
// the unsupported banner + disables Connect when either is unmet, instead of letting requestPort() throw an
// opaque error. Pure (takes the navigator/context in) so it is node-testable without a browser.

/** True when the Web Serial API is present (Chromium-desktop). Pass `navigator`; a fake in tests. */
export function serialApiSupported(nav: unknown): boolean {
  return typeof nav === 'object' && nav !== null && 'serial' in nav && (nav as { serial?: unknown }).serial != null
}

/** True when the page runs in a secure context (required for WebSerial). Pass `window.isSecureContext`. */
export function secureContextOk(isSecureContext: boolean | undefined): boolean {
  return isSecureContext === true
}

/** Both conditions for the Connect affordance to be enabled (F7). */
export function webSerialReady(nav: unknown, isSecureContext: boolean | undefined): boolean {
  return serialApiSupported(nav) && secureContextOk(isSecureContext)
}
