// Package plrender is the A27 W3 playlist render-anchor store (migration 0013): the device→playlist
// binding, the per-serial rotation cursor, and the fleet-shared content-addressed packed-frame cache.
// It is the DB half of the supervisor's built-in __playlist render path.
//
// Two correctness anchors live here:
//   - Binding mutual exclusion: BindPlaylist deletes any device_render_binding + upserts the playlist
//     binding in ONE tx under pg_advisory_xact_lock(hashtext(serial)) — the mirror of
//     faasstore.BindDevice — so a serial can never hold both a function and a playlist binding (§3/§5).
//   - Single-advance rotation: ResolveCursor runs the read→decide→advance under the SAME per-serial
//     advisory lock, so two concurrent /frame requests in one due-window advance the cursor exactly
//     once (the double-advance / image-skip hazard, §4.4). The advance is time-gated on advanced_at.
//
// The variant cache is keyed purely by content (image_sha, fit, dither): same bytes+policy pack
// identically for every device, so the worker runs once per variant fleet-wide (pack once, serve N).
package plrender

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Pool is a Querier that can open a transaction — BindPlaylist and ResolveCursor need it to run their
// per-serial advisory-locked tx. *pgxpool.Pool satisfies it.
type Pool interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ErrNotFound is returned when a variant-cache row does not exist (a plain miss to the caller).
var ErrNotFound = errors.New("plrender: not found")

// Binding is the resolved render binding for a serial: at most one of PlaylistID / FunctionID is
// non-zero (mutual exclusion). Both zero means the device is unbound (renders as before).
type Binding struct {
	PlaylistID int64
	FunctionID int64
}

// Cursor is the resolved rotation state after ResolveCursor: Index is the 0-based step into the
// rotation order (already clamped into range), Advanced reports whether this call advanced the
// cursor, and AdvancedAt is the timestamp the current step started (for the next-wake computation).
type Cursor struct {
	Index      int
	Advanced   bool
	AdvancedAt time.Time
}
