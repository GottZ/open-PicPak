#include "net.h"
#include <string.h>
#include <stdlib.h>
#include <stdio.h>
#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"
#include "esp_wifi.h"
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "esp_http_client.h"
#include "esp_crt_bundle.h"
#include "nvs.h"
#include "logbuf.h"
#include "ota.h"

static const char *TAG = "net";

#define WIFI_CONNECTED_BIT BIT0
#define WIFI_FAIL_BIT      BIT1
static EventGroupHandle_t s_wifi_eg;
static int s_retry, s_max_retry = 8;
static bool s_netif_done;
static bool s_started;              /* esp_wifi_start() already called (allowed only once) */
static esp_netif_t *s_netif;        /* for dhcpc_stop / set_ip_info (static-IP path) */
static volatile bool s_connected;   /* WiFi currently connected -> idempotent re-use in keep-awake */
static bool s_static_mode;          /* current attempt uses static IP (no DHCP) */
static bool s_pinned;               /* current attempt uses BSSID/channel pin (no scan) */
static volatile bool s_stopping;    /* intentional net_wifi_stop -> auto-reconnect in handler off */

/* Phase timing (us since boot) for power profiling: start->connected = scan+auth,
 * connected->got_ip = DHCP. Purely additive, does not affect the flow. */
static int64_t s_t_start, s_t_conn, s_t_ip;

/* Connect cache (#2): BSSID+channel -> scan omitted; IP/GW/mask -> DHCP omitted.
 * In NVS (survives rst:0x15 AND deep sleep, unlike RTC-RAM).
 * The two levers are switchable individually -> separate validation (one change,
 * one test). Enable WARM_STATIC_IP only once the static-IP path is verified. */
/* BSSID pin OFF: empirically ~0 gain (scan is not the bottleneck — scan+auth is
 * dominated by AP-side SAE/association-refused, measured 0.6-2.4s variable),
 * and risky with mesh/roaming (might pin a no-longer-optimal AP -> needless
 * warm fail). Re-enable with a guaranteed single AP. */
#define WARM_BSSID_PIN  0   /* pin BSSID+channel -> no scan */
#define WARM_STATIC_IP  1   /* stop DHCP + static IP -> no DHCP roundtrip (the real lever) */

/* Adaptive WiFi TX power (0.25-dBm units). Steps 11->13->14 dBm: on the tether
 * (rssi -78) verified all <=14 dBm connect cleanly in ~640ms, 15 dBm flaps
 * (TX-PA peak collapses the supply -> reassoc loop). Start at 11 dBm = smallest
 * brownout peak; if the connect does not succeed, step up gradually to max 14 dBm. The
 * working index is persisted in NVS -> the next boot starts straight with it (no
 * stepping up per cycle). A COLD connect (cache miss/location change/NETCLR) re-probes
 * from the base -> the value drops back to the minimum after a move to a location with
 * a better signal. Only takes effect when set BEFORE the first assoc burst (STA_START
 * handler / before re-assoc), otherwise the assoc still sends at full power. */
static const int8_t s_tx_steps[] = { 44, 52, 56 };   /* 11, 13, 14 dBm */
#define TX_NSTEPS ((int)(sizeof(s_tx_steps) / sizeof(s_tx_steps[0])))
static int s_tx_idx;   /* current step in s_tx_steps; 0 = base (11 dBm) */
#define NETCACHE_NS "netcache"
typedef struct {
    uint8_t  valid;
    uint8_t  channel;
    uint8_t  bssid[6];
    uint32_t ip, gw, mask, dns;
    char     ssid[33];   /* cache is valid only for THIS network -> SSID change invalidates it */
} net_cache_t;
static net_cache_t s_cache;
static bool s_cache_loaded;
static esp_netif_ip_info_t s_got;   /* most recently DHCP-obtained IP (-> cache) */
static char s_ssid[33];             /* SSID of the current/last attempt (for net_ssid, survives stop) */

static void cache_load(void)
{
    if (s_cache_loaded) return;
    s_cache_loaded = true;
    nvs_handle_t h;
    if (nvs_open(NETCACHE_NS, NVS_READONLY, &h) == ESP_OK) {
        size_t sz = sizeof(s_cache);
        if (nvs_get_blob(h, "c", &s_cache, &sz) != ESP_OK || sz != sizeof(s_cache))
            s_cache.valid = 0;
        nvs_close(h);
    } else {
        s_cache.valid = 0;
    }
}

