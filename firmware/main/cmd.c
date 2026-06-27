/* Berry C2 command surface + executor (Doc 13 Wave 3a). See cmd.h.
 *
 * Bindings mirror netberry.c: each l_* gates arity + type at the entry and never touches an
 * unchecked slot. The surface is the SAFE subset (no wifi connect/stop re-drive, no drawing, no
 * OTA-trigger/erase -- those earn their own negative-probed wave). Config/NVS writes + read-only
 * queries + the three control-flow intents. */
#include "cmd.h"
#include "config.h"
#include "wifi_store.h"
#include "net.h"
#include "store.h"   /* store_register / rtc_register */
#include "dev.h"     /* dev_register */
#include "esp_log.h"
#include "nvs.h"
#include <string.h>

static const char *TAG = "c2";

/* --- control-flow intent channel (set by reboot/refresh/sleep, taken by the caller) --- */
static cmd_intent_t s_intent;
static uint32_t     s_intent_sleep_s;

#define SLEEP_CLAMP_S 86400u   /* 24 h: a bad sleep value can't strand the device */

/* --- config / NVS writes --- */
static int l_set_url(bvm *vm)
{
    if (be_top(vm) < 1 || !be_isstring(vm, 1)) { be_pushbool(vm, 0); be_return(vm); }
    be_pushbool(vm, cfg_set_url(be_tostring(vm, 1)) == ESP_OK);
    be_return(vm);
}

static int l_set_wifi(bvm *vm)
{
    if (be_top(vm) < 2 || !be_isstring(vm, 1) || !be_isstring(vm, 2)) { be_pushbool(vm, 0); be_return(vm); }
    be_pushbool(vm, cfg_set_wifi(be_tostring(vm, 1), be_tostring(vm, 2)) == ESP_OK);
    be_return(vm);
}

static int l_wifi_add(bvm *vm)
{
    if (be_top(vm) < 2 || !be_isstring(vm, 1) || !be_isstring(vm, 2)) { be_pushbool(vm, 0); be_return(vm); }
    int prio = (be_top(vm) >= 3 && be_isint(vm, 3)) ? be_toint(vm, 3) : 0;
    char key[64];
    be_pushbool(vm, wifi_store_add(be_tostring(vm, 1), be_tostring(vm, 2), prio, key, sizeof key));
    be_return(vm);
}

/* nvs_set(ns, key, val) -> bool. Generic NVS string write (mirrors the NVSSET console cmd). */
static int l_nvs_set(bvm *vm)
{
    if (be_top(vm) < 3 || !be_isstring(vm, 1) || !be_isstring(vm, 2) || !be_isstring(vm, 3)) {
        be_pushbool(vm, 0); be_return(vm);
    }
    nvs_handle_t h;
    esp_err_t e = nvs_open(be_tostring(vm, 1), NVS_READWRITE, &h);
    if (e == ESP_OK) {
        e = nvs_set_str(h, be_tostring(vm, 2), be_tostring(vm, 3));
        if (e == ESP_OK) e = nvs_commit(h);
        nvs_close(h);
    }
    be_pushbool(vm, e == ESP_OK);
    be_return(vm);
}

/* --- net maintenance + read-only queries (NO connect/stop here) --- */
static int l_net_clear(bvm *vm) { net_cache_clear(); be_return_nil(vm); }

static int l_tx_power(bvm *vm)
{
    if (be_top(vm) >= 1 && be_isint(vm, 1)) net_set_tx_dbm(be_toint(vm, 1));
    be_pushint(vm, net_tx_dbm());
    be_return(vm);
}

static int l_ip(bvm *vm)   { char b[20]; if (net_ip(b, sizeof b))   { be_pushstring(vm, b); be_return(vm); } be_return_nil(vm); }
static int l_ssid(bvm *vm) { char b[33]; if (net_ssid(b, sizeof b)) { be_pushstring(vm, b); be_return(vm); } be_return_nil(vm); }
static int l_rssi(bvm *vm) { int r;      if (net_rssi(&r))          { be_pushint(vm, r);    be_return(vm); } be_return_nil(vm); }
static int l_connected(bvm *vm) { be_pushbool(vm, net_is_connected()); be_return(vm); }

/* --- control-flow intents: set-and-return; the caller actions them AFTER persisting any ack --- */
static int l_reboot(bvm *vm)  { (void)vm; s_intent = CMD_INTENT_REBOOT;  be_return_nil(vm); }
static int l_refresh(bvm *vm) { (void)vm; s_intent = CMD_INTENT_REFRESH; be_return_nil(vm); }
static int l_device_sleep(bvm *vm)
{
    int s = (be_top(vm) >= 1 && be_isint(vm, 1)) ? be_toint(vm, 1) : 0;
    if (s < 0) s = 0;
    if ((uint32_t)s > SLEEP_CLAMP_S) s = SLEEP_CLAMP_S;
    s_intent = CMD_INTENT_SLEEP;
    s_intent_sleep_s = (uint32_t)s;
    be_return_nil(vm);
}

void cmd_register(bvm *vm)
{
    be_regfunc(vm, "set_url",      l_set_url);
    be_regfunc(vm, "set_wifi",     l_set_wifi);
    be_regfunc(vm, "wifi_add",     l_wifi_add);
    be_regfunc(vm, "nvs_set",      l_nvs_set);
    be_regfunc(vm, "net_clear",    l_net_clear);
    be_regfunc(vm, "tx_power",     l_tx_power);
    be_regfunc(vm, "ip",           l_ip);
    be_regfunc(vm, "ssid",         l_ssid);
    be_regfunc(vm, "rssi",         l_rssi);
    be_regfunc(vm, "connected",    l_connected);
    be_regfunc(vm, "reboot",       l_reboot);
    be_regfunc(vm, "refresh",      l_refresh);
    be_regfunc(vm, "device_sleep", l_device_sleep);
}

cmd_intent_t berry_c2(const char *script, uint32_t *sleep_s, bool *ok_out)
{
    s_intent = CMD_INTENT_NONE;
    s_intent_sleep_s = 0;

    bvm *vm = be_vm_new();
    cmd_register(vm);
    store_register(vm);   /* store_set/get/del/has/keys/ok */
    rtc_register(vm);     /* rtc_set/get/del/has */
    dev_register(vm);     /* mac/serial/chip/uptime/battery + nvs_str (read-only) */
    int r = be_loadstring(vm, script);
    if (r == BE_OK) r = be_pcall(vm, 0);
    if (r != BE_OK) { ESP_LOGE(TAG, "Berry C2 failed (res=%d)", r); be_dumpexcept(vm); }
    be_vm_delete(vm);

    if (ok_out) *ok_out = (r == BE_OK);
    if (sleep_s) *sleep_s = s_intent_sleep_s;
    return s_intent;
}
