package plrender

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ResolveBinding resolves BOTH bindings for a serial in ONE round-trip (a LEFT JOIN of the two
// binding tables), so the /frame hot path pays a single point-lookup even for the fn-bound/unbound
// majority that never has a playlist (§4.3/§6). PlaylistID wins the supervisor's dispatch when set;
// mutual exclusion (the advisory-locked bind paths) guarantees at most one is non-zero.
func ResolveBinding(ctx context.Context, q Querier, serial string) (Binding, error) {
	var pl, fn *int64
	err := q.QueryRow(ctx, `
		SELECT pb.playlist_id, rb.function_id
		FROM (SELECT $1::text AS serial) s
		LEFT JOIN device_playlist_binding pb ON pb.serial = s.serial
		LEFT JOIN device_render_binding   rb ON rb.serial = s.serial`, serial).Scan(&pl, &fn)
	if err != nil {
		return Binding{}, err
	}
	var b Binding
	if pl != nil {
		b.PlaylistID = *pl
	}
	if fn != nil {
		b.FunctionID = *fn
	}
	return b, nil
}

// BindPlaylist points a serial at a playlist and enforces mutual exclusion with a function binding
// (the mirror of faasstore.BindDevice): under pg_advisory_xact_lock(hashtext(serial)) it drops any
// device_render_binding, upserts the playlist binding, and RESETS the cursor (a re-bind starts the
// rotation fresh, §3). Running the whole sequence under the per-serial advisory lock makes the
// "both rows exist" TOCTOU race mechanically impossible.
func BindPlaylist(ctx context.Context, pool Pool, serial string, playlistID int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, serial); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM device_render_binding WHERE serial = $1`, serial); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO device_playlist_binding (serial, playlist_id) VALUES ($1, $2)
		ON CONFLICT (serial) DO UPDATE SET playlist_id = EXCLUDED.playlist_id, bound_at = now()`,
		serial, playlistID); err != nil {
		return err
	}
	// Reset the cursor so the re-bind shows the new playlist from its first step.
	if _, err := tx.Exec(ctx, `DELETE FROM playlist_cursor WHERE serial = $1`, serial); err != nil {
		return err
	}
	// "Aufs Panel" cleanup (b), design/33 §4.5b: a serial switching to a REAL playlist drops its managed
	// single-image playlist — otherwise the FK RESTRICT on that playlist's lone item would keep the
	// backing image undeletable (409 image_in_use) forever. The `id <> playlistID` guard makes this a
	// no-op in the defensive case of binding straight to a managed playlist id (never the operator path).
	if _, err := tx.Exec(ctx, `DELETE FROM playlist WHERE managed_serial = $1 AND id <> $2`, serial, playlistID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// managedName derives the (hidden, ValidName-conform) name for a serial's managed "Aufs Panel"
// playlist. A serial carries uppercase and may hold '~'/'/' shapes the playlist name regex forbids
// (27:90), so the serial itself is unusable as a name; a stable per-serial fnv64 hash is both
// regex-safe (`^[a-z0-9][a-z0-9._-]{0,127}$`) and case-collision-free across serials. The name is
// cosmetic — managed_serial is the real upsert key; ListPlaylists hides managed rows anyway.
func managedName(serial string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(serial))
	return fmt.Sprintf("managed.%016x", h.Sum64())
}

