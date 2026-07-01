package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnboardFWServe_TraversalSafe is the F10 negatively-probed gate (Design 26 D26.9): the /onboard-fw/
// mount must serve configured artifacts but never a byte of the parent tree, and never a directory listing.
func TestOnboardFWServe_TraversalSafe(t *testing.T) {
	root := t.TempDir()
	fwDir := filepath.Join(root, "fw")
	if err := os.MkdirAll(filepath.Join(fwDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fwDir, "manifest.json"), []byte(`{"name":"picpak-cfw"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fwDir, "bin", "app.bin"), []byte("APPBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	const secret = "TOPSECRETKEY"
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	h := onboardFWHandler{dir: fwDir}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.serve(rec, req)
		return rec
	}

	// happy path: configured artifacts serve.
	if rec := get("/onboard-fw/manifest.json"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "picpak-cfw") {
		t.Fatalf("manifest: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec := get("/onboard-fw/bin/app.bin"); rec.Code != http.StatusOK || rec.Body.String() != "APPBYTES" {
		t.Fatalf("bin: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// traversal → 404, never the secret bytes.
	for _, p := range []string{
		"/onboard-fw/../secret.txt",
		"/onboard-fw/../../secret.txt",
		"/onboard-fw/%2e%2e/secret.txt",
		"/onboard-fw/bin/../../secret.txt",
		"/onboard-fw//etc/hostname",
	} {
		rec := get(p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("traversal %q: code=%d, want 404", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("traversal %q leaked the parent-tree secret", p)
		}
	}

	// bare-dir requests → 404, never a listing.
	for _, p := range []string{"/onboard-fw/", "/onboard-fw/bin/", "/onboard-fw/."} {
		if rec := get(p); rec.Code != http.StatusNotFound {
			t.Errorf("dir %q: code=%d, want 404 (no listing)", p, rec.Code)
		}
	}
}

// TestOnboardFWServe_Unconfigured: default-off (ADMIN_ONBOARD_FW_DIR unset) → a legible 404, not a 500.
func TestOnboardFWServe_Unconfigured(t *testing.T) {
	h := onboardFWHandler{dir: ""}
	req := httptest.NewRequest(http.MethodGet, "/onboard-fw/manifest.json", nil)
	rec := httptest.NewRecorder()
	h.serve(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unconfigured") {
		t.Fatalf("body=%q, want the unconfigured notice", rec.Body.String())
	}
}
