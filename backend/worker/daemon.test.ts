import { test, expect } from "bun:test";
import { startDaemon } from "./daemon";
import { encodeFrame, FrameReader, KIND_RENDER_REQUEST, KIND_RENDER_RESPONSE } from "./frame";

function makeReq(source: string, timeoutMs = 8000) {
  return {
    v: 1,
    id: "t",
    fn: { id: 1, ver: 1, source },
    ctx: { serial: "S1", channel: "c", trigger: { type: "render" }, now: "2026-07-01T00:00:00Z" },
    force: false,
    dither_default: "none",
    secrets: {},
    egress_allow: [] as string[],
    egress_cred: "",
    limits: { timeout_ms: timeoutMs, mem_mb: 256 },
  };
}

function beU32(b: Uint8Array, off: number): number {
  return b[off] * 0x1000000 + (b[off + 1] << 16) + (b[off + 2] << 8) + b[off + 3];
}

// rpc connects to the daemon UDS, sends one render-request, and resolves with the parsed response.
function rpc(sockPath: string, reqObj: unknown, timeoutMs = 5000): Promise<{ meta: any; raw: Uint8Array }> {
  const reader = new FrameReader();
  return new Promise((resolve, reject) => {
    const to = setTimeout(() => reject(new Error("rpc client timeout")), timeoutMs);
    Bun.connect({
      unix: sockPath,
      socket: {
        open(socket) {
          socket.write(encodeFrame(KIND_RENDER_REQUEST, new TextEncoder().encode(JSON.stringify(reqObj))));
        },
        data(socket, chunk) {
          const frames = reader.feed(new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength));
          for (const f of frames) {
            if (f.kind !== KIND_RENDER_RESPONSE) continue;
            clearTimeout(to);
            const metaLen = beU32(f.payload, 0);
            const meta = JSON.parse(new TextDecoder().decode(f.payload.subarray(4, 4 + metaLen)));
            const raw = f.payload.subarray(4 + metaLen);
            socket.end();
            resolve({ meta, raw: new Uint8Array(raw) });
          }
        },
        error(_socket, err) {
          clearTimeout(to);
          reject(err);
        },
      },
    });
  });
}

const goodFn =
  "export default async (ctx, cap) => ({ image: cap.sharp({create:{width:400,height:300,channels:3,background:{r:10,g:20,b:30}}}) })";

function sock(name: string): string {
  return `${import.meta.dir}/.test-${name}-${process.pid}.sock`;
}

test("daemon renders a function end-to-end over M4", async () => {
  const s = sock("ok");
  const d = startDaemon({ sockPath: s, slots: 2, proxyUrl: "http://127.0.0.1:1" });
  try {
    const { meta, raw } = await rpc(s, makeReq(goodFn));
    expect(meta.ok).toBe(true);
    expect(meta.w).toBe(400);
    expect(raw.length).toBe(360000);
    expect([raw[0], raw[1], raw[2]]).toEqual([10, 20, 30]);
  } finally {
    d.close();
  }
});

test("daemon surfaces a thrown function as an error frame (never a torn frame)", async () => {
  const s = sock("throw");
  const d = startDaemon({ sockPath: s, slots: 1, proxyUrl: "http://127.0.0.1:1" });
  try {
    const { meta, raw } = await rpc(s, makeReq("export default async () => { throw new Error('boom'); }"));
    expect(meta.ok).toBe(false);
    expect(meta.err.kind).toBe("throw");
    expect(raw.length).toBe(0);
  } finally {
    d.close();
  }
});

test("daemon rejects an off-size image (offsize error, no re-fit)", async () => {
  const s = sock("offsize");
  const d = startDaemon({ sockPath: s, slots: 1, proxyUrl: "http://127.0.0.1:1" });
  try {
    const { meta } = await rpc(
      s,
      makeReq("export default async (ctx, cap) => ({ image: cap.sharp({create:{width:100,height:100,channels:3,background:{r:0,g:0,b:0}}}) })"),
    );
    expect(meta.ok).toBe(false);
    expect(meta.err.kind).toBe("offsize");
  } finally {
    d.close();
  }
});

// T3: a runaway function is killed by termination; the request returns a timeout error and the
// (single) slot is freed — the pool does not starve.
test("daemon kills a runaway function and reuses the slot (T3)", async () => {
  const s = sock("timeout");
  const d = startDaemon({ sockPath: s, slots: 1, proxyUrl: "http://127.0.0.1:1" });
  try {
    const start = Date.now();
    const { meta } = await rpc(s, makeReq("export default async () => { while (true) {} }", 300), 8000);
    expect(meta.ok).toBe(false);
    expect(meta.err.kind).toBe("timeout");
    expect(Date.now() - start).toBeLessThan(3000); // killed well before the rpc client timeout

    // the single slot must be usable again (respawned) — proves the pool did not starve.
    const { meta: meta2, raw } = await rpc(s, makeReq(goodFn));
    expect(meta2.ok).toBe(true);
    expect(raw.length).toBe(360000);
  } finally {
    d.close();
  }
});