static void cache_store(void)
{
    nvs_handle_t h;
    if (nvs_open(NETCACHE_NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_blob(h, "c", &s_cache, sizeof(s_cache));
        nvs_commit(h);
        nvs_close(h);
    }
}

/* Persisted TX step (NVS key "txidx" in NETCACHE_NS, separate from the cache blob ->
 * survives cache_store/-clear only as long as we do not actively reset it). */
static int tx_idx_load(void)
{
    uint8_t v = 0;
    nvs_handle_t h;
    if (nvs_open(NETCACHE_NS, NVS_READONLY, &h) == ESP_OK) {
        nvs_get_u8(h, "txidx", &v);
        nvs_close(h);
    }
    return (v < TX_NSTEPS) ? v : 0;
}

static void tx_idx_save(int idx)
{
    nvs_handle_t h;
    if (nvs_open(NETCACHE_NS, NVS_READWRITE, &h) == ESP_OK) {
        nvs_set_u8(h, "txidx", (uint8_t)idx);
        nvs_commit(h);
        nvs_close(h);
    }
}

void net_cache_clear(void)
{
    s_cache.valid = 0;
    s_cache_loaded = true;   /* do not reload from NVS */
    cache_store();
    tx_idx_save(0);          /* cache gone = fresh context -> re-probe TX from the base */
}

static void wifi_evt(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        /* Set the TX power of the current step BEFORE the association sends the first
         * (brownout-critical) TX burst. esp_wifi_set_max_tx_power only takes effect
         * after esp_wifi_start -> the STA_START handler is the earliest safe point. */
        esp_wifi_set_max_tx_power(s_tx_steps[s_tx_idx]);
        esp_wifi_connect();
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_CONNECTED) {
        s_t_conn = esp_timer_get_time();   /* associated (before DHCP/IP) */
        /* Even with static IP an IP_EVENT_STA_GOT_IP follows (esp_netif posts it once
         * the static IP is active after the assoc) -> the CONNECTED_BIT is set there,
         * only then is lwIP really usable. */
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        s_connected = false;
        if (s_stopping) return;   /* intentional stop (net_wifi_stop) -> do not reconnect */
        if (s_retry < s_max_retry) {
            s_retry++;
            esp_wifi_connect();   /* return value irrelevant: ESP_ERR_WIFI_CONN during an active attempt is harmless */
            ESP_LOGW(TAG, "reconnect %d/%d", s_retry, s_max_retry);
        } else {
            xEventGroupSetBits(s_wifi_eg, WIFI_FAIL_BIT);
        }
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *e = (ip_event_got_ip_t *)data;
        s_t_ip = esp_timer_get_time();
        s_got = e->ip_info;   /* -> cache (cold/DHCP path) */
        ESP_LOGI(TAG, "got ip " IPSTR, IP2STR(&e->ip_info.ip));
        s_retry = 0;
        s_connected = true;
        xEventGroupSetBits(s_wifi_eg, WIFI_CONNECTED_BIT);
    }
}

/* One-time WiFi/netif initialization (handler, mode). Idempotent. */
static void net_init_once(void)
{
    if (s_netif_done) return;
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    s_netif = esp_netif_create_default_wifi_sta();
    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&cfg));
    /* Register handlers only ONCE (otherwise they accumulate per call). */
    ESP_ERROR_CHECK(esp_event_handler_instance_register(WIFI_EVENT, ESP_EVENT_ANY_ID, &wifi_evt, NULL, NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(IP_EVENT, IP_EVENT_STA_GOT_IP, &wifi_evt, NULL, NULL));
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    s_netif_done = true;
}

/* One connect attempt. warm=true: pin BSSID+channel (no scan) + static IP from
 * cache (no DHCP). warm=false: full scan + DHCP. Returns true when connected. */
