# PicPak — Stock BLE Protocol

The complete GATT interface the stock firmware exposes over Bluetooth Low Energy:
service layout, the `AA … FF` wire frame, the three characteristic dispatchers, the
image-upload sequence, and OTA-over-BLE. This is the byte-level companion to the
overview in [`device.md` §12](device.md).

Statements are marked binary-verified `[bin]` (Ghidra decompile of the stock ESP-IDF
image, cross-checked against a working `bleak` client on a live device) or inferred
from the vendor app `[inf]`. Firmware version: `PicPak V0.5.0` (ESP-IDF v5.5); the
protocol is unchanged back to V0.3.5 (§9).

> **Air-gap note.** No real device identifiers appear here. Serials are shown as the
> format-preserving placeholder `D2XXXXN` (or the firmware default `SN000000000000`);
> see the stub conventions at the top of [`device.md`](device.md). The function
> addresses in §10 are RE artifacts of the public V0.5.0 image, not secrets.

---

## 1. Activation

The BLE GATT server is brought up when the user **long-presses the device button** →
BLE pairing / update mode. The stock firmware is a **pure BLE peripheral with no
Wi-Fi** (see [`device.md` §10](device.md)); the phone app is the gateway that pulls
photos and firmware from the cloud and pushes them to the device over the channels
below. `[bin]`

---

## 2. GATT layout

| | |
|---|---|
| Service UUID | `0000FF00-0000-1000-8000-00805F9B34FB` |
| MTU | 517 |
| Appearance | `0x42` |
| Characteristics | 3, **all** READ / WRITE / WRITE_NO_RESP / NOTIFY + INDICATE (CCCD `0x2902`) |

| Characteristic | Role |
|---|---|
| `0xFF01` | Control / device-info dispatcher |
| `0xFF02` | Image channel + device→app responses |
| `0xFF03` | OTA-over-BLE (firmware push) |

The device **indicates** primarily (it waits for the indicate-confirm). `[bin]`

---

## 3. Wire frame

Every frame is framed by `0xAA … 0xFF`:

```
AA | type | payload… | FF
[0]  [1]    [2 .. N-2]  [N-1]
```

Validation (parser): `len ≥ 4`, `pkt[0] == 0xAA`, `pkt[N-1] == 0xFF`, and `type` must
be in the accepted set. The type validator was checked across all 256 values and
accepts exactly: `[bin]`

```
{ 0x01, 0x02, 0x03, 0x04, 0x05, 0x30, 0x32, 0x34, 0x36, 0x38 }
```

### Return codes `[bin]`

| Layer | Code | Meaning |
|---|---|---|
| Frame parser | `0` | OK |
| | `1` | invalid |
| | `2` | bad header (`pkt[0] != 0xAA`) |
| | `3` | bad tail (`pkt[N-1] != 0xFF`) |
| | `4` | bad type |
| | `5` | too large |
| Dispatcher | `0x102` | frame too short |
| | `0xFFFFFFFF` | bad frame |
| | `0x106` | unknown command |

### Image data frame

Frames carrying image bytes use this payload layout (e.g. `type = 0x01` on `0xFF02`):

```
AA | type | image_id(u16 LE) | data_index(u8) | is_last(u8) | data_length(u16 LE) | data[data_length] | FF
[0]  [1]    [2..3]             [4]              [5]           [6..7]                 [8..]               [N-1]
```

`data_length < 237`; total frame length `= data_length + 9`. The vendor app sends
~200 payload bytes per write, using `write_gatt_char(..., response=True)`. `[bin]`

---

## 4. Control characteristic — `0xFF01`

Dispatcher. Opcodes `0x01–0x04` and `0x30/0x32/0x34/0x36/0x38` are routed to the image
path (§5); the `0x30`-range is selected via bit-mask `0x155`. The control-specific
opcodes: `[bin]`

| Opcode | Command | Behavior |
|---|---|---|
| `0x06` | NAME | r/w at `pkt[2]`. Write → NVS `dev_name` (JSON) + BLE GAP name; read → notify |
| `0x07` | CONFIG | write (`len ≥ 9`): `refresh_time` (u32) + icebox flag at `pkt[7]` → NVS `dev_config` (JSON) |
| `0x08` | DEVICE VERSION | `cmd == 2` at `pkt[2]` → battery / battery% / hw-version / sw-version + serial; replies with a 0x38B response on `0xFF02` |
| `0x09` | CAPABILITY | replies with a 14-byte response on `0xFF02`: `legacy=3`, `max-images=500`, `att=700`, `flags=0x202` |

---

## 5. Image characteristic — `0xFF02`

Image transfer plus the channel the device uses to push data back to the app. `[bin]`

| Opcode | Command | Behavior |
|---|---|---|
| `0x01` | WRITE | stream an image to flash (per-image MD5, staged temp → target, id `1..N`) |
| `0x02` | READ | inbound log-only — the opcode the **device** sends to push data to the app |
| `0x03` | READ-REQUEST | stream an image back to the phone |
| `0x04` | MD5 | get/set: validate a 16-byte MD5; on match copy staged → target and index it |
| `0x30` | GET STATE | |
| `0x32` | DELETE | image `1..700`; replies with a 6-byte response on `0xFF01` |
| `0x34` | GET-ALL | bitmap of present images, max 700 |
| `0x36` | DISPLAY | posts an event to the e-ink task (full refresh) |
| `0x38` | GET DISPLAY-STATE | replies with a 7-byte response |

