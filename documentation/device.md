# PicPak — Device Reference

What's actually inside a PicPak frame, how the stock firmware lays out flash and
NVS, how the four panel colors and the four case colors are distinguished, and how
the button, LED, IMU and display are wired and driven.

Everything here was recovered by reverse engineering (esptool flash dumps, eFuse
reads, radare2/Ghidra on the stock ESP-IDF images, and disassembly of the vendor
Android app), then cross-checked against measurements on live hardware. Statements
are marked where they are binary-verified `[bin]`, measured on a device `[hw]`, or
inferred from app strings `[inf]`.

> **Air-gap note — no real identifiers.** This repository is built clean: it carries
> no real serial numbers, MAC addresses, RF-calibration blobs, BLE bonding keys or
> firmware hashes from any physical unit. Every such value below is a **placeholder
> that preserves the real format** so the structure is documented without leaking a
> specific device's identity. Placeholder conventions:
>
> | Field | Placeholder used | Real format it stands for |
> |---|---|---|
> | Serial number | `D2XXXXN` / `…W` / `…B` / `…R` | `D2` + 4-char unit id + color suffix |
> | Base MAC (example) | `B0:A6:04:00:11:22` | 6-byte EUI-48, real OUI `B0:A6:04` |
> | RF cal / cal_mac / IRK | `<…>` / size only | per-device binary blobs |
> | Firmware image digest | `<sha256>` | 32-byte content hash |

---

## 1. At a glance

| | |
|---|---|
| Product | PicPak — 4.2″ four-color e-paper photo frame |
| SoC | Espressif **ESP32-C3** (RISC-V single-core @ 160 MHz, rev v0.4) |
| RAM | 400 KB on-chip SRAM (no PSRAM); 8 KB RTC SRAM retained in deep sleep |
| Wireless | BLE only on stock firmware (no Wi-Fi linked, see §10) |
| Radio/power | one RF PHY shared by BLE/Wi-Fi; brownout-sensitive at TX peaks (see §11) |
| Display | 4.2″ **400 × 300** four-color (BWRY) e-paper, panel ID `0x060401` |
| Flash | 32 MB physical (`t25s256`), stock FW addresses 16 MB |
| IMU | ST **LSM6DS3TR-C** (accel/gyro, motion/tilt wake) |
| Inputs/outputs | 1 button (GPIO2), 1 status LED (GPIO21) |
| USB | native USB-Serial/JTAG, VID `0x303A` / PID `0x1001` |
| Identity | base MAC in eFuse BLOCK1; serial + RF-cal in NVS |

---

## 2. SoC — ESP32-C3

- RISC-V single core, 160 MHz, silicon revision **v0.4**.
- **On-chip memory (datasheet):** 400 KB SRAM (16 KB of it configurable as cache),
  384 KB ROM, and 8 KB RTC FAST SRAM that survives deep sleep (used for wake
  counters / a log ring across sleep cycles). **No PSRAM** — the ESP32-C3 has no
  external-RAM support, so the 400 KB SRAM is the entire RAM budget for any firmware.
- **Native USB-Serial/JTAG** controller (no external USB bridge). Enumerates as
  VID `0x303A` / PID `0x1001`, appears as `/dev/ttyACM0` (Linux). `[hw]`
- USB data lines: **D− = GPIO18**, **D+ = GPIO19**. Stock probe routines avoid
  GPIO18 on purpose because it carries D−. `[bin]`
- Bootloader (download) mode: USB unplugged, hold button, plug USB in, release once
  the port appears. Boot banner markers: `USB_BOOT` / `wait usb download`. `[hw]`

---

## 3. Flash

| | |
|---|---|
| Physical chip | `t25s256` — **32 MB** |
| Addressed by stock FW | **16 MB** |
| Mode / frequency | DIO, 80 MHz |

The stock V0.5.0 firmware auto-detects the full part at boot and resizes
(`Overriding flash chip size … 32MB detected`). `[bin]` For the flash layout (partition
table, magic values, NVS) see §7.

---

## 4. Display — 4.2″ 400×300 BWRY e-paper

