# open-picpak

> The open firmware, tooling and documentation the PicPak e-ink frame should have
> shipped with — no cloud, no lock-in, fully in your hands.

**open-picpak** is a clean-room, vendor-independent project around the PicPak 4.2″
e-ink photo frame: a reverse-engineering reference, runnable tools, a full custom
firmware with features the stock product never had, and a self-hostable backend.
Everything is developed in public.

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

### What I published

- [x] [`documentation/device.md`](documentation/device.md) — hardware, memory layout,
      identity, colors, radio/brownout, BLE overview, image format
- [x] [`documentation/ble-protocol.md`](documentation/ble-protocol.md) — full stock
      BLE GATT / wire-frame / OTA spec
- [x] [`documentation/image-pipeline.md`](documentation/image-pipeline.md) —
      RGB→palette (BT.601 + Atkinson) with a runnable JS reference
- [x] MPL-2.0 license

### What I planned

The whole point. Re-implementing everything I already have — step by step, so it stays
clean and gets refined on the way — then going far past what the stock firmware does.

**Documentation**
- [ ] flashing & backup guide (esptool params, bootloader mode, stock restore)
- [ ] reverse-engineering methodology (ESP-image → ELF, radare2, Ghidra, blutter)
- [ ] custom-firmware notes (adaptive TX, OTA block transfer, streaming-header pitfall)

**`firmware/` — custom ESP-IDF firmware**
- [ ] **USB mass-storage config interface** — a virtual filesystem overlay where text
      files are the configuration UI, **TOML** as the language *(untested concept)*
- [ ] **`control/` action folder** — deleting a named "file" runs that action
      (`reboot`, `test-wifi`, …)
- [ ] **WiFi / IP config via TOML** — stubbed with DHCP and WiFi **disabled** until
      configured
- [ ] **embedded fonts** — pixel fonts for on-device text + simple shape rendering
      *(feasibility to confirm)*
- [ ] **self-debug screen** — serial, MACs, WiFi status, IP / subnet / gateway, …
- [ ] **playlist & rotation** — rotate stored images, configurable cycle interval,
      remote **sync playlist** (poll for updates on an interval — not yet implemented),
      single-frame remote (Home-Assistant-style backend), OTA URL
- [ ] **virtual `picture/` folder** — stored images exposed as editable,
      color-indexed PNGs
- [ ] **BLE-stack re-implementation** — keep the original PicPak app able to manage
      stored images (on-flash storage format TBD)
- [ ] **use the upper 16 MB of flash** — make the currently unaddressed region usable
      *(research: swapping / code paging — TBD)*
- [ ] **Lua interpreter** — user scripting on the device

**`server/` — Docker backend**
- [ ] **serial-number differentiation** — serve separate frames per device
- [ ] single-frame remote backend, generalized from the Home Assistant PoC
- [ ] serves the `web-usb/` tool as static assets

**`web-usb/` — browser tool (served by the server)**
- [ ] flashing & stock backup / restore (esptool-js — no native install)
- [ ] image editor / uploader (built on the `image-pipeline.md` JS reference)

**`scripts/` — host-side tooling**
- [ ] BLE client (push / manage images without the phone app)
- [ ] dev & automation helpers

---

## Repository layout

```
documentation/   reference & reverse-engineering docs        (published)
firmware/        custom ESP-IDF firmware                     (planned)
server/          self-hostable Docker backend; serves web-usb (planned)
web-usb/         browser flashing + image editor/uploader    (planned)
scripts/         host-side scripts / CLI tooling             (planned)
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

---

## Hardware in one line

ESP32-C3 (400 KB SRAM, 16 MB addressed flash) · 4.2″ 400×300 BWRY e-ink (BLE only on
stock). Full breakdown in [`documentation/device.md`](documentation/device.md).

---

## Author & license

Created and maintained by **Jan-Stefan Janetzky (GottZ)** — [git@gottz.de](mailto:git@gottz.de).

Licensed under the [Mozilla Public License 2.0](LICENSE). Copyright © 2026 Jan-Stefan Janetzky.
