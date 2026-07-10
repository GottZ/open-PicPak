package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"

	// The thumbnail route re-decodes the stored source; register the same format decoders the
	// store admits (png via the named image/png import above; gif/jpeg blank here). Registration is
	// process-global, but importing them where image.Decode is CALLED keeps the dependency explicit
	// instead of relying on imgstore's transitive registration.
	_ "image/gif"
	_ "image/jpeg"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminaudit"
	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/imgstore"
)

// defaultMaxImageBytes caps a single uploaded source blob (design E28.3): headroom over "several MB"
// photo sources, above the 16 MiB firmware cap because photos run larger. Env-configurable
// (ADMIN_MAX_IMAGE_BYTES, Policy=Data). This is the COMPRESSED byte cap; the pixel-dimension
// (decompression-bomb) cap is enforced separately inside imgstore.PutImage.
const defaultMaxImageBytes = 24 << 20

// imageFormOverhead is the slack MaxBytesReader allows ABOVE maxBytes for the multipart envelope, so a
// legitimately max-sized blob plus its form framing is not rejected as oversized (ota_http.go:54
// pattern). The blob itself is still bounded to maxBytes by the LimitReader belt-and-suspenders.
const imageFormOverhead = 1 << 20

// imageMultipartMemory is the small ParseMultipartForm memory budget — a small value forces the file
// part to spill to a temp file instead of holding a full max-sized image in RAM per parallel upload
// (design §4.2 / B4).
const imageMultipartMemory = 4 << 20

// thumbMaxDim is the longest-edge bound of the grid thumbnail (design 29 §6, E-A29-5): a small,
// same-origin, cacheable variant so the library grid at target scale (hundreds of images) never pulls
// a multi-MB source per tile — "das Grid muss ohne Full-Res-Downloads funktionieren". 320 px covers a
// retina-doubled ~160 px CSS tile. A source already within the box is re-encoded, never upscaled.
const thumbMaxDim = 320

// imageMIMEAllowlist is the B5 POSITIVE allowlist (design §5 B5 layer a). The upload is admitted ONLY
// when http.DetectContentType sniffs it as one of these — the mechanism is the allowlist, NOT a
// negative SVG/HTML detector: a bare <svg> sniffs as text/plain and falls through simply because it is
// absent from this set. imgstore.PutImage re-validates via image.DecodeConfig and stores the
// authoritative mime, so the served Content-Type is always upload-validated (layer b).
var imageMIMEAllowlist = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

type imageHandlers struct {
	pool     *pgxpool.Pool
	blobDir  string
	maxBytes int64
}

// registerImageRoutes mounts the image CRUD surface (design §4.2, W5): upload (content-addressed,
// idempotent), keyset list, meta / raw byte serve, and delete. Reads gate on image:read, writes on
// image:write (RequireScope) — an api_token reaches exactly its granted scopes, never the fleet-admin
// routes. Single wiring source so main.go and the gating test can never drift (mirror of
// registerTokenRoutes / registerOTARoutes). Mounted before the SPA catch-all.
func registerImageRoutes(mux *http.ServeMux, pool *pgxpool.Pool, blobDir string, maxBytes int64) {
	h := imageHandlers{pool: pool, blobDir: blobDir, maxBytes: maxBytes}
	mux.Handle("POST /api/images", adminhttp.Auth(pool)(adminhttp.RequireScope(adminhttp.ScopeImageWrite)(http.HandlerFunc(h.upload))))
	mux.Handle("GET /api/images", adminhttp.Auth(pool)(adminhttp.RequireScope(adminhttp.ScopeImageRead)(http.HandlerFunc(h.list))))
	mux.Handle("GET /api/images/{id}", adminhttp.Auth(pool)(adminhttp.RequireScope(adminhttp.ScopeImageRead)(http.HandlerFunc(h.get))))
	// More specific than GET /api/images/{id}; Go 1.22 routing prefers it (no conflict with the meta/raw
	// serve). Same image:read gate — a read-only operator sees the grid preview (design §5 S9 / E-A29-4).
	mux.Handle("GET /api/images/{id}/thumbnail", adminhttp.Auth(pool)(adminhttp.RequireScope(adminhttp.ScopeImageRead)(http.HandlerFunc(h.thumbnail))))
	mux.Handle("DELETE /api/images/{id}", adminhttp.Auth(pool)(adminhttp.RequireScope(adminhttp.ScopeImageWrite)(http.HandlerFunc(h.delete))))
}

