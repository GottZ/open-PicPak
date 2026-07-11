package faascore

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
	"github.com/open-picpak/backend/internal/imgstore"
	"github.com/open-picpak/backend/internal/playliststore"
	"github.com/open-picpak/backend/internal/plrender"
)

// plPool opens the ephemeral DB (0013 applied) and truncates the render-anchor + source tables.
func plPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — playlist DB tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE frame_variant_cache, playlist_cursor, device_playlist_binding, device_render_binding,
		         playlist_item, playlist, image, faas_functions RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// plHarness seeds images through the production write path (imgstore.PutImage → real blob on disk) and
// wires a counting playlist-render stub that returns a per-image marker frame, so a served frame is
// traceable back to the item shown WITHOUT a live worker.
type plHarness struct {
	pool    *pgxpool.Pool
	blobDir string
	markers map[string]byte // string(blob) -> marker byte in the returned frame
	calls   int
	mu      sync.Mutex
	sleep   time.Duration // optional per-render delay to force single-flight overlap
}

func newPlHarness(t *testing.T) *plHarness {
	return &plHarness{pool: plPool(t), blobDir: t.TempDir(), markers: map[string]byte{}}
}

// render is the injected playlistRenderFunc: counts drives and returns a 30000-byte frame marked with
// the seeded per-image byte, so the caller can tell which image was rendered.
func (h *plHarness) render(_ context.Context, _ string, input []byte, _ string) RenderResult {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	if h.sleep > 0 {
		time.Sleep(h.sleep)
	}
	return RenderResult{Packed: bytes.Repeat([]byte{h.markers[string(input)]}, 30000), Meta: faasproto.ResponseMeta{V: 1, OK: true}}
}

func (h *plHarness) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *plHarness) supervisor() *Supervisor {
	return &Supervisor{
		pool:           h.pool,
		cache:          newFrameCache(),
		wake:           WakeConfig{NightStartHour: 23, NightEndHour: 6, DayInterval: 3600, MaxWake: 8 * 3600},
		renderTTL:      120 * time.Second,
		retryWake:      600,
		renderPlaylist: h.render,
		plFlight:       newSingleFlight(),
		imgBlobDir:     h.blobDir,
		jitterFrac:     0.15,
		rng:            func() float64 { return 0 }, // deterministic (no jitter) unless a test overrides
		render:         okRender(frameOf(0xFF)),     // fn-path stub (non-regression / gate d)
		lastRun:        map[int64]time.Time{},
	}
}

// seedImage writes a distinct PNG through the production store and records its marker byte.
func (h *plHarness) seedImage(t *testing.T, n int, marker byte) int64 {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: uint8(n), G: uint8(n * 7), B: uint8(n*13 + 1), A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png %d: %v", n, err)
	}
	blob := buf.Bytes()
	h.markers[string(blob)] = marker
	row, err := imgstore.PutImage(context.Background(), h.pool, h.blobDir, nil, blob)
	if err != nil {
		t.Fatalf("put image %d: %v", n, err)
	}
	return row.ID
}

func (h *plHarness) seedPlaylist(t *testing.T, mode string, imageIDs ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	// The pool is truncated per test, so a fixed lowercase name (ValidName-legal) is unique enough.
	pl, err := playliststore.CreatePlaylist(ctx, h.pool, playliststore.CreateParams{Name: "plr", OrderMode: mode})
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	for _, id := range imageIDs {
		if _, err := playliststore.AddItem(ctx, h.pool, pl.ID, playliststore.AddItemParams{ImageID: id}); err != nil {
			t.Fatalf("add item %d: %v", id, err)
		}
	}
	return pl.ID
}

