#include "wifi_store.h"
#include "store.h"
#include <ctype.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#define WIFI_KEY_PREFIX "wifi."
#define WIFI_MIGRATED_KEY "migrated_wifi"

static uint32_t fnv1a(const char *s)
{
    uint32_t h = 2166136261u;
    while (s && *s) {
        h ^= (unsigned char)*s++;
        h *= 16777619u;
    }
    return h;
}

static bool wifi_key_for_ssid(const char *ssid, char *out, size_t cap)
{
    if (!ssid || !ssid[0] || !out || cap == 0) return false;
    char slug[40];
    size_t n = 0;
    for (const unsigned char *p = (const unsigned char *)ssid; *p && n < sizeof(slug) - 1; p++) {
        unsigned char c = *p;
        if (isalnum(c)) slug[n++] = (char)tolower(c);
        else if (c == '-' || c == '_') slug[n++] = (char)c;
        else if (n > 0 && slug[n - 1] != '-') slug[n++] = '-';
    }
    while (n > 0 && slug[n - 1] == '-') n--;
    if (n == 0) memcpy(slug, "net", 4);
    else slug[n] = '\0';
    int w = snprintf(out, cap, WIFI_KEY_PREFIX "%s-%04x", slug, (unsigned)(fnv1a(ssid) & 0xffffu));
    return w > 0 && (size_t)w < cap;
}

static bool append_char(char *out, size_t cap, size_t *pos, char c)
{
    if (*pos + 1 >= cap) return false;
    out[(*pos)++] = c;
    out[*pos] = '\0';
    return true;
}

static bool append_lit(char *out, size_t cap, size_t *pos, const char *s)
{
    while (*s) if (!append_char(out, cap, pos, *s++)) return false;
    return true;
}

static bool append_json_string(char *out, size_t cap, size_t *pos, const char *s)
{
    if (!append_char(out, cap, pos, '"')) return false;
    for (const unsigned char *p = (const unsigned char *)(s ? s : ""); *p; p++) {
        unsigned char c = *p;
        if (c == '"' || c == '\\') {
            if (!append_char(out, cap, pos, '\\') || !append_char(out, cap, pos, (char)c)) return false;
        } else if (c < 0x20) {
            static const char hex[] = "0123456789abcdef";
            if (!append_lit(out, cap, pos, "\\u00") ||
                !append_char(out, cap, pos, hex[c >> 4]) ||
                !append_char(out, cap, pos, hex[c & 0xf])) return false;
        } else {
            if (!append_char(out, cap, pos, (char)c)) return false;
        }
    }
    return append_char(out, cap, pos, '"');
}

static bool wifi_json(char *out, size_t cap, const char *ssid, const char *pass, int prio)
{
    size_t pos = 0;
    if (cap == 0) return false;
    out[0] = '\0';
    if (!append_lit(out, cap, &pos, "{\"ssid\":") ||
        !append_json_string(out, cap, &pos, ssid) ||
        !append_lit(out, cap, &pos, ",\"pass\":") ||
        !append_json_string(out, cap, &pos, pass ? pass : "") ||
        !append_lit(out, cap, &pos, ",\"prio\":")) return false;
    int w = snprintf(out + pos, cap - pos, "%d}", prio);
    return w > 0 && (size_t)w < cap - pos;
}

bool wifi_store_put_key(const char *key, const char *ssid, const char *pass, int prio)
{
    char json[192];
    return key && ssid && ssid[0] && wifi_json(json, sizeof json, ssid, pass, prio) && store_put(key, json);
}

bool wifi_store_add(const char *ssid, const char *pass, int prio, char *out_key, size_t out_cap)
{
    char key[64];
    if (!wifi_key_for_ssid(ssid, key, sizeof key)) return false;
    if (!wifi_store_put_key(key, ssid, pass, prio)) return false;
    if (out_key && out_cap) snprintf(out_key, out_cap, "%s", key);
    return true;
}

bool wifi_store_del(const char *slug_or_key)
{
    if (!slug_or_key || !slug_or_key[0]) return false;
    char key[64];
    int w;
    if (strncmp(slug_or_key, WIFI_KEY_PREFIX, strlen(WIFI_KEY_PREFIX)) == 0)
        w = snprintf(key, sizeof key, "%s", slug_or_key);
    else
        w = snprintf(key, sizeof key, WIFI_KEY_PREFIX "%s", slug_or_key);
    if (w <= 0 || (size_t)w >= sizeof key) return false;
    return store_remove(key);
}

bool wifi_store_has_entries(void)
{
    return store_has_prefix(WIFI_KEY_PREFIX);
}

void wifi_store_list(void)
{
    store_list_prefix(WIFI_KEY_PREFIX);
}

bool wifi_store_migrate_legacy(const picpak_cfg_t *cfg)
{
    char flag[2];
    if (!cfg || !cfg->ssid[0] || store_fetch(WIFI_MIGRATED_KEY, flag, sizeof flag) >= 0) return false;
    if (!wifi_store_put_key("wifi.0", cfg->ssid, cfg->pass, 0)) return false;
    store_put(WIFI_MIGRATED_KEY, "1");
    return true;
}
