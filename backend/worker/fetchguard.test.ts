import { test, expect, afterAll } from "bun:test";
import { makeFetch, hostAllowed } from "./fetchguard";

test("hostAllowed matches host and host:port entries", () => {
  expect(hostAllowed("ha.local", "8123", ["ha.local:8123"])).toBe(true);
  expect(hostAllowed("ha.local", "443", ["ha.local"])).toBe(true);
  expect(hostAllowed("evil.example", "443", ["ha.local"])).toBe(false);
});

// A mock origin we drive directly (no proxy): makeFetch is given an empty proxyUrl and the loopback
// host on its allowlist, so the guard's early host check passes and the request hits our server. The
// response cap is the unit under test; the network proxy is out of scope here.
const server = Bun.serve({
  port: 0,
  fetch(req) {
    const u = new URL(req.url);
    const n = Number(u.searchParams.get("n") ?? "0");
    const body = new Uint8Array(n); // n zero-bytes
    if (u.searchParams.get("cl") === "1") {
      // honest Content-Length path (early reject before any body read)
      return new Response(body, { headers: { "content-length": String(n) } });
    }
    // chunked/streaming path: no reliable Content-Length — the streaming cap must bite mid-read
    const stream = new ReadableStream<Uint8Array>({
      start(c) {
        const chunk = new Uint8Array(1024);
        let sent = 0;
        const pump = () => {
          if (sent >= n) return c.close();
          c.enqueue(chunk);
          sent += chunk.length;
          queueMicrotask(pump);
        };
        pump();
      },
    });
    return new Response(stream);
  },
});
const base = `http://127.0.0.1:${server.port}`;
afterAll(() => server.stop(true));

function guard(maxBytes: number): typeof fetch {
  // proxyUrl "" => Bun ignores the proxy field and connects directly to the origin (test-only).
  return makeFetch({ allow: ["127.0.0.1"], proxyUrl: "", cred: "x", maxBytes });
}

test("A31.4 over-cap response via Content-Length is rejected early (no buffering)", async () => {
  const f = guard(64 * 1024); // 64 KiB cap
  await expect(f(`${base}/?n=${1 << 20}&cl=1`)).rejects.toThrow(/too large|cap|exceed/i);
});

test("A31.4 over-cap response via streaming body aborts mid-read (no full buffer / OOM)", async () => {
  const f = guard(64 * 1024); // 64 KiB cap; server streams 1 MiB
  const res = await f(`${base}/?n=${1 << 20}`);
  await expect(res.arrayBuffer()).rejects.toThrow(/cap|exceed|too large/i);
});

test("A31.4 an under-cap response passes through intact", async () => {
  const f = guard(1 << 20); // 1 MiB cap
  const res = await f(`${base}/?n=1024`);
  const buf = new Uint8Array(await res.arrayBuffer());
  expect(buf.length).toBe(1024);
});

test("A31.4 a non-allowlisted host is rejected before any fetch (unchanged guard)", async () => {
  const f = guard(1 << 20);
  await expect(f("http://evil.example/pic.jpg")).rejects.toThrow(/egress/);
});
