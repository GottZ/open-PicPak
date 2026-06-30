// Package devicestore is the operator-side device registry: registration/re-bond, listing, and the
// per-device read model. The ECDSA public key is the single durable identity (D17.6); the session
// fields are owned by the C2 re-key handshake and only RESET here on a genuine identity change.
package devicestore

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var serialRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,31}$`)

// ValidSerial reports whether s is a legal device serial: the charset/length pinned across every
// consumer (the {"serial_number":"<S>"} envelope, the on-device sn[32]/31-char cap, the ?sn URL).
// '*' is reserved as the fleet-broadcast target and is NOT a valid registration serial.
func ValidSerial(s string) bool { return s != "*" && serialRe.MatchString(s) }

// ValidPubkey reports whether b is a raw 65-byte uncompressed P-256 point (0x04||X||Y) ON the curve —
// the same gate the C2 re-key path applies. A bare length check passes an off-curve/all-zero blob.
func ValidPubkey(b []byte) bool {
	if len(b) != 65 || b[0] != 0x04 {
		return false
	}
	x, _ := elliptic.Unmarshal(elliptic.P256(), b) //nolint:staticcheck // raw point validation; no ecdh path
	return x != nil
}

// Row is the control-plane device list shape: identity + membership + liveness. bonded is the
// session-established signal (the c2rekey handshake completed), NOT mere pubkey presence (COH1) — a
// registered-but-never-polled device reads bonded=false.
type Row struct {
	Serial   string     `json:"serial"`
	Label    *string    `json:"label"`
	Channel  string     `json:"channel"`
	LastSeen *time.Time `json:"last_seen"`
	Bonded   bool       `json:"bonded"`
}

// Detail is the single-device read model: the list row plus the two bond states kept distinct —
// pubkey_present (enrolled, identity registered) vs bonded (session established) (COH1).
type Detail struct {
	Row
	PubkeyPresent bool `json:"pubkey_present"`
}

// Register upserts a device and its bond pubkey idempotently and returns the action taken:
//
//	created  — new device
//	rebonded — existing device, pubkey CHANGED → the session is reset (re-key required next poll)
//	updated  — existing device, identical pubkey → metadata touch only, session untouched
//
// channel/label are preserved when their argument is nil (omitted), never clobbered (D17.4/T11). The
// C2 cursor is seeded to HEAD only for a brand-new cursor (Root A / GAP-M1): an identical re-POST
// never bumps an existing cursor forward, so queued commands are never silently dropped.
func Register(ctx context.Context, pool *pgxpool.Pool, serial string, label, channel *string, pubkey []byte) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var existing []byte
	exists := true
	switch err = tx.QueryRow(ctx, `SELECT ecdsa_pubkey FROM device_auth WHERE serial = $1`, serial).Scan(&existing); {
	case errors.Is(err, pgx.ErrNoRows):
		exists = false
	case err != nil:
		return "", err
	}
	rebond := exists && !bytes.Equal(existing, pubkey)
	action := "created"
	if exists {
		if action = "updated"; rebond {
			action = "rebonded"
		}
	}

	// devices: COALESCE on the PARAMETER (not EXCLUDED) so an omitted channel/label preserves the
	// stored value; a brand-new row takes 'stable' / the column default (T11).
	if _, err = tx.Exec(ctx,
		`INSERT INTO devices (serial, channel, label, last_seen)
		   VALUES ($1, COALESCE($2,'stable'), $3, now())
		 ON CONFLICT (serial) DO UPDATE SET
		   last_seen = now(),
		   channel   = COALESCE($2, devices.channel),
		   label     = COALESCE($3, devices.label)`,
		serial, channel, label); err != nil {
		return "", err
	}

	// device_auth: store the pubkey; reset the session fields ONLY on a real identity change ($3),
	// never on an identical re-POST (D17.4/T5/T5b). Registration never writes session state directly.
	if _, err = tx.Exec(ctx,
		`INSERT INTO device_auth (serial, ecdsa_pubkey) VALUES ($1, $2)
		 ON CONFLICT (serial) DO UPDATE SET
		   ecdsa_pubkey         = $2,
		   session_secret       = CASE WHEN $3 THEN NULL  ELSE device_auth.session_secret END,
		   session_bootstrapped = CASE WHEN $3 THEN false ELSE device_auth.session_bootstrapped END,
		   session_epoch        = CASE WHEN $3 THEN device_auth.session_epoch + 1 ELSE device_auth.session_epoch END,
		   session_last_counter = CASE WHEN $3 THEN 0     ELSE device_auth.session_last_counter END`,
		serial, pubkey, rebond); err != nil {
		return "", err
	}

	// Seed the C2 cursor at HEAD ONLY for a brand-new cursor (Root A / GAP-M1) — DO NOTHING never
	// touches an existing cursor, so a re-POST cannot jump it past queued commands.
	if _, err = tx.Exec(ctx,
		`INSERT INTO device_c2_cursor (serial, applied_seq)
		   VALUES ($1, (SELECT COALESCE(MAX(seq),0) FROM command_queue))
		 ON CONFLICT (serial) DO NOTHING`,
		serial); err != nil {
		return "", err
	}

	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return action, nil
}

// List returns every device with its membership + bond state (bonded = session_bootstrapped, COH1).
func List(ctx context.Context, pool *pgxpool.Pool) ([]Row, error) {
	rows, err := pool.Query(ctx,
		`SELECT d.serial, d.label, d.channel, d.last_seen, COALESCE(da.session_bootstrapped, false)
		   FROM devices d LEFT JOIN device_auth da ON da.serial = d.serial
		   ORDER BY d.serial`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Serial, &r.Label, &r.Channel, &r.LastSeen, &r.Bonded); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one device's read model, or found=false if the serial is unknown.
func Get(ctx context.Context, pool *pgxpool.Pool, serial string) (Detail, bool, error) {
	var d Detail
	err := pool.QueryRow(ctx,
		`SELECT d.serial, d.label, d.channel, d.last_seen,
		        (da.ecdsa_pubkey IS NOT NULL) AS pubkey_present,
		        COALESCE(da.session_bootstrapped, false) AS bonded
		   FROM devices d LEFT JOIN device_auth da ON da.serial = d.serial
		   WHERE d.serial = $1`, serial).
		Scan(&d.Serial, &d.Label, &d.Channel, &d.LastSeen, &d.PubkeyPresent, &d.Bonded)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, false, nil
	}
	if err != nil {
		return Detail{}, false, err
	}
	return d, true, nil
}

// Patch updates label and/or channel; a nil argument preserves the stored value (T11). Returns the
// refreshed read model, or found=false if the serial is unknown.
func Patch(ctx context.Context, pool *pgxpool.Pool, serial string, label, channel *string) (Detail, bool, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE devices SET channel = COALESCE($2, channel), label = COALESCE($3, label) WHERE serial = $1`,
		serial, channel, label)
	if err != nil {
		return Detail{}, false, err
	}
	if tag.RowsAffected() == 0 {
		return Detail{}, false, nil
	}
	return Get(ctx, pool, serial)
}

