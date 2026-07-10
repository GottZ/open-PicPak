// builtin/clock — a self-contained render_fn with no egress: draw the current time. A minimal prefab
// that needs no trust profile (empty egress_allow / secret_bindings), so the picker has a zero-config
// example next to the egress-bearing external-image.
export default async (ctx, cap) => {
  const now = new Date().toISOString().slice(11, 16);
  const image = cap.canvas(ctx.width, ctx.height).text(now, { size: 48, align: "center" }).render();
  return { image };
};