// upload — POST /api/images (image:write): accept a source image as multipart (SPA path, field "file")
// OR a raw request body (machine client), enforce the byte cap + the B5 positive mime allowlist, then
// content-addressed-store it. A re-POST of identical bytes is a dedup hit → 200 with the existing id; a
// first upload → 201 (design §4.2 idempotency; the store dedups atomically regardless).
func (h imageHandlers) upload(w http.ResponseWriter, r *http.Request) {
	pr, _ := adminhttp.PrincipalFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes+imageFormOverhead)

	blob, ok := h.readUpload(w, r)
	if !ok {
		return // readUpload already wrote the error
	}
	if len(blob) == 0 {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "empty_blob", "image body is empty")
		return
	}
	if int64(len(blob)) > h.maxBytes {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "blob_too_large", "image exceeds the size cap")
		return
	}
	// B5 layer (a): positive mime allowlist. Anything outside the set is a 422 BEFORE the store touches
	// the blob volume — the load-bearing XSS defense on the raw serve (design §5 B5).
	if !allowedImageMIME(blob) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unsupported_type", "content type is not an allowed image format")
		return
	}

	// Idempotency status decision: content-addressed. The pre-check only chooses 200-vs-201; the actual
	// dedup (ON CONFLICT sha256, return-existing) is atomic inside PutImage regardless of this race.
	sum := sha256.Sum256(blob)
	sha := hex.EncodeToString(sum[:])
	_, lookupErr := imgstore.GetImageBySha(r.Context(), h.pool, sha)
	existed := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, imgstore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image lookup failed")
		return
	}

	img, err := imgstore.PutImage(r.Context(), h.pool, h.blobDir, mintedBy(pr), blob)
	switch {
	case errors.Is(err, imgstore.ErrUnsupportedFormat):
		// The store's DecodeConfig second gurt (e.g. a webp the allowlist admits but the A27 store has no
		// decoder for yet — deferred; open point in the W5 report).
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unsupported_type", "content type is not an allowed image format")
		return
	case errors.Is(err, imgstore.ErrImageTooLarge):
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "blob_too_large", "image dimensions exceed the pixel cap")
		return
	case err != nil:
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image store failed")
		return
	}

	fields := imageMetaFields(img)
	if existed {
		adminhttp.WriteOK(w, r, fields) // 200 — dedup hit, same id
		return
	}
	adminhttp.WriteCreated(w, r, fields) // 201 — new content
}

// readUpload extracts the raw image bytes from either a multipart body (field "file") or a raw request
// body, bounded to maxBytes+1 by a LimitReader (the belt-and-suspenders beside MaxBytesReader). It
// writes its own error and returns ok=false on any failure.
func (h imageHandlers) readUpload(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(imageMultipartMemory); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "blob_too_large", "multipart upload exceeds the size cap")
				return nil, false
			}
			adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed multipart body")
			return nil, false
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_file", "multipart field 'file' required")
			return nil, false
		}
		defer file.Close() //nolint:errcheck // read-only
		return h.limitedRead(w, r, file)
	}
	return h.limitedRead(w, r, r.Body)
}

func (h imageHandlers) limitedRead(w http.ResponseWriter, r *http.Request, src io.Reader) ([]byte, bool) {
	blob, err := io.ReadAll(io.LimitReader(src, h.maxBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "blob_too_large", "image exceeds the size cap")
			return nil, false
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image read failed")
		return nil, false
	}
	return blob, true
}

// list — GET /api/images (image:read): a keyset page over the store, ordered by id (WHERE id > after).
// The store issues ONE query with no per-row playlist fanout (imgstore.ListImages) — the N+1 the design
// forbids (§4.2 / §6) is impossible by construction. next_cursor is the last id when a full page was
// returned (more may follow), else null.
func (h imageHandlers) list(w http.ResponseWriter, r *http.Request) {
	limit := clampImageLimit(r.URL.Query().Get("limit"))
	cursor := parseImageCursor(r.URL.Query().Get("after"))
	imgs, err := imgstore.ListImages(r.Context(), h.pool, limit, cursor)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image list failed")
		return
	}
	var next any
	if len(imgs) == limit && len(imgs) > 0 {
		next = imgs[len(imgs)-1].ID
	}
	adminhttp.WriteOK(w, r, map[string]any{"images": imgs, "next_cursor": next})
}

