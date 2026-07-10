import { test, expect } from "bun:test";
import sharp from "sharp";
import { runFunction, rasterise, sharpFacade, RAW, type Ctx, type Cap } from "./runtime";

// The built-in __playlist source is a server-authored constant generated Go-side (playlistframe.go
// playlistSource). This mirrors it verbatim to prove the ctx.input plumbing + resize→rasterise path
// yields the fixed 400x300 raw frame the supervisor packs — the one place the built-in render is
// exercised end-to-end (the Go gates stub the worker drive).
function playlistSource(fit: string): string {
  return `export default async (ctx, cap) => {
  const b = await cap.sharp(ctx.input).resize(400, 300, { fit: ${JSON.stringify(fit)} }).removeAlpha().raw().toBuffer();
  return { image: cap.sharp(b, { raw: { width: 400, height: 300, channels: 3 } }) };
}`;
}

async function drive(fit: string, input: Uint8Array): Promise<Uint8Array> {
  const ctx: Ctx = { serial: "S", channel: "c", trigger: { type: "render" }, now: "t", input };
  const cap: Cap = {
    fetch: (() => {
      throw new Error("no egress in this test");
    }) as unknown as typeof fetch,
    sharp: sharpFacade(),
    secrets: {},
    log: () => {},
  };
  const res = await runFunction(playlistSource(fit), ctx, cap);
  return rasterise(res.image);
}

test("built-in __playlist source resizes an off-size source image to the exact 400x300 raw frame", async () => {
  // A source image that is NOT panel-sized: the built-in source owns the resize to 400x300.
  const src = await sharp({ create: { width: 800, height: 600, channels: 3, background: { r: 10, g: 20, b: 30 } } })
    .png()
    .toBuffer();
  for (const fit of ["cover", "contain", "fill"]) {
    const raw = await drive(fit, new Uint8Array(src));
    expect(raw.length).toBe(RAW); // 360000 — exactly what bwry.Pack consumes
  }
});
