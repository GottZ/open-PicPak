// Package faasproto is the M4 wire protocol between the FaaS supervisor (Go) and the
// Bun worker: length-prefixed, binary-safe frames carrying VALUES only (never the
// secrets key or a DB handle, D18.7/D24.4). The response image is a FIXED-SIZE raw
// RGB block — ReadFrame enforces a hard ceiling BEFORE any allocation and DecodeResponse
// rejects any image byte count other than the exact 400x300 raw size, so a malicious
// worker can neither over-declare a length to OOM the key+DB supervisor nor smuggle a
// compressed payload it would have to decode (fix-4).
package faasproto

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Frame kinds (the u8 that leads every message payload).
const (
	KindRenderRequest  = 0x01 // sup -> worker
	KindRenderResponse = 0x02 // worker -> sup
	KindCancel         = 0x03 // sup -> worker   payload = JSON {id}
	KindReady          = 0x04 // worker -> sup   payload = JSON {slot, idle}
)

const (
	// RawFrameSize is the fixed raw RGB frame the worker returns: 400*300*3. The panel
	// geometry is fixed (bwry.Width/Height); a mismatch here is a protocol violation.
	RawFrameSize = 400 * 300 * 3 // 360000
	// MetaMax caps the response meta JSON (small: dither/hint/log/err).
	MetaMax = 16 << 10 // 16 KiB
	// MaxFrame bounds any single M4 message before allocation: kind(1) + the response's
	// metaLen(4) + meta + raw image. Requests (source + secret values + limits) are far
	// smaller than the raw response, so this one ceiling bounds both directions.
	MaxFrame = 1 + 4 + MetaMax + RawFrameSize
)

// WriteFrame writes one length-prefixed message: u32be(total) | kind | payload, where
// total = 1 + len(payload). It rejects a message that would exceed MaxFrame.
func WriteFrame(w io.Writer, kind byte, payload []byte) error {
	total := 1 + len(payload)
	if total > MaxFrame {
		return fmt.Errorf("faasproto: frame len %d > max %d", total, MaxFrame)
	}
	buf := make([]byte, 4+total)
	binary.BigEndian.PutUint32(buf[:4], uint32(total))
	buf[4] = kind
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one message. It reads the 4-byte length prefix, REJECTS a declared
// length > MaxFrame (or < 1) BEFORE allocating the payload buffer (the OOM ceiling,
// T4), then reads exactly that many bytes and splits off the kind.
func ReadFrame(r io.Reader) (kind byte, payload []byte, err error) {
	var lenb [4]byte
	if _, err = io.ReadFull(r, lenb[:]); err != nil {
		return 0, nil, err
	}
	total := binary.BigEndian.Uint32(lenb[:])
	if total < 1 {
		return 0, nil, fmt.Errorf("faasproto: frame len %d < 1", total)
	}
	if total > MaxFrame {
		return 0, nil, fmt.Errorf("faasproto: frame len %d > max %d (rejected before alloc)", total, MaxFrame)
	}
	buf := make([]byte, total)
	if _, err = io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}
