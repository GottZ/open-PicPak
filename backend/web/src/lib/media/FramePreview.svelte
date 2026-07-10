<script lang="ts">
  // <FramePreview> (design 29 §4.3) — the extracted BWRY canvas idiom, lifted verbatim out of the FaaS
  // test-run inline preview (FunctionsEditor). It paints the EXACT 30000 packed bytes a device receives onto
  // a 400×300 <canvas> via decodePacked → putImageData (the only preview that cannot lie about quantize /
  // Y-flip), and, when a pre-pack raw RGB buffer is supplied, a side-by-side raw canvas. Pure Canvas
  // (putImageData needs no img-src, no blob:) → CSP-neutral (web.go:51). The paint path is browser-only
  // (vite test env = node, §2 — no DOM/canvas), so the decode contract it relies on is node-pinned in
  // framepreview.test.ts; the component is svelte-check-only. Captions/alt come from the caller — this
  // component holds no hardcoded UI strings of its own.
  import { decodePacked, rawRGBToRGBA, W, H } from '../faas/bwrydecode'

  interface Props {
    packed: Uint8Array | null
    raw?: Uint8Array | null
    alt: string // panel figcaption + canvas aria-label
    rawAlt?: string // raw figcaption + aria-label (only rendered when `raw` is present)
  }
  const { packed, raw = null, alt, rawAlt = '' }: Props = $props()

  let panelCanvas = $state<HTMLCanvasElement | null>(null)
  let rawCanvas = $state<HTMLCanvasElement | null>(null)

  // Build a 400×300 ImageData from an RGBA buffer via .data.set — avoids the ImageData(data,…) constructor
  // overload whose lib type rejects a Uint8ClampedArray<ArrayBufferLike> (SharedArrayBuffer union).
  function toImageData(rgba: Uint8ClampedArray): ImageData {
    const img = new ImageData(W, H)
    img.data.set(rgba)
    return img
  }

  // Paint the panel (BWRY decode) + raw (pre-pack RGB) canvases whenever the bytes or the canvas nodes
  // change. ImageData/putImageData are browser-only (never runs in vitest).
  $effect(() => {
    if (packed && panelCanvas) {
      const c = panelCanvas.getContext('2d')
      if (c) c.putImageData(toImageData(decodePacked(packed)), 0, 0)
    }
  })
  $effect(() => {
    if (raw && rawCanvas) {
      const c = rawCanvas.getContext('2d')
      if (c) c.putImageData(toImageData(rawRGBToRGBA(raw)), 0, 0)
    }
  })
</script>

<div class="previews">
  <figure>
    <figcaption>{alt}</figcaption>
    <canvas bind:this={panelCanvas} width={W} height={H} aria-label={alt}></canvas>
  </figure>
  {#if raw}
    <figure>
      <figcaption>{rawAlt}</figcaption>
      <canvas bind:this={rawCanvas} width={W} height={H} aria-label={rawAlt}></canvas>
    </figure>
  {/if}
</div>

<style>
  .previews {
    display: flex;
    flex-wrap: wrap;
    gap: 1rem;
  }
  figure {
    margin: 0;
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
  }
  figcaption {
    font-size: 0.72rem;
    color: var(--fg-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  canvas {
    border: 1px solid var(--border);
    border-radius: 4px;
    width: 400px;
    max-width: 100%;
    height: auto;
    image-rendering: pixelated;
  }
</style>