// TestPlaylistRotationSequence is the rotation gate: consecutive /frame renders (spaced by the
// interval) serve image 1, 2, 3 in turn and wrap; within one interval the same image is held. Without
// the advance-gate every render would serve image 1 (or skip).
func TestPlaylistRotationSequence(t *testing.T) {
	h := newPlHarness(t)
	s := h.supervisor()
	ctx := context.Background()
	id1 := h.seedImage(t, 1, 0xA1)
	id2 := h.seedImage(t, 2, 0xB2)
	id3 := h.seedImage(t, 3, 0xC3)
	plID := h.seedPlaylist(t, "sequential", id1, id2, id3)
	if err := plrender.BindPlaylist(ctx, h.pool, "DEV", plID); err != nil {
		t.Fatalf("bind: %v", err)
	}

	interval := 900 * time.Second // playlist default
	t0 := time.Now()
	frameMarker := func(now time.Time) (byte, string) {
		packed, status, _, _ := s.buildPlaylistFrame(ctx, "DEV", plID, now)
		return packed[0], status
	}

	steps := []struct {
		now  time.Time
		want byte
	}{
		{t0, 0xA1},                               // fresh → image 1
		{t0.Add(time.Second), 0xA1},              // within interval → hold image 1
		{t0.Add(interval + time.Second), 0xB2},   // due → image 2
		{t0.Add(2*interval + time.Second), 0xC3}, // due → image 3
		{t0.Add(3*interval + time.Second), 0xA1}, // due → wrap to image 1
	}
	for i, st := range steps {
		got, status := frameMarker(st.now)
		if status != "ok" {
			t.Fatalf("step %d: status %s, want ok", i, status)
		}
		if got != st.want {
			t.Fatalf("step %d: served image marker %#x, want %#x (rotation advance-gate)", i, got, st.want)
		}
	}
}

// TestPlaylistVariantCacheHit is the variant-cache gate: a second render of the SAME variant serves
// from frame_variant_cache without driving the worker again (pack once, serve N).
func TestPlaylistVariantCacheHit(t *testing.T) {
	h := newPlHarness(t)
	s := h.supervisor()
	ctx := context.Background()
	id1 := h.seedImage(t, 1, 0x42)
	plID := h.seedPlaylist(t, "sequential", id1)
	if err := plrender.BindPlaylist(ctx, h.pool, "DEV", plID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	now := time.Now()
	p1, _, _, _ := s.buildPlaylistFrame(ctx, "DEV", plID, now)
	p2, _, _, _ := s.buildPlaylistFrame(ctx, "DEV", plID, now) // same interval → same item → cache hit
	if h.callCount() != 1 {
		t.Fatalf("worker driven %d times, want 1 (variant-cache hit)", h.callCount())
	}
	if !bytes.Equal(p1, p2) || p1[0] != 0x42 {
		t.Fatalf("cache-hit frame mismatch: eq=%v marker=%#x", bytes.Equal(p1, p2), p1[0])
	}
}

// TestPlaylistSingleFlight is the cache-stampede gate: N concurrent renders of a COLD variant collapse
// to ONE worker drive (the in-flight dedup), not N (§4.3 step 4). Isolated at packedVariant so the
// cursor advance does not perturb which variant is contended.
func TestPlaylistSingleFlight(t *testing.T) {
	h := newPlHarness(t)
	h.sleep = 30 * time.Millisecond // hold the leader so the concurrent callers pile onto the flight
	s := h.supervisor()
	ctx := context.Background()
	id1 := h.seedImage(t, 1, 0x77)
	img, err := imgstore.GetImage(ctx, h.pool, id1)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	item := playliststore.PlaylistItem{ImageID: id1, Fit: "cover", Dither: "none"}

	const N = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	frames := make([][]byte, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			frames[i], errs[i] = s.packedVariant(ctx, img, item)
		}(i)
	}
	close(start)
	wg.Wait()

	if h.callCount() != 1 {
		t.Fatalf("cold variant drove the worker %d times under %d concurrent misses, want 1 (single-flight)", h.callCount(), N)
	}
	for i := 0; i < N; i++ {
		if errs[i] != nil || len(frames[i]) != 30000 || frames[i][0] != 0x77 {
			t.Fatalf("caller %d: err=%v len=%d marker=%#x", i, errs[i], len(frames[i]), frames[i][0])
		}
	}
}

