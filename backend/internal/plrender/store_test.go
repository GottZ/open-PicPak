package plrender

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/faasstore"
	"github.com/open-picpak/backend/internal/imgstore"
	"github.com/open-picpak/backend/internal/playliststore"
)

// DB property tests — skipped unless TEST_DATABASE_URL is set (run in the e2e gate against an
// ephemeral postgres with migrations 0001..0013 applied). Mirrors imgstore/playliststore/faasstore.
func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — DB property tests skipped")
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

// mkFn creates a minimal render function (for the fn-binding side of the mutual-exclusion test).
func mkFn(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	id, err := faasstore.Create(context.Background(), pool, faasstore.CreateParams{
		Name: "fn1", Source: "x", TriggerType: faasstore.TriggerRender,
	})
	if err != nil {
		t.Fatalf("create fn: %v", err)
	}
	return id
}

// TestResolveBinding proves the one-round-trip LEFT JOIN reports each binding kind, and none for an
// unbound serial (the fn-bound/unbound majority non-regression: the playlist join adds no false hit).
func TestResolveBinding(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	fnID := mkFn(t, pool)

	// unbound serial → both zero.
	b, err := ResolveBinding(ctx, pool, "S-NONE")
	if err != nil || b.PlaylistID != 0 || b.FunctionID != 0 {
		t.Fatalf("unbound: %+v err=%v", b, err)
	}

	// fn-bound serial → FunctionID only (existing render path unchanged).
	if err := faasstore.BindDevice(ctx, pool, "S-FN", fnID); err != nil {
		t.Fatalf("bind fn: %v", err)
	}
	if b, err = ResolveBinding(ctx, pool, "S-FN"); err != nil || b.FunctionID != fnID || b.PlaylistID != 0 {
		t.Fatalf("fn-bound: %+v err=%v", b, err)
	}

	// playlist-bound serial → PlaylistID only.
	plID := seedPlaylist(t, pool)
	if err := BindPlaylist(ctx, pool, "S-PL", plID); err != nil {
		t.Fatalf("bind playlist: %v", err)
	}
	if b, err = ResolveBinding(ctx, pool, "S-PL"); err != nil || b.PlaylistID != plID || b.FunctionID != 0 {
		t.Fatalf("playlist-bound: %+v err=%v", b, err)
	}
}

