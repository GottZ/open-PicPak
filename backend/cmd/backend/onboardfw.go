package main

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
)

// onboardFWHandler serves the open-picpak CFW flash artifacts (manifest + bin parts) for the /onboard page's
// WebSerial flash (Design 26 §4.7 / D26.9). Served same-origin (so connect-src 'self' holds) and READ-ONLY,
// out of the binary by default: ADMIN_ONBOARD_FW_DIR unset → the route 404s a "not configured" notice while
// the page's flash-from-local-file + backup/restore still work.
//
// cmd/admin is the secret/rollout write zone (M5), so the mount is TRAVERSAL-SAFE by construction: os.DirFS +
// io/fs.ValidPath reject any ".." element and absolute/rooted paths, and no directory listing is ever emitted
// (404 on a bare-dir request, never an index). http.Dir is deliberately NOT used (it roots on a string prefix
// and has a documented edge-case history); os.DirFS is the escape-proof choice. Auth-gated like every admin
// route; the local flash steps need only a valid operator login (D26.8), so no RequireAdmin here.
type onboardFWHandler struct {
	dir string
}

const onboardFWPrefix = "/onboard-fw/"

func registerOnboardFWRoutes(mux *http.ServeMux, pool *pgxpool.Pool, dir string) {
	h := onboardFWHandler{dir: dir}
	mux.Handle("GET "+onboardFWPrefix, adminhttp.Auth(pool)(http.HandlerFunc(h.serve)))
}

func (h onboardFWHandler) serve(w http.ResponseWriter, r *http.Request) {
	if h.dir == "" {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "onboard_fw_unconfigured",
			"firmware artifacts not configured (ADMIN_ONBOARD_FW_DIR unset)")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, onboardFWPrefix)
	// io/fs.ValidPath rejects "", any ".." element, and absolute/rooted paths — escape-proof. A traversal or
	// bare-name request therefore serves no byte of the parent tree (F10).
	if name == "" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	f, err := os.DirFS(h.dir).Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil || info.IsDir() { // no directory listing — 404 a dir (incl. ".", handled here)
		http.NotFound(w, r)
		return
	}
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, info.ModTime(), rs)
		return
	}
	// Non-seekable fs.File fallback: artifacts are small (manifest + bin parts), so a full read is fine.
	data, err := io.ReadAll(f)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "artifact read failed")
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(data))
}