| | |
|---|---|
| Resolution | 400 × 300 |
| Colors | 4 fixed: black / white / red / yellow (BWRY) |
| Panel ID | `0x060401` (read over SPI by the FW) `[bin]` |
| Panel class | Good Display GDEY042F51 / GDEM042F52 / F86 family |
| Controller | UC8xxx / SSD2683-class multicolor driver |
| SPI | write-only path, 1 MHz |
| Framebuffer | 30 000 B per image (400×300 × 2 bpp, 4 px/byte, MSB-first) |
| Orientation | image is **vertically flipped** before sending |

### 4.1 Panel ID and case color are independent

The firmware reads the panel ID over SPI and logs
`Detected EPD ID 0x060401, applied specific init sequence` (fallback:
`Applied default init sequence`). The ID is **identical across all case colors** —
the e-paper panel is the same part regardless of the housing color. Case color is
encoded elsewhere (see §9). `[bin]`

On at least some units the ID register reads back `000000` (`EPDPROBE` reports
`id=000000 expected=060401 match=0`); the panel is then driven **write-only** via a
fixed init sequence (`epd_init_writeonly`). `[hw]`

### 4.2 SPI wiring

| Signal | GPIO |
|---|---|
| SCLK | 6 |
| MOSI | 3 |
| MISO | 4 |
| CS | 9 |
| DC | 8 |
| RST | 10 |
| BUSY | 20 |

The IMU shares the SPI bus (SCLK/MOSI/MISO) with the EPD. `[bin]`

### 4.3 Color palette

Four fixed colors. "Logical" is the index→RGB used when packing; "panel" is the
measured on-glass appearance (approximate — these are camera-capture measurements,
not exact spec values).

| Index | Logical RGB | Panel (measured ≈) |
|---|---|---|
| 0 | black `#000000` | `#121412` (18,20,18) |
| 1 | white `#FFFFFF` | `#CDCEC6` (205,206,198) |
| 2 | yellow `#FFFF00` | `#CAAE3E` (202,174,62) |
| 3 | red `#FF0000` | `#A43F37` (164,63,55) |

**No grey, no blue, no green.** Intermediate tones exist only through dithering
(orange from red+yellow reads well; blue/green collapse toward greyscale). `[hw]`

### 4.4 Timing (measured)

| Operation | Value |
|---|---|
| Full refresh | **~18.8 s** (consistent over repeated wipes) |
| Partial / windowed update | **not supported** by this panel |

A 4-color BWRY panel physically requires a full waveform refresh; there is no
partial-refresh LUT (that exists only on plain B/W panels). The display flickers
through colors during refresh. Switching between pre-loaded images is possible but
every switch is a full refresh. `[hw]`

---

## 5. IMU — LSM6DS3TR-C

- ST **LSM6DS3TR-C** accelerometer/gyro, WHO_AM_I = `0x6A`. `[hw]`
- **INT1 → GPIO5, active HIGH.** Used for motion/tilt wake. `[bin]`
- Shares the SPI bus with the EPD (§4.2).

---

## 6. Button, LED, battery and pin map

### 6.1 Button

- **`BOARD_KEY_GPIO` = GPIO2, active LOW.** `[bin]`
- Wakes the device and triggers a refresh on a press; a long press enters BLE
  pairing / update mode (§12). `[hw]`
- **GPIO2 is dual-purpose:** it is also the battery-ADC input (§6.3). The stock
  firmware manages this mode-switch internally (`keyGpio=restored`). `[bin]`

### 6.2 LED

- **Status LED = GPIO21, active HIGH** (`led_flash`). `[bin]`

### 6.3 Battery sense

- **Battery = ADC1 channel 2 = GPIO2** — the same pin as the button. `[bin]`
- **Stock conversion** (recovered by disassembling the stock-class firmware): `[bin]`
  - Sample raw on ADC1_CH2 at attenuation `DB_12`; the stock sampler takes a median + valley
    over 5×20 reads and temporarily flips GPIO2 out of button mode for the read (then restores it).
  - `pinMv = adc_cali_raw_to_voltage(raw)` (factory curve-fit calibration).
  - **`batteryMv = (pinMv × 145 + 50) / 100`** — i.e. ×1.45; the on-board resistor divider
    attenuates the battery by ~0.69 onto the pin.
  - **percent**: piecewise-linear LUT — ≤ 3200 mV → 0 %, then +10 % per 100 mV up to 4000 mV → 80 %,
    and > 4089 mV → 100 %.
  - plausibility window 2800–4300 mV.

