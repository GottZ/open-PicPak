// Package otaticket mints and verifies the short-lived HMAC download ticket that authorizes a single
// firmware.bin GET (D20.4). It is INGEST-ONLY: the signing key (OTA_TICKET_KEY) lives only in the
// cmd/ingest address space, and cmd/admin must NEVER import this package (build guard T8). The ticket
// turns the binary serve from "anonymous, any-version, forever" into "this version, ~2 min" — the
// interim authz until per-device HOTP on the binary GET (F3).
package otaticket

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// MinKeyLen is the floor for a usable signing key (D20.9). A shorter/empty key is trivially forgeable,
// so cmd/ingest 503-disables the serve route at boot rather than minting with it (KeyStrong is that
// gate). 32 bytes = the HMAC-SHA256 block-equivalent strength the ticket leans on.
const MinKeyLen = 32

// KeyStrong reports whether key is long enough to sign tickets (D20.9). cmd/ingest calls this at boot;
// a weak/empty key disables the firmware.bin route (503) instead of serving a forgeable binary.
func KeyStrong(key string) bool { return len(key) >= MinKeyLen }

// Ticket errors are deliberately coarse: a caller maps any of them to a single 403 with a constant
// message (§4.3 step 2) so the wire never learns which check failed.
var (
	ErrMalformed = errors.New("ota ticket: malformed")
	ErrBadMAC    = errors.New("ota ticket: bad signature")
	ErrExpired   = errors.New("ota ticket: expired")
)

// enc is b64url WITHOUT padding: alphabet A-Za-z0-9-_ — excludes both '.' and '|'. That exclusion is
// what makes the '.'-joined encoded triple injective (D20.4/§4.5).
var enc = base64.RawURLEncoding

// signingInput builds the INJECTIVE signing string from the b64url WIRE segments (D20.4/§4.5):
//
//	b64url(sn) "." b64url(version) "." dec(exp)
//
// Because the b64url alphabet excludes '.', the '.'-joined encoded triple is injective: no two distinct
// (sn, version, exp) produce the same signing bytes. It is NOT the decoded sn|version|exp — version is
// free-form esp_app_desc text that may contain '|', which would make the decoded join forgeable.
func signingInput(snB64, verB64 string, exp int64) string {
	return snB64 + "." + verB64 + "." + strconv.FormatInt(exp, 10)
}

// mac returns b64url(HMAC-SHA256(key, input)).
func mac(key, input string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(input))
	return enc.EncodeToString(m.Sum(nil))
}

// Mint returns a self-describing, stateless wire ticket binding (sn, version) for ttl from now:
//
//	b64url(sn) "." b64url(version) "." exp "." b64url(mac)
//
// The first three fields ARE the signing input; the MAC is appended. cmd/ingest sets it into the /pp
// response header X-Firmware-Ticket; the FW replays it on the firmware.bin GET. The ticket binds the
// version at mint time, so a concurrent admin rollout change cannot pivot the subsequent GET to a
// different blob (TOCTOU closed — §4.2).
func Mint(key, sn, version string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	snB64 := enc.EncodeToString([]byte(sn))
	verB64 := enc.EncodeToString([]byte(version))
	in := signingInput(snB64, verB64, exp)
	return in + "." + mac(key, in)
}

// Verify checks a wire ticket and returns the bound (sn, version) or an error. It recomputes the MAC
// over the RECEIVED first three segments verbatim (never decode-then-re-encode), ConstantTimeCompares
// it, and ONLY THEN decodes sn/version/exp and requires exp > now. Order is load-bearing: the MAC is
// checked before exp is parsed, so a tampered exp cannot extend the replay window past a forged MAC.
func Verify(key, token string) (sn, version string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return "", "", ErrMalformed
	}
	snB64, verB64, expStr, macB64 := parts[0], parts[1], parts[2], parts[3]

	// Recompute over the received segments verbatim and compare the canonical b64url forms in constant
	// time. A wrong-length or non-canonical received MAC simply fails the compare (uniform ErrBadMAC).
	want := mac(key, snB64+"."+verB64+"."+expStr)
	if subtle.ConstantTimeCompare([]byte(want), []byte(macB64)) != 1 {
		return "", "", ErrBadMAC
	}

	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", "", ErrMalformed
	}
	if time.Now().Unix() >= exp {
		return "", "", ErrExpired
	}
	snB, err := enc.DecodeString(snB64)
	if err != nil {
		return "", "", ErrMalformed
	}
	verB, err := enc.DecodeString(verB64)
	if err != nil {
		return "", "", ErrMalformed
	}
	return string(snB), string(verB), nil
}
