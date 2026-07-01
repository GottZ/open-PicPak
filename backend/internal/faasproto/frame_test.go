package faasproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	for _, kind := range []byte{KindRenderRequest, KindRenderResponse, KindCancel, KindReady} {
		payload := []byte("payload-for-" + string([]byte{kind}))
		var buf bytes.Buffer
		if err := WriteFrame(&buf, kind, payload); err != nil {
			t.Fatalf("WriteFrame(%#x): %v", kind, err)
		}
		gotKind, gotPayload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame(%#x): %v", kind, err)
		}
		if gotKind != kind || !bytes.Equal(gotPayload, payload) {
			t.Fatalf("round-trip %#x: got (%#x,%q)", kind, gotKind, gotPayload)
		}
	}
}

// T4 (framing half): ReadFrame rejects an over-declared length BEFORE allocating — a
// worker cannot over-declare a giant length to OOM the key+DB supervisor. We feed ONLY
// the 4-byte prefix (no body); the ceiling must fire before any read of the body.
func TestReadFrameCeilingBeforeAlloc(t *testing.T) {
	var lenb [4]byte
	binary.BigEndian.PutUint32(lenb[:], 50<<20) // declare 50 MiB
	_, _, err := ReadFrame(bytes.NewReader(lenb[:]))
	if err == nil || !strings.Contains(err.Error(), "before alloc") {
		t.Fatalf("want ceiling-before-alloc error, got %v", err)
	}
	// A zero length is also rejected (need at least the kind byte).
	binary.BigEndian.PutUint32(lenb[:], 0)
	if _, _, err := ReadFrame(bytes.NewReader(lenb[:])); err == nil {
		t.Fatal("zero-length frame accepted")
	}
}

func TestWriteFrameRejectsOversize(t *testing.T) {
	if err := WriteFrame(&bytes.Buffer{}, KindRenderResponse, make([]byte, MaxFrame)); err == nil {
		t.Fatal("WriteFrame accepted an oversize payload")
	}
}

func TestResponseFixedSize(t *testing.T) {
	raw := make([]byte, RawFrameSize)
	for i := range raw {
		raw[i] = byte(i)
	}
	// OK round-trip with the exact raw size.
	okMeta := ResponseMeta{V: 1, ID: "x", OK: true, W: 400, H: 300, Dither: "none"}
	enc, err := EncodeResponse(okMeta, raw)
	if err != nil {
		t.Fatalf("EncodeResponse ok: %v", err)
	}
	meta, gotRaw, err := DecodeResponse(enc)
	if err != nil || !meta.OK || !bytes.Equal(gotRaw, raw) {
		t.Fatalf("DecodeResponse ok: meta=%+v err=%v rawEq=%v", meta, err, bytes.Equal(gotRaw, raw))
	}

	// Encode rejects an OK response whose image != the fixed size (no compressed smuggle).
	if _, err := EncodeResponse(okMeta, raw[:RawFrameSize-1]); err == nil {
		t.Error("EncodeResponse accepted an undersized ok image")
	}
	// Error response must carry no image.
	errMeta := ResponseMeta{V: 1, ID: "x", OK: false, Err: &RenderErr{Kind: "throw", Msg: "boom"}}
	if _, err := EncodeResponse(errMeta, raw); err == nil {
		t.Error("EncodeResponse accepted an image on an error response")
	}
	encErr, err := EncodeResponse(errMeta, nil)
	if err != nil {
		t.Fatalf("EncodeResponse err-frame: %v", err)
	}
	if meta, gotRaw, err := DecodeResponse(encErr); err != nil || meta.Err == nil || len(gotRaw) != 0 {
		t.Fatalf("DecodeResponse err-frame: meta=%+v raw=%d err=%v", meta, len(gotRaw), err)
	}

	// Decode rejects a hand-forged OK payload whose trailing image is the wrong size.
	forged := make([]byte, 4+2+10) // metaLen=2, meta="{}", 10 image bytes (!= 360000)
	binary.BigEndian.PutUint32(forged[:4], 2)
	copy(forged[4:], "{}")
	if _, _, err := DecodeResponse(forged); err == nil {
		t.Error("DecodeResponse accepted an ok payload with a short image")
	}
}

// T2: the M4 request shape carries resolved secret VALUES only — never the secrets key,
// never a DB DSN. A structural scan of a marshalled request proves the leak surface is
// absent by construction.
func TestRequestValuesOnly(t *testing.T) {
	req := Request{
		V:  1,
		ID: "abc",
		Fn: RequestFn{ID: 42, Ver: 7, Source: "export default async()=>({})"},
		Ctx: RequestCtx{
			Serial: "S1", Channel: "living-room",
			Trigger: Trigger{Type: "render"}, Now: "2026-07-01T00:00:00Z",
		},
		Secrets:     map[string]string{"ha_token": "SECRET-VALUE-XYZ"},
		EgressAllow: []string{"ha.local:8123"},
		EgressCred:  "per-call-cred",
		Limits:      Limits{TimeoutMs: 8000, MemMB: 256},
	}
	blob, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(blob)
	if !strings.Contains(js, "SECRET-VALUE-XYZ") {
		t.Error("resolved secret value missing from request")
	}
	for _, banned := range []string{"SECRETS_KEY", "SECRETS_KEY_PREV", "POSTGRES_", "dsn", "DATABASE_URL", "postgres://"} {
		if strings.Contains(js, banned) {
			t.Errorf("request leaked %q: %s", banned, js)
		}
	}
}