The slot manager supports **700 images** (code-confirmed; V0.5.0). Note the
capability response still reports `max-images=500` with `att=700` — treat `att` as the
effective ceiling. `[bin]`

---

## 6. Image upload sequence (stock)

The app packs the 400×300 image to 30 000 bytes (2 bpp, 4 px/byte, MSB-first — see
[`image-pipeline.md`](image-pipeline.md)), then streams it in chunks and commits with
an MD5.

**Data packet** (`create_image_packet`, type `0x01`):

```
AA 01 | image_id(u16 LE) | packet_idx(u8) | is_last(u8) | len(u16 LE) | chunk[len] | FF
```

**Commit packet** (`create_md5_packet`, type `0x04`):

```
AA 04 | image_id(u16 LE) | 00 | md5[16] | FF
```

After all packets are received and the MD5 matches, the staged image is promoted to
its target slot. A `DISPLAY` (`0x36`) then triggers the full e-ink refresh. `[bin]`

A verified reference client (`bleak`, Python) exists in the wild (the "miley42" gist);
the packet builders above match it byte-for-byte. `[inf]`

---

## 7. OTA-over-BLE — `0xFF03`

The on-device firmware-update path. The app uses it **after** downloading a firmware
image from the cloud; it is separate from the cloud transfer itself. Same `AA … FF`
framing; the parser validates `len > 3`, leading `0xAA`, trailing `0xFF`. `[bin]`

### START — type `0x10`, fixed 33 bytes

```
AA 10 | binsize(u32 LE) | version_str[26] | FF
        [2..5]            [6..31]
```

Firmware: `esp_ota_get_next_update_partition(NULL)` → `esp_ota_begin(part,
OTA_SIZE_UNKNOWN)`, sets an "OTA in progress" flag, logs partition / size / offset /
version. If a session is already running it calls `esp_ota_abort` and logs *"Previous
OTA session not finished, aborting"*. `[bin]`

### DATA — type `0x11`, length `= datalen + 7`

```
AA 11 | index(u16 LE) | is_last(u8) | datalen(u8 ≤237) | data[datalen] | FF
        [2..3]          [4]           [5]                 [6..]
```

Firmware: without a prior START → *"Received DATA before START, ignoring"*. Otherwise
`esp_ota_write(handle, data, datalen)`; a write error logs *"esp_ota_write failed at
index %d"*. On `is_last == 1` → `esp_ota_end` → `esp_ota_set_boot_partition` → *"OTA
complete, rebooting …"* → reboot. An unknown type logs *"Unknown OTA packet type:
0x%02X"*. `[bin]`

### ACK (notify on `0xFF03`)

The device notifies ACKs back on `0xFF03`. The app matches the ACK sequence number
against the sent chunk index for flow control (it logs a *sequence mismatch* / *ACK
response too short* on error). A device rejection comes back as an ACK with an error
code → app status `otaUpgradeRejected` / `otaAlreadyInProgress`. A START is treated as
successful if **no** fail-ACK arrives within the timeout. App-side enums:
`OtaStatus`, `FirmwareUpdatePhase`, `FirmwareUpdateErrorKind`. `[inf]`

---

## 8. Serial & device info

The firmware treats the serial as an **opaque ≤30-character string**: NVS key
`dev_sn` = JSON `{"serial_number":"D2XXXXN"}`, loaded into a global buffer (default
`SN000000000000`) and reported over device-info (`0x08` on `0xFF01`). There is **no
firmware-side color or variant derivation** — the case color is derived entirely by
the app from the last serial character (see [`device.md` §9](device.md)). `[bin]`

---

## 9. Version stability

The BLE protocol is **unchanged across V0.3.5 → V0.5.0**: the type validator is
byte-identical, the dispatcher modules are the same, and both versions ship
OTA-over-BLE and the no-Wi-Fi property. The V0.3.5 → V0.5.0 changes are storage
(FATFS → NVS, 500 → 700 slots) and UX, **not** protocol. `[bin]`

---

## 10. Verification & provenance

Recovered from the stock V0.5.0 image with Ghidra (decompile), cross-checked against a
live device with a `bleak` client. Key handlers in the public V0.5.0 image (addresses
are RE artifacts, version-specific):

| Element | Address / symbol |
|---|---|
| GATT server init | `FUN_ram_42019596` |
| Frame router | `FUN_ram_42012f3e` |
| `0xFF01` control dispatcher | `FUN_ram_4200f98e` |
| `0xFF02` image dispatcher | `FUN_ram_420118a8` |
| Image path | `FUN_ram_42011d00` |
| Data-frame parser | `FUN_ram_42014e56` |
| Type validator | `FUN_ram_42014a52` |
| `0xFF03` OTA handler / parser | `FUN_ram_42017c6e` / `FUN_ram_42017a1a` (module `ota_protocol`) |

The BLE stack is bluedroid (NVS `bt_config.conf` holds the bonding keys). The app side
(`libapp.so`, Flutter) exposes `BleFrame` (1-byte command opcode), a `FrameType` enum,
and `BleCommandClient`; cloud OTA is a separate HTTPS path (`api.picpak.org/v1`). `[inf]`
