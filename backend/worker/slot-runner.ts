// slot-runner.ts — a per-call kill-able slot (a subprocess, not a Worker: sharp's native binding
// runs cleanly in a normal process, and an OS kill(9) reliably stops even a synchronous while(true)
// — a Web Worker's terminate() cannot). Reads one job as JSON on stdin, evaluates the operator
// function in the curated scope, rasterises to raw RGB, and writes the M4 render-response PAYLOAD
// (u32be metaLen | meta | raw) to stdout, then exits. This per-call subprocess IS the shape M2's
// isolation takes (§4.7) — the M4 protocol does not change.
import { runFunction, rasterise, sharpFacade, OffSizeError, type Ctx } from "./runtime";
import { makeFetch } from "./fetchguard";
import { encodeResponse, type ResponseMeta } from "./frame";

interface Job {
  id: string;
  source: string;
  ctx: Ctx;
  secrets: Record<string, string>;
  egress_allow: string[];
  egress_cred: string;
  proxy_url: string;
}

const logs: { lvl: string; msg: string }[] = [];
let jobId = "?";
let payload: Uint8Array;

try {
  const job = JSON.parse(await Bun.stdin.text()) as Job;
  jobId = job.id;
  const cap = {
    fetch: makeFetch({ allow: job.egress_allow, proxyUrl: job.proxy_url, cred: job.egress_cred }),
    sharp: sharpFacade(),
    secrets: job.secrets,
    log: (lvl: string, msg: string) => logs.push({ lvl, msg }),
  };
  const res = await runFunction(job.source, job.ctx, cap);
  const raw = await rasterise(res.image);
  const meta: ResponseMeta = {
    v: 1,
    id: jobId,
    ok: true,
    w: 400,
    h: 300,
    dither: res.dither,
    next_wake_hint: res.next_wake_hint,
    log: logs,
    err: null,
  };
  payload = encodeResponse(meta, raw);
} catch (err) {
  const kind = err instanceof OffSizeError ? "offsize" : "throw";
  const meta: ResponseMeta = {
    v: 1,
    id: jobId,
    ok: false,
    w: 400,
    h: 300,
    log: logs,
    err: { kind, msg: err instanceof Error ? err.message : String(err) },
  };
  payload = encodeResponse(meta, null);
}

await Bun.write(Bun.stdout, payload);
