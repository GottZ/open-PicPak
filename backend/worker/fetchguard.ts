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

export interface FetchGuardOpts {
  allow: string[];
  proxyUrl: string; // http://faas-egress:8888
  cred: string; // per-call egress credential (Proxy-Authorization)
}

export function makeFetch(opts: FetchGuardOpts): typeof fetch {
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
    return fetch(input, { ...init, headers, proxy: opts.proxyUrl } as RequestInit & { proxy: string });
  }) as typeof fetch;
}
