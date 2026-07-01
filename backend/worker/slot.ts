// slot.ts — run one render in a kill-able subprocess. The subprocess reads the job on stdin and
// writes the M4 render-response PAYLOAD on stdout. A wall-clock deadline kills it with SIGKILL
// (the only reliable JS timeout — a sync while(true) never yields to a timer, §4.4). Returns the
// response payload bytes, or null on timeout / empty output (the daemon then emits a timeout frame).

export interface SlotJob {
  id: string;
  source: string;
  ctx: unknown;
  secrets: Record<string, string>;
  egress_allow: string[];
  egress_cred: string;
  proxy_url: string;
}

export async function runSlot(
  runnerPath: string,
  cwd: string,
  job: SlotJob,
  timeoutMs: number,
): Promise<Uint8Array | null> {
  const proc = Bun.spawn(["bun", runnerPath], { cwd, stdin: "pipe", stdout: "pipe", stderr: "inherit" });
  proc.stdin.write(JSON.stringify(job));
  proc.stdin.end();

  let timedOut = false;
  const killer = setTimeout(() => {
    timedOut = true;
    proc.kill(9); // SIGKILL — stops even a synchronous busy loop
  }, timeoutMs);

  let out: ArrayBuffer;
  try {
    out = await new Response(proc.stdout).arrayBuffer();
  } catch {
    out = new ArrayBuffer(0);
  }
  clearTimeout(killer);
  await proc.exited;

  if (timedOut) return null;
  const u8 = new Uint8Array(out);
  return u8.length >= 4 ? u8 : null;
}
