# PicPak — Image Pipeline (RGB → display)

The exact RGB→display conversion the vendor app performs before uploading an image:
resize, palette quantization, dither, vertical flip, and 2 bpp packing. This is what a
custom editor/uploader must reproduce to get the same on-screen result.

This is an **app-side** algorithm, not a device property — the firmware just stores
and displays the 30 000-byte buffer it receives (upload framing is in
[`ble-protocol.md` §6](ble-protocol.md); panel and palette are in
[`device.md` §4](device.md)).

Source: disassembly of the official app (`libapp.so`, Dart AOT) plus device captures
(upload an image → `esptool read_flash 0x350000` → decode the 2 bpp buffer). Marked
binary-verified `[bin]` or measured on hardware `[hw]`.

> **Air-gap note.** Nothing device-specific appears here. The hex offsets in §8 are RE
> artifacts of the public app binary. See the stub conventions in [`device.md`](device.md).

---

## 1. Pipeline order

```
input image
  → 1. resize to 400×300            (fit policy is the editor's UX choice)
  → 2. quantize per pixel           (nearest palette color, luma-weighted RGB distance)
  → 3. Atkinson error diffusion     (dither)
  → 4. vertical flip                (reverse row order)
  → 5. pack 2 bpp, 4 px/byte, MSB-first  → 30 000 bytes
  → 6. upload                       (BLE image-write, ble-protocol.md §6)
```

Steps 2 + 3 run on the **upright** image; the flip (4) comes afterward. `[bin]`

---

## 2. Palette (4 colors, 2 bpp)

| Code | Logical (mapping anchor) | Panel actual (preview only) |
|---|---|---|
| 0 | black `(0,0,0)` | `(18,20,18)` |
| 1 | white `(255,255,255)` | `(205,206,198)` |
| 2 | yellow `(255,255,0)` | `(202,174,62)` |
| 3 | red `(255,0,0)` | `(164,63,55)` |

**No green, no blue, no grey.** Use the *logical* anchors for the mapping math; render
the *panel-actual* colors only for a realistic editor preview. `[hw]`

---

## 3. Step 2 — Quantization (nearest color)

Luma-weighted squared RGB distance, weights = **ITU-R BT.601**: `[bin]`

```
dist²(C, palette_k) = 0.299·(R−Rk)² + 0.587·(G−Gk)² + 0.114·(B−Bk)²
code = argmin_k dist²        (k over the 4 palette colors)
```

This is **not** CIELAB and **not** unweighted RGB. (An earlier note elsewhere claimed
"Euclidean RGB distance" — that is wrong; the weighting is what reproduces the observed
behavior.) The weighting explains exactly what the panel does: `[hw]`

| Input | Result | Why |
|---|---|---|
| blue `(0,0,B)`, any brightness | **always black** | weight `0.114` is tiny → black is nearest at every level |
| green `(0,G,0)` | black up to **G ≈ 192**, then yellow-dither | yellow wins only when `0.299·255² < 0.587·(510G − 255²)` |
| red `(k,0,0)` / yellow `(k,k,0)` | black up to **k ≈ 128**, then red/yellow | symmetric (the weighted channel cancels) |

---

## 4. Step 3 — Dither (Atkinson)

Atkinson error diffusion, error computed in sRGB code values (0..255, **not**
linearized): `[bin]`

```
err = (R,G,B) − palette[code]                         # per channel
distribute err × 1/8 to 6 neighbors:
   (x+1, y)  (x+2, y)  (x−1, y+1)  (x, y+1)  (x+1, y+1)  (x, y+2)
```

**Critical: NO clamping** of the accumulated error before the next distance
computation. The unclamped negative red error (green→yellow produces a −255 R error)
drags neighbors negative, so they fall to black. That is why green stays conservative
and dark (solid green → ~83% yellow / 17% black). `[bin]`

(The app also offers Floyd–Steinberg internally, but the **default — and what was
measured on the device — is Atkinson**.) `[hw]`

---

## 5. Step 4 — Vertical flip

Reverse the row order before packing (`out[y] = img[H−1−y]`). The storage format
expects the image vertically mirrored. `[hw]`

---

## 6. Step 5 — Packing (`pack_pixel_codes`)

2 bits/pixel, 4 px/byte, **MSB-first**, row-major over the flipped image → exactly
**30 000 bytes** (`400 × 300 ÷ 4`):

```
byte = (c0<<6) | (c1<<4) | (c2<<2) | c3       # c0 = leftmost pixel of the 4-px block
```

---

## 7. Reference implementation (JS, Web-USB-ready)

Verified against device captures. Input is canvas `ImageData` already resized to
400×300; output is the 30 000-byte buffer ready for BLE image-write.

```js
// in:  Uint8ClampedArray RGBA, exactly 400*300*4 (resized)
// out: Uint8Array(30000)
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
```

---

## 8. Verification & provenance

**Disassembly** (`libapp.so`, Dart AOT, arm64; conversion function @ `0x4195a4`): `[bin]`
- BT.601 weights as double constants in the object pool — `0.299`, `0.587`, `0.114`
  (consecutive); white level `255` nearby. The `fmul`/`fsub`/`fadd` chain forms
  `Σ wᵢ·Δᵢ²`.
- Atkinson divisor `1/8`: `fmov d3, 0.125` @ `0x419860`; 6-neighbor kernel.
- `argmin` over 4 distances via an `fcmp` chain; **no clamp instruction** before the
  distance computation.
- Palette `(0,0,0)/(255,255,255)/(255,255,0)/(255,0,0)` in the pool.
- Tools: `blutter` (Dart 3.12) for pool/structure, `radare2` for the async function
  body (blutter does not dump async bodies).

**Device captures** (official app → upload → `esptool read_flash 0x350000` → 2 bpp
decode): 3 test charts (swatches; grey/green/blue ramps; yellow/red/green and
green→yellow ramps). The model reproduces every axis within ≈ ±5%. Pixel-exact match
is impossible in principle — Atkinson noise decorrelates under any minimal pipeline
difference — but ratios and structure match. `[hw]`

**Open (not disambiguated):** the exact dither↔flip order. Horizontal test ramps are
flip-symmetric, so they can't distinguish it; the order above (dither upright, then
flip) is consistent with the captures.