// SetManagedImage is the "Aufs Panel" shortcut store op (design/33 §4.5b, E-A33-4): it points a serial
// at a single image via a per-serial MANAGED playlist, atomically and mutually-exclusive with any
// function binding. Everything runs in ONE tx under pg_advisory_xact_lock(hashtext(serial)) — the A27-W3
// pattern — so two concurrent shortcut clicks (or a shortcut racing a bind) serialise:
//
//   1. CLEAR any device_render_binding for the serial (räumen, not shadow: a later managed-playlist
//      removal must not silently reactivate a stale fn binding — the mutual-exclusion invariant, §5);
//   2. UPSERT the one managed playlist for the serial keyed by managed_serial (exactly one per serial,
//      no growth past the device count at 2000-device scale, §6) — an existing REAL playlist binding is
//      thereby replaced (step 4), a managed one is reused in place;
//   3. REPLACE its content with exactly the one image (position 0, cover/none);
//   4. BIND the device to the managed playlist and RESET the rotation cursor.
//
// The caller pre-checks the image exists (clean 422), so the item insert never hits the FK. Returns the
// managed playlist id.
func SetManagedImage(ctx context.Context, pool Pool, serial string, imageID int64) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	// Serialise every bind of this serial (function OR playlist OR this shortcut) against each other.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, serial); err != nil {
		return 0, err
	}
	// Mutual exclusion: drop any function binding — the serial now shows an image, and the row must be
	// GONE so it can never resurface if the managed playlist is later removed.
	if _, err := tx.Exec(ctx, `DELETE FROM device_render_binding WHERE serial = $1`, serial); err != nil {
		return 0, err
	}
	// Upsert the one managed playlist for the serial. ON CONFLICT (managed_serial) makes a repeat click a
	// version bump on the SAME row (never a second row), which is what makes the concurrent double-click
	// probe converge on exactly one managed playlist.
	var plID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO playlist (name, managed_serial, interval_s, order_mode)
		VALUES ($1, $2, 900, 'sequential')
		ON CONFLICT (managed_serial) DO UPDATE SET version = playlist.version + 1, updated_at = now()
		RETURNING id`, managedName(serial), serial).Scan(&plID); err != nil {
		return 0, err
	}
	// Replace content: exactly the one image.
	if _, err := tx.Exec(ctx, `DELETE FROM playlist_item WHERE playlist_id = $1`, plID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO playlist_item (playlist_id, image_id, position, fit, dither)
		VALUES ($1, $2, 0, 'cover', 'none')`, plID, imageID); err != nil {
		return 0, err
	}
	// Bind the device to the managed playlist (replacing any real-playlist binding) and reset the cursor.
	if _, err := tx.Exec(ctx, `
		INSERT INTO device_playlist_binding (serial, playlist_id) VALUES ($1, $2)
		ON CONFLICT (serial) DO UPDATE SET playlist_id = EXCLUDED.playlist_id, bound_at = now()`,
		serial, plID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM playlist_cursor WHERE serial = $1`, serial); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return plID, nil
}

// CountBindings returns how many devices are currently bound to a playlist — the DELETE /api/playlists/
// {id} in-use guard (a bound playlist is a 409 rather than a silent ON DELETE CASCADE that blanks N
// panels AND orphans their FK-less playlist_cursor rows; image-delete-in-use parity, §5). Reads the
// device_playlist_binding(playlist_id) index.
func CountBindings(ctx context.Context, q Querier, playlistID int64) (int, error) {
	var n int
	err := q.QueryRow(ctx,
		`SELECT count(*) FROM device_playlist_binding WHERE playlist_id = $1`, playlistID).Scan(&n)
	return n, err
}

// UnbindPlaylist drops a serial's playlist binding + cursor (falls back to "no binding"). Used by the
// device-delete cascade (K2/W5) and an explicit unbind. Returns whether a binding row existed.
func UnbindPlaylist(ctx context.Context, q Querier, serial string) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM device_playlist_binding WHERE serial = $1`, serial)
	if err != nil {
		return false, err
	}
	if _, err := q.Exec(ctx, `DELETE FROM playlist_cursor WHERE serial = $1`, serial); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ResolveCursor advances (or holds) the per-serial rotation cursor and returns the step to show. n is
