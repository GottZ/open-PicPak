// The sim-manifest surface (design/33 §3, W11 doctrine "output the config"). firmware/sim/build.sh
// writes firmware/sim/sim-manifest.json AND copies it to backend/web/public/picpak-berry.manifest.json
// so the panel can fetch it at /picpak-berry.manifest.json and show the Berry pin + emsdk version + the
// expected .wasm.br size — the drift-visible provenance of the WASM the operator is running. Pure and
// CM6-free so the parse/format is node-testable; the fetch itself lives in the panel.

export interface SimManifest {
  berry_pin: string
  berry_conf_base_sha256: string
  sim_conf_patch_sha256: string
  fb_c_sha256: string
  emsdk_version: string
  expected_wasm_br_bytes: number
}

/** A short, human pin line: `berry bd9c93b · emsdk 4.0.16 · 79.6 KB`. */
export function pinLine(m: SimManifest): string {
  const pin = m.berry_pin.slice(0, 7)
  const kb = (m.expected_wasm_br_bytes / 1024).toFixed(1)
  return `berry ${pin} · emsdk ${m.emsdk_version} · ${kb} KB`
}
