#!/usr/bin/env bash
# Fetch + prepare Nayuki's QR-Code-generator (C) as an ESP-IDF component.
# It is third-party (MIT) and NOT vendored here; this pins the exact version and lays out
# components/qrcodegen/. Same fetch-by-pin pattern as setup-berry.sh / setup-littlefs.sh.
set -euo pipefail
PIN=7ad95cedd8464a87f82221283612732ae4f3f305   # nayuki QR-Code-generator v1.8.0
HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$HERE/.qrcodegen-src"
COMP="$HERE/components/qrcodegen"

if [ ! -d "$WORK/.git" ]; then
  git clone https://github.com/nayuki/QR-Code-generator.git "$WORK"
fi
git -C "$WORK" fetch --depth 1 origin "$PIN" 2>/dev/null || git -C "$WORK" fetch origin
git -C "$WORK" checkout -q "$PIN"

# only the C library (2 files) is needed; they carry the MIT header inline.
rm -rf "$COMP"; mkdir -p "$COMP/src"
cp "$WORK/c/qrcodegen.c" "$WORK/c/qrcodegen.h" "$COMP/src/"
[ -f "$WORK/Readme.markdown" ] && cp "$WORK/Readme.markdown" "$COMP/" || true

cat > "$COMP/CMakeLists.txt" <<'CMAKE'
# Nayuki QR-Code-generator (MIT, pinned) as an ESP-IDF component (assembled by scripts/setup-qrcodegen.sh).
idf_component_register(SRCS "src/qrcodegen.c" INCLUDE_DIRS "src")
# upstream third-party C — keep the firmware's -Werror off it
target_compile_options(${COMPONENT_LIB} PRIVATE -w)
CMAKE

echo "qrcodegen component ready at components/qrcodegen (pin ${PIN:0:7} = v1.8.0)"
