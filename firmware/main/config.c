#include "config.h"
#include <string.h>
#include "nvs.h"

#define NS "picpak"

static void load_str(nvs_handle_t h, const char *key, char *out, size_t cap)
{
    size_t len = cap;
    if (nvs_get_str(h, key, out, &len) != ESP_OK) {
        out[0] = '\0';
    }
}

bool cfg_load(picpak_cfg_t *out)
{
    memset(out, 0, sizeof(*out));
    nvs_handle_t h;
    if (nvs_open(NS, NVS_READONLY, &h) != ESP_OK) {
        return false;   /* namespace does not exist yet -> no config */
    }
    load_str(h, "ssid", out->ssid, sizeof(out->ssid));
    load_str(h, "pass", out->pass, sizeof(out->pass));
    load_str(h, "url",  out->url,  sizeof(out->url));
    nvs_close(h);
    return out->ssid[0] != '\0' && out->url[0] != '\0';
}

static esp_err_t set_str(const char *key, const char *val)
{
    nvs_handle_t h;
    esp_err_t e = nvs_open(NS, NVS_READWRITE, &h);
    if (e != ESP_OK) return e;
    e = nvs_set_str(h, key, val);
    if (e == ESP_OK) e = nvs_commit(h);
    nvs_close(h);
    return e;
}

static bool get_flag(const char *key)
{
    nvs_handle_t h;
    uint8_t v = 0;
    if (nvs_open(NS, NVS_READONLY, &h) == ESP_OK) {
        if (nvs_get_u8(h, key, &v) != ESP_OK) v = 0;
        nvs_close(h);
    }
    return v != 0;
}

static void set_flag(const char *key, bool val)
{
    nvs_handle_t h;
    if (nvs_open(NS, NVS_READWRITE, &h) != ESP_OK) return;
    uint8_t cur = 0;
    bool have = (nvs_get_u8(h, key, &cur) == ESP_OK);
    if (!have || (cur != 0) != val) {   /* write only on change -> spare flash wear */
        if (nvs_set_u8(h, key, val ? 1 : 0) == ESP_OK) nvs_commit(h);
    }
    nvs_close(h);
}

esp_err_t cfg_set_wifi(const char *ssid, const char *pass)
{
    esp_err_t e = set_str("ssid", ssid);
    if (e == ESP_OK) e = set_str("pass", pass ? pass : "");
    if (e == ESP_OK) set_flag("verified", false);   /* new creds -> must re-prove */
    return e;
}

esp_err_t cfg_set_url(const char *url)
{
    esp_err_t e = set_str("url", url);
    if (e == ESP_OK) set_flag("verified", false);    /* new URL -> must re-prove */
    return e;
}

uint32_t cfg_wake_s(uint32_t fallback)
{
    nvs_handle_t h;
    uint32_t v = 0;
    if (nvs_open(NS, NVS_READONLY, &h) == ESP_OK) {
        if (nvs_get_u32(h, "wake_s", &v) != ESP_OK) v = 0;
        nvs_close(h);
    }
    return v ? v : fallback;   /* 0 / unset -> caller's compile-time default */
}

esp_err_t cfg_set_wake_s(uint32_t secs)
{
    nvs_handle_t h;
    esp_err_t e = nvs_open(NS, NVS_READWRITE, &h);
    if (e != ESP_OK) return e;
    e = nvs_set_u32(h, "wake_s", secs);
    if (e == ESP_OK) e = nvs_commit(h);
    nvs_close(h);
    return e;
}

bool cfg_is_verified(void)        { return get_flag("verified"); }
void cfg_set_verified(bool v)     { set_flag("verified", v); }
bool cfg_force_setup(void)        { return get_flag("fsetup"); }
void cfg_set_force_setup(bool v)  { set_flag("fsetup", v); }

esp_err_t cfg_erase(void)
{
    nvs_handle_t h;
    esp_err_t e = nvs_open(NS, NVS_READWRITE, &h);
    if (e != ESP_OK) return e;
    e = nvs_erase_all(h);              /* only namespace "picpak" */
    if (e == ESP_OK) e = nvs_commit(h);
    nvs_close(h);
    return e;
}