// TestPlaylistWakeJitter is the K12 gate: the next-wake is spread per device (deterministically testable
// via an injected rng) and clamped to [60, MaxWake], and it never exceeds the intended cadence/advance
// boundary (jitter only pulls earlier).
func TestPlaylistWakeJitter(t *testing.T) {
	s := &Supervisor{wake: WakeConfig{DayInterval: 3600, MaxWake: 8 * 3600}, jitterFrac: 0.15}

	s.rng = func() float64 { return 0 }
	if got := s.jitter(1000); got != 1000 {
		t.Fatalf("jitter with rng=0 = %d, want 1000 (no reduction)", got)
	}
	s.rng = func() float64 { return 1 }
	if got := s.jitter(1000); got != 850 {
		t.Fatalf("jitter with rng=1 = %d, want 850 (max 15%% earlier)", got)
	}
	// Across the rng range the jittered value stays within [base*0.85, base] — never later, never
	// below the design fraction.
	for _, r := range []float64{0, 0.25, 0.5, 0.9, 1} {
		s.rng = func() float64 { return r }
		got := s.jitter(1000)
		if got < 850 || got > 1000 {
			t.Fatalf("jitter(1000) with rng=%v = %d, out of [850,1000]", r, got)
		}
	}

	// playlistWake picks the sooner of cadence and time-to-next-advance, then clamps to >= 60.
	s.rng = func() float64 { return 0 }
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) // daytime → cadence 3600
	cur := plrender.Cursor{AdvancedAt: now}              // just advanced → toNext = interval
	if got := s.playlistWake(now, 300*time.Second, cur); got != 300 {
		t.Fatalf("playlistWake: %d, want 300 (interval < cadence, no jitter)", got)
	}
	// A tiny remaining interval still clamps up to the 60s wake floor.
	cur.AdvancedAt = now.Add(-299 * time.Second) // 1s left on a 300s interval
	if got := s.playlistWake(now, 300*time.Second, cur); got != 60 {
		t.Fatalf("playlistWake near-boundary: %d, want 60 (wake floor)", got)
	}
}

// TestHandleRenderPlaylistFirst is gate (d) + the non-regression: with BOTH a render binding and a
// playlist binding artificially present, the supervisor serves the PLAYLIST frame (playlist-first);
// an fn-only device still serves the fn frame (existing path unchanged by the ResolveBinding prefix).
func TestHandleRenderPlaylistFirst(t *testing.T) {
	h := newPlHarness(t)
	s := h.supervisor()
	ctx := context.Background()

	// fn-only device: the render path is unchanged — serves the fn stub frame (0xFF). Enabled so the
	// sync path renders inline (K15: a disabled function is frozen and would serve last-good/error).
	fnID, err := faasstore.Create(ctx, h.pool, faasstore.CreateParams{
		Name: "fn", Source: "x", TriggerType: faasstore.TriggerRender, TriggerConfig: json.RawMessage(`{"mode":"sync"}`)})
	if err != nil {
		t.Fatalf("create fn: %v", err)
	}
	if _, err := faasstore.SetEnabled(ctx, h.pool, fnID, true); err != nil {
		t.Fatalf("enable fn: %v", err)
	}
	if err := faasstore.BindDevice(ctx, h.pool, "FN-ONLY", fnID); err != nil {
		t.Fatalf("bind fn: %v", err)
	}
	if marker := postRender(t, s, "FN-ONLY"); marker != 0xFF {
		t.Fatalf("fn-only device served marker %#x, want 0xFF (fn path non-regression)", marker)
	}

	// double-row device: force both bindings (bypassing the advisory-locked exclusion) and prove the
	// supervisor dispatches the PLAYLIST, never the fn.
	id1 := h.seedImage(t, 9, 0x5A)
	plID := h.seedPlaylist(t, "sequential", id1)
	if _, err := h.pool.Exec(ctx, `INSERT INTO device_render_binding (serial, function_id) VALUES ('BOTH',$1)`, fnID); err != nil {
		t.Fatalf("seed render binding: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO device_playlist_binding (serial, playlist_id) VALUES ('BOTH',$1)`, plID); err != nil {
		t.Fatalf("seed playlist binding: %v", err)
	}
	if marker := postRender(t, s, "BOTH"); marker != 0x5A {
		t.Fatalf("double-row device served marker %#x, want 0x5A (playlist-first, never fn)", marker)
	}
}

// postRender drives handleRender via httptest and returns the served frame's first (marker) byte.
func postRender(t *testing.T, s *Supervisor, serial string) byte {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"serial": serial})
	req := httptest.NewRequest(http.MethodPost, "/render", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleRender(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("render %s: status %d", serial, rec.Code)
	}
	out := rec.Body.Bytes()
	if len(out) != 30000 {
		t.Fatalf("render %s: body %d bytes, want 30000", serial, len(out))
	}
	return out[0]
}
