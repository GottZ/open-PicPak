# `firmware/` — custom ESP-IDF firmware (ESP32-C3)

Work in progress. This is the open firmware for the PicPak e-ink frame, built from scratch
on ESP-IDF (C). It boots, provisions over a serial console, pulls a server-rendered image
over WiFi, displays it, and deep-sleeps — and it is self-healing via the **recovery guard**
(the first landed component; see below).

> Air-gap: this tree carries no real identifiers (SSIDs, URLs, MACs, keys, hostnames). All
> credentials are provisioned at runtime into NVS via the console; nothing is hardcoded.

## Layout (`main/`)

| File | Role |
|---|---|
| `main.c` | boot flow: wake (timer/button) → console (on a real boot) → WiFi → fetch → EPD → deep sleep |
| `guard.{c,h}`, `guard_core.h` | **recovery guard** — bootloop → USB safe-mode (pure decision logic is host-testable) |
| `config.{c,h}` | NVS config (SSID / password / frame URL), provisioned over the console |
| `console.{c,h}` | USB-serial setup console (`SETWIFI`, `SETURL`, `INFO`, `GUARD`, …) |
| `net.{c,h}` | WiFi connect + HTTP frame fetch (adaptive TX power, connect cache) |
| `ota.{c,h}` | OTA over WiFi (block transfer, SHA-256 verify, fail-closed) |
| `epd.{c,h}` | e-ink panel driver (UC81xx-class, 400×300 BWRY) |
| `led.{c,h}`, `logbuf.{c,h}`, `screens.h` | status LED, RTC-RAM log ring, baked-in setup screens |
| `test/test_guard.c` | host unit test for the guard's pure logic (no hardware) |

## Recovery guard

A device that crashes early on every boot would otherwise be bricked without a reset pin.
The guard turns that into a recoverable state:

- A reset-proof NVS counter is incremented **early in `app_main`** (before WiFi/EPD). A boot
  is marked **stable** (counter → 0) after an uninterrupted stable-uptime window, or on an
  ordered deep-sleep entry.
- More than `threshold` consecutive un-stable boots → **safe mode**: no WiFi, no EPD, never
  sleeps; the USB console + esptool stay reachable so the device can be recovered or reflashed
  with no reset pin. `GUARD CLEAR` in the console resumes normal boot.
- **Reset-reason-agnostic** (a time/sleep marker, not a reset-reason allowlist), so it catches
  panics, watchdog hangs, brownouts and power glitches alike.
- **Fresh-app reset:** a freshly flashed / OTA'd image (elf-sha changed) resets the counter to
  1 — a reflash of a *new* build recovers, a reflash of the *same* buggy build does not (it
  hides no bug).

Validated on hardware (negative / positive / recovery / real bootloop / esptool-reflash /
fresh-app / watchdog). Known limit: a crash that consistently happens *after* the stable
marker never trips safe mode — but such a device is reachable each cycle, so esptool recovers it.

### Console (`GUARD`)

```
GUARD                 show bad_boots, threshold, stable_ms
GUARD CLEAR           reset the counter (recovery)
GUARD THRESHOLD <n>   set the safe-mode threshold (persisted in NVS)
GUARD UPTIME <ms>     set the stable-uptime window (persisted in NVS)
```

Defaults: threshold = 4, stable-uptime = 20000 ms (compile-time defaults in `guard_core.h`
/ `guard.c`, overridable at runtime via the keys above).

## Build

`main/screens.h` is a build artifact (not committed) — generate it first from
`screens-src/gen_screens.py` (needs Python 3 + Pillow + the bundled fonts), then build:

```sh
python3 screens-src/gen_screens.py        # writes main/screens.h (host needs Pillow)
docker run --rm -v "$PWD":/project -w /project espressif/idf:v5.5.3 \
  bash -lc "idf.py set-target esp32c3 build"
```

The build fails fast with a clear message if `screens.h` is missing.

Host unit test (guard logic, no hardware):

```sh
cc -I main -O2 -Wall -o /tmp/test_guard test/test_guard.c && /tmp/test_guard
```

## Flash

The ESP32-C3 has a native USB-Serial-JTAG interface (`303a:1001`), so esptool resets it into
download mode without a button. Flash with **no full erase** so the NVS config survives:

```sh
esptool --chip esp32c3 -p <PORT> --before default-reset --after hard-reset \
  write-flash --flash-mode dio --flash-size 16MB --flash-freq 80m \
  0x0 build/bootloader/bootloader.bin \
  0x8000 build/partition_table/partition-table.bin \
  0x10000 build/ota_data_initial.bin \
  0x20000 build/picpak_fw.bin
```

## Provisioning

Open the serial console after a fresh boot and provide credentials (stored in NVS):

```
SETWIFI <ssid> <password>
SETURL  <frame-url>
```
