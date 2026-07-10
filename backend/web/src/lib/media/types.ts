// Wire types for the /media image API (design 29 §3), hand-maintained against the
// SHIPPED A28 response — not the design's speculative shape. Verified against
// cmd/admin/image_http.go (imageMetaFields) + internal/imgstore/types.go: the real
// API has NO `name` field, keys the byte length `byte_size` (not `bytes`), and the
// route is /api/images (not /api/media/images). No OpenAPI — the Go golden test is
// the only drift anchor (Ist-Konvention api/types.ts).

export interface Image {
  id: number
  sha256: string
  mime: string
  width: number
  height: number
  byte_size: number
  created_at: string
  // List items serialize imgstore.Image directly and MAY carry this; the upload /
  // get responses go through imageMetaFields and omit it (image_http.go).
  operator_key_id?: number
}

// Upload success body: adminhttp spreads the image fields at the top level next to
// success:true (WriteOK), so the parsed envelope is an Image plus the flag.
export type UploadedImage = Image & { success: boolean }

// GET /api/images — cursor-paginated at target scale (§6). next_cursor is the last
// id of a full page, else null. W4 consumes only the first page (single-shot
// Resource); W5 swaps in the paged accumulator (lib/media/paged.svelte.ts).
export interface ImagesResponse {
  success: boolean
  images: Image[]
  next_cursor: number | null
}

// ---- Playlist wire vocabulary (design 29 §7 W7) ----
// Hand-maintained against the SHIPPED A28-W5b surface (cmd/admin/playlist_http.go,
// commit e783294) — the Go structs in internal/playliststore/types.go are the
// source of truth, drift-guarded bidirectionally by playlist_types_golden_test.go
// (go test ./web/). The wire field is `order_mode` (NOT the design's older
// `rotation_policy`); items carry only `image_id` (no embedded image meta — the
// thumbnail URL is built from it via /api/images/{id}/thumbnail).

// Rotation order (playliststore.orderModes) — the editor's order-mode select.
export const ORDER_MODES = ['sequential', 'shuffle'] as const
export type OrderMode = (typeof ORDER_MODES)[number]

// Per-item resize fit (playliststore.fits) — the empty server default is "cover".
export const FITS = ['cover', 'contain', 'fill'] as const
export type Fit = (typeof FITS)[number]

// Per-item dither policy (playliststore.dithers) — the empty server default is "none".
export const DITHERS = ['none', 'floyd-steinberg', 'atkinson', 'ordered'] as const
export type Dither = (typeof DITHERS)[number]

// Full playlist row — playliststore.Playlist (POST create, PATCH update, GET detail).
export interface Playlist {
  id: number
  operator_key_id?: number | null
  name: string
  interval_s: number
  order_mode: OrderMode
  shuffle_epoch: number
  version: number
  managed_serial?: string | null
  created_at: string
  updated_at: string
}

// List projection — playliststore.Summary (GET /api/playlists rows).
export interface PlaylistSummary {
  id: number
  name: string
  interval_s: number
  order_mode: OrderMode
  version: number
}

// One ordered entry — playliststore.PlaylistItem. No embedded image metadata:
// thumbnails are built from image_id (§7 W7 API-truth).
export interface PlaylistItem {
  id: number
  playlist_id: number
  image_id: number
  position: number
  fit: Fit
  dither: Dither
}

// GET /api/playlists — keyset page of summaries.
export interface PlaylistsResponse {
  success: boolean
  playlists: PlaylistSummary[]
  next_cursor: number | null
}

// GET /api/playlists/{id} — the playlist plus a keyset page of its items (?after=<position>).
export interface PlaylistDetailResponse {
  success: boolean
  playlist: Playlist
  items: PlaylistItem[]
  next_cursor: number | null
}

// POST /api/playlists + PATCH /api/playlists/{id} — the single mutated playlist.
export interface PlaylistResponse {
  success: boolean
  playlist: Playlist
}

// POST /api/playlists/{id}/items — the appended item.
export interface PlaylistItemResponse {
  success: boolean
  item: PlaylistItem
}
