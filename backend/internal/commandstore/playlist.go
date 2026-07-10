package commandstore

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PlaylistRefreshScript is the FIXED, server-authored Berry command the direct-display bridge enqueues
// to a playlist's bound devices: refresh() forces an EPD full refresh after the poll completes (berry
// manifest, intent class), so the device re-fetches /frame and shows the edited playlist instead of
// waiting for its next rotation wake (27:§4.6). It is a CODE CONSTANT — never operator- or
// playlist-derived input — so it opens no injection surface into the device's Berry command channel
// (the same server-owned-asset principle as the built-in __playlist source, 27:§4.6/§5).
const PlaylistRefreshScript = "refresh()"

// playlistRefreshKey is the per-serial idempotency key a refresh enqueue carries. A device needs at
// most ONE pending "re-fetch /frame" no matter how many edits burst onto its playlist, so a CONSTANT
// key collapses a double-clicked/retried edit to a single pending row per serial via the 0017
// command_queue_pending_dedup partial index (K4). This REUSES the A30-W3 dedup guard — it is not a
// second guard. Once the device acks (MarkApplied clears applied=false), the key leaves the index and
// a later edit re-enters with a fresh refresh.
const playlistRefreshKey = "playlist-refresh"

// EnqueuePlaylistRefresh is the direct-display bridge (27:§4.6). It batch-enqueues the fixed refresh
// command to EVERY registered device bound to playlistID in ONE INSERT ... SELECT over
// device_playlist_binding JOIN devices — the registration check the per-serial Enqueue does with a
// SELECT becomes the join, so a 1000-device playlist costs one statement, not N round-trips (§6). Each
// inserted row fires pg_notify('c2_cmd', serial) via the 0006 trigger, so the wake wave rides the
// enqueue for free. The per-serial idempotency key dedups a double-trigger to one pending refresh per
// device (K4). Returns the number of NEW rows enqueued (a fully-deduped re-fire returns 0).
//
// operatorKeyID is the forensic stamp; nil enqueues a NULL attribution (the command_queue FK column is
// nullable) — the honest value when the refresh is a server-side mutation side-effect rather than a
// direct operator command. The bridge stays DEFAULT-OFF at its call sites (K10/HOTP gate): nothing in
// A27 drives it until the on-device HOTP gate is armed (§5, K-HOTP handoff).
func EnqueuePlaylistRefresh(ctx context.Context, pool *pgxpool.Pool, playlistID int64, operatorKeyID *int64) (int, error) {
	tag, err := pool.Exec(ctx, `
		INSERT INTO command_queue (serial, script, operator_key_id, idempotency_key)
		SELECT b.serial, $2, $3, $4
		  FROM device_playlist_binding b
		  JOIN devices d USING (serial)
		 WHERE b.playlist_id = $1
		ON CONFLICT (serial, idempotency_key) WHERE idempotency_key IS NOT NULL AND NOT applied
		DO NOTHING`,
		playlistID, PlaylistRefreshScript, operatorKeyID, playlistRefreshKey)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// NotifyPlaylistChanged signals the supervisor's pre-pack warmer that playlistID changed (a mutation
// or a re-bind), so it can warm the playlist's packed variants — through the SAME single-flight the
// live /frame path takes — and fan out the refresh BEFORE the devices wake (27:§4.6). Payload = the
// playlist id. Fire-and-forget: the notify is a no-op when no warmer is listening (flag off), and the
// /frame single-flight is the correctness backstop if a warm has not finished, so a lost signal only
// costs one cold render, never a wrong frame (§4.3). This is the A28 wiring seam — the mutation handler
// calls it after committing an edit.
func NotifyPlaylistChanged(ctx context.Context, pool *pgxpool.Pool, playlistID int64) error {
	_, err := pool.Exec(ctx, `SELECT pg_notify('playlist_changed', $1)`, strconv.FormatInt(playlistID, 10))
	return err
}
