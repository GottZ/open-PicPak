// builtin/external-image — Deliverable 4 flagship: fetch an external image from the pinned egress
// host, convert to the device colour space, and return the packed frame. {{url}} is substituted at
// apply time (W6); the egress host is fixed by the template's egress_allow (not parametrisable, §4.4).
export default async (ctx, cap) => {
  const res = await cap.fetch("{{url}}");
  const buf = await res.arrayBuffer();
  const image = await cap.sharp(buf).resize(ctx.width, ctx.height).toColorSpace("bwry");
  return { image, dither: true };
};
