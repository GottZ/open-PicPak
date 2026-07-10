import { test, expect } from "bun:test";
import sharp from "sharp";
import { readFileSync } from "fs";
import { join } from "path";
import { runFunction, rasterise, sharpFacade, RAW, type Cap, type Ctx } from "./runtime";

// A31.6 — the shipping external-image builtin, driven through the REAL sharp facade. We load the exact
// source that go:embed ships (internal/templatestore/builtins/external-image.js), stub cap.fetch with a
// canned Response (the egress host-allowlist wall is the network-layer proxy, out of this harness), and
// assert the resize→rewrap→removeAlpha path yields the fixed 400x300 raw RGB frame the supervisor packs
// — and that the facade's decompression-bomb floor (A31.4) bites on a fetched bomb.
const externalImageSource = readFileSync(
  join(import.meta.dir, "../internal/templatestore/builtins/external-image.js"),
  "utf8",
);

function capWith(fetchImpl: () => Promise<Response>): Cap {
  return {
    fetch: fetchImpl as unknown as typeof fetch,
    sharp: sharpFacade(),
    secrets: {},
    log: () => {},
  };
}

const ctxFixture: Ctx = { serial: "S1", channel: "c", trigger: { type: "render" }, now: "t" };

test("external-image builtin fetches, resizes an off-size RGBA image to the exact 400x300 raw frame", async () => {
  // A non-panel-sized RGBA source: the builtin owns the resize + raw-rewrap + removeAlpha to 400x300x3.
  const src = await sharp({
    create: { width: 1024, height: 768, channels: 4, background: { r: 10, g: 20, b: 30, alpha: 1 } },
  })
    .png()
    .toBuffer();
  const res = await runFunction(externalImageSource, ctxFixture, capWith(async () => new Response(src)));
  const raw = await rasterise(res.image);
  expect(raw.length).toBe(RAW); // 360000 — exactly what bwry.Pack consumes
  // The return carries ONLY image — no untrusted dither field (dither is trigger_config policy, A31.2).
  expect(res.dither).toBeUndefined();
});

test("external-image builtin: the sharp facade decompression-bomb floor bites on a fetched bomb (A31.4)", async () => {
  // 7000x7000 = 49M px > the 24M facade floor, but a flat fill compresses to a tiny PNG. The builtin
  // passes no limitInputPixels, so only the facade floor stands between it and an OOM on the fetched body.
  const bomb = await sharp({ create: { width: 7000, height: 7000, channels: 3, background: { r: 0, g: 0, b: 0 } } })
    .png()
    .toBuffer();
  await expect(runFunction(externalImageSource, ctxFixture, capWith(async () => new Response(bomb)))).rejects.toThrow(
    /pixel|limit/i,
  );
});
