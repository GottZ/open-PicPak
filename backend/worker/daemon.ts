// daemon.ts — the pool daemon (§4.4/§4.7). Listens on the M4 UDS, reads render-request frames,
// runs each in a kill-able subprocess slot (bounded to K concurrent), and writes back a
// render-response frame. UNTRUSTED-by-design: no SECRETS_KEY, no DB route, no default route (egress
// via the proxy only) — those HARD walls are the container/network config (W5), not this code.
import {
  FrameReader,
  encodeFrame,
  encodeResponse,
  decodeRequest,
  KIND_RENDER_REQUEST,
  KIND_RENDER_RESPONSE,
  type ResponseMeta,
} from "./frame";
import { runSlot } from "./slot";
import { unlinkSync } from "node:fs";

export interface DaemonOpts {
  sockPath: string;
  slots?: number;
  proxyUrl?: string;
  runnerPath?: string;
  cwd?: string;
}

export interface DaemonHandle {
  close(): void;
}

// Semaphore bounds concurrent slots to K (one in-flight render per slot).
class Semaphore {
  private active = 0;
  private waiters: (() => void)[] = [];
  constructor(private max: number) {}
  async acquire(): Promise<void> {
    if (this.active < this.max) {
      this.active++;
      return;
    }
    await new Promise<void>((r) => this.waiters.push(r)); // resumed by release(), slot transferred
  }
  release(): void {
    const w = this.waiters.shift();
    if (w) w();
    else this.active--;
  }
}

export function startDaemon(opts: DaemonOpts): DaemonHandle {
  const runnerPath = opts.runnerPath ?? new URL("./slot-runner.ts", import.meta.url).pathname;
  const cwd = opts.cwd ?? new URL(".", import.meta.url).pathname;
  const proxyUrl = opts.proxyUrl ?? process.env.EGRESS_PROXY_URL ?? "http://faas-egress:8888";
  const sem = new Semaphore(opts.slots ?? 4);
  const readers = new WeakMap<object, FrameReader>();
  // A render-response frame is ~360 KB; Bun's socket.write() can partial-write under backpressure,
  // so we buffer the remainder per socket and resume on drain (else the frame is truncated and the
  // supervisor's FrameReader waits forever for the declared length).
  const pending = new WeakMap<object, Uint8Array>();
  const flush = (socket: { write: (d: Uint8Array) => number }, buf: Uint8Array) => {
    const n = socket.write(buf);
    if (n < buf.length) pending.set(socket as object, buf.subarray(n));
    else pending.delete(socket as object);
  };
  const send = (socket: { write: (d: Uint8Array) => number }, data: Uint8Array) => {
    const p = pending.get(socket as object);
    if (p) {
      const merged = new Uint8Array(p.length + data.length);
      merged.set(p, 0);
      merged.set(data, p.length);
      flush(socket, merged);
    } else {
      flush(socket, data);
    }
  };

  try {
    unlinkSync(opts.sockPath);
  } catch {
    /* first run: no stale socket */
  }

  const server = Bun.listen({
    unix: opts.sockPath,
    socket: {
      open(socket) {
        readers.set(socket, new FrameReader());
      },
      data(socket, chunk) {
        const reader = readers.get(socket);
        if (!reader) return;
        let frames;
        try {
          frames = reader.feed(new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength));
        } catch {
          socket.end(); // malformed / oversized frame — drop the connection
          return;
        }
        for (const f of frames) {
          if (f.kind === KIND_RENDER_REQUEST) {
            void handleRequest((d) => send(socket, d), f.payload, sem, runnerPath, cwd, proxyUrl);
          }
        }
      },
      drain(socket) {
        const p = pending.get(socket);
        if (p) flush(socket, p);
      },
      close(socket) {
        readers.delete(socket);
        pending.delete(socket);
      },
    },
  });

  return {
    close() {
      server.stop(true);
      try {
        unlinkSync(opts.sockPath);
      } catch {
        /* already gone */
      }
    },
  };
}

async function handleRequest(
  send: (d: Uint8Array) => void,
  payload: Uint8Array,
  sem: Semaphore,
  runnerPath: string,
  cwd: string,
  proxyUrl: string,
): Promise<void> {
  let req;
  try {
    req = decodeRequest(payload);
  } catch {
    return; // undecodable request — ignore (the supervisor times out and falls back)
  }

  await sem.acquire();
  let resp: Uint8Array | null;
  try {
    resp = await runSlot(
      runnerPath,
      cwd,
      {
        id: req.id,
        source: req.fn.source,
        ctx: req.ctx,
        secrets: req.secrets ?? {},
        egress_allow: req.egress_allow ?? [],
        egress_cred: req.egress_cred ?? "",
        proxy_url: proxyUrl,
      },
      req.limits?.timeout_ms ?? 8000,
    );
  } finally {
    sem.release();
  }

  if (resp === null) {
    // timeout / empty slot output — emit a timeout error frame (never a torn frame).
    const meta: ResponseMeta = { v: 1, id: req.id, ok: false, w: 400, h: 300, err: { kind: "timeout", msg: "render timed out" } };
    resp = encodeResponse(meta, null);
  }
  send(encodeFrame(KIND_RENDER_RESPONSE, resp));
}

if (import.meta.main) {
  const sockPath = process.env.FAAS_M4_SOCK ?? "/run/faas/m4.sock";
  startDaemon({ sockPath });
  console.log(`faas-worker: M4 daemon on ${sockPath}`);
}
