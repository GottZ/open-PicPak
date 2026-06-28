package main

// Per-device authentication for the C2 route (Doc 15, session-only after the cutover).
//
// C2 ships remote code execution, so the route must prove the request genuinely came from the device
// it claims to be — otherwise a forged GET ?sn=X&ack=<huge> would advance device X's cursor and make
// it skip every real command (a silent DoS). Auth runs BEFORE the cursor advance, inside the same tx,
// under SELECT ... FOR UPDATE on the device's auth row (serialises retries / cable ticks).
//
// A device authenticates with a per-session HOTP secret (HMAC-SHA1, decimal c+otp query params) over
// a FLAT session counter — established out-of-band via the signed ECDSA re-key handshake (c2rekey.go).
// There is no boot_count composite, so no bc-lockout. A device with no session yet (or unknown) gets a
// constant 401 with a dummy HMAC (no replay/spoof/existence oracle) and must re-key first.

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/open-picpak/backend/internal/auth"
)

// Policy = data (server config). The auth-contract defaults; future: load from config/env.
const (
	authWindow    = 8    // normal rtc-resync window
	authWindowFar = 4096 // session bootstrap / gap look-ahead bound
)

// dummyAuthKey equalises the unknown/no-session path's timing (constant 401, no existence oracle).
var dummyAuthKey = []byte("00000000000000000000")

// authDevice validates the session HOTP (c, otp) for serial against the device_auth row under
// FOR UPDATE, and on success persists the new counter within the caller's tx. Returns true iff the
// request is authentic. Never reveals why it failed.
func (s *server) authDevice(ctx context.Context, tx pgx.Tx, serial string, q map[string][]string) bool {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	claimedC, errC := strconv.ParseUint(get("c"), 10, 64)
	otp64, errO := strconv.ParseUint(get("otp"), 10, 32)
	if errC != nil || errO != nil {
		auth.HOTP(dummyAuthKey, claimedC, 8) // equalise timing even on a malformed request
		return false
	}
	otp := uint32(otp64)

	var sessionSecret []byte
	var sessLast int64
	var digits int
	var sessBoot bool
	err := tx.QueryRow(ctx,
		`SELECT session_secret, session_last_counter, session_bootstrapped, digits
		   FROM device_auth WHERE serial = $1 FOR UPDATE`,
		serial).Scan(&sessionSecret, &sessLast, &sessBoot, &digits)
	if errors.Is(err, pgx.ErrNoRows) {
		auth.HOTP(dummyAuthKey, claimedC, 8) // unknown device: dummy HMAC, then fail
		return false
	}
	if err != nil {
		return false
	}
	if sessionSecret == nil {
		// bonded but not yet re-keyed (or never bonded) -> dummy HMAC, then fail: the device must
		// run the re-key handshake first to establish a session secret.
		auth.HOTP(dummyAuthKey, claimedC, digitsOr8(digits))
		return false
	}

	cfg := auth.Config{Digits: digitsOr8(digits), Window: authWindow, WindowFar: authWindowFar}
	res := auth.ValidateSession(cfg, sessionSecret, uint64(sessLast), sessBoot, claimedC, otp)
	if !res.OK {
		return false
	}
	_, uerr := tx.Exec(ctx,
		`UPDATE device_auth SET session_last_counter = $1, session_bootstrapped = true, last_seen_at = now()
		  WHERE serial = $2`, int64(res.NewCounter), serial)
	return uerr == nil
}

func digitsOr8(d int) int {
	if d < 6 || d > 8 {
		return 8
	}
	return d
}
