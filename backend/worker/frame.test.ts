import { test, expect } from "bun:test";
import {
  encodeFrame,
  FrameReader,
  encodeResponse,
  decodeRequest,
  KIND_RENDER_REQUEST,
  KIND_RENDER_RESPONSE,
  RAW_FRAME_SIZE,
  MAX_FRAME,
  META_MAX,
  type ResponseMeta,
} from "./frame";

test("constants match the faasproto spec", () => {
  expect(RAW_FRAME_SIZE).toBe(360000);
  expect(META_MAX).toBe(16384);
  expect(MAX_FRAME).toBe(1 + 4 + 16384 + 360000);
});

test("encodeFrame produces the spec wire format (u32be total | kind | payload)", () => {
  const f = encodeFrame(KIND_RENDER_REQUEST, new Uint8Array([0x68, 0x69])); // "hi"
  expect(Array.from(f)).toEqual([0x00, 0x00, 0x00, 0x03, 0x01, 0x68, 0x69]);
});

test("FrameReader round-trips and reassembles across chunk boundaries", () => {
  const payload = new TextEncoder().encode("a".repeat(1000));
  const frame = encodeFrame(KIND_RENDER_RESPONSE, payload);
  const r = new FrameReader();
  // feed in three arbitrary splits
  expect(r.feed(frame.slice(0, 2))).toHaveLength(0);
  expect(r.feed(frame.slice(2, 500))).toHaveLength(0);
  const out = r.feed(frame.slice(500));
  expect(out).toHaveLength(1);
  expect(out[0].kind).toBe(KIND_RENDER_RESPONSE);
  expect(out[0].payload).toEqual(payload);
});

test("FrameReader yields multiple frames from one chunk", () => {
  const a = encodeFrame(0x01, new Uint8Array([1]));
  const b = encodeFrame(0x02, new Uint8Array([2, 3]));
  const merged = new Uint8Array(a.length + b.length);
  merged.set(a, 0);
  merged.set(b, a.length);
  const out = new FrameReader().feed(merged);
  expect(out.map((f) => f.kind)).toEqual([0x01, 0x02]);
});

test("FrameReader rejects an over-declared length (ceiling before reading a body)", () => {
  const prefix = new Uint8Array(4);
  new DataView(prefix.buffer).setUint32(0, MAX_FRAME + 1, false);
  expect(() => new FrameReader().feed(prefix)).toThrow(/> max/);
});

test("encodeResponse enforces the fixed 360000-byte image invariant", () => {
  const raw = new Uint8Array(RAW_FRAME_SIZE);
  const ok: ResponseMeta = { v: 1, id: "x", ok: true, w: 400, h: 300, err: null };
  expect(encodeResponse(ok, raw).length).toBe(4 + JSON.stringify(ok).length + RAW_FRAME_SIZE);
  // ok response with the wrong image size is rejected
  expect(() => encodeResponse(ok, raw.subarray(0, RAW_FRAME_SIZE - 1))).toThrow(/!=/);
  // error response must carry no image
  const err: ResponseMeta = { v: 1, id: "x", ok: false, w: 400, h: 300, err: { kind: "throw", msg: "boom" } };
  expect(() => encodeResponse(err, raw)).toThrow(/no image/);
  expect(encodeResponse(err, null).length).toBeGreaterThan(4);
});

test("decodeRequest parses a request payload", () => {
  const req = {
    v: 1,
    id: "abc",
    fn: { id: 1, ver: 1, source: "x" },
    ctx: { serial: "S1", channel: "c", trigger: { type: "render" }, now: "2026-07-01T00:00:00Z" },
    force: false,
    dither_default: "none",
    secrets: { tok: "V" },
    egress_allow: ["ha.local:8123"],
    egress_cred: "cred",
    limits: { timeout_ms: 8000, mem_mb: 256 },
  };
  const got = decodeRequest(new TextEncoder().encode(JSON.stringify(req)));
  expect(got.ctx.serial).toBe("S1");
  expect(got.secrets.tok).toBe("V");
});
