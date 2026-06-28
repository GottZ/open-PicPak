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
#include "esp_attr.h"   /* RTC_DATA_ATTR for the cross-wake C2 request counter */
#include "ota.h"        /* ota_boots() = reset-proof boot counter */
#include "auth_core.h"  /* auth_hotp (HOTP, byte-compatible with the backend) */
#include "nvs.h"
#include <string.h>
#include <stdio.h>    /* snprintf for the authed C2 poll URL */
#include <stdlib.h>   /* malloc for the C2 response buffer + strtoul for the ack seq */
#include "c2key.h"          /* long-term ECDSA P-256 identity: bond + sign (Doc 15) */
#include "mbedtls/sha256.h" /* re-key message hash */
#include "esp_random.h"     /* session secret RNG */

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

/* --- C2 poll (Doc 13 Wave 3b): fetch a Berry command script from the C2 endpoint + run it --- */
#define C2_RESP_MAX 8192   /* hard cap on a fetched C2 script (well under the WiFi+VM RAM spike) */
#define C2_NS       "picpak"

bool c2_set_url(const char *url)
{
    nvs_handle_t h;
    if (nvs_open(C2_NS, NVS_READWRITE, &h) != ESP_OK) return false;
    esp_err_t e = nvs_set_str(h, "c2_url", url);
    if (e == ESP_OK) e = nvs_commit(h);
    nvs_close(h);
    return e == ESP_OK;
}

bool c2_set_period(uint32_t secs)
{
    nvs_handle_t h;
    if (nvs_open(C2_NS, NVS_READWRITE, &h) != ESP_OK) return false;
    esp_err_t e = nvs_set_u32(h, "c2_period", secs);
    if (e == ESP_OK) e = nvs_commit(h);
    nvs_close(h);
    return e == ESP_OK;
}

uint32_t c2_poll_period(void)
{
    nvs_handle_t h; uint32_t v = 0;
    if (nvs_open(C2_NS, NVS_READONLY, &h) == ESP_OK) {
        if (nvs_get_u32(h, "c2_period", &v) != ESP_OK) v = 0;
        nvs_close(h);
    }
    return v;   /* 0 = C2 poll disabled (default) */
}

/* --- C2 device auth (Doc 13 Wave 3d FW-half): HOTP triple c/otp/bc --- */
/* Composite counter C = (boot_count << 24) | rtc_counter (auth-contract k=24). boot_count is the
 * reset-proof NVS counter; rtc_counter lives in RTC-RAM (survives a deep-sleep wake, nulled on a cold
 * boot / port-open reset) and is reset to 0 on a boot_count change, then ++ per request. */
RTC_DATA_ATTR static uint32_t s_c2_rtc;
RTC_DATA_ATTR static uint32_t s_c2_lastbc;

