/* In-memory log ring buffer for FW-internal visibility on the USB tether.
 *
 * Problem: during run_cycle the USB pad is detached from the bus (WiFi coexistence
 * fix) -> all ESP_LOG output of the cycle (net/http/epd) is invisible on the serial
 * tether. An open host port during the cycle also sabotages it, and the required
 * port-open triggers rst:0x15.
 *
 * Solution: an esp_log_set_vprintf hook additionally mirrors every log line into an
 * RTC-RAM ring. RTC-RAM survives deep sleep AND rst:0x15 (like s_tm_* in main.c)
 * -> fetch flow: close port, run PRESS n, open port (rst), LOG -> the logs of the
 * blind window appear. In the field (no host) a harmless no-op. */
#pragma once
#include "logbuf_core.h"

/* Install the esp_log_set_vprintf hook (idempotent). The RTC-RAM ring itself
 * persists across resets/wakes and is NOT cleared here. */
void logbuf_init(void);

/* Print the ring contents chronologically to stdout (for the LOG console command). */
void logbuf_dump(void);

/* Clear the ring (LOG CLEAR) -> before a measurement run, so only the cycle is in it. */
void logbuf_clear(void);

/* Set the current log epoch (usually the NVS boot counter). Changing epoch clears the
 * RTC ring state; setting the same epoch is a no-op. */
void logbuf_set_epoch(uint32_t epoch);

/* Snapshot ring state for telemetry/log-streaming cursors. */
void logbuf_get_state(logbuf_state_t *out);

/* Export the ring header-safe into dst (always 0-terminated): \n -> '|',
 * \r + other control characters dropped. For the X-Picpak-Log HTTP header with
 * which the FW exfiltrates its logs over WiFi (tether serial is dead during the
 * cycle). Returns the copied string length. */
#include <stddef.h>
size_t logbuf_export(char *dst, size_t cap);