static bool try_connect(const char *ssid, const char *pass, int timeout_ms, bool warm)
{
    bool pin       = warm && WARM_BSSID_PIN;
    bool static_ip = warm && WARM_STATIC_IP;
    xEventGroupClearBits(s_wifi_eg, WIFI_CONNECTED_BIT | WIFI_FAIL_BIT);
    s_retry = 0;
    s_stopping = false;           /* fresh connect -> reconnect logic armed again */
    s_max_retry = warm ? 2 : 8;   /* warm fails fast -> move into the fallback quickly */
    s_static_mode = static_ip;
    s_pinned = pin;

    strncpy(s_ssid, ssid, sizeof(s_ssid) - 1);   /* remember for net_ssid (survives net_wifi_stop) */
    s_ssid[sizeof(s_ssid) - 1] = '\0';

    wifi_config_t wc = { 0 };
    strncpy((char *)wc.sta.ssid, ssid, sizeof(wc.sta.ssid) - 1);
    strncpy((char *)wc.sta.password, pass, sizeof(wc.sta.password) - 1);
    wc.sta.threshold.authmode = WIFI_AUTH_WPA2_PSK;   /* allows WPA2..WPA3 */
    wc.sta.sae_pwe_h2e = WPA3_SAE_PWE_BOTH;            /* WPA3-SAE (Hash-to-Element + hunting-and-pecking) */
    wc.sta.pmf_cfg.capable = true;
    if (pin) {
        memcpy(wc.sta.bssid, s_cache.bssid, sizeof(wc.sta.bssid));
        wc.sta.bssid_set = true;          /* this AP directly -> no scan */
        wc.sta.channel = s_cache.channel;
    }
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wc));

    if (static_ip) {
        /* Stop the DHCP client + set a static IP -> no DHCP roundtrip. With the DHCP
         * client stopped there is NO active DNS server anymore -> a DNS URL
         * (https://host/...) would not resolve. So re-set the resolver cached during
         * the cold connect (fallback: gateway). Unused with an IP URL, but harmless
         * -> url decides ip-vs-dns without a further switch. */
        esp_netif_dhcpc_stop(s_netif);    /* ALREADY_STOPPED is ok */
        esp_netif_ip_info_t ip = { 0 };
        ip.ip.addr = s_cache.ip; ip.gw.addr = s_cache.gw; ip.netmask.addr = s_cache.mask;
        esp_netif_set_ip_info(s_netif, &ip);
        esp_netif_dns_info_t dns = { 0 };
        dns.ip.type = ESP_IPADDR_TYPE_V4;
        dns.ip.u_addr.ip4.addr = s_cache.dns ? s_cache.dns : s_cache.gw;
        esp_netif_set_dns_info(s_netif, ESP_NETIF_DNS_MAIN, &dns);
    } else {
        esp_netif_dhcpc_start(s_netif);   /* ALREADY_STARTED is ok (warm may have stopped it) */
    }

    s_t_start = s_t_conn = s_t_ip = 0;
    s_t_start = esp_timer_get_time();
    if (!s_started) {
        ESP_ERROR_CHECK(esp_wifi_start());   /* -> STA_START -> sets TX step -> esp_wifi_connect */
        s_started = true;
    } else {
        /* Stack already running (re-connect/escalation): STA_START no longer fires ->
         * set the TX step here explicitly BEFORE the assoc. */
        esp_wifi_set_max_tx_power(s_tx_steps[s_tx_idx]);
        esp_wifi_connect();
    }

    EventBits_t bits = xEventGroupWaitBits(s_wifi_eg, WIFI_CONNECTED_BIT | WIFI_FAIL_BIT,
                                           pdTRUE, pdFALSE, pdMS_TO_TICKS(timeout_ms));
    return (bits & WIFI_CONNECTED_BIT) != 0;
}

/* After a cold (DHCP) connect, cache BSSID/channel/IP/DNS for the next warm connect. */
static void cache_cold(const char *ssid)
{
    wifi_ap_record_t ap;
    if (esp_wifi_sta_get_ap_info(&ap) != ESP_OK) return;
    memcpy(s_cache.bssid, ap.bssid, sizeof(s_cache.bssid));
    s_cache.channel = ap.primary;
    s_cache.ip = s_got.ip.addr; s_cache.gw = s_got.gw.addr; s_cache.mask = s_got.netmask.addr;
    esp_netif_dns_info_t dns;
    s_cache.dns = (esp_netif_get_dns_info(s_netif, ESP_NETIF_DNS_MAIN, &dns) == ESP_OK
                   && dns.ip.type == ESP_IPADDR_TYPE_V4) ? dns.ip.u_addr.ip4.addr : 0;
    strncpy(s_cache.ssid, ssid, sizeof(s_cache.ssid) - 1);
    s_cache.ssid[sizeof(s_cache.ssid) - 1] = '\0';   /* -> SSID-change detection */
    s_cache.valid = 1;
    cache_store();
    ESP_LOGI(TAG, "netcache: ch=%u bssid=%02x:%02x:%02x:%02x:%02x:%02x ip=" IPSTR,
             s_cache.channel, ap.bssid[0], ap.bssid[1], ap.bssid[2],
             ap.bssid[3], ap.bssid[4], ap.bssid[5], IP2STR(&s_got.ip));
}

