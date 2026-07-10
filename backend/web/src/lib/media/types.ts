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
