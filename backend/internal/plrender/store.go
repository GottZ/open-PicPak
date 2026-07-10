package plrender

import (
	"context"
	"errors"
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
	return tx.Commit(ctx)
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
