#pragma once

#include <stdbool.h>
#include <stddef.h>
#include "config.h"

/* Helpers for the Berry-owned multi-WiFi store namespace. The policy remains in
 * policy.be; this file only supports console provisioning and one-shot legacy migration. */
bool wifi_store_add(const char *ssid, const char *pass, int prio, char *out_key, size_t out_cap);
bool wifi_store_put_key(const char *key, const char *ssid, const char *pass, int prio);
bool wifi_store_del(const char *slug_or_key);
bool wifi_store_has_entries(void);
void wifi_store_list(void);
bool wifi_store_migrate_legacy(const picpak_cfg_t *cfg);
