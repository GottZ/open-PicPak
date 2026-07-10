// gen_atkinson_golden.mjs — regenerate the CROSS-CONFORMANCE golden for the
// Atkinson dither path directly from the JS reference implementation in
// documentation/image-pipeline.md:116-155 (copied VERBATIM below, between the
// reference markers). Run once with bun or node from this directory:
//
//     bun run gen_atkinson_golden.mjs        # or: node gen_atkinson_golden.mjs
//
// The golden it writes is the byte oracle the Go test (dither_test.go) locks
// PackWithDither(_, DitherAtkinson) against. It is produced by the JS reference,
// NOT derived from the Go code — that is the whole point of a cross-conformance
// golden (the Go engine must reproduce the reference's exact bytes).
//
// Input:  fixture.bin — 400*300*3 raw RGB, row-major, upright — the SAME fixture
//         the Go golden test feeds through FromRGB.
// Output: golden_atkinson.bin — 30000 bytes.

import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));

// ===== BEGIN reference (image-pipeline.md:116-155, verbatim) =====
const W = 400, H = 300;
const PAL = [[0,0,0],[255,255,255],[255,255,0],[255,0,0]];
const LW  = [0.299, 0.587, 0.114];                  // BT.601

function toPicPak(rgba) {
  // float work buffers (upright); error accumulates UNclamped
  const r = new Float32Array(W*H), g = new Float32Array(W*H), b = new Float32Array(W*H);
  for (let i = 0; i < W*H; i++) { r[i]=rgba[i*4]; g[i]=rgba[i*4+1]; b[i]=rgba[i*4+2]; }
  const code = new Uint8Array(W*H);
  const K = [[1,0],[2,0],[-1,1],[0,1],[1,1],[0,2]]; // Atkinson, 1/8 each

  for (let y = 0; y < H; y++) for (let x = 0; x < W; x++) {
    const i = y*W + x, R = r[i], G = g[i], B = b[i];
    let best = 0, bd = Infinity;
    for (let k = 0; k < 4; k++) {
      const dr=R-PAL[k][0], dg=G-PAL[k][1], db=B-PAL[k][2];
      const d = LW[0]*dr*dr + LW[1]*dg*dg + LW[2]*db*db;
      if (d < bd) { bd = d; best = k; }
    }
    code[i] = best;
    const eR=(R-PAL[best][0])/8, eG=(G-PAL[best][1])/8, eB=(B-PAL[best][2])/8;
    for (const [dx,dy] of K) {
      const nx=x+dx, ny=y+dy;
      if (nx>=0 && nx<W && ny>=0 && ny<H) { const j=ny*W+nx; r[j]+=eR; g[j]+=eG; b[j]+=eB; }
    }
  }

  // vertical flip + pack (2 bpp, MSB-first)
  const out = new Uint8Array(W*H/4);
  let o = 0;
  for (let y = H-1; y >= 0; y--) for (let x = 0; x < W; x += 4) {
    const base = y*W + x;
    out[o++] = (code[base]<<6)|(code[base+1]<<4)|(code[base+2]<<2)|code[base+3];
  }
  return out; // 30000 bytes
}
// ===== END reference =====

const rgb = readFileSync(join(here, "fixture.bin"));
if (rgb.length !== W * H * 3) {
  throw new Error(`fixture.bin ${rgb.length} != ${W * H * 3}`);
}
// The reference consumes RGBA; the fixture is RGB (the Go rgbImage treats every
// pixel as fully opaque, so alpha is irrelevant to the conversion).
const rgba = new Uint8ClampedArray(W * H * 4);
for (let i = 0; i < W * H; i++) {
  rgba[i * 4] = rgb[i * 3];
  rgba[i * 4 + 1] = rgb[i * 3 + 1];
  rgba[i * 4 + 2] = rgb[i * 3 + 2];
  rgba[i * 4 + 3] = 255;
}
const out = toPicPak(rgba);
if (out.length !== W * H / 4) {
  throw new Error(`out ${out.length} != ${W * H / 4}`);
}
writeFileSync(join(here, "golden_atkinson.bin"), Buffer.from(out));
console.log(`wrote golden_atkinson.bin (${out.length} bytes)`);
