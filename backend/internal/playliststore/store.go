package playliststore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

const playlistCols = `id, operator_key_id, name, interval_s, order_mode, shuffle_epoch, version, managed_serial, created_at, updated_at`
const itemCols = `id, playlist_id, image_id, position, fit, dither`

// CreateParams is the input to CreatePlaylist. IntervalS<=0 and an empty OrderMode fall back to
// the 0012 defaults (900s / sequential). ManagedSerial is the Delta-3 "Aufs Panel" upsert key;
// leave nil for an operator-authored playlist (the common case; those show in the default list).
type CreateParams struct {
	OperatorKeyID *int64
	Name          string
	IntervalS     int
	OrderMode     string
	ManagedSerial *string
}

// CreatePlaylist validates the name + policy Go-side (clean 422 before the DB CHECKs), then inserts
// a playlist row (version=1, shuffle_epoch=1 by default). A duplicate name/managed_serial surfaces
// as a unique violation (IsUniqueViolation → 409).
func CreatePlaylist(ctx context.Context, q Querier, p CreateParams) (Playlist, error) {
	if !ValidName(p.Name) {
		return Playlist{}, ErrNameInvalid
	}
	interval := p.IntervalS
	if interval <= 0 {
		interval = 900
	}
	mode := p.OrderMode
	if mode == "" {
		mode = "sequential"
	}
	if !ValidOrderMode(mode) {
		return Playlist{}, ErrPolicyInvalid
	}
	return scanPlaylist(q.QueryRow(ctx, `
		INSERT INTO playlist (operator_key_id, name, interval_s, order_mode, managed_serial)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+playlistCols,
		p.OperatorKeyID, p.Name, interval, mode, p.ManagedSerial))
}

// GetPlaylist returns a playlist row by id or ErrNotFound.
func GetPlaylist(ctx context.Context, q Querier, id int64) (Playlist, error) {
	return scanPlaylist(q.QueryRow(ctx, `SELECT `+playlistCols+` FROM playlist WHERE id = $1`, id))
}

// DeletePlaylist removes a playlist; its items go with it (ON DELETE CASCADE). Returns ErrNotFound
// if no row matched.
func DeletePlaylist(ctx context.Context, q Querier, id int64) error {
	tag, err := q.Exec(ctx, `DELETE FROM playlist WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPlaylists returns a keyset page ordered by id (WHERE id > cursor). Pass cursor=0 for the
// first page; the last returned id is the next cursor. By DEFAULT managed playlists (the "Aufs
// Panel" per-serial rows) are hidden — an operator list is not flooded with management noise
// (Delta 3). includeManaged=true is for internal callers that need the full set.
func ListPlaylists(ctx context.Context, q Querier, limit int, cursor int64, includeManaged bool) ([]Summary, error) {
	if limit <= 0 {
		limit = 50
	}
	sql := `SELECT id, name, interval_s, order_mode, version FROM playlist
	        WHERE id > $1 AND managed_serial IS NULL ORDER BY id LIMIT $2`
	if includeManaged {
		sql = `SELECT id, name, interval_s, order_mode, version FROM playlist
		       WHERE id > $1 ORDER BY id LIMIT $2`
	}
	rows, err := q.Query(ctx, sql, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.ID, &s.Name, &s.IntervalS, &s.OrderMode, &s.Version); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AddItemParams is the input to AddItem. Fit/Dither fall back to the 0012 defaults (cover/none)
// when empty.
type AddItemParams struct {
	ImageID int64
	Fit     string
	Dither  string
}

// AddItem appends an item to the END of a playlist (position = current max + 1, gap-tolerant) AND
// bumps playlist.version in the SAME statement (data-modifying CTE → the item change and the
// selection cache-bust are atomic on a plain Querier). A missing image surfaces as an FK violation
// (IsForeignKeyViolation). The bump CTE runs even though the main query does not read it (a WITH
// data-modifying statement always executes to completion).
func AddItem(ctx context.Context, q Querier, playlistID int64, p AddItemParams) (PlaylistItem, error) {
	fit := p.Fit
	if fit == "" {
		fit = "cover"
	}
	dither := p.Dither
	if dither == "" {
		dither = "none"
	}
	return scanItem(q.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO playlist_item (playlist_id, image_id, position, fit, dither)
			VALUES ($1, $2,
			        COALESCE((SELECT max(position) + 1 FROM playlist_item WHERE playlist_id = $1), 0),
			        $3, $4)
			RETURNING `+itemCols+`
		),
		bumped AS (
			UPDATE playlist SET version = version + 1, updated_at = now() WHERE id = $1
		)
		SELECT `+itemCols+` FROM ins`,
		playlistID, p.ImageID, fit, dither))
}

// RemoveItem deletes an item AND bumps its playlist's version in the same statement. Returns
// ErrNotFound if the item does not exist.
func RemoveItem(ctx context.Context, q Querier, itemID int64) error {
	var playlistID int64
	err := q.QueryRow(ctx, `
		WITH del AS (
			DELETE FROM playlist_item WHERE id = $1 RETURNING playlist_id
		),
		bumped AS (
			UPDATE playlist SET version = version + 1, updated_at = now()
			WHERE id = (SELECT playlist_id FROM del)
		)
		SELECT playlist_id FROM del`, itemID).Scan(&playlistID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// PolicyParams carries the mutable rotation policy. IntervalS must be > 0 and OrderMode must be a
// valid mode (validated Go-side for a clean 422).
type PolicyParams struct {
	IntervalS int
	OrderMode string
}

// SetPolicy updates interval_s + order_mode and bumps version. It does NOT touch shuffle_epoch —
// a mid-rotation policy edit must not re-shuffle every device's order (§4.4); that is Reshuffle's
// job. Returns ErrNotFound if no row matched.
func SetPolicy(ctx context.Context, q Querier, playlistID int64, p PolicyParams) error {
	if p.IntervalS <= 0 || !ValidOrderMode(p.OrderMode) {
		return ErrPolicyInvalid
	}
	tag, err := q.Exec(ctx, `
		UPDATE playlist SET interval_s = $2, order_mode = $3, version = version + 1, updated_at = now()
		WHERE id = $1`, playlistID, p.IntervalS, p.OrderMode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Reshuffle bumps shuffle_epoch (the ONLY thing that does, §4.4) AND version — an explicit
// "reshuffle now" operator command re-seeds every device's permutation. Item mutations never bump
// the epoch, so the shuffle order is stable across restarts and item edits until this is called.
func Reshuffle(ctx context.Context, q Querier, playlistID int64) error {
	tag, err := q.Exec(ctx, `
		UPDATE playlist SET shuffle_epoch = shuffle_epoch + 1, version = version + 1, updated_at = now()
		WHERE id = $1`, playlistID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListItems returns a keyset page of one playlist's items ordered by position
// (WHERE position > cursor). Positions are gap-tolerant and start at 0, so pass cursor=-1 for the
// first page; the last returned position is the next cursor. Keyset over the
// (playlist_id, position) index, never OFFSET — playlist_item is the highest-cardinality entity at
// target scale (hundreds of images), the editor must never load it unpaginated (§6).
func ListItems(ctx context.Context, q Querier, playlistID int64, limit int, cursor int) ([]PlaylistItem, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, `SELECT `+itemCols+` FROM playlist_item
		WHERE playlist_id = $1 AND position > $2 ORDER BY position LIMIT $3`, playlistID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlaylistItem{}
	for rows.Next() {
		item, err := scanItemRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Reorder renumbers a playlist's items to the given id order (orderedItemIDs[0] → position 0, …)
// and bumps version, all in ONE transaction (§4.2). Because UNIQUE(playlist_id, position) is
// non-deferrable, a naive single UPDATE would hit transient collisions mid-permutation; the
// two-stage method sidesteps that: first offset every current position into the disjoint negative
// range (-1-position, always distinct and < 0), then assign the final 0..n-1 positions (all >= 0,
// so they cannot collide with the staged negatives). orderedItemIDs MUST be exactly the playlist's
// current item set — a missing/foreign/duplicate id is rejected (ErrReorderIncomplete, tx rolled
// back) rather than stranding rows in the negative range.
func Reorder(ctx context.Context, pool Pool, playlistID int64, orderedItemIDs []int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	// Stage 1: move every current position of this playlist into the negative staging range.
	tag, err := tx.Exec(ctx, `UPDATE playlist_item SET position = -1 - position WHERE playlist_id = $1`, playlistID)
	if err != nil {
		return err
	}
	if int(tag.RowsAffected()) != len(orderedItemIDs) {
		return ErrReorderIncomplete // caller did not supply exactly the current item set
	}

	// Stage 2: assign final positions 0..n-1 by the given id order. unnest keeps this a single
	// statement with no dynamic SQL; the WHERE playlist_id guard prevents touching foreign rows.
	positions := make([]int32, len(orderedItemIDs))
	for i := range orderedItemIDs {
		positions[i] = int32(i)
	}
	tag, err = tx.Exec(ctx, `
		UPDATE playlist_item AS pi SET position = v.pos
		FROM (SELECT * FROM unnest($2::bigint[], $3::int[]) AS t(id, pos)) v
		WHERE pi.id = v.id AND pi.playlist_id = $1`, playlistID, orderedItemIDs, positions)
	if err != nil {
		return err
	}
	if int(tag.RowsAffected()) != len(orderedItemIDs) {
		return ErrReorderIncomplete // a foreign/duplicate id matched fewer rows than expected
	}

	if _, err := tx.Exec(ctx,
		`UPDATE playlist SET version = version + 1, updated_at = now() WHERE id = $1`, playlistID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- helpers ---

// scanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query loop).
type scanner interface {
	Scan(dest ...any) error
}

func scanPlaylistRow(s scanner) (Playlist, error) {
	var p Playlist
	err := s.Scan(&p.ID, &p.OperatorKeyID, &p.Name, &p.IntervalS, &p.OrderMode,
		&p.ShuffleEpoch, &p.Version, &p.ManagedSerial, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func scanPlaylist(row pgx.Row) (Playlist, error) {
	p, err := scanPlaylistRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Playlist{}, ErrNotFound
	}
	if err != nil {
		return Playlist{}, err
	}
	return p, nil
}

func scanItemRow(s scanner) (PlaylistItem, error) {
	var it PlaylistItem
	err := s.Scan(&it.ID, &it.PlaylistID, &it.ImageID, &it.Position, &it.Fit, &it.Dither)
	return it, err
}

func scanItem(row pgx.Row) (PlaylistItem, error) {
	it, err := scanItemRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlaylistItem{}, ErrNotFound
	}
	if err != nil {
		return PlaylistItem{}, err
	}
	return it, nil
}
