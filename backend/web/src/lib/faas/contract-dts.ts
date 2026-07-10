// contract-dts.ts — the cap/ctx/return contract the FaaS operator editor type-checks against.
//
// This string is loaded into the in-editor TypeScript environment (ts-service.worker.ts) as a GLOBAL
// ambient declaration file — it has no import/export, so the interfaces are global and visible to the
// operator's `export default async (ctx, cap) => …` after it is wrapped for contextual typing (ts-wrap.ts).
//
// It MUST mirror the REAL worker runtime surface in backend/worker/runtime.ts (interfaces Cap / Ctx /
// FnResult). The bidirectional drift anchor contract-surface-golden.test.ts fails if a top-level member is
// added on one side only. The sub-shapes (sharp's chain, fetch) are pragmatic author-facing approximations,
// NOT security types — the real hard walls are the process/egress boundaries (runtime.ts header, D24.7).
export const FAAS_CONTRACT_DTS = `
interface FaasCtx {
  serial: string;
  channel: string;
  trigger: { type: string; payload?: unknown };
  now: string;
  input?: Uint8Array;
}

interface FaasSharpImage {
  resize(width?: number, height?: number, opts?: unknown): FaasSharpImage;
  extend(opts: unknown): FaasSharpImage;
  composite(images: unknown[]): FaasSharpImage;
  removeAlpha(): FaasSharpImage;
  ensureAlpha(alpha?: number): FaasSharpImage;
  flatten(opts?: unknown): FaasSharpImage;
  grayscale(on?: boolean): FaasSharpImage;
  rotate(angle?: number, opts?: unknown): FaasSharpImage;
  raw(opts?: unknown): FaasSharpImage;
  png(opts?: unknown): FaasSharpImage;
  jpeg(opts?: unknown): FaasSharpImage;
  toColourspace(colourspace: string): FaasSharpImage;
  metadata(): Promise<{ width?: number; height?: number; format?: string; channels?: number }>;
  toBuffer(): Promise<Uint8Array>;
}

interface FaasCap {
  fetch: typeof fetch;
  sharp: (input?: unknown, opts?: unknown) => FaasSharpImage;
  secrets: Record<string, string>;
  log: (lvl: string, msg: string) => void;
}

interface FaasResult {
  image: unknown;
  dither?: string;
  next_wake_hint?: number;
}
`