// Delete removes a device and everything keyed to it, in one tx (D17.8). CASCADE clears device_auth,
// the C2 cursor, the log fragments and the nonce; command_queue and rollout_targets have no FK and are
// cleared explicitly. Returns found=false if the serial does not exist (nothing is committed).
//
// purgeTimeSeries (the A22/A21 K2 seam, gated by ADMIN_DELETE_PURGES_TELEMETRY default-off) additionally
// drops this serial's telemetry AND logs rows IN THE SAME TX — both-or-neither (D22.11). The hypertables
// have no FK to devices and are retention-managed (telemetry 365 d, logs 90 d), so without this a
// decommissioned/handed-off device's battery curves, boot history, src_ip and log lines otherwise linger
// up to a year and BLEED into a re-registered same serial (D17.4 re-bond). Default-off because
// immediate-purge-vs-retention is an operator data-lifecycle choice; opt-in for right-to-erasure. The
// purge is plain SQL here in Doc 17's tx, NOT routed through Doc 22's read-only internal/telemetry — so
// the 22→17 seam (Doc 17 telemetry-read-free) holds. Later waves extend this tx (FaaS render tables, K2).
func Delete(ctx context.Context, pool *pgxpool.Pool, serial string, purgeTimeSeries bool) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(ctx, `DELETE FROM command_queue WHERE serial = $1`, serial); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM rollout_targets WHERE serial = $1`, serial); err != nil {
		return false, err
	}
	if purgeTimeSeries {
		// both-or-neither under the one flag: telemetry (D22.11, A22) + logs (A21's half, parked here).
		if _, err := tx.Exec(ctx, `DELETE FROM telemetry WHERE serial = $1`, serial); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM logs WHERE serial = $1`, serial); err != nil {
			return false, err
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM devices WHERE serial = $1`, serial)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil // unknown serial — the deferred rollback discards the no-op deletes
	}
	return true, tx.Commit(ctx)
}