bool net_wifi_connect(const char *ssid, const char *pass, int timeout_ms)
{
    /* Idempotent: in keep-awake (USB) run_cycle is called repeatedly and the WiFi
     * stays connected in between -> re-use, no new start. */
    if (s_connected) return true;

    if (!s_wifi_eg) s_wifi_eg = xEventGroupCreate();
    cache_load();
    net_init_once();

    /* SSID change -> the cache (BSSID/channel/static-IP/DNS) belongs to the old network
     * and is worthless. Discard it autonomously, otherwise a necessarily failing warm
     * connect runs against the old BSSID/IP (previously: manual NETCLR required). */
    if (s_cache.valid && strncmp(s_cache.ssid, ssid, sizeof(s_cache.ssid)) != 0) {
        ESP_LOGI(TAG, "SSID change -> connect cache discarded (connect cold + re-cache)");
        net_cache_clear();
    }

    bool warm = s_cache.valid && (WARM_BSSID_PIN || WARM_STATIC_IP);
    int start_idx = warm ? tx_idx_load() : 0;   /* warm: persisted step; cold: base */
    s_tx_idx = start_idx;

    /* 1) First attempt at the start step (warm, if cache valid). */
    bool ok = try_connect(ssid, pass, timeout_ms, warm);

    /* 2) warm failed -> cold fallback (full scan/DHCP). net_cache_clear sets the TX
     *    persistence to the base -> the cold attempt re-probes from 11 dBm. */
    if (!ok && warm) {
        ESP_LOGW(TAG, "warm connect failed -> fallback to full scan/DHCP");
        net_cache_clear();
        s_tx_idx = 0;
        esp_wifi_disconnect();
        vTaskDelay(pdMS_TO_TICKS(100));   /* let hanging assoc attempts settle */
        ok = try_connect(ssid, pass, timeout_ms, false);
    }

    /* 3) Still not connected -> step the TX power up (cold) to max (14 dBm). A weak
     *    signal needs more TX; the steps stay below the flapping limit (15 dBm). */
    while (!ok && s_tx_idx + 1 < TX_NSTEPS) {
        s_tx_idx++;
        ESP_LOGW(TAG, "connect failed -> TX up to %ddBm", s_tx_steps[s_tx_idx] / 4);
        esp_wifi_disconnect();
        vTaskDelay(pdMS_TO_TICKS(100));
        ok = try_connect(ssid, pass, timeout_ms, false);
    }

    /* 4) Persist the working step (if different) -> the next boot starts straight
     *    with it, no stepping up again. */
    if (ok && s_tx_idx != start_idx)
        tx_idx_save(s_tx_idx);

    if (ok && !s_static_mode) cache_cold(ssid);   /* cache BSSID/channel/IP for the next warm connect */
    if (ok) {
        int64_t scan_auth = (s_t_conn > s_t_start) ? (s_t_conn - s_t_start) / 1000 : -1;
        int64_t dhcp = (s_t_ip > s_t_conn) ? (s_t_ip - s_t_conn) / 1000 : -1;
        int64_t total = (s_t_ip - s_t_start) / 1000;
        const char *mode = (s_static_mode && s_pinned) ? "warm: bssid-pin + static-ip"
                         : s_static_mode               ? "warm: static-ip + scan"
                         : s_pinned                    ? "warm: bssid-pin + dhcp"
                         :                               "cold: scan + dhcp";
        ESP_LOGI(TAG, "TIMING connect: scan+auth=%lldms dhcp=%lldms total=%lldms [%s] tx=%ddBm",
                 scan_auth, dhcp, total, mode, s_tx_steps[s_tx_idx] / 4);
    }
    return ok;
}

void net_wifi_stop(void)
{
    if (!s_started) return;
    /* RF modem fully off (not just deassociate): no more DTIM RX burst that could
     * overlap with another peak load (EPD charge pump), and lower baseline current.
     * s_stopping suppresses the auto-reconnect that esp_wifi_disconnect/stop would
     * otherwise trigger via STA_DISCONNECTED; the next net_wifi_connect resets it and
     * restarts the stack normally. */
    s_stopping = true;
    esp_wifi_disconnect();
    esp_wifi_stop();
    s_started = false;
    s_connected = false;
}

