package main

// Per-device HOTP authentication for the C2 route (Doc 13 Wave 3d, backend half).
//
// C2 ships remote code execution, so the route must prove the request genuinely came from the device
// it claims to be — otherwise a forged GET ?sn=X&ack=<huge> would advance device X's cursor and make
// it skip every real command (a silent DoS). Auth runs BEFORE the cursor advance, inside the same tx,
// under SELECT ... FOR UPDATE on the device's auth row (serialises retries / cable ticks).
//
// Contract (byte-exact, shared with the firmware auth core): HMAC-SHA1 HOTP over the 8-byte
// big-endian composite counter C = (boot_count << 24) | rtc_counter; the device sends c, otp, bc as
// decimal query params. Any failure -> a constant 401 with no detail (no replay/spoof/existence
// oracle); an unknown/unprovisioned device still burns a dummy HMAC to equalise timing.

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
	authWindowFar = 4096 // recovery / bootstrap look-ahead bound
)

// dummyAuthKey equalises the unknown-device path's timing (constant 401, no existence oracle).
var dummyAuthKey = []byte("00000000000000000000")

// authDevice validates the HOTP triple (c, otp, bc) for serial against the device_auth row under
// FOR UPDATE, and on success persists the new counter within the caller's tx. Returns true iff the
// request is authentic. Never reveals why it failed.
func (s *server) authDevice(ctx context.Context, tx pgx.Tx, serial string, q map[string][]string) bool {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	claimedC, err1 := strconv.ParseUint(get("c"), 10, 64)
	bc, err2 := strconv.ParseUint(get("bc"), 10, 64)
	otp64, err3 := strconv.ParseUint(get("otp"), 10, 32)
	if err1 != nil || err2 != nil || err3 != nil {
		auth.HOTP(dummyAuthKey, claimedC, 8) // equalise timing even on a malformed triple
		return false
	}
	otp := uint32(otp64)

	var secret []byte
	var lastBC, lastRTC int64
	var digits int
	err := tx.QueryRow(ctx,
		`SELECT hotp_secret, last_boot_count, last_rtc_counter, digits
		   FROM device_auth WHERE serial = $1 FOR UPDATE`,
		serial).Scan(&secret, &lastBC, &lastRTC, &digits)
	if errors.Is(err, pgx.ErrNoRows) {
		auth.HOTP(dummyAuthKey, claimedC, digitsOr8(digits)) // unknown device: dummy HMAC, then fail
		return false
	}
	if err != nil {
		return false
	}

	// last_rtc_counter defaults to -1 (never accepted) -> not yet bootstrapped.
	bootstrapped := lastRTC >= 0
	var lastC uint64
	if bootstrapped {
		lastC = (uint64(lastBC) << auth.KSplit) | uint64(lastRTC)
	}

	res := auth.Validate(
		auth.Config{Digits: digits, Window: authWindow, WindowFar: authWindowFar},
		auth.DeviceState{Secret: secret, LastCounter: lastC, Bootstrapped: bootstrapped},
		claimedC, bc, otp)
	if !res.OK {
		return false
	}

	newBC := int64(res.NewCounter >> auth.KSplit)
	newRTC := int64(res.NewCounter & ((1 << auth.KSplit) - 1))
	if _, err := tx.Exec(ctx,
		`UPDATE device_auth
		    SET last_boot_count = $1, last_rtc_counter = $2, last_seen_at = now()
		  WHERE serial = $3`,
		newBC, newRTC, serial); err != nil {
		return false
	}
	return true
}

func digitsOr8(d int) int {
	if d < 6 || d > 8 {
		return 8
	}
	return d
}
