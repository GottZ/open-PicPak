#!/usr/bin/env bash
# Fetch + prepare esp_littlefs as an ESP-IDF component for this firmware.
# esp_littlefs is third-party (joltwallet, MIT) wrapping the littlefs core
# (littlefs-project, BSD-3-Clause, a git submodule) and is NOT vendored here;
# this pins the exact version and lays out components/littlefs/.
set -euo pipefail
PIN=8307cab1d201920f84359d207f82f5d1d33944c7   # esp_littlefs v1.9.2 (submodule littlefs is gitlink-pinned at this tag)
HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$HERE/.littlefs-src"
COMP="$HERE/components/littlefs"

if [ ! -d "$WORK/.git" ]; then
  git clone https://github.com/joltwallet/esp_littlefs.git "$WORK"
fi
git -C "$WORK" fetch --depth 1 origin "$PIN" 2>/dev/null || git -C "$WORK" fetch origin
git -C "$WORK" checkout -q "$PIN"
# the littlefs core is a submodule (src/littlefs) — its version is the gitlink recorded at $PIN
git -C "$WORK" submodule update --init --recursive

# assemble the IDF component: esp_littlefs ships its own CMakeLists/Kconfig/project_include
# (we keep them as-is) + the littlefs core sources from the submodule. No idf_component.yml
# (stays a pure local component, no component-manager fetch at build time).
rm -rf "$COMP"; mkdir -p "$COMP"
cp -R "$WORK/CMakeLists.txt" "$WORK/Kconfig" "$WORK/project_include.cmake" \
      "$WORK/LICENSE" "$WORK/include" "$WORK/src" "$COMP/"
# the BSD-3-Clause core license travels with the submodule (src/littlefs/LICENSE.md) — leave it in place.

# upstream third-party C — keep the firmware's -Werror=all off its sources (same as berry).
cat >> "$COMP/CMakeLists.txt" <<'CMAKE'

# vendored third-party — don't let the firmware's -Werror fail the upstream sources
target_compile_options(${COMPONENT_LIB} PRIVATE -w)
CMAKE

echo "littlefs component ready at components/littlefs (pin ${PIN:0:7} = v1.9.2)"
