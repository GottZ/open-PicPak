import { test, expect } from "bun:test";
import sharp from "sharp";
import { runFunction, rasterise, sharpFacade, OffSizeError, type Cap, type Ctx } from "./runtime";

const ctxFixture: Ctx = { serial: "S1", channel: "c", trigger: { type: "render" }, now: "2026-07-01T00:00:00Z" };
function capFixture(): Cap {
  return {
    fetch: (async () => new Response("")) as unknown as typeof fetch,
    sharp: sharpFacade(),
    secrets: {},
    log: () => {},
  };
}

test("sharp facade rejects string/path input (buffer-only, fix-8)", () => {
  expect(() => sharpFacade()("/etc/passwd")).toThrow(/not allowed/);
});

test("sharp facade removes the toFile filesystem sink", () => {
  const inst = sharpFacade()({ create: { width: 1, height: 1, channels: 3, background: { r: 0, g: 0, b: 0 } } });
  expect(() => (inst as unknown as { toFile: (p: string) => void }).toFile("x.png")).toThrow(/toFile/);
});

test("curated scope hides ambient globals (hygiene)", async () => {
  const src =
    'export default async (ctx, cap) => ({ image: null, next_wake_hint: ' +
    '(typeof process === "undefined" && typeof require === "undefined" && typeof Bun === "undefined") ? 1 : 2 })';
  const res = await runFunction(src, ctxFixture, capFixture());
  expect(res.next_wake_hint).toBe(1);
});

test("runFunction requires a default export", async () => {
  await expect(runFunction("const x = 1;", ctxFixture, capFixture())).rejects.toThrow();
});

test("runFunction sees ctx + caps and returns an image", async () => {
  const src =
    'export default async (ctx, cap) => { cap.log("info", "hi " + ctx.serial); ' +
    'return { image: cap.sharp({create:{width:400,height:300,channels:3,background:{r:255,g:255,b:0}}}), dither: "none" }; }';
  const res = await runFunction(src, ctxFixture, capFixture());
  expect(res.dither).toBe("none");
  const raw = await rasterise(res.image);
  expect(raw.length).toBe(360000);
});

test("rasterise produces raw 360000 RGB for a 400x300 image", async () => {
  const img = sharp({ create: { width: 400, height: 300, channels: 3, background: { r: 1, g: 2, b: 3 } } });
  const raw = await rasterise(img);
  expect(raw.length).toBe(360000);
  expect([raw[0], raw[1], raw[2]]).toEqual([1, 2, 3]);
});

test("rasterise rejects off-size (the function owns any resize, no re-fit, D24.3)", async () => {
  const img = sharp({ create: { width: 100, height: 100, channels: 3, background: { r: 0, g: 0, b: 0 } } });
  await expect(rasterise(img)).rejects.toBeInstanceOf(OffSizeError);
});

test("rasterise decodes an encoded image buffer of the right size", async () => {
  const png = await sharp({ create: { width: 400, height: 300, channels: 3, background: { r: 9, g: 8, b: 7 } } })
    .png()
    .toBuffer();
  const raw = await rasterise(png);
  expect(raw.length).toBe(360000);
  expect([raw[0], raw[1], raw[2]]).toEqual([9, 8, 7]);
});
