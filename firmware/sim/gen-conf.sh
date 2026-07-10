#!/usr/bin/env bash
# Generate sim/berry_conf.h from the firmware Berry conf + the declared override
# patch, and GATE on drift (design/33 §4.2, S6). The sim build disables
# FILE_SYSTEM/OS/SYS/SHARED_LIB for real (a -D override is a no-op — the header
# defines the switches unconditionally, design/33 §2.1). The gate is diff-based:
#
#   - base_sha256(firmware berry_conf.h) MUST equal the pinned sim/berry_conf.base.sha256
#     -> catches a Berry version/config bump that the sim didn't follow (S6 drift).
#   - sha256(sim/conf-overrides.patch) MUST equal the pinned .patch.sha256
#     -> catches tampering with the declared override set.
#
# Any undeclared deviation exits non-zero (RED build). On success, sim/berry_conf.h
# is (re)generated; put -I firmware/sim ahead of the Berry include path so it wins.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
FW="$(cd "$HERE/.." && pwd)"
BASE="$FW/components/berry/berry_conf.h"
PATCH="$HERE/conf-overrides.patch"
OUT="$HERE/berry_conf.h"
BASE_PIN="$HERE/berry_conf.base.sha256"
PATCH_PIN="$HERE/conf-overrides.patch.sha256"

fail() { echo "GATE RED: $*" >&2; exit 1; }

[ -f "$BASE" ]  || fail "firmware Berry conf missing ($BASE) — run scripts/setup-berry.sh first"
[ -f "$PATCH" ] || fail "override patch missing ($PATCH)"

base_now="$(sha256sum "$BASE"  | awk '{print $1}')"
patch_now="$(sha256sum "$PATCH" | awk '{print $1}')"
base_pin="$(cat "$BASE_PIN")"
patch_pin="$(cat "$PATCH_PIN")"

[ "$base_now" = "$base_pin" ] || \
  fail "berry_conf.h base drift: firmware=$base_now pinned=$base_pin (Berry conf changed; review the override set, regenerate the pin)"
[ "$patch_now" = "$patch_pin" ] || \
  fail "conf-overrides.patch drift: file=$patch_now pinned=$patch_pin (override set tampered)"

# base + declared patch == the exact sim conf. Apply to a copy (never touch firmware).
tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT
cp "$BASE" "$tmp"
patch -s -p1 "$tmp" < "$PATCH" || fail "patch did not apply cleanly against the pinned base"
mv "$tmp" "$OUT"; trap - EXIT

echo "sim/berry_conf.h generated (base $base_now / patch $patch_now)"
