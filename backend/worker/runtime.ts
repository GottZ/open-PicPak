// runtime.ts — evaluate an operator function in a CURATED SCOPE and rasterise its returned
// image to raw 400x300 RGB. The curated scope is HYGIENE, not a security boundary: a
// Function-constructor or dynamic import() defeats lexical scoping, so it CANNOT deny
// fs/child_process/require/net (D24.7/D24.11). The two HARD walls are elsewhere — the process
// boundary (no SECRETS_KEY, no DB route) and the egress proxy (no default route) — both
// enforced by the container/network, wired in W5. This file provides the casual-path scope +
// friendly errors + the sharp buffer-facade (defense-in-depth), and the raw-RGB rasteriser.
import sharp, { type Sharp } from "sharp";

export const W = 400;
export const H = 300;
export const RAW = W * H * 3; // 360000

export interface Ctx {
  serial: string;
  channel: string;
  trigger: { type: string; payload?: unknown };
  now: string;
}

export interface Cap {
  fetch: typeof fetch;
  sharp: (input?: unknown, opts?: unknown) => Sharp;
  secrets: Record<string, string>;
  log: (lvl: string, msg: string) => void;
}

export interface FnResult {
  image: unknown;
  dither?: string;
  next_wake_hint?: number;
}

export class OffSizeError extends Error {}

// MAX_INPUT_PIXELS is the hard decoded-pixel ceiling the sharp facade enforces on EVERY input —
// a decompression-bomb guard (A31.4, design §5). It mirrors the server-side blob-store ceiling
// `imgstore.MaxImagePixels` (backend/internal/imgstore/types.go, 24_000_000 ≈ 40× the 400×300
// panel target): a byte cap alone cannot catch a few-hundred-KB PNG that decodes to gigabytes, so
// the wall is on width*height read from the header. Kept in lockstep with that Go constant.
export const MAX_INPUT_PIXELS = 24_000_000;

// clampInputPixels forces `limitInputPixels` to the floor regardless of what the (operator-authored,
// untrusted) template passes: sharp's default is ~268M px and `false`/`0`/`true` disable or reset the
// limit, so a template could otherwise widen or drop the ceiling. A caller may only make the ceiling
// TIGHTER (a smaller positive number survives); anything larger, absent, or a limit-disabling value is
// pinned to MAX_INPUT_PIXELS. The author cannot edit the floor away (design §4.3/§5).
function clampInputPixels(opts?: unknown): Record<string, unknown> {
  const o = opts && typeof opts === "object" ? { ...(opts as Record<string, unknown>) } : {};
  const caller = o.limitInputPixels;
  o.limitInputPixels =
    typeof caller === "number" && caller > 0 && caller <= MAX_INPUT_PIXELS ? caller : MAX_INPUT_PIXELS;
  return o;
}

// isBinaryInput reports whether `input` is compressed bytes to be DECODED (Buffer / ArrayBuffer /
// typed-array view). That is the only decompression-bomb vector — a `{create}`/`{raw}`/`{text}`
// descriptor declares its own dimensions and generates in-memory, and sharp forbids passing a
// separate options object alongside such a descriptor, so the pixel floor is injected ONLY on the
// binary decode path.
function isBinaryInput(input: unknown): boolean {
  return input instanceof Uint8Array || input instanceof ArrayBuffer || ArrayBuffer.isView(input);
}

// sharpFacade wraps sharp as buffer-in/buffer-out (fix-8): a string/path input throws, the
// filesystem sink toFile is removed, and on the binary-decode path `limitInputPixels` is pinned to
// MAX_INPUT_PIXELS (decompression-bomb floor, A31.4). Defense-in-depth — a determined function can
// re-import sharp; that fs path is the M2 read-only-rootfs/seccomp job, not this facade's.
export function sharpFacade(real: typeof sharp = sharp): Cap["sharp"] {
  return (input?: unknown, opts?: unknown): Sharp => {
    if (typeof input === "string") {
      throw new Error("sharp: path/string input is not allowed (buffer or {create} only)");
    }
    const inst = isBinaryInput(input)
      ? real(input as never, clampInputPixels(opts) as never)
      : real(input as never, opts as never);
    (inst as unknown as { toFile: () => never }).toFile = () => {
      throw new Error("sharp: toFile is not allowed (no filesystem sink)");
    };
    return inst;
  };
}

// runFunction compiles the operator source in the curated scope and invokes its default export
// `async (ctx, cap) => ({ image, dither?, next_wake_hint? })`.
export async function runFunction(source: string, ctx: Ctx, cap: Cap): Promise<FnResult> {
  const transformed = source.replace(/export\s+default\s+/, "__default = ");
  if (transformed === source) {
    throw new Error("function must 'export default' an async (ctx, cap) => {...}");
  }
  const body =
    '"use strict";\n' +
    // shadow the obvious ambient globals (hygiene — clear errors on the casual path).
    "const require = undefined, process = undefined, Bun = undefined, globalThis = undefined,\n" +
    "      global = undefined, module = undefined, exports = undefined,\n" +
    "      __dirname = undefined, __filename = undefined;\n" +
    "const { fetch, sharp, secrets, log } = cap;\n" +
    "let __default;\n" +
    transformed +
    "\n;if (typeof __default !== 'function') throw new Error('default export must be a function');\n" +
    "return __default(ctx, cap);";
  const factory = new Function("ctx", "cap", body);
  return (await factory(ctx, cap)) as FnResult;
}

function isSharp(x: unknown): x is Sharp {
  return !!x && typeof (x as Sharp).toBuffer === "function" && typeof (x as Sharp).metadata === "function";
}

// rasterise turns the function's returned image into raw 400x300 RGB (removeAlpha => 360000).
// Off-size is a render ERROR (the function owns any resize; the supervisor never re-fits, D24.3).
export async function rasterise(image: unknown): Promise<Uint8Array> {
  if (image == null) throw new Error("function returned no image");
  const inst: Sharp = isSharp(image) ? image : sharp(image as never);
  const meta = await inst.metadata();
  if (meta.width !== W || meta.height !== H) {
    throw new OffSizeError(`image ${meta.width}x${meta.height} != ${W}x${H}`);
  }
  const raw = await inst.removeAlpha().raw().toBuffer();
  if (raw.length !== RAW) throw new Error(`raw ${raw.length} != ${RAW}`);
  return new Uint8Array(raw);
}
