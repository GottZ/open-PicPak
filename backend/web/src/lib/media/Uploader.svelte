<script lang="ts">
  // Uploader (design 29 §4.4) — the thin browser shell over uploader.ts. File-input
  // + drag-drop, a client dimension guard that blocks a decode-bomb BEFORE any
  // request, a best-effort BWRY approx preview through <FramePreview>, an upload with
  // a real progress fraction (apiUpload/XHR), and a localized error surface for the
  // server 422 envelope. All guard/orchestration decisions live in uploader.ts
  // (node-tested); this file holds only the DOM + the browser-only decode. No
  // hardcoded UI strings — every label comes from the Paraglide catalog.
  import FramePreview from './FramePreview.svelte'
  import { apiUpload } from '../api-binary'
  import { notify } from '../toasts.svelte'
  import { session } from '../auth.svelte'
  import { mutationAffordance } from '../readonly'
  import { m } from '../../paraglide/messages.js'
  import {
    ACCEPT_ATTR,
    HEADER_SLICE,
    W,
    H,
    GuardError,
    guardedUpload,
    guardText,
    uploadErrorText,
    readImageDimensions,
    quantizeNearest,
  } from './uploader'
  import type { UploadedImage } from './types'

  // Refresh the library after a successful upload (design 29 §7 W4 gate: success ⇒
  // library refresh). MediaHome passes its Resource.reload.
  const { onUploaded }: { onUploaded: () => void } = $props()

  let dragging = $state(false)
  let uploading = $state(false)
  let progress = $state(0) // 0..1, upload-progress fraction
  let previewPacked = $state<Uint8Array | null>(null)
  let previewName = $state('')
  let fileInput = $state<HTMLInputElement | null>(null)

  const affordance = $derived(mutationAffordance(session.is_admin))

  // Draw the approx preview from the RESIZED bitmap only — createImageBitmap's
  // resizeWidth/Height bounds the OUTPUT surface (400×300); the source pixel cap was
  // already enforced by the header guard, so this decode is safe. Best-effort: any
  // failure just skips the preview (S7 — the preview is non-authoritative).
  async function renderApproxPreview(file: File): Promise<void> {
    try {
      const bitmap = await createImageBitmap(file, {
        resizeWidth: W,
        resizeHeight: H,
        resizeQuality: 'high',
      })
      const canvas = new OffscreenCanvas(W, H)
      const ctx = canvas.getContext('2d')
      if (!ctx) return
      ctx.drawImage(bitmap, 0, 0, W, H)
      bitmap.close()
      const rgba = ctx.getImageData(0, 0, W, H).data
      previewPacked = quantizeNearest(rgba)
    } catch {
      previewPacked = null
    }
  }

  async function handleFile(file: File): Promise<void> {
    if (uploading || !session.is_admin) return
    uploading = true
    progress = 0
    previewPacked = null
    previewName = file.name
    try {
      const res = await guardedUpload(file, {
        probe: async (f) => {
          const head = new Uint8Array(await f.slice(0, HEADER_SLICE).arrayBuffer())
          const dims = readImageDimensions(head)
          if (!dims) throw new Error('undecodable header')
          return dims
        },
        upload: async (form, onProgress) => {
          // Guard passed → the resized decode is bounded; draw the approx preview,
          // then push the original bytes with real progress.
          await renderApproxPreview(file)
          return apiUpload<UploadedImage>('/api/images', form, onProgress)
        },
        onProgress: (frac) => {
          progress = frac
        },
      })
      notify.success(m['media.upload.success']({ id: String(res.id) }))
      onUploaded()
    } catch (e) {
      previewPacked = null
      if (e instanceof GuardError) notify.error(guardText(e.reason))
      else notify.error(uploadErrorText(e))
    } finally {
      uploading = false
      progress = 0
      if (fileInput) fileInput.value = '' // allow re-selecting the same file
    }
  }

  function onInputChange(e: Event): void {
    const input = e.currentTarget as HTMLInputElement
    const file = input.files?.[0]
    if (file) void handleFile(file)
  }

  function onDrop(e: DragEvent): void {
    e.preventDefault()
    dragging = false
    if (!session.is_admin) return
    const file = e.dataTransfer?.files?.[0]
    if (file) void handleFile(file)
  }

  function onDragOver(e: DragEvent): void {
    e.preventDefault()
    if (session.is_admin) dragging = true
  }

  function onDragLeave(): void {
    dragging = false
  }
</script>

<section class="uploader" aria-label={m['media.upload.heading']()}>
  <h2>{m['media.upload.heading']()}</h2>

  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="dropzone"
    class:dragging
    class:disabled={affordance.disabled}
    ondragover={onDragOver}
    ondragleave={onDragLeave}
    ondrop={onDrop}
  >
    <p class="hint">{m['media.upload.dropzone']()}</p>
    <input
      bind:this={fileInput}
      type="file"
      accept={ACCEPT_ATTR}
      onchange={onInputChange}
      disabled={affordance.disabled || uploading}
      aria-label={m['media.upload.choose']()}
    />
    {#if affordance.disabled}<p class="muted">{affordance.title}</p>{/if}
  </div>

  {#if uploading}
    <div class="progress" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(progress * 100)}>
      <div class="bar" style:width={`${Math.round(progress * 100)}%`}></div>
      <span class="pct">{m['media.upload.uploading']({ pct: String(Math.round(progress * 100)) })}</span>
    </div>
  {/if}

  {#if previewPacked}
    <figure class="preview">
      <figcaption>{m['media.upload.preview_caption']({ name: previewName })}</figcaption>
      <FramePreview packed={previewPacked} alt={m['media.upload.preview_caption']({ name: previewName })} />
    </figure>
  {/if}
</section>

<style>
  .uploader {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  .dropzone {
    border: 1.5px dashed var(--border);
    border-radius: 8px;
    padding: 1.25rem;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    align-items: flex-start;
    transition: border-color 0.15s, background 0.15s;
  }
  .dropzone.dragging {
    border-color: var(--accent, #4a90d9);
    background: color-mix(in srgb, var(--accent, #4a90d9) 8%, transparent);
  }
  .dropzone.disabled {
    opacity: 0.6;
  }
  .hint {
    margin: 0;
    color: var(--fg-muted);
    font-size: 0.875rem;
  }
  .muted {
    margin: 0;
    color: var(--fg-muted);
    font-size: 0.8rem;
  }
  .progress {
    position: relative;
    height: 1.25rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    overflow: hidden;
    background: var(--bg-muted, #eee);
  }
  .bar {
    height: 100%;
    background: var(--accent, #4a90d9);
    transition: width 0.1s linear;
  }
  .pct {
    position: absolute;
    inset: 0;
    display: grid;
    place-items: center;
    font-size: 0.75rem;
    color: var(--fg);
  }
  .preview {
    margin: 0;
  }
  .preview figcaption {
    font-size: 0.8rem;
    color: var(--fg-muted);
    margin-bottom: 0.35rem;
  }
</style>
