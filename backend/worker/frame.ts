// M4 wire protocol (TS side) — mirrors internal/faasproto byte-for-byte:
//   message := u32be total_len | u8 kind | payload[total_len-1]
//   render-response payload := u32be metaLen | meta JSON | raw image bytes
// The end-to-end parity (Go supervisor <-> this worker over the UDS) is exercised in W4.

export const KIND_RENDER_REQUEST = 0x01;
export const KIND_RENDER_RESPONSE = 0x02;
export const KIND_CANCEL = 0x03;
export const KIND_READY = 0x04;

export const RAW_FRAME_SIZE = 400 * 300 * 3; // 360000
export const META_MAX = 16 << 10; // 16384
export const MAX_FRAME = 1 + 4 + META_MAX + RAW_FRAME_SIZE;

function beU32(b: Uint8Array, off: number): number {
  // multiply the high byte to stay positive (lengths here are far below 2^31 anyway)
  return b[off] * 0x1000000 + (b[off + 1] << 16) + (b[off + 2] << 8) + b[off + 3];
}

function putU32(b: Uint8Array, off: number, v: number): void {
  b[off] = (v >>> 24) & 0xff;
  b[off + 1] = (v >>> 16) & 0xff;
  b[off + 2] = (v >>> 8) & 0xff;
  b[off + 3] = v & 0xff;
}

// encodeFrame builds one length-prefixed message: u32be(1+payload) | kind | payload.
export function encodeFrame(kind: number, payload: Uint8Array): Uint8Array {
  const total = 1 + payload.length;
  if (total > MAX_FRAME) throw new Error(`faasproto: frame len ${total} > max ${MAX_FRAME}`);
  const out = new Uint8Array(4 + total);
  putU32(out, 0, total);
  out[4] = kind;
  out.set(payload, 5);
  return out;
}

export interface Frame {
  kind: number;
  payload: Uint8Array;
}

// FrameReader accumulates stream chunks and yields complete frames. It rejects a declared
// length > MAX_FRAME (or < 1) — the same ceiling faasproto.ReadFrame enforces.
export class FrameReader {
  private buf = new Uint8Array(0);

  feed(chunk: Uint8Array): Frame[] {
    const merged = new Uint8Array(this.buf.length + chunk.length);
    merged.set(this.buf, 0);
    merged.set(chunk, this.buf.length);
    this.buf = merged;

    const out: Frame[] = [];
    for (;;) {
      if (this.buf.length < 4) break;
      const total = beU32(this.buf, 0);
      if (total < 1) throw new Error(`faasproto: frame len ${total} < 1`);
      if (total > MAX_FRAME) throw new Error(`faasproto: frame len ${total} > max ${MAX_FRAME}`);
      if (this.buf.length < 4 + total) break;
      out.push({ kind: this.buf[4], payload: this.buf.slice(5, 4 + total) });
      this.buf = this.buf.slice(4 + total);
    }
    return out;
  }
}

export interface RenderErr {
  kind: string; // throw|timeout|egress|offsize|oom
  msg: string;
}

export interface ResponseMeta {
  v: number;
  id: string;
  ok: boolean;
  w: number;
  h: number;
  dither?: string;
  next_wake_hint?: number;
  log?: { lvl: string; msg: string }[];
  err: RenderErr | null;
}

// encodeResponse builds the render-response payload: u32be metaLen | meta JSON | raw. An OK
// response MUST carry exactly RAW_FRAME_SIZE image bytes; an error response MUST carry none
// (the fixed-size invariant faasproto.DecodeResponse enforces on the Go side).
export function encodeResponse(meta: ResponseMeta, raw: Uint8Array | null): Uint8Array {
  const rawLen = raw ? raw.length : 0;
  if (!meta.err && rawLen !== RAW_FRAME_SIZE) {
    throw new Error(`faasproto: ok response image ${rawLen} != ${RAW_FRAME_SIZE}`);
  }
  if (meta.err && rawLen !== 0) {
    throw new Error(`faasproto: err response must carry no image (${rawLen})`);
  }
  const mj = new TextEncoder().encode(JSON.stringify(meta));
  if (mj.length > META_MAX) throw new Error(`faasproto: meta ${mj.length} > max ${META_MAX}`);
  const out = new Uint8Array(4 + mj.length + rawLen);
  putU32(out, 0, mj.length);
  out.set(mj, 4);
  if (raw) out.set(raw, 4 + mj.length);
  return out;
}

export interface RenderRequest {
  v: number;
  id: string;
  fn: { id: number; ver: number; source: string };
  ctx: { serial: string; channel: string; trigger: { type: string; payload?: unknown }; now: string };
  force: boolean;
  dither_default: string;
  secrets: Record<string, string>;
  egress_allow: string[];
  egress_cred: string;
  limits: { timeout_ms: number; mem_mb: number };
}

export function decodeRequest(payload: Uint8Array): RenderRequest {
  return JSON.parse(new TextDecoder().decode(payload)) as RenderRequest;
}
