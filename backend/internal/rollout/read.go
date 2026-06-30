package rollout

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ResolveTarget answers the authoritative OTA target for a device by serial, using the SERVER-owned
// devices.channel as the routing input (D20.5 — NEVER the device-reported X-Picpak-Channel/?ch, which
// a device could spoof to self-promote to beta and pull firmware never staged to it). It loads the
// channel, the device's relevant rollout rows (its own per-serial row + the channel '*' fleet row),
// and the channel default, then runs the single shared Resolve (D20.2) — so the value ingest serves on
// /pp and the value admin returns on GET /api/resolve are identical by construction (T12).
//
// An unknown serial resolves to SourceNone without error: the OTA signal is fail-open (D20.8), so a
// not-yet-registered device simply gets no header rather than a 500.
func ResolveTarget(ctx context.Context, pool *pgxpool.Pool, serial string) (Resolved, error) {
	var ch string
	switch err := pool.QueryRow(ctx, `SELECT channel FROM devices WHERE serial = $1`, serial).Scan(&ch); {
	case errors.Is(err, pgx.ErrNoRows):
		return Resolved{Source: SourceNone}, nil
	case err != nil:
		return Resolved{}, err
	}

	rows, err := pool.Query(ctx,
		`SELECT serial, channel, version, state FROM rollout_targets
		   WHERE channel = $1 AND serial IN ($2, '*')`, ch, serial)
	if err != nil {
		return Resolved{}, err
	}
	defer rows.Close()
	var ts []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.Serial, &t.Channel, &t.Version, &t.State); err != nil {
			return Resolved{}, err
		}
		ts = append(ts, t)
	}
	if err := rows.Err(); err != nil {
		return Resolved{}, err
	}

	// channels.default_version is nullable (FK, may be unset) → COALESCE to ''. The FK on
	// devices.channel guarantees the channel row exists, so ErrNoRows is not expected here.
	var chDefault string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(default_version, '') FROM channels WHERE name = $1`, ch).Scan(&chDefault); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return Resolved{}, err
		}
	}

	return Resolve(ch, ts, chDefault, DefaultPrecedence), nil
}
