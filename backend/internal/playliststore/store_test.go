package playliststore

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-picpak/backend/internal/imgstore"
)

// DB property tests — skipped unless TEST_DATABASE_URL is set (run in the e2e gate against an
// ephemeral postgres with migrations 0001..0012 applied). Mirrors imgstore/faasstore.
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
		`TRUNCATE playlist, playlist_item, image RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// nthPNG builds a distinct, decodable 2×2 PNG per n (distinct bytes → distinct sha → distinct
// image row), so a playlist can hold several items pointing at real image rows.
func nthPNG(t *testing.T, n int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: uint8(n), G: uint8(n * 7), B: uint8(n * 13), A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png %d: %v", n, err)
	}
	return buf.Bytes()
}

// seedImage stores a distinct image row and returns its id (used to reference from items).
func seedImage(t *testing.T, pool *pgxpool.Pool, blobDir string, n int) int64 {
	t.Helper()
	img, err := imgstore.PutImage(context.Background(), pool, blobDir, nil, nthPNG(t, n))
	if err != nil {
		t.Fatalf("seed image %d: %v", n, err)
	}
	return img.ID
}

func version(t *testing.T, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	pl, err := GetPlaylist(context.Background(), pool, id)
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	return pl.Version
}

func strp(s string) *string { return &s }

// TestValidNameRegex is DB-independent (runs in -short): the name regex is the 422 gate. This is
// the W2 name probe (b): uppercase and the '~'/'/' the "Aufs Panel" convention would have needed
// are rejected — the shortcut therefore lives in a column, not a name.
func TestValidNameRegex(t *testing.T) {
	for _, n := range []string{"a", "x0", "weather.frame", "ha-clock_2"} {
		if !ValidName(n) {
			t.Errorf("rejected valid %q", n)
		}
	}
	for _, n := range []string{"", "A", "Upper", "has~tilde", "with/slash", "-lead", ".lead", "has space"} {
		if ValidName(n) {
			t.Errorf("accepted invalid %q", n)
		}
	}
	// CreatePlaylist rejects an invalid name BEFORE any DB access (nil Querier is never touched).
	if _, err := CreatePlaylist(context.Background(), nil, CreateParams{Name: "BAD~name"}); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("CreatePlaylist bad name: want ErrNameInvalid, got %v", err)
	}
}

// TestVersionBumpOnMutation is the W2 stale-cache probe (a): EVERY mutation must bump
// playlist.version (the selection cache-bust). Without the bump this test is the red state.
func TestVersionBumpOnMutation(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()

	pl, err := CreatePlaylist(ctx, pool, CreateParams{Name: "rot", OrderMode: "shuffle"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if pl.Version != 1 || pl.ShuffleEpoch != 1 {
		t.Fatalf("fresh playlist: version=%d epoch=%d (want 1/1)", pl.Version, pl.ShuffleEpoch)
	}
	img1 := seedImage(t, pool, blobDir, 1)
	img2 := seedImage(t, pool, blobDir, 2)

	it1, err := AddItem(ctx, pool, pl.ID, AddItemParams{ImageID: img1})
	if err != nil {
		t.Fatalf("AddItem 1: %v", err)
	}
	if v := version(t, pool, pl.ID); v != 2 {
		t.Fatalf("AddItem did not bump version: got %d, want 2", v)
	}
	if _, err := AddItem(ctx, pool, pl.ID, AddItemParams{ImageID: img2}); err != nil {
		t.Fatalf("AddItem 2: %v", err)
	}
	if v := version(t, pool, pl.ID); v != 3 {
		t.Fatalf("AddItem 2 version: got %d, want 3", v)
	}
	if err := SetPolicy(ctx, pool, pl.ID, PolicyParams{IntervalS: 120, OrderMode: "sequential"}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if v := version(t, pool, pl.ID); v != 4 {
		t.Fatalf("SetPolicy version: got %d, want 4", v)
	}
	if err := RemoveItem(ctx, pool, it1.ID); err != nil {
		t.Fatalf("RemoveItem: %v", err)
	}
	if v := version(t, pool, pl.ID); v != 5 {
		t.Fatalf("RemoveItem version: got %d, want 5", v)
	}
}

// TestSetPolicyDoesNotReshuffle: a policy edit bumps version but NOT shuffle_epoch (§4.4);
// Reshuffle bumps both. This is what keeps device order stable across mid-rotation edits.
func TestReshuffleVsPolicy(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	pl, err := CreatePlaylist(ctx, pool, CreateParams{Name: "shf", OrderMode: "shuffle"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := SetPolicy(ctx, pool, pl.ID, PolicyParams{IntervalS: 60, OrderMode: "shuffle"}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	got, _ := GetPlaylist(ctx, pool, pl.ID)
	if got.ShuffleEpoch != 1 {
		t.Fatalf("SetPolicy touched shuffle_epoch: got %d, want 1", got.ShuffleEpoch)
	}
	if got.Version != 2 {
		t.Fatalf("SetPolicy version: got %d, want 2", got.Version)
	}
	if err := Reshuffle(ctx, pool, pl.ID); err != nil {
		t.Fatalf("Reshuffle: %v", err)
	}
	got, _ = GetPlaylist(ctx, pool, pl.ID)
	if got.ShuffleEpoch != 2 || got.Version != 3 {
		t.Fatalf("after Reshuffle: epoch=%d version=%d (want 2/3)", got.ShuffleEpoch, got.Version)
	}
}

// TestListPlaylistsHidesManaged is the W2 managed-serial probe (c): a managed playlist (Delta 3
// shortcut row) is INVISIBLE in the default list and VISIBLE with includeManaged=true.
func TestListPlaylistsHidesManaged(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	if _, err := CreatePlaylist(ctx, pool, CreateParams{Name: "operator-list"}); err != nil {
		t.Fatalf("create operator playlist: %v", err)
	}
	if _, err := CreatePlaylist(ctx, pool, CreateParams{Name: "managed-one", ManagedSerial: strp("ABC123")}); err != nil {
		t.Fatalf("create managed playlist: %v", err)
	}

	def, err := ListPlaylists(ctx, pool, 50, 0, false)
	if err != nil {
		t.Fatalf("list default: %v", err)
	}
	if len(def) != 1 || def[0].Name != "operator-list" {
		t.Fatalf("default list leaked managed rows: %+v", def)
	}

	all, err := ListPlaylists(ctx, pool, 50, 0, true)
	if err != nil {
		t.Fatalf("list includeManaged: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("includeManaged missing rows: %+v", all)
	}
}

// TestImageInUseBlocksDelete proves the 0012 FK RESTRICT end-to-end: an image referenced by a
// playlist_item cannot be deleted (imgstore.DeleteImage → ErrImageInUse). Removing the item frees
// it. This is the image_id-FK-RESTRICT probe (§7) — only exercisable now that playlist_item exists.
func TestImageInUseBlocksDelete(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()

	pl, err := CreatePlaylist(ctx, pool, CreateParams{Name: "pinned"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	imgID := seedImage(t, pool, blobDir, 42)
	item, err := AddItem(ctx, pool, pl.ID, AddItemParams{ImageID: imgID})
	if err != nil {
		t.Fatalf("AddItem: %v", err)
	}

	if err := imgstore.DeleteImage(ctx, pool, blobDir, imgID); !errors.Is(err, imgstore.ErrImageInUse) {
		t.Fatalf("in-use image deletable: want ErrImageInUse, got %v", err)
	}
	// Free it, then delete succeeds.
	if err := RemoveItem(ctx, pool, item.ID); err != nil {
		t.Fatalf("RemoveItem: %v", err)
	}
	if err := imgstore.DeleteImage(ctx, pool, blobDir, imgID); err != nil {
		t.Fatalf("freed image not deletable: %v", err)
	}
}

// TestAddItemMissingImage: AddItem against a non-existent image_id is an FK violation.
func TestAddItemMissingImage(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	pl, err := CreatePlaylist(ctx, pool, CreateParams{Name: "nofk"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = AddItem(ctx, pool, pl.ID, AddItemParams{ImageID: 999999})
	if !IsForeignKeyViolation(err) {
		t.Fatalf("missing image: want FK violation, got %v", err)
	}
}

// TestReorderAndListItems: append 3 items (positions 0,1,2), reorder, verify the new order and
// the keyset ListItems paging; a wrong id set is rejected and rolls back.
func TestReorderAndListItems(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()

	pl, err := CreatePlaylist(ctx, pool, CreateParams{Name: "ord"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var itemIDs []int64
	for i := 1; i <= 3; i++ {
		it, err := AddItem(ctx, pool, pl.ID, AddItemParams{ImageID: seedImage(t, pool, blobDir, i)})
		if err != nil {
			t.Fatalf("AddItem %d: %v", i, err)
		}
		itemIDs = append(itemIDs, it.ID)
	}
	// Appended in order → positions 0,1,2.
	items, err := ListItems(ctx, pool, pl.ID, 50, -1)
	if err != nil || len(items) != 3 {
		t.Fatalf("ListItems: len=%d err=%v", len(items), err)
	}
	for i, it := range items {
		if it.Position != i || it.ID != itemIDs[i] {
			t.Fatalf("initial order wrong at %d: %+v", i, it)
		}
	}

	verBefore := version(t, pool, pl.ID)
	// Reorder to reverse: [id3, id2, id1].
	if err := Reorder(ctx, pool, pl.ID, []int64{itemIDs[2], itemIDs[1], itemIDs[0]}); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	if v := version(t, pool, pl.ID); v != verBefore+1 {
		t.Fatalf("Reorder did not bump version: %d -> %d", verBefore, v)
	}
	items, _ = ListItems(ctx, pool, pl.ID, 50, -1)
	wantOrder := []int64{itemIDs[2], itemIDs[1], itemIDs[0]}
	for i, it := range items {
		if it.Position != i || it.ID != wantOrder[i] {
			t.Fatalf("reordered order wrong at %d: got id=%d pos=%d", i, it.ID, it.Position)
		}
	}

	// Keyset paging: page size 2 then continue from the last position.
	page1, _ := ListItems(ctx, pool, pl.ID, 2, -1)
	if len(page1) != 2 {
		t.Fatalf("page1 len=%d", len(page1))
	}
	page2, _ := ListItems(ctx, pool, pl.ID, 2, page1[len(page1)-1].Position)
	if len(page2) != 1 || page2[0].Position != 2 {
		t.Fatalf("page2 wrong: %+v", page2)
	}

	// Wrong id set (only 2 of 3) → ErrReorderIncomplete, rolled back (order unchanged).
	if err := Reorder(ctx, pool, pl.ID, []int64{itemIDs[0], itemIDs[1]}); !errors.Is(err, ErrReorderIncomplete) {
		t.Fatalf("short reorder: want ErrReorderIncomplete, got %v", err)
	}
	items, _ = ListItems(ctx, pool, pl.ID, 50, -1)
	for i, it := range items {
		if it.ID != wantOrder[i] {
			t.Fatalf("rolled-back reorder mutated order at %d: %+v", i, it)
		}
	}
}

// TestDuplicateName: a second playlist with the same name is a unique violation (→ 409).
func TestDuplicateName(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	if _, err := CreatePlaylist(ctx, pool, CreateParams{Name: "dup"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := CreatePlaylist(ctx, pool, CreateParams{Name: "dup"})
	if !IsUniqueViolation(err) {
		t.Fatalf("duplicate name: want unique violation, got %v", err)
	}
}

// TestDeletePlaylistCascades: deleting a playlist drops its items (ON DELETE CASCADE), which
// releases the FK RESTRICT so the images become deletable again.
func TestDeletePlaylistCascades(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()

	pl, err := CreatePlaylist(ctx, pool, CreateParams{Name: "casc"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	imgID := seedImage(t, pool, blobDir, 7)
	if _, err := AddItem(ctx, pool, pl.ID, AddItemParams{ImageID: imgID}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if err := DeletePlaylist(ctx, pool, pl.ID); err != nil {
		t.Fatalf("DeletePlaylist: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM playlist_item WHERE playlist_id = $1`, pl.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("items survived cascade: count=%d err=%v", n, err)
	}
	if err := imgstore.DeleteImage(ctx, pool, blobDir, imgID); err != nil {
		t.Fatalf("image not freed after cascade: %v", err)
	}
	if err := DeletePlaylist(ctx, pool, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: want ErrNotFound, got %v", err)
	}
}
