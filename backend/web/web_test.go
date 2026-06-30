package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// fixtureFS mimics a real Vite build output: hashed assets with .br/.gz
// siblings, an unhashed public/ file, and the index.html entry.
func fixtureFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html><html><body>picpak-spa-entry</body></html>")},
		"favicon.svg":             {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
		"assets/app-abc123.js":    {Data: []byte("console.log('original-js')")},
		"assets/app-abc123.js.br": {Data: []byte("br-compressed-payload")},
		"assets/app-abc123.js.gz": {Data: []byte("gz-compressed-payload")},
		"assets/plain-xyz.css":    {Data: []byte(".plain{color:red}")},
	}
}

// placeholderFS models a build without frontend: dist/ carries only .gitkeep.
func placeholderFS() fstest.MapFS {
	return fstest.MapFS{".gitkeep": {Data: []byte("")}}
}

func doReq(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// T1 — TestSPAFallbackAcceptGuard: only HTML navigations get the history-API
// fallback; mistyped API URLs stay 404 so JSON clients never parse 200+HTML.
func TestSPAFallbackAcceptGuard(t *testing.T) {
	h := handlerFor(fixtureFS())

	tests := []struct {
		name       string
		target     string
		accept     string
		wantStatus int
		wantSPA    bool
	}{
		{"html navigation to SPA route", "/settings", "text/html,application/xhtml+xml", http.StatusOK, true},
		{"deep link", "/logs/D2XXXXR", "text/html", http.StatusOK, true},
		{"json client typo", "/api/missing", "application/json", http.StatusNotFound, false},
		{"wildcard accept (curl)", "/api/qery", "*/*", http.StatusNotFound, false},
		{"no accept header", "/noexist.js", "", http.StatusNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tt.accept != "" {
				hdr["Accept"] = tt.accept
			}
			rec := doReq(t, h, http.MethodGet, tt.target, hdr)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			gotSPA := strings.Contains(rec.Body.String(), "picpak-spa-entry")
			if gotSPA != tt.wantSPA {
				t.Errorf("SPA body served = %v, want %v (body %q)", gotSPA, tt.wantSPA, rec.Body.String())
			}
		})
	}
}

// T2 (cache half) — TestCacheControl proves the handler sets cache headers
// outright (cmd/admin has no global no-store middleware to override): hashed
// assets become immutable, unhashed entry points become no-cache. Negative
// probe: drop the Set("Cache-Control", ...) lines in handlerFor → empty value.
func TestCacheControl(t *testing.T) {
	h := handlerFor(fixtureFS())

	tests := []struct {
		target string
		want   string
	}{
		{"/assets/app-abc123.js", "public, max-age=31536000, immutable"},
		{"/index.html", "no-cache"},
		{"/favicon.svg", "no-cache"},
		{"/", "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			rec := doReq(t, h, http.MethodGet, tt.target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.want {
				t.Errorf("Cache-Control = %q, want %q", got, tt.want)
			}
		})
	}
}

// T3 — TestPreCompressedEncoding: .br/.gz selection by Accept-Encoding, original
// Content-Type, Vary on anything with compressed siblings.
func TestPreCompressedEncoding(t *testing.T) {
	h := handlerFor(fixtureFS())

	tests := []struct {
		name         string
		target       string
		acceptEnc    string
		wantBody     string
		wantEncoding string
		wantVary     bool
	}{
		{"brotli preferred", "/assets/app-abc123.js", "gzip, deflate, br", "br-compressed-payload", "br", true},
		{"gzip only", "/assets/app-abc123.js", "gzip", "gz-compressed-payload", "gzip", true},
		{"identity client", "/assets/app-abc123.js", "", "console.log('original-js')", "", true},
		{"no compressed sibling", "/assets/plain-xyz.css", "gzip, br", ".plain{color:red}", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tt.acceptEnc != "" {
				hdr["Accept-Encoding"] = tt.acceptEnc
			}
			rec := doReq(t, h, http.MethodGet, tt.target, hdr)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Encoding"); got != tt.wantEncoding {
				t.Errorf("Content-Encoding = %q, want %q", got, tt.wantEncoding)
			}
			ct := rec.Header().Get("Content-Type")
			if ct == "application/octet-stream" || ct == "" {
				t.Errorf("Content-Type = %q, want the ORIGINAL file's type", ct)
			}
			if strings.HasSuffix(tt.target, ".js") && !strings.Contains(ct, "javascript") {
				t.Errorf("Content-Type = %q, want a javascript type", ct)
			}
			gotVary := strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding")
			if gotVary != tt.wantVary {
				t.Errorf("Vary Accept-Encoding = %v, want %v", gotVary, tt.wantVary)
			}
		})
	}
}

