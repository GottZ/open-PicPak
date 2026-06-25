// Package auth implements server-side per-device HOTP validation (RFC 4226), byte-compatible with
// the firmware HOTP core (firmware/main/auth_core.h).
package auth

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 4226 mandates HMAC-SHA1; this is a protocol requirement, not a hash-security choice
	"encoding/binary"
)

// HOTP computes RFC 4226 HOTP over the 8-byte big-endian counter, matching the firmware's
// auth_hotp() byte-for-byte (HMAC-SHA1, dynamic truncation, mod 10^digits). digits must be 6..8
// (mirrors the firmware guard); other values return 0. The HMAC message is the counter only;
// per-device binding comes from the per-device secret, not from the message.
func HOTP(secret []byte, c uint64, digits int) uint32 {
	if digits < 6 || digits > 8 {
		return 0
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], c)
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)

	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off]&0x7f) << 24) |
		(uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) |
		uint32(sum[off+3])
	return bin % pow10(digits)
}

func pow10(n int) uint32 {
	p := uint32(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}