### 6.4 Full GPIO map

| GPIO | Function | Notes |
|---|---|---|
| 2 | Button (active-low) **+** battery ADC1_CH2 | dual-purpose, FW-managed |
| 3 | SPI MOSI | shared EPD/IMU |
| 4 | SPI MISO | shared EPD/IMU |
| 5 | IMU INT1 (active-high) | motion/tilt wake |
| 6 | SPI SCLK | shared EPD/IMU |
| 8 | EPD DC | |
| 9 | EPD CS | |
| 10 | EPD RST | |
| 18 | USB D− | avoided by probe routines |
| 19 | USB D+ | |
| 20 | EPD BUSY | |
| 21 | Status LED (active-high) | |

(GPIO7 was seen referenced but its function is unconfirmed.)

### 6.5 Wake model

The device deep-sleeps and wakes on three sources: `[bin]`

1. **Button** — GPIO2, LOW.
2. **IMU motion** — GPIO5, HIGH.
3. **RTC timer.**

Note: ESP32-C3 deep-sleep GPIO wake is only available on GPIO0–5, which both wake
GPIOs (2 and 5) satisfy.

---

## 7. Memory layout (stock firmware)

### 7.1 Partition table — stock V0.5.0

Read from a live device. `[hw]`

| Partition | Offset | Size | Purpose |
|---|---|---|---|
| `nvs` | `0x9000` | 256 K | NVS — personalization, RF cal, BLE bonding |
| `otadata` | `0x49000` | — | active OTA slot selector |
| `phy_init` | `0x4B000` | — | PHY init table (identical across devices) |
| `ota_0` | `0x50000` | 1.5 M | app slot 0 (factory firmware) |
| `ota_1` | `0x1D0000` | 1.5 M | app slot 1 (OTA'd firmware) |
| `storage` | `0x350000` | 12 M | image slots; image data starts at offset 0 |

> An earlier fleet version (V0.3.5) used **FATFS on `/extflash`** with a legacy
> 500-slot layout. V0.5.0 replaced this with **NVS-based slot management** (700
> slots) plus a runtime migration from the old layout. The change was storage + UX,
> not protocol (see §10). `[bin]`

### 7.2 Magic values (16 MB dump)

| Offset | Magic | Meaning |
|---|---|---|
| `0x0` | `0xE9` | ESP image header |
| `0x8000` | `0xAA50` | partition-table magic |
| app + `0x20` | `0xABCD5432` | app-descriptor magic (at `ota_0`+`0x50020`, `ota_1`+`0x1D0020`) |
| `0x9000` | `0xFCFFFFFF` | NVS page state = active |

### 7.3 NVS contents (`@0x9000`) — where personalization lives

The app image is **not** personalized — every device of the same version carries a
byte-identical app. All per-device data lives in NVS: `[bin]`

| Key | Type | Content |
|---|---|---|
| `dev_sn` | JSON | `{"serial_number":"D2XXXXN"}` — authoritative stock serial |
| `cal_mac` | blob, 6 B | device base MAC; FW checks it against the runtime MAC |
| `cal_data` | blob, **1904 B** | factory RF calibration — unique per device |
| `cal_version` | int | `1201` (uniform across devices) |
| `dev_name` | JSON | BLE GAP name (set via 0xFF01 NAME command, §8.2) |
| `dev_config` | JSON | refresh interval + icebox flag (0xFF01 CONFIG command) |
| `bt_config.conf` | blob | BLE bonding keys (`LE_LOCAL_KEY_IRK`/`IR`) — `<…>` |

> RF calibration is **MAC-bound**: the firmware compares `cal_mac` to the live MAC
> and rejects mismatched calibration (`calibration data MAC check failed: expected …
> found …`). Restoring one device's NVS onto a different unit yields the wrong
> serial/color and broken RF cal — never flash a foreign backup. `[bin]`

> **Serial survives a custom-firmware flash.** `dev_sn` lives only in NVS at
> `0x9000`. A partition-respecting reflash (write at `0x0`/`0x8000`/app offset, plus
> an `otadata` erase) does not touch `0x9000`, so the serial — and the derived color
> — survive and stay readable. The only thing that destroys them is a full-chip
> `erase-flash`: there is no eFuse or secondary copy to recover from. `[hw]`

---

## 8. Identity — MAC and serial

### 8.1 MAC addresses (one eFuse base, the rest derived)

| MAC | Value | Source |
|---|---|---|
| Base / Wi-Fi-STA | `B0:A6:04:00:11:22` *(example)* | eFuse **BLOCK1** (universal MAC) |
| Bluetooth | base **+1** → `…23` | derived at runtime (`esp_read_mac`/`ESP_MAC_BT`) |

- `esptool`/`flash_id` reads exactly the **base MAC** (BLOCK1). `[hw]`
- The **BT MAC is base+1, computed at runtime** — it is *not* stored raw in flash
  (searching the dump for the BT MAC bytes yields zero hits). `[bin]`
- The base MAC is mirrored into NVS only as `cal_mac` (RF namespace).
- The observed OUI for this fleet is `B0:A6:04` (Espressif); one earlier
  V0.3.5-only batch used a different OUI. The host bytes are device-specific and
  masked here.
- **The MAC is the reliable device identifier** — the serial differs between stock
  and custom firmware derivations, but the MAC is constant for the same silicon. `[hw]`

### 8.2 eFuse blocks

| Block | Content |
|---|---|
| BLOCK1 | base MAC (universal) — see §8.1 |
| BLOCK2 | 128-bit chip-unique ID (**not** the MAC) |
| BLOCK3 (`CUSTOM_MAC`) | empty |
| `BLOCK_USR_DATA` | empty (on every device sampled) |

### 8.3 Serial number structure

```
D2  XXXX  N
│   │     └─ color suffix  (N/W/B/R — see §9)
│   └─────── 4-char unit id (base36; no batch/date/checksum pattern observed)
└─────────── product prefix
```

- The firmware treats the serial as an **opaque ≤30-char string** (NVS `dev_sn`,
  default `SN000000000000`) and exposes it over BLE device-info. There is **no
  firmware-side color or variant derivation** anywhere in the image. `[bin]`
- The unit id bytes are base36 values with no recognizable batch/date/checksum
  structure across sampled units — effectively a per-unit counter or random id. `[bin]`

---

## 9. Case color — encoded in the last serial character

The **case color is the last character of the serial number** (in NVS `dev_sn`):

| Suffix | Color |
|---|---|
| `N` | black (noir) |
| `W` | white |
| `B` | blue |
| `R` | red |

This is derived **entirely by the Android app**, locally (`colorIndexing` →
localization key `picpak_device_color_{black|white|blue|red}`). There is:

- **no cloud lookup** for color,
- **no eFuse** involvement (`BLOCK_USR_DATA` is empty on every device),
- **no hardware/panel** involvement (the firmware is byte-identical across colors
  and the EPD ID `0x060401` is the same for all).

So: the panel is always the same 4-color BWRY part; the *housing* color is purely a
string suffix the app maps to a label. `[inf]` (app), verified consistent across
multiple devices `[hw]`.

---

## 10. Stock firmware character

- The stock firmware (`PicPak V0.5.0`, ESP-IDF v5.5) is a **pure BLE peripheral —
  no Wi-Fi**. Wi-Fi drivers, the lwIP TCP/IP stack, `esp_netif` and DHCP are **not
  linked into the image** (zero `wifi:`/`net80211`/`lwip`/`tcpip`/`esp_netif`/
  `esp_wifi` references in IROM; the `ESP_ERR_WIFI_*` strings are only the static
  `esp_err_to_name()` table). There is no stock Wi-Fi provisioning. `[bin]`
- **The phone app is the internet gateway.** It pulls photos and firmware from the
  cloud and pushes them to the device over BLE (image on 0xFF01/0xFF02, firmware on
  0xFF03). Cloud endpoints referenced by the app: `api.picpak.org/v1` (HTTPS) and a
  firmware host (`file.picpak.org`). `[inf]`
- **Cloud firmware == flashed firmware, byte-identical.** The V0.5.0 cloud image is
  bit-for-bit the same as what runs on the device; there is no per-device patch. The
  app image is not personalized — all per-device state is in NVS (§7.3). `[bin]`
- **Fleet versions:** `ota_0` holds the factory firmware (V0.3.0 or V0.3.5
  depending on batch), `ota_1` holds the OTA-upgraded V0.5.0. `[bin]`
- **The BLE protocol is unchanged across V0.3.5 → V0.5.0** (byte-identical type
  validator, same dispatcher modules, OTA-over-BLE and the no-Wi-Fi property present
  in both). `[bin]`

---

## 11. Radio, power & brownout

BLE and Wi-Fi share **one RF PHY** (a single `phy_init` partition, §7.1). The stock
firmware links BLE only (§10); Wi-Fi requires custom firmware. The power behavior
below is a **hardware property** of the board's supply and power amplifier — it
applies to *any* firmware that drives the radio, BLE included. The numbers were
captured during custom-firmware bring-up; the supply limit they reveal is not
firmware-specific.

### 11.1 Brownout at the TX peak

The RF power-amplifier transmit peak collapses a weak supply, producing an **analog
SoC brownout** (reset reason `rr=9`). This is **independent of the software brownout
detector**: even with `CONFIG_ESP_BROWNOUT_DET=n`, the analog brownout still fires on
a real voltage dip. (The `rst:0x15` tether loop in §11.4 is a *different*
failure — a USB glitch, not this brownout.) `[hw]`

The EPD refresh is the other heavy load: its charge pump draws hard for the full
~18.8 s refresh (§4.4). Radio TX peaks and the EPD charge-pump load are the two
events to budget supply for.

### 11.2 Measured TX-power threshold

Measured on a USB tether with the AP at RSSI ≈ −78 dBm: `[hw]`

| TX power | Result |
|---|---|
| ≤ 14 dBm | associates cleanly (~640 ms to connect) |
| 15 dBm | **flaps** — connect peak dips the rail → reassociation loop |

**Mitigation that works:** adaptive TX power **11 → 14 dBm** — start low, step up only
after a failed connect, persist the working level in NVS, re-probe from cold. The cap
must be applied **before the first association burst** (in the `STA_START` handler);
calling `esp_wifi_set_max_tx_power` *after* the connect trigger still lets the
association send at full power and brown out.

These thresholds are specific to the measured tether/RSSI setup — treat them as a
starting point, not an absolute spec. The point that generalizes: this board browns
out at the radio TX peak well before the SoC's nominal max TX power.

### 11.3 Wi-Fi quirks (custom-firmware operation)

- **WPA3-SAE works** (WPA2 not required); 2.4 GHz; DHCP observed with a /22 mask. The
  DHCP lease is volatile → a static/cached IP avoids the DHCP roundtrip on each wake.
- Connect-state signatures (`esp_wifi` state log): `init → auth → init` repeating
  (never reaches assoc) = wrong credentials / WPA-mode mismatch, **not** "AP not
  found". A good connect is `init → auth → assoc → run` followed by `got ip`.

### 11.4 USB / tether quirks (bench only)

- **`USB_UART_CHIP_RESET` loop (`rst:0x15`):** an active USB serial-JTAG pad plus a
  Wi-Fi TX current peak causes a VBUS glitch → reset at radio start. This is
  **tether-specific** — on battery (no USB host) it does not occur, and a plain USB
  power adapter (no host, no SOF packets) does not trigger it either. A common
  workaround is to detach the USB pad (`usb_serial_jtag_ll_phy_enable_pad(false)`) for
  the duration of the radio cycle. `[hw]`
- **Opening `/dev/ttyACM0` resets the device** (DTR/RTS pulse). A serial capture
  therefore knocks the device out of any awake state and back into boot — verify
  stability **non-invasively** (e.g. USB re-enumeration timing), not by holding the
  port open. `[hw]`

---

## 12. BLE interface (stock)

Full GATT service exposed by the stock firmware. Activated by a **long button
press** → BLE pairing / update mode. `[hw]`

| | |
|---|---|
| Service UUID | `0000FF00-0000-1000-8000-00805F9B34FB` |
| MTU | 517 |
| Appearance | `0x42` |
| Characteristics | 3, all READ/WRITE/WRITE_NO_RESP/NOTIFY+INDICATE (CCCD `0x2902`) |

The device **indicates** primarily (waits for the indicate-confirm).

| Characteristic | Role |
|---|---|
| `0xFF01` | Control / device-info dispatcher |
| `0xFF02` | Image channel + device→app responses |
| `0xFF03` | OTA-over-BLE (firmware push) |

### 12.1 Wire frame

All frames are `AA <type> <payload…> FF`: `pkt[0]=0xAA`, `pkt[1]=type`,
`pkt[len-1]=0xFF`, `len ≥ 4`. The type validator accepts exactly
`{01,02,03,04,05,30,32,34,36,38}`. `[bin]`

For image data frames: `pkt[2..3]` = image id (LE16), `pkt[4]` = data index,
`pkt[5]` = is_last, `pkt[6..7]` = data length (LE16, < 237), `pkt[8..]` = payload;
total length = data_length + 9.

### 12.2 Control dispatcher (`0xFF01`)

| Opcode | Command | Effect |
|---|---|---|
| `0x06` | NAME | r/w device name → NVS `dev_name` + BLE GAP name |
| `0x07` | CONFIG | write refresh_time (u32) + icebox flag → NVS `dev_config` |
| `0x08` | DEVICE VERSION | returns battery / batt% / hw-ver / sw-ver + serial |
| `0x09` | CAPABILITY | returns legacy=3, max-images=500, att=700, flags=`0x202` |

(`0x01–0x04` and `0x30/32/34/36/38` route to the image path.)

### 12.3 Image dispatcher (`0xFF02`)

| Opcode | Command |
|---|---|
| `0x01` | WRITE — stream image to flash (per-image MD5, temp→target) |
| `0x02` | READ — inbound log-only opcode the device sends to push data to the app |
| `0x03` | READ-REQUEST — stream an image back to the phone |
| `0x04` | MD5 — validate 16-byte MD5, copy staged→target on match |
| `0x30` | GET STATE |
| `0x32` | DELETE (1–700) |
| `0x34` | GET-ALL (bitmap, max 700) |
| `0x36` | DISPLAY (event to the e-ink task) |
| `0x38` | GET DISPLAY-STATE |

Max image count is **700** (code-confirmed). `[bin]`

### 12.4 OTA-over-BLE (`0xFF03`)

Frame `AA <type> <payload> FF`. This is the on-device update path the app uses after
downloading an image from the cloud; it is separate from the cloud transfer itself.

**START — type `0x10`, fixed 33 bytes:**
```
AA 10 | binsize(u32 LE) | version_str[26] | FF
        [2..5]            [6..31]
```
→ `esp_ota_get_next_update_partition` → `esp_ota_begin(SIZE_UNKNOWN)`.

**DATA — type `0x11`, length = datalen + 7:**
```
AA 11 | index(u16 LE) | is_last(u8) | datalen(u8 ≤237) | data[datalen] | FF
        [2..3]          [4]           [5]                 [6..]
```
→ `esp_ota_write`; on `is_last == 1` → `esp_ota_end` → `esp_ota_set_boot_partition`
→ reboot.

**ACK:** the device notifies ACKs back on `0xFF03`; the app matches the ACK sequence
number against the sent chunk index for flow control. `[bin]`

---

## 13. Image format (summary)

| | |
|---|---|
| Geometry | 400 × 300, 2 bpp, 4 px/byte, MSB-first |
| Size | 30 000 bytes packed |
| Orientation | vertically flipped before sending |
| Storage | `storage` partition `@0x350000`, image data from offset 0 |

Packing: each byte holds 4 pixels, `value |= code << (6 - 2*i)` for `i` in 0..3
(MSB-first, 2 bits per pixel).

The vendor app's RGB→palette conversion (nearest-color metric and dither) is
documented separately — it is an app concern, not a device property.

---

*Sources: esptool flash/eFuse dumps, radare2/Ghidra on the stock ESP-IDF images,
disassembly of the vendor Android app, and measurements on live hardware. All
device-specific identifiers above are format-preserving placeholders (see the
air-gap note at the top).*
