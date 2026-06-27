# `firmware/` — custom ESP-IDF firmware (ESP32-C3)

Work in progress. This is the open firmware for the PicPak e-ink frame, built from scratch on
ESP-IDF (C). Each wake it boots, optionally opens a setup console, pulls or renders a frame,
displays it **only on a change**, and deep-sleeps — self-healing via the **recovery guard**,
scriptable on-device via an embedded **Berry engine** (config *is* a script), and remotely
steerable via a HOTP-authenticated **C2 command channel**.

> Air-gap: this tree carries no real identifiers (SSIDs, URLs, MACs, keys, hostnames). All
> credentials are provisioned at runtime into NVS via the console (or the C2 channel); nothing is
> hardcoded.

## What it does each wake

1. **boot diagnostics** — reset-proof boot counter + reset reason recorded to NVS, exfiltrated on
   the next fetch via the RTC-RAM log ring.
2. **recovery guard** — increments a reset-proof counter early; too many un-stable boots → a
   USB-reachable safe mode (see below).
3. **store mount** — the littlefs `fs` partition is mounted *after* the guard; a missing/corrupt FS
   degrades gracefully (`store_fs_ok()==false`) and the device keeps running.
4. **low-battery gate** (default-off) — below a threshold the device polls in short deep sleeps and
   resumes on a detected voltage rise, instead of running a full cycle on a near-dead cell.
5. **console** — only on a *real* boot (power-on / reset / USB connect), never on a timer/button
   wake; a triple-press on any wake forces it on the next boot.
6. **run-cycle** — WiFi (multi-SSID, by RSSI + priority) → fetch a server-rendered frame *or* render
   on-device with Berry → display **only on a changed frame** → C2 poll (if enabled) → deep sleep.
   On USB power the WLAN is held across cycles and decoupled only around the actual refresh.

## Layout (`main/`)

| File | Role |
|---|---|
| `main.c` | the wake flow above: wake → guard → store → low-batt → console → run-cycle → deep sleep |
| `guard.{c,h}`, `guard_core.h` | **recovery guard** — bootloop → USB safe-mode (pure decision logic is host-testable) |
| `config.{c,h}` | NVS config (SSID / password / frame URL), provisioned over the console |
| `wifi_store.{c,h}` | **multi-SSID store** — several networks, selected by priority + RSSI |
| `console.{c,h}` | USB-serial setup + diagnostics console (full command surface below) |
| `net.{c,h}` | WiFi connect + HTTP(S) frame fetch (cert bundle, adaptive TX power, connect cache) |
| `netberry.{c,h}` | Berry **net surface** (`wifi_scan/connect/ip/ssid/rssi/stop`, `http_get`) for the policy phase |
| `policy.be` → `policy_script.h` | net-phase Berry script: scan, match `wifi.*` store keys, connect by priority/RSSI |
| `fb.{c,h}` | **Berry graphics stdlib** over a 400×300 BWRY framebuffer (`text/line/rect/disc/circle/triangle/fill/qr`) |
| `render.be` → `render_script.h` | the on-device render script — *config is a script* (text + live device vars + shapes + QR) |
| `cmd.{c,h}`, `auth_core.h` | **C2 command surface + executor** + per-device RFC-4226 HOTP (host-testable, no mbedtls) |
| `store.{c,h}`, `store_core.h` | littlefs key-value store (Berry-persistent) + RTC-RAM ephemeral store |
| `lowbatt.{c,h}`, `lowbatt_core.h` | smart low-battery gate (timer-poll + voltage-rise charge detect) |
| `ota.{c,h}`, `ota_core.h` | OTA over WiFi (block transfer, SHA-256 verify, fail-closed) |
| `epd.{c,h}` | e-ink panel driver (UC81xx-class, 400×300 BWRY) |
| `dev.{c,h}` | device facts exposed to Berry (MACs, chip, battery mV via the stock divider/curve) |
| `led.{c,h}`, `logbuf.{c,h}`, `logbuf_core.h` | status LED + RTC-RAM log ring (exfiltrated via a request header) |
| `rtc_core.h` | RTC composite counter `(boot_count << 24) \| rtc` — the HOTP moving factor |
| `screens.h`, `font16.h`, `font8x8.h` | baked-in setup screens + glyph tables (screens/font16 are build artifacts) |
| `test/test_*.c` | host unit tests for the pure `*_core.h` logic — no hardware (see Build) |