static int hexval(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static size_t hex_decode(const char *hex, uint8_t *out, size_t cap)
{
    size_t n = strlen(hex);
    if (n == 0 || (n & 1)) return 0;          /* must be a non-empty even-length hex string */
    size_t b = n / 2;
    if (b > cap) return 0;
    for (size_t i = 0; i < b; i++) {
        int hi = hexval(hex[2 * i]), lo = hexval(hex[2 * i + 1]);
        if (hi < 0 || lo < 0) return 0;
        out[i] = (uint8_t)((hi << 4) | lo);
    }
    return b;
}

bool c2_set_secret(const char *hex)
{
    uint8_t tmp[48];
    if (hex_decode(hex, tmp, sizeof tmp) == 0) return false;   /* validate before storing */
    nvs_handle_t h;
    if (nvs_open(C2_NS, NVS_READWRITE, &h) != ESP_OK) return false;
    esp_err_t e = nvs_set_str(h, "c2_secret", hex);
    if (e == ESP_OK) e = nvs_commit(h);
    nvs_close(h);
    return e == ESP_OK;
}

/* Compute the next HOTP auth triple. Returns false if no c2_secret is provisioned. Advances the
 * per-request rtc_counter (after a boot_count change it restarts at 1 -> the server's recovery
 * window absorbs the jump). digits = 8 (the fleet wire value, auth-contract). */
bool c2_compute_auth(uint64_t *c_out, uint32_t *otp_out, uint32_t *bc_out)
{
    char hex[100]; hex[0] = '\0';
    nvs_handle_t h;
    if (nvs_open(C2_NS, NVS_READONLY, &h) == ESP_OK) {
        size_t l = sizeof hex;
        if (nvs_get_str(h, "c2_secret", hex, &l) != ESP_OK) hex[0] = '\0';
        nvs_close(h);
    }
    if (hex[0] == '\0') return false;
    uint8_t secret[48];
    size_t slen = hex_decode(hex, secret, sizeof secret);
    if (slen == 0) return false;

    uint32_t bc = ota_boots();
    if (bc != s_c2_lastbc) { s_c2_rtc = 0; s_c2_lastbc = bc; }   /* boot_count changed -> rtc resets */
    s_c2_rtc++;
    uint64_t c = ((uint64_t)bc << 24) | (uint64_t)(s_c2_rtc & 0xFFFFFFu);
    uint32_t otp = auth_hotp(secret, slen, c, 8);

    if (c_out)   *c_out   = c;
    if (otp_out) *otp_out = otp;
    if (bc_out)  *bc_out  = bc;
    return true;
}

/* Extract serial_number from the storage/dev_sn JSON blob ({"serial_number":"D22CHGW"}). The factory
 * identity is JSON (config-IS-Berry; render.be parses the same key) -> the C2 sn param needs the bare
 * value. Minimal scan, no cJSON pull-in: locate the key, the colon, the opening quote, copy to the
 * closing quote. Returns false (and out="") on a missing key / unparseable blob. */
static bool c2_device_serial(char *out, size_t cap)
{
    if (!out || cap == 0) return false;
    out[0] = '\0';
    char raw[160];
    nvs_handle_t h;
    if (nvs_open("storage", NVS_READONLY, &h) != ESP_OK) return false;
    size_t l = sizeof raw;
    esp_err_t e = nvs_get_str(h, "dev_sn", raw, &l);
    nvs_close(h);
    if (e != ESP_OK) return false;
    const char *p = strstr(raw, "\"serial_number\"");
    if (!p) return false;
    p = strchr(p + 15, ':');            /* 15 = strlen("\"serial_number\"") */
    if (!p) return false;
    p = strchr(p, '"');                 /* opening quote of the value */
    if (!p) return false;
    p++;
    size_t i = 0;
    while (*p && *p != '"' && i < cap - 1) out[i++] = *p++;
    out[i] = '\0';
    return i > 0;
}

/* --- Doc 15 session re-key + RTC-RAM session state --- */
#define C2_SESS_MAGIC 0x53455332u   /* "SES2" — RTC-RAM session validity marker */
/* RTC_NOINIT: survives a deep-sleep wake (reuse the session, no re-key) but reads as garbage on a
 * cold boot / brownout / USB re-enum reset -> the magic mismatch forces a re-key. The exact survival
 * per reset class is the G-RTC empirical gate (design 15 §6). */
RTC_NOINIT_ATTR static struct {
    uint32_t magic;
    uint32_t counter;     /* flat per-session HOTP counter (no boot_count composite) */
    uint8_t  secret[20];  /* session HOTP secret (RAM-only; never persisted to NVS) */
} s_c2sess;

static bool c2_sess_valid(void)      { return s_c2sess.magic == C2_SESS_MAGIC; }
static void c2_sess_invalidate(void) { s_c2sess.magic = 0; }

static void hexenc(const uint8_t *in, size_t n, char *out)
{
    static const char H[] = "0123456789abcdef";
    for (size_t i = 0; i < n; i++) { out[2 * i] = H[in[i] >> 4]; out[2 * i + 1] = H[in[i] & 0xf]; }
    out[2 * n] = '\0';
}

/* Extract the 32-hex value of "nonce" from the challenge JSON ({"nonce":"..."}) into out (>=33). */
static bool c2_parse_nonce(const char *json, char *out)
{
    const char *p = strstr(json, "\"nonce\"");
    if (!p) return false;
    p = strchr(p + 7, ':'); if (!p) return false;
    p = strchr(p, '"');     if (!p) return false;
    p++;
    int i = 0;
    while (p[i] && p[i] != '"' && i < 32) { out[i] = p[i]; i++; }
    out[i] = '\0';
    return i == 32;
}

/* Re-key handshake (Doc 15): GET <base>/challenge -> sign the nonce+secret with the long-term ECDSA
 * key -> POST <base>/rekey. On HTTP 204, writes the new 20-byte session secret into secret_out. */
static bool c2_rekey(const char *base, const char *sn, uint8_t secret_out[20])
{
    char curl[256];
    int n = snprintf(curl, sizeof curl, "%s/challenge?sn=%s", base, sn);
    if (n <= 0 || n >= (int)sizeof curl) return false;
    uint8_t cbuf[160]; size_t clen = 0;
    if (!net_http_get(curl, cbuf, sizeof cbuf - 1, &clen)) { ESP_LOGW(TAG, "rekey: challenge failed"); return false; }
    cbuf[clen] = '\0';
    char nonce[40];
    if (!c2_parse_nonce((char *)cbuf, nonce)) { ESP_LOGW(TAG, "rekey: no nonce in challenge"); return false; }

    uint8_t secret[20];
    esp_fill_random(secret, sizeof secret);
    uint8_t sh[32];
    mbedtls_sha256(secret, sizeof secret, sh, 0);
    char shhex[65]; hexenc(sh, 32, shhex);
    char msg[160];
    int mlen = snprintf(msg, sizeof msg, "c2rekey\n%s\n%s\n%s", sn, nonce, shhex);
    if (mlen <= 0 || mlen >= (int)sizeof msg) return false;

    uint8_t sig[80]; size_t siglen = 0;
    if (!c2_key_sign((uint8_t *)msg, mlen, sig, sizeof sig, &siglen)) { ESP_LOGW(TAG, "rekey: sign failed"); return false; }

    char sechex[41]; hexenc(secret, 20, sechex);
    char sighex[161]; hexenc(sig, siglen, sighex);
    char body[400];
    int blen = snprintf(body, sizeof body, "{\"nonce\":\"%s\",\"secret\":\"%s\",\"sig\":\"%s\"}", nonce, sechex, sighex);
    if (blen <= 0 || blen >= (int)sizeof body) return false;

    char rurl[256];
    n = snprintf(rurl, sizeof rurl, "%s/rekey?sn=%s", base, sn);
    if (n <= 0 || n >= (int)sizeof rurl) return false;
    int st = net_http_post(rurl, "application/json", (uint8_t *)body, blen);
    if (st != 204) { ESP_LOGW(TAG, "rekey: POST status %d", st); return false; }
    memcpy(secret_out, secret, 20);
    ESP_LOGI(TAG, "rekey OK: session established");
    return true;
}

cmd_intent_t c2_poll(uint32_t *sleep_s, bool *ran)
{
    if (ran) *ran = false;
    if (sleep_s) *sleep_s = 0;

    char base[200]; base[0] = '\0';
    nvs_handle_t h;
    if (nvs_open(C2_NS, NVS_READONLY, &h) == ESP_OK) {
        size_t l = sizeof base;
        if (nvs_get_str(h, "c2_url", base, &l) != ESP_OK) base[0] = '\0';
        nvs_close(h);
    }
    if (base[0] == '\0') return CMD_INTENT_NONE;   /* not configured */
    /* HTTPS-ONLY (RCE safety, design 13b §0): the C2 response is executed as code, so the server
     * MUST be CA-verified. Refuse a plaintext c2_url outright. */
    if (strncmp(base, "https://", 8) != 0) {
        ESP_LOGW(TAG, "C2 poll: refusing non-https c2_url");
        return CMD_INTENT_NONE;
    }

    /* Device identity for the backend's per-device cursor + HOTP row lookup. No serial -> no poll
     * (an unkeyed request can only ever earn a 404/401). */
    char sn[32];
    if (!c2_device_serial(sn, sizeof sn)) {
        ESP_LOGW(TAG, "C2 poll: no device serial (storage/dev_sn) -> skip");
        return CMD_INTENT_NONE;
    }

    /* ack = the highest C2 seq already applied (persisted in picpak/c2_seq, default 0 = cold device).
     * The backend advances its cursor forward-only (GREATEST) from this. */
    uint32_t ack = 0;
    if (nvs_open(C2_NS, NVS_READONLY, &h) == ESP_OK) {
        if (nvs_get_u32(h, "c2_seq", &ack) != ESP_OK) ack = 0;
        nvs_close(h);
    }

    char url[512];
    int n;
    bool session = c2_key_present();   /* bonded (ECDSA keypair) -> Doc 15 session path */
    if (session) {
        /* Re-key on RTC-RAM loss (cold boot / reset), then HOTP over the RTC-RAM session secret with
         * a FLAT counter (no boot_count) -> the bc-lockout cannot occur. A deep-sleep wake keeps the
         * session (no re-key). */
        if (!c2_sess_valid()) {
            if (!c2_rekey(base, sn, s_c2sess.secret)) {
                ESP_LOGW(TAG, "C2 poll: rekey failed -> skip");
                return CMD_INTENT_NONE;
            }
            s_c2sess.counter = 0;
            s_c2sess.magic = C2_SESS_MAGIC;
        }
        uint32_t cc = ++s_c2sess.counter;
        uint32_t otp = auth_hotp(s_c2sess.secret, sizeof s_c2sess.secret, cc, 8);
        n = snprintf(url, sizeof url, "%s?sn=%s&ack=%lu&c=%lu&otp=%lu",
                     base, sn, (unsigned long)ack, (unsigned long)cc, (unsigned long)otp);
    } else {
        /* Legacy bc-composite path (un-bonded device; bridge until cutover). */
        uint64_t c; uint32_t otp, bc;
        if (!c2_compute_auth(&c, &otp, &bc)) {
            ESP_LOGW(TAG, "C2 poll: no key/secret -> skip authed poll");
            return CMD_INTENT_NONE;
        }
        n = snprintf(url, sizeof url, "%s?sn=%s&ack=%lu&c=%llu&otp=%lu&bc=%lu",
                     base, sn, (unsigned long)ack, (unsigned long long)c,
                     (unsigned long)otp, (unsigned long)bc);
    }
    if (n <= 0 || n >= (int)sizeof url) {
        ESP_LOGW(TAG, "C2 poll: URL build overflow (%d)", n);
        return CMD_INTENT_NONE;
    }

    uint8_t *buf = malloc(C2_RESP_MAX);
    if (!buf) return CMD_INTENT_NONE;
    size_t len = 0;
    char seq[16];
    bool ok = net_http_c2(url, buf, C2_RESP_MAX - 1, &len, seq, sizeof seq);   /* https verified via crt_bundle */
    cmd_intent_t in = CMD_INTENT_NONE;
    if (ok && len > 0) {
        buf[len] = '\0';                       /* NUL-terminate for be_loadstring */
        bool sok = false;
        in = berry_c2((char *)buf, sleep_s, &sok);
        if (ran) *ran = true;
        /* Persist the ack cursor BEFORE the caller actions the intent: a reboot/sleep intent must not
         * make the device re-run this seq on the next poll. Acked on a 200 with a seq regardless of
         * the Berry result -> a faulty script can't wedge the device in a poison-pill re-serve loop. */
        if (seq[0]) {
            uint32_t s = (uint32_t)strtoul(seq, NULL, 10);
            nvs_handle_t hh;
            if (nvs_open(C2_NS, NVS_READWRITE, &hh) == ESP_OK) {
                nvs_set_u32(hh, "c2_seq", s);
                nvs_commit(hh);
                nvs_close(hh);
            }
        }
        ESP_LOGI(TAG, "C2 poll: %u bytes seq=%s script ok=%d intent=%d", (unsigned)len, seq, (int)sok, (int)in);
    } else if (ok) {
        ESP_LOGI(TAG, "C2 poll: 204 in sync (ack=%lu)", (unsigned long)ack);   /* len==0, nothing to do */
    } else {
        ESP_LOGW(TAG, "C2 poll: fetch failed");
        /* session path: a 401 (backend rotated / stale) or transport error -> drop the session so the
         * next poll re-keys. Self-healing; bounded (one extra handshake), never a hard lockout. */
        if (session) c2_sess_invalidate();
    }
    free(buf);
    return in;
}
