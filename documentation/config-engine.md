# Config-/logic-engine — the configuration *is* a script

The custom firmware does not use a static configuration format. The device's policy —
what it shows, when it wakes, what it does on a button press, how it renders a frame — is a
small **[Berry](https://github.com/berry-lang/berry) script**, and the firmware exposes a
narrow, pinned C **stdlib** as the script's only window onto the hardware.

> **Mechanism = the pinned C stdlib** (what the device *can* do).
> **Policy = the script** (what it *does* on a given wake).

A working seed lives in [`../firmware/`](../firmware/).

## Why a language, not a data file

The feature set is behaviour, not data: named triggers (time / button / motion) firing named
actions (fetch, pull image, OTA, set a variable, conditionals), with variables and references.
Expressing that in TOML/JSON rebuilds a programming language inside a data format (the
GitHub-Actions-YAML / Home-Assistant-automations anti-pattern). And the device is meant to
**generate frames on-device** later — fonts, vector paths, patterns, sprites — which a
declarative format cannot express at all. One embedded VM covers both config logic and graphics.

## Why Berry

Berry is an ultra-light embedded VM (register-based, one-pass compiler, ANSI C99) used in
production for exactly this class of device. On-device measurement on the ESP32-C3 (which has
**no PSRAM**): the VM baseline is ~3 KB of heap, and a VM plus a procedural frame-gen load plus
the WiFi stack plus the 30000-byte framebuffer coexist with comfortable headroom. Config logic
and graphics therefore live in one language on the real hardware.

## The stdlib (as seeded in the demo)

- **graphics** (`fb.c`): `fill / pixel / rect / disc / circle / triangle / text` into the
  400×300 BWRY framebuffer (2 bpp, MSB-first, panel vertical-mirror handled in `pixel()`),
  with an 8×8 bitmap font. This is the seed of the planned GFX blitter.
- **device** (`dev.c`): `dev_mac` / `dev_bt_mac` / `dev_chip` / `dev_uptime_ms` / `dev_reset` /
  `dev_batt_*`, plus a generic `nvs_str(namespace, key)`.
- **parsing in the script**: the factory NVS stores JSON, so the script parses it with Berry's
  built-in `json` module rather than baking field extraction into C — a direct example of the
  mechanism/policy split.

Native functions are registered with `be_regfunc`; color/format constants are defined in the
script. Battery is sampled **first thing after wake**, before WiFi/EPD/Berry load the rail.

## Status & next steps

- **Done (validated on-device):** the render path end to end — a Berry script draws a scene
  (text + live device variables + shapes) and it reaches the panel.
- **Next:** fetch the script (or precompiled bytecode) per wake over the existing HTTP path;
  harden the native functions (argument-arity guards, geometry clamping, color-range checks);
  grow the graphics stdlib (vector paths, sprites, patterns); add the trigger/action surface.