bool net_wifi_try(const char *ssid, const char *pass, int timeout_ms)
{
    /* Forcing connect (CB1): unlike net_wifi_connect this does NOT short-circuit on
     * s_connected -- a scan-match / password-rotation policy must be able to (re)associate
     * against the SSID/pass it passes. If already associated, drop it first so the new
     * config takes effect, then a fresh cold attempt at base TX (fail-fast: the caller
     * iterates candidates and wants a quick yes/no, not the multi-second TX climb). */
    if (!s_wifi_eg) s_wifi_eg = xEventGroupCreate();
    cache_load();
    net_init_once();

    if (s_connected) {
        s_stopping = true;
        esp_wifi_disconnect();
        s_connected = false;
        vTaskDelay(pdMS_TO_TICKS(50));
    }
    if (s_cache.valid && strncmp(s_cache.ssid, ssid, sizeof(s_cache.ssid)) != 0) {
        ESP_LOGI(TAG, "wifi_try: SSID change -> connect cache discarded");
        net_cache_clear();
    }
    s_tx_idx = 0;
    bool ok = try_connect(ssid, pass, timeout_ms, false);
    if (ok && !s_static_mode) cache_cold(ssid);
    ESP_LOGI(TAG, "wifi_try \"%s\": %s", ssid, ok ? "connected" : "failed");
    return ok;
}

bool net_ip(char *buf, size_t cap)
{
    if (!buf || cap < 16 || s_got.ip.addr == 0) return false;
    snprintf(buf, cap, IPSTR, IP2STR(&s_got.ip));
    return true;
}

bool net_ssid(char *buf, size_t cap)
{
    if (!buf || cap < 1 || s_ssid[0] == '\0') return false;
    strncpy(buf, s_ssid, cap - 1);
    buf[cap - 1] = '\0';
    return true;
}

bool net_rssi(int *out)
{
    /* H2: RSSI is only meaningful while the RF is on and associated. */
    if (!out || !s_connected) return false;
    wifi_ap_record_t ap;
    if (esp_wifi_sta_get_ap_info(&ap) != ESP_OK) return false;
    *out = ap.rssi;
    return true;
}

int net_wifi_scan(net_ap_t *out, int max)
{
    if (!out || max <= 0) return -1;
    if (!s_wifi_eg) s_wifi_eg = xEventGroupCreate();
    net_init_once();
    /* H1: a scan on an active STA triggers STA_DISCONNECTED -> suppress the auto-reconnect
     * during the scan window. The caller (multi-WLAN policy) scans BEFORE the final connect. */
    s_stopping = true;
    if (!s_started) {
        esp_wifi_set_max_tx_power(s_tx_steps[s_tx_idx]);
        if (esp_wifi_start() != ESP_OK) { s_stopping = false; return -1; }
        s_started = true;
    }
    wifi_scan_config_t sc = { 0 };                    /* active scan, all channels */
    if (esp_wifi_scan_start(&sc, true) != ESP_OK) { s_stopping = false; return -1; }
    uint16_t found = 0;
    esp_wifi_scan_get_ap_num(&found);
    uint16_t want = found < (uint16_t)max ? found : (uint16_t)max;
    wifi_ap_record_t *recs = calloc(want ? want : 1, sizeof(wifi_ap_record_t));
    if (!recs) { s_stopping = false; return -1; }
    uint16_t got = want;
    esp_wifi_scan_get_ap_records(&got, recs);
    for (uint16_t i = 0; i < got; i++) {
        strncpy(out[i].ssid, (char *)recs[i].ssid, sizeof(out[i].ssid) - 1);
        out[i].ssid[sizeof(out[i].ssid) - 1] = '\0';
        out[i].rssi = recs[i].rssi;
        out[i].auth = (uint8_t)recs[i].authmode;
    }
    free(recs);
    s_connected = false;     /* the scan dropped any prior association */
    s_stopping = false;
    return (int)got;
}

void net_last_connect_ms(int32_t *scan_auth, int32_t *dhcp)
{
    if (scan_auth) *scan_auth = (s_t_conn > s_t_start) ? (int32_t)((s_t_conn - s_t_start) / 1000) : -1;
    if (dhcp)      *dhcp      = (s_t_ip   > s_t_conn ) ? (int32_t)((s_t_ip   - s_t_conn ) / 1000) : -1;
}

/* --- HTTP pull --- */
typedef struct {
    uint8_t *buf; size_t cap; size_t len; uint32_t next_wake;
    char *fw_version; size_t fw_version_cap;   /* X-Firmware-Version -> target FW for OTA */
} pull_ctx_t;

