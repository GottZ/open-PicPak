package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/bwry"
	"github.com/open-picpak/backend/internal/templatestore"
)

// template_preview_http.go is the W-A33.5a preview backend (design 33 §4.5a): the gallery card's
// preview image. A render_fn card loads GET /api/templates/{id}/preview; the route serves a durable,
// (template_id, version)-keyed PNG from template_preview (0019), ETag/304 + immutable-cached. Builtins
// are warmed at startup with a SINGLE fleet-slot budget so a gallery cold-visit never contends the 4
// shared faas worker slots with the /frame path (§6). The preview render profile is fail-CLOSED:
// EgressAllow=[] is set EXPLICITLY on the seam (not inherited from the function — testrender forwards
// the function's allowlist, testrender.go:99/131; an empty list ⇒ deny, faas-egress decide()), and
// secrets are STUBBED by the supervisor test-render arm (testrender.go:123). Fetch builtins whose live
// render would egress-deny to the error frame carry a curated preview_fixture PNG instead (§4.5a).

// previewRenderTimeout bounds one preview render (worker cold-start ~18 s; a generous outer bound,
// parity testRunTimeout). The warmup walks builtins one at a time under this bound.
const previewRenderTimeout = 40 * time.Second

// previewRenderer produces preview PNGs for templates. The single-element `sem` is the fleet-slot
// budget (§4.5a): EVERY live preview render — warmup and on-demand alike — acquires it, so previews
// never occupy more than one of the 4 shared faas worker slots at a time, regardless of how many
// admins browse the gallery cold at once. A fixture serve skips the render (and the slot) entirely.
// `render` is the packed-frame producer (the supervisor test-render seam in production, injectable in
// tests); nil `render` ⇒ no live render is possible (FAAS_TEST_SOCK unset), previews degrade.
type previewRenderer struct {
	pool     *pgxpool.Pool
	sem      chan struct{}                                 // cap 1 — the single fleet-slot budget
	fixtures map[string][]byte                             // builtin name → curated preview PNG
	render   func(ctx context.Context, source string) ([]byte, error) // packed-frame producer
}

// errPreviewUnavailable is returned when a live preview render cannot run (no seam / seam error). The
// handler maps it to a 404 so the card's <img onerror> degrades to the neutral "no preview" note.
var errPreviewUnavailable = errors.New("preview render unavailable")

// produce returns the preview PNG for a template. A builtin with a curated fixture serves the fixture
// bytes verbatim (no render, no slot). Otherwise a render_fn is live-rendered under the single-slot
// budget; a non-render_fn has no frame preview. The slot is held only across the actual render.
func (p *previewRenderer) produce(ctx context.Context, name, kind, source string) ([]byte, error) {
	if fx, ok := p.fixtures[name]; ok {
		return fx, nil // curated example — the live-render (egress-deny) path is deliberately skipped
	}
	if kind != templatestore.KindRenderFn {
		return nil, errPreviewUnavailable // only render_fn produces a frame
	}
	if p.render == nil {
		return nil, errPreviewUnavailable
	}
	p.sem <- struct{}{} // acquire the ONE slot — blocks a concurrent preview render (never >1 in flight)
	defer func() { <-p.sem }()
	packed, err := p.render(ctx, source)
	if err != nil {
		return nil, err
	}
	return templatestore.PackedToPNG(packed)
}

// renderAndStore produces the preview and writes it into the durable cache (best-effort — a cache-write
// failure still serves the freshly rendered bytes; the next request re-renders).
func (p *previewRenderer) renderAndStore(ctx context.Context, t *templatestore.Template) ([]byte, error) {
	png, err := p.produce(ctx, t.Name, t.Kind, t.Source)
	if err != nil {
		return nil, err
	}
	if serr := templatestore.StorePreview(ctx, p.pool, t.ID, t.Version, png); serr != nil {
		log.Printf("templatestore: WARN preview cache write for %q v%d failed: %v", t.Name, t.Version, serr)
	}
	return png, nil
}

// warmup pre-renders every builtin render_fn preview into template_preview at startup, sequentially and
// under the single-slot budget (§4.5a / §6). It is idempotent across restarts: a (id, version) already
// cached is skipped, so a deploy on an unchanged catalog re-renders nothing. Fail-open: a per-builtin
// render error is logged and the loop continues — the missing card degrades, the rest warm.
func (p *previewRenderer) warmup(ctx context.Context) {
	targets, err := templatestore.BuiltinPreviewTargets(ctx, p.pool)
	if err != nil {
		log.Printf("templatestore: WARN preview warmup skipped, target query failed: %v", err)
		return
	}
	var warmed, skipped, failed int
	for _, t := range targets {
		if _, err := templatestore.LoadPreview(ctx, p.pool, t.ID, t.Version); err == nil {
			skipped++
			continue // already cached at this version — idempotent no-op
		}
		png, err := p.produce(ctx, t.Name, templatestore.KindRenderFn, t.Source)
		if err != nil {
			failed++
			log.Printf("templatestore: WARN preview warmup for %q v%d failed: %v", t.Name, t.Version, err)
			continue
		}
		if err := templatestore.StorePreview(ctx, p.pool, t.ID, t.Version, png); err != nil {
			failed++
			log.Printf("templatestore: WARN preview warmup cache write for %q v%d failed: %v", t.Name, t.Version, err)
			continue
		}
		warmed++
	}
	log.Printf("templatestore: preview warmup done (warmed=%d skipped=%d failed=%d)", warmed, skipped, failed)
}

