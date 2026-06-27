# open-picpak

> The open firmware, tooling and documentation the PicPak e-ink frame should have
> shipped with — no cloud, no lock-in, fully in your hands.

**open-picpak** is a clean-room, vendor-independent project around the PicPak 4.2″
e-ink photo frame: a reverse-engineering reference, runnable tools, a full custom
firmware with features the stock product never had, and a self-hostable backend.
Everything is developed in public.

**Status**

| Component | State |
|---|---|
| `documentation/` | **published** |
| `firmware/` | **started** — recovery guard (first component) |
| `tools/picpak-ops/` | **started** — operator deployment TUI (on-device validation pending) |
| `server/` | not public yet |
| `web-usb/` | not public yet |
| `scripts/` | not public yet |

---

## Contents

- [Motivation](#motivation)
- [What this is](#what-this-is)
- [Status](#status) · [what they did](#what-they-did--the-stock-product) · [what I did](#what-i-did) · [what I published](#what-i-published) · [what I planned](#what-i-planned)
- [Repository layout](#repository-layout)
- [Documentation](#documentation)
- [Hardware in one line](#hardware-in-one-line)
- [Disclaimer](#disclaimer)
- [Author & license](#author--license)

---

## Motivation

I backed the Kickstarter because the campaign imagery showed **greens and blues** — I
assumed I was getting a mature color e-ink panel at a fair price.

What shipped was **400 × 300 BWRY**: four colors — black, white, red, yellow. No
green, no blue, no grey. That hope was gone on first refresh.

Worse, the stock firmware does the bare minimum. It is a thin, cloud-tethered image
pusher: the phone app pulls photos and firmware from the vendor cloud and shoves them
over BLE. None of what I hoped to do with the device is possible out of the box — and
the vendor had far longer to build a proper firmware than the **single day** it took
me to stand up a fully working Home Assistant WiFi pull-frame on my own firmware.

So I'm building the firmware (and the tools, and the backend) the device deserved —
**in public**, so the community gets to use it too. I won't stop before every planned
feature below is real.

---

## What this is

A full suite, built clean:

- a **documentation / reverse-engineering reference** (what the device actually is),
- a **custom ESP-IDF firmware** with features the stock never had,
- a **self-hostable backend** (Docker) that serves per-device content,
- and the **tools** to flash, configure and feed the device — without any cloud.

> **Air-gapped, no secrets.** This repository carries no real device identifiers — no
> serial numbers, MAC addresses, RF-calibration blobs, BLE keys or firmware hashes
> from any physical unit. Every such value is a format-preserving placeholder. Bring
> your own device. See the stub conventions at the top of
> [`documentation/device.md`](documentation/device.md).

---

## Status

### What they did — the stock product

- 4.2″ **400 × 300 BWRY** e-ink (black/white/red/yellow only; no green/blue/grey),
  full-refresh only (~19 s).
- **ESP32-C3**, **BLE-only** firmware — no WiFi linked at all.
- The **phone app is a mandatory cloud gateway**: it pulls photos and firmware from
  the vendor cloud and pushes them to the device over BLE.
- 700 image slots, no on-device configuration, no scripting, no network of its own.

### What I did

- **Reverse-engineered the device end-to-end:** hardware & pin map, flash / NVS /
  eFuse layout, MAC & serial identity, case-color encoding, the radio/brownout
  envelope, the full BLE GATT protocol (`0xFF01/02/03` incl. OTA-over-BLE), and the
  exact RGB→panel image pipeline (BT.601 nearest-color + Atkinson).
- **Built a fully working Home Assistant WiFi pull-frame in a day** on custom ESP-IDF
  firmware: wake (timer/button) → WiFi → fetch a server-rendered image → display →
  deep sleep, with **SHA-256-verified OTA over WiFi** and brownout-hardened transfers.
- **Built a recovery guard** — bootloop detection → USB-reachable safe-mode (WiFi/EPD off,
  esptool + console stay up); reset-reason-agnostic, fresh-app reset, NVS-tunable
  threshold/stable-uptime. Host-tested + on-device validated.
- **Decided on-device scripting: Berry** — config *and* procedural graphics in one VM;
  RAM feasibility validated on the C3 (VM ~3 KB, coexists with WiFi + framebuffer, no PSRAM).

### What I published

- [x] [`documentation/device.md`](documentation/device.md) — hardware, memory layout,
      identity, colors, radio/brownout, BLE overview, image format
- [x] [`documentation/ble-protocol.md`](documentation/ble-protocol.md) — full stock
      BLE GATT / wire-frame / OTA spec
- [x] [`documentation/image-pipeline.md`](documentation/image-pipeline.md) —
      RGB→palette (BT.601 + Atkinson) with a runnable JS reference
- [x] MPL-2.0 license
- [x] [`firmware/`](firmware/) — custom ESP-IDF firmware: the **recovery guard** (bootloop →
      USB-reachable safe-mode; host-tested + on-device validated) plus an integrated **Berry
      render/config engine** — a Berry script renders the frame on-device (text + live device
      variables + shapes + a Pacman), "config is a script, not a data file". Validates Berry on the
      C3 (no PSRAM, coexists with WiFi); host-previewable. Battery uses the stock divider/curve
      recovered by disassembly.

### What I planned

The whole point. Re-implementing everything I already have — step by step, so it stays
clean and gets refined on the way — then going far past what the stock firmware does.

**Documentation**
- [ ] flashing & backup guide (esptool params, bootloader mode, stock restore)
- [ ] reverse-engineering methodology (ESP-image → ELF, radare2, Ghidra, blutter)
- [ ] custom-firmware notes (adaptive TX, OTA block transfer, streaming-header pitfall)

**`firmware/` — custom ESP-IDF firmware (C)**
- [~] **config-/logic-engine (Berry, not TOML)** — config *is* a Berry script fetched per wake
      (rotation, intervals, OTA URL, triggers/actions); the device exposes a small pinned C stdlib,
      the script decides. Feasibility validated on-device; seed in
      [`firmware/`](firmware/). WiFi/IP stubbed off until configured.
- [ ] **control actions** — named actions (`reboot`, `test-wifi`, …) as Berry stdlib calls,
      triggered from the config tool
- [~] **embedded fonts + simple shapes** — Adafruit-GFX-style 1-bit glyph blitter (text, lines,
      rects, discs, triangles), ~0 extra RAM; seeded as the Berry graphics stdlib in
      [`firmware/`](firmware/)
- [~] **self-debug screen** — serial, MACs, chip, uptime, reset, battery; rendered on-device
      (the `firmware/` scene already shows these)
- [ ] **playlist & rotation** — rotate stored images, configurable cycle interval,
      remote **sync playlist** (poll for updates on an interval — not yet implemented),
      single-frame remote (Home-Assistant-style backend), OTA URL
- [ ] **image store** — stored frames, exposed to the config tool as editable
      color-indexed PNGs
- [ ] **BLE / WiFi, swappable** — never both at once (RAM budget = the larger, not the
      sum); configurable swap (BLE for provisioning/OTA ↔ WiFi run-cycle), NimBLE
- [ ] **BLE-stack re-implementation** — keep the original PicPak app able to manage
      stored images (on-flash storage format TBD)
- [ ] **use the upper flash region** — make the unaddressed 16→32 MB usable for bulk
      image storage *(research — data only, not code)*
**`server/` — Docker backend**
- [ ] **serial-number differentiation** — serve separate frames per device
- [ ] single-frame remote backend, generalized from the Home Assistant PoC
- [ ] serves the `web-usb/` tool as static assets

**`web-usb/` — browser config & flashing tool (served by the server)**
> Replaces a USB mass-storage interface — impossible on the ESP32-C3 (no USB-OTG) —
> with the same file-like UX over two transports: **Web-USB** (WebSerial, on the
> cable) and **Web-WiFi** (HTTP, on the network).
- [ ] flashing & stock backup / restore (esptool-js — no native install)
- [ ] config form (writes the device TOML), control-action buttons, image gallery
- [ ] image editor / uploader (built on the `image-pipeline.md` JS reference)

**`scripts/` — host-side tooling**
- [ ] BLE client (push / manage images without the phone app)
- [ ] dev & automation helpers

---

## Repository layout

```
documentation/      reference & reverse-engineering docs         (published)
firmware/           custom ESP-IDF firmware                      (in progress)
firmware/ Berry config-/logic-engine + GFX render slice (published)
server/             self-hostable Docker backend; serves web-usb (planned)
web-usb/            browser config + flashing tool               (planned)
scripts/            host-side scripts / CLI tooling              (planned)
tools/picpak-ops/   operator deployment TUI (build/flash/console/OTA) (in progress)
```

`web-usb/` and `server/` are coupled: the backend ships the browser tool as static
assets, so flashing, uploading and configuring all work from one self-hosted URL.

---

## Documentation

| Doc | What it covers |
|---|---|
| [device.md](documentation/device.md) | The hardware: ESP32-C3, the BWRY panel, full pin map, flash/NVS/eFuse layout, identity, colors, radio/brownout, BLE overview |
| [ble-protocol.md](documentation/ble-protocol.md) | The stock BLE protocol byte-for-byte: GATT layout, wire frame, dispatchers, image upload, OTA-over-BLE |
| [image-pipeline.md](documentation/image-pipeline.md) | Exact RGB→panel conversion (BT.601 nearest-color + unclamped Atkinson) with a Web-USB-ready JS reference |
| [config-engine.md](documentation/config-engine.md) | Why the config *is* a Berry script (mechanism = pinned C stdlib / policy = script), the on-device feasibility result, and the stdlib seeded in `firmware/` |

---

## Hardware in one line

ESP32-C3 (400 KB SRAM, 16 MB addressed flash) · 4.2″ 400×300 BWRY e-ink (BLE only on
stock). Full breakdown in [`documentation/device.md`](documentation/device.md).

---

## Disclaimer

open-picpak is an independent, community-run project. It is **not affiliated with,
authorized by, or endorsed by AUTOHEART TECH CO., LTD**, the maker of PicPak.
"PicPak" / "PICPAK" and any related names or marks are the property of AUTOHEART TECH
CO., LTD and are used here only to identify the hardware this project targets.

This is **clean-room, interoperability** work: it documents and reimplements the
device's behavior through reverse engineering. **No vendor source code, firmware
binaries, assets, keys or secrets are included or redistributed** (see the air-gap
note above). Studying and testing how a program you own behaves — and reverse
engineering for interoperability — are expressly permitted under EU law (Directive
2009/24/EC, Arts. 5–6; in Germany § 69e UrhG), and computer programs "as such" are
not patentable in the EU (Art. 52 EPC). The author lives in Germany (EU) and is not a
US citizen. Comparable open-source projects stand on the same footing despite pressure
from original vendors — e.g. VideoLAN/VLC.

None of this is legal advice.

---

## Author & license

Created and maintained by **Jan-Stefan Janetzky (GottZ)** — [git@gottz.de](mailto:git@gottz.de).

Licensed under the [Mozilla Public License 2.0](LICENSE). Copyright © 2026 Jan-Stefan Janetzky.