The `*_core.h` split keeps each subsystem's decision logic free of ESP-IDF calls so it runs in a
host unit test; the matching `.c` is the thin IDF binding around it.

## Berry engine

The device embeds a Berry VM — validated on the C3 (~3 KB, coexists with WiFi + framebuffer, no
PSRAM). It runs in three phases, each with a pinned, hardened C stdlib (*mechanism = C, policy =
script*; every binding gates arity + type at the entry and never touches an unchecked slot):

- **render** (`render.be`, RF off) — draws the frame from primitives
  (`text/line/rect/disc/circle/triangle/fill/qr`) plus live device variables (`dev_mac`, `nvs_str`,
  battery, …). "Config is a script, not a data file." Host-previewable via `host/host_render.c` +
  `host/fb2png.py`.
- **policy** (`policy.be`, WiFi up) — scans, matches the `wifi.*` store keys, and connects by
  priority + RSSI through the `netberry` surface.
- **C2 command** (`cmd.c`) — runs a server-supplied Berry script on a deliberately **safe** subset:
  config/NVS writes, read-only queries, the key-value store, and `reboot`/`refresh`/`sleep` intents.
  No drawing, no WiFi re-drive, no OTA trigger — those earn their own negative-probed wave. Each
  script is fetched over **HTTPS only** (the payload is code) and authenticated per-device with
  RFC-4226 HOTP (byte-compatible with the backend); the ack is persisted *before* the intent is
  actioned, so a `reboot` command can never loop. See [`../backend/`](../backend/) for the channel.

## Recovery guard

A device that crashes early on every boot would otherwise be bricked without a reset pin. The guard
turns that into a recoverable state:

- A reset-proof NVS counter is incremented **early in `app_main`** (before WiFi/EPD). A boot is
  marked **stable** (counter → 0) after an uninterrupted stable-uptime window, or on an ordered
  deep-sleep entry.
- More than `threshold` consecutive un-stable boots → **safe mode**: no WiFi, no EPD, never sleeps;
  the USB console + esptool stay reachable so the device can be recovered or reflashed with no reset
  pin. `GUARD CLEAR` in the console resumes normal boot.
- **Reset-reason-agnostic** (a time/sleep marker, not a reset-reason allowlist), so it catches
  panics, watchdog hangs, brownouts and power glitches alike.
- **Fresh-app reset:** a freshly flashed / OTA'd image (elf-sha changed) resets the counter to 1 — a
  reflash of a *new* build recovers, a reflash of the *same* buggy build does not (it hides no bug).

Validated on hardware (negative / positive / recovery / real bootloop / esptool-reflash / fresh-app
/ watchdog). Known limit: a crash that consistently happens *after* the stable marker never trips
safe mode — but such a device is reachable each cycle, so esptool recovers it.

Defaults: threshold = 4, stable-uptime = 20000 ms (compile-time defaults in `guard_core.h` /
`guard.c`, overridable at runtime via `GUARD THRESHOLD` / `GUARD UPTIME`).

## Console

On a real boot (or a forced triple-press) the USB-serial console exposes the setup + diagnostics
surface. All writes persist to NVS:

