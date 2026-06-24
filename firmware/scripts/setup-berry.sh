#!/usr/bin/env bash
# Fetch + prepare the Berry interpreter as an ESP-IDF component for this demo.
# Berry is third-party (MIT) and is NOT vendored here; this pins the exact version
# and runs its constant-object codegen (coc), then lays out components/berry/.
set -euo pipefail
PIN=bd9c93b65dfadddc27e3203fc04e60e986a0fa5f   # berry 1.1.0
HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$HERE/.berry-src"
COMP="$HERE/components/berry"

if [ ! -d "$WORK/.git" ]; then
  git clone https://github.com/berry-lang/berry.git "$WORK"
fi
git -C "$WORK" fetch --depth 1 origin "$PIN" 2>/dev/null || true
git -C "$WORK" checkout -q "$PIN"

# coc generates the constant tables the .c files include
mkdir -p "$WORK/generate"
python3 "$WORK/tools/coc/coc" -o "$WORK/generate" "$WORK/src" "$WORK/default" -c "$WORK/default/berry_conf.h"

# assemble the IDF component (sources + generated headers + port + conf)
rm -rf "$COMP"; mkdir -p "$COMP/src" "$COMP/port" "$COMP/generate"
cp "$WORK"/src/*.c "$WORK"/src/*.h            "$COMP/src/"
cp "$WORK"/default/be_port.c "$WORK"/default/be_modtab.c "$COMP/port/"
cp "$WORK"/generate/*.h                        "$COMP/generate/"
cp "$WORK"/default/berry_conf.h                "$COMP/berry_conf.h"
cat > "$COMP/CMakeLists.txt" <<'CMAKE'
# Berry 1.1.0 (pinned) as an ESP-IDF component (assembled by scripts/setup-berry.sh).
file(GLOB BERRY_SRCS ${CMAKE_CURRENT_SOURCE_DIR}/src/*.c)
idf_component_register(
    SRCS ${BERRY_SRCS} "port/be_port.c" "port/be_modtab.c"
    INCLUDE_DIRS "src" "generate" ".")
# upstream third-party C — don't let IDF's -Werror fail its build
target_compile_options(${COMPONENT_LIB} PRIVATE -w)
CMAKE
echo "Berry component ready at components/berry (pin ${PIN:0:7})"
