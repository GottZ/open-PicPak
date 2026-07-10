#!/usr/bin/env bash
# Build the Berry WASM simulator artifact (design/33 §4.2, E-A33-7 "build stage,
# not committed artifact"). Runs the PINNED emscripten/emsdk image over the repo
# root (both firmware/ and backend/ live there), then brotli-compresses and writes
# sim-manifest.json. Standalone on purpose: Dockerfile.admin's build context is
# backend/ and cannot reach firmware/ sources, so this is invoked out-of-band
# (locally / in CI) BEFORE the admin image build, which then embeds the artifact
# from backend/web/public via the frontend `vite build` -> //go:embed all:dist.
#
# Gates (each exits non-zero = RED build):
#   - sim/gen-conf.sh  : Berry-conf drift (base sha) + override-set tamper (patch sha)
#   - budget           : .wasm.br must stay <= BUDGET_BYTES (design/33 §6, 512 KB)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
FW="$(cd "$HERE/.." && pwd)"
ROOT="$(cd "$FW/.." && pwd)"

EMSDK_IMAGE="emscripten/emsdk:4.0.16"      # PINNED (E-A33-7); emcc 4.0.16
BERRY_PIN="bd9c93b65dfadddc27e3203fc04e60e986a0fa5f"   # Berry 1.1.0 (setup-berry.sh)
BUDGET_BYTES=${BUDGET_BYTES:-$((512 * 1024))}   # overridable for the red-path probe
OUT_DIR="$ROOT/backend/web/public"
MANIFEST="$HERE/sim-manifest.json"

fail() { echo "BUILD RED: $*" >&2; exit 1; }

# 1. vendored third-party sources (gitignored, fetch-by-pin) + generated font.
[ -f "$FW/components/berry/berry_conf.h" ]     || bash "$FW/scripts/setup-berry.sh"
[ -d "$FW/components/qrcodegen/src" ]           || bash "$FW/scripts/setup-qrcodegen.sh"
[ -f "$FW/main/font16.h" ]                      || python3 "$FW/host/gen_font16.py"

# 2. conf gate + generate sim/berry_conf.h (drift = RED before we spend a build).
bash "$HERE/gen-conf.sh"

# 3. compile in the pinned emsdk container. -I firmware/sim FIRST so the generated
#    sim conf provides berry_conf.h (component root is deliberately NOT on -I).
mkdir -p "$OUT_DIR"
cd "$ROOT"   # so firmware/components/berry/src/*.c expands host-side against the repo root
docker run --rm -v "$ROOT":/src -w /src "$EMSDK_IMAGE" \
  emcc -Os -sSUPPORT_LONGJMP=emscripten -sALLOW_MEMORY_GROWTH \
    -sMODULARIZE -sEXPORT_ES6 \
    "-sEXPORTED_FUNCTIONS=['_sim_reset','_sim_set_dev','_sim_run','_sim_compile_only','_sim_error','_sim_fb','_malloc','_free']" \
    "-sEXPORTED_RUNTIME_METHODS=['ccall','cwrap','UTF8ToString','stringToUTF8','HEAPU8','lengthBytesUTF8']" \
    -I firmware/main -I firmware/components/berry/src -I firmware/components/berry/generate \
    -I firmware/sim -I firmware/components/qrcodegen/src \
    firmware/sim/sim_main.c firmware/main/fb.c firmware/components/qrcodegen/src/qrcodegen.c \
    firmware/components/berry/src/*.c firmware/components/berry/port/be_port.c \
    firmware/components/berry/port/be_modtab.c \
    -o backend/web/public/picpak-berry.mjs

# 4. brotli sibling (vite-plugin-compression2's include filter ignores .wasm, §2.2).
brotli -q 11 -f "$OUT_DIR/picpak-berry.wasm" -o "$OUT_DIR/picpak-berry.wasm.br"

# 5. budget gate.
br_bytes=$(stat -c '%s' "$OUT_DIR/picpak-berry.wasm.br")
echo "picpak-berry.wasm.br = ${br_bytes} bytes (budget ${BUDGET_BYTES})"
[ "$br_bytes" -le "$BUDGET_BYTES" ] || \
  fail ".wasm.br ${br_bytes} > budget ${BUDGET_BYTES} — likely -O0/ASSERTIONS regression"

# 6. manifest (drift-tracking config surface, design/33 §3, W11).
base_sha=$(sha256sum "$FW/components/berry/berry_conf.h" | awk '{print $1}')
patch_sha=$(sha256sum "$HERE/conf-overrides.patch" | awk '{print $1}')
fb_sha=$(sha256sum "$FW/main/fb.c" | awk '{print $1}')
cat > "$MANIFEST" <<JSON
{
  "berry_pin": "${BERRY_PIN}",
  "berry_conf_base_sha256": "${base_sha}",
  "sim_conf_patch_sha256": "${patch_sha}",
  "fb_c_sha256": "${fb_sha}",
  "emsdk_version": "4.0.16",
  "expected_wasm_br_bytes": ${br_bytes}
}
JSON
echo "wrote $MANIFEST"

# 7. surface the manifest to the web bundle so the simulator panel can fetch it at
# /picpak-berry.manifest.json and show the pin (design/33 §3, W-A33.3). A committed copy in public/ is the
# dev fallback (shows the last-known pin without a build); this keeps it in sync at deploy time.
cp "$MANIFEST" "$OUT_DIR/picpak-berry.manifest.json"
echo "copied manifest -> $OUT_DIR/picpak-berry.manifest.json"

echo "BUILD GREEN"