```
SETWIFI <ssid> <pass>                 save WiFi   (SETWIFI ADD/LIST/DEL <slug> = multi-WiFi store)
SETURL  <url>                         save the frame URL
NVSSET  <ns> <key> <val>              generic NVS string write (e.g. storage dev_sn ...)
INFO                                  status
REFRESH | SLEEP [s] | PRESS [n]       run a cycle now / sleep / simulate n button-press cycles
LOG [CLEAR] | NETCLR | OTA            log ring / drop connect cache / OTA status
GUARD [CLEAR|THRESHOLD n|UPTIME ms]   bootloop guard: status / clear / tune
STORE OK|LIST|GET k|SET k v|DEL k     littlefs key-value store
RTC   STAT|GET k|SET k v|DEL k        RTC-RAM key-value (ephemeral)
NET   SCAN|TRY ssid pass|IP|SSID|RSSI|STOP|TXPOWER [dBm]
BATT  [ON|OFF|ARM mv|CLEAR mv|RISE mv|WAKE s|STREAK n]   low-battery gate / status
C2    RUN <berry> | URL <https-url> | PERIOD <s> | SECRET <hex> | AUTH   command channel
ERASE | HELP
```

## Build

The vendored components are fetched by setup scripts (not committed), and `main/` needs four
generated headers before the IDF build. One-time setup, then generate the artifacts, then build:

```sh
# one-time: fetch the vendored components (Berry VM, littlefs, qrcodegen)
scripts/setup-berry.sh && scripts/setup-littlefs.sh && scripts/setup-qrcodegen.sh

# generate the build artifacts (host needs Python 3 + Pillow + the bundled fonts)
python3 screens-src/gen_screens.py     # -> main/screens.h
python3 host/gen_font16.py             # -> main/font16.h
python3 host/gen_render.py             # -> main/render_script.h   (from main/render.be)
python3 host/gen_policy.py             # -> main/policy_script.h   (from main/policy.be)

docker run --rm -v "$PWD":/project -w /project espressif/idf:v5.5.3 \
  bash -lc "idf.py set-target esp32c3 build"
```

The build fails fast with a clear message naming any missing artifact.

Host unit tests — pure `*_core.h` logic, no hardware:

```sh
for t in guard ota store rtc lowbatt logbuf auth; do
  cc -I main -O2 -Wall -o /tmp/test_$t test/test_$t.c && /tmp/test_$t || break
done
```

`test/test_policy.c` exercises the real `policy.be` against the Berry VM, so it builds with the
Berry component (host harness) rather than a bare `cc`.

## Flash

The ESP32-C3 has a native USB-Serial-JTAG interface (`303a:1001`), so esptool resets it into
download mode without a button. **Flash write-only — never `erase-flash` / `erase-region` over `0x9000`.**
The NVS partition at `0x9000` holds the **factory per-device data** (RF calibration, base MAC, serial,
BT config). This firmware coexists with that NVS additively, but a full erase destroys it **irrecoverably**
(only a prior full-flash backup can restore it) — and it also breaks a later clean stock re-flash, since
stock and this firmware share the same NVS region. Write-only:

```sh
esptool --chip esp32c3 -p <PORT> --before default-reset --after hard-reset \
  write-flash --flash-mode dio --flash-size 16MB --flash-freq 80m \
  0x0 build/bootloader/bootloader.bin \
  0x8000 build/partition_table/partition-table.bin \
  0x10000 build/ota_data_initial.bin \
  0x20000 build/picpak_fw.bin
```

The `fs` (littlefs) partition is **additive behind the OTA slots**, so it moves no earlier
partition and the stock NVS stays intact. A device flashed before the `fs` row existed gets it only
via an esptool **partition-table** reflash (never over OTA, which writes app slots only); until then
`store.c` degrades to `store_fs_ok()==false` and the device runs without on-device persistence.

## Provisioning

Open the serial console after a fresh boot and provide credentials (stored in NVS):

```
SETWIFI <ssid> <password>           # or SETWIFI ADD <ssid> <password> for several networks
SETURL  <frame-url>
```

To enable the remote command channel, point it at a self-hosted backend and arm the per-device key:

```
C2 URL    <https-url>               # HTTPS only — the response is executed as Berry
C2 SECRET <hex>                     # per-device HOTP key (must match the backend row)
C2 PERIOD <seconds>                 # poll cadence (0 = off, the default)
```