// seedPlaylist inserts a bare playlist row and returns its id (no items needed for binding tests).
func seedPlaylist(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO playlist (name) VALUES ($1) RETURNING id`, "pl-"+t.Name()).Scan(&id); err != nil {
		t.Fatalf("seed playlist: %v", err)
	}
	return id
}

// TestBindPlaylistMutualExclusion proves a serial never holds a function AND a playlist binding at
// once: each bind path deletes the other row under the advisory lock (§3/§5). Gate (d), positive half.
func TestBindPlaylistMutualExclusion(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	fnID := mkFn(t, pool)
	plID := seedPlaylist(t, pool)

	// fn → playlist: binding the playlist drops the render binding + primes no cursor row.
	if err := faasstore.BindDevice(ctx, pool, "SN", fnID); err != nil {
		t.Fatalf("bind fn: %v", err)
	}
	if err := BindPlaylist(ctx, pool, "SN", plID); err != nil {
		t.Fatalf("bind playlist: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM device_render_binding WHERE serial='SN'`); n != 0 {
		t.Fatalf("render binding survived a playlist bind: %d rows", n)
	}
	b, _ := ResolveBinding(ctx, pool, "SN")
	if b.PlaylistID != plID || b.FunctionID != 0 {
		t.Fatalf("after fn→playlist: %+v", b)
	}

	// playlist → fn: binding the function drops the playlist binding + its cursor.
	if _, err := pool.Exec(ctx, `INSERT INTO playlist_cursor (serial, playlist_id) VALUES ('SN',$1)`, plID); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	if err := faasstore.BindDevice(ctx, pool, "SN", fnID); err != nil {
		t.Fatalf("re-bind fn: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM device_playlist_binding WHERE serial='SN'`); n != 0 {
		t.Fatalf("playlist binding survived a fn bind: %d rows", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM playlist_cursor WHERE serial='SN'`); n != 0 {
		t.Fatalf("cursor survived a fn bind: %d rows", n)
	}
	b, _ = ResolveBinding(ctx, pool, "SN")
	if b.FunctionID != fnID || b.PlaylistID != 0 {
		t.Fatalf("after playlist→fn: %+v", b)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestResolveCursorSingleAdvance is the double-advance gate (§4.4). RED first: two concurrent
// UNGUARDED advances (a plain SQL position=position+1, the shape the code would have WITHOUT the
// advisory lock + advanced_at gate) skip a step — Postgres serialises the two UPDATEs on the row
// lock, so the second reads the first's committed value and increments again (0→1→2). GREEN: the
// production ResolveCursor, run at the SAME concurrency, advances EXACTLY once (0→1) because the
// per-serial advisory lock serialises the read→decide→write and the second caller sees the fresh
// advanced_at and holds.
func TestResolveCursorSingleAdvance(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	const n = 3
	interval := 900 * time.Second
	t0 := time.Now()

	// --- RED: unguarded concurrent advance skips a step ---
	if _, err := pool.Exec(ctx,
		`INSERT INTO playlist_cursor (serial, playlist_id, position, advanced_at) VALUES ('R',1,0,$1)`,
		t0.Add(-interval-time.Second)); err != nil { // advanced_at in the past → an advance is due
		t.Fatalf("seed red cursor: %v", err)
	}
	racyAdvance := func() {
		_, _ = pool.Exec(ctx, `UPDATE playlist_cursor SET position = position + 1, advanced_at = now() WHERE serial='R'`)
	}
	runConcurrent(2, racyAdvance)
	if pos := cursorPos(t, pool, "R"); pos != 2 {
		t.Fatalf("RED probe expected a double-advance skip to 2 (proves the hazard the lock guards), got %d", pos)
	}

	// --- GREEN: production ResolveCursor advances exactly once under the same concurrency ---
	// Prime the cursor at step 0 (fresh), then backdate advanced_at so the next resolve is due.
	if _, err := ResolveCursor(ctx, pool, "G", 1, interval, n, t0); err != nil {
		t.Fatalf("prime green cursor: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE playlist_cursor SET advanced_at=$1 WHERE serial='G'`,
		t0.Add(-interval-time.Second)); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	var (
		mu       sync.Mutex
		advances int
	)
	runConcurrent(20, func() {
		cur, err := ResolveCursor(ctx, pool, "G", 1, interval, n, t0)
		if err != nil {
			t.Errorf("resolve: %v", err)
			return
		}
		if cur.Advanced {
			mu.Lock()
			advances++
			mu.Unlock()
		}
	})
	if advances != 1 {
		t.Fatalf("GREEN: %d callers advanced under concurrency, want exactly 1 (single-advance)", advances)
	}
	if pos := cursorPos(t, pool, "G"); pos != 1 {
		t.Fatalf("GREEN: cursor at %d after one due-window, want 1 (no skip)", pos)
	}
}

// TestResolveCursorSequence proves the advance-gate: within one interval the same step is held; only
// once the interval elapses does the cursor step forward, wrapping at n (§4.4). Without the gate every
// /frame would advance (or every /frame would hold step 0).
func TestResolveCursorSequence(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	const n = 3
	interval := 900 * time.Second
	t0 := time.Now()

	step := func(now time.Time) int {
		cur, err := ResolveCursor(ctx, pool, "SEQ", 7, interval, n, now)
		if err != nil {
			t.Fatalf("resolve @%v: %v", now, err)
		}
		return cur.Index
	}
	got := []int{
		step(t0),                               // fresh → 0
		step(t0.Add(1 * time.Second)),          // within interval → hold 0
		step(t0.Add(interval + time.Second)),   // due → 1
		step(t0.Add(2*interval + time.Second)), // due → 2
		step(t0.Add(3*interval + time.Second)), // due → wrap to 0
	}
	want := []int{0, 0, 1, 2, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rotation sequence = %v, want %v", got, want)
		}
	}
}