static esp_err_t http_evt(esp_http_client_event_t *e)
{
    pull_ctx_t *c = (pull_ctx_t *)e->user_data;
    if (e->event_id == HTTP_EVENT_ON_HEADER) {
        if (e->header_key && strcasecmp(e->header_key, "X-Next-Wake-Seconds") == 0) {
            c->next_wake = (uint32_t)strtoul(e->header_value, NULL, 10);
            ESP_LOGI(TAG, "X-Next-Wake-Seconds=%lu", (unsigned long)c->next_wake);
        } else if (e->header_key && strcasecmp(e->header_key, "X-Firmware-Version") == 0) {
            if (c->fw_version && c->fw_version_cap) {
                strncpy(c->fw_version, e->header_value ? e->header_value : "", c->fw_version_cap - 1);
                c->fw_version[c->fw_version_cap - 1] = '\0';
            }
            ESP_LOGI(TAG, "X-Firmware-Version=%s", e->header_value ? e->header_value : "(null)");
        } else if (e->header_key && strcasecmp(e->header_key, "Date") == 0) {
            /* The server delivers the Date header automatically (GMT). A candidate for a
             * free wall clock (no SNTP roundtrip). For now only log/validate. */
            ESP_LOGI(TAG, "HTTP Date: %s", e->header_value ? e->header_value : "(null)");
        }
    } else if (e->event_id == HTTP_EVENT_ON_DATA) {
        if (c->len + e->data_len <= c->cap) {
            memcpy(c->buf + c->len, e->data, e->data_len);
            c->len += e->data_len;
        } else {
            ESP_LOGW(TAG, "overflow: %u + %d > %u", (unsigned)c->len, e->data_len, (unsigned)c->cap);
        }
    }
    return ESP_OK;
}

bool net_http_fetch_frame(const char *url, uint8_t *buf, size_t frame_len,
                          uint32_t *next_wake_s, char *fw_version, size_t fw_version_len)
{
    if (fw_version && fw_version_len) fw_version[0] = '\0';
    pull_ctx_t ctx = { .buf = buf, .cap = frame_len, .len = 0, .next_wake = 0,
                       .fw_version = fw_version, .fw_version_cap = fw_version_len };
    esp_http_client_config_t cfg = {
        .url = url,
        .event_handler = http_evt,
        .user_data = &ctx,
        .timeout_ms = 15000,
        /* Verify https:// URLs against the compiled-in root bundle; for http:// URLs
         * esp_http_client ignores the field -> url alone decides the protocol, no
         * second config switch needed. */
        .crt_bundle_attach = esp_crt_bundle_attach,
        .buffer_size = 2048,
        /* TX buffer: the default 512 B is NOT enough for the X-Picpak-Log header (up to
         * ~2 KB log ring) + GET line + Host -> the header would be silently dropped, the
         * GET would still go through with 200. Give reserve (heap is plentiful). */
        .buffer_size_tx = 3072,
    };
    esp_http_client_handle_t h = esp_http_client_init(&cfg);
    /* Send the FW-internal log ring as a header -> the picture server logs it.
     * The only exfil channel: USB serial is dead during the cycle, and a port-open
     * reset nulls the RTC-RAM. The header carries the net-connect logs
     * (scan/auth/dhcp/got-ip) of the CURRENT cycle -> exactly the power-profile phase. */
    static char fwlog[2100];
    if (logbuf_export(fwlog, sizeof(fwlog)) > 0)
        esp_http_client_set_header(h, "X-Picpak-Log", fwlog);
    /* OTA diagnostics (partition states + NVS counters) as a header -> the OTA history
     * is visible in the server log on battery without USB/serial (the logbuf ring
     * rotates the boot lines away; NVS values survive). */
    static char fwdiag[256];
    if (ota_diag_export(fwdiag, sizeof(fwdiag)) > 0)
        esp_http_client_set_header(h, "X-Picpak-Diag", fwdiag);
    esp_err_t err = esp_http_client_perform(h);
    int status = esp_http_client_get_status_code(h);
    esp_http_client_cleanup(h);

    if (next_wake_s) *next_wake_s = ctx.next_wake;
    if (err != ESP_OK) { ESP_LOGE(TAG, "http err: %s", esp_err_to_name(err)); return false; }
    ESP_LOGI(TAG, "http %d, %u/%u bytes", status, (unsigned)ctx.len, (unsigned)frame_len);
    return status == 200 && ctx.len == frame_len;
}
