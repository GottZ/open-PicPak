// fetchguard.ts — the worker's `cap.fetch`. This is DEFENSE-IN-DEPTH, NOT the wall (D24.8): the
// real egress guarantee is the network layer — the worker container has no default route, so ALL
// egress (this fetch, a globalThis.fetch escape, a raw node:net socket) can only reach the
// faas-egress proxy, which IP-pins the post-resolve connect IP. Here we (a) reject a non-allowlisted
// host early with a friendly error, and (b) route via the proxy carrying the per-call credential.

export function hostAllowed(host: string, port: string, allow: string[]): boolean {
  for (const a of allow) {
    if (a === host || a === `${host}:${port}`) return true;
  }
  return false;
}

// DEFAULT_MAX_RESPONSE_BYTES caps how large a fetched body cap.fetch will admit into the slot
// (A31.4, design §5). `arrayBuffer()` on an unbounded response pulls a multi-GB reply straight into
// slot memory and OOMs the whole worker container inside the render timeout — the wall-clock kill is
// too slow for a fast allocator. 32 MiB comfortably exceeds any legitimate 400×300 source image yet
// stays far under the container mem_limit belt. The mem_limit (compose) is the outer belt; this is
// the early, named abort.
export const DEFAULT_MAX_RESPONSE_BYTES = 32 * 1024 * 1024;

export interface FetchGuardOpts {
  allow: string[];
  proxyUrl: string; // http://faas-egress:8888
  cred: string; // per-call egress credential (Proxy-Authorization)
  maxBytes?: number; // response-size cap; defaults to DEFAULT_MAX_RESPONSE_BYTES
}

// capBody wraps a response body stream so that reading past `cap` bytes aborts with a named error
// instead of buffering the whole reply. Catches chunked / missing / lying Content-Length — never
// more than one chunk beyond the cap is held.
function capBody(body: ReadableStream<Uint8Array>, cap: number): ReadableStream<Uint8Array> {
  const reader = body.getReader();
  let total = 0;
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      const { done, value } = await reader.read();
      if (done) {
        controller.close();
        return;
      }
      total += value.byteLength;
      if (total > cap) {
        await reader.cancel();
        controller.error(new Error(`fetch: response exceeded cap ${cap} bytes`));
        return;
      }
      controller.enqueue(value);
    },
    cancel(reason) {
      return reader.cancel(reason);
    },
  });
}

export function makeFetch(opts: FetchGuardOpts): typeof fetch {
  const cap = opts.maxBytes ?? DEFAULT_MAX_RESPONSE_BYTES;
  return (async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
    const raw = typeof input === "string" ? input : input instanceof URL ? input.href : (input as Request).url;
    const url = new URL(raw);
    const port = url.port || (url.protocol === "https:" ? "443" : "80");
    if (!hostAllowed(url.hostname, port, opts.allow)) {
      throw new Error(`egress: ${url.hostname}:${port} not in egress_allow`);
    }
    const headers = new Headers(init?.headers);
    headers.set("Proxy-Authorization", `Bearer ${opts.cred}`);
    // Bun routes the request through the forward proxy; the proxy re-validates at the network
    // layer (the load-bearing check). The allowlist check above is only an early, friendly reject.
    // An empty proxyUrl means "no proxy" (test-only direct connect).
    const reqInit = { ...init, headers } as RequestInit & { proxy?: string };
    if (opts.proxyUrl) reqInit.proxy = opts.proxyUrl;
    const res = await fetch(input, reqInit);
    // (a) honest Content-Length → reject early, before a single body byte is read.
    const cl = res.headers.get("content-length");
    if (cl !== null && Number(cl) > cap) {
      throw new Error(`fetch: response too large (content-length ${cl} > cap ${cap} bytes)`);
    }
    // (b) chunked / missing / lying Content-Length → enforce the cap while streaming.
    if (!res.body) return res;
    const outHeaders = new Headers(res.headers);
    outHeaders.delete("content-length"); // body is now a re-chunked stream; the old length is stale
    return new Response(capBody(res.body, cap), {
      status: res.status,
      statusText: res.statusText,
      headers: outHeaders,
    });
  }) as typeof fetch;
}