// the current item count. The read→decide→write runs in ONE tx under pg_advisory_xact_lock(hashtext
// (serial)): two concurrent /frame requests in the same due-window therefore serialise, the first
// advances and stamps advanced_at=now, and the second — reading that fresh timestamp — finds the
// interval not yet elapsed and holds the same step. Exactly one advance per window (§4.4, the
// double-advance guard). The advance itself is (index+1) mod n; a shrunk item set clamps the stored
// index into range. n MUST be > 0 (the caller handles the empty-playlist case before calling).
func ResolveCursor(ctx context.Context, pool Pool, serial string, playlistID int64, interval time.Duration, n int, now time.Time) (Cursor, error) {
	if n <= 0 {
		return Cursor{}, errors.New("plrender: ResolveCursor n must be > 0")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Cursor{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, serial); err != nil {
		return Cursor{}, err
	}

	var (
		curPL  int64
		curPos int
		curAt  time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT playlist_id, position, advanced_at FROM playlist_cursor WHERE serial = $1`, serial,
	).Scan(&curPL, &curPos, &curAt)
	fresh := errors.Is(err, pgx.ErrNoRows) || (err == nil && curPL != playlistID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Cursor{}, err
	}

	if fresh {
		// First render on this playlist (new serial or a re-bind to a different playlist): start at
		// step 0 and stamp now, so the next render within the interval holds step 0.
		if _, err := tx.Exec(ctx, `
			INSERT INTO playlist_cursor (serial, playlist_id, position, advanced_at)
			VALUES ($1, $2, 0, $3)
			ON CONFLICT (serial) DO UPDATE SET playlist_id = EXCLUDED.playlist_id, position = 0, advanced_at = EXCLUDED.advanced_at`,
			serial, playlistID, now); err != nil {
			return Cursor{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Cursor{}, err
		}
		return Cursor{Index: 0, Advanced: true, AdvancedAt: now}, nil
	}

	pos := curPos % n // clamp a stale index from a shrunk item set into range
	if pos < 0 {
		pos += n
	}
	if now.Sub(curAt) >= interval {
		next := (pos + 1) % n
		if _, err := tx.Exec(ctx,
			`UPDATE playlist_cursor SET position = $2, advanced_at = $3 WHERE serial = $1`,
			serial, next, now); err != nil {
			return Cursor{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Cursor{}, err
		}
		return Cursor{Index: next, Advanced: true, AdvancedAt: now}, nil
	}
	// Not due — hold the current step. Persist a clamp if the item set shrank under us.
	if pos != curPos {
		if _, err := tx.Exec(ctx, `UPDATE playlist_cursor SET position = $2 WHERE serial = $1`, serial, pos); err != nil {
			return Cursor{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Cursor{}, err
	}
	return Cursor{Index: pos, Advanced: false, AdvancedAt: curAt}, nil
}

// VariantGet returns the fleet-shared packed frame for a content variant, or ErrNotFound on a miss.
func VariantGet(ctx context.Context, q Querier, imageSha, fit, dither string) ([]byte, error) {
	var packed []byte
	err := q.QueryRow(ctx,
		`SELECT packed FROM frame_variant_cache WHERE image_sha = $1 AND fit = $2 AND dither = $3`,
		imageSha, fit, dither).Scan(&packed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return packed, nil
}

// VariantPut stores a packed frame for a content variant (idempotent — the same bytes+policy pack
// identically, so a concurrent second writer is a harmless no-op). packed must be 30000 bytes
// (enforced by the 0013 CHECK).
func VariantPut(ctx context.Context, q Querier, imageSha, fit, dither string, packed []byte) error {
	_, err := q.Exec(ctx, `
		INSERT INTO frame_variant_cache (image_sha, fit, dither, packed) VALUES ($1, $2, $3, $4)
		ON CONFLICT (image_sha, fit, dither) DO NOTHING`,
		imageSha, fit, dither, packed)
	return err
}

// GCVariantCache deletes every frame_variant_cache row no longer referenced by ANY playlist_item
// (maintenance, W6 / §6). Refcount is existence of a matching item, joined image.sha256 = fvc.image_sha
// AND item.fit = fvc.fit AND item.dither = fvc.dither — the exact (content, policy) tuple the packed
// bytes were rendered for. This is DELIBERATELY NOT a created_at TTL: a stable variant is packed once
// (created_at fixed) and served forever, so a time-based sweep would evict the HOTTEST entries and
// trigger a re-pack storm (§6 / the design's anti-TTL note). A row survives on age alone as long as one
// item still points at it; it is collected the moment the last reference goes. The NOT EXISTS join
// rides playlist_item_image_idx (image_id, fit, dither), so it does not scan per image. Idempotent — a
// second run finds nothing left to reference-check false. Returns the number of rows evicted.
func GCVariantCache(ctx context.Context, q Querier) (int64, error) {
	tag, err := q.Exec(ctx, `
		DELETE FROM frame_variant_cache fvc
		WHERE NOT EXISTS (
			SELECT 1 FROM playlist_item pi
			JOIN image i ON i.id = pi.image_id
			WHERE i.sha256 = fvc.image_sha AND pi.fit = fvc.fit AND pi.dither = fvc.dither)`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RotationOrder returns the visit order of item INDICES [0..n) for one serial. `ids` are the item ids
// in stored (position-ascending) order; the returned slice is a permutation of [0..len(ids)).
//
//   - sequential → identity [0,1,..,n-1] (stored position order).
//   - shuffle    → a deterministic per-serial permutation. The permutation is a STABLE sort of the
//     indices by a per-item hash key hash(serial, playlistID, epoch, item.id) — deterministic (same
//     epoch ⇒ same order, stable across restarts, seeded only by serial/playlist/epoch, NOT by
//     playlist.version), a full permutation (no repeat until every item is shown), and PREFIX-STABLE
//     when an item is appended: existing items keep their relative order, the new item just slots in
//     by its own key. (Deliberate deviation from the design's "Fisher-Yates" wording, §4.4: plain FY
//     over [0..n) re-permutes the whole sequence when n grows, which would break the design's own
//     "an item add must NOT reshuffle already-shown positions" gate; a per-item stable hash-key sort
//     gives every property FY was chosen for AND that stability.)
func RotationOrder(mode string, epoch int, serial string, playlistID int64, ids []int64) []int {
	n := len(ids)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if mode != "shuffle" || n < 2 {
		return order
	}
	keys := make([]uint64, n)
	for i, id := range ids {
		keys[i] = shuffleKey(serial, playlistID, epoch, id)
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if keys[ia] != keys[ib] {
			return keys[ia] < keys[ib]
		}
		return ids[ia] < ids[ib] // tie-break on id keeps it a total, deterministic order
	})
	return order
}

// shuffleKey derives the deterministic per-(serial, playlist, epoch, item) sort key.
func shuffleKey(serial string, playlistID int64, epoch int, id int64) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(serial))
	var buf [8]byte
	putU64(&buf, uint64(playlistID))
	_, _ = h.Write(buf[:])
	putU64(&buf, uint64(int64(epoch)))
	_, _ = h.Write(buf[:])
	putU64(&buf, uint64(id))
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

func putU64(b *[8]byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * (7 - i)))
	}
}