// get — GET /api/images/{id} (image:read): metadata JSON by default, or the raw bytes with ?raw=1.
func (h imageHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, ok := imageID(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("raw") == "1" {
		h.serveRaw(w, r, id)
		return
	}
	img, err := imgstore.GetImage(r.Context(), h.pool, id)
	if errors.Is(err, imgstore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such image")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image read failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"image": imageMetaFields(img)})
}

// serveRaw streams an image's bytes with the three B5 hardening layers (design §5 B5): the Content-Type
// is the mime VALIDATED AT UPLOAD (never re-sniffed from the bytes), plus nosniff, the tightest CSP
// (default-src 'none' — a raw image needs no resources), and Content-Disposition: inline. A missing
// blob fails closed (500), never a wrong-typed serve.
func (h imageHandlers) serveRaw(w http.ResponseWriter, r *http.Request, id int64) {
	blob, img, err := imgstore.LoadBlob(r.Context(), h.pool, h.blobDir, id)
	if errors.Is(err, imgstore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such image")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image blob unavailable")
		return
	}
	w.Header().Set("Content-Type", img.Mime) // (b) upload-validated stored mime, never re-guessed
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("Content-Disposition", "inline; filename=\""+strconv.FormatInt(id, 10)+extForMIME(img.Mime)+"\"")
	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// thumbnail — GET /api/images/{id}/thumbnail (image:read): a small, downscaled PNG variant of the
// stored source for the library grid (design 29 §6 / E-A29-5). The variant is DERIVED deterministically
// from immutable, content-addressed source bytes (an id → sha256 row never changes), so it carries a
// strong ETag (the sha) and `immutable` caching — every browser fetches a given thumbnail exactly once.
// Cache-Control is `private` (never `public`): the bytes are image:read-gated, so a shared proxy must
// not retain them and serve an unauthenticated client (§5 S9 enumeration containment). Content-Type is
// authoritative image/png because THIS handler encodes it — plus the B5 nosniff + tightest CSP, mirror
// of serveRaw. A missing blob or an undecodable source fails closed (500), never a wrong-typed serve.
func (h imageHandlers) thumbnail(w http.ResponseWriter, r *http.Request) {
	id, ok := imageID(w, r)
	if !ok {
		return
	}
	blob, img, err := imgstore.LoadBlob(r.Context(), h.pool, h.blobDir, id)
	if errors.Is(err, imgstore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such image")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image blob unavailable")
		return
	}

	etag := thumbETag(img.Sha256)
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		writeThumbCacheHeaders(w, etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	src, _, err := image.Decode(bytes.NewReader(blob))
	if err != nil {
		// The bytes were DecodeConfig-validated at upload, so this is an internal condition (a decoder
		// gap, a corrupted blob), not a client error — fail closed rather than serve a wrong type.
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image decode failed")
		return
	}
	var out bytes.Buffer
	if err := png.Encode(&out, downscale(src, thumbMaxDim)); err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "thumbnail encode failed")
		return
	}

	w.Header().Set("Content-Type", "image/png") // authoritative — encoded here, never re-sniffed
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	writeThumbCacheHeaders(w, etag)
	w.Header().Set("Content-Length", strconv.Itoa(out.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Bytes())
}

// thumbETag is the strong validator for a thumbnail: the source content address plus the variant
// dimension, so a future maxDim change never collides with a cached old variant.
func thumbETag(sha string) string {
	return `"t` + strconv.Itoa(thumbMaxDim) + "-" + sha + `"`
}

// writeThumbCacheHeaders sets the immutable, browser-private cache contract shared by the 200 and 304
// paths (design §6 "cachebar"). private (not public): the bytes are auth-gated.
func writeThumbCacheHeaders(w http.ResponseWriter, etag string) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
}

// etagMatches reports whether an If-None-Match header lists etag (or is the wildcard). Tolerates the
// weak `W/` prefix and a comma-separated list (RFC 9110 §8.8.3) — a thumbnail is stable, so a weak
// match is as good as a strong one here.
func etagMatches(inm, etag string) bool {
	bare := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(inm, ",") {
		p := strings.TrimSpace(part)
		if p == "*" || strings.TrimPrefix(p, "W/") == bare {
			return true
		}
	}
	return false
}

// downscale box-averages src into a PNG-ready RGBA bounded to maxDim on its longest edge, preserving
// aspect and never upscaling (a source within the box keeps its size). One pass over the source
// accumulates each source pixel into its target cell — O(source pixels), bounded by the upload
// pixel-dimension cap (imgstore.MaxImagePixels), so the cost is bounded per cache miss. Box averaging
// (not nearest) so a downscaled photo does not alias into noise.
func downscale(src image.Image, maxDim int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := fitBox(sw, sh, maxDim)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	if sw <= 0 || sh <= 0 {
		return dst
	}

	n := dw * dh
	var rs, gs, bs, as, cnt = make([]uint64, n), make([]uint64, n), make([]uint64, n), make([]uint64, n), make([]uint64, n)
	for y := 0; y < sh; y++ {
		ty := y * dh / sh
		rowBase := ty * dw
		for x := 0; x < sw; x++ {
			tx := x * dw / sw
			i := rowBase + tx
			r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA() // 16-bit, premultiplied
			rs[i] += uint64(r >> 8)
			gs[i] += uint64(g >> 8)
			bs[i] += uint64(bl >> 8)
			as[i] += uint64(a >> 8)
			cnt[i]++
		}
	}
	for i := 0; i < n; i++ {
		c := cnt[i]
		if c == 0 {
			c = 1 // a target cell no source pixel mapped to (only at extreme ratios) stays transparent
		}
		dst.Pix[i*4+0] = uint8(rs[i] / c)
		dst.Pix[i*4+1] = uint8(gs[i] / c)
		dst.Pix[i*4+2] = uint8(bs[i] / c)
		dst.Pix[i*4+3] = uint8(as[i] / c)
	}
	return dst
}

