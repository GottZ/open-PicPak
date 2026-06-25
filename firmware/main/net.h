/* Network: WiFi STA + HTTP pull of a ready-made panel frame. */
#pragma once
#include <stdint.h>
#include <stdbool.h>
#include <stddef.h>

/* Connects WiFi STA (WPA2/WPA3). Returns true once an IP is obtained.
 * Uses an NVS connect cache (BSSID+channel -> no scan, static IP -> no DHCP);
 * on a stale cache it automatically falls back to a full scan + DHCP. */
bool net_wifi_connect(const char *ssid, const char *pass, int timeout_ms);

/* Max WiFi TX power. Default 11 dBm (brownout headroom under load); per-device override
 * persisted in NVS so a weak-signal site can raise it without a reflash. Hard-capped at
 * 14 dBm (15 dBm collapses the TX-PA supply). net_set_tx_dbm applies on the next connect. */
void net_set_tx_dbm(int dbm);
int  net_tx_dbm(void);

/* Forcing connect for the Berry net policy (scan-match / password-rotation): unlike
 * net_wifi_connect it does NOT short-circuit on an existing association -- it (re)associates
 * against the passed SSID/pass and returns the real result. Cold, base TX, fail-fast. */
bool net_wifi_try(const char *ssid, const char *pass, int timeout_ms);

/* Getters for the Berry net surface. net_ip: the IP snapshot (survives net_wifi_stop, valid
 * until overwritten); net_ssid: SSID of the current/last attempt; net_rssi: only while
 * associated (false otherwise). All return false when no value is available. */
bool net_ip(char *buf, size_t cap);
bool net_ssid(char *buf, size_t cap);
bool net_rssi(int *out);

/* A scanned access point. */
typedef struct { char ssid[33]; int8_t rssi; uint8_t auth; } net_ap_t;

/* Active scan; fills out[0..max-1], returns the count (<= max) or -1 on error. Drops any
 * current association (suppressed auto-reconnect) -> scan BEFORE the final connect. */
int net_wifi_scan(net_ap_t *out, int max);

/* Generic HTTP GET for the Berry net surface: GET url -> up to cap bytes into buf, *outlen set.
 * URL must be http(s):// (validated; rejects other schemes and over-length). cap is the hard
 * size limit (the binding keeps it well under the RAM spike). Returns false on a bad URL,
 * non-200, an over-cap response, or a transport error. */
bool net_http_get(const char *url, uint8_t *buf, size_t cap, size_t *outlen);

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
