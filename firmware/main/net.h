/* Network: WiFi STA + HTTP pull of a ready-made panel frame. */
#pragma once
#include <stdint.h>
#include <stdbool.h>
#include <stddef.h>

/* Connects WiFi STA (WPA2/WPA3). Returns true once an IP is obtained.
 * Uses an NVS connect cache (BSSID+channel -> no scan, static IP -> no DHCP);
 * on a stale cache it automatically falls back to a full scan + DHCP. */
bool net_wifi_connect(const char *ssid, const char *pass, int timeout_ms);

/* Discard the connect cache -> the next connect does a full scan + DHCP and
 * re-caches. For tests (cold baseline vs. warm) and after a network change. */
void net_cache_clear(void);

/* Turn off the WiFi RF modem once the frame is fetched and no OTA is pending: the
 * subsequent EPD refresh needs no network. Prevents DTIM RX bursts from coinciding
 * with the EPD charge-pump peaks (brownout), and lowers the baseline current during
 * the refresh. The next net_wifi_connect rebuilds the stack normally. */
void net_wifi_stop(void);

/* GET url -> fills buf (exactly frame_len bytes expected). Optionally reads the headers
 * "X-Next-Wake-Seconds" into *next_wake_s (0 = not set) and "X-Firmware-Version"
 * into fw_version (set to "" if the header is missing; NULL/0 = do not query).
 * Returns true on exactly frame_len received bytes and HTTP 200. */
bool net_http_fetch_frame(const char *url, uint8_t *buf, size_t frame_len,
                          uint32_t *next_wake_s, char *fw_version, size_t fw_version_len);

/* Phase times (ms) of the most recent net_wifi_connect: scan_auth = wifi_start..assoc,
 * dhcp = assoc..got_ip. Returns -1 if the respective phase was not captured.
 * For power profiling on the tether (values survive in RTC-RAM, output on the
 * next boot before the WiFi start kills the USB-CDC port). */
void net_last_connect_ms(int32_t *scan_auth, int32_t *dhcp);