type templatePreviewHandlers struct {
	pool *pgxpool.Pool
	rndr *previewRenderer
}

// registerTemplatePreviewRoutes mounts GET /api/templates/{id}/preview and kicks off the builtin
// warmup. The route is Auth-gated (any valid key) — a builtin preview is any-read (E-A33-6); the
// handler additionally requires admin for an operator-authored template's on-demand render (admin-only,
// E-A33-6). Single call from main.go's template block keeps that file's change to one line.
func registerTemplatePreviewRoutes(ctx context.Context, mux *http.ServeMux, pool *pgxpool.Pool, testSock string) {
	fixtures, err := templatestore.PreviewFixtures()
	if err != nil {
		log.Printf("templatestore: WARN preview fixtures unavailable (fetch-builtin cards will degrade): %v", err)
		fixtures = map[string][]byte{}
	}
	rndr := &previewRenderer{
		pool:     pool,
		sem:      make(chan struct{}, 1), // the single fleet-slot budget
		fixtures: fixtures,
		render:   newPreviewSeamRender(testSock),
	}
	h := templatePreviewHandlers{pool: pool, rndr: rndr}
	mux.Handle("GET /api/templates/{id}/preview", adminhttp.Auth(pool)(http.HandlerFunc(h.preview)))
	go rndr.warmup(ctx)
}

// preview — GET /api/templates/{id}/preview (auth): the gallery card image. builtins are any-read;
// an operator-authored template's preview is admin-only (its on-demand render is admin-gated, E-A33-6).
// Serves the durable cache; a miss for a render_fn renders on-demand under the single-slot budget.
func (h templatePreviewHandlers) preview(w http.ResponseWriter, r *http.Request) {
	id, ok := templateID(w, r)
	if !ok {
		return
	}
	t, err := templatestore.Load(r.Context(), h.pool, id)
	if errors.Is(err, templatestore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such template")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "template load failed")
		return
	}
	// Operator-authored previews are admin-only (E-A33-6): the on-demand render is admin-gated, so the
	// serve is too (a read-only key never triggers an operator render, never sees the frame).
	if !t.Builtin {
		if pr, ok := adminhttp.PrincipalFrom(r.Context()); !ok || !pr.IsAdmin {
			adminhttp.WriteErr(w, r, http.StatusForbidden, "forbidden",
				"operator-authored template previews are admin-only")
			return
		}
	}

	png, err := templatestore.LoadPreview(r.Context(), h.pool, id, t.Version)
	if errors.Is(err, templatestore.ErrNotFound) {
		png, err = h.rndr.renderAndStore(r.Context(), t)
		if errors.Is(err, errPreviewUnavailable) {
			adminhttp.WriteErr(w, r, http.StatusNotFound, "no_preview", "no preview available for this template")
			return
		}
		if err != nil {
			adminhttp.WriteErr(w, r, http.StatusBadGateway, "preview_render_failed", "preview render failed")
			return
		}
	} else if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "preview load failed")
		return
	}

	// ETag is (id, version): the version bumps on any real template change (seed refresh / operator
	// edit), so the immutable cache is safe — a bumped version is a new URL (?v=) AND a new ETag.
	etag := fmt.Sprintf(`"%d-%d"`, id, t.Version)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified) // 304, no body — the browser reuses its cached PNG
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// newPreviewSeamRender builds the packed-frame producer over the supervisor test-render arm
// (FAAS_TEST_SOCK). The seam sets egress_allow=[] EXPLICITLY (fail-closed — not the template's own
// allowlist) and trigger=render; the supervisor stubs secrets. Returns nil when the socket is unset
// (no live render possible — previews degrade, pausability-safe like the test-run arm).
func newPreviewSeamRender(sock string) func(context.Context, string) ([]byte, error) {
	if sock == "" {
		return nil
	}
	client := &http.Client{
		Timeout: previewRenderTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
	return func(ctx context.Context, source string) ([]byte, error) {
		seam, _ := json.Marshal(map[string]any{
			"source":       source,
			"egress_allow": []string{}, // EXPLICIT empty — fail-closed egress (deny), never the template's list
			"trigger":      "render",
			"serial":       "preview0", // synthetic ctx.serial; the preview is serial-invariant
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://faas/test-render", bytes.NewReader(seam))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errPreviewUnavailable, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%w: supervisor status %d", errPreviewUnavailable, resp.StatusCode)
		}
		return parsePackedFromTestFrame(resp.Body)
	}
}

// parsePackedFromTestFrame extracts the 30000-byte packed frame from the supervisor's framed test-render
// response (u32be metaLen | meta JSON | packed(30000) | raw). The supervisor already substitutes the
// error frame into the packed slot on a render error (testrender.go:156), so a fetch-builtin that
// egress-denies here yields the error-frame PNG — exactly the fixture-less fetch-builtin behaviour.
func parsePackedFromTestFrame(body io.Reader) ([]byte, error) {
	all, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return nil, err
	}
	if len(all) < 4 {
		return nil, fmt.Errorf("preview: framed response too short (%d bytes)", len(all))
	}
	metaLen := int(binary.BigEndian.Uint32(all[:4]))
	start := 4 + metaLen
	if start+bwry.PackedSize > len(all) {
		return nil, fmt.Errorf("preview: framed response missing packed frame (len=%d metaLen=%d)", len(all), metaLen)
	}
	return all[start : start+bwry.PackedSize], nil
}
