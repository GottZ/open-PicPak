/* Persistent device config in NVS — replaces the earlier compile-time
 * wifi_config.h. Source of truth for WiFi + frame URL.
 * Provisioned via the serial console (console.c). */
#pragma once
#include <stdbool.h>
#include "esp_err.h"

/* WPA2-PSK <=63 chars, SSID <=32, URL generous. Incl. NUL. */
typedef struct {
    char ssid[33];
    char pass[65];
    char url[129];
} picpak_cfg_t;

/* Loads ssid/pass/url from NVS (namespace "picpak").
 * Returns true if ssid AND url are non-empty (pass may be empty, open network).
 * Missing keys yield empty strings. */
bool cfg_load(picpak_cfg_t *out);

esp_err_t cfg_set_wifi(const char *ssid, const char *pass);
esp_err_t cfg_set_url(const char *url);
esp_err_t cfg_erase(void);   /* erases all keys in namespace "picpak" */

/* "wake_s": deep-sleep interval between field cycles (battery reconnect cadence), seconds. NVS
 * (namespace "picpak", u32). Policy-as-data: resolves the run_cycle TODO(config-pull) for the
 * interval. cfg_wake_s returns the stored value or `fallback` when unset; cfg_set_wake_s persists.
 * Console: SETWAKE <s>. DEFAULT_WAKE_S is the compile-time fallback used when wake_s is unset. */
#define DEFAULT_WAKE_S 3600   /* 1 h */
uint32_t  cfg_wake_s(uint32_t fallback);
esp_err_t cfg_set_wake_s(uint32_t secs);

/* "verified": set true after a cycle actually connected + fetched a frame; reset to
 * false whenever new creds are written (SETWIFI/SETURL). Drives the on-device setup
 * screen (shown while not verified) — see main.c. */
bool cfg_is_verified(void);
void cfg_set_verified(bool v);

/* "force_setup": set by a triple-press to open the setup console on the next boot
 * (survives the browser's port-open reset, unlike RTC-RAM). Cleared after the console
 * has run. */
bool cfg_force_setup(void);
void cfg_set_force_setup(bool v);