// T2 (CSP half) — TestCSPOnHTMLOnly: HTML responses carry the CSP, asset
// responses do not. Negative probe: drop the *.html CSP Set → assertion fails.
func TestCSPOnHTMLOnly(t *testing.T) {
	h := handlerFor(fixtureFS())

	tests := []struct {
		name    string
		target  string
		accept  string
		wantCSP bool
	}{
		{"root entry", "/", "", true},
		{"explicit index.html", "/index.html", "", true},
		{"spa fallback", "/settings", "text/html", true},
		{"hashed asset", "/assets/app-abc123.js", "", false},
		{"public file", "/favicon.svg", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tt.accept != "" {
				hdr["Accept"] = tt.accept
			}
			rec := doReq(t, h, http.MethodGet, tt.target, hdr)
			got := rec.Header().Get("Content-Security-Policy")
			if tt.wantCSP && got != csp {
				t.Errorf("CSP = %q, want the package csp constant", got)
			}
			if !tt.wantCSP && got != "" {
				t.Errorf("CSP = %q on non-HTML response, want none", got)
			}
		})
	}
}

// TestCSPHasNoWorkerSrc pins D19.10/O3: D19 ships tight, no worker-src is
// granted here — a feature doc that needs it (Doc 25/26) widens the CSP in its
// own doc after measuring. Red if a worker-src directive creeps back in.
func TestCSPHasNoWorkerSrc(t *testing.T) {
	if strings.Contains(csp, "worker-src") {
		t.Errorf("csp contains worker-src; D19 ships tight (O3) — widen in the feature doc that needs it, not here")
	}
}

// T4 — TestPlaceholderWithoutBuild: dist/ with only .gitkeep (go install / CI
// without bun) serves a 503 hint on HTML navigations and keeps non-HTML
// requests at 404 — and never panics. Proves D19.3.
func TestPlaceholderWithoutBuild(t *testing.T) {
	h := handlerFor(placeholderFS())

	rec := doReq(t, h, http.MethodGet, "/", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UI not built") {
		t.Errorf("body = %q, want the placeholder hint", rec.Body.String())
	}

	rec = doReq(t, h, http.MethodGet, "/settings", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("SPA route status = %d, want 503", rec.Code)
	}

	rec = doReq(t, h, http.MethodGet, "/api/anything", map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("API typo status = %d, want 404", rec.Code)
	}

	rec = doReq(t, h, http.MethodGet, "/", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-HTML / status = %d, want 404", rec.Code)
	}
}

// TestEmbeddedDistHandler exercises Handler() against the REAL embedded dist. It
// is build-state agnostic: with the committed .gitkeep placeholder it answers
// 503, after a frontend build 200 — never panic, always no-cache on the entry.
func TestEmbeddedDistHandler(t *testing.T) {
	h := Handler()

	rec := doReq(t, h, http.MethodGet, "/", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 200 (built) or 503 (placeholder)", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

// TestNoSPAFallbackForMutations: POST and friends never reach the SPA.
func TestNoSPAFallbackForMutations(t *testing.T) {
	h := handlerFor(fixtureFS())

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doReq(t, h, method, "/", map[string]string{"Accept": "text/html"})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s / status = %d, want 404", method, rec.Code)
		}
	}
}

// TestPathTraversalStays404: cleaned/invalid paths cannot escape dist.
func TestPathTraversalStays404(t *testing.T) {
	h := handlerFor(fixtureFS())

	for _, target := range []string{"/../web.go", "/..%2fweb.go", "/assets/../../go.mod"} {
		req := httptest.NewRequest(http.MethodGet, "http://x", nil)
		req.URL.Path = strings.ReplaceAll(target, "%2f", "/")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "picpak-spa-entry") {
			t.Errorf("GET %s served a non-SPA file (status %d)", target, rec.Code)
		}
	}
}
