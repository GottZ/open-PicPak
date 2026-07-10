// builtin/external-image — Deliverable-4 flagship: fetch ONE external image from the pinned egress
// host and hand it back as the raw 400x300 RGB frame the rasteriser expects. {{url}} is substituted at
// apply time (W6); the egress host is fixed by the template's egress_allow, not parametrisable (§4.4).
//
// The return is ONLY { image }. Dither is TRUSTED trigger_config policy (A31.2), resolved and applied
// in the supervisor (bwry.PackWithDither) — NOT the template's to choose: an untrusted `dither` in the
// return would be a Policy=Data override and is deliberately absent. There is likewise no colour-space
// conversion here — the worker yields RGB and the trusted supervisor owns the BWRY pack.
//
// It round-trips the resize through a raw 400x300x3 buffer and re-wraps it, exactly like the built-in
// __playlist source: the rasteriser reads sharp.metadata() BEFORE toBuffer(), and a pending .resize()
// does NOT change metadata() (it reports the INPUT dimensions), so a bare .resize(400,300) image would
// fail the off-size check. Re-wrapping the raw buffer makes metadata() report the true 400x300.
export default async (ctx, cap) => {
  const res = await cap.fetch("{{url}}");
  const buf = await res.arrayBuffer();
  const b = await cap.sharp(buf).resize(400, 300, { fit: "cover" }).removeAlpha().raw().toBuffer();
  return { image: cap.sharp(b, { raw: { width: 400, height: 300, channels: 3 } }) };
};