// TestRotationOrderShuffle proves the shuffle permutation is deterministic (same epoch ⇒ same order),
// a full permutation (no repeat until every item is shown), prefix-stable under an item append (an
// add must NOT reshuffle already-shown items' relative order, §4.4), and epoch-sensitive.
func TestRotationOrderShuffle(t *testing.T) {
	ids := []int64{11, 22, 33, 44, 55, 66, 77, 88}
	a := RotationOrder("shuffle", 3, "S1", 9, ids)
	b := RotationOrder("shuffle", 3, "S1", 9, ids)
	if !intsEqual(a, b) {
		t.Fatalf("shuffle not deterministic: %v vs %v", a, b)
	}
	if !isPermutation(a, len(ids)) {
		t.Fatalf("shuffle order %v is not a permutation of [0..%d)", a, len(ids))
	}
	// sequential is the identity order.
	seq := RotationOrder("sequential", 3, "S1", 9, ids)
	for i := range seq {
		if seq[i] != i {
			t.Fatalf("sequential order not identity: %v", seq)
		}
	}
	// epoch-sensitive: a reshuffle (epoch bump) changes the order.
	if intsEqual(a, RotationOrder("shuffle", 4, "S1", 9, ids)) {
		t.Fatal("shuffle order did not change across epochs")
	}
	// per-serial: different serials get different permutations.
	if intsEqual(a, RotationOrder("shuffle", 3, "S2", 9, ids)) {
		t.Fatal("shuffle order identical across serials")
	}

	// prefix stability: append one item; the existing items keep their relative visit order.
	base := []int64{11, 22, 33, 44}
	grown := []int64{11, 22, 33, 44, 99}
	ob := RotationOrder("shuffle", 3, "S1", 9, base)
	og := RotationOrder("shuffle", 3, "S1", 9, grown)
	beforeSeq := idSequence(base, ob)                  // ids in visit order, base playlist
	afterSeq := filterIDs(idSequence(grown, og), base) // ids in visit order among the ORIGINAL set, grown playlist
	if !int64sEqual(beforeSeq, afterSeq) {
		t.Fatalf("item append reshuffled shown positions: before=%v after(filtered)=%v", beforeSeq, afterSeq)
	}
}

// TestVariantCache proves the fleet-shared packed cache round-trips and that the 0013 CHECK rejects an
// off-size pack (only an exact 30000-byte frame may enter the cache, §5).
func TestVariantCache(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	packed := make([]byte, 30000)
	for i := range packed {
		packed[i] = byte(i)
	}
	if _, err := VariantGet(ctx, pool, "deadbeef", "cover", "none"); err != ErrNotFound {
		t.Fatalf("cold get: want ErrNotFound, got %v", err)
	}
	if err := VariantPut(ctx, pool, "deadbeef", "cover", "none", packed); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := VariantGet(ctx, pool, "deadbeef", "cover", "none")
	if err != nil || len(got) != 30000 || got[123] != packed[123] {
		t.Fatalf("get after put: len=%d err=%v", len(got), err)
	}
	// off-size pack is rejected by the CHECK (no truncated frame ever enters the cache).
	if err := VariantPut(ctx, pool, "cafe", "cover", "none", packed[:29999]); err == nil {
		t.Fatal("VariantPut accepted an off-size (29999-byte) frame")
	}
}

// --- helpers ---

func runConcurrent(n int, fn func()) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // barrier: maximise real overlap
			fn()
		}()
	}
	close(start)
	wg.Wait()
}

