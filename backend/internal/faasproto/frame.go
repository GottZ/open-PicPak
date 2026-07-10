// Package faasproto is the M4 wire protocol between the FaaS supervisor (Go) and the
// Bun worker: length-prefixed, binary-safe frames carrying VALUES only (never the
// secrets key or a DB handle, D18.7/D24.4). The response image is a FIXED-SIZE raw
// RGB block — ReadFrame enforces a hard ceiling BEFORE any allocation and DecodeResponse
// rejects any image byte count other than the exact 400x300 raw size, so a malicious
// worker can neither over-declare a length to OOM the key+DB supervisor nor smuggle a
// compressed payload it would have to decode (fix-4).
//
// Proto v2 (A27 W3 / K7) splits the single frame ceiling into two DIRECTION-specific caps.
// The sup→worker REQUEST may now carry a multi-MB source image (Request.Input, base64), so
// WriteFrame (which the trusted supervisor uses to WRITE requests) is bounded by the larger
// MaxRequestFrame. The worker→sup RESPONSE stays tight: ReadFrame (which the trusted supervisor
// uses to READ the untrusted worker's response) keeps MaxFrame, so a malicious worker still
// cannot over-declare a multi-MB response length to OOM the key+DB supervisor (T4). One constant
// can never be both tight (response) and multi-MB (request) — hence two.
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
	// MaxFrame bounds the worker→sup RESPONSE direction before allocation: kind(1) + metaLen(4)
	// + meta + raw image. ReadFrame (the trusted supervisor reading the untrusted worker) enforces
	// it, so a malicious worker cannot over-declare a giant length to OOM the key+DB supervisor
	// (T4). Kept tight — it does NOT cover the request's source image (that is MaxRequestFrame).
	MaxFrame = 1 + 4 + MetaMax + RawFrameSize
	// MaxRequestImageBytes caps the source image the built-in __playlist source receives in a
	// render request (Request.Input). Multi-MB source blobs (§4.3), aligned with the imgstore
	// ingest ceiling (imgstore.MaxImagePixels is a pixel cap; this is the request-frame byte cap).
	MaxRequestImageBytes = 12 << 20 // 12 MiB
	// MaxRequestFrame bounds the sup→worker REQUEST direction: kind(1) + the request JSON envelope
	// (source + secret values + ctx + limits, MetaMax slack) + the base64-encoded source image.
	// SEPARATE from MaxFrame (K7): only the trusted supervisor produces this direction, so a larger
	// ceiling here does NOT weaken the T4 response wall. base64 inflates the raw image by 4/3.
	MaxRequestFrame = 1 + MetaMax + (MaxRequestImageBytes/3+1)*4
)

// WriteFrame writes one length-prefixed message: u32be(total) | kind | payload, where
// total = 1 + len(payload). In Go this is the supervisor→worker REQUEST writer (driveWorker),
// so the ceiling is MaxRequestFrame — the request may carry a multi-MB source image (K7). The
// response direction is written by the TS worker (encodeFrame, MaxFrame) and read here by
// ReadFrame (MaxFrame), which keeps the T4 wall to the untrusted worker.
func WriteFrame(w io.Writer, kind byte, payload []byte) error {
	total := 1 + len(payload)
	if total > MaxRequestFrame {
		return fmt.Errorf("faasproto: frame len %d > max %d", total, MaxRequestFrame)
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
