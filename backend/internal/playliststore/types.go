// Package playliststore is the playlist + ordered-item store (migration 0012, A27 W2):
// operator-attributed playlist rows (rotation cadence, order mode, shuffle epoch, version) and
// their ordered playlist_item rows (image reference + per-item fit/dither policy). Policy=Data —
// the rotation is a set of rows, never a code constant (M3).
//
// EVERY mutation (AddItem/RemoveItem/Reorder/SetPolicy/Reshuffle) bumps playlist.version, which is
// the selection cache-bust the render path reads (K7 parity with faasstore.Update). The version
// bump rides in the SAME statement as the item change (data-modifying CTE) so an item mutation and
// its bump are atomic on a plain Querier; Reorder is the one genuinely multi-statement op and
// takes a Pool to run its two-stage renumber in one tx (§4.2).
//
// All simple access goes through a Querier so a call can run on the pool or inside a tx (e.g. the
// device-delete cascade, W5). name uniqueness is GLOBAL for now (single-operator); the
// // TODO(multi-tenant) seam in 0012 moves it to UNIQUE(operator_key_id, name).
package playliststore

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// nameRe is the operator-chosen playlist name charset (matches the 0012 comment / 27:90). The
// managed_serial shortcut key is deliberately NOT held here: serials carry uppercase and '~'/'/'
// are illegal, so a name-encoded convention is unbuildable — the column is the key (Delta 3).
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidName reports whether name is an acceptable playlist name (validated before the DB, → 422).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// orderModes is the closed set the 0012 CHECK enforces; validated Go-side for a clean 422 before
// the DB round-trip.
var orderModes = map[string]bool{"sequential": true, "shuffle": true}

// ValidOrderMode reports whether m is a supported rotation order.
func ValidOrderMode(m string) bool { return orderModes[m] }

var (
	// ErrNotFound is returned when a playlist / item row does not exist.
	ErrNotFound = errors.New("playliststore: not found")
	// ErrNameInvalid is returned when a playlist name fails ValidName (→ 422). The '~'/'/'/
	// uppercase cases the "Aufs Panel" shortcut would have needed are exactly what this rejects.
	ErrNameInvalid = errors.New("playliststore: invalid playlist name")
	// ErrPolicyInvalid is returned for a bad order_mode or a non-positive interval (→ 422).
	ErrPolicyInvalid = errors.New("playliststore: invalid rotation policy")
	// ErrReorderIncomplete is returned when the id set passed to Reorder is not exactly the
	// playlist's current item set (missing/foreign/duplicate ids) — the reorder is rolled back
	// rather than leaving items stranded in the negative staging range.
	ErrReorderIncomplete = errors.New("playliststore: reorder id set does not match playlist items")
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Pool is a Querier that can open a transaction — Reorder needs it for its two-stage renumber
// (offset into the negative range, then assign final positions) which cannot be a single
// statement under the non-deferrable UNIQUE(playlist_id, position). *pgxpool.Pool satisfies it.
type Pool interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Playlist is a full playlist row.
type Playlist struct {
	ID            int64     `json:"id"`
	OperatorKeyID *int64    `json:"operator_key_id,omitempty"`
	Name          string    `json:"name"`
	IntervalS     int       `json:"interval_s"`
	OrderMode     string    `json:"order_mode"`
	ShuffleEpoch  int       `json:"shuffle_epoch"`
	Version       int       `json:"version"`
	ManagedSerial *string   `json:"managed_serial,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// PlaylistItem is one ordered entry in a playlist.
type PlaylistItem struct {
	ID         int64  `json:"id"`
	PlaylistID int64  `json:"playlist_id"`
	ImageID    int64  `json:"image_id"`
	Position   int    `json:"position"`
	Fit        string `json:"fit"`
	Dither     string `json:"dither"`
}

// Summary is the list-view projection (no per-item detail).
type Summary struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	IntervalS int    `json:"interval_s"`
	OrderMode string `json:"order_mode"`
	Version   int    `json:"version"`
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint violation (23505) — the
// admin handler maps a duplicate playlist name (or managed_serial) to 409.
func IsUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// IsForeignKeyViolation reports whether err is a Postgres FK violation (23503) — AddItem against a
// missing image surfaces this (→ 422/404 at the edge).
func IsForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}
