package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

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