// fitBox returns the largest (w,h) within a maxDim×maxDim box that preserves the sw:sh aspect ratio,
// never upscaling. Both dimensions are clamped to a minimum of 1.
func fitBox(sw, sh, maxDim int) (int, int) {
	if sw <= maxDim && sh <= maxDim {
		return atLeast1(sw), atLeast1(sh)
	}
	if sw >= sh {
		return maxDim, atLeast1(sh * maxDim / sw)
	}
	return atLeast1(sw * maxDim / sh), maxDim
}

func atLeast1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// delete — DELETE /api/images/{id} (image:write): remove the row + blob, then write a synchronous
// image.delete audit row. An image still referenced by a playlist_item is a 409 (FK RESTRICT →
// ErrImageInUse); an unknown id is a uniform 404.
func (h imageHandlers) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := imageID(w, r)
	if !ok {
		return
	}
	pr, _ := adminhttp.PrincipalFrom(r.Context())

	err := imgstore.DeleteImage(r.Context(), h.pool, h.blobDir, id)
	switch {
	case errors.Is(err, imgstore.ErrNotFound):
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such image")
		return
	case errors.Is(err, imgstore.ErrImageInUse):
		adminhttp.WriteErr(w, r, http.StatusConflict, "image_in_use", "image is referenced by a playlist")
		return
	case err != nil:
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image delete failed")
		return
	}

	// Synchronous audit — a delete is a security-relevant mutation (design §4.6); a crash must not
	// swallow it. target is the image id (never a secret); actor + remote_ip from the resolved request.
	if err := adminaudit.Write(r.Context(), h.pool, adminaudit.Entry{
		ActorKind: string(pr.Kind), ActorID: pr.ID, Action: "image.delete",
		Target: strconv.FormatInt(id, 10), RemoteIP: adminhttp.RemoteIP(r), OK: true,
	}); err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image delete audit failed")
		return
	}

	// Render cache invalidation (design §4.2 DELETE / §6 / §9, critical merge point): a bild mutation
	// does NOT bump the bound fn.Version, so a render function reading the store dynamically would serve
	// a STALE hot-cache frame until TTL and keep the durable last-good indefinitely. The explicit
	// invalidation contract targets the A27-W3 frame_variant_cache — which does NOT exist yet (A27-W3
	// unmerged). Per the W5 brief no call is built on spec; this is the wiring point once W3 lands.
	adminhttp.WriteOK(w, r, map[string]any{"deleted": id})
}

// --- helpers ---

func imageMetaFields(img imgstore.Image) map[string]any {
	return map[string]any{
		"id":         img.ID,
		"sha256":     img.Sha256,
		"mime":       img.Mime,
		"width":      img.Width,
		"height":     img.Height,
		"byte_size":  img.ByteSize,
		"created_at": img.CreatedAt,
	}
}

// imageID parses {id} and writes a uniform 404 (no enumeration signal, design §4.2) on a non-positive
// or non-integer path value.
func imageID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such image")
		return 0, false
	}
	return id, true
}

// clampImageLimit parses ?limit into [1,200] with a 50 default (design §4.2 keyset page bound).
func clampImageLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// parseImageCursor parses ?after into a non-negative keyset cursor (0 = first page).
func parseImageCursor(raw string) int64 {
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// allowedImageMIME reports whether http.DetectContentType sniffs blob as an allowlisted image type
// (design §5 B5 layer a). DetectContentType reads only the first 512 bytes; the charset suffix (present
// on the text/* fallbacks a rejected upload sniffs as) is trimmed before the set membership test.
func allowedImageMIME(blob []byte) bool {
	ct := http.DetectContentType(blob)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return imageMIMEAllowlist[strings.TrimSpace(ct)]
}

// extForMIME maps a stored mime to the Content-Disposition filename extension.
func extForMIME(m string) string {
	switch m {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}
