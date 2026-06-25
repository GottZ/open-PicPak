/* OTA: FW update over WiFi (plain HTTP, no TLS — consistent with the frame pull).
 *
 * The server delivers its provided FW version in the header X-Firmware-Version
 * (piggybacked on the frame fetch). If it differs from the running version, the FW
 * pulls the new image from <host>/firmware.bin into the inactive OTA partition and
 * sets it as the boot partition. Rollback (CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE)
 * protects against a broken FW: the freshly booted app MUST confirm itself, otherwise
 * the bootloader falls back to the previous partition on the next reboot.
 */
#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* True if the running app was freshly booted via OTA and is still in state
 * PENDING_VERIFY waiting for its confirmation. Then do NOT open the setup console,
 * but go straight to run_cycle -> connect quickly + mark_valid (otherwise rollback). */
bool ota_is_pending(void);

/* Confirms the running app (cancel rollback) if it runs in state PENDING_VERIFY after
 * an OTA. Call once the core function is verified — here: after a successful WiFi
 * connect (connectivity = the device can receive an OTA again next time -> no brick).
 * No-op if not in the PENDING_VERIFY state. */
void ota_mark_valid_if_pending(void);

/* Compares server_version with the running version (string inequality). On a
 * difference: GET <host>/firmware.bin (host/port derived from frame_url) -> writes
 * the image into the next OTA partition and sets it as the boot partition.
 * Returns true if an image was successfully written + boot-set
 *  -> the caller MUST then call esp_restart() to start the new FW.
 * Returns false on "no change" or on any error (then keeps running unchanged). */
bool ota_update_if_changed(const char *frame_url, const char *server_version);

/* Reset-proof diagnostics: increments an NVS boot counter and records the reset
 * reason (esp_reset_reason) of this boot. Call early in app_main.
 * Reveals crash/brownout/WDT during an OTA, even when the logbuf ring has long
 * overwritten the boot line by the next fetch. */
void ota_record_boot(void);

/* Reset-proof boot counter from NVS namespace "otadiag", key "boots". This is the
 * canonical boot-count source for telemetry, auth counters, and log epochs. */
uint32_t ota_boots(void);

/* Diagnostics: prints running/boot/next/last_invalid, version, ota_0/ota_1 states and
 * the NVS diagnostics (boots/reset_reason/ota_stage) (console command OTA). */
void ota_print_status(void);

/* Compact single-line diagnostics (running partition+version, ota_0/ota_1 states,
 * last_invalid, NVS counters) -> exfiltratable as HTTP header X-Picpak-Diag, so that
 * the OTA history is visible in the server log on battery (without USB/serial).
 * State codes: 0=NEW 1=PENDING_VERIFY 2=VALID 3=INVALID 4=ABORTED 255=UNDEFINED.
 * Returns the string length. */
size_t ota_diag_export(char *buf, size_t cap);