func cursorPos(t *testing.T, pool *pgxpool.Pool, serial string) int {
	t.Helper()
	var pos int
	if err := pool.QueryRow(context.Background(),
		`SELECT position FROM playlist_cursor WHERE serial=$1`, serial).Scan(&pos); err != nil {
		t.Fatalf("cursor pos %s: %v", serial, err)
	}
	return pos
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func int64sEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isPermutation(order []int, n int) bool {
	if len(order) != n {
		return false
	}
	seen := make([]bool, n)
	for _, v := range order {
		if v < 0 || v >= n || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}

// idSequence maps a visit order of indices to the item ids visited, in order.
func idSequence(ids []int64, order []int) []int64 {
	out := make([]int64, len(order))
	for i, idx := range order {
		out[i] = ids[idx]
	}
	return out
}

// filterIDs keeps only the ids present in `keep`, preserving order.
func filterIDs(seq, keep []int64) []int64 {
	set := map[int64]bool{}
	for _, id := range keep {
		set[id] = true
	}
	out := []int64{}
	for _, id := range seq {
		if set[id] {
			out = append(out, id)
		}
	}
	return out
}

// plTinyPNG is a real, decodable 2×2 RGBA PNG so the image row + blob are seeded through the
// production PutImage path (not hand-inserted) — the GC's refcount join reads a genuine sha256.
func plTinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{G: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestGCVariantCacheRefcount is the W6 TTL-regression gate (masterplan / §6). It builds the cache
// through the production write path (PutImage → CreatePlaylist → AddItem → VariantPut) and proves the
// GC is refcount-driven, NOT a created_at TTL:
//   - a variant whose (sha, fit, dither) a playlist_item still references SURVIVES even when its
//     created_at is backdated far past any plausible TTL — the anti-regression core: a naive
//     created_at sweep would delete exactly this hot, referenced entry (the red state);
//   - a variant with no referencing item (a fit the item never used) is EVICTED regardless of being
//     freshly created;
//   - a variant for a sha no image/item carries is EVICTED;
//   - the run is idempotent (a second pass evicts 0).
func TestGCVariantCacheRefcount(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()

	// Real image row + blob through the production path.
	img, err := imgstore.PutImage(ctx, pool, blobDir, nil, plTinyPNG(t))
	if err != nil {
		t.Fatalf("PutImage: %v", err)
	}
	// Real playlist + item referencing (img.sha256, cover, none) through the production path.
	pl, err := playliststore.CreatePlaylist(ctx, pool, playliststore.CreateParams{Name: "gc-pl"})
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if _, err := playliststore.AddItem(ctx, pool, pl.ID,
		playliststore.AddItemParams{ImageID: img.ID, Fit: "cover", Dither: "none"}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}

	packed := make([]byte, 30000) // 0013 octet_length=30000 CHECK
	// Referenced variant (matches the item's tuple) — survives.
	if err := VariantPut(ctx, pool, img.Sha256, "cover", "none", packed); err != nil {
		t.Fatalf("VariantPut referenced: %v", err)
	}
	// Unreferenced variant: same image, a fit no item uses — evicted.
	if err := VariantPut(ctx, pool, img.Sha256, "contain", "none", packed); err != nil {
		t.Fatalf("VariantPut unreferenced-fit: %v", err)
	}
	// Unreferenced variant: a sha no image/item carries — evicted.
	danglingSha := "00000000000000000000000000000000000000000000000000000000deadbeef"
	if err := VariantPut(ctx, pool, danglingSha, "cover", "none", packed); err != nil {
		t.Fatalf("VariantPut dangling: %v", err)
	}

	// Backdate the REFERENCED row far into the past: age must be irrelevant to a refcount GC. A
	// created_at TTL would evict this hottest entry — that is the regression this gate catches.
	if _, err := pool.Exec(ctx,
		`UPDATE frame_variant_cache SET created_at = now() - interval '400 days'
		 WHERE image_sha = $1 AND fit = 'cover' AND dither = 'none'`, img.Sha256); err != nil {
		t.Fatalf("backdate referenced: %v", err)
	}

	n, err := GCVariantCache(ctx, pool)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 2 { // the unreferenced-fit + the dangling-sha rows
		t.Fatalf("GC removed=%d, want 2", n)
	}
	// The aged, referenced variant must still be present (anti-TTL invariant).
	if got := countRows(t, pool,
		`SELECT count(*) FROM frame_variant_cache WHERE image_sha='`+img.Sha256+`' AND fit='cover' AND dither='none'`); got != 1 {
		t.Fatalf("referenced (aged 400d) variant evicted: %d rows (TTL regression)", got)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM frame_variant_cache`); got != 1 {
		t.Fatalf("cache after GC has %d rows, want 1 (only the referenced one)", got)
	}

	// Idempotent: nothing left to collect.
	n2, err := GCVariantCache(ctx, pool)
	if err != nil {
		t.Fatalf("GC 2: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second GC removed=%d, want 0 (idempotent)", n2)
	}
}
