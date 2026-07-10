package faasproto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// Request is the render-request (0x01) payload. `Ctx` is the ONLY function-visible
// object (serial/channel/trigger/now); Force/DitherDefault/Secrets/Egress*/Limits are
// engine fields the worker runtime consumes and never exposes in `ctx`. `Secrets` holds
// only the bound names, each already a resolved plaintext VALUE (least privilege, D24.4)
// — there is NO secrets-key field and NO DB DSN field anywhere in this shape (D18.7/T2).
type Request struct {
	V             int               `json:"v"`
	ID            string            `json:"id"`
	Fn            RequestFn         `json:"fn"`
	Ctx           RequestCtx        `json:"ctx"`
	Force         bool              `json:"force"`
	DitherDefault string            `json:"dither_default"`
	Secrets       map[string]string `json:"secrets"`
	EgressAllow   []string          `json:"egress_allow"`
	EgressCred    string            `json:"egress_cred"`
	Limits        Limits            `json:"limits"`
	// Input is an ADDITIVE engine field (A27 W3, proto v2): the raw source-image bytes the
	// built-in __playlist source decodes via cap.sharp (base64 in JSON). Operator functions never
	// set it and never see it (the supervisor injects it ONLY on the playlist render path), so it
	// is omitempty — a plain operator render marshals byte-for-byte as before. It is the reason the
	// request direction gets its own MaxRequestFrame ceiling (frame.go / K7): a multi-MB source
	// image would not fit under the tight response-direction MaxFrame.
	Input []byte `json:"input,omitempty"`
}

// RequestFn is the function identity + source shipped per call (no worker code cache).
type RequestFn struct {
	ID     int64  `json:"id"`
	Ver    int    `json:"ver"`
	Source string `json:"source"`
}

// RequestCtx is exactly the function-visible context.
type RequestCtx struct {
	Serial  string  `json:"serial"`
	Channel string  `json:"channel"`
	Trigger Trigger `json:"trigger"`
	Now     string  `json:"now"`
}

// Trigger carries the trigger type and, for webhook, the inbound POST body.
type Trigger struct {
	Type    string          `json:"type"` // render|prerender|schedule|webhook
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Limits are the per-call resource ceilings (production defaults or test-run clamped).
type Limits struct {
	TimeoutMs int `json:"timeout_ms"`
	MemMB     int `json:"mem_mb"`
}

// ResponseMeta is the render-response (0x02) JSON that precedes the raw image bytes.
type ResponseMeta struct {
	V            int        `json:"v"`
	ID           string     `json:"id"`
	OK           bool       `json:"ok"`
	W            int        `json:"w"`
	H            int        `json:"h"`
	Dither       string     `json:"dither,omitempty"`
	NextWakeHint *int       `json:"next_wake_hint,omitempty"`
	Log          []LogLine  `json:"log,omitempty"`
	Err          *RenderErr `json:"err"`
}

// LogLine is a structured line from cap.log, collected into the response meta.
type LogLine struct {
	Lvl string `json:"lvl"` // info|warn|error
	Msg string `json:"msg"`
}

// RenderErr is the failure descriptor. The worker emits throw|timeout|egress|offsize|oom;
// the supervisor additionally surfaces secret|pack on the wrapped render path.
type RenderErr struct {
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

// EncodeResponse builds the render-response payload: u32be metaLen | meta JSON | raw.
// An OK response MUST carry exactly RawFrameSize image bytes; an error response MUST
// carry none (the image is meaningless on failure). This is the encode-side half of the
// fixed-size invariant (D24.4/fix-4).
func EncodeResponse(meta ResponseMeta, raw []byte) ([]byte, error) {
	if meta.Err == nil && len(raw) != RawFrameSize {
		return nil, fmt.Errorf("faasproto: ok response image %d != %d", len(raw), RawFrameSize)
	}
	if meta.Err != nil && len(raw) != 0 {
		return nil, fmt.Errorf("faasproto: err response must carry no image (has %d)", len(raw))
	}
	mj, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if len(mj) > MetaMax {
		return nil, fmt.Errorf("faasproto: meta %d > max %d", len(mj), MetaMax)
	}
	out := make([]byte, 4+len(mj)+len(raw))
	binary.BigEndian.PutUint32(out[:4], uint32(len(mj)))
	copy(out[4:], mj)
	copy(out[4+len(mj):], raw)
	return out, nil
}

// DecodeResponse parses a render-response payload and enforces the fixed-size invariant:
// an OK response's image byte count must equal RawFrameSize exactly (no over-declare, no
// compressed smuggle), an error response must carry no image (D24.4/fix-4).
func DecodeResponse(payload []byte) (ResponseMeta, []byte, error) {
	var meta ResponseMeta
	if len(payload) < 4 {
		return meta, nil, fmt.Errorf("faasproto: response payload %d < 4", len(payload))
	}
	metaLen := binary.BigEndian.Uint32(payload[:4])
	if metaLen > MetaMax || int(4)+int(metaLen) > len(payload) {
		return meta, nil, fmt.Errorf("faasproto: bad metaLen %d", metaLen)
	}
	if err := json.Unmarshal(payload[4:4+metaLen], &meta); err != nil {
		return meta, nil, fmt.Errorf("faasproto: meta json: %w", err)
	}
	raw := payload[4+metaLen:]
	if meta.Err == nil {
		if len(raw) != RawFrameSize {
			return meta, nil, fmt.Errorf("faasproto: ok response image %d != %d", len(raw), RawFrameSize)
		}
	} else if len(raw) != 0 {
		return meta, nil, fmt.Errorf("faasproto: err response carries %d image bytes", len(raw))
	}
	return meta, raw, nil
}
